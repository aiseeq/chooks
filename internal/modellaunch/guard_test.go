package modellaunch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/aiseeq/chooks/internal/core"
	"github.com/aiseeq/chooks/internal/tools"
)

// hookCommand — команда хука, как её передаёт Claude Code оболочке и префиксу
const hookCommand = `"$HOME/bin/chooks" user-prompt-submit`

var errDenied = errors.New("permission denied")

type fakeProcess struct {
	parent     map[int]int
	exe        map[int]string
	exeErr     map[int]error
	comm       map[int]string
	environ    map[int][]string
	environErr map[int]error
	argv       map[int][]string
	argvErr    map[int]error
	cwd        map[int]string
	cwdErr     map[int]error
	fail       bool
	calls      map[string]int
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
	if err := f.environErr[pid]; err != nil {
		return nil, err
	}
	return f.environ[pid], nil
}
func (f *fakeProcess) Cmdline(pid int) ([]string, error) {
	if err := f.argvErr[pid]; err != nil {
		return nil, err
	}
	return f.argv[pid], nil
}
func (f *fakeProcess) Cwd(pid int) (string, error) {
	if err := f.cwdErr[pid]; err != nil {
		return "", err
	}
	return f.cwd[pid], nil
}

func (f *fakeProcess) add(pid, parent int, exe string, env, argv []string) {
	f.parent[pid], f.exe[pid], f.environ[pid], f.argv[pid], f.cwd[pid] = parent, exe, env, argv, "/work"
}

// nonDumpable — процесс, чей бинарь не читается (chmod 111): environ, cwd и
// exe закрыты, cmdline и comm читаются
func (f *fakeProcess) nonDumpable(pid int) {
	f.environErr[pid], f.cwdErr[pid], f.exeErr[pid] = errDenied, errDenied, errDenied
}

// testGuard — проверка с судьёй model_guard на списке «opus» и HOME во
// временном каталоге: тест не читает настоящий ~/.claude/settings.json
func testGuard(t *testing.T, proc ProcessReader) (*Guard, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "approved")
	if err := os.WriteFile(path, []byte("opus claude-opus-5-5\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MODELS_APPROVED", path)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	logPath := filepath.Join(t.TempDir(), "chooks.log")
	logger, err := core.NewLogger(core.LoggerConfig{Level: "debug", Format: "text", Output: "file", LogFile: logPath})
	if err != nil {
		t.Fatal(err)
	}
	modelGuard, err := tools.NewModelGuardTool(core.ToolConfig{Enabled: true}, logger)
	if err != nil {
		t.Fatal(err)
	}
	return New(proc, modelGuard, logger), logPath
}

func runtimeGuard(t *testing.T, proc ProcessReader) *Guard {
	t.Helper()
	guard, _ := testGuard(t, proc)
	return guard
}

func newFake() *fakeProcess {
	return &fakeProcess{parent: map[int]int{}, exe: map[int]string{}, exeErr: map[int]error{}, comm: map[int]string{}, environ: map[int][]string{}, environErr: map[int]error{}, argv: map[int][]string{}, argvErr: map[int]error{}, cwd: map[int]string{}, cwdErr: map[int]error{}, calls: map[string]int{}}
}

// agentTree — хук 100, его родитель 90 (claude сессии), над ним bash 80
func agentTree(argv []string, env ...string) *fakeProcess {
	f := newFake()
	f.add(100, 90, "/usr/bin/chooks", nil, []string{"chooks", "user-prompt-submit"})
	f.add(90, 80, "/opt/claude/claude", env, argv)
	f.add(80, 1, "/usr/bin/bash", nil, []string{"bash"})
	return f
}

// userChain ставит над процессом 80 цепочку пользователя konsole → bash → cl
func userChain(f *fakeProcess) {
	f.add(80, 70, "/usr/local/bin/cl", nil, []string{"cl"})
	f.add(70, 60, "/usr/bin/bash", nil, []string{"bash"})
	f.add(60, 1, "/usr/bin/konsole", nil, []string{"konsole"})
}

func wantReject(t *testing.T, f *fakeProcess) string {
	t.Helper()
	message, err := runtimeGuard(t, f).Check(context.Background(), 100)
	if err != nil || message == "" {
		t.Fatalf("Check = %q, %v; want rejection", message, err)
	}
	if !strings.HasPrefix(message, rejectionPrefix) {
		t.Fatalf("rejection prefix missing: %q", message)
	}
	return message
}

