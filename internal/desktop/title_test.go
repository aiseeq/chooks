package desktop

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSanitizeTitle(t *testing.T) {
	tests := []struct {
		name     string
		title    string
		expected string
	}{
		{name: "обычный заголовок", title: "🔵 DEMO · main", expected: "🔵 DEMO · main"},
		// Konsole принимает %d и %n за подстановки каталога и имени сессии
		{name: "подстановки Konsole", title: "100% DONE · %d", expected: "100 DONE · d"},
		// Перевод строки оборвал бы escape-последовательность OSC
		{name: "управляющие символы", title: "DEMO\nвторая строка", expected: "DEMO вторая строка"},
		{name: "пустой заголовок", title: "   ", expected: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitizeTitle(tt.title); got != tt.expected {
				t.Errorf("sanitizeTitle(%q) = %q, ожидалось %q", tt.title, got, tt.expected)
			}
		})
	}
}

// Цепочки замерены на живых сессиях 04.11: адрес вкладки наследуется, и только
// сессия этой вкладки может менять её заголовок
func TestOwnsKonsoleTab(t *testing.T) {
	tests := []struct {
		name  string
		chain []string
		want  bool
	}{
		{"своя сессия через cl", []string{"chooks", "claude", "cl", "bash", "konsole", "systemd"}, true},
		{"своя сессия с оболочкой хука", []string{"chooks", "sh", "claude", "bash", "konsole", "systemd"}, true},
		{"проба агента из Bash", []string{"chooks", "bash", "claude", "cl", "bash", "konsole", "systemd"}, true},
		{"claude в отдельном tmux", []string{"chooks", "claude", "cl", "bash", "tmux: server", "systemd"}, false},
		{"claude внутри claude", []string{"chooks", "claude", "script", "bash", "claude", "cl", "bash", "konsole", "systemd"}, false},
		{"терминал не Konsole", []string{"chooks", "claude", "bash", "foot", "systemd"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ownsKonsoleTab(tt.chain); got != tt.want {
				t.Errorf("ownsKonsoleTab(%v) = %v, ожидалось %v", tt.chain, got, tt.want)
			}
		})
	}
}

func TestProcessNamesStartsWithItself(t *testing.T) {
	names, err := processNames(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	self := filepath.Base(os.Args[0])
	if len(self) > 15 {
		self = self[:15]
	}
	if len(names) == 0 || names[0] != self {
		t.Errorf("processNames = %v, первым ожидалось %q", names, self)
	}
}

func TestKonsoleSessionWithoutKonsole(t *testing.T) {
	// Вне Konsole нужен запасной путь через OSC, а не попытка вызова D-Bus
	t.Setenv("KONSOLE_DBUS_SERVICE", "")
	t.Setenv("KONSOLE_DBUS_SESSION", "")

	if _, _, ok := konsoleSession(); ok {
		t.Error("без переменных Konsole сессия Konsole распознаваться не должна")
	}
}

func TestKonsoleSessionDetected(t *testing.T) {
	t.Setenv("KONSOLE_DBUS_SERVICE", "org.kde.konsole-123")
	t.Setenv("KONSOLE_DBUS_SESSION", "/Sessions/1")

	service, session, ok := konsoleSession()
	if !ok || service != "org.kde.konsole-123" || session != "/Sessions/1" {
		t.Errorf("сессия Konsole должна читаться из окружения, получено %q %q %v", service, session, ok)
	}
}
