package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/aiseeq/chooks/internal/fallbackwatch"
)

// Набор 1 — equal.py: лог в stderr на уровне error, notifier и skill_reload
// выключены
const eq1Config = `logger:
  level: error
  output: stderr
tools:
  git_guard: {enabled: true}
  jira_style: {enabled: true}
  kill_guard: {enabled: true}
  model_guard: {enabled: true}
  rm_guard: {enabled: true}
  rsync_guard: {enabled: true}
  write_guard: {enabled: true}
  notifier: {enabled: false}
  skill_reload: {enabled: false}
  webfetch_fallback: {enabled: true}
  night_hint: {enabled: true}
`

// Набор 2 — equal2.py: лог в файл на уровне debug, notifier и skill_reload
// включены
const eq2Config = `logger:
  level: debug
  output: file
  file: ~/.claude/logs/chooks.log
tools:
  git_guard: {enabled: true}
  jira_style: {enabled: true}
  kill_guard: {enabled: true}
  model_guard: {enabled: true}
  rm_guard: {enabled: true}
  rsync_guard: {enabled: true}
  write_guard: {enabled: true}
  notifier: {enabled: true, sound: false, desktop: false}
  skill_reload: {enabled: true}
  webfetch_fallback: {enabled: true}
  night_hint: {enabled: true}
`

const (
	homeMark        = "@HOME@"
	workMark        = "@WORK@"
	outputClipLimit = 600
	fileClipLimit   = 700
)

// eqCase — один вход: подкоманда, JSON stdin (или сырой), аргументы, правки env
// и cwd процесса
type eqCase struct {
	name, suite, cmd string
	data             any
	raw              []byte
	hasRaw           bool
	args             []string
	env              map[string]string
	drop             []string
	procCwd          string
}

type obj = map[string]any

// extraCasesJSON — добавочные входы equal2.py в том же порядке; данные, не
// код: модели в них — входы проверок, а не запуск моделей
//
//go:embed testdata/extra-cases.json
var extraCasesJSON []byte

// selfPIDMark — в env входа заменяется pid этого процесса (CLAUDE_PID жив)
const selfPIDMark = "@SELF_PID@"

func extraCases() ([]eqCase, error) {
	var recs []struct {
		Cmd     string            `json:"cmd"`
		Data    any               `json:"data"`
		Raw     *string           `json:"raw"`
		Args    []string          `json:"args"`
		Env     map[string]string `json:"env"`
		Drop    []string          `json:"drop"`
		ProcCwd string            `json:"proc_cwd"`
	}
	if err := json.Unmarshal(extraCasesJSON, &recs); err != nil {
		return nil, fmt.Errorf("extra cases: %w", err)
	}
	self := strconv.Itoa(os.Getpid())
	cases := make([]eqCase, 0, len(recs))
	for i, r := range recs {
		c := eqCase{suite: "equal2", cmd: r.Cmd, data: r.Data, args: r.Args, drop: r.Drop, procCwd: r.ProcCwd}
		if r.Raw != nil {
			c.raw, c.hasRaw = []byte(*r.Raw), true
		}
		if r.Env != nil {
			c.env = map[string]string{}
			for k, v := range r.Env {
				c.env[k] = strings.ReplaceAll(v, selfPIDMark, self)
			}
		}
		c.name = fmt.Sprintf("equal2:extra%02d:%s %s", i, c.cmd, strings.Join(c.args, " "))
		cases = append(cases, c)
	}
	return cases, nil
}

