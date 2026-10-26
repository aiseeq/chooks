package core

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// SessionState описывает, чем занята сессия Claude Code.
// Строка статуса рисуется отдельным процессом и сама этого знать не может,
// поэтому состояние записывают хуки
type SessionState string

const (
	StateWorking SessionState = "working"
	StateWaiting SessionState = "waiting"
	// StateDone — ход закончен, а человек его ещё не видел: вкладка не
	// открывалась с конца хода
	StateDone SessionState = "done"
	// StateIdle — ход закончен и человек открыл вкладку после этого
	StateIdle SessionState = "idle"
	// StatePaused — Claude остановился, но ждёт фоновые задачи или будильник
	// и вернётся к работе сам. Для человека это всё ещё работа
	StatePaused SessionState = "paused"
	// StateUnknown — запись состояния не прочиталась. Хранилищу не пишется:
	// им вызывающий явно помечает, что предыдущее состояние не получено
	StateUnknown SessionState = "unknown"
)

// Marker — значок состояния во вкладке терминала. Значки выбрал пользователь
// 26.10 по каталогу: «1 ⚙», «72 🔔», «99 ✨», «134 ✅»; «ждёт свои задачи»
// сначала слили с «работает», потом вернули отдельным: «Тогда 35 ⏸» — по
// нему видно, что ход кончился, а держит сессию висящая задача
func (s SessionState) Marker() string {
	switch s {
	case StateWaiting:
		return "🔔"
	case StatePaused:
		return "⏸️"
	case StateDone:
		return "✨"
	case StateIdle:
		return "✅"
	default:
		return "⚙️"
	}
}

// stateTTL определяет, как долго запись считается актуальной.
// Сессии завершаются без уведомления, поэтому старые файлы просто устаревают
const stateTTL = 24 * time.Hour

// SaveSessionState запоминает состояние сессии и попутно вычищает записи
// давно завершившихся сессий. Пустой идентификатор — событие без сессии,
// запоминать нечего
func SaveSessionState(sessionID string, state SessionState) error {
	if sessionID == "" {
		return nil
	}

	path, err := sessionStatePath(sessionID)
	if err != nil {
		return err
	}
	err = withStateLock(func() error {
		if err := os.WriteFile(path, []byte(state), 0o644); err != nil {
			return fmt.Errorf("cannot write session state: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return cleanupStaleStates(filepath.Dir(path))
}

// SwapSessionState меняет состояние сессии на to, только если сейчас оно
// from; true — заменено. Строка статуса и хуки пишут состояние из разных
// процессов: без сверки под блокировкой «посмотрел» затёр бы «работает»
// от только что отправленного сообщения
func SwapSessionState(sessionID string, from, to SessionState) (bool, error) {
	if sessionID == "" {
		return false, nil
	}
	path, err := sessionStatePath(sessionID)
	if err != nil {
		return false, err
	}
	swapped := false
	err = withStateLock(func() error {
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("cannot read session state: %w", err)
		}
		if SessionState(strings.TrimSpace(string(data))) != from {
			return nil
		}
		if err := os.WriteFile(path, []byte(to), 0o644); err != nil {
			return fmt.Errorf("cannot write session state: %w", err)
		}
		swapped = true
		return nil
	})
	return swapped, err
}

// withStateLock выполняет fn под блокировкой каталога состояний
func withStateLock(fn func() error) error {
	dir := stateDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("cannot create session state dir: %w", err)
	}
	lock, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("cannot open session state dir: %w", err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("cannot lock session state dir: %w", err)
	}
	return fn()
}

// LoadSessionState читает состояние сессии. Для сессии без записи
// возвращается StateWorking: раз хук ещё не отработал, работа идёт.
// Нечитаемая запись — ошибка без состояния: чем её заменить, решает вызывающий
func LoadSessionState(sessionID string) (SessionState, error) {
	state, recorded, err := LookupSessionState(sessionID)
	if err != nil {
		return "", err
	}
	if !recorded {
		return StateWorking, nil
	}
	return state, nil
}

// LookupSessionState читает состояние и сообщает, была ли запись. Записи нет
// у сессии, где ещё не было ни одного запроса: новая или сброшенная /clear —
// у неё новый идентификатор. Пустой идентификатор — тоже без записи
func LookupSessionState(sessionID string) (SessionState, bool, error) {
	if sessionID == "" {
		return "", false, nil
	}

	path, err := sessionStatePath(sessionID)
	if err != nil {
		return "", false, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("cannot read session state: %w", err)
	}

	switch SessionState(strings.TrimSpace(string(data))) {
	case StateWaiting:
		return StateWaiting, true, nil
	case StateDone:
		return StateDone, true, nil
	case StatePaused:
		return StatePaused, true, nil
	case StateIdle:
		return StateIdle, true, nil
	default:
		return StateWorking, true, nil
	}
}

// SessionStateChangedSince — записал ли кто-то состояние сессии после since.
// Нужно хуку, который решает долго: за это время человек мог отправить новое
// сообщение, и решение по старому концу хода уже неверно. Сессия без записи
// не менялась
func SessionStateChangedSince(sessionID string, since time.Time) (bool, error) {
	if sessionID == "" {
		return false, nil
	}
	path, err := sessionStatePath(sessionID)
	if err != nil {
		return false, err
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("cannot stat session state: %w", err)
	}
	return info.ModTime().After(since), nil
}

// sessionStatePath возвращает путь к файлу состояния сессии
func sessionStatePath(sessionID string) (string, error) {
	// Имя сессии приходит извне и в путь попадать не должно
	safeID := filepath.Base(strings.TrimSpace(sessionID))
	if safeID == "" || safeID == "." || safeID == string(filepath.Separator) {
		return "", fmt.Errorf("invalid session id %q: %w", sessionID, os.ErrInvalid)
	}

	return filepath.Join(stateDir(), safeID), nil
}

// stateDir возвращает каталог для состояний сессий
func stateDir() string {
	return filepath.Join(RuntimeDir(), "sessions")
}

// RuntimeDir возвращает каталог для недолговечных файлов chooks.
// Каталог времени выполнения очищается при перезагрузке — это как раз то,
// что нужно состояниям сессий и блокировкам
func RuntimeDir() string {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, "chooks")
}

// cleanupStaleStates удаляет записи завершившихся сессий. Записи, которые
// не удалось убрать, перечисляются в ошибке; свежие пропускаются
func cleanupStaleStates(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("cannot list session states: %w", err)
	}

	deadline := time.Now().Add(-stateTTL)
	var errs []error
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			// Запись исчезла между чтением каталога и опросом: чистить нечего
			if os.IsNotExist(err) {
				continue
			}
			errs = append(errs, err)
			continue
		}
		if info.ModTime().After(deadline) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
