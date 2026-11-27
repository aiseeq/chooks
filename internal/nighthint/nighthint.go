// Package nighthint решает, когда сессии напомнить, что пользователь спит:
// вопрос ему ночью — тикетом админу пространства, а работа идёт дальше.
// Пользователь 15.11:
// «этот скилл должен автоматом применяться где-то после 22», «если хочешь
// что-то спросить - глянь на время. Если ночь - кидай свои вопросы в Сон»,
// «Когда я сплю я лочу комп. Обычно я доступен где-то до 22-23, а
// просыпаюсь в 8-9» (тикет chooks night-autonomous-work-hint). Признак «спит» —
// экран заблокирован или крышка закрыта, и местное время 22:00–09:00.
// Днём подсказка не включается. Адрес вопроса с 16.11 —
// админ пространства, не Сон: пользователь 16.11 — «Я буду отвечать на вопросы
// Админам пространств» (tl 13d1062b:15130), «можно всё в эту доку писать»
// (tl 13d1062b:15683); п.3 autonomous-work с этим адресом — «да» на текст
// (tl deac8a3d:18093), тикет night-hint-question-to-admin.
//
// SessionStart — раз за ночь на сессию и снова после сжатия и /clear.
// UserPromptSubmit — на каждом ходе, пока в сессии не вызван скилл
// autonomous-work, после вызова — раз за ночь: сессия после /clear получала
// подсказку на SessionStart, первый ход будило письмо другой сессии, и агент
// начинал со скилла роли (8 из 30 сессий 15–17.11, тикет
// night-hint-after-clear-first-turn). Признак вызова — запись хука
// skill-reload, не транскрипт; записи нет — скилл не вызван
package nighthint

