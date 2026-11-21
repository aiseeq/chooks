package main

// Review probe (not part of the change): a hook request must not read the
// executor's environment or cwd. Each case runs twice through executeHook with
// the same request: once with the process env/cwd equal to the request, once
// with the process env/cwd pointing to an empty "poison" tree. Output, exit
// code and files left in the sandbox must match, and the poison tree must stay
// empty.

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/aiseeq/chooks/internal/hookctx"
)

const poisonConfig = `logger:
  level: debug
  output: file
  file: ~/.claude/logs/chooks.log
tools:
  git_guard: {enabled: true}
  jira_style: {enabled: true}
  kill_guard: {enabled: true}
  model_guard: {enabled: true}
  rm_guard: {enabled: true}
  notifier: {enabled: true, sound: false, desktop: false}
  skill_reload: {enabled: true}
  webfetch_fallback: {enabled: true}
  night_hint: {enabled: true}
`

var poisonDigits = regexp.MustCompile(`\d+`)

func poisonSandbox(t *testing.T, root string) (home, work string) {
	t.Helper()
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	home, work = filepath.Join(root, "home"), filepath.Join(root, "work")
	for _, d := range []string{home + "/.config/chooks", home + "/.config/models", home + "/.claude/logs", home + "/.claude/rules",
		home + "/.claude/projects/-work", home + "/bin", work, root + "/run", root + "/tmp", home + "/.local/state"} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p, s string, mode os.FileMode) {
		if err := os.WriteFile(p, []byte(s), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(home+"/.config/chooks/config.yaml", poisonConfig, 0o600)
	write(home+"/.config/models/approved", "opus claude-opus-5-5\nsonnet claude-sonnet-5-5\n", 0o600)
	write(home+"/.claude/CLAUDE.md", "# rules\n", 0o600)
	write(home+"/.claude/settings.json", `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"$HOME/bin/chooks pre-tool-use","timeout":15}]}]}}`, 0o600)
	write(home+"/.claude/projects/-work/s1.jsonl",
		`{"parentUuid":null,"isSidechain":false,"type":"user","uuid":"u1","timestamp":"2026-11-21T10:00:00.000Z","sessionId":"s1","origin":{"kind":"human"},"message":{"role":"user","content":"поправь правило в CLAUDE.md"}}`+"\n"+
			`{"parentUuid":"u1","isSidechain":false,"type":"assistant","uuid":"a1","timestamp":"2026-11-21T10:00:05.000Z","sessionId":"s1","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Write","input":{}},{"type":"tool_use","id":"t2","name":"Bash","input":{"command":"echo \"unterminated"}}]}}`+"\n", 0o600)
	write(work+"/page.txt", "Offline webtext fixture.\n", 0o600)
	write(work+"/body.json", `{"body":"готово — сейчас"}`, 0o600)
	write(home+"/bin/webtext", "#!/bin/sh\nprintf '%s\\n' '{\"ok\":true,\"source\":\"direct\",\"path\":\"page.txt\"}'\n", 0o700)
	write(home+"/bin/claude", "#!/bin/sh\necho \"claude $*\" >> \"$HOME/stub-calls.log\"\n{ pwd; env | sort; } >> \"$HOME/stub-env.log\"\ncat >/dev/null\nprintf '%s\\n' '{\"type\":\"result\",\"result\":\"{\\\"verdict\\\":\\\"ok\\\",\\\"why\\\":\\\"stub\\\"}\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}'\n", 0o700)
	return home, work
}

func poisonTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		data, _ := os.ReadFile(p)
		rel, _ := filepath.Rel(dir, p)
		out[rel] = poisonDigits.ReplaceAllString(string(data), "N")
		return nil
	})
	return out
}

func setProcess(t *testing.T, env []string, cwd string) {
	t.Helper()
	os.Clearenv()
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		os.Setenv(k, v)
	}
	if err := os.Chdir(cwd); err != nil {
		t.Fatal(err)
	}
}