func wantPass(t *testing.T, f *fakeProcess) {
	t.Helper()
	message, err := runtimeGuard(t, f).Check(context.Background(), 100)
	if err != nil || message != "" {
		t.Fatalf("Check = %q, %v; want pass", message, err)
	}
}

func TestRuntimeModelGuardSkipsUnreadableSystemdAncestor(t *testing.T) {
	makeTree := func(env ...string) *fakeProcess {
		f := agentTree([]string{"claude", "--model", "haiku"}, env...)
		f.add(80, 70, "/usr/bin/bash", nil, []string{"bash"})
		f.add(70, 1, "/usr/lib/systemd/systemd", nil, []string{"systemd", "--user"})
		f.exeErr[70] = errDenied
		f.comm[70] = "systemd"
		return f
	}

	t.Run("agent environment classifies immediately", func(t *testing.T) {
		f := makeTree("CLAUDECODE=1")
		wantReject(t, f)
		if f.calls["exe:70"] != 0 {
			t.Fatalf("read upper systemd exe %d times; environment should classify immediately", f.calls["exe:70"])
		}
	})

	t.Run("user environment scans past unreadable ancestor", func(t *testing.T) {
		f := makeTree("CLAUDE_BASE_DIR=/home/user/.claude", "CLAUDE_CODE_SHELL_PREFIX=x")
		wantPass(t, f)
		if f.calls["exe:70"] == 0 {
			t.Fatal("unreadable systemd ancestor was not visited")
		}
	})
}

func TestRuntimeModelGuardIdentifiesClaudeByCommWithoutExe(t *testing.T) {
	f := agentTree([]string{"claude", "--model", "haiku"}, "CLAUDECODE=1")
	f.exeErr[90] = errDenied
	f.comm[90] = "claude"
	wantReject(t, f)
}

// Предок claude без читаемого exe и с argv под другим именем узнаётся по comm
func TestRuntimeModelGuardClaudeAncestorByComm(t *testing.T) {
	f := agentTree([]string{"renamed-client", "--model", "haiku"})
	f.add(80, 70, "/opt/claude/versions/2.1.330", nil, []string{"2.1.330", "-p", "parent"})
	f.exeErr[80] = errDenied
	f.comm[80] = "claude"
	f.add(70, 1, "/usr/bin/bash", nil, []string{"bash"})
	wantReject(t, f)
}

func TestRuntimeModelGuardSessionParentNameIsIrrelevant(t *testing.T) {
	t.Run("version path and comm", func(t *testing.T) {
		f := agentTree([]string{"2.1.999", "--model", "haiku"}, "CLAUDECODE=1")
		f.exe[90] = "/home/user/.local/share/claude/versions/2.1.999"
		f.comm[90] = "2.1.999"
		wantReject(t, f)
	})

	t.Run("shell -c wrapper", func(t *testing.T) {
		f := agentTree(nil)
		f.add(90, 80, "/bin/sh", nil, []string{"sh", "-c", `"$HOME/bin/chooks" user-prompt-submit < "$FAKE_IN"`})
		f.add(80, 70, "/home/review/bin/fake", []string{"CLAUDECODE=1"}, []string{"renamed-fake", "--model", "haiku"})
		f.add(70, 1, "/usr/bin/bash", nil, []string{"bash"})
		wantReject(t, f)
	})

	t.Run("exec -a", func(t *testing.T) {
		wantReject(t, agentTree([]string{"foo", "--model", "haiku"}, "CLAUDECODE=1"))
	})

	t.Run("argv[0] is not an option", func(t *testing.T) {
		wantPass(t, agentTree([]string{"--model=haiku", "--model", "opus"}, "CLAUDECODE=1"))
	})
}

// Оболочка пропускается, только если запускает команду хука: иначе это
// процесс сессии, и признаки агента берутся из его environ
func TestRuntimeModelGuardShellWithoutHookCommandIsSessionProcess(t *testing.T) {
	f := agentTree([]string{"sh", "-c", "notify"}, "CLAUDECODE=1", "ANTHROPIC_MODEL=haiku")
	f.exe[90] = "/home/agent/bin/sh"
	userChain(f)
	wantReject(t, f)
}

func TestRuntimeModelGuardRejectsNonRegularSettingsPath(t *testing.T) {
	wantReject(t, agentTree([]string{"claude", "--settings", "/dev/fd/63"}, "CLAUDECODE=1"))
}

