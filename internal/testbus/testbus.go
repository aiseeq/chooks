// Package testbus — молчащая шина D-Bus и счёт горутин и fd для тестов
// сроков обработчиков D-Bus (desktop, nighthint)
package testbus

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"
)

// Silent слушает unix-сокет по пути и абстрактный сокет, принимает
// подключения и молчит; возвращает адреса path, path=@ и abstract=
func Silent(t testing.TB) []string {
	t.Helper()
	dir := t.TempDir()
	sock := filepath.Join(dir, "bus")
	// Абстрактное имя общее на машину: pid отделяет параллельные пакеты
	name := "chooks-silent-" + strconv.Itoa(os.Getpid()) + "-" + filepath.Base(dir)
	for _, addr := range []string{sock, "@" + name} {
		l, err := net.Listen("unix", addr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = l.Close() })
		go func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					t.Errorf("test bus accept loop panicked: %v", recovered)
				}
			}()
			for {
				c, err := l.Accept()
				if err != nil {
					if !errors.Is(err, net.ErrClosed) {
						t.Errorf("test bus accept: %v", err)
					}
					return
				}
				go func() {
					if _, err := io.Copy(io.Discard, c); err != nil && !errors.Is(err, net.ErrClosed) {
						t.Errorf("test bus drain: %v", err)
					}
					if err := c.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
						t.Errorf("test bus close: %v", err)
					}
				}()
			}
		}()
	}
	return []string{"unix:path=" + sock, "unix:path=@" + name, "unix:abstract=" + name}
}

// Counts — число горутин и открытых fd процесса
func Counts() (goroutines, fds int) {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		panic(fmt.Errorf("read process fd directory: %w", err))
	}
	return runtime.NumGoroutine(), len(entries)
}

// AssertReleased ждёт до секунды, пока горутины и fd вернутся к g0 и f0
func AssertReleased(t testing.TB, label string, g0, f0 int) {
	t.Helper()
	until := time.Now().Add(time.Second)
	for {
		g, f := Counts()
		if g <= g0 && f <= f0 {
			return
		}
		if time.Now().After(until) {
			t.Fatalf("%s: goroutines/fds before %d/%d after %d/%d", label, g0, f0, g, f)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