import (
	"context"
	"errors"
	"fmt"
	"regexp"
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

// AutonomousSkill — скилл, который велит вызвать подсказка: пока он не вызван,
// UserPromptSubmit получает подсказку на каждом ходе
const AutonomousSkill = "autonomous-work"

// NightWorkSkill — скилл sc2go вместо AutonomousSkill: описание autonomous-work —
// «(в sc2go - `night-work`)», и сессии sc2go вызывают его; без него подсказка
// шла бы на каждом ходе всю ночь (тикет night-hint-night-work-budget)
const NightWorkSkill = "night-work"

// AutonomousSkills — скиллы, вызов любого из которых засчитывается как вызов
// AutonomousSkill
func AutonomousSkills() []string {
	return []string{AutonomousSkill, NightWorkSkill}
}

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

// Deps — время, признак блокировки экрана, записи сессий и признак вызова
// скилла; в тестах подменяются
type Deps struct {
	Now       func() time.Time
	Locked    func(ctx context.Context) (bool, error)
	LidClosed func(ctx context.Context) (bool, error)
	Store     Store
	// SkillCalled — вызван ли в сессии главного агента AutonomousSkill (или
	// другой из AutonomousSkills).
	// Ошибка или nil — неизвестно: UserPromptSubmit идёт по правилу «раз
	// за ночь»
	SkillCalled func(sessionID string) (bool, error)
}

// Result — подсказка для контекста агента или причина, почему её нет.
// LockErr и LidErr — признак не получен: подсказки нет, причина — в лог
type Result struct {
	Context          string
	Skip             string
	LockErr          error
	LidErr           error
	PresenceSkipInfo bool
	// Damaged — запись сессии была испорчена и заменена (ErrRecordDamaged):
	// решение принято, причина — в лог
	Damaged error
	// SkillErr — вызов AutonomousSkill не узнать: UserPromptSubmit пошёл по
	// правилу «раз за ночь», причина — в лог
	SkillErr error
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
	return fmt.Sprintf("Сейчас ночь (%s), пользователь спит: первым действием хода вызови Skill autonomous-work, если его текста нет в контексте; вопрос — тикетом админу пространства (autonomous-work п.3), работай дальше", t.Format("15:04"))
}

// questionLine — строка, которая начинается меткой ❓: после отступа и
// маркера списка (`- `, `* `, `+ `, `N. `) и открытого выделения (`**❓`,
// `__❓`); знак внутри строки — упоминание, не вопрос; цитата `> ❓` —
// черновик чужого вопроса (тикет nighthint-qmark-line-start)
var questionLine = regexp.MustCompile(`(?m)^[ \t]*(?:[-*+][ \t]+|\d+[.)][ \t]+)?(?:\*\*|__)?` + regexp.QuoteMeta(questionMark))

// HasQuestion сообщает, что в ответе агента есть строка, начинающаяся меткой ❓
func HasQuestion(message string) bool {
	return questionLine.MatchString(message)
}

// Run решает подсказку по событию. Дешёвые признаки (событие, час) — до
// вызова признака блокировки: днём шина не спрашивается
func Run(ctx context.Context, deps Deps, event Event) (Result, error) {
	deps.Store.Context = ctx
	precheck, skip, err := precheckEvent(deps, event)
	if err != nil || skip {
		return precheck, err
	}
	damaged := precheck.Damaged

	now := deps.Now()
	if !IsNight(now) {
		return Result{Skip: "день", Damaged: damaged}, nil
	}
	locked, err := deps.Locked(ctx)
	if err != nil {
		return Result{Skip: "признак блокировки экрана не получен", LockErr: err, Damaged: damaged}, nil
	}
	if !locked {
		if deps.LidClosed != nil {
			closed, lidErr := deps.LidClosed(ctx)
			if lidErr != nil {
				return Result{Skip: "признак крышки не получен", LidErr: lidErr, Damaged: damaged}, nil
			}
			locked = closed
		}
		if !locked {
			first, markErr := deps.Store.MarkPresenceSkip(event.SessionID, NightOf(now))
			if errors.Is(markErr, ErrRecordDamaged) {
				damaged, markErr = markErr, nil
			}
			if markErr != nil {
				return Result{}, fmt.Errorf("mark presence skip: %w", markErr)
			}
			return Result{Skip: "экран не заблокирован и крышка открыта", PresenceSkipInfo: first, Damaged: damaged}, nil
		}
	}

	fresh, err := markEvent(deps, event, now)
	if errors.Is(err, ErrRecordDamaged) {
		damaged, err = err, nil
	}
	if err != nil {
		return Result{}, fmt.Errorf("mark %s: %w", event.Name, err)
	}
	fresh, skillErr := repeatUntilSkill(deps, event, fresh)
	if !fresh {
		return Result{Skip: "подсказка уже дана (раз за ночь, на Stop — раз за ход)", Damaged: damaged, SkillErr: skillErr}, nil
	}
	return Result{Context: Text(now), Damaged: damaged, SkillErr: skillErr}, nil
}

func precheckEvent(deps Deps, event Event) (Result, bool, error) {
	var damaged error
	switch event.Name {
	case EventSessionStart:
	case EventUserPromptSubmit:
		// Новый ход снимает отметку подсказки на Stop: она держится только
		// до следующей реплики, чтобы продолжение после подсказки не
		// получало её по кругу
		if err := deps.Store.ResetStop(event.SessionID); err != nil {
			if !errors.Is(err, ErrRecordDamaged) {
				return Result{}, false, fmt.Errorf("reset stop mark: %w", err)
			}
			damaged = err
		}
	case EventPreToolUse:
		if event.ToolName != askTool {
			return Result{Skip: "не " + askTool}, true, nil
		}
	case EventStop:
		if event.StopHookActive {
			return Result{Skip: "повторный Stop (stop_hook_active)"}, true, nil
		}
		if !HasQuestion(event.LastMessage) {
			return Result{Skip: "в ответе нет строки с " + questionMark}, true, nil
		}
	default:
		return Result{}, false, fmt.Errorf("%w %q", ErrUnsupportedEvent, event.Name)
	}
	return Result{Damaged: damaged}, false, nil
}

func markEvent(deps Deps, event Event, now time.Time) (bool, error) {
	fresh := true
	var err error
	switch event.Name {
	case EventSessionStart, EventUserPromptSubmit:
		// Ночь отмечается всегда: после вызова скилла UserPromptSubmit идёт
		// по ней — раз за ночь, письма сессий тоже приходят его ходом, и
		// подсказка на каждое забила бы контекст
		fresh, err = deps.Store.MarkNight(event.SessionID, NightOf(now))
		fresh = fresh || event.contextReset()
	case EventStop:
		fresh, err = deps.Store.MarkStop(event.SessionID)
	}
	return fresh, err
}

// repeatUntilSkill — давать ли подсказку на UserPromptSubmit, когда ночь уже
// отмечена: подсказка этой ночи была, но скилл не вызван — ход по письму
// сессии после /clear начинается со скилла роли, и подсказка на SessionStart
// до него не доходит, поэтому повтор на этом ходе. Признака нет — решение
// прежнее (раз за ночь), причина — вторым значением
func repeatUntilSkill(deps Deps, event Event, fresh bool) (bool, error) {
	if fresh || event.Name != EventUserPromptSubmit {
		return fresh, nil
	}
	called, err := skillCalled(deps, event.SessionID)
	if err != nil {
		return false, err
	}
	return !called, nil
}

// ErrSkillUnknown — признака вызова скилла нет (не подключён или выключен):
// UserPromptSubmit идёт по правилу «раз за ночь»
var ErrSkillUnknown = errors.New("skill call check off")

// skillCalled — вызван ли AutonomousSkill в сессии; без признака — ошибка
func skillCalled(deps Deps, sessionID string) (bool, error) {
	if deps.SkillCalled == nil {
		return false, ErrSkillUnknown
	}
	return deps.SkillCalled(sessionID)
}