// /proc/self/fd/N и /dev/stdin хук открыл бы в своём процессе: там может
// оказаться обычный файл без модели, а claude читал свой
func TestRuntimeModelGuardRejectsHookProcessFDSettings(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "settings")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString("{}"); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "s.json")
	if err := os.Symlink("/proc/self/fd/"+strconv.Itoa(int(file.Fd())), link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/proc/self/fd/" + strconv.Itoa(int(file.Fd())), "/dev/stdin", link} {
		for _, args := range [][]string{{"claude", "--settings", path}, {"claude", "--settings=" + path}, {"claude", "--agents", path}} {
			t.Run(strings.Join(args, " "), func(t *testing.T) {
				wantReject(t, agentTree(args, "CLAUDECODE=1"))
			})
		}
	}
}

func TestRuntimeModelGuardUserParentCanHaveAnyName(t *testing.T) {
	f := agentTree([]string{"fake-client", "--model", "fable"}, "CLAUDE_BASE_DIR=/home/user/.claude")
	f.exe[90] = "/tmp/renamed-client"
	f.comm[90] = "renamed-client"
	wantPass(t, f)
}

func TestRuntimeModelGuardSessionIDAndPIDMarkers(t *testing.T) {
	for _, env := range []string{"CLAUDE_CODE_SESSION_ID=abc", "CLAUDE_PID=55"} {
		t.Run(env, func(t *testing.T) {
			wantReject(t, agentTree([]string{"claude", "--model", "haiku"}, env))
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
		{"model=", []string{"claude", "--model=haiku", "-p", "x"}, []string{"CLAUDECODE=1"}, true},
		{"model after prompt", []string{"claude", "-p", "x", "--model", "haiku"}, []string{"CLAUDECODE=1"}, true},
		{"fallback", []string{"claude", "--model", "opus", "--fallback-model", "haiku"}, []string{"CLAUDECODE=1"}, true},
		{"settings", []string{"claude", "--settings", `{"model":"haiku"}`}, []string{"CLAUDECODE=1"}, true},
		{"settings=", []string{"claude", `--settings={"model":"haiku"}`}, []string{"CLAUDECODE=1"}, true},
		{"agents", []string{"claude", "--agents", `{"x":{"description":"d","prompt":"p","model":"haiku"}}`}, []string{"CLAUDECODE=1"}, true},
		{"environment model", []string{"claude", "-p", "hello"}, []string{"CLAUDECODE=1", "ANTHROPIC_MODEL=haiku"}, true},
		{"opus", []string{"claude", "--model", "opus"}, []string{"CLAUDECODE=1"}, false},
		{"no model", []string{"claude", "-p", "hello"}, []string{"CLAUDECODE=1"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.deny {
				wantReject(t, agentTree(tc.argv, tc.env...))
			} else {
				wantPass(t, agentTree(tc.argv, tc.env...))
			}
		})
	}
}

// Текст промпта и системного промпта — данные: модель в нём не судится
func TestRuntimeModelGuardPromptTextIsData(t *testing.T) {
	tricky := "Проверь: `claude --model haiku -p x` даёт отказ; ANTHROPIC_MODEL=haiku; {\"model\": \"haiku\"} > .claude/settings.json; $(which claude) --model fable"
	cases := map[string][]string{
		"prompt which claude":        {"claude", "-p", "--model", "opus", "проверь $(which claude) --model fable"},
		"append system prompt":       {"claude", "-p", "--model", "opus", "--append-system-prompt", "не запускай $(which claude) --model haiku", "x"},
		"append system prompt=":      {"claude", "-p", "--model", "opus", "--append-system-prompt=$(which claude) --model haiku", "x"},
		"system prompt flag-like":    {"claude", "-p", "--model", "opus", "--system-prompt", "--model=haiku", "x"},
		"after dashdash":             {"claude", "-p", "--model", "opus", "--", "--model", "haiku"},
		"tricky prompt":              {"claude", "-p", "--model", "claude-opus-5-5", "--settings", `{"skillOverrides":{"stop-3":"off"}}`, "--append-system-prompt", tricky, tricky},
		"heredoc text":               {"claude", "-p", "--model", "opus", "cat <<EOF\nclaude --model haiku\nEOF"},
		"tools empty and stream":     {"claude", "-p", "--tools", "", "--system-prompt", tricky, "--model", "opus", "--output-format", "stream-json", "x"},
		"disallowed tools variadic":  {"claude", "-p", "--model", "opus", "--disallowedTools", "Bash(git commit:*)", "Agent", "x"},
		"interpreter flags":          {"node", "--max-old-space-size=4096", "/opt/claude/cli.js", "--model", "opus", "-p", "x"},
		"interpreter script is data": {"node", "/opt/claude/cli.js", "claude --model haiku"},
	}
	for name, argv := range cases {
		t.Run(name, func(t *testing.T) {
			wantPass(t, agentTree(argv, "CLAUDECODE=1"))
		})
	}
}

func TestModelArgsKeepsOnlyModelFlags(t *testing.T) {
	got := modelArgs([]string{"node", "/opt/claude/cli.js", "-p", "--model", "haiku", "--append-system-prompt", "--settings x", "prompt", "--add-dir", "/d", "--", "--agent", "a"})
	want := []string{"--model", "haiku", "--add-dir", "/d"}
	if !slices.Equal(got, want) {
		t.Fatalf("modelArgs = %q; want %q", got, want)
	}
}

func TestRuntimeModelGuardInterpreterLaunch(t *testing.T) {
	t.Run("node script", func(t *testing.T) {
		wantReject(t, agentTree([]string{"node", "/opt/claude/claude", "--model", "haiku"}, "CLAUDECODE=1"))
	})
	t.Run("node flags before script", func(t *testing.T) {
		wantReject(t, agentTree([]string{"node", "--max-old-space-size=4096", "/opt/claude/cli.js", "--model", "haiku"}, "CLAUDECODE=1"))
	})
	// копия claude под именем интерпретатора: первые слова — флаги claude
	t.Run("claude copy named node", func(t *testing.T) {
		wantReject(t, agentTree([]string{"node", "--model", "haiku", "-p", "x"}, "CLAUDECODE=1"))
	})
}

func TestRuntimeModelGuardClaudeAncestorMarksAgent(t *testing.T) {
	f := agentTree([]string{"claude", "--model", "haiku"})
	f.add(80, 70, "/opt/claude/claude", nil, []string{"claude", "-p", "nested"})
	f.add(70, 1, "/usr/bin/bash", nil, []string{"bash"})
	wantReject(t, f)
}

func TestRuntimeModelGuardFindsClaudeAncestorPastUnreadableExe(t *testing.T) {
	f := agentTree([]string{"renamed-client", "--model", "haiku"})
	f.add(80, 75, "/usr/bin/sudo", nil, []string{"sudo", "-u", "user", "renamed-client"})
	f.exeErr[80] = errDenied
	f.comm[80] = "sudo"
	f.add(75, 70, "/usr/bin/bash", nil, []string{"bash"})
	f.add(70, 1, "/opt/claude/claude", nil, []string{"claude", "-p", "parent"})
	wantReject(t, f)
}

func TestRuntimeModelGuardInteractiveUserPasses(t *testing.T) {
	for _, model := range []string{"fable", "haiku"} {
		t.Run(model, func(t *testing.T) {
			f := agentTree([]string{"claude", "--model", model}, "CLAUDE_BASE_DIR=/home/user/.claude", "CLAUDE_CODE_SHELL_PREFIX=/home/user/bin/cl-oom-prefix")
			userChain(f)
			wantPass(t, f)
		})
	}
}

func TestRuntimeModelGuardProcReadFailurePasses(t *testing.T) {
	f := agentTree([]string{"claude", "--model", "haiku"}, "CLAUDECODE=1")
	f.fail = true
	wantPass(t, f)
}

// prefixTree — claude 90 гонит хук 100 через префикс без exec: процесс
// префикса 92 и его потомок 95 (bash -c '<команда хука>')
func prefixTree(prefixValue string, prefixArgv []string, claudeArgv []string, claudeEnv ...string) *fakeProcess {
	f := newFake()
	env := append([]string{"CLAUDE_CODE_SHELL_PREFIX=" + prefixValue}, claudeEnv...)
	childEnv := append(slices.Clone(env), "CLAUDECODE=1", "CLAUDE_CODE_SESSION_ID=s")
	f.add(100, 95, "/usr/bin/chooks", childEnv, []string{"/home/u/bin/chooks", "user-prompt-submit"})
	f.add(95, 92, "/usr/bin/bash", childEnv, []string{"bash", "-c", hookCommand})
	f.add(92, 90, "/usr/bin/timeout", childEnv, prefixArgv)
	f.add(90, 80, "/home/u/.local/share/claude/versions/2.1.330", env, claudeArgv)
	f.add(80, 1, "/usr/bin/bash", []string{"CLAUDECODE=1"}, []string{"bash"})
	return f
}

func TestRuntimeModelGuardShellPrefixWithoutExec(t *testing.T) {
	timeoutArgv := []string{"timeout", "60", "bash", "-c", hookCommand}
	t.Run("timeout bash -c", func(t *testing.T) {
		wantReject(t, prefixTree("timeout 60 bash -c", timeoutArgv, []string{"claude", "--model", "haiku", "-p", "x"}, "CLAUDECODE=1"))
	})
	t.Run("script prefix", func(t *testing.T) {
		f := prefixTree("/r/w-fork.sh", []string{"/bin/bash", "/r/w-fork.sh", hookCommand}, []string{"claude", "--model", "haiku"}, "CLAUDECODE=1")
		f.exe[92] = "/usr/bin/bash"
		wantReject(t, f)
	})
	t.Run("prefix words expanded from environment", func(t *testing.T) {
		f := prefixTree("timeout $LIMIT bash -c", timeoutArgv, []string{"claude", "--model", "haiku"}, "CLAUDECODE=1", "LIMIT=60")
		wantReject(t, f)
	})
	t.Run("prefix unset for the hook", func(t *testing.T) {
		f := prefixTree("timeout 60 bash -c", timeoutArgv, []string{"claude", "--model", "haiku"}, "CLAUDECODE=1")
		f.environ[100] = []string{"CLAUDECODE=1"}
		f.environ[95] = []string{"CLAUDECODE=1"}
		wantReject(t, f)
	})
	t.Run("prefix set only for hooks", func(t *testing.T) {
		f := prefixTree("timeout 60 bash -c", timeoutArgv, []string{"claude", "--model", "haiku"}, "CLAUDECODE=1")
		f.environ[90] = []string{"CLAUDECODE=1"}
		f.environ[92] = []string{"CLAUDECODE=1"}
		wantReject(t, f)
	})
	t.Run("chained prefix processes", func(t *testing.T) {
		f := prefixTree("flock /tmp/l timeout 60 bash -c", []string{"timeout", "60", "bash", "-c", hookCommand}, []string{"claude", "--model", "haiku"}, "CLAUDECODE=1")
		f.parent[92] = 91
		f.add(91, 90, "/usr/bin/flock", f.environ[92], []string{"flock", "/tmp/l", "timeout", "60", "bash", "-c", hookCommand})
		wantReject(t, f)
	})
	t.Run("opus", func(t *testing.T) {
		wantPass(t, prefixTree("timeout 60 bash -c", timeoutArgv, []string{"claude", "--model", "opus"}, "CLAUDECODE=1"))
	})
	t.Run("user with prefix", func(t *testing.T) {
		f := prefixTree("timeout 60 bash -c", timeoutArgv, []string{"claude", "--model", "fable"}, "CLAUDE_BASE_DIR=/home/u/.claude")
		userChain(f)
		wantPass(t, f)
	})
	// claude, похожий на процесс префикса, судится как запуск: модель из его
	// argv и окружения не пропадает
	t.Run("claude disguised as prefix process", func(t *testing.T) {
		f := prefixTree("claude --model haiku -p", []string{"claude", "--model", "haiku", "-p", "user-prompt-submit"}, []string{"bash", "-c", "agent command"}, "CLAUDECODE=1")
		f.exe[92] = "/home/u/.local/share/claude/versions/2.1.330"
		wantReject(t, f)
	})
}

// Хук наследует окружение и каталог claude сессии: модель в них судится,
// даже если процесс над хуком принят за оболочку хука
func TestRuntimeModelGuardJudgesHookEnvironment(t *testing.T) {
	f := agentTree([]string{"claude", "-p", "x"}, "CLAUDECODE=1")
	f.environ[100] = []string{"CLAUDECODE=1", "ANTHROPIC_MODEL=haiku"}
	wantReject(t, f)
}

func TestRuntimeModelGuardUnreadableClaudeEnviron(t *testing.T) {
	agent := func(argv ...string) *fakeProcess {
		f := agentTree(argv)
		f.nonDumpable(90)
		f.environ[80] = []string{"CLAUDECODE=1"}
		return f
	}
	t.Run("agent bash parent", func(t *testing.T) {
		wantReject(t, agent("claude-x", "--model", "haiku", "-p", "x"))
	})
	t.Run("agent bash parent opus", func(t *testing.T) {
		wantPass(t, agent("claude-x", "--model", "opus", "-p", "x"))
	})
	t.Run("cmdline unreadable too", func(t *testing.T) {
		f := agent("claude-x", "--model", "opus")
		f.argvErr[90] = errDenied
		message := wantReject(t, f)
		if !strings.Contains(message, "модель не проверить") {
			t.Fatalf("message = %q", message)
		}
	})
	t.Run("model in environment seen through hook", func(t *testing.T) {
		f := agent("claude-x", "-p", "x")
		f.environ[100] = []string{"CLAUDECODE=1", "ANTHROPIC_MODEL=haiku"}
		wantReject(t, f)
	})
	t.Run("project settings in cwd seen through hook", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".claude", "settings.json"), []byte(`{"model":"haiku"}`), 0600); err != nil {
			t.Fatal(err)
		}
		f := agent("claude-x", "-p", "x")
		f.cwd[100] = dir
		wantReject(t, f)
	})
	t.Run("claude ancestor above", func(t *testing.T) {
		f := agent("claude-x", "--model", "haiku")
		f.environ[80] = nil
		f.parent[80] = 70
		f.add(70, 1, "/opt/claude/claude", nil, []string{"claude"})
		wantReject(t, f)
	})
	t.Run("user chain", func(t *testing.T) {
		f := agentTree([]string{"claude-x", "--model", "fable"})
		f.nonDumpable(90)
		f.argvErr[90] = errDenied
		userChain(f)
		wantPass(t, f)
	})
}

