package desktop

import (
	"context"
	"fmt"
	"os"
	"path"
	"strconv"
	"time"

	"github.com/godbus/dbus/v5"
)

// visibleTimeout — сколько ждать ответа Konsole: строка статуса рисуется
// каждые несколько секунд, зависший D-Bus не должен её задерживать
const visibleTimeout = 300 * time.Millisecond

// TabVisible сообщает, видит ли человек вкладку этого процесса: она текущая
// в своём окне Konsole, а окно в фокусе. Не Konsole — false без ошибки:
// узнать, смотрел ли человек, там нечем
func TabVisible(ctx context.Context) (bool, error) {
	service, session, ok := konsoleSession()
	window := os.Getenv("KONSOLE_DBUS_WINDOW")
	if !ok || window == "" {
		return false, nil
	}
	conn, err := dbus.SessionBus()
	if err != nil {
		return false, fmt.Errorf("session bus unavailable: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, visibleTimeout)
	defer cancel()

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
