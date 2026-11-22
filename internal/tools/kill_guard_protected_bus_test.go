package tools

import (
	"context"
	"io/fs"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/aiseeq/chooks/internal/core"
	"github.com/aiseeq/chooks/internal/hookctx"
)

const testSessionBus = "unix:path=/run/user/1000/bus"

func validateKillRequest(t *testing.T, tool *KillGuardTool, command string, env []string) *core.ValidationResult {
	t.Helper()
	request := hookctx.New(nil, nil, nil, env, "/home/u", 2200, 2100)
	result, err := tool.ValidateTool(hookctx.WithRequest(context.Background(), request),
		&core.ToolInput{ToolName: "Bash", Command: command, CWD: "/home/u"})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestKillGuardTestDBusIsNotSessionBus(t *testing.T) {
	tool := newKillGuard(t)
	addFakeProc(t, tool, "3600", "1500", "dbus-daemon", "0::/user.slice/user-1000.slice/user@1000.service/app.slice/acc2-bus.service")
	if err := os.WriteFile(tool.procRoot+"/3600/cmdline", []byte("/usr/bin/dbus-daemon\x00--nofork\x00--config-file=/usr/share/dbus-1/session.conf\x00--address=unix:path=/home/aisee/.cache/claude-tmp/acc-ppd2/bus\x00"), 0600); err != nil {
		t.Fatal(err)
	}
	tool.resolvePath = func(path string) (string, error) { return path, nil }
	tool.lstatPath = func(string) (fs.FileInfo, error) { return nil, fs.ErrNotExist }
	for _, command := range []string{"systemctl --user stop acc2-bus.service", "kill 3600"} {
		result := validateKillRequest(t, tool, command, []string{"DBUS_SESSION_BUS_ADDRESS=" + testSessionBus})
		if !result.IsValid {
			t.Errorf("%s: expected allow, got %v", command, result.Suggestions)
		}
	}
}

func TestKillGuardSessionDBusAddressForms(t *testing.T) {
	for _, tc := range []struct{ name, args, requestAddress string }{
		{"request address", "--address=unix:path=/run/user/1000/custom-bus", "unix:path=/run/user/1000/custom-bus"},
		{"standard runtime path", "--address=unix:path=/run/user/1000/bus", testSessionBus},
		{"runtime tmpdir", "--address=unix:tmpdir=/run/user/1000", testSessionBus},
		{"runtime abstract address", "--address=unix:abstract=/run/user/1000/session-bus", "unix:abstract=/run/user/1000/session-bus"},
		{"session default", "--session", testSessionBus},
		{"systemd activation", "--address=systemd:", testSessionBus},
		{"unknown address", "", testSessionBus},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool := newKillGuard(t)
			addFakeProc(t, tool, "3600", "1500", "dbus-daemon", "0::/user.slice/user-1000.slice/user@1000.service/session.slice/dbus.service")
			cmdline := strings.Join(append([]string{"/usr/bin/dbus-daemon", "--nofork"}, strings.Split(tc.args, " ")...), "\x00") + "\x00"
			if tc.args == "" {
				cmdline = "/usr/bin/dbus-daemon\x00--nofork\x00"
			}
			if err := os.WriteFile(tool.procRoot+"/3600/cmdline", []byte(cmdline), 0600); err != nil {
				t.Fatal(err)
			}
			for _, command := range []string{"systemctl --user stop dbus.service", "kill 3600"} {
				result := validateKillRequest(t, tool, command, []string{"DBUS_SESSION_BUS_ADDRESS=" + tc.requestAddress})
				if result.IsValid {
					t.Errorf("%s: expected refusal", command)
					continue
				}
				joined := strings.Join(result.Suggestions, " ")
				if strings.Contains(joined, "kill 12345") || strings.Contains(joined, "убей лишний процесс") {
					t.Errorf("%s: hint repeats forbidden action: %s", command, joined)
				}
				if command == "kill 3600" && !slices.Contains(result.Suggestions, protectedProcessHint) {
					t.Errorf("missing protected-process advice: %v", result.Suggestions)
				}
			}
		})
	}
}

