package desktop

import (
	"fmt"
	"os"
	"path"
	"strconv"

	"github.com/godbus/dbus/v5"
)

// SelectTab делает вкладку этого процесса текущей в её окне Konsole: окно
// по клику поднимает KWin, а вкладку в нём выбирает только сама Konsole
// (Window.setCurrentSession, пользователь 26.10: «переключает на окно где
// живёт консоль агента, но не прям на его вкладку»). Адрес вкладки — из
// окружения, его фоновый процесс уведомления наследует от хука. Не Konsole —
// выбирать нечего, без ошибки
func SelectTab(conn *dbus.Conn) error {
	service, session, ok := konsoleSession()
	window := os.Getenv("KONSOLE_DBUS_WINDOW")
	if !ok || window == "" {
		return nil
	}
	// Сессия — /Sessions/13, вкладку окно выбирает по её номеру
	id, err := strconv.Atoi(path.Base(session))
	if err != nil {
		return fmt.Errorf("konsole session %q: %w", session, err)
	}
	if call := conn.Object(service, dbus.ObjectPath(window)).Call("org.kde.konsole.Window.setCurrentSession", 0, int32(id)); call.Err != nil {
		return fmt.Errorf("konsole setCurrentSession: %w", call.Err)
	}
	return nil
}
