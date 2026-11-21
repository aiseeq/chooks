package modellaunch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/aiseeq/chooks/internal/core"
	"github.com/aiseeq/chooks/internal/tools"
)

type fakeProcess struct {
	parent  map[int]int
	exe     map[int]string
	exeErr  map[int]error
	comm    map[int]string
	environ map[int][]string
	argv    map[int][]string
	cwd     map[int]string
	fail    bool
	calls   map[string]int
}

func (f *fakeProcess) Parent(pid int) (int, error) {
	if f.fail {
		return 0, errors.New("proc unavailable")
	}
	return f.parent[pid], nil
}
func (f *fakeProcess) Exe(pid int) (string, error) {
	f.calls["exe:"+strconv.Itoa(pid)]++
	return f.exe[pid], f.exeErr[pid]
}
func (f *fakeProcess) Comm(pid int) (string, error) {
	if f.comm[pid] != "" {
		return f.comm[pid], nil
	}
	return filepath.Base(f.exe[pid]), nil
}
func (f *fakeProcess) Environ(pid int) ([]string, error) {
	if f.fail {
		return nil, errors.New("proc unavailable")
	}
	return f.environ[pid], nil
}
func (f *fakeProcess) Cmdline(pid int) ([]string, error) { return f.argv[pid], nil }
func (f *fakeProcess) Cwd(pid int) (string, error)       { return f.cwd[pid], nil }

func (f *fakeProcess) add(pid, parent int, exe string, env, argv []string) {
	f.parent[pid], f.exe[pid], f.environ[pid], f.argv[pid], f.cwd[pid] = parent, exe, env, argv, "/work"
}

