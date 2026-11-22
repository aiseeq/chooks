package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/chooks/internal/core"
	"github.com/aiseeq/chooks/internal/hookctx"
)

// relativeEnvHelper — переменная, по которой бинарь тестов в TestMain
// становится CLI chooks: аргументы — из relativeEnvArgs, env и cwd —
// процесса, как у бинаря, запущенного Claude Code
const (
	relativeEnvHelper = "CHOOKS_RELATIVE_ENV_CLI"
	relativeEnvArgs   = "CHOOKS_RELATIVE_ENV_ARGS"
	relativeEnvDelete = "CHOOKS_RELATIVE_ENV_DELETE_CWD"
)

// runRelativeEnvCLI — main() бинаря chooks в процессе тестов: os.Args из
// relativeEnvArgs, запрос — FromProcess. С relativeEnvDelete cwd процесса
// удаляется до запроса, как у хука, чей каталог стёрли
func runRelativeEnvCLI() int {
	var args []string
	if err := json.Unmarshal([]byte(os.Getenv(relativeEnvArgs)), &args); err != nil {
		fmt.Fprintln(os.Stderr, "helper args:", err)
		return 125
	}
	if os.Getenv(relativeEnvDelete) == "1" {
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintln(os.Stderr, "helper cwd:", err)
			return 125
		}
		if err := os.Remove(cwd); err != nil {
			fmt.Fprintln(os.Stderr, "helper remove cwd:", err)
			return 125
		}
	}
	for _, name := range []string{relativeEnvHelper, relativeEnvArgs, relativeEnvDelete} {
		if err := os.Unsetenv(name); err != nil {
			fmt.Fprintln(os.Stderr, "helper env:", err)
			return 125
		}
	}
	os.Args = append([]string{"chooks"}, args...)
	request, _ := hookctx.FromProcess()
	ctx := context.WithValue(context.Background(), cliTerminationKey{}, (func(int))(os.Exit))
	return execute(ctx, request)
}

// relativeEnvCase — хук с относительным значением одного ключа env или
// входа. files лежат в cwd запроса, decoy — в cwd процесса демона: путь,
// разрешённый от cwd процесса, нашёл бы decoy, и исход стал бы другим
type relativeEnvCase struct {
	name        string
	hook        string
	config      string // --config; "" — по умолчанию из env
	env         map[string]string
	files       map[string]string
	decoy       map[string]string
	input       string
	deletedCwd  bool
	wantCode    int
	wantStdout  string // подстрока stdout
	wantStderr  string // подстрока stderr
	wantFile    string // файл в cwd запроса, который хук создал или изменил
	wantContent string // подстрока wantFile
}

type relativeEnvOutcome struct {
	code           int
	stdout, stderr string
	tree           map[string]string
}

const relativeEnvConfig = `logger:
  level: debug
  output: file
  file: ~/log/chooks.log
tools:
  notifier: {enabled: false}
  skill_reload: {enabled: false}
  night_hint: {enabled: false}
  webfetch_fallback: {enabled: false}
  understand_check: {enabled: false}
  model_guard: {enabled: true}
  git_guard: {enabled: true}
  jira_style: {enabled: true}
  wide_search_hint: {enabled: false}
`

