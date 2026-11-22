package desktop

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aiseeq/chooks/internal/hookctx"
	"github.com/godbus/dbus/v5"
)

// visibleTimeout — сколько ждать ответа Konsole: строка статуса рисуется
// каждые несколько секунд, зависший D-Bus не должен её задерживать
const visibleTimeout = 300 * time.Millisecond

// TabVisible сообщает, видит ли человек вкладку этого процесса: она текущая
// в своём окне Konsole, а окно в фокусе. Не Konsole — false без ошибки:
// узнать, смотрел ли человек, там нечем
func TabVisible(ctx context.Context) (bool, error) {
	request, ok := hookctx.Request(ctx)
	if !ok {
		return false, fmt.Errorf("hook request is missing")
	}
	service := request.Getenv("KONSOLE_DBUS_SERVICE")
	session := request.Getenv("KONSOLE_DBUS_SESSION")
	window := request.Getenv("KONSOLE_DBUS_WINDOW")
	if service == "" || session == "" || window == "" {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, visibleTimeout)
	defer cancel()
	conn, err := requestSessionBusContext(ctx, request)
	if err != nil {
		return false, fmt.Errorf("session bus unavailable: %w", err)
	}
	defer conn.Close()

	var current int32
	if err := conn.Object(service, dbus.ObjectPath(window)).CallWithContext(ctx, "org.kde.konsole.Window.currentSession", 0).Store(&current); err != nil {
		return false, fmt.Errorf("konsole currentSession: %w", err)
	}
	// Сессия — /Sessions/13, текущая вкладка окна — её номер
	if strconv.Itoa(int(current)) != path.Base(session) {
		return false, nil
	}

	// Окно /Windows/1 — это виджет /konsole/MainWindow_1
	var active bool
	widget := conn.Object(service, dbus.ObjectPath("/konsole/MainWindow_"+path.Base(window)))
	if err := widget.CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0, "org.qtproject.Qt.QWidget", "isActiveWindow").Store(&active); err != nil {
		return false, fmt.Errorf("konsole isActiveWindow: %w", err)
	}
	return active, nil
}

func requestSessionBus(request hookctx.Context) (*dbus.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), visibleTimeout)
	defer cancel()
	return requestSessionBusContext(ctx, request)
}

func requestSessionBusContext(ctx context.Context, request hookctx.Context) (*dbus.Conn, error) {
	address := request.Getenv("DBUS_SESSION_BUS_ADDRESS")
	if address == "" || address == "autolaunch:" {
		var err error
		address, err = discoverRequestBus(ctx, request)
		if err != nil {
			return nil, err
		}
	}
	return request.ConnectBus(ctx, address)
}

// discoverRequestBus mirrors the usual Linux session-bus discovery without
// changing the daemon's environment or inheriting its PATH/cwd.
func discoverRequestBus(ctx context.Context, request hookctx.Context) (string, error) {
	runtime := filepath.Join("/run/user", strconv.Itoa(os.Getuid()))
	bus := filepath.Join(runtime, "bus")
	if _, err := os.Stat(bus); err == nil {
		return "unix:path=" + dbus.EscapeBusAddressValue(bus), nil
	}
	if data, err := os.ReadFile(filepath.Join(runtime, "dbus-session")); err == nil {
		if address, ok := strings.CutPrefix(string(data), "DBUS_SESSION_BUS_ADDRESS="); ok {
			return strings.TrimRight(address, "\n\r"), nil
		}
	}
	path, err := request.LookPath("dbus-launch")
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, path)
	cmd.WaitDelay = 20 * time.Millisecond
	cmd.Args[0] = "dbus-launch"
	cmd.Env = append([]string{}, request.Env...)
	cmd.Dir = request.ChildDir()
	data, err := cmd.CombinedOutput()
	if err != nil {
		return "", err
	}
	line, _, _ := strings.Cut(string(data), "\n")
	_, address, ok := strings.Cut(line, "=")
	if !ok || address == "" {
		return "", fmt.Errorf("dbus: couldn't determine address of session bus")
	}
	return address, nil
}
