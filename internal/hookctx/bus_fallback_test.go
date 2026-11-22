package hookctx

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

// Первый адрес списка открылся, но auth не прошёл (пир закрыл сокет):
// godbus к следующему адресу не переходит, ConnectBus тоже
func TestConnectBusNoFallbackAfterFailedAuth(t *testing.T) {
	dir := t.TempDir()
	closing, next := filepath.Join(dir, "closing"), filepath.Join(dir, "next")
	hits := map[string]chan struct{}{}
	for _, sock := range []string{closing, next} {
		l, err := net.Listen("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = l.Close() })
		hit := make(chan struct{}, 16)
		hits[sock] = hit
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				hit <- struct{}{}
				_ = c.Close()
			}
		}()
	}
	address := "unix:path=" + closing + ";unix:path=" + next
	if conn, err := dbus.Connect(address); err == nil {
		_ = conn.Close()
		t.Fatal("godbus connected through a peer that closes")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req := New(nil, nil, nil, nil, dir, 1, 2)
	if conn, err := req.ConnectBus(ctx, address); err == nil {
		_ = conn.Close()
		t.Fatal("ConnectBus connected through a peer that closes")
	}
	time.Sleep(50 * time.Millisecond)
	if got := len(hits[closing]); got != 2 {
		t.Errorf("first address dialed %d times, want 2 (godbus and ConnectBus)", got)
	}
	if got := len(hits[next]); got != 0 {
		t.Errorf("second address dialed %d times after a failed auth, want 0 as in godbus", got)
	}
}