func TestRuntimeModelGuardAgentCmdlineUnreadableRejects(t *testing.T) {
	f := agentTree([]string{"claude", "--model", "opus"}, "CLAUDECODE=1")
	f.argvErr[90] = errDenied
	wantReject(t, f)
}

type panicJudge struct{}

func (panicJudge) ValidateTool(context.Context, *core.ToolInput) (*core.ValidationResult, error) {
	panic("judge broke")
}
func (panicJudge) CandidateModels(*core.ToolInput) []string { return nil }

// Паника судьи не перехватывается здесь: её ловит и пишет в лог вызывающий
// (cmd/chooks), и тест этого — там. Здесь — что судья вызывается для агента
func TestRuntimeModelGuardCallsJudgeForAgent(t *testing.T) {
	logger, err := core.NewLogger(core.LoggerConfig{Level: "error", Output: "stderr"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("judge was not called")
		}
	}()
	_, _ = New(agentTree([]string{"claude", "--model", "haiku"}, "CLAUDECODE=1"), panicJudge{}, logger).Check(context.Background(), 100)
}

func TestRuntimeModelGuardLogsDecision(t *testing.T) {
	guard, logPath := testGuard(t, prefixTree("timeout 60 bash -c", []string{"timeout", "60", "bash", "-c", hookCommand}, []string{"claude", "--model", "haiku"}, "CLAUDECODE=1"))
	if message, _ := guard.Check(context.Background(), 100); message == "" {
		t.Fatal("want rejection")
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"claude_pid=90", "actor=agent", "result=rejected", "launchers=2"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("log lacks %q:\n%s", want, data)
		}
	}
}