// corpusCases — входы eq1/inputs: подкаталог — подкоманда
func corpusCases(inputs, suite string) ([]eqCase, error) {
	dirs, err := os.ReadDir(inputs)
	if err != nil {
		return nil, fmt.Errorf("inputs: %w", err)
	}
	var out []eqCase
	for _, d := range dirs {
		files, err := filepath.Glob(filepath.Join(inputs, d.Name(), "*.json"))
		if err != nil {
			return nil, fmt.Errorf("glob: %w", err)
		}
		slices.Sort(files)
		for _, f := range files {
			raw, err := os.ReadFile(f)
			if err != nil {
				return nil, fmt.Errorf("read input: %w", err)
			}
			var data any
			if err := json.Unmarshal(raw, &data); err != nil {
				return nil, fmt.Errorf("input %s: %w", f, err)
			}
			e := eqCase{name: suite + ":" + d.Name() + "/" + filepath.Base(f), suite: suite, cmd: d.Name(), data: data}
			switch {
			case d.Name() == "pre-tool-use" && filepath.Base(f) == "00.json":
				e.args = []string{"--verbose"}
			case suite == "equal" && d.Name() == "pre-tool-use" && filepath.Base(f) == "01.json":
				e.args = []string{"--config", homeMark + "/.config/chooks/config.yaml", "--timeout", "5s"}
			}
			out = append(out, e)
		}
	}
	return out, nil
}

func subst(v any, home, work string) any {
	switch x := v.(type) {
	case string:
		return strings.NewReplacer(homeMark, home, workMark, work).Replace(x)
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = subst(item, home, work)
		}
		return out
	case obj:
		out := obj{}
		for k, item := range x {
			out[k] = subst(item, home, work)
		}
		return out
	default:
		return v
	}
}

func marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("json: %w", err)
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// Офлайн-скрипты вместо webtext, claude, codex и dbus-launch из equal2.py:
// пишут вызов в $HOME и отвечают постоянным текстом, модели не запускаются
const (
	webtextScript = "#!/bin/sh\nprintf '%s\\n' '{\"ok\":true,\"source\":\"direct\",\"path\":\"page.txt\"}'\n"
	claudeScript  = "#!/bin/sh\necho \"claude $*\" >> \"$HOME/stub-calls.log\"\n{ pwd; env | sort; } >> \"$HOME/stub-env.log\"\ncat >/dev/null\n" +
		`printf '%s\n' '{"type":"result","result":"{\"verdict\":\"ok\",\"why\":\"stub\"}","usage":{"input_tokens":1,"output_tokens":1}}'` + "\n"
	codexScript = "#!/bin/sh\necho \"codex $*\" >> \"$HOME/stub-calls.log\"\ncat >/dev/null\necho '{}'\n"
	dbusScript  = "#!/bin/sh\necho \"dbus-launch\" >> \"$HOME/stub-calls.log\"\nexit 1\n"
)

// sandbox — песочница одного вызова, сбрасывается перед каждым
type sandbox struct {
	root, home, work, run string
}