func TestReviewPoisonedProcess(t *testing.T) {
	saved := os.Environ()
	savedWd, _ := os.Getwd()
	defer func() { setProcess(t, saved, savedWd) }()
	base := t.TempDir()
	root := filepath.Join(base, "sb")
	poison := filepath.Join(base, "poison")
	tp := root + "/home/.claude/projects/-work/s1.jsonl"
	w := root + "/work"
	h := root + "/home"
	cases := []struct{ hook, input string }{
		{hookPreToolUse, `{"session_id":"s1","cwd":"` + w + `","tool_name":"Bash","tool_input":{"command":"grep -r x ~/.cache"}}`},
		{hookPreToolUse, `{"session_id":"s1","cwd":"` + w + `","tool_name":"Bash","tool_input":{"command":"rm -rf \"$HOME/\""}}`},
		{hookPreToolUse, `{"session_id":"s1","cwd":"` + w + `","tool_name":"Bash","tool_input":{"command":"claude -p --model haiku hi"}}`},
		{hookPreToolUse, `{"session_id":"s1","cwd":"` + w + `","tool_name":"Agent","tool_input":{"subagent_type":"x","model":"sonnet","prompt":"p"}}`},
		{hookPreToolUse, `{"session_id":"s1","cwd":"` + w + `","agent_id":"a","tool_name":"Bash","tool_input":{"command":"git -C ~/other reset --hard"}}`},
		{hookPreToolUse, `{"session_id":"s1","cwd":"` + w + `","agent_id":"a","tool_name":"Bash","tool_input":{"command":"git -C $TMPDIR/x reset --hard"}}`},
		{hookPreToolUse, `{"session_id":"s1","cwd":"` + w + `","tool_name":"Bash","tool_input":{"command":"curl -X POST -d @body.json https://x.atlassian.net/rest/api/3/issue/PROJ-1/comment"}}`},
		{hookPreToolUse, `{"session_id":"s1","cwd":"work","tool_name":"Bash","tool_input":{"command":"curl -X POST -d @body.json https://x.atlassian.net/rest/api/3/issue/PROJ-1/comment"}}`},
		{hookPreToolUse, `{"session_id":"s1","cwd":"` + w + `","tool_name":"Bash","tool_input":{"command":"pgrep -f chooks"}}`},
		{hookUnderstandCheck, `{"session_id":"s1","cwd":"` + w + `","transcript_path":"` + tp + `","tool_name":"Write","tool_input":{"file_path":"` + h + `/.claude/CLAUDE.md","content":"x"}}`},
		{hookUnderstandCheck, `{"session_id":"s1","cwd":"` + w + `","transcript_path":"` + tp + `","tool_name":"Bash","tool_input":{"command":"echo x >> ~/.claude/CLAUDE.md"}}`},
		{hookUnderstandCheck, `{"session_id":"s1","cwd":"` + w + `","transcript_path":"` + tp + `","tool_name":"Bash","tool_input":{"command":"echo \"unterminated"}}`},
		{hookStop, `{"session_id":"s1","cwd":"` + w + `","transcript_path":"` + tp + `","hook_event_name":"Stop"}`},
		{hookStop, `{"session_id":"s1","cwd":"` + w + `","hook_event_name":"Stop","background_tasks":[],"session_crons":[]}`},
		{hookNotification, `{"session_id":"s1","cwd":"` + w + `","hook_event_name":"Notification","notification_type":"permission_prompt"}`},
		{hookStopFailure, `{"session_id":"s1","cwd":"` + w + `","hook_event_name":"StopFailure","error":"rate_limit"}`},
		{hookUserPromptSubmit, `{"session_id":"s1","cwd":"` + w + `","transcript_path":"` + tp + `","hook_event_name":"UserPromptSubmit","prompt":"hi"}`},
		{hookPostToolBatch, `{"session_id":"s1","cwd":"` + w + `","transcript_path":"` + tp + `","hook_event_name":"PostToolBatch"}`},
		{hookUnsentCheck, `{"session_id":"s1","cwd":"` + w + `","transcript_path":"` + tp + `","hook_event_name":"Stop","last_assistant_message":"Ещё не отправлено: ответ."}`},
		{hookNightHint, `{"session_id":"s1","cwd":"` + w + `","transcript_path":"` + tp + `","hook_event_name":"SessionStart"}`},
		{hookNightHint, `{"session_id":"s1","cwd":"` + w + `","transcript_path":"` + tp + `","hook_event_name":"PreToolUse","tool_name":"AskUserQuestion","tool_input":{}}`},
		{hookNoPrompt, `{"session_id":"s1","hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"ls"}}`},
		{hookWebFetchFallback, `{"session_id":"s1","cwd":"` + w + `","hook_event_name":"PostToolUse","tool_name":"WebFetch","tool_input":{"url":"https://example.org/"},"tool_response":{"code":403,"result":"Forbidden","url":"https://example.org/"}}`},
		{hookWebFetchFallback, `{"session_id":"s1","cwd":"` + w + `","hook_event_name":"PostToolUseFailure","tool_name":"WebFetch","tool_input":{"url":"https://example.org/"},"error":"Request failed with status code 403"}`},
		{"statusline", `{"session_id":"s1","transcript_path":"` + tp + `","model":{"id":"claude-opus-5-5"},"workspace":{"current_dir":"` + w + `"},"rate_limits":{"seven_day":{"used_percentage":37,"resets_at":1795000000}}}`},
		{"statusline", `{"session_id":"s1"}`},
	}
	env := func() []string {
		return []string{"HOME=" + h, "XDG_CONFIG_HOME=" + h + "/.config", "XDG_STATE_HOME=" + h + "/.local/state",
			"XDG_RUNTIME_DIR=" + root + "/run", "CLAUDE_CONFIG_DIR=" + h + "/.claude", "TMPDIR=" + root + "/tmp",
			"PATH=" + h + "/bin:/usr/bin:/bin", "LANG=C.UTF-8", "CLAUDE_PID=" + fmt.Sprint(os.Getpid()),
			"DBUS_SESSION_BUS_ADDRESS=unix:path=" + root + "/no-bus", "DBUS_SYSTEM_BUS_ADDRESS=unix:path=" + root + "/no-sys-bus",
			"KONSOLE_DBUS_SERVICE=:1.9", "KONSOLE_DBUS_SESSION=/Sessions/1", "KONSOLE_DBUS_WINDOW=/Windows/1"}
	}
	poisonEnv := []string{"HOME=" + poison + "/home", "XDG_CONFIG_HOME=" + poison + "/config", "XDG_STATE_HOME=" + poison + "/state",
		"XDG_RUNTIME_DIR=" + poison + "/run", "CLAUDE_CONFIG_DIR=" + poison + "/claude", "TMPDIR=" + poison + "/tmp",
		"PATH=" + poison + "/bin", "MODELS_APPROVED=" + poison + "/approved", "CLAUDE_PID=1",
		"KONSOLE_DBUS_SERVICE=:1.poison", "KONSOLE_DBUS_SESSION=/Sessions/poison", "KONSOLE_DBUS_WINDOW=/Windows/poison",
		"DBUS_SESSION_BUS_ADDRESS=unix:path=" + poison + "/bus", "DBUS_SYSTEM_BUS_ADDRESS=unix:path=" + poison + "/sysbus"}
	run := func(i int, poisoned bool) (string, map[string]string) {
		_, work := poisonSandbox(t, root)
		if err := os.RemoveAll(poison); err != nil {
			t.Fatal(err)
		}
		for _, d := range []string{"home", "config", "state", "run", "claude", "tmp", "bin", "cwd"} {
			if err := os.MkdirAll(filepath.Join(poison, d), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		if poisoned {
			setProcess(t, poisonEnv, filepath.Join(poison, "cwd"))
		} else {
			setProcess(t, env(), work)
		}
		var stdout, stderr bytes.Buffer
		req := hookctx.New(strings.NewReader(cases[i].input), &stdout, &stderr, env(), work, os.Getpid(), os.Getppid())
		code := executeHook(context.Background(), req, cases[i].hook)
		setProcess(t, saved, savedWd)
		tree := poisonTree(t, root)
		for k, v := range poisonTree(t, poison) {
			tree["POISON/"+k] = v
		}
		return fmt.Sprintf("code=%d\nstdout=%s\nstderr=%s", code, poisonDigits.ReplaceAllString(stdout.String(), "N"), poisonDigits.ReplaceAllString(stderr.String(), "N")), tree
	}
	for i := range cases {
		wantOut, wantTree := run(i, false)
		gotOut, gotTree := run(i, true)
		if i == 9 && wantTree["home/stub-calls.log"] == "" {
			t.Errorf("probe did not reach the judge: %v", wantTree)
		}
		if i == 9 && !strings.Contains(wantOut, "не удалось разобрать команду Bash") {
			t.Errorf("probe did not classify historical Bash: %s", wantOut)
		}
		if i == 0 && !strings.Contains(wantOut, "additionalContext") {
			t.Errorf("probe did not reach wide-search hint: %s", wantOut)
		}
		t.Logf("case %d %s: %d files, %s", i, cases[i].hook, len(wantTree), strings.ReplaceAll(wantOut, "\n", " | ")[:min(160, len(wantOut))])
		if wantOut != gotOut {
			t.Errorf("case %d %s: output differs under poisoned process\n clean: %q\npoison: %q", i, cases[i].hook, wantOut, gotOut)
		}
		keys := map[string]bool{}
		for k := range wantTree {
			keys[k] = true
		}
		for k := range gotTree {
			keys[k] = true
		}
		var names []string
		for k := range keys {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			if wantTree[k] != gotTree[k] {
				a, b := wantTree[k], gotTree[k]
				if len(a) > 600 {
					a = a[len(a)-600:]
				}
				if len(b) > 600 {
					b = b[len(b)-600:]
				}
				t.Errorf("case %d %s: file %s differs\n clean: %q\npoison: %q", i, cases[i].hook, k, a, b)
			}
		}
	}
}
