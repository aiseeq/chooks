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
	"github.com/aiseeq/chooks/internal/hookctx"
	"github.com/aiseeq/chooks/internal/tools"
)

// requestContext — запрос этого процесса теста: model_guard читает env и cwd
// запроса, его берут после t.Setenv теста
func requestContext(t *testing.T) context.Context {
	t.Helper()
	request, err := hookctx.FromProcess()
	if err != nil {
		t.Fatal(err)
	}
	return hookctx.WithRequest(context.Background(), request)
}

// hookCommand — команда хука, как её передаёт Claude Code оболочке и префиксу
const hookCommand = `"$HOME/bin/chooks" user-prompt-submit`

var errDenied = errors.New("permission denied")

type fakeProcess struct {
	parent     map[int]int
	tty        map[int]int64
	ttyErr     map[int]error
	cgroup     map[int]string
	cgroupErr  map[int]error
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
func (f *fakeProcess) TTYNr(pid int) (int64, error) { return f.tty[pid], f.ttyErr[pid] }
func (f *fakeProcess) Cgroup(pid int) (string, error) {
	return f.cgroup[pid], f.cgroupErr[pid]
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
	f.tty[pid] = 1
	f.cgroup[pid] = "0::/user.slice/user-1000.slice/session-1.scope"
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
	// как newRuntimeGuard в cmd/chooks: проверка запуска судит только модель
	return New(proc, modelGuard.ModelsOnly(), logger), logPath
}

func runtimeGuard(t *testing.T, proc ProcessReader) *Guard {
	t.Helper()
	guard, _ := testGuard(t, proc)
	return guard
}

func newFake() *fakeProcess {
	return &fakeProcess{parent: map[int]int{}, tty: map[int]int64{}, ttyErr: map[int]error{}, cgroup: map[int]string{}, cgroupErr: map[int]error{}, exe: map[int]string{}, exeErr: map[int]error{}, comm: map[int]string{}, environ: map[int][]string{}, environErr: map[int]error{}, argv: map[int][]string{}, argvErr: map[int]error{}, cwd: map[int]string{}, cwdErr: map[int]error{}, calls: map[string]int{}}
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
	message, err := runtimeGuard(t, f).Check(requestContext(t), 100)
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
	message, err := runtimeGuard(t, f).Check(requestContext(t), 100)
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

// openFile — открытый файл теста с содержимым и его путь /proc/self/fd/N
func openFile(t *testing.T, content string) string {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "settings")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { file.Close() })
	if _, err := file.WriteString(content); err != nil {
		t.Fatal(err)
	}
	return "/proc/self/fd/" + strconv.Itoa(int(file.Fd()))
}

func symlink(t *testing.T, target, link string) string {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	return link
}

