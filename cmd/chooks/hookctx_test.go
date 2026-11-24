package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aiseeq/akit/proc"
	"github.com/aiseeq/chooks/internal/core"
	"github.com/aiseeq/chooks/internal/hookctx"
)

func TestNoPromptRequestStreams(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("logger:\n  output: stderr\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	request := hookctx.New(strings.NewReader(`{"hook_event_name":"Elicitation"}`), &stdout, &stderr, nil, t.TempDir(), 123, 45)
	request.ConfigPath = path
	code, err := runHook(hookctx.WithRequest(context.Background(), request), hookNoPrompt)
	if err != nil || code != exitAllowed {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if !strings.Contains(stdout.String(), `"action":"decline"`) {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "prompt denied") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

// TestNoPromptParallel checks each result against the fixture, independently
// of the other requests.
func TestNoPromptParallel(t *testing.T) {
	const count = 50
	type result struct {
		out, errout string
		code        int
		err         string
	}
	base := t.TempDir()
	run := func(i int) result {
		dir := filepath.Join(base, fmt.Sprint(i))
		var out, errout bytes.Buffer
		input := `{"hook_event_name":"Elicitation"}`
		if i%2 == 0 {
			input = `{"hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"echo ok"}}`
		}
		req := hookctx.New(strings.NewReader(input), &out, &errout, []string{"HOME=" + dir, "XDG_CONFIG_HOME=" + filepath.Join(dir, "config"), "CLAUDE_CONFIG_DIR=" + filepath.Join(dir, "claude"), fmt.Sprintf("CLAUDE_PID=%d", i+1000)}, dir, i+2000, i+3000)
		code, err := runHook(hookctx.WithRequest(context.Background(), req), hookNoPrompt)
		res := result{out: out.String(), errout: errout.String(), code: code}
		if err != nil {
			res.err = err.Error()
		}
		return res
	}
	for i := range count {
		dir := filepath.Join(base, fmt.Sprint(i), "config", "chooks")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		level := "error"
		if i%3 == 0 {
			level = "invalid"
		}
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("logger:\n  level: "+level+"\n  output: stderr\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var got [count]result
	var wg sync.WaitGroup
	for i := range got {
		wg.Go(func() { got[i] = run(i) })
	}
	wg.Wait()
	for i, result := range got {
		if i%3 == 0 {
			if result.code != exitError || !strings.Contains(result.err, "invalid logger level") || result.out != "" {
				t.Errorf("request %d: %+v", i, result)
			}
			continue
		}
		expected := "{\"hookSpecificOutput\":{\"action\":\"decline\",\"hookEventName\":\"Elicitation\"}}\n"
		if i%2 == 0 {
			payload, err := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{
				"hookEventName": "PermissionRequest",
				"decision":      map[string]any{"behavior": "deny", "interrupt": false, "message": permissionDeniedMessage},
			}})
			if err != nil {
				t.Fatal(err)
			}
			expected = string(payload) + "\n"
		}
		if result.code != exitAllowed || result.err != "" || result.errout != "" || result.out != expected {
			t.Errorf("request %d: %+v", i, result)
		}
	}
}

func TestPromptRequestRuntimeDir(t *testing.T) {
	root := t.TempDir()
	for i := range 2 {
		dir := filepath.Join(root, fmt.Sprint(i))
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		request := hookctx.New(strings.NewReader(`{"session_id":"shared"}`), &stdout, &stderr, []string{"HOME=" + dir, "XDG_RUNTIME_DIR=" + dir}, dir, 123+i, 45)
		ctx, procCalls := withFixtureProc(hookctx.WithRequest(context.Background(), request))
		code, err := runHook(ctx, hookUserPromptSubmit)
		if err != nil || code != exitAllowed {
			t.Fatalf("request %d: code=%d err=%v", i, code, err)
		}
		if procCalls.Load() == 0 {
			t.Fatalf("request %d: проверка модели claude не шла по дереву процессов запроса", i)
		}
		state, err := os.ReadFile(filepath.Join(dir, "chooks", "sessions", "shared"))
		if err != nil || string(state) != "working" {
			t.Fatalf("request %d: state=%q err=%v", i, state, err)
		}
	}
}

func TestPromptParallel(t *testing.T) {
	const count = 50
	root := t.TempDir()
	type outcome struct {
		code           int
		output, errors string
		procCalls      int64
	}
	run := func(i int) outcome {
		dir := filepath.Join(root, fmt.Sprint(i))
		var out, stderr bytes.Buffer
		req := hookctx.New(strings.NewReader(`{"session_id":"shared"}`), &out, &stderr, []string{"HOME=" + dir, "XDG_RUNTIME_DIR=" + dir, "CLAUDE_CONFIG_DIR=" + dir, fmt.Sprintf("CLAUDE_PID=%d", 1000+i)}, dir, 2000+i, 3000+i)
		ctx, procCalls := withFixtureProc(hookctx.WithRequest(context.Background(), req))
		code, err := runHook(ctx, hookUserPromptSubmit)
		errText := stderr.String()
		if err != nil {
			errText += err.Error()
		}
		return outcome{code, out.String(), errText, procCalls.Load()}
	}
	for i := range count {
		dir := filepath.Join(root, fmt.Sprint(i), ".config", "chooks")
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("logger:\n  level: error\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var got [count]outcome
	var wg sync.WaitGroup
	for i := range got {
		wg.Go(func() { got[i] = run(i) })
	}
	wg.Wait()
	for i, result := range got {
		if result.code != exitAllowed || result.output != "" || result.errors != "" || result.procCalls == 0 {
			t.Errorf("request %d: %+v", i, result)
		}
		state, err := os.ReadFile(filepath.Join(root, fmt.Sprint(i), "chooks", "sessions", "shared"))
		if err != nil || string(state) != "working" {
			t.Errorf("request %d: state=%q err=%v", i, state, err)
		}
	}
}

func TestUnderstandRequestSessionName(t *testing.T) {
	root := t.TempDir()
	for i, name := range []string{"first", "second"} {
		home := filepath.Join(root, fmt.Sprint(i))
		dir := filepath.Join(home, ".claude", "sessions")
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		data := fmt.Appendf(nil, `{"pid":123,"name":%q}`, name)
		if err := os.WriteFile(filepath.Join(dir, "123.json"), data, 0600); err != nil {
			t.Fatal(err)
		}
		req := hookctx.New(nil, nil, nil, []string{"HOME=" + home, "CLAUDE_PID=123"}, home, 456, 123)
		got := understandCall(hookctx.WithRequest(context.Background(), req), &core.ToolInput{}, &captureLogger{})
		if got.Name != name {
			t.Errorf("request %d: name %q, want %q", i, got.Name, name)
		}
	}
}

func TestUnderstandRequestPaths(t *testing.T) {
	root := t.TempDir()
	for i := range 2 {
		dir := filepath.Join(root, fmt.Sprint(i))
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		approved := filepath.Join(dir, "approved")
		if err := os.WriteFile(approved, []byte("opus claude-opus-5-5\n"), 0600); err != nil {
			t.Fatal(err)
		}
		req := hookctx.New(nil, nil, nil, []string{"HOME=" + dir, "XDG_RUNTIME_DIR=" + dir, "XDG_STATE_HOME=" + dir, "MODELS_APPROVED=" + approved}, dir, 100+i, 50+i)
		hook, err := newUnderstandHook(hookctx.WithRequest(context.Background(), req), &captureLogger{})
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(dir, "chooks", "understand-check.jsonl")
		if hook.JournalPath != want {
			t.Errorf("request %d: journal=%q want=%q", i, hook.JournalPath, want)
		}
	}
}

func TestStopRequestRuntimeDir(t *testing.T) {
	root := t.TempDir()
	config := &core.Config{Tools: map[string]core.ToolConfig{"notifier": {Enabled: false}}}
	for i := range 2 {
		dir := filepath.Join(root, fmt.Sprint(i))
		req := hookctx.New(nil, nil, nil, []string{"HOME=" + dir, "XDG_RUNTIME_DIR=" + dir}, dir, 100+i, 50+i)
		decision := announceSessionEvent(core.EventStop, &core.ToolInput{SessionID: "shared"}, config, &captureLogger{}, &req)
		if decision.State != core.StateDone {
			t.Fatalf("request %d: decision %+v", i, decision)
		}
		state, err := core.LoadSessionStateIn(req.RuntimeDir(), "shared")
		if err != nil || state != core.StateDone {
			t.Fatalf("request %d: state=%q err=%v", i, state, err)
		}
	}
}

func TestNightHintRequestState(t *testing.T) {
	root := t.TempDir()
	for i := range 2 {
		dir := filepath.Join(root, fmt.Sprint(i))
		req := hookctx.New(nil, nil, nil, []string{"HOME=" + dir, "XDG_STATE_HOME=" + dir}, dir, 100+i, 50+i)
		deps, err := nightHintDepsForRequest(core.DefaultConfig(), time.Now().Add(time.Second), req)
		if err != nil {
			t.Fatal(err)
		}
		first, err := deps.Store.MarkNight("shared", "2026-11-21")
		if err != nil || !first {
			t.Fatalf("request %d: first=%v err=%v", i, first, err)
		}
		second, err := deps.Store.MarkNight("shared", "2026-11-21")
		if err != nil || second {
			t.Fatalf("request %d: second=%v err=%v", i, second, err)
		}
	}
}

func TestWideSearchRequestHome(t *testing.T) {
	input := &core.ToolInput{ToolName: "Bash", CWD: "/work", ToolInput: []byte(`{"command":"grep -r x /home/alice/.cache"}`)}
	config := &core.Config{Tools: map[string]core.ToolConfig{}}
	for _, tc := range []struct {
		home string
		want bool
	}{{"/home/alice", true}, {"/home/bob", false}} {
		req := hookctx.New(nil, nil, nil, []string{"HOME=" + tc.home}, "/work", 1, 2)
		hint, err := wideSearchContext(hookctx.WithRequest(context.Background(), req), input, config, &captureLogger{})
		if err != nil || (hint != "") != tc.want {
			t.Errorf("home=%s hint=%q err=%v", tc.home, hint, err)
		}
	}
}

func TestExecuteHookStatusline(t *testing.T) {
	dir := t.TempDir()
	var out, stderr bytes.Buffer
	input := strings.NewReader(`{"session_id":"test","cwd":"/work/demo"}`)
	req := hookctx.New(input, &out, &stderr, []string{"HOME=" + dir, "XDG_RUNTIME_DIR=" + dir, "XDG_STATE_HOME=" + dir, "CLAUDE_CONFIG_DIR=" + dir}, dir, 123, 45)
	if code := executeHook(context.Background(), req, "statusline"); code != exitAllowed {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if out.Len() == 0 {
		t.Fatal("statusline output is empty")
	}
}

func TestPreToolRequestRelativeCWD(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "work")
	if err := os.Mkdir(work, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "body.json"), []byte(`{"body":"готово — сейчас"}`), 0600); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(config, []byte("logger:\n  level: error\ntools:\n  jira_style: {enabled: true}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	input := `{"tool_name":"Bash","cwd":"work","tool_input":{"command":"curl -X POST -d @body.json https://x.atlassian.net/rest/api/3/issue/PROJ-1/comment"}}`
	var stdout, stderr bytes.Buffer
	req := hookctx.New(strings.NewReader(input), &stdout, &stderr, []string{"HOME=" + root, "XDG_RUNTIME_DIR=" + root}, root, 123, 45)
	req.ConfigPath = config
	if code := executeHook(context.Background(), req, hookPreToolUse); code != exitBlocked {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

// TestHookCommandsParallel checks all hook entry points against fixed fixture
// expectations. Notifications, external fetches and the judge are disabled.
func TestHookCommandsParallel(t *testing.T) {
	const count = 50
	commands := []string{hookPreToolUse, hookPostToolBatch, hookUserPromptSubmit, hookUnderstandCheck, hookUnsentCheck, hookStop, hookStopFailure, hookNotification, hookNightHint, hookNoPrompt, hookWebFetchFallback, "statusline"}
	root := t.TempDir()
	configText := "logger:\n  level: error\ntools:\n  notifier: {enabled: false}\n  skill_reload: {enabled: false}\n  night_hint: {enabled: false}\n  webfetch_fallback: {enabled: false}\n  understand_check: {enabled: false}\n"
	for i := range count {
		dir := filepath.Join(root, fmt.Sprint(i))
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(configText), 0600); err != nil {
			t.Fatal(err)
		}
	}
	type outcome struct {
		code                 int
		out, stderr, failure string
		procCalls            int64
	}
	run := func(i int) outcome {
		dir := filepath.Join(root, fmt.Sprint(i))
		if err := core.SaveSessionStateIn(filepath.Join(dir, "run", "chooks"), "shared", core.StateWorking); err != nil {
			return outcome{failure: err.Error()}
		}
		name := commands[i%len(commands)]
		input := `{"session_id":"shared"}`
		switch name {
		case hookPreToolUse:
			input = `{"session_id":"shared","tool_name":"Bash","tool_input":{"command":"git status"}}`
		case hookNoPrompt:
			input = `{"session_id":"shared","hook_event_name":"Elicitation"}`
		case hookNotification:
			input = `{"session_id":"shared","notification_type":"permission_prompt"}`
		}
		var stdout, stderr bytes.Buffer
		req := hookctx.New(strings.NewReader(input), &stdout, &stderr, []string{"HOME=" + dir, "XDG_RUNTIME_DIR=" + filepath.Join(dir, "run"), "XDG_STATE_HOME=" + filepath.Join(dir, "state"), "CLAUDE_CONFIG_DIR=" + filepath.Join(dir, "claude"), fmt.Sprintf("CLAUDE_PID=%d", 2000000+i)}, dir, 3000000+i, 4000000+i)
		req.ConfigPath = filepath.Join(dir, "config.yaml")
		ctx, procCalls := withFixtureProc(context.Background())
		code := executeHook(ctx, req, name)
		return outcome{code: code, out: stdout.String(), stderr: stderr.String(), procCalls: procCalls.Load()}
	}
	var got [count]outcome
	var wg sync.WaitGroup
	for i := range got {
		wg.Go(func() { got[i] = run(i) })
	}
	wg.Wait()
	for i, result := range got {
		name := commands[i%len(commands)]
		if result.code != exitAllowed || result.failure != "" {
			t.Errorf("request %d (%s): %+v", i, name, result)
			continue
		}
		// pid запроса выдуман: проверка модели claude идёт по фикстуре, не по /proc
		if name == hookUserPromptSubmit && result.procCalls == 0 {
			t.Errorf("request %d (%s): проверка модели claude не шла по дереву процессов запроса", i, name)
		}
		switch name {
		case hookNoPrompt:
			if !strings.Contains(result.out, `"action":"decline"`) || result.stderr != "" {
				t.Errorf("request %d (%s): %+v", i, name, result)
			}
		case "statusline":
			if !strings.Contains(result.out, " ~ ") || !strings.Contains(result.stderr, "]0;🔄 ~") {
				t.Errorf("request %d (%s): %+v", i, name, result)
			}
		default:
			if result.out != "" || result.stderr != "" {
				t.Errorf("request %d (%s): %+v", i, name, result)
			}
		}
	}
}

func TestPreToolCommandsParallel(t *testing.T) {
	const count = 50
	root := t.TempDir()
	configText := "logger:\n  level: error\ntools:\n  kill_guard: {enabled: true}\n  rm_guard: {enabled: true}\n  model_guard: {enabled: true}\n  jira_style: {enabled: true}\n  wide_search_hint: {enabled: false}\n"
	for i := range count {
		dir := filepath.Join(root, fmt.Sprint(i))
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(configText), 0600); err != nil {
			t.Fatal(err)
		}
		model := "opus claude-opus-5-5\n"
		if i%2 == 0 {
			model = "sonnet claude-sonnet-5-5\n"
		}
		if err := os.WriteFile(filepath.Join(dir, "approved"), []byte(model), 0600); err != nil {
			t.Fatal(err)
		}
		body := `{"body":"готово"}`
		if i%2 == 0 {
			body = `{"body":"готово — сейчас"}`
		}
		if err := os.WriteFile(filepath.Join(dir, "body.json"), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	type outcome struct {
		code        int
		out, stderr string
	}
	run := func(i int) outcome {
		dir := filepath.Join(root, fmt.Sprint(i))
		var raw string
		switch i % 5 {
		case 0:
			raw = `{"tool_name":"Bash","command":"","tool_input":{"command":"git status"}}`
		case 1:
			raw = `{"tool_name":"Agent","tool_input":{"model":"sonnet"}}`
		case 2:
			raw = `{"tool_name":"Bash","tool_input":{"command":"kill -9 1"}}`
		case 3:
			raw = `{"tool_name":"Bash","tool_input":{"command":"false && rm -f $REQUEST_PREFIX/usr"}}`
		default:
			raw = `{"tool_name":"Bash","tool_input":{"command":"curl -X POST -d @body.json https://x.atlassian.net/rest/api/3/issue/PROJ-1/comment"}}`
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(raw), &event); err != nil {
			return outcome{code: -1, stderr: err.Error()}
		}
		event["cwd"] = dir
		data, err := json.Marshal(event)
		if err != nil {
			return outcome{code: -1, stderr: err.Error()}
		}
		var out, stderr bytes.Buffer
		env := []string{"HOME=.", "XDG_RUNTIME_DIR=run", "TMPDIR=tmp", "XDG_STATE_HOME=state", "XDG_CONFIG_HOME=config", "CLAUDE_CONFIG_DIR=claude", "MODELS_APPROVED=approved", "CLAUDE_CODE_TMPDIR=claude-tmp", fmt.Sprintf("CLAUDE_PID=%d", 2000000+i)}
		if i%2 == 0 {
			env = append(env, "REQUEST_PREFIX="+dir)
		}
		req := hookctx.New(bytes.NewReader(data), &out, &stderr, env, dir, 3000000+i, 4000000+i)
		req.ConfigPath = "config.yaml"
		code := executeHook(context.Background(), req, hookPreToolUse)
		return outcome{code: code, out: out.String(), stderr: stderr.String()}
	}
	var got [count]outcome
	var wg sync.WaitGroup
	for i := range got {
		wg.Go(func() { got[i] = run(i) })
	}
	wg.Wait()
	for i, result := range got {
		expected := exitAllowed
		switch i % 5 {
		case 1:
			if i%2 != 0 {
				expected = exitBlocked
			}
		case 2:
			expected = exitBlocked
		case 3:
			if i%2 != 0 {
				expected = exitBlocked
			}
		case 4:
			if i%2 == 0 {
				expected = exitBlocked
			}
		}
		if result.code != expected || result.out != "" || (expected == exitBlocked && result.stderr == "") || (expected == exitAllowed && result.stderr != "") {
			t.Errorf("request %d: %+v want code=%d", i, result, expected)
		}
	}
}

// Запрос в процессе с относительным или пустым cwd граница отклоняет до хука:
// Abs оставил бы путь относительным, и ОС открыла бы его от cwd исполнителя
func TestHookRequestRejectsRelativeCWD(t *testing.T) {
	for _, cwd := range []string{"relative", ""} {
		var out, stderr bytes.Buffer
		req := hookctx.New(strings.NewReader(`{}`), &out, &stderr, nil, cwd, 1, 2)
		if code := executeHook(context.Background(), req, hookNoPrompt); code != exitError || !strings.Contains(stderr.String(), "cwd must be absolute") || out.Len() != 0 {
			t.Errorf("cwd=%q: code=%d stdout=%q stderr=%q", cwd, code, out.String(), stderr.String())
		}
	}
}

// Relative paths must be resolved from each request, even when all requests
// arrive together. Expectations come from the fixture, not another run.
func TestRequestRelativeDirectoriesParallel(t *testing.T) {
	const count = 50
	root := t.TempDir()
	var pids [count]int
	for i := range pids {
		child := exec.Command("/bin/sleep", "600")
		child.Env = []string{}
		child.Dir = root
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		pids[i] = child.Process.Pid
		t.Cleanup(func() {
			_, _ = proc.StopChild(child.Process.Pid, 0, func() error { return child.Process.Kill() })
			_ = child.Wait()
		})
	}
	const config = "logger:\n  level: error\ntools:\n  jira_style: {enabled: true}\n  wide_search_hint: {enabled: false}\n"
	for i := range count {
		dir := filepath.Join(root, fmt.Sprint(i))
		if err := os.MkdirAll(filepath.Join(dir, "work"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(config), 0600); err != nil {
			t.Fatal(err)
		}
		body := `{"body":"готово"}`
		if i%2 == 0 {
			body = `{"body":"готово — сейчас"}`
		}
		if err := os.WriteFile(filepath.Join(dir, "work", "body.json"), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for i := range count {
		wg.Go(func() {
			dir := filepath.Join(root, fmt.Sprint(i))
			var out, stderr bytes.Buffer
			input := `{"cwd":"work","tool_name":"Bash","tool_input":{"command":"curl -X POST -d @body.json https://x.atlassian.net/rest/api/3/issue/PROJ-1/comment"}}`
			pid := pids[i]
			req := hookctx.New(strings.NewReader(input), &out, &stderr, []string{"HOME=" + dir, "CLAUDE_CONFIG_DIR=" + filepath.Join(dir, "claude"), "CLAUDE_PID=" + fmt.Sprint(pid), "XDG_RUNTIME_DIR=" + filepath.Join(dir, "run")}, dir, pid, os.Getpid())
			req.ConfigPath = filepath.Join(dir, "config.yaml")
			code := executeHook(context.Background(), req, hookPreToolUse)
			want := exitAllowed
			if i%2 == 0 {
				want = exitBlocked
			}
			if code != want || out.Len() != 0 || (want == exitBlocked && !strings.Contains(stderr.String(), "тире")) || (want == exitAllowed && stderr.Len() != 0) {
				t.Errorf("request %d: code=%d stdout=%q stderr=%q, want code=%d", i, code, out.String(), stderr.String(), want)
			}
		})
	}
	wg.Wait()
}

func TestHookWithoutRequestFails(t *testing.T) {
	if code, err := runHook(context.Background(), hookNoPrompt); code != exitError || err == nil {
		t.Fatalf("runHook without request: code=%d err=%v", code, err)
	}
	if code, err := runPreToolUse(context.Background(), nil, nil, nil); code != exitError || err == nil {
		t.Fatalf("runPreToolUse without request: code=%d err=%v", code, err)
	}
}
