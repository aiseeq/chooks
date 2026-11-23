package claimcheck

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// DefaultSlots — сколько судей claim-check идут на машине одновременно: судья
// — codex с моделью у поставщика, а 16 мастеров кончают ход вместе (противник
// замысла, сценарий 5)
const DefaultSlots = 4

// slotPoll — как часто ждущий хук пробует места
const slotPoll = 100 * time.Millisecond

// errQueueTimeout — бюджет вышел раньше, чем освободилось место
var errQueueTimeout = errors.New("no judge slot within budget")

// acquireSlot занимает одно из n мест — файл slot-<i>.lock в dir под flock.
// Место держит открытый файл: процесс, упавший с занятым местом, освобождает
// его сам. Ждёт до конца ctx: дальше errQueueTimeout. Блокировки flock — у
// открытого файла, поэтому места делят и хуки в одном процессе (демон)
func acquireSlot(ctx context.Context, dir string, n int, poll time.Duration) (func(), error) {
	if n < 1 {
		return nil, fmt.Errorf("judge slots must be positive, got %d", n)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create slot dir: %w", err)
	}
	if poll <= 0 {
		poll = slotPoll
	}
	for {
		// С разных мест: ждущие не бьются все в slot-0
		first := rand.IntN(n)
		for k := range n {
			path := filepath.Join(dir, fmt.Sprintf("slot-%d.lock", (first+k)%n))
			file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
			if err != nil {
				return nil, fmt.Errorf("open judge slot: %w", err)
			}
			err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
			if err == nil {
				return func() { _ = file.Close() }, nil
			}
			_ = file.Close()
			if !errors.Is(err, unix.EWOULDBLOCK) {
				return nil, fmt.Errorf("lock judge slot: %w", err)
			}
		}
		timer := time.NewTimer(poll/2 + rand.N(poll))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, errQueueTimeout
		case <-timer.C:
		}
	}
}

// lockWithin берёт исключительную блокировку файла path, ожидая до конца
// ctx: дальше errQueueTimeout. Снимает её возвращённая функция. Файлы
// блокировок — в каталоге времени выполнения: их чистит выход из системы
func lockWithin(ctx context.Context, path string, poll time.Duration) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create lock dir: %w", err)
	}
	if poll <= 0 {
		poll = slotPoll
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock: %w", err)
	}
	for {
		err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return func() { _ = file.Close() }, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			return nil, errors.Join(fmt.Errorf("lock %s: %w", path, err), file.Close())
		}
		timer := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, errors.Join(errQueueTimeout, file.Close())
		case <-timer.C:
		}
	}
}

// withFileLock исполняет fn под исключительной блокировкой файла path:
// чтение-правка-запись общего состояния параллельными хуками
func withFileLock(path string, fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create lock dir: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open lock: %w", err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		return errors.Join(fmt.Errorf("lock %s: %w", path, err), file.Close())
	}
	// Блокировку снимает закрытие файла: в файл ничего не пишется
	return errors.Join(fn(), file.Close())
}
