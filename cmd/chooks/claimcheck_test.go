package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aiseeq/akit/proc"
	"github.com/aiseeq/chooks/internal/claimcheck"
	"github.com/aiseeq/chooks/internal/core"
	"github.com/aiseeq/chooks/internal/hookctx"
	"github.com/aiseeq/chooks/internal/questioncheck"
	"github.com/aiseeq/chooks/internal/understandcheck"
)

// claimEnv — каталоги запроса и заглушки codex и claude: ни одна модель не
// вызывается. Заглушки спят FAKE_*_DELAY секунд в дочернем sleep и пишут pid
// свой и sleep: по ним тест видит, что процессы не остались
type claimEnv struct {
	root, bin, pids string
	env             []string
}

const (
	claimAnswerFlag = `{"claims":[{"text":"тест прошёл","verdict":"unchecked","basis":"нет вызова go test","could_check":"go test ./..."}]}`
	understandFix   = `{"verdict":"fix","fixes":[{"quote":"напиши","action":"тест прошёл","why":"в реплике этого нет","fix":"убери"}]}`
)

func newClaimEnv(t *testing.T) claimEnv {
	t.Helper()
	root := t.TempDir()
	e := claimEnv{root: root, bin: filepath.Join(root, "bin"), pids: filepath.Join(root, "pids")}
	for _, dir := range []string{e.bin, e.pids, filepath.Join(root, "home")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	agent, _ := json.Marshal(map[string]any{"type": "item.completed", "item": map[string]any{"type": "agent_message", "text": claimAnswerFlag}})
	claude, _ := json.Marshal(map[string]any{"result": understandFix, "is_error": false, "usage": map[string]int{"input_tokens": 5, "output_tokens": 1}})
	scripts := map[string]string{
		"codex": `#!/bin/sh
echo $$ >> "$FAKE_PIDS/codex"
printf '%s\n' "CODEX_HOME=$CODEX_HOME $*" >> "$FAKE_PIDS/codex-args"
cat > /dev/null
if [ -n "$FAKE_CODEX_OUT_HOME" ] && [ "$CODEX_HOME" = "$FAKE_CODEX_OUT_HOME" ]; then
	echo '{"type":"turn.started"}'
	echo '{"type":"turn.failed","error":{"message":"Your workspace is out of credits. Ask your workspace owner to refill."}}'
	exit 1
fi
sleep "$FAKE_CODEX_DELAY" &
echo $! >> "$FAKE_PIDS/codex"
wait $!
cat <<'EOF'
` + string(agent) + `
{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":0,"output_tokens":10}}
EOF
`,
		"claude": `#!/bin/sh
echo $$ >> "$FAKE_PIDS/claude"
cat > /dev/null
sleep "$FAKE_CLAUDE_DELAY" &
echo $! >> "$FAKE_PIDS/claude"
wait $!
cat <<'EOF'
` + string(claude) + `
EOF
`,
	}
	for name, body := range scripts {
		if err := os.WriteFile(filepath.Join(e.bin, name), []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	approved := filepath.Join(root, "approved")
	if err := os.WriteFile(approved, []byte("opus claude-opus-5-5\nluna gpt-6-luna\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.env = []string{"HOME=" + filepath.Join(root, "home"), "PATH=" + e.bin + ":/usr/bin:/bin", "MODELS_APPROVED=" + approved,
		"XDG_RUNTIME_DIR=" + filepath.Join(root, "run"), "XDG_STATE_HOME=" + filepath.Join(root, "state"), "FAKE_PIDS=" + e.pids}
	return e
}

func (e claimEnv) request(stdout *bytes.Buffer, extra ...string) hookctx.Context {
	return hookctx.New(strings.NewReader(""), stdout, &bytes.Buffer{}, append(append([]string{}, e.env...), extra...), e.root, 100, 99)
}

// alive — pid ещё жив и не зомби
func alive(pid int) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	fields := strings.Fields(string(data[bytes.LastIndexByte(data, ')')+1:]))
	return len(fields) > 0 && fields[0] != "Z"
}

func (e claimEnv) assertNoProcesses(t *testing.T) {
	t.Helper()
	entries, _ := os.ReadDir(e.pids)
	for _, entry := range entries {
		if entry.Name() == "codex-args" {
			continue
		}
		f, err := os.Open(filepath.Join(e.pids, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			pid, err := strconv.Atoi(strings.TrimSpace(scanner.Text()))
			if err != nil {
				t.Fatalf("%s: pid line %q: %v", entry.Name(), scanner.Text(), err)
			}
			deadline := time.Now().Add(2 * time.Second)
			for alive(pid) && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
			}
			if alive(pid) {
				t.Errorf("%s process %d left running", entry.Name(), pid)
			}
		}
		f.Close()
	}
}

func readJournal(t *testing.T, path string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("journal: %v", err)
	}
	var out []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

const claimLetter = "git-admin, тест прошёл."

func letterTranscript(t *testing.T, dir string) string {
	t.Helper()
	human, _ := json.Marshal("напиши git-admin итог")
	call, _ := json.Marshal(map[string]any{"to": "git-admin", "message": claimLetter})
	lines := []string{
		`{"type":"user","message":{"role":"user","content":` + string(human) + `},"uuid":"u1","timestamp":"2026-11-23T00:00:00Z","origin":{"kind":"human"},"sessionId":"s"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"SendMessage","input":` + string(call) + `}]},"uuid":"a1","timestamp":"2026-11-23T00:00:05Z","sessionId":"s"}`,
	}
	path := filepath.Join(dir, "s.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Письмо: судьи понимания и claim-check идут параллельно через настоящие
// CLIJudge на заглушках codex и claude с задержками в единицах u. Сроки как в
// хуке: понимание 30u, claim-check 75u, таймаут хука 90u. Отказ одного судьи
// не теряется при сбое или таймауте другого, процессы не остаются
func TestLetterJudgesCombined(t *testing.T) {
	// u и задержки — с запасом на запуск заглушки под нагрузкой гейта: при
	// 40 мс и 25u против срока 30u (200 мс запаса) судья понимания падал
	// по сроку при load average 22
	const u = 80 * time.Millisecond
	cases := []struct {
		name                      string
		understand, claim         float64 // задержки в u
		wantUnderstand, wantClaim bool
		understandVerdict         string
		claimOutcome              string
	}{
		// 35 и 50 из приёмки: понимание выходит за свои 30u — таймаут, отказ
		// claim-check остаётся
		{"35/50", 35, 50, false, true, understandcheck.VerdictTimeout, claimcheck.OutcomeDeny},
		{"15/50 both deny", 15, 50, true, true, understandcheck.VerdictFix, claimcheck.OutcomeDeny},
		{"claim hangs", 15, 100, true, false, understandcheck.VerdictFix, claimcheck.OutcomeError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newClaimEnv(t)
			transcript := letterTranscript(t, e.root)
			secs := func(n float64) string { return strconv.FormatFloat((time.Duration(n) * u).Seconds(), 'f', 3, 64) }
			var stdout bytes.Buffer
			req := e.request(&stdout, "FAKE_CODEX_DELAY="+secs(tc.claim), "FAKE_CLAUDE_DELAY="+secs(tc.understand))
			deps := claimDeps{understandBudget: 30 * u, sendBudget: 75 * u, notify: func(string, string) error { return nil }}
			ctx := context.WithValue(hookctx.WithRequest(context.Background(), req), claimDepsKey{}, deps)
			config, logger := silentConfig(t)
			input, _ := json.Marshal(map[string]any{"session_id": "s1", "transcript_path": transcript, "tool_name": "SendMessage",
				"tool_use_id": "toolu_1", "tool_input": map[string]any{"to": "git-admin", "message": claimLetter}})
			started := time.Now()
			runUnderstandCheck(ctx, input, config, logger, &stdout)
			elapsed := time.Since(started)
			if elapsed >= 90*u {
				t.Errorf("hook took %s, over its 90u timeout %s", elapsed, 90*u)
			}
			var out preToolDenyOutput
			if err := json.Unmarshal(stdout.Bytes(), &out); err != nil || out.HookSpecificOutput.PermissionDecision != "deny" {
				t.Fatalf("no deny: %q %v", stdout.String(), err)
			}
			reason := out.HookSpecificOutput.PermissionDecisionReason
			if got := strings.Contains(reason, "Проверка понимания"); got != tc.wantUnderstand {
				t.Errorf("understand reason present=%v, want %v:\n%s", got, tc.wantUnderstand, reason)
			}
			if got := strings.Contains(reason, "claim-check") && strings.Contains(reason, "«тест прошёл» — unchecked: нет вызова go test"); got != tc.wantClaim {
				t.Errorf("claim reason present=%v, want %v:\n%s", got, tc.wantClaim, reason)
			}
			state := filepath.Join(e.root, "state", "chooks")
			if got := readJournal(t, filepath.Join(state, "understand-check.jsonl")); got[0]["verdict"] != tc.understandVerdict {
				t.Errorf("understand journal %v", got)
			}
			claim := readJournal(t, claimcheck.JournalPath(state))
			if claim[0]["outcome"] != tc.claimOutcome || claim[0]["event"] != "send" {
				t.Errorf("claim journal %v", claim)
			}
			args, err := os.ReadFile(filepath.Join(e.pids, "codex-args"))
			if err != nil || !strings.Contains(string(args), "CODEX_HOME="+filepath.Join(e.root, "home", ".codex")+" ") ||
				!strings.Contains(string(args), "--output-schema "+filepath.Join(e.root, "run", "chooks", "claimcheck", "verdict-schema.json")) ||
				!strings.Contains(string(args), `model_reasoning_effort="high"`) || !strings.Contains(string(args), "-m gpt-6-luna") {
				t.Errorf("codex args %q %v", args, err)
			}
			e.assertNoProcesses(t)
		})
	}
}

// Объединение решений: отказ любого судьи — отказ, причины обоих в одном
// ответе
func TestWriteCheckOutcomesKeepsEveryDenial(t *testing.T) {
	_, logger := silentConfig(t)
	input := &core.ToolInput{SessionID: "s", ToolName: "SendMessage"}
	cases := []struct {
		u    understandcheck.Outcome
		c    claimcheck.Outcome
		want []string
	}{
		{understandcheck.Outcome{Deny: true, Reason: "U"}, claimcheck.Outcome{Block: true, Reason: "C"}, []string{"U", "C"}},
		{understandcheck.Outcome{Verdict: "timeout"}, claimcheck.Outcome{Block: true, Reason: "C"}, []string{"C"}},
		{understandcheck.Outcome{Deny: true, Reason: "U"}, claimcheck.Outcome{Result: "error"}, []string{"U"}},
	}
	for _, tc := range cases {
		var out bytes.Buffer
		writeCheckOutcomes(tc.u, tc.c, input, logger, &out)
		var got preToolDenyOutput
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatalf("%+v: %q", tc, out.String())
		}
		if r := got.HookSpecificOutput.PermissionDecisionReason; r != strings.Join(tc.want, "\n\n") {
			t.Errorf("%+v: reason %q", tc, r)
		}
	}
	var out bytes.Buffer
	writeCheckOutcomes(understandcheck.Outcome{Verdict: "ok"}, claimcheck.Outcome{Result: "pass"}, input, logger, &out)
	if out.Len() != 0 {
		t.Fatalf("pass printed %q", out.String())
	}
}

type fixedAsker struct {
	answer string
	calls  *int
}

func (a fixedAsker) Model() string { return "stub/high" }
func (a fixedAsker) Ask(context.Context, string, string) (string, questioncheck.Usage, error) {
	*a.calls++
	return a.answer, questioncheck.Usage{}, nil
}

// claim-stop: флаг — JSON decision block с reason; stop_hook_active —
// выход без судьи; выключенный ключ — без судьи
func TestClaimStopHook(t *testing.T) {
	answer := "Готово: тест прошёл."
	for _, tc := range []struct {
		name      string
		active    bool
		disabled  bool
		wantBlock bool
		wantCalls int
	}{
		{"flag", false, false, true, 1},
		{"stop hook active", true, false, false, 0},
		{"disabled", false, true, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newClaimEnv(t)
			lines := []string{
				`{"type":"user","message":{"role":"user","content":"сделай"},"timestamp":"2026-11-23T00:00:00Z","origin":{"kind":"human"}}`,
				`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"` + answer + `"}]},"timestamp":"2026-11-23T00:00:05Z"}`,
			}
			transcript := filepath.Join(e.root, "t.jsonl")
			if err := os.WriteFile(transcript, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			var stdout bytes.Buffer
			calls := 0
			deps := claimDeps{judge: fixedAsker{answer: claimAnswerFlag, calls: &calls}, notify: func(string, string) error { return nil }}
			ctx := context.WithValue(hookctx.WithRequest(context.Background(), e.request(&stdout)), claimDepsKey{}, deps)
			config, logger := silentConfig(t)
			if tc.disabled {
				config.Tools[claimCheckTool] = core.ToolConfig{Enabled: false}
			}
			data, _ := json.Marshal(map[string]any{"session_id": "s1", "transcript_path": transcript, "hook_event_name": "Stop",
				"stop_hook_active": tc.active, "last_assistant_message": answer})
			runClaimStop(ctx, data, config, logger, &stdout)
			if calls != tc.wantCalls {
				t.Errorf("judge calls %d, want %d", calls, tc.wantCalls)
			}
			if !tc.wantBlock {
				if stdout.Len() != 0 {
					t.Fatalf("printed %q", stdout.String())
				}
				return
			}
			var out stopBlockOutput
			if err := json.Unmarshal(stdout.Bytes(), &out); err != nil || out.Decision != "block" ||
				!strings.Contains(out.Reason, "«тест прошёл» — unchecked: нет вызова go test") || !strings.Contains(out.Reason, "Чего проверить нечем") {
				t.Fatalf("block output %q %v", stdout.String(), err)
			}
		})
	}
}

// Учётки по умолчанию — ~/.codex, затем ~/.codex-personal; config.yaml их
// заменяет, ~/ — от HOME запроса
func TestClaimAccounts(t *testing.T) {
	req := hookctx.New(nil, nil, nil, []string{"HOME=/h"}, "/w", 1, 1)
	got, err := claimAccounts(&core.Config{Tools: map[string]core.ToolConfig{}}, req)
	if err != nil || strings.Join(got, ",") != "/h/.codex,/h/.codex-personal" {
		t.Fatalf("default %v %v", got, err)
	}
	config := &core.Config{Tools: map[string]core.ToolConfig{claimCheckTool: {Enabled: true, CodexHomes: []string{"~/.codex-personal", "/abs/c", "rel"}}}}
	got, err = claimAccounts(config, req)
	if err != nil || strings.Join(got, ",") != "/h/.codex-personal,/abs/c,/w/rel" {
		t.Fatalf("config %v %v", got, err)
	}
	marks, err := workerAccountMarks(hookctx.New(nil, nil, nil, []string{"HOME=/h", "WORKER_STATE=/ws"}, "/w", 1, 1))
	if err != nil || marks["/h/.codex"] != "/ws/accounts/work.json" || marks["/h/.codex-personal"] != "/ws/accounts/personal.json" {
		t.Fatalf("worker marks %v %v", marks, err)
	}
}

func writeStopTranscript(t *testing.T, dir, answer string) string {
	t.Helper()
	lines := []string{
		`{"type":"user","message":{"role":"user","content":"сделай"},"timestamp":"2026-11-23T00:00:00Z","origin":{"kind":"human"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"` + answer + `"}]},"timestamp":"2026-11-23T00:00:05Z"}`,
	}
	path := filepath.Join(dir, "t.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// codex выходит с ненулевым кодом, а отказ по кредитам — turn.failed в JSON
// stdout: учётка метится, вызов идёт на вторую (ревью Terra 5ec62fa, п. 4)
func TestClaimStopFailoverOnCreditsExit(t *testing.T) {
	e := newClaimEnv(t)
	answer := "Готово: тест прошёл."
	transcript := writeStopTranscript(t, e.root, answer)
	home := filepath.Join(e.root, "home")
	work, personal := filepath.Join(home, ".codex"), filepath.Join(home, ".codex-personal")
	var stdout bytes.Buffer
	req := e.request(&stdout, "FAKE_CODEX_DELAY=0", "FAKE_CODEX_OUT_HOME="+work)
	ctx := context.WithValue(hookctx.WithRequest(context.Background(), req), claimDepsKey{}, claimDeps{notify: func(string, string) error { return nil }})
	config, logger := silentConfig(t)
	data, _ := json.Marshal(map[string]any{"session_id": "s1", "transcript_path": transcript, "hook_event_name": "Stop", "last_assistant_message": answer})
	runClaimStop(ctx, data, config, logger, &stdout)
	var out stopBlockOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil || out.Decision != "block" {
		t.Fatalf("no block after failover: %q %v", stdout.String(), err)
	}
	args, _ := os.ReadFile(filepath.Join(e.pids, "codex-args"))
	if !strings.Contains(string(args), "CODEX_HOME="+work+" ") || !strings.Contains(string(args), "CODEX_HOME="+personal+" ") {
		t.Fatalf("codex calls %q", args)
	}
	marks, _ := filepath.Glob(filepath.Join(e.root, "state", "chooks", "claim-check", "accounts", "codex-*.json"))
	if len(marks) != 1 {
		t.Fatalf("account marks %v", marks)
	}
	journal := readJournal(t, claimcheck.JournalPath(filepath.Join(e.root, "state", "chooks")))
	if journal[0]["account"] != personal || journal[0]["outcome"] != claimcheck.OutcomeBlock {
		t.Fatalf("journal %v", journal)
	}
}

// SIGTERM CLI-хука (таймаут или отмена хода в Claude Code) не оставляет
// судью: заглушка codex жива до сигнала и мертва после выхода хука (ревью
// Terra 5ec62fa, п. 1)
func TestClaimStopSIGTERMKillsJudge(t *testing.T) {
	e := newClaimEnv(t)
	answer := "Готово: тест прошёл."
	transcript := writeStopTranscript(t, e.root, answer)
	if err := os.MkdirAll(filepath.Join(e.root, "tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]any{"session_id": "s1", "transcript_path": transcript, "hook_event_name": "Stop", "last_assistant_message": answer})
	cmd := exec.Command(testBinary(t), hookClaimStop)
	cmd.Env = append(append([]string{}, e.env...), "FAKE_CODEX_DELAY=60", argvCLIHelper+"=1", "TMPDIR="+filepath.Join(e.root, "tmp"),
		"XDG_CONFIG_HOME="+filepath.Join(e.root, "config"), "DBUS_SESSION_BUS_ADDRESS=unix:path="+filepath.Join(e.root, "no-bus"))
	cmd.Dir = e.root
	cmd.Stdin = bytes.NewReader(data)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pidsPath := filepath.Join(e.pids, "codex")
	var pids []int
	deadline := time.Now().Add(20 * time.Second)
	for len(pids) < 2 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		pids = nil
		raw, _ := os.ReadFile(pidsPath)
		for line := range strings.FieldsSeq(string(raw)) {
			if pid, err := strconv.Atoi(line); err == nil {
				pids = append(pids, pid)
			}
		}
	}
	if len(pids) < 2 {
		_, _ = proc.StopChild(cmd.Process.Pid, 0, func() error { return cmd.Process.Kill() })
		_ = cmd.Wait()
		t.Fatalf("codex stub not started: %v, stderr %s", pids, stderr.String())
	}
	for _, pid := range pids {
		if !alive(pid) {
			t.Fatalf("codex process %d not alive before the signal", pid)
		}
	}
	if _, err := proc.StopChild(cmd.Process.Pid, 2*time.Second, func() error { return cmd.Process.Signal(syscall.SIGTERM) }); err != nil {
		t.Fatal(err)
	}
	err := cmd.Wait()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != killedExitCode {
		t.Errorf("hook exit %v, want %d", err, killedExitCode)
	}
	left := 0
	for _, pid := range pids {
		deadline := time.Now().Add(2 * time.Second)
		for alive(pid) && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if alive(pid) {
			left++
			_, _ = proc.StopChild(pid, 0, func() error { return syscall.Kill(pid, syscall.SIGKILL) })
		}
	}
	if left > 0 {
		t.Fatalf("%d codex processes left after hook SIGTERM", left)
	}
}
