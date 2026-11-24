package desktop

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/aiseeq/chooks/internal/hookctx"
	"github.com/godbus/dbus/v5"
)

const (
	konsoleSessionIface = "org.kde.konsole.Session"

	// konsoleLocalTab — контекст формата заголовка для локальной сессии.
	// У удалённой (ssh) сессии формат свой, но хуки работают локально
	konsoleLocalTab = int32(0)

	// konsoleDisplayedTitle — роль показываемого заголовка, а не имени профиля
	konsoleDisplayedTitle = int32(1)
)

// SetTerminalTitleForRequest меняет заголовок окна терминала запроса.
//
// Konsole собирает заголовок по шаблону («%d : %n» по умолчанию) и escape-
// последовательность OSC при этом игнорирует, поэтому шаблон заменяется целиком
// через D-Bus. Для прочих терминалов остаётся OSC — его понимают почти все.
// Ошибка означает, что заголовок Konsole поставить не удалось; работу хука
// это не останавливает — решает вызывающий.
//
// Адрес вкладки в окружении наследуется, поэтому сессия claude, запущенная
// внутри tmux или другого claude, видит адрес чужой вкладки. Такая сессия идёт
// путём прочих терминалов, OSC в stderr: вкладку родителя он не трогает.
// Окружение, дерево процессов (от PID) и stderr — запроса
func SetTerminalTitleForRequest(title string, request hookctx.Context) error {
	return SetTerminalTitleWith(title, request, SetKonsoleTitle)
}

// KonsoleTitler ставит заголовок вкладки Konsole по адресу сессии D-Bus
type KonsoleTitler func(service, session, title string, request hookctx.Context) error

// SetKonsoleTitle — заголовок вкладки Konsole через шину сессии запроса
func SetKonsoleTitle(service, session, title string, request hookctx.Context) error {
	conn, err := requestSessionBus(context.Background(), request)
	if err != nil {
		return fmt.Errorf("session bus unavailable: %w", err)
	}
	defer conn.Close()
	return setKonsoleTitleOn(conn, service, session, title)
}

// SetTerminalTitleWith — SetTerminalTitleForRequest, где вкладку Konsole
// ставит konsole: тень демона подставляет запись вместо вызова D-Bus. Дерево
// процессов читается через ProcFS запроса
func SetTerminalTitleWith(title string, request hookctx.Context, konsole KonsoleTitler) error {
	title = sanitizeTitle(title)
	if title == "" {
		return nil
	}
	service := request.Getenv("KONSOLE_DBUS_SERVICE")
	session := request.Getenv("KONSOLE_DBUS_SESSION")
	if service != "" && session != "" {
		names, err := processNames(request.ProcFS(), request.PID)
		if err != nil {
			return fmt.Errorf("konsole tab owner unknown: %w", err)
		}
		if ownsKonsoleTab(names) {
			return konsole(service, session, title, request)
		}
	}
	fmt.Fprintf(request.Stderr, "\033]0;%s\007", title)
	return nil
}

// konsoleSession сообщает адрес D-Bus сессии Konsole, из которой запущен
// процесс, если это Konsole
func konsoleSession() (service, session string, ok bool) {
	service = os.Getenv("KONSOLE_DBUS_SERVICE")
	session = os.Getenv("KONSOLE_DBUS_SESSION")
	return service, session, service != "" && session != ""
}

func setKonsoleTitleOn(conn *dbus.Conn, service, session, title string) error {
	object := conn.Object(service, dbus.ObjectPath(session))

	// Шаблон переживёт перерисовку заголовка, но применяется не сразу:
	// Konsole пересобирает заголовок по событиям сессии. Поэтому текущий
	// заголовок задаётся ещё и напрямую
	if call := object.Call(konsoleSessionIface+".setTabTitleFormat", 0, konsoleLocalTab, title); call.Err != nil {
		return fmt.Errorf("konsole setTabTitleFormat: %w", call.Err)
	}
	if call := object.Call(konsoleSessionIface+".setTitle", 0, konsoleDisplayedTitle, title); call.Err != nil {
		return fmt.Errorf("konsole setTitle: %w", call.Err)
	}

	return nil
}

// ownsKonsoleTab сообщает, что вкладка из окружения принадлежит этой сессии:
// по цепочке процессов от хука вверх есть konsole и не больше одного claude.
// tmux отрывает цепочку от konsole, вложенный claude даёт второго claude
func ownsKonsoleTab(names []string) bool {
	konsole, claude := false, 0
	for _, name := range names {
		switch name {
		case "konsole":
			konsole = true
		case "claude":
			claude++
		}
	}
	return konsole && claude <= 1
}

// sanitizeTitle убирает из заголовка то, что исказит его при выводе:
// перевод строки обрывает escape-последовательность, а знак процента
// Konsole принимает за начало подстановки вроде %d или %n
func sanitizeTitle(title string) string {
	title = strings.Map(func(r rune) rune {
		if r == '%' {
			return -1
		}
		if r < ' ' || r == 0x7f {
			return ' '
		}
		return r
	}, title)

	return strings.TrimSpace(title)
}