func relativeEnvCases() []relativeEnvCase {
	defaultConfig := map[string]string{"home/.config/chooks/config.yaml": relativeEnvConfig, "home/.config/models/approved": "opus claude-opus-5-5\n"}
	with := func(base map[string]string, extra map[string]string) map[string]string {
		out := map[string]string{}
		maps.Copy(out, base)
		maps.Copy(out, extra)
		return out
	}
	config := func(edit func(string) string) map[string]string {
		return with(defaultConfig, map[string]string{"home/.config/chooks/config.yaml": edit(relativeEnvConfig)})
	}
	webtext := func(page string) string {
		return "#!/bin/sh\nprintf '%s\\n' '{\"ok\":true,\"source\":\"direct\",\"kind\":\"html\",\"path\":\"" + page + "\"}'\n"
	}
	webFetch := `{"session_id":"s1","cwd":"@CWD@","hook_event_name":"PostToolUse","tool_name":"WebFetch","tool_input":{"url":"https://example.org/x"},"tool_response":{"url":"https://example.org/x","code":403,"codeText":"Forbidden","result":"Forbidden"}}`
	webFiles := with(config(func(c string) string {
		return strings.Replace(c, "webfetch_fallback: {enabled: false}", "webfetch_fallback: {enabled: true}", 1)
	}), map[string]string{
		"tools/webtext": webtext("page-tools.txt"), "home/bin/webtext": webtext("page-home.txt"),
		"page-tools.txt": "TOOLS PAGE\n", "page-home.txt": "HOME PAGE\n",
	})
	human := `{"type":"user","uuid":"u1","timestamp":"2026-10-29T10:00:00Z","origin":{"kind":"human"},"message":{"content":"что с ответом Ане?"}}`
	history := `{"type":"assistant","uuid":"m1","timestamp":"2026-10-29T10:01:00Z","message":{"stop_reason":"tool_use","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"reddy history 'Клиент - задачи' | tail -20"}}]}}`
	jira := `{"session_id":"s1","cwd":"work","hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"curl -X POST -d @body.json https://x.atlassian.net/rest/api/3/issue/PROJ-1/comment"}}`
	return []relativeEnvCase{
		{name: "HOME", hook: hookNoPrompt, files: defaultConfig, decoy: config(func(c string) string { return strings.Replace(c, "~/log/", "~/decoy-log/", 1) }),
			input: `{"session_id":"s1","hook_event_name":"Elicitation"}`, wantStdout: `"action":"decline"`, wantFile: "home/log/chooks.log"},
		{name: "XDG_CONFIG_HOME", hook: hookNoPrompt, env: map[string]string{"XDG_CONFIG_HOME": "cfg"}, files: map[string]string{"cfg/chooks/config.yaml": relativeEnvConfig},
			input: `{"session_id":"s1","hook_event_name":"Elicitation"}`, wantCode: exitError, wantStderr: "path in $XDG_CONFIG_HOME is relative"},
		{name: "config and log path", hook: hookNoPrompt, config: "c.yaml", files: map[string]string{"c.yaml": strings.Replace(relativeEnvConfig, "~/log/", "logs/", 1)},
			decoy: map[string]string{"c.yaml": "logger:\n  level: debug\n  output: file\n  file: decoy.log\n"},
			input: `{"session_id":"s1","hook_event_name":"Elicitation"}`, wantStdout: `"action":"decline"`, wantFile: "logs/chooks.log"},
		// Фоновая задача: запись задач — с временем в поясе запроса
		{name: "XDG_RUNTIME_DIR", hook: hookStop, env: map[string]string{"XDG_RUNTIME_DIR": "rt"}, files: defaultConfig,
			input:    `{"session_id":"s1","cwd":"@CWD@","hook_event_name":"Stop","background_tasks":[{"id":"t1","type":"local_bash","status":"running","description":"x"}]}`,
			wantFile: "rt/chooks/sessions/s1"},
		{name: "TMPDIR", hook: hookStop, env: map[string]string{"XDG_RUNTIME_DIR": "", "TMPDIR": "tmp"}, files: defaultConfig,
			input: `{"session_id":"s1","cwd":"@CWD@","hook_event_name":"Stop"}`, wantFile: "tmp/chooks/sessions/s1", wantContent: "done"},
		{name: "XDG_STATE_HOME", hook: hookPostToolBatch, env: map[string]string{"XDG_STATE_HOME": "st"},
			files: with(config(func(c string) string {
				return strings.Replace(c, "skill_reload: {enabled: false}", "skill_reload: {enabled: true}", 1)
			}), map[string]string{"t.jsonl": human + "\n"}),
			input: `{"session_id":"s1","cwd":"@CWD@","transcript_path":"@CWD@/t.jsonl","hook_event_name":"PostToolBatch"}`, wantFile: "st/chooks/skill-reload/s1.json", wantContent: "{"},
		// Правка скилла после загрузки: время в выдаче и в записи сессии — в
		// поясе запроса
		{name: "skill reload notice", hook: hookPostToolBatch, env: map[string]string{"XDG_STATE_HOME": "st"},
			files: with(config(func(c string) string {
				return strings.Replace(c, "skill_reload: {enabled: false}", "skill_reload: {enabled: true}", 1)
			}), map[string]string{
				"t.jsonl":              `{"type":"user","isMeta":true,"timestamp":"2026-10-29T09:00:00Z","sourceToolUseID":"toolu_1","message":{"role":"user","content":[{"type":"text","text":"Base directory for this skill: @CWD@/skills/demo\n\n# А\nкоротко."}]}}` + "\n",
				"skills/demo/SKILL.md": "---\nname: demo\ndescription: тестовый скилл\n---\n\n# А\nдлиннее и по-другому.\n",
			}),
			input: `{"session_id":"s1","cwd":"@CWD@","transcript_path":"@CWD@/t.jsonl","hook_event_name":"PostToolBatch"}`, wantStdout: "demo", wantFile: "st/chooks/skill-reload/s1.json", wantContent: "loaded_at"},
		{name: "CLAUDE_CONFIG_DIR", hook: hookPreToolUse, env: map[string]string{"CLAUDE_CONFIG_DIR": "cc"},
			files: with(defaultConfig, map[string]string{"cc/settings.json": `{"model":"sonnet"}`}), decoy: map[string]string{"cc/settings.json": `{"model":"opus"}`},
			input: `{"session_id":"s1","cwd":"@CWD@","hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"claude -p hi"}}`, wantCode: exitBlocked, wantStderr: "sonnet"},
		{name: "MODELS_APPROVED", hook: hookPreToolUse, env: map[string]string{"MODELS_APPROVED": "ap"},
			files: with(defaultConfig, map[string]string{"ap": "opus claude-opus-5-5\n"}), decoy: map[string]string{"ap": "sonnet claude-sonnet-5-5\n"},
			input: `{"session_id":"s1","cwd":"@CWD@","hook_event_name":"PreToolUse","tool_name":"Agent","tool_input":{"model":"sonnet","prompt":"x"}}`, wantCode: exitBlocked, wantStderr: "sonnet"},
		// Пустой и относительный элементы PATH: webtext найден в tools, но
		// exec.ErrDot — путь из $HOME/bin
		{name: "PATH ErrDot", hook: hookWebFetchFallback, env: map[string]string{"PATH": ":tools:/usr/bin:/bin"}, files: webFiles,
			decoy: map[string]string{"webtext": webtext("page-decoy.txt"), "page-decoy.txt": "DECOY\n"}, input: webFetch, wantStdout: "HOME PAGE"},
		{name: "PATH execerrdot=0", hook: hookWebFetchFallback, env: map[string]string{"PATH": ":tools:/usr/bin:/bin", "GODEBUG": "execerrdot=0"}, files: webFiles,
			decoy: map[string]string{"webtext": webtext("page-decoy.txt"), "page-decoy.txt": "DECOY\n"}, input: webFetch, wantStdout: "TOOLS PAGE"},
		{name: "cwd", hook: hookPreToolUse, files: with(defaultConfig, map[string]string{"work/body.json": `{"body":"готово — сейчас"}`}),
			decoy: map[string]string{"work/body.json": `{"body":"готово"}`}, input: jira, wantCode: exitBlocked, wantStderr: "тире"},
		{name: "transcript_path", hook: hookUnsentCheck, files: with(defaultConfig, map[string]string{"t.jsonl": human + "\n"}), decoy: map[string]string{"t.jsonl": human + "\n" + history + "\n"},
			input: `{"session_id":"s1","cwd":"@CWD@","transcript_path":"t.jsonl","hook_event_name":"Stop","last_assistant_message":"Ещё не отправлено: ответ Оле"}`, wantStdout: `"decision":"block"`},
		// Снимок недели пишет время сброса со смещением пояса запроса
		{name: "statusline limits", hook: "statusline", env: map[string]string{"XDG_STATE_HOME": "st"}, files: defaultConfig,
			input:      fmt.Sprintf(`{"session_id":"s1","cwd":"@CWD@","rate_limits":{"seven_day":{"used_percentage":10,"resets_at":%d}}}`, time.Now().Add(72*time.Hour).Unix()),
			wantStdout: "нед", wantFile: "st/chooks/limits.json", wantContent: "sevenDayResetsAt"},
		// Относительный $HOME с абсолютным каталогом проекта лексически не
		// совпадает: имя проекта — последний элемент, у CLI и у демона
		{name: "statusline HOME match", hook: "statusline", files: defaultConfig,
			input: `{"session_id":"s1","cwd":"@CWD@/home/a/b/c","workspace":{"current_dir":"@CWD@/home/a/b/c"}}`, wantStdout: " C "},
		// Удалённый cwd: относительные пути не открываются ни у CLI, ни у демона
		{name: "deleted cwd", hook: hookPreToolUse, deletedCwd: true, input: jira, wantCode: exitBlocked, wantStderr: "stat work/body.json: no such file or directory"},
		{name: "deleted cwd config", hook: hookNoPrompt, config: "c.yaml", deletedCwd: true, input: `{"session_id":"s1","hook_event_name":"Elicitation"}`, wantStdout: `"action":"decline"`},
	}
}