func prepare(root, suite string) (sandbox, error) {
	if err := os.RemoveAll(root); err != nil {
		return sandbox{}, fmt.Errorf("reset sandbox: %w", err)
	}
	s := sandbox{root: root, home: filepath.Join(root, "home"), work: filepath.Join(root, "work"), run: filepath.Join(root, "run")}
	for _, d := range []string{".config/chooks", ".config/models", ".claude/logs", ".claude/rules", ".claude/projects/-work", "bin", ".local/state"} {
		if err := os.MkdirAll(filepath.Join(s.home, d), 0o700); err != nil {
			return s, fmt.Errorf("sandbox: %w", err)
		}
	}
	for _, d := range []string{s.work, s.run, filepath.Join(root, "tmp")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return s, fmt.Errorf("sandbox: %w", err)
		}
	}
	config := eq1Config
	if suite == "equal2" {
		config = eq2Config
	}
	transcript := []obj{
		{"parentUuid": nil, "isSidechain": false, "type": "user", "uuid": "u1", "timestamp": "2026-11-21T10:00:00.000Z",
			"sessionId": "s1", "origin": obj{"kind": "human"}, "message": obj{"role": "user", "content": "поправь правило про git в CLAUDE.md"}},
		{"parentUuid": "u1", "isSidechain": false, "type": "assistant", "uuid": "a1", "timestamp": "2026-11-21T10:00:05.000Z",
			"sessionId": "s1", "message": obj{"role": "assistant", "content": []any{obj{"type": "tool_use", "id": "t1", "name": "Write", "input": obj{}}}}},
	}
	var lines []string
	for _, line := range transcript {
		b, err := marshal(line)
		if err != nil {
			return s, err
		}
		lines = append(lines, string(b))
	}
	settings := `{"hooks": {"PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "$HOME/bin/chooks pre-tool-use", "timeout": 15}]}]}}`
	files := map[string]string{
		s.home + "/.config/chooks/config.yaml":      config,
		s.home + "/relcfg.yaml":                     strings.Replace(config, "level: debug", "level: info", 1),
		s.home + "/.config/models/approved":         "opus claude-opus-5-5\nsonnet claude-sonnet-5-5\n",
		s.home + "/.claude/CLAUDE.md":               "# rules\n",
		s.home + "/.claude/rules/git.md":            "a\n",
		s.home + "/.claude/settings.json":           settings,
		s.home + "/.claude/projects/-work/s1.jsonl": strings.Join(lines, "\n") + "\n",
		s.work + "/page.txt":                        "Offline webtext fixture.\n",
		s.work + "/body.json":                       `{"body": "готово — сейчас"}`,
		s.work + "/log.txt":                         "x\n",
	}
	scripts := map[string]string{"webtext": webtextScript}
	if suite == "equal2" {
		scripts["claude"], scripts["codex"], scripts["dbus-launch"] = claudeScript, codexScript, dbusScript
	}
	for name, body := range scripts {
		files[s.home+"/bin/"+name] = body
	}
	for path, body := range files {
		mode := os.FileMode(0o600)
		if filepath.Base(filepath.Dir(path)) == "bin" {
			mode = 0o700
		}
		if err := os.WriteFile(path, []byte(body), mode); err != nil {
			return s, fmt.Errorf("sandbox file: %w", err)
		}
	}
	return s, nil
}

var (
	reTime    = regexp.MustCompile(`\d{4}-\d\d-\d\d[ T]\d\d:\d\d:\d\d(?:\.\d+)?(?:Z|[+-]\d\d:\d\d)?`)
	reElapsed = regexp.MustCompile(`((?:elapsed_ms|timeout_ms)=)\d+`)
	reTimer   = regexp.MustCompile(`(⏱  )\d+(?:\.\d+)?( ms)`)
	reStages  = regexp.MustCompile(`stages=\S+`)
	reDigits  = regexp.MustCompile(`=\d+`)
	reDur     = regexp.MustCompile(`((?:duration|took)=)\S+`)
	reMS      = regexp.MustCompile(`( ms=)\d+`)
	reLatency = regexp.MustCompile(`("latency_ms":)\d+`)
)

// dropLines — строки, зависящие от нагрузки машины, и строка запасного пути
// (у отдельного CLI её нет)
var dropLines = []string{`msg="process skipped"`, "hook slow", `msg="hook via CLI fallback"`}

// normalize — время, длительности и pid хука заменены метками
func normalize(raw []byte, pid int) []byte {
	raw = reTime.ReplaceAll(raw, []byte("<time>"))
	raw = reElapsed.ReplaceAll(raw, []byte("${1}<ms>"))
	raw = reTimer.ReplaceAll(raw, []byte("${1}<ms>${2}"))
	raw = reStages.ReplaceAllFunc(raw, func(m []byte) []byte { return reDigits.ReplaceAll(m, []byte("=N")) })
	raw = reDur.ReplaceAll(raw, []byte("${1}<d>"))
	raw = reMS.ReplaceAll(raw, []byte("${1}<ms>"))
	raw = reLatency.ReplaceAll(raw, []byte("${1}0"))
	var kept [][]byte
	for line := range bytes.SplitSeq(raw, []byte("\n")) {
		if !slices.ContainsFunc(dropLines, func(d string) bool { return bytes.Contains(line, []byte(d)) }) {
			kept = append(kept, line)
		}
	}
	return replacePID(bytes.Join(kept, []byte("\n")), pid)
}

