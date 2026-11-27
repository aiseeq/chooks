package core

import (
	"context"
	"path/filepath"
	"time"
)

// Обёртки от окружения своего процесса нужны только тестам: хук передаёт
// каталог состояния своего запроса в функции *In

// SaveSessionState — SaveSessionStateIn в каталоге своего процесса (только тесты)
func SaveSessionState(sessionID string, state SessionState) error {
	return SaveSessionStateIn(context.Background(), RuntimeDir(), sessionID, state)
}

// SwapSessionState — SwapSessionStateIn в каталоге своего процесса (только тесты)
func SwapSessionState(sessionID string, from, to SessionState) (bool, error) {
	return SwapSessionStateIn(context.Background(), RuntimeDir(), sessionID, from, to)
}

// LoadSessionState — LoadSessionStateIn в каталоге своего процесса (только тесты)
func LoadSessionState(sessionID string) (SessionState, error) {
	return LoadSessionStateIn(RuntimeDir(), sessionID)
}

// LookupSessionState — LookupSessionStateIn в каталоге своего процесса (только тесты)
func LookupSessionState(sessionID string) (SessionState, bool, error) {
	return LookupSessionStateIn(RuntimeDir(), sessionID)
}

// sessionStatePath возвращает путь к файлу состояния сессии
func sessionStatePath(sessionID string) (string, error) {
	return sessionStatePathIn(stateDir(), sessionID)
}

// stateDir возвращает каталог для состояний сессий
func stateDir() string {
	return filepath.Join(RuntimeDir(), "sessions")
}

// RecordBackground — RecordBackgroundIn в каталоге своего процесса (только тесты)
func RecordBackground(sessionID string, tasks []BackgroundTask, crons []SessionCron, now time.Time) ([]BackgroundTask, error) {
	return RecordBackgroundIn(RuntimeDir(), sessionID, tasks, crons, now)
}

// LoadBackground — LoadBackgroundIn в каталоге своего процесса (только тесты)
func LoadBackground(sessionID string) (Background, error) {
	return LoadBackgroundIn(RuntimeDir(), sessionID)
}
