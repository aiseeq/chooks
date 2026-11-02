package core

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// ErrLockBusy — блокировку держит другой процесс дольше, чем можно ждать
var ErrLockBusy = errors.New("locked by another process")

// lockPoll — шаг ожидания блокировки
const lockPoll = 10 * time.Millisecond

// LockFile берёт исключительный flock файла и ждёт его не дольше deadline.
// Хук ограничен таймаутом Claude Code: ожидание без срока за процессом,
// который держит блокировку (под свопом или остановленным), съело бы весь
// таймаут, и Claude Code выбросил бы вывод хука
func LockFile(file *os.File, deadline time.Time) error {
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return fmt.Errorf("cannot lock %s: %w", file.Name(), err)
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("%s: %w", file.Name(), ErrLockBusy)
		}
		time.Sleep(lockPoll)
	}
}
