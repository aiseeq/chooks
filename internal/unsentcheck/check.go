// Package unsentcheck — проверка конца хода: ответ пользователю говорит, что
// текст человеку не отправлен, а чат в этом ходе не открывали. Такой статус —
// утверждение без проверки (пользователь 29.10 после двух ложных «не
// отправлен» у work-admin: «Эх, опять утверждение без проверки =(»)
package unsentcheck

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/aiseeq/chooks/internal/tailscan"

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

// Run проверяет конец хода. Повторный Stop (stop_hook_active) пропускается,
// только если в этом ходе останавливал сам unsent-check: подсказка уже была,
// а ложная фраза (письмо сессии, рассказ о механизме) иначе держала бы ход по
// кругу. После остановки другим хуком (question-check) проверка в силе:
// продолжение может содержать новое утверждение (work-admin 23.10 11:08)
func Run(event Event) (Result, error) {
	if event.AgentID != "" {
		return Result{Skip: "subagent"}, nil
	}
	phrase := Claim(event.LastMessage)
	if phrase == "" {
		return Result{}, nil
	}
	turn, err := ReadTurn(event.TranscriptPath)
	if errors.Is(err, os.ErrNotExist) {
		// `claude -p --no-session-persistence` (ночные шаги Сна) передаёт путь,
		// но транскрипт не пишет: проверять нечего, это не сбой
		return Result{Claim: phrase, Skip: "no transcript"}, nil
	}
	if err != nil {
		return Result{Claim: phrase}, err
	}
	if turn.ChatOpened {
		return Result{Claim: phrase}, nil
	}
	if event.StopHookActive && turn.Blocked {
		return Result{Claim: phrase, Skip: "stop hook active"}, nil
	}
	return Result{Block: true, Claim: phrase, Reason: blockReason(phrase)}, nil
}

// blockMark — часть подсказки, по которой своя остановка узнаётся в
// транскрипте: Claude Code пишет её записью isMeta «Stop hook feedback»
const blockMark = "а чат в этом ходе не открывался"

func blockReason(phrase string) string {
	return fmt.Sprintf("В ответе сказано, что текст человеку не отправлен («%s»), "+blockMark+": `reddy history` не вызывался. "+
		"Открой чат адресата (`reddy history <чат>`), проверь по истории, ушёл ли текст, и напиши статус по ней. "+
		"Если фраза не про текст человеку в чате (письмо сессии, рассказ о механизме) — закончи ход как есть", phrase)
}

// Turn — что известно о текущем ходе из транскрипта
type Turn struct {
	// ChatOpened — Bash с `reddy … history` вызывался и не завершился ошибкой.
	// Код выхода — всё, что видно: `reddy history x | tail` при сбое reddy
	// выходит с нулём и считается открытием чата
	ChatOpened bool
	// Blocked — в ходе уже была подсказка unsent-check
	Blocked bool
}

// ReadTurn читает текущий ход. Ход — от последней реплики человека, письма
// сессии или запроса программы (`claude -p`, SDK: запись без origin); начала
// хода нет и в предельном окне — смотрится всё окно
func ReadTurn(path string) (Turn, error) {
	var turn Turn
	err := tailscan.Scan(path, func(buf []byte) bool {
		var turnFound bool
		turn, turnFound = scanTurn(buf)
		return turnFound
	})
	return turn, err
}

// scanTurn идёт по строкам с конца до начала хода или открытия чата. Вывод
// инструмента в транскрипте идёт после вызова, поэтому при обратном проходе
// ошибка известна раньше вызова
func scanTurn(buf []byte) (turn Turn, turnFound bool) {
	failed := map[string]bool{}
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
			if scanEvents(events, failed, &turn) {
				return turn, true
			}
		}
	}
	return turn, false
}

// scanEvents идёт по событиям одной записи с конца и копит сведения о ходе;
// true — дальше смотреть незачем: начало хода или открытый чат
func scanEvents(events []transcript.Event, failed map[string]bool, turn *Turn) bool {
	for j := len(events) - 1; j >= 0; j-- {
		event := &events[j]
		switch {
		case event.Sidechain:
		case event.Role == transcript.User || event.Role == transcript.Peer || (event.Role == transcript.Note && event.Who == "prompt"):
			return true
		case event.Role == transcript.Note && event.Who == "meta" && strings.Contains(event.Text, blockMark):
			turn.Blocked = true
		case event.Role == transcript.Out && event.Error:
			failed[event.CallID] = true
		case event.Role == transcript.Cmd && event.Who == "Bash" && !failed[event.CallID] && chatCall(event.Text):
			turn.ChatOpened = true
			return true
		}
	}
	return false
}
