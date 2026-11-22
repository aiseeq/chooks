package tools

import (
	"context"
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
	for _, endpoint := range []string{"unix:path=/tmp/test-bus", "unix:abstract=/tmp/test-bus", "unix:tmpdir=/tmp"} {
		t.Run(endpoint, func(t *testing.T) {
			tool := newKillGuard(t)
			addFakeProc(t, tool, "3600", "1500", "dbus-daemon", "0::/user.slice/user-1000.slice/user@1000.service/app.slice/acc2-bus.service")
			proc := tool.procRoot + "/3600/cmdline"
			if err := os.WriteFile(proc, []byte("/usr/bin/dbus-daemon\x00--nofork\x00--address="+endpoint+"\x00"), 0600); err != nil {
				t.Fatal(err)
			}
			for _, command := range []string{"systemctl --user stop acc2-bus.service", "kill 3600"} {
				result := validateKillRequest(t, tool, command, []string{"DBUS_SESSION_BUS_ADDRESS=" + testSessionBus})
				if !result.IsValid {
					t.Errorf("%s: expected allow, got %v", command, result.Suggestions)
				}
			}
		})
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
			cmdline := "/usr/bin/dbus-daemon\x00--nofork\x00" + strings.ReplaceAll(tc.args, " ", "\x00") + "\x00"
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
