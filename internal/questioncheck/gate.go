package questioncheck

import (
	"github.com/aiseeq/chooks/internal/core"
)

// Scope — чьи концы хода проверка берёт на себя
type Scope struct {
	ClaudeDir string
	Targets   []Target
}

// GateStatus — что проверка сделает с концом хода
type GateStatus int

const (
	// GateSkip — не её конец хода: субагент, чужая сессия, нет вопроса
	GateSkip GateStatus = iota
	// GateAfterBlock — ход после остановки: только запись в журнал
	GateAfterBlock
	// GateJudge — проверка позовёт судью
	GateJudge
)

// Gate — решение, звать ли судью, и из чего оно сделано
type Gate struct {
	Status    GateStatus
	Skip      string
	Target    Target
	Questions []Question
}

// Gate решает, позовёт ли проверка судью. Им же пользуется Stop-хук
// оповещений: на GateJudge он не звонит, звонит question-check после
// вердикта. Транскрипт не читается — только вход хука и запись сессии.
// Срока нет: проба кончилась 07.11, и пользователь оставил проверку как есть
// (07.11 «Вроде бы фича полезная и работает», decisions.md
// question-checker-tune)
func (s Scope) Gate(event Event) Gate {
	if event.AgentID != "" {
		return Gate{Skip: "субагент"}
	}
	name, err := SessionName(s.ClaudeDir, event.ClaudePID)
	if err != nil {
		return Gate{Skip: "имя сессии не прочитано: " + err.Error()}
	}
	target, ok := MatchTarget(s.Targets, name, event.TranscriptPath)
	if !ok {
		return Gate{Skip: "не своя сессия: " + name}
	}

	gate := Gate{Target: target, Questions: Questions(event.LastMessage)}
	switch {
	case event.StopHookActive:
		gate.Status = GateAfterBlock
		gate.Skip = "stop_hook_active"
	case len(gate.Questions) == 0:
		gate.Skip = "вопроса нет"
	default:
		gate.Status = GateJudge
	}
	return gate
}

// HookSubcommand — подкоманда, по которой проверка видна в settings.json:
// последнее слово команды хука, путь к бинарю не важен
const HookSubcommand = "question-check"

// Registered — стоит ли проверка в Stop-хуках settings.json. Stop-хук
// оповещений отдаёт ей звонок только тогда: установленный бинарник без
// записи в settings.json иначе заглушил бы конец хода, и не позвонил бы никто
func Registered(settingsPath string) (bool, error) {
	// Ошибка в другом событии не отменяет найденную запись в Stop
	hooks, err := core.SettingsHooks(settingsPath)
	for _, hook := range hooks {
		if hook.Event == "Stop" && hook.Subcommand == HookSubcommand {
			return true, nil
		}
	}
	return false, err
}