// relativeEnvBase — env хука без ключа под проверкой: HOME относительный,
// шины D-Bus нет, пояс — не пояс машины (время в логе и в файле длительностей
// — пояса запроса)
func relativeEnvBase(c relativeEnvCase) []string {
	env := map[string]string{"HOME": "home", "PATH": "/usr/bin:/bin", "LANG": "C.UTF-8", "TZ": "Asia/Kathmandu",
		"DBUS_SESSION_BUS_ADDRESS": "unix:path=/nonexistent/chooks-bus", "DBUS_SYSTEM_BUS_ADDRESS": "unix:path=/nonexistent/chooks-bus"}
	maps.Copy(env, c.env)
	var out []string
	for k, v := range env {
		if v != "" {
			out = append(out, k+"="+v)
		}
	}
	sort.Strings(out)
	return out
}

func writeRelativeEnvFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o600)
		if filepath.Base(name) == "webtext" {
			mode = 0o700
		}
		if err := os.WriteFile(path, []byte(strings.ReplaceAll(body, "@CWD@", dir)), mode); err != nil {
			t.Fatal(err)
		}
	}
}

var (
	relativeEnvNoise = regexp.MustCompile(`\d+`)
	relativeEnvTime  = regexp.MustCompile(`\d{4}-\d\d-\d\d[ T](\d\d):\d\d:\d\d(?:\.\d+)?(Z|[+-]\d\d:\d\d)?`)
)

