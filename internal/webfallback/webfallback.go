// Package webfallback подменяет отказ сайта в ответе WebFetch текстом страницы
// из утилиты akit webtext. WebFetch представляется сайтам как Claude-User, и
// часть сайтов отвечает ему 403 или страницей проверки; webtext пробует прямой
// запрос под другим честным именем и архив Wayback. Задание лида 25.10, «да»
// пользователя — транскрипт лида 0bbcc980:11253 («1+2 - да»)
package webfallback

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	// webtextTimeout — граница webtext на все ступени вместе; таймаут хука в
	// settings.json 60 с, остаток — на запуск и чтение файла
	webtextTimeout = "45s"
	// Budget — сколько хук ждёт webtext, прежде чем убить его и оставить
	// ответ WebFetch как был
	Budget = 55 * time.Second
	// resultRunes — сколько текста страницы идёт агенту; остальное — ссылкой
	// на файл. Фоновый разбор projectC файл прочитать не может (у него только
	// WebSearch и WebFetch), но статьи почти всегда короче
	resultRunes = 30000
	// checkPageRunes — где искать признаки страницы проверки: WebFetch отдаёт
	// пересказ модели, и о заглушке она пишет в первой фразе, а дальше по
	// тексту те же слова встречаются в обычных статьях (документация Claude
	// Code о Cloudflare)
	checkPageRunes = 300
)

// Коды отказа. 202 — пустая страница проверки AWS WAF (eur-lex), 429 — сайт
// уже ограничил нас по частоте
var blockCodes = map[int]bool{202: true, 402: true, 403: true, 429: true, 503: true}

// Признаки страницы проверки в начале пересказа. Сухой прогон 12–25.10:
// все попадания — заглушки Anubis и reCAPTCHA, на 604 ответах 200 ложных нет
var checkPageMarks = []string{
	"anubis", "captcha", "checking your browser", "just a moment",
	"verify you are human", "verifying you are human",
	"проверки браузера", "проверка браузера",
}

// Fetch — ответ WebFetch в tool_response; та же форма нужна updatedToolOutput,
// иначе Claude Code его отвергает
type Fetch struct {
	Bytes      int    `json:"bytes"`
	Code       int    `json:"code"`
	CodeText   string `json:"codeText"`
	Result     string `json:"result"`
	DurationMs int    `json:"durationMs"`
	URL        string `json:"url"`
}

// Event — нужное из входа PostToolUse
type Event struct {
	SessionID    string          `json:"session_id"`
	ToolName     string          `json:"tool_name"`
	ToolResponse json.RawMessage `json:"tool_response"`
}

// Reason — почему ответ считается отказом сайта; "" — обычный ответ
func Reason(fetch Fetch) string {
	if blockCodes[fetch.Code] {
		return fmt.Sprintf("HTTP %d", fetch.Code)
	}
	if fetch.Code == 200 && isCheckPage(fetch.Result) {
		return "страница проверки"
	}
	return ""
}

func isCheckPage(result string) bool {
	head := []rune(result)
	head = head[:min(len(head), checkPageRunes)]
	lower := strings.ToLower(string(head))
	for _, mark := range checkPageMarks {
		if strings.Contains(lower, mark) {
			return true
		}
	}
	return false
}

// Runner запускает webtext с аргументами и возвращает stdout и код выхода
type Runner func(ctx context.Context, args []string) ([]byte, int, error)

// ErrNoWebtext — утилита не установлена: ответ WebFetch остаётся как был
var ErrNoWebtext = errors.New("webtext not installed")

// report — вывод webtext --json
type report struct {
	OK       bool   `json:"ok"`
	Source   string `json:"source"`
	Snapshot string `json:"snapshot"`
	Kind     string `json:"kind"`
	Path     string `json:"path"`
	Steps    []struct {
		Step   string `json:"step"`
		Result string `json:"result"`
	} `json:"steps"`
	Hint string `json:"hint"`
}

// Outcome — чем кончилась подмена, для лога
type Outcome struct {
	Reason string
	// Opened — webtext открыл страницу; false — к ответу WebFetch добавлен
	// его отчёт о ступенях
	Opened bool
	Source string
}