// /proc/self/fd/N и /dev/stdin хук открыл бы в своём процессе: там может
// оказаться обычный файл, который model_guard пропустит, а claude читал свой.
// Файлы теста model_guard пропускает: отказ — только от проверки пути
func TestRuntimeModelGuardRejectsHookProcessFDSettings(t *testing.T) {
	flags := map[string]string{
		"--settings": "{}",
		"--agents":   `{"rev":{"description":"d","prompt":"p","model":"opus"}}`,
	}
	for flag, content := range flags {
		fd := openFile(t, content)
		dir := t.TempDir()
		link := symlink(t, fd, filepath.Join(dir, "s.json"))
		// каталог-ссылка: сам путь не ссылка, ссылка — его каталог
		viaDir := filepath.Join(symlink(t, "/proc/self/fd", filepath.Join(dir, "fd")), filepath.Base(fd))
		for _, path := range []string{fd, "/dev/stdin", link, viaDir} {
			for _, args := range [][]string{{"claude", flag, path}, {"claude", flag + "=" + path}} {
				t.Run(strings.Join(args, " "), func(t *testing.T) {
					wantReject(t, agentTree(args, "CLAUDECODE=1"))
				})
			}
		}
		// «~/» — каталог HOME claude, а не каталог «~» в cwd: там обычный
		// файл, который model_guard прочтёт, а claude читает ссылку из HOME
		t.Run(flag+" ~/", func(t *testing.T) {
			cwd := t.TempDir()
			if err := os.MkdirAll(filepath.Join(cwd, "~"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cwd, "~", "s.json"), []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			f := agentTree([]string{"claude", flag, "~/s.json"}, "CLAUDECODE=1", "HOME="+dir)
			f.cwd[90] = cwd
			wantReject(t, f)
		})
		t.Run(flag+" control", func(t *testing.T) {
			plain := filepath.Join(t.TempDir(), "plain.json")
			if err := os.WriteFile(plain, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			wantPass(t, agentTree([]string{"claude", flag, plain}, "CLAUDECODE=1"))
		})
	}
}

// Путь в /proc у запускателя между claude и хуком судится так же
func TestRuntimeModelGuardRejectsLauncherFDSettings(t *testing.T) {
	fd := openFile(t, "{}")
	f := prefixTree("claude --settings "+fd+" -p", []string{"claude", "--settings", fd, "-p", "user-prompt-submit"}, []string{"bash", "-c", "agent command"}, "CLAUDECODE=1")
	wantReject(t, f)
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
		{"model after inline system prompt", []string{"claude", "--append-system-prompt=x", "--model", "haiku"}, []string{"CLAUDECODE=1"}, true},
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

func TestRuntimeDetachedLaunchClassification(t *testing.T) {
	makeTree := func(model, cgroup string, tty int64, parentComm string) *fakeProcess {
		f := agentTree([]string{"claude", "--model", model})
		f.comm[80] = parentComm
		// родитель в том же юните, что claude: так на машине (агент в agents.slice)
		f.cgroup[80], f.cgroup[90], f.tty[90] = cgroup, cgroup, tty
		return f
	}
	for _, tc := range []struct {
		name, cgroup, parent string
		tty                  int64
	}{
		{"setsid in inherited agent scope", "0::/user.slice/agents.slice/agent-luna-123.scope", "bash", 1},
		{"systemd run without tty", "0::/user.slice/run-abc.service", "bash", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantReject(t, makeTree("fable", tc.cgroup, tc.tty, tc.parent))
			wantPass(t, makeTree("opus", tc.cgroup, tc.tty, tc.parent))
		})
	}

	for _, tc := range []struct {
		name, cgroup, parent string
		tty                  int64
	}{
		{"cl in agent scope with tty", "0::/user.slice/agents.slice/agent-luna-123.scope", "cl", 1},
		{"interactive session scope", "0::/user.slice/session-4.scope", "bash", 1},
		{"Chrome app scope without tty", "0::/user.slice/app-com.google.Chrome-456.scope", "chrome", 0},
	} {
		t.Run("user "+tc.name, func(t *testing.T) {
			f := makeTree("fable", tc.cgroup, tc.tty, tc.parent)
			// In a user's cl session the cl parent has a live shell above it;
			// an orphaned cl is the detached agent form.
			if tc.parent == "cl" {
				f.add(80, 70, "/usr/bin/bash", nil, []string{"bash"})
				f.tty[80] = tc.tty
			}
			wantPass(t, f)
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

func TestRuntimeModelGuardModelOverrides(t *testing.T) {
	for _, source := range []string{"inline", "settings file", "project settings", "user settings", "without overrides"} {
		t.Run(source, func(t *testing.T) {
			f := agentTree([]string{"claude", "--model", "opus"}, "CLAUDECODE=1")
			guard := runtimeGuard(t, f)
			home, project, outside := os.Getenv("HOME"), t.TempDir(), t.TempDir()
			f.cwd[90] = outside
			write := func(path, value string) {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(value), 0600); err != nil {
					t.Fatal(err)
				}
			}
			bad := `{"modelOverrides":{"claude-opus-5-5":"fable"}}`
			good := `{"modelOverrides":{"claude-opus-5-5":"opus"}}`
			switch source {
			case "inline":
				f.argv[90] = []string{"claude", "--settings", bad, "--model", "opus"}
			case "settings file":
				path := filepath.Join(outside, "settings.json")
				write(path, bad)
				f.argv[90] = []string{"claude", "--settings", path, "--model", "opus"}
			case "project settings":
				write(filepath.Join(project, ".git", "HEAD"), "ref: refs/heads/main\n")
				write(filepath.Join(project, ".claude", "settings.json"), bad)
				f.cwd[90] = project
			case "user settings":
				write(filepath.Join(home, ".claude", "settings.json"), bad)
			}
			if source == "without overrides" {
				if message, err := guard.Check(requestContext(t), 100); message != "" || err != nil {
					t.Fatalf("Check = %q, %v; want pass", message, err)
				}
				return
			}
			if message, err := guard.Check(requestContext(t), 100); err != nil || message == "" {
				t.Fatalf("unapproved override Check = %q, %v; want reject", message, err)
			}
			switch source {
			case "inline":
				f.argv[90] = []string{"claude", "--settings", good, "--model", "opus"}
			case "settings file":
				write(filepath.Join(outside, "settings.json"), good)
			case "project settings":
				write(filepath.Join(project, ".claude", "settings.json"), good)
			case "user settings":
				write(filepath.Join(home, ".claude", "settings.json"), good)
			}
			if message, err := guard.Check(requestContext(t), 100); message != "" || err != nil {
				t.Fatalf("approved override Check = %q, %v; want pass", message, err)
			}
		})
	}
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
func (panicJudge) CandidateModels(context.Context, *core.ToolInput) []string { return nil }

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
	_, _ = New(agentTree([]string{"claude", "--model", "haiku"}, "CLAUDECODE=1"), panicJudge{}, logger).Check(requestContext(t), 100)
}

func TestRuntimeModelGuardLogsDecision(t *testing.T) {
	guard, logPath := testGuard(t, prefixTree("timeout 60 bash -c", []string{"timeout", "60", "bash", "-c", hookCommand}, []string{"claude", "--model", "haiku"}, "CLAUDECODE=1"))
	if message, _ := guard.Check(requestContext(t), 100); message == "" {
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
	want := "ANTHROPIC_MODEL='opus' HOME='/home/a user' 'claude' '--model' 'a'\\''b'"
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

// Claude пользователя с читаемым environ без признаков агента — сессия
// пользователя, даже если признаки агента есть у предков: konsole или tmux,
// поднятые агентом, несут его environ
func TestRuntimeModelGuardUserClaudeUnderMarkedAncestors(t *testing.T) {
	f := agentTree([]string{"claude", "--model", "fable"}, "CLAUDE_BASE_DIR=/home/user/.claude")
	userChain(f)
	f.environ[70] = []string{"CLAUDECODE=1", "CLAUDE_CODE_SESSION_ID=agent"}
	f.environ[60] = []string{"CLAUDECODE=1", "CLAUDE_PID=55"}
	wantPass(t, f)
}

// Префикс-скрипт, найденный через PATH: в префиксе имя, в argv — полный путь
func TestRuntimeModelGuardPrefixScriptFromPath(t *testing.T) {
	f := prefixTree("w-fork.sh", []string{"/bin/bash", "/usr/local/bin/w-fork.sh", hookCommand}, []string{"claude", "--model", "haiku"}, "CLAUDECODE=1")
	f.exe[92] = "/usr/bin/bash"
	wantReject(t, f)
}

// Слово за словами интерпретатора без «/» — не путь скрипта: claude
// «claude -c '<текст с маркером>'» не процесс префикса «… bash -c»
func TestRuntimeModelGuardClaudeOptionIsNotScriptPath(t *testing.T) {
	f := prefixTree("timeout 60 bash -c", []string{"timeout", "60", "bash", "-c", hookCommand}, []string{"claude", "-c", "почини хук user-prompt-submit"}, "CLAUDECODE=1", "ANTHROPIC_MODEL=haiku")
	userChain(f)
	wantReject(t, f)
}

// Оболочка хука с закрытым exe и argv[0] под другим именем узнаётся по comm
func TestRuntimeModelGuardHookShellByComm(t *testing.T) {
	f := agentTree(nil)
	f.add(90, 80, "/usr/bin/bash", []string{"CLAUDECODE=1"}, []string{"renamed", "-c", hookCommand})
	f.exeErr[90] = errDenied
	f.comm[90] = "bash"
	f.add(80, 70, "/opt/claude/claude", []string{"CLAUDECODE=1"}, []string{"claude", "--model", "haiku"})
	f.add(70, 1, "/usr/bin/bash", nil, []string{"bash"})
	wantReject(t, f)
}

// disguisedTree — Р1: агент (bash 91 под claude пользователя 90) ставит
// префикс «pw.sh <claude> --model M -p» и запускает «<claude> --model M -p
// user-prompt-submit»: claude 92 и процесс префикса 93 похожи на запускатели
// хука, проход по предкам доходит до claude пользователя
func disguisedTree(model string) *fakeProcess {
	const claude = "/opt/claude/claude"
	prefixValue := "/r/pw.sh " + claude + " --model " + model + " -p"
	agentEnv := []string{"CLAUDECODE=1", "CLAUDE_CODE_SESSION_ID=agent", "CLAUDE_CODE_SHELL_PREFIX=" + prefixValue}
	f := newFake()
	f.add(100, 93, "/usr/bin/chooks", agentEnv, []string{"/home/u/bin/chooks", "user-prompt-submit"})
	f.add(93, 92, "/usr/bin/bash", agentEnv, []string{"/bin/bash", "/r/pw.sh", claude, "--model", model, "-p", hookCommand})
	f.add(92, 91, claude, agentEnv, []string{claude, "--model", model, "-p", "user-prompt-submit"})
	f.add(91, 90, "/usr/bin/bash", []string{"CLAUDECODE=1"}, []string{"/bin/bash", "-c", "bash /r/disg.sh user-prompt-submit"})
	f.add(90, 80, "/home/u/.local/share/claude/versions/2.1.330", []string{"CLAUDE_BASE_DIR=/home/u/.claude"}, []string{"claude", "--model", "fable"})
	f.comm[90] = "claude"
	userChain(f)
	return f
}

func TestRuntimeModelGuardClaudeDisguisedAsPrefixUnderUser(t *testing.T) {
	t.Run("haiku", func(t *testing.T) {
		wantReject(t, disguisedTree("haiku"))
	})
	t.Run("settings in /proc", func(t *testing.T) {
		fd := openFile(t, "{}")
		f := disguisedTree("opus")
		f.argv[92] = []string{"/opt/claude/claude", "--settings", fd, "-p", "user-prompt-submit"}
		f.environ[100] = []string{"CLAUDE_CODE_SHELL_PREFIX=/r/pw.sh /opt/claude/claude --settings " + fd + " -p"}
		f.argv[93] = []string{"/bin/bash", "/r/pw.sh", "/opt/claude/claude", "--settings", fd, "-p", hookCommand}
		wantReject(t, f)
	})
	t.Run("opus", func(t *testing.T) {
		wantPass(t, disguisedTree("opus"))
	})
	// env запускателя — env claude пользователя: модель в нём не судится
	t.Run("model in launcher environment", func(t *testing.T) {
		f := disguisedTree("opus")
		f.environ[92] = append(slices.Clone(f.environ[92]), "ANTHROPIC_MODEL=fable")
		f.environ[93] = f.environ[92]
		f.environ[100] = f.environ[92]
		wantPass(t, f)
	})
	// модель вне списка в настройках пользователя, а не в argv: не отказ
	t.Run("model in user settings", func(t *testing.T) {
		guard := runtimeGuard(t, disguisedTree("opus"))
		dir := filepath.Join(os.Getenv("HOME"), ".claude")
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"model":"fable"}`), 0600); err != nil {
			t.Fatal(err)
		}
		if message, err := guard.Check(requestContext(t), 100); err != nil || message != "" {
			t.Fatalf("Check = %q, %v; want pass", message, err)
		}
	})
}