func runtimeGuard(t *testing.T, proc *fakeProcess) *Guard {
	t.Helper()
	path := filepath.Join(t.TempDir(), "approved")
	if err := os.WriteFile(path, []byte("opus claude-opus-5-5\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MODELS_APPROVED", path)
	logger, err := core.NewLogger(core.LoggerConfig{Level: "error", Format: "text", Output: "stderr"})
	if err != nil {
		t.Fatal(err)
	}
	modelGuard, err := tools.NewModelGuardTool(core.ToolConfig{Enabled: true}, logger)
	if err != nil {
		t.Fatal(err)
	}
	return New(proc, modelGuard, logger)
}

func agentTree(argv []string, env ...string) *fakeProcess {
	f := &fakeProcess{parent: map[int]int{}, exe: map[int]string{}, exeErr: map[int]error{}, comm: map[int]string{}, environ: map[int][]string{}, argv: map[int][]string{}, cwd: map[int]string{}, calls: map[string]int{}}
	f.add(100, 90, "/usr/bin/chooks", nil, []string{"chooks", "user-prompt-submit"})
	f.add(90, 80, "/opt/claude/claude", env, argv)
	f.add(80, 1, "/usr/bin/bash", nil, []string{"bash"})
	return f
}

func TestRuntimeModelGuardSkipsUnreadableSystemdAncestor(t *testing.T) {
	makeTree := func(env ...string) *fakeProcess {
		f := agentTree([]string{"claude", "--model", "haiku"}, env...)
		f.add(80, 70, "/usr/bin/bash", nil, []string{"bash"})
		f.add(70, 1, "/usr/lib/systemd/systemd", nil, []string{"systemd", "--user"})
		f.exeErr[70] = errors.New("permission denied")
		f.comm[70] = "systemd"
		return f
	}

	t.Run("agent environment classifies immediately", func(t *testing.T) {
		f := makeTree("CLAUDECODE=1")
		message, err := runtimeGuard(t, f).Check(context.Background(), 100)
		if err != nil || message == "" {
			t.Fatalf("Check = %q, %v; want rejection", message, err)
		}
		if f.calls["exe:70"] != 0 {
			t.Fatalf("read upper systemd exe %d times; environment should classify immediately", f.calls["exe:70"])
		}
	})

	t.Run("user environment scans past unreadable ancestor", func(t *testing.T) {
		f := makeTree("CLAUDE_BASE_DIR=/home/user/.claude", "CLAUDE_CODE_SHELL_PREFIX=x")
		message, err := runtimeGuard(t, f).Check(context.Background(), 100)
		if err != nil || message != "" {
			t.Fatalf("Check = %q, %v; want pass", message, err)
		}
		if f.calls["exe:70"] == 0 {
			t.Fatal("unreadable systemd ancestor was not visited")
		}
	})
}

func TestRuntimeModelGuardIdentifiesClaudeByCommWithoutExe(t *testing.T) {
	f := agentTree([]string{"claude", "--model", "haiku"}, "CLAUDECODE=1")
	f.exeErr[90] = errors.New("permission denied")
	f.comm[90] = "claude"
	message, err := runtimeGuard(t, f).Check(context.Background(), 100)
	if err != nil || message == "" {
		t.Fatalf("Check = %q, %v; want rejection", message, err)
	}
}

func TestRuntimeModelGuardSessionParentNameIsIrrelevant(t *testing.T) {
	t.Run("version path and comm", func(t *testing.T) {
		f := agentTree([]string{"2.1.999", "--model", "haiku"}, "CLAUDECODE=1")
		f.exe[90] = "/home/user/.local/share/claude/versions/2.1.999"
		f.comm[90] = "2.1.999"
		message, err := runtimeGuard(t, f).Check(context.Background(), 100)
		if err != nil || message == "" {
			t.Fatalf("Check = %q, %v; want rejection", message, err)
		}
	})

	t.Run("shell -c wrapper", func(t *testing.T) {
		f := agentTree(nil)
		f.add(90, 80, "/bin/sh", nil, []string{"sh", "-c", `"$HOME/bin/chooks" user-prompt-submit < "$FAKE_IN"`})
		f.add(80, 70, "/home/review/bin/fake", []string{"CLAUDECODE=1"}, []string{"renamed-fake", "--model", "haiku"})
		f.add(70, 1, "/usr/bin/bash", nil, []string{"bash"})
		message, err := runtimeGuard(t, f).Check(context.Background(), 100)
		if err != nil || message == "" {
			t.Fatalf("Check = %q, %v; want rejection", message, err)
		}
	})

	t.Run("exec -a", func(t *testing.T) {
		f := agentTree([]string{"foo", "--model", "haiku"}, "CLAUDECODE=1")
		message, err := runtimeGuard(t, f).Check(context.Background(), 100)
		if err != nil || message == "" {
			t.Fatalf("Check = %q, %v; want rejection", message, err)
		}
	})
}

func TestRuntimeModelGuardRejectsNonRegularSettingsPath(t *testing.T) {
	f := agentTree([]string{"claude", "--settings", "/dev/fd/63"}, "CLAUDECODE=1")
	message, err := runtimeGuard(t, f).Check(context.Background(), 100)
	if err != nil || message == "" {
		t.Fatalf("Check = %q, %v; want rejection", message, err)
	}
}

func TestRuntimeModelGuardUserParentCanHaveAnyName(t *testing.T) {
	f := agentTree([]string{"fake-client", "--model", "fable"}, "CLAUDE_BASE_DIR=/home/user/.claude")
	f.exe[90] = "/tmp/renamed-client"
	f.comm[90] = "renamed-client"
	message, err := runtimeGuard(t, f).Check(context.Background(), 100)
	if err != nil || message != "" {
		t.Fatalf("Check = %q, %v; want pass", message, err)
	}
}

func TestRuntimeModelGuardSessionIDAndPIDMarkers(t *testing.T) {
	for _, env := range []string{"CLAUDE_CODE_SESSION_ID=abc", "CLAUDE_PID=55"} {
		t.Run(env, func(t *testing.T) {
			message, err := runtimeGuard(t, agentTree([]string{"claude", "--model", "haiku"}, env)).Check(context.Background(), 100)
			if err != nil || message == "" {
				t.Fatalf("Check = %q, %v; want rejection", message, err)
			}
		})
	}
}

func TestRuntimeModelGuardAgentCases(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		env  []string
		deny bool
	}{
		{"haiku", []string{"claude", "--model", "haiku"}, []string{"CLAUDECODE=1"}, true},
		{"fable", []string{"claude", "--model", "fable"}, []string{"CLAUDECODE=1"}, true},
		{"fable id", []string{"claude", "--model", "claude-fable-5-2"}, []string{"CLAUDECODE=1"}, true},
		{"settings", []string{"claude", "--settings", `{"model":"haiku"}`}, []string{"CLAUDECODE=1"}, true},
		{"environment model", []string{"claude", "-p", "hello"}, []string{"CLAUDECODE=1", "ANTHROPIC_MODEL=haiku"}, true},
		{"opus", []string{"claude", "--model", "opus"}, []string{"CLAUDECODE=1"}, false},
		{"no model", []string{"claude", "-p", "hello"}, []string{"CLAUDECODE=1"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			message, err := runtimeGuard(t, agentTree(tc.argv, tc.env...)).Check(context.Background(), 100)
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			if denied := message != ""; denied != tc.deny {
				t.Fatalf("message=%q, denied=%v; want %v", message, denied, tc.deny)
			}
			if tc.deny && message[:len(rejectionPrefix)] != rejectionPrefix {
				t.Fatalf("rejection prefix missing: %q", message)
			}
		})
	}
}