// Fallback зовёт webtext на отказ сайта и возвращает новый ответ WebFetch.
// nil без ошибки — ответ обычный, подменять нечего; ошибка — утилита не
// сработала, ответ остаётся как был
func Fallback(ctx context.Context, fetch Fetch, run Runner) (*Fetch, Outcome, error) {
	outcome := Outcome{Reason: Reason(fetch)}
	if outcome.Reason == "" || fetch.URL == "" {
		return nil, outcome, nil
	}

	args := []string{"--json", "--timeout", webtextTimeout}
	// Второй прямой запрос после 429 только продлит ограничение
	if fetch.Code == 429 {
		args = append(args, "--no-direct")
	}
	stdout, code, err := run(ctx, append(args, fetch.URL))
	if err != nil {
		return nil, outcome, err
	}
	if code != 0 && code != 1 {
		return nil, outcome, fmt.Errorf("webtext exit %d: %s", code, firstLine(stdout))
	}
	var rep report
	if err := json.Unmarshal(stdout, &rep); err != nil {
		return nil, outcome, fmt.Errorf("webtext output: %w", err)
	}

	updated := fetch
	if !rep.OK {
		updated.Result = fetch.Result + "\n\n" + failureNote(rep)
		return &updated, outcome, nil
	}
	text, err := os.ReadFile(rep.Path)
	if err != nil {
		return nil, outcome, fmt.Errorf("webtext text: %w", err)
	}
	outcome.Opened, outcome.Source = true, rep.Source
	updated.Result = openedText(fetch, outcome.Reason, rep, string(text))
	updated.Bytes = len(text)
	return &updated, outcome, nil
}

// openedText — ответ агенту: откуда текст и что промпт WebFetch к нему не
// применён, затем сам текст (заголовок, source, страница)
func openedText(fetch Fetch, reason string, rep report, text string) string {
	source := rep.Source
	if rep.Snapshot != "" {
		source += " " + rep.Snapshot
	}
	if rep.Kind != "" {
		source += ", " + rep.Kind
	}
	if fetch.CodeText != "" && fetch.Code != 200 {
		reason += " " + fetch.CodeText
	}
	header := fmt.Sprintf("WebFetch получил отказ сайта (%s). Страницу открыл webtext (%s). "+
		"Промпт WebFetch к тексту не применён, ниже текст страницы как есть", reason, source)

	runes := []rune(text)
	if len(runes) > resultRunes {
		header += fmt.Sprintf(": первые %d из %d символов, целиком — %s", resultRunes, len(runes), rep.Path)
		text = string(runes[:resultRunes])
	}
	return header + ".\n\n" + text
}

// failureNote — отчёт webtext, когда страницу не открыть: агент не повторяет
// пройденные ступени и видит следующий шаг
func failureNote(rep report) string {
	steps := make([]string, 0, len(rep.Steps))
	for _, step := range rep.Steps {
		steps = append(steps, step.Step+": "+step.Result)
	}
	note := "webtext тоже не открыл страницу"
	if len(steps) > 0 {
		note += " (" + strings.Join(steps, "; ") + ")"
	}
	note += "."
	if rep.Hint != "" {
		note += " " + rep.Hint
	}
	return note
}

func firstLine(data []byte) string {
	line, _, _ := bytes.Cut(bytes.TrimSpace(data), []byte("\n"))
	return string(line)
}

// LogURL — адрес для лога без query и фрагмента: в query бывают токены
func LogURL(raw string) string {
	address, _, _ := strings.Cut(raw, "#")
	address, _, _ = strings.Cut(address, "?")
	return strings.TrimPrefix(strings.TrimPrefix(address, "https://"), "http://")
}

// ExecRunner запускает webtext из PATH, а без него — ~/bin/webtext: хук
// наследует PATH сессии, а у фоновых claude -p он бывает урезан
func ExecRunner(ctx context.Context, args []string) ([]byte, int, error) {
	path, err := webtextPath()
	if err != nil {
		return nil, 0, err
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.WaitDelay = time.Second
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	err = cmd.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && ctx.Err() == nil {
		return stdout.Bytes(), exitErr.ExitCode(), nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("run webtext: %w", err)
	}
	return stdout.Bytes(), 0, nil
}

func webtextPath() (string, error) {
	if path, err := exec.LookPath("webtext"); err == nil {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", ErrNoWebtext
	}
	path := filepath.Join(home, "bin", "webtext")
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		return "", ErrNoWebtext
	}
	return path, nil
}