// relativeEnvLetters пишет цифры буквами: час и пояс времени остаются в
// сравнении, когда остальные числа стёрты
func relativeEnvLetters(text string) string {
	return strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return 'a' + (r - '0')
		}
		return r
	}, text)
}

// relativeEnvTree — файлы под dir с содержимым без чисел (время, pid, мс) и
// без имени dir; строки лога о медленном хуке и старте процесса зависят от
// процесса, а не от запроса
func relativeEnvTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	tree := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		tree[rel] = relativeEnvNormalize(string(data), dir)
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	return tree
}

func relativeEnvNormalize(text, dir string) string {
	var kept []string
	for line := range strings.SplitSeq(strings.ReplaceAll(text, dir, "<cwd>"), "\n") {
		if strings.Contains(line, "hook slow") || strings.Contains(line, "process skipped") || strings.Contains(line, "process start") {
			continue
		}
		kept = append(kept, line)
	}
	// Путь в тексте ошибки: CLI называет его как дал env, запрос — от cwd
	// запроса; файл — один
	text = strings.ReplaceAll(strings.Join(kept, "\n"), "<cwd>/", "")
	text = relativeEnvTime.ReplaceAllStringFunc(text, func(stamp string) string {
		match := relativeEnvTime.FindStringSubmatch(stamp)
		return "<time " + relativeEnvLetters(match[1]+match[2]) + ">"
	})
	return relativeEnvNoise.ReplaceAllString(text, "N")
}