// replacePID — pid отдельным числом (не частью другого числа) меняется на метку
func replacePID(raw []byte, pid int) []byte {
	p := strconv.Itoa(pid)
	var out []byte
	for i := 0; i < len(raw); {
		if !isDigit(raw[i]) {
			out = append(out, raw[i])
			i++
			continue
		}
		j := i
		for j < len(raw) && isDigit(raw[j]) {
			j++
		}
		if string(raw[i:j]) == p {
			out = append(out, "<hook-pid>"...)
		} else {
			out = append(out, raw[i:j]...)
		}
		i = j
	}
	return out
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// tsvNorm — время и длительности в журнале хуков. Столбцы сверх шести —
// исполнитель (daemon, cli) и причина ухода в CLI — есть только у вызова
// через прокси: у отдельного CLI их нет, сверка — по первым шести
func tsvNorm(raw []byte) []byte {
	var lines []string
	for line := range strings.SplitSeq(strings.TrimSuffix(string(raw), "\n"), "\n") {
		f := strings.Split(line, "\t")
		if len(f) >= 6 {
			f = f[:6]
			f[0], f[3], f[4] = "<time>", "N", "N"
		}
		lines = append(lines, strings.Join(f, "\t"))
	}
	return []byte(strings.Join(lines, "\n"))
}

func snapshot(root string, pid int) (map[string]string, error) {
	snap := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return fmt.Errorf("rel: %w", err)
		}
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return fmt.Errorf("readlink: %w", err)
			}
			snap[rel] = "symlink:" + target
		case fallbackState(rel):
			// Счёт ухода в CLI (fallbackwatch) пишет только CLI запасного пути
		case d.IsDir():
			snap[rel] = "<dir>"
		default:
			data, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("snapshot: %w", err)
			}
			if strings.HasSuffix(rel, "chooks-hooks.tsv") {
				snap[rel] = string(tsvNorm(data))
			} else {
				snap[rel] = string(normalize(data, pid))
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk sandbox: %w", err)
	}
	return snap, nil
}

// fallbackState — файлы счёта fallbackwatch в каталоге состояния chooks
func fallbackState(rel string) bool {
	base := filepath.Base(rel)
	return base == fallbackwatch.FileName || base == fallbackwatch.LockName || strings.HasPrefix(base, "."+fallbackwatch.FileName+".tmp-")
}

// observed — наблюдаемое вызова после нормализации
type observed struct {
	code           int
	stdout, stderr string
	files          map[string]string
}

type equalRun struct {
	sandbox         string
	socket, wrapper string
	fallbackLog     string
}

