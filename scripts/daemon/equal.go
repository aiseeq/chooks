package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
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
	"time"
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

const (
	fixtureMTimeYear = 2026
	fixtureMTimeDay  = 26
)

var fixtureMTime = time.Date(fixtureMTimeYear, time.November, fixtureMTimeDay, 0, 0, 0, 0, time.UTC)

// eqCase — один вход: подкоманда, JSON stdin (или сырой), аргументы, правки env
// и cwd процесса
type eqCase struct {
	name, suite, cmd string
	branch           string
	project          string
	data             any
	raw              []byte
	transcript       []byte
	fixtures         map[string]string
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

func corpusMetadataPath(inputs, subdir, cmd, name, ext string) string {
	return filepath.Join(inputs, subdir, cmd, strings.TrimSuffix(filepath.Base(name), ".json")+ext)
}

func loadCorpusProject(inputs, cmd, name string, e *eqCase) error {
	path := corpusMetadataPath(inputs, "sources", cmd, name, ".json")
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read source metadata %s: %w", path, err)
	}
	var source struct {
		CWD string `json:"cwd"`
	}
	if err := json.Unmarshal(data, &source); err != nil {
		return fmt.Errorf("source metadata %s: %w", path, err)
	}
	e.project = source.CWD
	return nil
}

func loadCorpusBranch(inputs, cmd, name string, e *eqCase) error {
	path := corpusMetadataPath(inputs, "branches", cmd, name, ".txt")
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read branch label %s: %w", path, err)
	}
	e.branch = strings.TrimSpace(string(data))
	return nil
}

func loadCorpusFixtures(inputs, cmd, name string, e *eqCase) error {
	path := corpusMetadataPath(inputs, "fixtures", cmd, name, ".json")
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read fixture %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &e.fixtures); err != nil {
		return fmt.Errorf("fixture %s: %w", path, err)
	}
	return nil
}

func loadCorpusTranscript(inputs, cmd, name string, e *eqCase) error {
	path := corpusMetadataPath(inputs, "transcripts", cmd, name, ".jsonl")
	snapshot, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read transcript snapshot %s: %w", path, err)
	}
	e.transcript = snapshot
	return nil
}