func TestLiteralCommandQuotesEveryWord(t *testing.T) {
	got := literalCommand([]string{"--model", "a'b"}, envWords([]string{"HOME=/home/a user", "ANTHROPIC_MODEL=opus", "PATH=/bin"}))
	want := "env 'ANTHROPIC_MODEL=opus' 'HOME=/home/a user' 'claude' '--model' 'a'\\''b'"
	if got != want {
		t.Fatalf("literalCommand = %q; want %q", got, want)
	}
}

// Оболочка, бинарь которой заменило обновление пакета, остаётся оболочкой хука
func TestRuntimeModelGuardDeletedShellBinaryIsHookShell(t *testing.T) {
	f := agentTree(nil)
	f.add(90, 80, "/usr/bin/bash (deleted)", []string{"CLAUDECODE=1"}, []string{"/bin/bash", "-c", hookCommand + "; exit $?"})
	f.comm[90] = "bash"
	f.add(80, 70, "/opt/claude/claude", []string{"CLAUDECODE=1"}, []string{"claude", "--model", "haiku"})
	f.add(70, 1, "/usr/bin/bash", nil, []string{"bash"})
	wantReject(t, f)
}

// Петля ссылок пути настроек — ошибка, а не «путь вне /proc»
func TestLinksIntoProcessLoop(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	if err := os.Symlink(b, a); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(a, b); err != nil {
		t.Fatal(err)
	}
	if links, err := linksIntoProcess(a); err == nil {
		t.Fatalf("linksIntoProcess = %v, nil; want loop error", links)
	}
	wantReject(t, agentTree([]string{"claude", "--settings", a}, "CLAUDECODE=1"))
}
