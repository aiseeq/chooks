package notifier

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiseeq/chooks/internal/core"
	"github.com/aiseeq/chooks/internal/desktop"
)

func newNotifier(t *testing.T, config core.ToolConfig) *Notifier {
	t.Helper()
	return New(config, testLogger(t))
}

func TestNotifier_BuildAlert(t *testing.T) {
	notifier := newNotifier(t, core.ToolConfig{
		Enabled:         true,
		Sound:           true,
		Desktop:         true,
		ActivateOnClick: true,
	})

	t.Run("вопрос показывает текст запроса", func(t *testing.T) {
		alert, err := notifier.buildAlert(core.EventNotification, &core.ToolInput{
			Message: "Claude needs your permission to use Bash",
		}, "my-project")
		if err != nil {
			t.Fatalf("событие должно обрабатываться: %v", err)
		}
		if alert.Message != "Claude needs your permission to use Bash" {
			t.Errorf("текст запроса не подставлен: %q", alert.Message)
		}
		// Без списка процессов уведомление останется без действия по клику
		if len(alert.ActivatePIDs) == 0 {
			t.Error("процессы для активации окна не определены")
		}
	})

	t.Run("завершение работы без текста запроса", func(t *testing.T) {
		alert, err := notifier.buildAlert(core.EventStop, &core.ToolInput{}, "my-project")
		if err != nil {
			t.Fatalf("событие должно обрабатываться: %v", err)
		}
		if !strings.Contains(alert.Message, "my-project") {
			t.Errorf("сообщение: %q", alert.Message)
		}
	})

	// Нулевое значение Urgency — low: хук должен задавать normal явно,
	// иначе его уведомления тихо станут незаметнее
	t.Run("хуки шлют обычную срочность", func(t *testing.T) {
		for _, event := range []string{core.EventStop, core.EventStopFailure, core.EventNotification} {
			alert, err := notifier.buildAlert(event, &core.ToolInput{}, "my-project")
			if err != nil {
				t.Fatalf("%s: %v", event, err)
			}
			if alert.Urgency != desktop.UrgencyNormal {
				t.Errorf("%s: срочность %s, ожидалась normal", event, alert.Urgency)
			}
		}
	})

	t.Run("посторонние события — ошибка", func(t *testing.T) {
		if _, err := notifier.buildAlert("Write", &core.ToolInput{}, "my-project"); err == nil {
			t.Error("Write не является событием сессии")
		}
	})
}

func TestNotifier_ActivationDisabled(t *testing.T) {
	notifier := newNotifier(t, core.ToolConfig{Enabled: true, Desktop: true, ActivateOnClick: false})

	alert, err := notifier.buildAlert(core.EventStop, &core.ToolInput{}, "my-project")
	if err != nil {
		t.Fatalf("событие должно обрабатываться: %v", err)
	}
	if len(alert.ActivatePIDs) != 0 {
		t.Error("при выключенной активации список процессов должен быть пуст")
	}
}

// Без звука и уведомления Announce не запускает фоновый процесс и не падает
func TestNotifier_AnnounceSilent(t *testing.T) {
	t.Setenv("KONSOLE_DBUS_SERVICE", "")
	notifier := newNotifier(t, core.ToolConfig{Enabled: true})

	decision := Decision{State: core.StateDone, Alert: true}
	if err := notifier.Announce(core.EventStop, &core.ToolInput{CWD: "/tmp/p"}, decision); err != nil {
		t.Errorf("Announce: %v", err)
	}
	if err := notifier.Announce(core.EventStop, &core.ToolInput{}, Decision{State: core.StatePaused}); err != nil {
		t.Errorf("пауза ничего не объявляет: %v", err)
	}
}

func TestNotifierTool_ProjectName(t *testing.T) {
	notifier := newNotifier(t, core.ToolConfig{Enabled: true})
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Проекты вне ~/work и вложенные каталоги: имя определяется по факту, а не по шаблону пути
	for _, dir := range []string{"work/chooks", "git/life", "work/projecta/frontend/admin-app"} {
		if err := os.MkdirAll(filepath.Join(home, dir), 0o755); err != nil {
			t.Fatalf("не удалось создать каталог: %v", err)
		}
	}
	encodedHome := encodePathPart(home)

	tests := []struct {
		name     string
		input    core.ToolInput
		expected string
	}{
		{
			// Агент сделал cd в подпапку чужого проекта: вкладка называет сессию по каталогу запуска
			name:     "каталог запуска приоритетнее текущего",
			input:    core.ToolInput{CWD: filepath.Join(home, "work/projecta/frontend"), TranscriptPath: "/p/" + encodedHome + "-work-chooks/s.jsonl"},
			expected: "chooks",
		},
		{
			name:     "без транскрипта берётся текущий каталог",
			input:    core.ToolInput{CWD: filepath.Join(home, "git/life")},
			expected: "life",
		},
		{
			name:     "домашний каталог вместо имени пользователя",
			input:    core.ToolInput{CWD: home},
			expected: "~",
		},
		{
			name:     "корень файловой системы",
			input:    core.ToolInput{CWD: "/"},
			expected: "/",
		},
		{
			name:     "дефис в имени проекта не является разделителем",
			input:    core.ToolInput{TranscriptPath: "/p/" + encodedHome + "-work-chooks/s.jsonl"},
			expected: "chooks",
		},
		{
			name:     "проект вне рабочего каталога",
			input:    core.ToolInput{TranscriptPath: "/p/" + encodedHome + "-git-life/s.jsonl"},
			expected: "life",
		},
		{
			// Родительский каталог различает ~/work/projecta/frontend и ~/work/glint/frontend
			name:     "вложенный проект показывается вместе с родителем",
			input:    core.ToolInput{TranscriptPath: "/p/" + encodedHome + "-work-projecta-frontend-admin-app/s.jsonl"},
			expected: "frontend/admin-app",
		},
		{
			name:     "проект первого уровня остаётся без родителя",
			input:    core.ToolInput{CWD: filepath.Join(home, "work/chooks")},
			expected: "chooks",
		},
		{
			name:     "сессия из домашнего каталога",
			input:    core.ToolInput{TranscriptPath: "/p/" + encodedHome + "/s.jsonl"},
			expected: "~",
		},
		{
			name:     "удалённый каталог: дефис считается разделителем",
			input:    core.ToolInput{TranscriptPath: "/p/-var-lib-service/s.jsonl"},
			expected: "service",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := notifier.ProjectName(&tt.input); got != tt.expected {
				t.Errorf("ожидалось %q, получено %q", tt.expected, got)
			}
		})
	}
}

