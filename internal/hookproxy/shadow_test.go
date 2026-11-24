package hookproxy

import (
	"errors"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Тень (этап 2б-3а): исход отправки проверяется по признаку — этап, errno и
// число построений кадра, — а не по стене часов: под нагрузкой гейта срок в
// мс не меряется. Срок, которого нет, виден как зависание: shadowGuard в
// сотни раз больше shadowTestTimeout. Замер в мс — scripts/daemon timing
// --mode shadow (мера 3)
const (
	shadowTestTimeout = 30 * time.Millisecond
	shadowGuard       = 20 * time.Second
)

type shadowOutcome struct {
	err    error
	frames int
}

// sendShadowGuarded — SendShadow с кадром size байт; не вернулся к
// shadowGuard — срока у этапа нет
func sendShadowGuarded(t *testing.T, path string, size int) shadowOutcome {
	t.Helper()
	done := make(chan shadowOutcome, 1)
	go func() {
		frames := 0
		err := SendShadow(path, shadowTestTimeout, func() ([]byte, error) {
			frames++
			return make([]byte, size), nil
		})
		done <- shadowOutcome{err: err, frames: frames}
	}()
	select {
	case o := <-done:
		return o
	case <-time.After(shadowGuard):
		t.Fatalf("SendShadow did not return in %s: a stage has no deadline", shadowGuard)
		return shadowOutcome{}
	}
}

func wantStage(t *testing.T, o shadowOutcome, stage string, errno syscall.Errno, frames int) {
	t.Helper()
	var se *ShadowError
	if !errors.As(o.err, &se) || se.Stage != stage || !errors.Is(o.err, errno) || o.frames != frames {
		t.Fatalf("err %v (stage %+v), frames %d; want stage %s, %v, frames %d", o.err, se, o.frames, stage, errno, frames)
	}
}

// rawListener — сокет демона без accept с очередью приёма backlog
func rawListener(t *testing.T, backlog int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "d.sock")
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Close(fd) })
	if err := syscall.Bind(fd, &syscall.SockaddrUnix{Name: path}); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Listen(fd, backlog); err != nil {
		t.Fatal(err)
	}
	return path
}

// Сокета нет: ошибка dial без ожидания, кадр не строится
func TestSendShadowNoSocket(t *testing.T) {
	o := sendShadowGuarded(t, filepath.Join(t.TempDir(), "none.sock"), 16)
	wantStage(t, o, ShadowDial, syscall.ENOENT, 0)
}

// Очередь приёма полна (демон не принимает): connect ждёт срок dial и
// кончается EAGAIN, кадр не строится. Без срока dial connect висел бы
func TestSendShadowAcceptQueueFull(t *testing.T) {
	path := rawListener(t, 0)
	filled := 0
	for range 64 {
		fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC|syscall.SOCK_NONBLOCK, 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = syscall.Close(fd) })
		if err := syscall.Connect(fd, &syscall.SockaddrUnix{Name: path}); errors.Is(err, syscall.EAGAIN) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		filled++
	}
	if filled == 0 || filled == 64 {
		t.Fatalf("accept queue filled by %d connections", filled)
	}
	o := sendShadowGuarded(t, path, 16)
	wantStage(t, o, ShadowDial, syscall.EAGAIN, 0)
}

// Демон принял соединение и не читает, кадр больше буфера сокета: запись
// кончается сроком (ETIMEDOUT), кадр построен один раз. Без срока записи
// send висел бы на полном буфере
func TestSendShadowDaemonNotReading(t *testing.T) {
	path := rawListener(t, 16)
	o := sendShadowGuarded(t, path, 16<<20)
	wantStage(t, o, ShadowWrite, syscall.ETIMEDOUT, 1)
}

// Демон читает: кадр уходит целиком, ошибки нет
func TestSendShadowDelivered(t *testing.T) {
	path := rawListener(t, 16)
	if o := sendShadowGuarded(t, path, 1024); o.err != nil || o.frames != 1 {
		t.Fatalf("err %v frames %d", o.err, o.frames)
	}
}
