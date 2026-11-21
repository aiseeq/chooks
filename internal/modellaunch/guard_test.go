package modellaunch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/aiseeq/chooks/internal/core"
	"github.com/aiseeq/chooks/internal/tools"
)

type fakeProcess struct {
	parent  map[int]int
	exe     map[int]string
	environ map[int][]string
	argv    map[int][]string
	cwd     map[int]string
	fail    bool
}

func (f *fakeProcess) Parent(pid int) (int, error) {
	if f.fail {
		return 0, errors.New("proc unavailable")
	}
	return f.parent[pid], nil
}
func (f *fakeProcess) Exe(pid int) (string, error) { return f.exe[pid], nil }
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
	f := &fakeProcess{parent: map[int]int{}, exe: map[int]string{}, environ: map[int][]string{}, argv: map[int][]string{}, cwd: map[int]string{}}
	f.add(100, 90, "/usr/bin/chooks", nil, []string{"chooks", "user-prompt-submit"})
	f.add(90, 80, "/opt/claude/claude", env, argv)
	f.add(80, 1, "/usr/bin/bash", nil, []string{"bash"})
	return f
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
	got, err := literalCommand([]string{"claude", "--model", "a'b"}, []string{"HOME=/home/a user", "ANTHROPIC_MODEL=opus"})
	if err != nil {
		t.Fatal(err)
	}
	want := "env 'ANTHROPIC_MODEL=opus' 'HOME=/home/a user' 'claude' '--model' 'a'\\''b'"
	if got != want {
		t.Fatalf("literalCommand = %q; want %q", got, want)
	}
}
