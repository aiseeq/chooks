package nighthint

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aiseeq/chooks/internal/hookctx"
	"github.com/godbus/dbus/v5"
)

// lockTimeout — сколько ждать logind: хук идёт на каждый ход, таймаут хука —
// 3 секунды. Замер 15.11: два вызова системной шины — единицы миллисекунд
const lockTimeout = 500 * time.Millisecond

const (
	login1Service    = "org.freedesktop.login1"
	seat0Path        = dbus.ObjectPath("/org/freedesktop/login1/seat/seat0")
	seatInterface    = "org.freedesktop.login1.Seat"
	seatActiveProp   = "ActiveSession" // graft:leak-ok свойство D-Bus API logind (org.freedesktop.login1.Seat), не имя проекта
	sessionInterface = "org.freedesktop.login1.Session"
	propertiesGet    = "org.freedesktop.DBus.Properties.Get"
)

// errNoActiveSession — на seat0 нет активной сессии: экран некому
// показывать, признак блокировки неизвестен
var errNoActiveSession = errors.New("no active session on seat0")

// ScreenLocked — LockedHint активной сессии seat0 по системной шине logind.
// Его ставит kscreenlocker (SetLockedHint в libKScreenLocker) при блокировке
// и снимает при разблокировке. Системная шина, а не сессионная
// org.freedesktop.ScreenSaver: сессии, поднятые systemd или перезапуском,
// могут не видеть сессионную шину, а logind отвечает любому процессу
func ScreenLocked(ctx context.Context) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, lockTimeout)
	defer cancel()
	conn, err := connectSystemBus(ctx)
	if err != nil {
		return false, fmt.Errorf("connect system bus: %w", err)
	}
	defer conn.Close()

	var active dbus.Variant
	if err := conn.Object(login1Service, seat0Path).CallWithContext(ctx, propertiesGet, 0, seatInterface, seatActiveProp).Store(&active); err != nil {
		return false, fmt.Errorf("seat0 active session: %w", err)
	}
	var session struct {
		ID   string
		Path dbus.ObjectPath
	}
	if err := active.Store(&session); err != nil {
		return false, fmt.Errorf("seat0 active session %s: %w", active, err)
	}
	if session.ID == "" || !session.Path.IsValid() || session.Path == "/" {
		return false, errNoActiveSession
	}

	var hint dbus.Variant
	if err := conn.Object(login1Service, session.Path).CallWithContext(ctx, propertiesGet, 0, sessionInterface, "LockedHint").Store(&hint); err != nil {
		return false, fmt.Errorf("session %s LockedHint: %w", session.ID, err)
	}
	locked, ok := hint.Value().(bool)
	if !ok {
		return false, fmt.Errorf("session %s LockedHint %s: not a bool", session.ID, hint)
	}
	return locked, nil
}

// LidClosed возвращает LidClosed менеджера logind по системной шине.
func LidClosed(ctx context.Context) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, lockTimeout)
	defer cancel()
	conn, err := connectSystemBus(ctx)
	if err != nil {
		return false, fmt.Errorf("connect system bus: %w", err)
	}
	defer conn.Close()
	var hint dbus.Variant
	if err := conn.Object(login1Service, dbus.ObjectPath("/org/freedesktop/login1")).CallWithContext(ctx, propertiesGet, 0, "org.freedesktop.login1.Manager", "LidClosed").Store(&hint); err != nil {
		return false, fmt.Errorf("LidClosed: %w", err)
	}
	closed, ok := hint.Value().(bool)
	if !ok {
		return false, fmt.Errorf("LidClosed %s: not a bool", hint)
	}
	return closed, nil
}

func connectSystemBus(ctx context.Context) (*dbus.Conn, error) {
	if request, ok := hookctx.Request(ctx); ok {
		address := request.Getenv("DBUS_SYSTEM_BUS_ADDRESS")
		if address == "" {
			address = "unix:path=/var/run/dbus/system_bus_socket"
		}
		return dbus.Connect(request.BusAddress(address), dbus.WithContext(ctx))
	}
	return nil, fmt.Errorf("hook request is missing")
}
