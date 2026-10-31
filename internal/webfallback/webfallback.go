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
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf16"
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
	// contextUnits — граница additionalContext в PostToolUseFailure: Claude
	// Code режет строки хука на 10 000 символов (UTF-16, как длина строки JS),
	// а длиннее уносит в файл и показывает 2 000; запас — на свои строки
	// Claude Code вокруг
	contextUnits = 9500
	// errorRunes — сколько текста ошибки WebFetch идёт в заголовок
	errorRunes = 200
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

// Event — нужное из входа PostToolUse и PostToolUseFailure
type Event struct {
	SessionID     string          `json:"session_id"`
	HookEventName string          `json:"hook_event_name"`
	ToolName      string          `json:"tool_name"`
	ToolResponse  json.RawMessage `json:"tool_response"`
	ToolInput     struct {
		URL string `json:"url"`
	} `json:"tool_input"`
	// Error и IsInterrupt — только у PostToolUseFailure
	Error       string `json:"error"`
	IsInterrupt bool   `json:"is_interrupt"`
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

// ErrLocalAddress — адрес локальный или частной сети: WebFetch такие не
// открывает намеренно, и хук за него их не запрашивает
var ErrLocalAddress = errors.New("local address")

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

	// Второй прямой запрос после 429 только продлит ограничение
	rep, text, err := open(ctx, fetch.URL, fetch.Code == 429, run)
	if err != nil {
		return nil, outcome, err
	}

	updated := fetch
	if !rep.OK {
		updated.Result = fetch.Result + "\n\n" + failureNote(rep)
		return &updated, outcome, nil
	}
	outcome.Opened, outcome.Source = true, rep.Source
	reason := outcome.Reason
	if fetch.CodeText != "" && fetch.Code != 200 {
		reason += " " + fetch.CodeText
	}
	header := fmt.Sprintf("WebFetch получил отказ сайта (%s). Страницу открыл webtext (%s). "+
		"Промпт WebFetch к тексту не применён, ниже текст страницы как есть", reason, sourceLabel(rep))
	updated.Result = withText(header, text, rep.Path, resultRunes)
	updated.Bytes = len(text)
	return &updated, outcome, nil
}

// FailureContext зовёт webtext на упавший WebFetch (сеть, TLS, таймаут, отказ
// домена самим Claude Code) и возвращает additionalContext для
// PostToolUseFailure: заменить результат там нельзя, текст страницы идёт
// рядом с ошибкой. Ошибка — утилита не сработала, контекста нет
func FailureContext(ctx context.Context, rawURL, toolErr string, run Runner) (string, Outcome, error) {
	outcome := Outcome{Reason: FailureReason(toolErr)}
	rep, text, err := open(ctx, rawURL, false, run)
	if err != nil {
		return "", outcome, err
	}
	failed := fmt.Sprintf("WebFetch упал (%s).", outcome.Reason)
	if !rep.OK {
		return failed + " " + failureNote(rep), outcome, nil
	}
	outcome.Opened, outcome.Source = true, rep.Source
	header := fmt.Sprintf("%s Страницу открыл webtext (%s). Промпт WebFetch к тексту не применён, ниже текст страницы как есть",
		failed, sourceLabel(rep))
	// Пометка об обрезке длиннее всего с путём и двумя числами
	room := contextUnits - utf16Len(header) - utf16Len(truncNote(len(text), len(text), rep.Path))
	return withText(header, text, rep.Path, fitUTF16(text, room)), outcome, nil
}

// FailureReason — первая строка ошибки WebFetch, не длиннее errorRunes
func FailureReason(toolErr string) string {
	line := firstLine([]byte(toolErr))
	if runes := []rune(line); len(runes) > errorRunes {
		line = string(runes[:errorRunes]) + "…"
	}
	if line == "" {
		return "ошибка без текста"
	}
	return line
}

// open зовёт webtext и читает текст открытой страницы; rep.OK == false —
// webtext страницу не открыл, его ступени в rep
func open(ctx context.Context, rawURL string, noDirect bool, run Runner) (report, string, error) {
	var rep report
	if err := checkRemote(rawURL); err != nil {
		return rep, "", err
	}
	args := []string{"--json", "--timeout", webtextTimeout}
	if noDirect {
		args = append(args, "--no-direct")
	}
	stdout, code, err := run(ctx, append(args, rawURL))
	if err != nil {
		return rep, "", err
	}
	if code != 0 && code != 1 {
		return rep, "", fmt.Errorf("webtext exit %d: %s", code, firstLine(stdout))
	}
	if err := json.Unmarshal(stdout, &rep); err != nil {
		return rep, "", fmt.Errorf("webtext output: %w", err)
	}
	if !rep.OK {
		return rep, "", nil
	}
	text, err := os.ReadFile(rep.Path)
	if err != nil {
		return rep, "", fmt.Errorf("webtext text: %w", err)
	}
	return rep, string(text), nil
}

// Имена локальной сети: RFC 6761 (localhost), RFC 6762 (local), RFC 8375
// (home.arpa) и принятые в домашних и офисных сетях
var localSuffixes = []string{"localhost", "local", "lan", "internal", "localdomain", "home.arpa"}

// cgnat — общая адресация провайдера (RFC 6598): net.IP.IsPrivate её не знает
var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// checkRemote отказывает адресу без точки в имени (localhost, имя в
// локальной сети), имени локальной зоны, IP петли, частной или локальной
// сети и IP в сокращённой записи (127.1, 0x7f.0.0.1): у публичного домена
// последняя часть имени с цифры не начинается
func checkRemote(rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("webfetch url: %w", err)
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || cgnat.Contains(ip) {
			return fmt.Errorf("%w: %s", ErrLocalAddress, host)
		}
		return nil
	}
	dot := strings.LastIndex(host, ".")
	if dot < 0 || dot == len(host)-1 || host[dot+1] >= '0' && host[dot+1] <= '9' {
		return fmt.Errorf("%w: %q", ErrLocalAddress, host)
	}
	for _, suffix := range localSuffixes {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return fmt.Errorf("%w: %q", ErrLocalAddress, host)
		}
	}
	return nil
}

// sourceLabel — откуда текст: ступень webtext, дата снимка архива, вид
func sourceLabel(rep report) string {
	source := rep.Source
	if rep.Snapshot != "" {
		source += " " + rep.Snapshot
	}
	if rep.Kind != "" {
		source += ", " + rep.Kind
	}
	return source
}

// withText — заголовок и первые keep символов текста; не влезший текст —
// ссылкой на файл в заголовке
func withText(header, text, path string, keep int) string {
	runes := []rune(text)
	if len(runes) <= keep {
		return header + ".\n\n" + text
	}
	return header + truncNote(keep, len(runes), path) + string(runes[:keep])
}

func truncNote(kept, total int, path string) string {
	return fmt.Sprintf(": первые %d из %d символов, целиком — %s.\n\n", kept, total, path)
}

// fitUTF16 — сколько первых символов текста укладывается в room единиц UTF-16
func fitUTF16(text string, room int) int {
	kept, used := 0, 0
	for _, r := range text {
		used += utf16.RuneLen(r)
		if used > room {
			break
		}
		kept++
	}
	return kept
}

// utf16Len — длина строки так, как её считает Claude Code (JavaScript)
func utf16Len(s string) int {
	return len(utf16.Encode([]rune(s)))
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
