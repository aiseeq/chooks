package desktop

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aiseeq/chooks/internal/hookctx"
)

// busListeners слушает unix-сокет bus в каждом каталоге и сообщает имя
// каталога, в чей сокет пришло подключение
func busListeners(t *testing.T, dirs ...string) <-chan string {
	t.Helper()
	hit := make(chan string, len(dirs))
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		listener, err := net.Listen("unix", filepath.Join(dir, "bus"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { listener.Close() })
		go func(name string) {
			conn, err := listener.Accept()
			if err == nil {
				hit <- name
				conn.Close()
			}
		}(filepath.Base(dir))
	}
	return hit
}

func connectedTo(t *testing.T, hit <-chan string) string {
	t.Helper()
	select {
	case got := <-hit:
		return got
	case <-time.After(time.Second):
		t.Fatal("no socket received a connection")
		return ""
	}
}

// Относительный unix:path= godbus открывает от cwd процесса; у запроса
// демона путь — от cwd запроса
func TestReviewRelativeDBusAddress(t *testing.T) {
	root := t.TempDir()
	daemon := filepath.Join(root, "daemon")
	request := filepath.Join(root, "request")
	hit := busListeners(t, daemon, request)
	t.Chdir(daemon)
	req := hookctx.New(nil, nil, nil, []string{"DBUS_SESSION_BUS_ADDRESS=unix:path=bus"}, request, 1, 2)
	conn, _ := requestSessionBus(context.Background(), req)
	if conn != nil {
		conn.Close()
	}
	if got := connectedTo(t, hit); got != "request" {
		t.Fatalf("D-Bus connected to %s/bus, want request/bus: executor cwd used", got)
	}
}

// У CLI адрес остаётся как дан, и ОС разрешает его от cwd процесса, как в базе
func TestRelativeDBusAddressProcess(t *testing.T) {
	root := t.TempDir()
	process := filepath.Join(root, "process")
	hit := busListeners(t, process)
	t.Chdir(process)
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=bus")
	req, err := hookctx.FromProcess()
	if err != nil {
		t.Fatal(err)
	}
	conn, _ := requestSessionBus(context.Background(), req)
	if conn != nil {
		conn.Close()
	}
	if got := connectedTo(t, hit); got != "process" {
		t.Fatalf("D-Bus connected to %s/bus, want process/bus", got)
	}
}