func TestRuntimeModelGuardNodeLauncherDropsNodeArg(t *testing.T) {
	f := agentTree([]string{"node", "/opt/claude/claude", "--model", "haiku"}, "CLAUDECODE=1")
	message, err := runtimeGuard(t, f).Check(context.Background(), 100)
	if err != nil || message == "" {
		t.Fatalf("Check = %q, %v; want rejection", message, err)
	}
}

func TestRuntimeModelGuardClaudeAncestorMarksAgent(t *testing.T) {
	f := agentTree([]string{"claude", "--model", "haiku"})
	f.add(80, 70, "/opt/claude/claude", nil, []string{"claude", "-p", "nested"})
	f.add(70, 1, "/usr/bin/bash", nil, []string{"bash"})
	message, err := runtimeGuard(t, f).Check(context.Background(), 100)
	if err != nil || message == "" {
		t.Fatalf("Check = %q, %v; want rejection", message, err)
	}
}

func TestRuntimeModelGuardFindsClaudeAncestorPastUnreadableExe(t *testing.T) {
	f := agentTree([]string{"renamed-client", "--model", "haiku"})
	f.add(80, 75, "/usr/bin/sudo", nil, []string{"sudo", "-u", "user", "renamed-client"})
	f.exeErr[80] = errors.New("permission denied")
	f.comm[80] = "sudo"
	f.add(75, 70, "/usr/bin/bash", nil, []string{"bash"})
	f.add(70, 1, "/opt/claude/claude", nil, []string{"claude", "-p", "parent"})
	message, err := runtimeGuard(t, f).Check(context.Background(), 100)
	if err != nil || message == "" {
		t.Fatalf("Check = %q, %v; want rejection", message, err)
	}
}

func TestRuntimeModelGuardInteractiveUserPasses(t *testing.T) {
	for _, model := range []string{"fable", "haiku"} {
		t.Run(model, func(t *testing.T) {
			f := agentTree([]string{"claude", "--model", model}, "CLAUDE_BASE_DIR=/home/user/.claude", "CLAUDE_CODE_SHELL_PREFIX=x")
			f.add(80, 70, "/usr/local/bin/cl", nil, []string{"cl"})
			f.add(70, 60, "/usr/bin/bash", nil, []string{"bash"})
			f.add(60, 1, "/usr/bin/konsole", nil, []string{"konsole"})
			message, err := runtimeGuard(t, f).Check(context.Background(), 100)
			if err != nil || message != "" {
				t.Fatalf("Check = %q, %v; want pass", message, err)
			}
		})
	}
}

func TestRuntimeModelGuardProcReadFailurePasses(t *testing.T) {
	f := agentTree([]string{"claude", "--model", "haiku"}, "CLAUDECODE=1")
	f.fail = true
	message, err := runtimeGuard(t, f).Check(context.Background(), 100)
	if err != nil || message != "" {
		t.Fatalf("Check = %q, %v; want fail open", message, err)
	}
}

func TestLiteralCommandQuotesEveryWord(t *testing.T) {
	got := literalCommand([]string{"claude", "--model", "a'b"}, []string{"HOME=/home/a user", "ANTHROPIC_MODEL=opus"})
	want := "env 'ANTHROPIC_MODEL=opus' 'HOME=/home/a user' 'claude' '--model' 'a'\\''b'"
	if got != want {
		t.Fatalf("literalCommand = %q; want %q", got, want)
	}
}