// relativeEnvDir — cwd запроса: у удалённого — каталог без файлов, который
// удаляет сам запуск
func relativeEnvDir(t *testing.T, c relativeEnvCase) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir = filepath.Join(dir, "request")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeRelativeEnvFiles(t, dir, c.files)
	return dir
}

func (c relativeEnvCase) args() []string {
	var args []string
	if c.config != "" {
		args = append(args, "--config", c.config)
	}
	return append(args, c.hook)
}

func runRelativeEnvCLICase(t *testing.T, c relativeEnvCase) relativeEnvOutcome {
	t.Helper()
	dir := relativeEnvDir(t, c)
	args, err := json.Marshal(c.args())
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0])
	cmd.Dir = dir
	cmd.Env = append(relativeEnvBase(c), relativeEnvHelper+"=1", relativeEnvArgs+"="+string(args))
	if c.deletedCwd {
		cmd.Env = append(cmd.Env, relativeEnvDelete+"=1")
	}
	cmd.Stdin = strings.NewReader(strings.ReplaceAll(c.input, "@CWD@", dir))
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	code := 0
	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &exitErr):
		code = exitErr.ExitCode()
	case err != nil:
		t.Fatal(err)
	}
	return relativeEnvOutcome{code: code, stdout: relativeEnvNormalize(stdout.String(), dir), stderr: relativeEnvNormalize(stderr.String(), dir), tree: relativeEnvTree(t, dir)}
}

// runRelativeEnvRequestCase — тот же хук запросом в процессе: cwd процесса —
// другой каталог с decoy, cwd запроса — свой
func runRelativeEnvRequestCase(t *testing.T, c relativeEnvCase) relativeEnvOutcome {
	t.Helper()
	dir := relativeEnvDir(t, c)
	if c.deletedCwd {
		if err := os.Remove(dir); err != nil {
			t.Fatal(err)
		}
	}
	executor := t.TempDir()
	writeRelativeEnvFiles(t, executor, c.decoy)
	before := relativeEnvTree(t, executor)
	t.Chdir(executor)
	// pid запроса — только что запущенный процесс, как хук у CLI: срок хука
	// считается от его старта, а не от старта процесса тестов
	hook := exec.Command("/bin/sleep", "60")
	if err := hook.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hook.Process.Kill(); _ = hook.Wait() })
	var stdout, stderr bytes.Buffer
	req := hookctx.New(strings.NewReader(strings.ReplaceAll(c.input, "@CWD@", dir)), &stdout, &stderr, relativeEnvBase(c), dir, hook.Process.Pid, os.Getpid())
	req.ConfigPath = c.config
	code := executeHook(context.Background(), req, c.hook)
	if after := relativeEnvTree(t, executor); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Errorf("запрос изменил cwd процесса: %v -> %v", before, after)
	}
	return relativeEnvOutcome{code: code, stdout: relativeEnvNormalize(stdout.String(), dir), stderr: relativeEnvNormalize(stderr.String(), dir), tree: relativeEnvTree(t, dir)}
}