func TestDecodeProjectDir(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "my-app", "sub"), 0o755); err != nil {
		t.Fatalf("не удалось создать каталог: %v", err)
	}
	encodedRoot := encodePathPart(root)

	tests := map[string]string{
		encodedRoot + "-my-app":     filepath.Join(root, "my-app"),
		encodedRoot + "-my-app-sub": filepath.Join(root, "my-app", "sub"),
		"":                          "",
		"-":                         "",
	}

	for encoded, expected := range tests {
		t.Run(encoded, func(t *testing.T) {
			if got := decodeProjectDir(encoded, testLogger(t)); got != expected {
				t.Errorf("ожидалось %q, получено %q", expected, got)
			}
		})
	}
}

// Путь может содержать каталог, чьё имя начинается с дефиса: /tmp/x/-home-user
func TestDecodeProjectDir_LeadingDashInName(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "-dashed", "app")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("не удалось создать каталог: %v", err)
	}

	encoded := encodePathPart(root) + "--dashed-app"
	if got := decodeProjectDir(encoded, testLogger(t)); got != nested {
		t.Errorf("ожидалось %q, получено %q", nested, got)
	}
}

// testLogger — логгер для тестов пакета: пишет в stderr, который go test
// показывает только при провале
func testLogger(t *testing.T) core.Logger {
	t.Helper()
	logger, err := core.NewLogger(core.LoggerConfig{Level: "debug"})
	if err != nil {
		t.Fatalf("не удалось создать логгер: %v", err)
	}
	return logger
}

// Claude Code кодирует дефисом и точку, и дефис в имени: каталог с точкой и
// UUID в пути не рвётся на части (DNS-проба Сна 01.11 с TMPDIR в scratchpad)
func TestDecodeProjectDir_DotsAndManyDashes(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".cache", "0f7d5c09-e2d3-4f09-bf78-3b04ec6368d5", "scratchpad")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := decodeProjectDir(encodePathPart(dir), testLogger(t)); got != dir {
		t.Errorf("ожидалось %q, получено %q", dir, got)
	}
}

// Два каталога дают одно кодирование — неоднозначность: пусто, имя берётся из cwd
func TestDecodeProjectDir_Ambiguous(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"my-app", filepath.Join("my", "app")} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if got := decodeProjectDir(encodePathPart(root)+"-my-app", testLogger(t)); got != "" {
		t.Errorf("ожидалось пусто, получено %q", got)
	}
}

// Снесённый каталог: от последнего найденного остаток дефисами-разделителями
func TestDecodeProjectDir_RemovedTail(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "my-app"), 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "my-app", "gone", "sub")
	if got := decodeProjectDir(encodePathPart(root)+"-my-app-gone-sub", testLogger(t)); got != want {
		t.Errorf("ожидалось %q, получено %q", want, got)
	}
}

func TestEncodePathPart(t *testing.T) {
	for in, want := range map[string]string{"/home/u/.cache": "-home-u--cache", "a_b c": "a-b-c", "дом": "---", "x😀": "x--"} {
		if got := encodePathPart(in); got != want {
			t.Errorf("encodePathPart(%q) = %q, want %q", in, got, want)
		}
	}
}

// Каталог с правом x без r (чужой home 711): списка нет, имя находится stat-ом,
// как в прежнем декодере (находка независимой проверки 01.11)
func TestDecodeProjectDir_ExecOnlyDir(t *testing.T) {
	root := t.TempDir()
	xonly := filepath.Join(root, "xonly")
	want := filepath.Join(xonly, "my-app")
	if err := os.MkdirAll(want, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(xonly, 0o311); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(xonly, 0o755) })
	if _, err := os.ReadDir(xonly); err == nil {
		t.Skip("каталог без права чтения читается (root?)")
	}
	if got := decodeProjectDir(encodePathPart(want), testLogger(t)); got != want {
		t.Errorf("ожидалось %q, получено %q", want, got)
	}
}