func (r equalRun) invoke(binary string, c *eqCase) (observed, string, error) {
	s, err := prepare(r.sandbox, c.suite)
	if err != nil {
		return observed{}, "", err
	}
	env := map[string]string{
		"HOME": s.home, "XDG_CONFIG_HOME": s.home + "/.config", "XDG_STATE_HOME": s.home + "/.local/state",
		"XDG_RUNTIME_DIR": s.run, "CLAUDE_CONFIG_DIR": s.home + "/.claude", "PATH": s.home + "/bin:/usr/bin:/bin",
		"LANG": "C.UTF-8", "DBUS_SESSION_BUS_ADDRESS": "unix:path=" + s.root + "/no-bus",
		"DBUS_SYSTEM_BUS_ADDRESS": "unix:path=" + s.root + "/no-system-bus",
	}
	if c.suite == "equal2" {
		env["TMPDIR"] = s.root + "/tmp"
	}
	maps.Copy(env, c.env)
	for _, k := range c.drop {
		delete(env, k)
	}
	// Переменные прокси у обеих сторон: env одинаков
	env["CHOOKS_HOOK_SOCKET"], env["CHOOKS_HOOK_CLI"] = r.socket, r.wrapper
	var list []string
	for _, k := range slices.Sorted(maps.Keys(env)) {
		list = append(list, k+"="+env[k])
	}
	var args []string
	if c.cmd != "" {
		args = append(args, c.cmd)
	}
	marks := strings.NewReplacer(homeMark, s.home, workMark, s.work)
	for _, a := range c.args {
		args = append(args, marks.Replace(a))
	}
	stdin := c.raw
	if !c.hasRaw {
		if stdin, err = marshal(subst(c.data, s.home, s.work)); err != nil {
			return observed{}, "", err
		}
	}
	dir := s.work
	switch c.procCwd {
	case "root":
		dir = s.root
	case "home":
		dir = s.home
	}
	// Журнал запасного пути создан до прогона: его нет — сбой, а не «нет записей»
	info, err := os.Stat(r.fallbackLog)
	if err != nil {
		return observed{}, "", fmt.Errorf("fallback log: %w", err)
	}
	o, err := runOne(binary, args, list, dir, stdin)
	if err != nil {
		return observed{}, "", err
	}
	data, err := os.ReadFile(r.fallbackLog)
	if err != nil {
		return observed{}, "", fmt.Errorf("fallback log: %w", err)
	}
	reason := ""
	if int64(len(data)) > info.Size() {
		reason = strings.TrimSpace(string(data[info.Size():]))
	}
	files, err := snapshot(s.root, o.pid)
	if err != nil {
		return observed{}, "", err
	}
	return observed{code: o.code, stdout: string(normalize(o.stdout, o.pid)), stderr: string(normalize(o.stderr, o.pid)), files: files}, reason, nil
}

func clip(s string, n int, tail bool) string {
	if len(s) <= n {
		return strconv.Quote(s)
	}
	if tail {
		start := len(s) - n
		for start < len(s) && !utf8.RuneStart(s[start]) {
			start++
		}
		return strconv.Quote("…" + s[start:])
	}
	for n > 0 && n < len(s) && !utf8.RuneStart(s[n]) {
		n--
	}
	return strconv.Quote(s[:n] + "…")
}

func diffObserved(a, b observed) []string {
	var out []string
	if a.code != b.code {
		out = append(out, fmt.Sprintf("  exit: cli=%d proxy=%d", a.code, b.code))
	}
	if a.stdout != b.stdout {
		out = append(out, fmt.Sprintf("  stdout: cli=%s\n          proxy=%s", clip(a.stdout, outputClipLimit, false), clip(b.stdout, outputClipLimit, false)))
	}
	if a.stderr != b.stderr {
		out = append(out, fmt.Sprintf("  stderr: cli=%s\n          proxy=%s", clip(a.stderr, outputClipLimit, false), clip(b.stderr, outputClipLimit, false)))
	}
	keys := slices.Sorted(maps.Keys(a.files))
	for k := range b.files {
		if _, ok := a.files[k]; !ok {
			keys = append(keys, k)
		}
	}
	for _, k := range keys {
		x, okA := a.files[k]
		y, okB := b.files[k]
		if x != y || okA != okB {
			out = append(out, fmt.Sprintf("  file %s:\n    cli=%s\n    proxy=%s", k, clip(x, fileClipLimit, true), clip(y, fileClipLimit, true)))
		}
	}
	return out
}

func compareCases(r equalRun, cli, proxy string, cases []eqCase) ([]string, int, int, map[string][]string, error) {
	var lines []string
	mismatches, served := 0, 0
	fallbacks := map[string][]string{}
	for i := range cases {
		c := &cases[i]
		want, _, err := r.invoke(cli, c)
		if err != nil {
			return nil, mismatches, served, fallbacks, err
		}
		got, reason, err := r.invoke(proxy, c)
		if err != nil {
			return nil, mismatches, served, fallbacks, err
		}
		if reason == "" {
			served++
		} else {
			fallbacks[reason] = append(fallbacks[reason], c.name)
		}
		diff := diffObserved(want, got)
		if len(diff) == 0 {
			continue
		}
		mismatches++
		path := "daemon"
		if reason != "" {
			path = "CLI: " + reason
		}
		lines = append(lines, fmt.Sprintf("### %s (path: %s)", c.name, path))
		lines = append(lines, diff...)
	}
	return lines, mismatches, served, fallbacks, nil
}

