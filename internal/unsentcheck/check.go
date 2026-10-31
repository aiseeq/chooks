// Package unsentcheck — проверка конца хода: ответ пользователю говорит, что
// текст человеку не отправлен, а чат в этом ходе не открывали. Такой статус —
// утверждение без проверки (пользователь 29.10 после двух ложных «не
// отправлен» у work-admin: «Эх, опять утверждение без проверки =(»)
package unsentcheck

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"

	"github.com/aiseeq/akit/shellcmd"
	"github.com/aiseeq/akit/transcript"
)

// claim — фраза о неотправленном тексте. Из сухого прогона work-admin 29.10
// (unsent.ts), кроме полной формы причастия: «набранный, но не отправленный
// текст» описывает, а не сообщает статус (ложная остановка 24.10), и «не»
// внутри слова («ответ Ане отправлен») — не отрицание. RE2 не знает \b для
// кириллицы: границы — не-буква или край текста
var claim = regexp.MustCompile(`(?i)(?:ещё|еще) не отправлено|(?:^|\P{L})ты (?:(?:ещё|еще) )?не отправил|(?:ответ|текст|черновик|сообщение|вопрос)\S*[^.\n]{0,60}?(?:^|\P{L})не (?:отправлен[оаы]?(?:\P{L}|$)|ушл|ушёл|ушел)|(?:^|\P{L})не отправлен[оаы]? (?:ответ|текст|черновик|вопрос)`)

// chatCall — открывает ли команда Bash чат: вызов reddy (через обёртки,
// bash -c, переменную с путём), первое слово после флагов — history. Слова
// reddy и history в тексте правки, тикета или промпта — не вызов (ложный
// пропуск 29.10 12:15 у chooks-master)
func chatCall(command string) bool {
	calls, err := shellcmd.Find(command, "reddy")
	var nested *shellcmd.NestedError
	if err != nil && !errors.As(err, &nested) {
		return false // shellcmd не разбирает только то, что отвергает и bash -n: такую команду оболочка не выполнила, чат не открывался
	}
	for _, call := range calls {
		for _, arg := range call.Args {
			if arg != "" && arg[0] != '-' {
				if arg == "history" {
					return true
				}
				break
			}
		}
	}
	return false
}

// Claim — первая фраза о неотправленном тексте в ответе, "" — её нет
func Claim(answer string) string {
	return claim.FindString(answer)
}

// Result — что хук отдаёт Claude Code
type Result struct {
	// Block — остановить конец хода, Reason — что агент прочтёт
	Block  bool
	Reason string
	// Claim — найденная фраза, "" — фразы нет и проверять нечего
	Claim string
	// Skip — почему проверка не делалась (для лога хука)
	Skip string
}

// Event — вход Stop, нужный проверке
type Event struct {
	TranscriptPath string
	AgentID        string
	StopHookActive bool
	LastMessage    string
}

// Run проверяет конец хода. Повторный Stop после остановки
// (stop_hook_active) пропускается: подсказка уже была, а ложная фраза (письмо
// сессии, рассказ о механизме) иначе держала бы ход по кругу
func Run(event Event) (Result, error) {
	switch {
	case event.AgentID != "":
		return Result{Skip: "subagent"}, nil
	case event.StopHookActive:
		return Result{Skip: "stop hook active"}, nil
	}
	phrase := Claim(event.LastMessage)
	if phrase == "" {
		return Result{}, nil
	}
	checked, err := ChatOpened(event.TranscriptPath)
	if errors.Is(err, os.ErrNotExist) {
		// `claude -p --no-session-persistence` (ночные шаги Сна) передаёт путь,
		// но транскрипт не пишет: проверять нечего, это не сбой
		return Result{Claim: phrase, Skip: "no transcript"}, nil
	}
	if err != nil {
		return Result{Claim: phrase}, err
	}
	if checked {
		return Result{Claim: phrase}, nil
	}
	return Result{Block: true, Claim: phrase, Reason: blockReason(phrase)}, nil
}

func blockReason(phrase string) string {
	return fmt.Sprintf("В ответе сказано, что текст человеку не отправлен («%s»), а чат в этом ходе не открывался: `reddy history` не вызывался. "+
		"Открой чат адресата (`reddy history <чат>`), проверь по истории, ушёл ли текст, и напиши статус по ней. "+
		"Если фраза не про текст человеку в чате (письмо сессии, рассказ о механизме) — закончи ход как есть", phrase)
}

// Окно чтения хвоста: растёт вдвое, пока не найдено начало хода, как у
// question-check — транскрипт бывает в сотни МБ
const (
	tailWindowStart = 1 << 20
	tailWindowMax   = 32 << 20
)

// ChatOpened — вызывался ли в текущем ходе Bash с `reddy … history`. Ход —
// от последней реплики человека, письма сессии или запроса программы
// (`claude -p`, SDK: запись без origin); начала хода нет и в предельном
// окне — смотрится всё окно
func ChatOpened(path string) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("open transcript: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return false, fmt.Errorf("stat transcript: %w", err)
	}
	size := info.Size()
	for window := int64(tailWindowStart); ; window *= 2 {
		window = min(window, size)
		buf := make([]byte, window)
		if _, err := file.ReadAt(buf, size-window); err != nil && err != io.EOF {
			return false, fmt.Errorf("read transcript tail: %w", err)
		}
		// Первая строка окна обрезана, если окно не с начала файла
		if window < size {
			if i := bytes.IndexByte(buf, '\n'); i >= 0 {
				buf = buf[i+1:]
			}
		}
		checked, turnFound := scanTurn(buf)
		if checked || turnFound || window >= size || window >= tailWindowMax {
			return checked, nil
		}
	}
}

// scanTurn идёт по строкам с конца до начала хода
func scanTurn(buf []byte) (checked, turnFound bool) {
	lines := bytes.Split(buf, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		line := lines[i]
		// Разбор JSON только у записей разговора: в хвосте мегабайты служебных
		// записей (api-request-blob)
		if !bytes.Contains(line, []byte(`"type":"assistant"`)) && !bytes.Contains(line, []byte(`"type":"user"`)) && !bytes.Contains(line, []byte(`"type":"attachment"`)) {
			continue
		}
		// Нечитаемая строка — обрезанная или дописываемая — без событий
		if events, err := transcript.NewParser().Parse(line); err == nil {
			if checked, turnFound := scanEvents(events); turnFound {
				return checked, true
			}
		}
	}
	return false, false
}

// scanEvents идёт по событиям одной записи с конца: вызов reddy history или
// начало хода
func scanEvents(events []transcript.Event) (checked, turnFound bool) {
	for j := len(events) - 1; j >= 0; j-- {
		event := &events[j]
		switch {
		case event.Sidechain:
		case event.Role == transcript.User || event.Role == transcript.Peer || (event.Role == transcript.Note && event.Who == "prompt"):
			return false, true
		case event.Role == transcript.Cmd && event.Who == "Bash" && chatCall(event.Text):
			return true, true
		}
	}
	return false, false
}
