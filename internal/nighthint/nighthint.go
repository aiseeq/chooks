// Package nighthint решает, когда сессии напомнить, что пользователь спит:
// вопрос ему ночью — тикетом админу пространства, а работа идёт дальше.
// Пользователь 15.11:
// «этот скилл должен автоматом применяться где-то после 22», «если хочешь
// что-то спросить - глянь на время. Если ночь - кидай свои вопросы в Сон»,
// «Когда я сплю я лочу комп. Обычно я доступен где-то до 22-23, а
// просыпаюсь в 8-9» (тикет chooks night-autonomous-work-hint). Признак «спит» —
// экран заблокирован и местное время 22:00–09:00: час без блокировки
// подсказку не включает, блокировка днём — тоже. Адрес вопроса с 16.11 —
// админ пространства, не Сон: пользователь 16.11 — «Я буду отвечать на вопросы
// Админам пространств» (tl 13d1062b:15130), «можно всё в эту доку писать»
// (tl 13d1062b:15683); п.3 autonomous-work с этим адресом — «да» на текст
// (tl deac8a3d:18093), тикет night-hint-question-to-admin
package nighthint

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Часы ночи по местному времени: с nightStartHour вечера до nightEndHour утра
const (
	nightStartHour = 22
	nightEndHour   = 9
)

// Имена событий Claude Code в hook_event_name, которые обслуживает подсказка
const (
	EventSessionStart     = "SessionStart"
	EventUserPromptSubmit = "UserPromptSubmit"
	EventPreToolUse       = "PreToolUse"
	EventStop             = "Stop"
)

// askTool — инструмент вопроса с выбором: ночью его вызов получает подсказку
const askTool = "AskUserQuestion"

// questionMark — метка строки «нужно от тебя» в ответе агента (CLAUDE.md,
// «Форма ответа»)
const questionMark = "❓"

// ErrUnsupportedEvent — событие, которого подсказка не обслуживает: ошибка
// регистрации подкоманды в settings.json
var ErrUnsupportedEvent = errors.New("night-hint: unsupported hook event")

// Event — поля stdin, по которым решается подсказка
type Event struct {
	Name           string
	SessionID      string
	ToolName       string
	LastMessage    string
	StopHookActive bool
	// Source — source у SessionStart: startup, resume, clear, compact
	Source string
}

// contextReset — SessionStart после сжатия или /clear (source есть только у
// SessionStart): прежняя подсказка ушла из контекста вместе с беседой, ночь
// её не отменяет
func (e Event) contextReset() bool {
	return e.Source == "compact" || e.Source == "clear"
}

// Deps — время, признак блокировки экрана и записи сессий; в тестах
// подменяются
type Deps struct {
	Now    func() time.Time
	Locked func(ctx context.Context) (bool, error)
	Store  Store
}

// Result — подсказка для контекста агента или причина, почему её нет.
// LockErr — признак блокировки не получен: подсказки нет, причина — в лог
type Result struct {
	Context string
	Skip    string
	LockErr error
	// Damaged — запись сессии была испорчена и заменена (ErrRecordDamaged):
	// решение принято, причина — в лог
	Damaged error
}

// IsNight сообщает, что t — ночные часы по местному времени
func IsNight(t time.Time) bool {
	hour := t.Hour()
	return hour >= nightStartHour || hour < nightEndHour
}

// NightOf — дата вечера, с которого началась ночь, содержащая t: ночь с
// 22:00 15.11 до 09:00 16.11 — «2026-11-15»
func NightOf(t time.Time) string {
	if t.Hour() < nightEndHour {
		t = t.AddDate(0, 0, -1)
	}
	return t.Format(time.DateOnly)
}

// Text — подсказка агенту на время t
func Text(t time.Time) string {
	return fmt.Sprintf("Сейчас ночь (%s): пользователь спит — скилл autonomous-work; вопрос — тикетом админу пространства (autonomous-work п.3), работай дальше", t.Format("15:04"))
}

// HasQuestion сообщает, что в ответе агента есть строка с меткой ❓
func HasQuestion(message string) bool {
	return strings.Contains(message, questionMark)
}

// Run решает подсказку по событию. Дешёвые признаки (событие, час) — до
// вызова признака блокировки: днём шина не спрашивается
func Run(ctx context.Context, deps Deps, event Event) (Result, error) {
	var damaged error
	switch event.Name {
	case EventSessionStart:
	case EventUserPromptSubmit:
		// Новый ход снимает отметку подсказки на Stop: она держится только
		// до следующей реплики, чтобы продолжение после подсказки не
		// получало её по кругу
		if err := deps.Store.ResetStop(event.SessionID); err != nil {
			if !errors.Is(err, ErrRecordDamaged) {
				return Result{}, fmt.Errorf("reset stop mark: %w", err)
			}
			damaged = err
		}
	case EventPreToolUse:
		if event.ToolName != askTool {
			return Result{Skip: "не " + askTool}, nil
		}
	case EventStop:
		if event.StopHookActive {
			return Result{Skip: "повторный Stop (stop_hook_active)"}, nil
		}
		if !HasQuestion(event.LastMessage) {
			return Result{Skip: "в ответе нет строки с " + questionMark}, nil
		}
	default:
		return Result{}, fmt.Errorf("%w %q", ErrUnsupportedEvent, event.Name)
	}

	now := deps.Now()
	if !IsNight(now) {
		return Result{Skip: "день", Damaged: damaged}, nil
	}
	locked, err := deps.Locked(ctx)
	if err != nil {
		return Result{Skip: "признак блокировки экрана не получен", LockErr: err, Damaged: damaged}, nil
	}
	if !locked {
		return Result{Skip: "экран не заблокирован", Damaged: damaged}, nil
	}

	fresh := true
	switch event.Name {
	case EventSessionStart, EventUserPromptSubmit:
		// Раз за ночь на сессию: письма сессий тоже приходят ходом
		// UserPromptSubmit, и подсказка на каждое забила бы контекст
		fresh, err = deps.Store.MarkNight(event.SessionID, NightOf(now))
		fresh = fresh || event.contextReset()
	case EventStop:
		fresh, err = deps.Store.MarkStop(event.SessionID)
	}
	// Испорченная запись читается пустой и даёт подсказку: повтора с ней
	// не бывает
	if errors.Is(err, ErrRecordDamaged) {
		damaged, err = err, nil
	}
	if err != nil {
		return Result{}, fmt.Errorf("mark %s: %w", event.Name, err)
	}
	if !fresh {
		return Result{Skip: "подсказка уже дана (раз за ночь, на Stop — раз за ход)"}, nil
	}
	return Result{Context: Text(now), Damaged: damaged}, nil
}