func TestKillGuardAllProtectedUnitHasSafeHint(t *testing.T) {
	tool := newKillGuard(t)
	addFakeProc(t, tool, "3600", "1500", "dbus-daemon", "0::/user.slice/user-1000.slice/user@1000.service/app.slice/acc2-bus.service")
	if err := os.WriteFile(tool.procRoot+"/3600/cmdline", []byte("dbus-daemon\x00--session\x00"), 0600); err != nil {
		t.Fatal(err)
	}
	result := validateKillRequest(t, tool, "systemctl --user stop acc2-bus.service", nil)
	if result.IsValid {
		t.Fatal("expected refusal")
	}
	joined := strings.Join(result.Suggestions, " ")
	if strings.Contains(joined, "убей лишний процесс") || strings.Contains(joined, "kill 12345") {
		t.Fatalf("hint repeats forbidden action: %s", joined)
	}
	if !strings.Contains(joined, "все процессы защищены") {
		t.Fatalf("missing safe explanation: %v", result.Suggestions)
	}
}

func TestKillGuardReviewDBusAddressCases(t *testing.T) {
	const (
		runtimeBus = "/run/user/1000/bus"
		aliasPath  = "/tmp/review-session-bus-link"
	)
	tests := []struct {
		name, requestAddress string
		argv                 []string
		deny                 bool
		parentAlias          bool
		targetSymlink        bool
		pathError            bool
	}{
		{name: "ticket external bus", requestAddress: "unix:path=" + runtimeBus, argv: []string{"--nofork", "--config-file=/usr/share/dbus-1/session.conf", "--address=unix:path=/home/aisee/.cache/claude-tmp/acc-ppd2/bus"}},
		{name: "other external socket", requestAddress: "unix:path=" + runtimeBus, argv: []string{"--address=unix:path=/tmp/test-bus"}},
		{name: "double slash", requestAddress: "unix:path=" + runtimeBus, argv: []string{"--address=unix:path=/run//user/1000/bus"}, deny: true},
		{name: "runtime other uid isolated", requestAddress: "unix:path=/tmp/unrelated", argv: []string{"--address=unix:path=/run/user/9999/bus"}, deny: true},
		{name: "dot segment isolated", requestAddress: "unix:path=/tmp/unrelated", argv: []string{"--address=unix:path=/tmp/a/../bus"}, deny: true},
		{name: "parent symlink missing endpoint", requestAddress: "unix:path=" + runtimeBus, argv: []string{"--address=unix:path=/tmp/review-link/nonexistent"}, deny: true, parentAlias: true},
		{name: "target symlink", requestAddress: "unix:path=" + runtimeBus, argv: []string{"--address=unix:path=/tmp/review-session-bus-link"}, deny: true, targetSymlink: true},
		{name: "path stat error", requestAddress: "unix:path=/run/user/1000/bus", argv: []string{"--address=unix:path=/tmp/private"}, deny: true, pathError: true},
		{name: "request guid fallback", requestAddress: "unix:path=/tmp/session-bus,guid=123", argv: []string{"--address=unix:path=/tmp/session-bus"}, deny: true},
		{name: "request exact path", requestAddress: "unix:path=/tmp/same-bus", argv: []string{"--address=unix:path=/tmp/same-bus"}, deny: true},
		{name: "request percent escape", requestAddress: "unix:path=/tmp/session-bus", argv: []string{"--address=unix:path=/tmp/session%2Dbus"}, deny: true},
		{name: "mixed endpoint params", requestAddress: "unix:path=/run/user/1000/bus", argv: []string{"--address=unix:path=/tmp/a,abstract=other"}, deny: true},
		{name: "address list", requestAddress: "unix:path=" + runtimeBus, argv: []string{"--address=unix:path=/tmp/test-bus;unix:path=/tmp/other"}, deny: true},
		{name: "space in argv value", requestAddress: "unix:path=/tmp/session bus", argv: []string{"--address=unix:path=/tmp/session bus"}, deny: true},
		{name: "missing address", argv: []string{"--config-file=/tmp/config"}, deny: true},
		{name: "print address", requestAddress: "unix:path=/tmp/session-bus", argv: []string{"--print-address", "--address=unix:path=/tmp/test-bus"}, deny: true},
		{name: "systemd address", requestAddress: "unix:path=" + runtimeBus, argv: []string{"--address=systemd:"}, deny: true},
		{name: "session flag", requestAddress: "unix:path=" + runtimeBus, argv: []string{"--session", "--address=unix:path=/tmp/test-bus"}, deny: true},
		{name: "two address flags", requestAddress: "unix:path=" + runtimeBus, argv: []string{"--address=unix:path=/tmp/test-bus", "--address=unix:path=/tmp/other"}, deny: true},
		{name: "separate address", requestAddress: "unix:path=" + runtimeBus, argv: []string{"--address", "unix:path=/tmp/test-bus"}, deny: true},
		{name: "abstract transport", requestAddress: "unix:path=" + runtimeBus, argv: []string{"--address=unix:abstract=/tmp/test-bus"}, deny: true},
		{name: "tmpdir transport", requestAddress: "unix:path=" + runtimeBus, argv: []string{"--address=unix:tmpdir=/tmp"}, deny: true},
		{name: "tcp transport", requestAddress: "unix:path=" + runtimeBus, argv: []string{"--address=tcp:host=localhost,port=1234"}, deny: true},
		{name: "unknown address option", requestAddress: "unix:path=" + runtimeBus, argv: []string{"--session", "--address=unix:path=/tmp/test-bus"}, deny: true},
		// значение --config-file отдельным аргументом не прячет флаги (ревью 3)
		{name: "address after config-file", requestAddress: "unix:path=" + runtimeBus, argv: []string{"--config-file", "--address=unix:path=/run/user/1000/bus", "--address=unix:path=/tmp/test-bus"}, deny: true},
		{name: "session after config-file", requestAddress: "unix:path=" + runtimeBus, argv: []string{"--config-file", "--session", "--address=unix:path=/tmp/test-bus"}, deny: true},
		{name: "print-address after config-file", requestAddress: "unix:path=" + runtimeBus, argv: []string{"--config-file", "--print-address", "--address=unix:path=/tmp/test-bus"}, deny: true},
		{name: "config-file value then test bus", requestAddress: "unix:path=" + runtimeBus, argv: []string{"--config-file", "/usr/share/dbus-1/session.conf", "--address=unix:path=/tmp/test-bus"}, deny: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tool := newKillGuard(t)
			addFakeProc(t, tool, "3600", "1500", "dbus-daemon", "0::/user.slice/user-1000.slice/user@1000.service/app.slice/acc2-bus.service")
			argv := append([]string{"/usr/bin/dbus-daemon"}, tc.argv...)
			if err := os.WriteFile(tool.procRoot+"/3600/cmdline", []byte(strings.Join(argv, "\x00")+"\x00"), 0600); err != nil {
				t.Fatal(err)
			}
			tool.resolvePath = func(path string) (string, error) {
				if tc.parentAlias && path == "/tmp/review-link" {
					return "/run/user/1000", nil
				}
				return path, nil
			}
			tool.lstatPath = func(path string) (fs.FileInfo, error) {
				if tc.pathError && path == "/tmp/private" {
					return nil, fs.ErrPermission
				}
				if tc.targetSymlink && path == aliasPath {
					link := t.TempDir() + "/link"
					if err := os.Symlink("target", link); err != nil {
						t.Fatal(err)
					}
					return os.Lstat(link)
				}
				return nil, fs.ErrNotExist
			}
			for _, command := range []string{"kill 3600", "systemctl --user stop acc2-bus.service"} {
				result := validateKillRequest(t, tool, command, []string{"DBUS_SESSION_BUS_ADDRESS=" + tc.requestAddress})
				if result.IsValid == tc.deny {
					t.Errorf("%s: deny=%v, suggestions=%q", command, tc.deny, result.Suggestions)
				}
				if !result.IsValid && command == "kill 3600" {
					joined := strings.Join(result.Suggestions, " ")
					if strings.Contains(joined, "kill 12345") || strings.Contains(joined, "убей лишний процесс") {
						t.Errorf("protected PID advice repeats the signal: %s", joined)
					}
				}
			}
		})
	}
}