func runEqual(argv []string) (code int, retErr error) {
	fsFlags := flag.NewFlagSet("equal", flag.ContinueOnError)
	var bin binaries
	bin.register(fsFlags)
	inputs := fsFlags.String("inputs", "", "eq1/inputs из hd/s2a/review2/equality")
	report := fsFlags.String("report", "", "файл отчёта о расхождениях")
	if err := fsFlags.Parse(argv); err != nil {
		return 2, fmt.Errorf("flags: %w", err)
	}
	if err := bin.resolve(); err != nil {
		return 2, err
	}
	stagedCLI, stagedProxy, err := prepareBinaries(bin.work, bin.cli, bin.proxy)
	if err != nil {
		return 2, err
	}
	bin.cli, bin.proxy = stagedCLI, stagedProxy
	eq1, err := corpusCases(*inputs, "equal")
	if err != nil {
		return 2, err
	}
	eq2, err := corpusCases(*inputs, "equal2")
	if err != nil {
		return 2, err
	}
	extra, err := extraCases()
	if err != nil {
		return 2, err
	}
	cases := slices.Concat(eq1, eq2, extra)
	r := equalRun{sandbox: filepath.Join(bin.work, "sandbox"), wrapper: filepath.Join(bin.work, "cli-fallback.sh"),
		fallbackLog: filepath.Join(bin.work, "fallbacks.log")}
	wrapper := "#!/bin/sh\nprintf '%s\\n' \"$CHOOKS_FALLBACK\" >> " + strconv.Quote(r.fallbackLog) + "\nexec " + strconv.Quote(bin.cli) + " \"$@\"\n"
	if err := os.WriteFile(r.wrapper, []byte(wrapper), 0o700); err != nil {
		return 2, fmt.Errorf("wrapper: %w", err)
	}
	if err := os.WriteFile(r.fallbackLog, nil, 0o600); err != nil {
		return 2, fmt.Errorf("fallback log: %w", err)
	}
	logFile, err := os.Create(filepath.Join(bin.work, "daemon.log"))
	if err != nil {
		return 2, fmt.Errorf("daemon log: %w", err)
	}
	defer logFile.Close()
	d, err := startDaemon(bin.cli, logFile, "")
	if err != nil {
		return 2, err
	}
	r.socket = d.socket
	lines, mismatches, served, fallbacks, runErr := compareCases(r, bin.cli, bin.proxy, cases)
	daemonExit := d.stop()
	if runErr != nil {
		return 2, runErr
	}
	head := []string{
		fmt.Sprintf("cases: %d (equal %d, equal2 %d)", len(cases), len(eq1), len(cases)-len(eq1)),
		fmt.Sprintf("mismatches: %d", mismatches),
		fmt.Sprintf("served by daemon: %d", served),
		fmt.Sprintf("CLI fallback: %d", len(cases)-served),
	}
	for _, reason := range slices.Sorted(maps.Keys(fallbacks)) {
		head = append(head, fmt.Sprintf("  %s: %d — %s", reason, len(fallbacks[reason]), strings.Join(fallbacks[reason], ", ")))
	}
	head = append(head, fmt.Sprintf("daemon exit: %d", daemonExit))
	fmt.Println(strings.Join(head, "\n"))
	if *report != "" {
		if err := os.WriteFile(*report, []byte(strings.Join(append(head, lines...), "\n")+"\n"), 0o600); err != nil {
			return 2, fmt.Errorf("report: %w", err)
		}
	}
	if mismatches > 0 || daemonExit != 0 {
		return 1, nil
	}
	return 0, nil
}