func loadCorpusMetadata(inputs, cmd, name string, e *eqCase) error {
	for _, load := range []func(string, string, string, *eqCase) error{
		loadCorpusProject, loadCorpusBranch, loadCorpusFixtures, loadCorpusTranscript,
	} {
		if err := load(inputs, cmd, name, e); err != nil {
			return err
		}
	}
	return nil
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
			if err := loadCorpusMetadata(inputs, d.Name(), f, &e); err != nil {
				return nil, err
			}
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
	claudeScript  = "#!/bin/sh\n# equal-stub-deny; recorded denial response: {\"verdict\":\"fix\"}\necho \"claude $*\" >> \"$HOME/stub-calls.log\"\n{ pwd; env | sort; } >> \"$HOME/stub-env.log\"\nrequest=$(cat)\n" +
		`deny=0; if [ -f "$HOME/deny-term.txt" ]; then term=$(cat "$HOME/deny-term.txt"); case "$request" in *"$term"*) deny=1 ;; esac; fi; if [ "$deny" = 1 ] && [ -f "$HOME/deny-answer.json" ]; then answer=$(cat "$HOME/deny-answer.json"); else answer='{"verdict":"ok"}'; fi` + "\n" +
		`escaped=$(printf '%s' "$answer" | sed 's/\\/\\\\/g; s/"/\\"/g')` + "\n" +
		`printf '{"type":"result","result":"%s","usage":{"input_tokens":1,"output_tokens":1}}\n' "$escaped"` + "\n"
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

func installTranscript(s sandbox, snapshot []byte) error {
	if len(snapshot) == 0 {
		return nil
	}
	marks := strings.NewReplacer(homeMark, s.home, workMark, s.work)
	path := marks.Replace(filepath.Join(workMark, "transcript.jsonl"))
	snapshot = []byte(marks.Replace(string(snapshot)))
	if err := os.WriteFile(path, snapshot, 0o600); err != nil {
		return fmt.Errorf("transcript snapshot: %w", err)
	}
	return nil
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
	reUUID    = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f-]{27,}\b`)
	reToolID  = regexp.MustCompile(`\btoolu_[A-Za-z0-9]+\b`)
)

func scrubPayloadIDs(value any) any {
	switch v := value.(type) {
	case string:
		v = reTime.ReplaceAllString(v, "<time>")
		v = reUUID.ReplaceAllString(v, "<uuid>")
		v = reToolID.ReplaceAllString(v, "<tool-id>")
		return v
	case []any:
		out := make([]any, len(v))
		for i := range v {
			out[i] = scrubPayloadIDs(v[i])
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, item := range v {
			lower := strings.ToLower(k)
			if lower == "session_id" || lower == "sessionid" || lower == "transcript_path" || lower == "tool_use_id" || lower == "uuid" || lower == "timestamp" || lower == "cwd" {
				continue
			}
			out[k] = scrubPayloadIDs(item)
		}
		return out
	default:
		return v
	}
}

func distinctPayload(c *eqCase) (string, error) {
	fixtures := map[string]any{}
	for k, v := range c.fixtures {
		fixtures[k] = v
	}
	canonical, err := json.Marshal(scrubPayloadIDs(map[string]any{"input": c.data, "transcript": string(c.transcript), "fixtures": fixtures}))
	if err != nil {
		return "", fmt.Errorf("canonical payload: %w", err)
	}
	return string(canonical), nil
}

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

func normalizeSandbox(raw []byte, pid int, root string) []byte {
	raw = normalize(raw, pid)
	return bytes.ReplaceAll(raw, []byte(root), []byte("@SANDBOX@"))
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
			data = bytes.ReplaceAll(data, []byte(filepath.Join(root, "work", "transcript.jsonl")), []byte("@WORK@/transcript.jsonl"))
			if strings.HasSuffix(rel, "chooks-hooks.tsv") {
				snap[rel] = string(tsvNorm(data))
			} else {
				data = normalizeSandbox(data, pid, root)
				snap[rel] = string(data)
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
	elapsed        time.Duration
}

type equalRun struct {
	sandbox         string
	socket, wrapper string
	fallbackLog     string
	invocations     int
	timingLines     []string
}

func (r *equalRun) invoke(binary string, c *eqCase) (result observed, reason string, retErr error) {
	root := filepath.Join(r.sandbox, fmt.Sprintf("case-%06d", r.invocations))
	r.invocations++
	s, err := prepare(root, c.suite)
	if err != nil {
		return observed{}, "", fmt.Errorf("prepare sandbox: %w", err)
	}
	defer func() {
		if err := removeSandbox(s.root); err != nil && retErr == nil {
			retErr = fmt.Errorf("cleanup sandbox: %w", err)
		}
	}()
	if err := installTranscript(s, c.transcript); err != nil {
		return observed{}, "", err
	}
	if err := installFixtures(s, c.fixtures); err != nil {
		return observed{}, "", err
	}
	env := invocationEnv(s, c)
	// Переменные прокси у обеих сторон: env одинаков
	env["CHOOKS_HOOK_SOCKET"], env["CHOOKS_HOOK_CLI"] = r.socket, r.wrapper
	var list []string
	for _, k := range slices.Sorted(maps.Keys(env)) {
		list = append(list, k+"="+env[k])
	}
	args, stdin, err := invocationInput(s, c)
	if err != nil {
		return observed{}, "", fmt.Errorf("prepare hook input: %w", err)
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
	var o outcome
	if c.cmd == "user-prompt-submit" && c.branch == "block" {
		o, err = runOneUnderClaude(binary, args, list, dir, stdin, "unlisted")
	} else {
		o, err = runOne(binary, args, list, dir, stdin)
	}
	if err != nil {
		return observed{}, "", fmt.Errorf("run hook: %w", err)
	}
	data, err := os.ReadFile(r.fallbackLog)
	if err != nil {
		return observed{}, "", fmt.Errorf("fallback log: %w", err)
	}
	reason = ""
	if int64(len(data)) > info.Size() {
		reason = strings.TrimSpace(string(data[info.Size():]))
	}
	files, err := snapshot(s.root, o.pid)
	if err != nil {
		return observed{}, "", err
	}
	return observed{code: o.code, stdout: string(normalizeSandbox(o.stdout, o.pid, s.root)), stderr: string(normalizeSandbox(o.stderr, o.pid, s.root)), files: files, elapsed: o.elapsed}, reason, nil
}

func installFixtures(s sandbox, fixtures map[string]string) error {
	for name, body := range fixtures {
		marks := strings.NewReplacer(homeMark, s.home, workMark, s.work)
		name, body = marks.Replace(name), marks.Replace(body)
		rel := filepath.Clean(filepath.FromSlash(name))
		if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("fixture path escapes sandbox: %q", name)
		}
		path := filepath.Join(s.root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return fmt.Errorf("fixture directory: %w", err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			return fmt.Errorf("fixture file: %w", err)
		}
		if err := os.Chtimes(path, fixtureMTime, fixtureMTime); err != nil {
			return fmt.Errorf("fixture time: %w", err)
		}
	}
	return nil
}

func invocationEnv(s sandbox, c *eqCase) map[string]string {
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
	return env
}

func invocationInput(s sandbox, c *eqCase) ([]string, []byte, error) {
	var args []string
	if c.cmd != "" {
		args = append(args, c.cmd)
	}
	marks := strings.NewReplacer(homeMark, s.home, workMark, s.work)
	for _, arg := range c.args {
		args = append(args, marks.Replace(arg))
	}
	if c.hasRaw {
		return args, c.raw, nil
	}
	stdin, err := marshal(subst(c.data, s.home, s.work))
	if err != nil {
		return nil, nil, fmt.Errorf("marshal hook input: %w", err)
	}
	return args, stdin, nil
}

var removeSandboxAttempt = os.RemoveAll

func removeSandbox(root string) error {
	var err error
	for range 10 {
		if err = removeSandboxAttempt(root); err == nil {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return err
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

type hookEqualStats struct {
	inputs, mismatches, served int
	branches                   map[string]int
	branchPayloads             map[string]map[string]bool
	branchSessions             map[string]map[string]bool
	branchProjects             map[string]map[string]bool
}

func decisionBranch(hook string, result observed) string {
	denied := bytes.Contains([]byte(result.stdout), []byte(`"permissionDecision":"deny"`))
	calledJudge := false
	for path := range result.files {
		if strings.HasSuffix(path, "stub-calls.log") {
			calledJudge = true
			break
		}
	}
	switch hook {
	case "understand-check":
		if denied {
			return "judge-deny"
		}
		if calledJudge {
			return "judge-ok"
		}
		return "skip"
	case "post-tool-batch":
		if strings.TrimSpace(result.stdout) != "" {
			return "reminder"
		}
		return "skip"
	case "user-prompt-submit":
		if result.code == 2 && strings.Contains(result.stderr, "не одобрена") {
			return "block"
		}
		if strings.TrimSpace(result.stdout) != "" {
			return "reminder"
		}
		return "skip"
	default:
		return "other"
	}
}

func compareCases(r *equalRun, cli, proxy string, cases []eqCase) ([]string, int, int, map[string][]string, map[string]hookEqualStats, error) {
	var lines []string
	mismatches, served := 0, 0
	fallbacks := map[string][]string{}
	byHook := map[string]hookEqualStats{}
	for i := range cases {
		c := &cases[i]
		// Reuse the exact absolute root for both executions. invoke cleans the CLI
		// sandbox before the daemon prepares it, so path-derived keys are compared as written.
		r.invocations = i * 2
		stats := byHook[c.cmd]
		stats.inputs++
		if stats.branches == nil {
			stats.branches = map[string]int{}
			stats.branchPayloads = map[string]map[string]bool{}
			stats.branchSessions = map[string]map[string]bool{}
			stats.branchProjects = map[string]map[string]bool{}
		}
		payload, err := distinctPayload(c)
		if err != nil {
			return nil, mismatches, served, fallbacks, byHook, err
		}
		want, _, err := r.invoke(cli, c)
		if err != nil {
			return nil, mismatches, served, fallbacks, byHook, err
		}
		r.invocations = i * 2
		got, reason, err := r.invoke(proxy, c)
		if err != nil {
			return nil, mismatches, served, fallbacks, byHook, err
		}
		r.timingLines = append(r.timingLines, fmt.Sprintf("timing %s: CLI %.3f ms, proxy+daemon %.3f ms", c.name, float64(want.elapsed.Microseconds())/1000, float64(got.elapsed.Microseconds())/1000))
		if reason == "" {
			served++
			stats.served++
		} else {
			fallbacks[reason] = append(fallbacks[reason], c.name)
		}
		branch := decisionBranch(c.cmd, want)
		if c.branch != "" && c.branch != branch {
			lines = append(lines, fmt.Sprintf("### %s (branch: want %s, got %s)", c.name, c.branch, branch))
			mismatches++
			stats.mismatches++
		}
		stats.branches[branch]++
		if payloadMap, ok := c.data.(map[string]any); ok {
			if session, ok := payloadMap["session_id"].(string); ok && session != "" {
				if stats.branchSessions[branch] == nil {
					stats.branchSessions[branch] = map[string]bool{}
				}
				stats.branchSessions[branch][session] = true
				project := c.project
				if project == "" {
					project, _ = payloadMap["cwd"].(string)
				}
				if stats.branchProjects[branch] == nil {
					stats.branchProjects[branch] = map[string]bool{}
				}
				stats.branchProjects[branch][project] = true
				if stats.branchPayloads[branch] == nil {
					stats.branchPayloads[branch] = map[string]bool{}
				}
				stats.branchPayloads[branch][payload] = true
			}
		}
		diff := diffObserved(want, got)
		if len(diff) == 0 {
			byHook[c.cmd] = stats
			continue
		}
		mismatches++
		stats.mismatches++
		byHook[c.cmd] = stats
		path := "daemon"
		if reason != "" {
			path = "CLI: " + reason
		}
		lines = append(lines, fmt.Sprintf("### %s (path: %s)", c.name, path))
		lines = append(lines, diff...)
	}
	return lines, mismatches, served, fallbacks, byHook, nil
}

func loadEqualCases(inputs, suite string) ([]eqCase, error) {
	if suite != "equal" && suite != "equal2" && suite != "both" {
		return nil, fmt.Errorf("invalid --suite %q: want equal, equal2 or both", suite)
	}
	var eq1, eq2, extra []eqCase
	var err error
	if suite == "equal" || suite == "both" {
		eq1, err = corpusCases(inputs, "equal")
		if err != nil {
			return nil, err
		}
	}
	if suite == "equal2" || suite == "both" {
		eq2, err = corpusCases(inputs, "equal2")
		if err != nil {
			return nil, err
		}
		if suite == "both" {
			extra, err = extraCases()
			if err != nil {
				return nil, err
			}
		}
	}
	return slices.Concat(eq1, eq2, extra), nil
}

func runEqual(argv []string) (code int, retErr error) {
	fsFlags := flag.NewFlagSet("equal", flag.ContinueOnError)
	var bin binaries
	bin.register(fsFlags)
	inputs := fsFlags.String("inputs", "", "eq1/inputs из hd/s2a/review2/equality")
	extraInputs := fsFlags.String("extra-inputs", "", "дополнительный корпус corpusCases")
	extraSuite := fsFlags.String("extra-suite", "equal", "набор дополнительного корпуса: equal или equal2")
	suite := fsFlags.String("suite", "both", "корпус: equal, equal2 или both")
	report := fsFlags.String("report", "", "файл отчёта о расхождениях")
	timings := fsFlags.Bool("timings", false, "печатать время CLI и прокси для каждого входа")
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
	cases, err := loadEqualCases(*inputs, *suite)
	if err != nil {
		return 2, err
	}
	cases, err = appendExtraCorpus(cases, *extraInputs, *extraSuite)
	if err != nil {
		return 2, err
	}
	r := equalRun{sandbox: filepath.Join(bin.work, "sandbox"), wrapper: filepath.Join(bin.work, "cli-fallback.sh"),
		fallbackLog: filepath.Join(bin.work, "fallbacks.log")}
	wrapper := "#!/bin/sh\nprintf '%s\\n' \"$CHOOKS_FALLBACK\" >> " + strconv.Quote(r.fallbackLog) + "\nexec " + strconv.Quote(bin.cli) + " \"$@\"\n"
	if err := os.WriteFile(r.wrapper, []byte(wrapper), 0o700); err != nil {
		return 2, fmt.Errorf("wrapper: %w", err)
	}
	if err := os.WriteFile(r.fallbackLog, nil, 0o600); err != nil {
		return 2, fmt.Errorf("fallback log: %w", err)
	}
	logFile, err := createDaemonLog(bin.work)
	if err != nil {
		return 2, err
	}
	defer closeDaemonLog(&retErr, logFile)
	d, err := startDaemon(bin.cli, logFile, "")
	if err != nil {
		return 2, err
	}
	r.socket = d.socket
	lines, mismatches, served, fallbacks, byHook, runErr := compareCases(&r, bin.cli, bin.proxy, cases)
	daemonExit := d.stop()
	if runErr != nil {
		return 2, runErr
	}
	head := []string{
		fmt.Sprintf("cases: %d (suite %s)", len(cases), *suite),
		fmt.Sprintf("mismatches: %d", mismatches),
		fmt.Sprintf("served by daemon: %d", served),
		fmt.Sprintf("CLI fallback: %d", len(cases)-served),
	}
	for _, reason := range slices.Sorted(maps.Keys(fallbacks)) {
		head = append(head, fmt.Sprintf("  %s: %d — %s", reason, len(fallbacks[reason]), strings.Join(fallbacks[reason], ", ")))
	}
	for _, hook := range slices.Sorted(maps.Keys(byHook)) {
		stats := byHook[hook]
		head = append(head, fmt.Sprintf("hook %s: inputs %d, mismatches %d, served by daemon %d", hook, stats.inputs, stats.mismatches, stats.served))
		for _, branch := range slices.Sorted(maps.Keys(stats.branches)) {
			head = append(head, fmt.Sprintf("  branch %s: %d, sessions %d, projects %d, distinct payloads %d", branch, stats.branches[branch], len(stats.branchSessions[branch]), len(stats.branchProjects[branch]), len(stats.branchPayloads[branch])))
		}
	}
	head = append(head, fmt.Sprintf("daemon exit: %d", daemonExit))
	fmt.Println(strings.Join(head, "\n"))
	if *timings {
		for _, line := range r.timingLines {
			fmt.Println(line)
		}
	}
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

func createDaemonLog(work string) (*os.File, error) {
	logFile, err := os.Create(filepath.Join(work, "daemon.log"))
	if err != nil {
		return nil, fmt.Errorf("daemon log: %w", err)
	}
	return logFile, nil
}

func closeDaemonLog(retErr *error, logFile *os.File) {
	if err := logFile.Close(); err != nil {
		*retErr = errors.Join(*retErr, fmt.Errorf("close daemon log: %w", err))
	}
}

func appendExtraCorpus(cases []eqCase, inputs, suite string) ([]eqCase, error) {
	if inputs == "" {
		return cases, nil
	}
	if suite != "equal" && suite != "equal2" {
		return nil, fmt.Errorf("invalid --extra-suite %q: want equal or equal2", suite)
	}
	extra, err := corpusCases(inputs, suite)
	if err != nil {
		return nil, err
	}
	return slices.Concat(cases, extra), nil
}