// TestRelativeEnvCLIAndRequest: на каждый ключ env, путь конфига, лога, cwd и
// transcript_path с относительным значением CLI (бинарь тестов как chooks, cwd
// процесса = cwd хука) и запрос в процессе с cwd процесса, отличным от cwd
// запроса, дают один код, вывод и одни файлы — и тот исход, что ждёт фикстура
func TestRelativeEnvCLIAndRequest(t *testing.T) {
	for _, c := range relativeEnvCases() {
		t.Run(c.name, func(t *testing.T) {
			cli := runRelativeEnvCLICase(t, c)
			request := runRelativeEnvRequestCase(t, c)
			if cli.code != request.code || cli.stdout != request.stdout || cli.stderr != request.stderr {
				t.Errorf("CLI code=%d stdout=%q stderr=%q\nrequest code=%d stdout=%q stderr=%q", cli.code, cli.stdout, cli.stderr, request.code, request.stdout, request.stderr)
			}
			for _, name := range relativeEnvKeys(cli.tree, request.tree) {
				if cli.tree[name] != request.tree[name] {
					t.Errorf("file %s: CLI %q, request %q", name, cli.tree[name], request.tree[name])
				}
			}
			for mode, got := range map[string]relativeEnvOutcome{"CLI": cli, "request": request} {
				if got.code != c.wantCode || !strings.Contains(got.stdout, c.wantStdout) || !strings.Contains(got.stderr, c.wantStderr) {
					t.Errorf("%s: code=%d stdout=%q stderr=%q, want code=%d stdout~%q stderr~%q", mode, got.code, got.stdout, got.stderr, c.wantCode, c.wantStdout, c.wantStderr)
				}
				if content, ok := got.tree[c.wantFile]; c.wantFile != "" && (!ok || !strings.Contains(content, c.wantContent)) {
					t.Errorf("%s: %s = %q, want ~%q; files %v", mode, c.wantFile, got.tree[c.wantFile], c.wantContent, relativeEnvKeys(got.tree))
				}
			}
		})
	}
}

func relativeEnvKeys(trees ...map[string]string) []string {
	seen := map[string]bool{}
	var keys []string
	for _, tree := range trees {
		for k := range tree {
			if !seen[k] {
				seen[k] = true
				keys = append(keys, k)
			}
		}
	}
	sort.Strings(keys)
	return keys
}

// Часы ночной подсказки — в поясе запроса, а не time.Local процесса
func TestNightHintClockInRequestLocation(t *testing.T) {
	request := hookctx.New(nil, nil, nil, []string{"XDG_STATE_HOME=/nonexistent/st", "TZ=Asia/Kathmandu"}, t.TempDir(), 1, 2)
	deps, err := nightHintDepsContext(context.Background(), &core.Config{}, time.Now().Add(time.Second), request)
	if err != nil {
		t.Fatal(err)
	}
	if loc := deps.Now().Location().String(); loc != "Asia/Kathmandu" {
		t.Errorf("night hint clock zone %q, want the request's Asia/Kathmandu", loc)
	}
}

// Часы журнала understand-check — в поясе запроса: время записи пишется со
// смещением
func TestUnderstandClockInRequestLocation(t *testing.T) {
	dir := t.TempDir()
	approved := filepath.Join(dir, "approved")
	if err := os.WriteFile(approved, []byte("opus claude-opus-5-5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	request := hookctx.New(nil, nil, nil, []string{"HOME=" + dir, "XDG_RUNTIME_DIR=" + dir, "XDG_STATE_HOME=" + dir, "MODELS_APPROVED=" + approved, "TZ=Asia/Kathmandu"}, dir, 1, 2)
	hook, err := newUnderstandHook(hookctx.WithRequest(context.Background(), request), &captureLogger{})
	if err != nil {
		t.Fatal(err)
	}
	if loc := hook.Now().Location().String(); loc != "Asia/Kathmandu" {
		t.Errorf("understand check clock zone %q, want the request's Asia/Kathmandu", loc)
	}
}
