package desktop

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/aiseeq/chooks/internal/hookctx"
	"github.com/aiseeq/chooks/internal/testbus"
)

// privateSessionBus поднимает свой dbus-daemon на сокете во временном
// каталоге и возвращает его адрес: проверка не зависит от шины машины
func privateSessionBus(t *testing.T) string {
	t.Helper()
	daemon, err := exec.LookPath("dbus-daemon")
	if err != nil {
		t.Fatalf("dbus-daemon is required for the bus test: %v", err)
	}
	sock := filepath.Join(t.TempDir(), "bus")
	address := "unix:path=" + sock
	cmd := exec.Command(daemon, "--session", "--nofork", "--nopidfile", "--address="+address)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	until := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(sock); err == nil {
			return address
		}
		if time.Now().After(until) {
			t.Fatalf("dbus-daemon did not create %s", sock)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Соединение из requestSessionBus живёт после возврата: title.go зовёт по
// нему Konsole (setKonsoleTitleOn). Срок подключения не закрывает его
func TestRequestSessionBusConnUsable(t *testing.T) {
	address := privateSessionBus(t)
	req := hookctx.New(nil, nil, nil, []string{"DBUS_SESSION_BUS_ADDRESS=" + address}, "/", 1, 2)
	conn, err := requestSessionBus(req)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close()
	time.Sleep(visibleTimeout + 50*time.Millisecond)
	var id string
	if err := conn.BusObject().Call("org.freedesktop.DBus.GetId", 0).Store(&id); err != nil {
		t.Fatalf("call on returned connection: %v (connected=%v)", err, conn.Connected())
	}
	if id == "" {
		t.Fatal("empty bus id")
	}
}

// TabVisible целиком на молчащей шине: каждый вызов кончается не позже
// visibleTimeout + 100 мс с ошибкой, горутины и fd возвращаются
func TestTabVisibleSilentBus(t *testing.T) {
	for _, addr := range testbus.Silent(t) {
		g0, f0 := testbus.Counts()
		for range 20 {
			req := hookctx.New(nil, nil, nil, []string{"DBUS_SESSION_BUS_ADDRESS=" + addr,
				"KONSOLE_DBUS_SERVICE=:1.9", "KONSOLE_DBUS_SESSION=/Sessions/1", "KONSOLE_DBUS_WINDOW=/Windows/1"}, "/", 1, 2)
			start := time.Now()
			_, err := TabVisible(hookctx.WithRequest(context.Background(), req))
			if el := time.Since(start); el > visibleTimeout+100*time.Millisecond || err == nil {
				t.Fatalf("%s: err=%v elapsed=%v", addr, err, el)
			}
		}
		testbus.AssertReleased(t, addr, g0, f0)
	}
}
