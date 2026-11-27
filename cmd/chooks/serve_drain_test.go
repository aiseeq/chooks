package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aiseeq/akit/proc"
	"github.com/aiseeq/chooks/internal/core"
	"github.com/aiseeq/chooks/internal/fallbackwatch"
	"github.com/aiseeq/chooks/internal/hookproxy"
	"github.com/aiseeq/chooks/internal/hookwire"
)

// plainCLI — исход вызова отдельным CLI без демона: эталон для вызовов,
// ушедших в CLI
func plainCLI(t *testing.T, hook, stdin string) proxyRun {
	t.Helper()
	home := t.TempDir()
	writeFiles(t, home, map[string]string{".config/chooks/config.yaml": serveConfig})
	return runBinary(t, argvCLIHelper, []string{hook}, absoluteEnv(home), home, []byte(stdin))
}

// fallbackEvents — вызовы в счёте уходов в CLI (fallbacks.json)
func fallbackEvents(t *testing.T, home string) []fallbackwatch.Event {
	t.Helper()
	text := readFile(t, filepath.Join(home, "state", "chooks", fallbackwatch.FileName))
	if text == "" {
		return nil
	}
	var state struct {
		Events []fallbackwatch.Event `json:"events"`
	}
	if err := json.Unmarshal([]byte(text), &state); err != nil {
		t.Fatalf("%s: %v", fallbackwatch.FileName, err)
	}
	return state.Events
}

// holdUntil — задержка заглушки webtext: ждать файла name в $HOME не
// дольше 20 с. Долгий запрос идёт, пока тест не отпустит его
func holdUntil(name string) string {
	return `0; i=0; while [ ! -e "$HOME/` + name + `" ] && [ $i -lt 400 ]; do sleep 0.05; i=$((i+1)); done`
}

// nightTestTZ chooses a fixed-offset zone whose current request hour is 22,
// so night-hint exercises the night path regardless of the host timezone.
func nightTestTZ() string {
	offset := 22 - time.Now().UTC().Hour()
	if offset < -12 {
		offset += 24
	}
	if offset > 14 {
		offset -= 24
	}
	if offset < 0 {
		return "Etc/GMT+" + strconv.Itoa(-offset)
	}
	return "Etc/GMT-" + strconv.Itoa(offset)
}

// Ревью 8f10b59, п. 2: install во время долгого запроса. v1 исполняет
// webfetch-fallback, файл заменён на v2, за это время 12 вызовов
// pre-tool-use. v1 принимает их и сразу отвечает «в CLI: upgrade»: ни одного
// ухода по «ack: timed out», каждый вызов обслужен с исходом отдельного CLI,
// в счёт уходов не идёт ни один и уведомления нет (порог 10). Долгий запрос
// исполнен v1 раз; после его конца следующий вызов исполняет v2. Под
// нагрузкой машины ответ «upgrade» бывает позже окна «принято» (100 мс):
// если v1 ответил на все 12, но прокси ушёл по «ack:» раньше, сценарий
// повторяется, не больше трёх раз
func TestServeUpgradeDuringLongRequest(t *testing.T) {
	base := plainCLI(t, hookPreToolUse, preToolUseLS)
	for attempt := 1; ; attempt++ {
		late := upgradeDuringLongRequest(t, base)
		if len(late) == 0 {
			return
		}
		if attempt == 3 {
			t.Fatalf("attempt %d: v1 answered \"upgrade\" after the ack window: %q", attempt, late)
		}
		t.Logf("attempt %d: v1 answered \"upgrade\" after the ack window (load): %q", attempt, late)
	}
}

// upgradeDuringLongRequest — один прогон сценария; возвращает уходы по
// «ack:», на которые v1 всё же ответил «upgrade» (опоздание под нагрузкой).
// Прочие расхождения — ошибки теста
func upgradeDuringLongRequest(t *testing.T, base proxyRun) []string {
	t.Helper()
	home, work := webFetchHome(t, holdUntil("release-long"))
	t.Cleanup(func() { writeFiles(t, home, map[string]string{"release-long": ""}) })
	exe := filepath.Join(home, "bin", "chooks")
	installVersion(t, exe, "v1")
	sv := startSupervised(t, exe, home)
	if !sv.waitLog(t, 20*time.Second, `msg="daemon: serving"`, "version=v1") {
		t.Fatal("v1 not serving")
	}
	runs := filepath.Join(home, "cli-runs")
	alerts := filepath.Join(home, "alerts")
	env := append(proxyEnv(t, absoluteEnv(home), sv.socket, runs), alertLogEnv+"="+alerts)
	long := make(chan proxyRun, 1)
	go func() {
		long <- runBinary(t, proxyHelper, []string{hookWebFetchFallback}, env, work, []byte(webFetchForbidden))
	}()
	if !waitFor(t, 20*time.Second, func() bool { return len(lines(readFile(t, filepath.Join(home, "effects")))) == 1 }) {
		t.Fatal("long request not started")
	}
	installVersion(t, exe, "v2")
	for i := 1; i <= 12; i++ {
		r := runBinary(t, proxyHelper, []string{hookPreToolUse}, env, home, []byte(preToolUseLS))
		if r.code != base.code || r.stdout != base.stdout || r.stderr != base.stderr {
			t.Errorf("call %d: code %d stdout %q stderr %q, plain CLI code %d stdout %q stderr %q",
				i, r.code, r.stdout, r.stderr, base.code, base.stdout, base.stderr)
		}
	}
	reasons := lines(readFile(t, runs))
	refused := strings.Count(readFile(t, sv.log), `msg="daemon: request left to CLI" hook="" reason=upgrade`)
	var late []string
	for _, reason := range reasons {
		switch {
		case reason == "upgrade":
		case strings.HasPrefix(reason, "ack: ") && refused == 12:
			late = append(late, reason)
		default:
			t.Errorf("fallback %q during the update, want only \"upgrade\"; all: %q; v1 refused %d", reason, reasons, refused)
		}
	}
	if len(reasons) != 12 || refused != 12 {
		t.Errorf("%d of 12 calls left to CLI (%q), v1 refused %d with \"upgrade\"", len(reasons), reasons, refused)
	}
	writeFiles(t, home, map[string]string{"release-long": ""})
	if r := <-long; r.code != exitAllowed {
		t.Errorf("long request: code %d stderr %q", r.code, r.stderr)
	}
	if got := servedBy(readFile(t, sv.log), hookWebFetchFallback); len(got) != 1 || got[0] != "v1" {
		t.Errorf("long request served by %v, want once by v1", got)
	}
	if effects := lines(readFile(t, filepath.Join(home, "effects"))); len(effects) != 1 {
		t.Errorf("%d webtext effects, want 1", len(effects))
	}
	if events := fallbackEvents(t, home); len(events) != len(late) {
		t.Errorf("update counted as daemon unavailable: %+v", events)
	}
	if decided := strings.Count(readFile(t, sv.log), `msg="daemon fallback alert"`); decided != 0 {
		t.Errorf("%d alert decisions, want 0", decided)
	}
	if !waitFor(t, 20*time.Second, func() bool {
		_ = runBinary(t, proxyHelper, []string{hookPreToolUse}, env, home, []byte(preToolUseLS))
		got := servedBy(readFile(t, sv.log), hookPreToolUse)
		return len(got) > 0 && got[len(got)-1] == "v2"
	}) {
		t.Fatalf("v2 serves nothing after the long request; starts:\n%s", readFile(t, sv.starts))
	}
	if got := lines(readFile(t, alerts)); len(got) != 0 {
		t.Errorf("alerts %q, want none", got)
	}
	return late
}

// Ревью 8f10b59, п. 3: демон упал посреди запроса (SIGKILL после 'C') —
// вызов не повторяется (эффект webtext один), но попадает в счёт уходов
// с причиной «lost: …» и в chooks-hooks.tsv строкой «lost» с исполнителем
// daemon; код 1 и строка о обрыве в stderr
func TestProxyLostAfterCommitIsRecorded(t *testing.T) {
	home, work := webFetchHome(t, holdUntil("release-long"))
	t.Cleanup(func() { writeFiles(t, home, map[string]string{"release-long": ""}) })
	exe := filepath.Join(home, "bin", "chooks")
	installVersion(t, exe, "v1")
	sv := startSupervised(t, exe, home)
	if !sv.waitLog(t, 20*time.Second, `msg="daemon: serving"`, "version=v1") {
		t.Fatal("v1 not serving")
	}
	runs := filepath.Join(home, "cli-runs")
	env := proxyEnv(t, absoluteEnv(home), sv.socket, runs)
	result := make(chan proxyRun, 1)
	go func() {
		result <- runBinary(t, proxyHelper, []string{hookWebFetchFallback}, env, work, []byte(webFetchForbidden))
	}()
	if !waitFor(t, 20*time.Second, func() bool { return len(lines(readFile(t, filepath.Join(home, "effects")))) == 1 }) {
		t.Fatal("request not started")
	}
	// Падение демона: SIGKILL проверенному потомку теста
	pid := servedPID(t, sv.starts, "v1")
	if err := proc.SignalDescendant(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	r := <-result
	if r.code != exitError || r.stdout != "" || !strings.Contains(r.stderr, "после «принято», хук не повторён") {
		t.Fatalf("code %d stdout %q stderr %q", r.code, r.stdout, r.stderr)
	}
	if effects := lines(readFile(t, filepath.Join(home, "effects"))); len(effects) != 1 {
		t.Errorf("%d webtext effects, want 1: the hook was repeated", len(effects))
	}
	reasons := lines(readFile(t, runs))
	if len(reasons) != 1 || !hookproxy.Lost(reasons[0]) {
		t.Fatalf("fallback reasons %q, want one lost", reasons)
	}
	rows := durationLines(t, home)
	if len(rows) != 1 || len(rows[0]) != 8 || rows[0][1] != hookWebFetchFallback || rows[0][5] != statusLost ||
		rows[0][6] != executorDaemon || rows[0][7] != reasons[0] {
		t.Fatalf("tsv rows %q", rows)
	}
	if events := fallbackEvents(t, home); len(events) != 1 || events[0].Hook != hookWebFetchFallback || events[0].Reason != reasons[0] {
		t.Fatalf("fallback count %+v, want the lost call", events)
	}
}

// Обрыв после 'C': безопасные хуки повторяются в CLI; судьи требуют ручного
// повтора, а хуки с внешними эффектами сохраняют исход. Демон — заглушка.
func TestProxyLostDaemonHooks(t *testing.T) {
	tests := []struct {
		name, hook, input, transcript string
		wantStdout                    string
		code                          int
		retry, compareCLI             bool
		checkLog                      bool
		telemetry                     bool
		wantState                     core.SessionState
		preState                      core.SessionState
		nightLocked                   bool
		nightHint                     bool
		lostMessage                   string
	}{
		{name: "denied model", hook: hookPreToolUse, input: `{"session_id":"s","hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"claude -p --model haiku hi"}}`, code: exitBlocked, lostMessage: `модель "haiku" не одобрена`, retry: true, telemetry: true},
		{name: "allowed command", hook: hookPreToolUse, input: `{"session_id":"s","hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"git status"}}`, compareCLI: true, retry: true, telemetry: true},
		{name: "check interrupted", hook: hookPreToolUse, input: `not json`, code: exitBlocked, lostMessage: "проверка прервана, повтори команду", retry: true, checkLog: true, telemetry: true},
		{name: "no-prompt repeats", hook: hookNoPrompt, input: `{"hook_event_name":"Elicitation"}`, compareCLI: true, retry: true, telemetry: true},
		{name: "unsent-check repeats and blocks", hook: hookUnsentCheck,
			input:      `{"session_id":"s","transcript_path":"@TRANSCRIPT@","last_assistant_message":"Ещё не отправлено: ответ Оле."}`,
			transcript: "{\"type\":\"user\",\"uuid\":\"u1\",\"timestamp\":\"2026-10-29T10:00:00Z\",\"origin\":{\"kind\":\"human\"},\"message\":{\"content\":\"что с ответом?\"}}\n", wantStdout: `"decision":"block"`, compareCLI: true, retry: true, telemetry: true},
		{name: "webfetch is not repeated", hook: hookWebFetchFallback, input: `{}`, code: exitError, lostMessage: "после «принято», хук не повторён", telemetry: true},
		{name: "user-prompt-submit repeats", hook: hookUserPromptSubmit, input: `{"session_id":"s","hook_event_name":"UserPromptSubmit","prompt":"проверь изменения"}`, compareCLI: true, retry: true, telemetry: true, wantState: core.StateWorking, preState: core.StateWaiting},
		{name: "post-tool-batch repeats", hook: hookPostToolBatch, input: `{"session_id":"s","hook_event_name":"PostToolBatch","transcript_path":"@TRANSCRIPT@"}`, compareCLI: true, retry: true, telemetry: true, wantState: core.StateWorking, preState: core.StateWaiting},
		{name: "stop repeats", hook: hookStop, input: `{"session_id":"s","hook_event_name":"Stop","background_tasks":[],"session_crons":[]}`, compareCLI: true, retry: true, telemetry: true, wantState: core.StateDone},
		{name: "stop-failure repeats", hook: hookStopFailure, input: `{"session_id":"s","hook_event_name":"StopFailure","error":"rate_limit"}`, compareCLI: true, retry: true, telemetry: true, wantState: core.StateError},
		{name: "notification repeats", hook: hookNotification, input: `{"session_id":"s","hook_event_name":"Notification","notification_type":"permission_prompt","message":"нужен ответ"}`, compareCLI: true, retry: true, telemetry: true, wantState: core.StateWaiting},
		{name: "understand-check requires command retry", hook: hookUnderstandCheck, input: `{}`, code: exitBlocked, lostMessage: "проверка прервана, повтори команду", telemetry: true},
		{name: "claim-stop requires stop retry", hook: hookClaimStop, input: `{}`, code: exitBlocked, lostMessage: "судья утверждений прерван обрывом демона: перепроверь свои утверждения в ответе", telemetry: true},
		{name: "night-hint repeats", hook: hookNightHint, input: `{"session_id":"s","hook_event_name":"UserPromptSubmit","prompt":"проверь подсказку"}`, wantStdout: "additionalContext", compareCLI: true, retry: true, telemetry: true, nightLocked: true, nightHint: true},
		{name: "statusline is not repeated", hook: hookStatusLine, input: `{}`, code: exitError, lostMessage: "после «принято», хук не повторён"},
	}
	covered := make(map[string]bool)
	for _, tt := range tests {
		covered[tt.hook] = true
		t.Run(tt.name, func(t *testing.T) {
			nightTZ := nightTestTZ()
			home := t.TempDir()
			writeFiles(t, home, map[string]string{
				".config/chooks/config.yaml": serveConfig,
				".config/models/approved":    "opus claude-opus-5-5\nsonnet claude-sonnet-5-5\n",
			})
			if tt.nightHint {
				configPath := filepath.Join(home, ".config/chooks/config.yaml")
				config, err := os.ReadFile(configPath)
				if err != nil {
					t.Fatal(err)
				}
				config = []byte(strings.Replace(string(config), "night_hint: {enabled: false}", "night_hint: {enabled: true}", 1))
				if err := os.WriteFile(configPath, config, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			input := strings.ReplaceAll(tt.input, "@TRANSCRIPT@", filepath.Join(home, "transcript.jsonl"))
			if tt.preState != "" {
				if err := core.SaveSessionStateIn(filepath.Join(home, "run", "chooks"), "s", tt.preState); err != nil {
					t.Fatal(err)
				}
			}
			if tt.transcript != "" {
				if err := os.WriteFile(filepath.Join(home, "transcript.jsonl"), []byte(tt.transcript), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			socket := filepath.Join(socketDir(t), "drop.sock")
			listener, err := listenSocket(socket)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = listener.Close() })
			go func() {
				for {
					conn, err := listener.AcceptUnix()
					if err != nil {
						return
					}
					_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
					buf := make([]byte, 64<<10)
					_, _ = conn.Read(buf)
					_, _ = conn.Write([]byte{hookwire.Accepted})
					_, _ = conn.Read(buf[:1])
					_ = conn.Close()
				}
			}()
			runs := filepath.Join(home, "cli-runs")
			env := proxyEnv(t, absoluteEnv(home), socket, runs)
			if tt.nightLocked {
				env = append(env, "CHOOKS_TEST_NIGHT_LOCKED=1", "TZ="+nightTZ)
			}
			r := runBinary(t, proxyHelper, []string{tt.hook}, env, home, []byte(input))
			wantCode := tt.code
			if tt.compareCLI {
				baseHome := t.TempDir()
				writeFiles(t, baseHome, map[string]string{
					".config/chooks/config.yaml": serveConfig,
					".config/models/approved":    "opus claude-opus-5-5\nsonnet claude-sonnet-5-5\n",
				})
				if tt.nightHint {
					configPath := filepath.Join(baseHome, ".config/chooks/config.yaml")
					config, err := os.ReadFile(configPath)
					if err != nil {
						t.Fatal(err)
					}
					config = []byte(strings.Replace(string(config), "night_hint: {enabled: false}", "night_hint: {enabled: true}", 1))
					if err := os.WriteFile(configPath, config, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				baseEnv := absoluteEnv(baseHome)
				if tt.nightLocked {
					baseEnv = append(baseEnv, "CHOOKS_TEST_NIGHT_LOCKED=1", "TZ="+nightTZ)
				}
				base := runBinary(t, argvCLIHelper, []string{tt.hook}, baseEnv, baseHome, []byte(input))
				wantCode = base.code
				if r.stdout != base.stdout {
					t.Fatalf("stdout %q, want CLI retry output %q", r.stdout, base.stdout)
				}
			}
			if r.code != wantCode || (tt.lostMessage != "" && !strings.Contains(r.stderr, tt.lostMessage)) {
				t.Fatalf("code %d stdout %q stderr %q, want code %d containing %q", r.code, r.stdout, r.stderr, wantCode, tt.lostMessage)
			}
			if tt.wantStdout != "" && !strings.Contains(r.stdout, tt.wantStdout) {
				t.Fatalf("stdout %q, want %q", r.stdout, tt.wantStdout)
			}
			if tt.retry && strings.Contains(r.stderr, "хук не повторён") {
				t.Fatalf("retryable hook was not repeated: %q", r.stderr)
			}
			if !tt.retry && tt.hook != hookUnderstandCheck && tt.hook != hookClaimStop && !strings.Contains(r.stderr, "хук не повторён") {
				t.Fatalf("non-retryable hook missing lost message: %q", r.stderr)
			}
			reasons := lines(readFile(t, runs))
			if len(reasons) != 1 || !hookproxy.Lost(reasons[0]) {
				t.Fatalf("fallback reasons %q", reasons)
			}
			if tt.telemetry {
				rows := durationLines(t, home)
				wantStatus, wantExecutor := statusLost, executorDaemon
				if tt.retry {
					wantStatus, wantExecutor = statusLostRetried, executorCLI
				}
				if len(rows) != 1 || len(rows[0]) != 8 || rows[0][1] != tt.hook || rows[0][5] != wantStatus ||
					rows[0][6] != wantExecutor || rows[0][7] != reasons[0] {
					t.Fatalf("duration rows %q", rows)
				}
				if events := fallbackEvents(t, home); len(events) != 1 || events[0].Hook != tt.hook || events[0].Reason != reasons[0] {
					t.Fatalf("fallback count %+v", events)
				}
			}
			if tt.checkLog {
				log := readFile(t, filepath.Join(home, ".claude", "logs", "chooks.log"))
				if !strings.Contains(log, "pre-tool-use check failed after daemon connection loss") || !strings.Contains(log, "invalid character") {
					t.Fatalf("original check error missing from log: %s", log)
				}
			}
			if tt.wantState != "" {
				got, err := core.LoadSessionStateIn(filepath.Join(home, "run", "chooks"), "s")
				if err != nil || got != tt.wantState {
					t.Fatalf("session state = %q, %v; want %q", got, err, tt.wantState)
				}
			}
		})
	}
	for hook := range hookTimeouts {
		if !covered[hook] {
			t.Errorf("daemon hook %q has no lost-connection case", hook)
		}
	}
}

// Session events commit their new state before delivery. A daemon that dies
// after both effects must not cause the CLI retry to deliver a second alert.
func TestProxyLostSessionEventDoesNotDuplicateAlert(t *testing.T) {
	tests := []struct {
		name, hook, input string
		state             core.SessionState
	}{
		{name: "stop", hook: hookStop, input: `{"session_id":"s","hook_event_name":"Stop","background_tasks":[],"session_crons":[]}`, state: core.StateDone},
		{name: "notification", hook: hookNotification, input: `{"session_id":"s","hook_event_name":"Notification","notification_type":"permission_prompt","message":"нужен ответ"}`, state: core.StateWaiting},
		{name: "stop-failure", hook: hookStopFailure, input: `{"session_id":"s","hook_event_name":"StopFailure","error":"rate_limit"}`, state: core.StateError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			config := strings.Replace(serveConfig, "notifier: {enabled: false}", "notifier: {enabled: true, desktop: true, sound: true}", 1)
			writeFiles(t, home, map[string]string{".config/chooks/config.yaml": config})
			socket := filepath.Join(socketDir(t), "effect-then-drop.sock")
			listener, err := listenSocket(socket)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = listener.Close() })
			alerts := filepath.Join(home, "alerts")
			go func() {
				conn, err := listener.AcceptUnix()
				if err != nil {
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				buf := make([]byte, 64<<10)
				_, _ = conn.Read(buf)
				_, _ = conn.Write([]byte{hookwire.Accepted})
				_, _ = conn.Read(buf[:1])
				if err := core.SaveSessionStateIn(filepath.Join(home, "run", "chooks"), "s", tt.state); err != nil {
					t.Errorf("save daemon state: %v", err)
				}
				appendTestLine(alerts, "daemon notification")
			}()
			release, pidlog := filepath.Join(home, "release"), filepath.Join(home, "alert-pids")
			env := append(proxyEnv(t, absoluteEnv(home), socket, filepath.Join(home, "cli-runs")), alertLogEnv+"="+alerts, alertHoldEnv+"="+release, alertPIDLogEnv+"="+pidlog)
			r := runBinary(t, proxyHelper, []string{tt.hook}, env, home, []byte(tt.input))
			logPath := filepath.Join(home, ".claude", "logs", "chooks.log")
			if !waitFor(t, 5*time.Second, func() bool {
				log := readFile(t, logPath)
				return strings.Contains(log, `msg="alert muted"`) || strings.Contains(log, `msg="alert allowed"`)
			}) {
				t.Fatalf("CLI alert decision was not logged: %s", readFile(t, logPath))
			}
			// An allowed decision starts deliver-alert asynchronously. Wait for
			// its PID and exit signal before checking the notification journal.
			if strings.Contains(readFile(t, logPath), `msg="alert allowed"`) {
				if !waitFor(t, 5*time.Second, func() bool {
					data, err := os.ReadFile(pidlog)
					return err == nil && len(lines(string(data))) > 0
				}) {
					t.Fatalf("deliver-alert was allowed but did not start: %q", readFile(t, pidlog))
				}
				if err := os.WriteFile(release, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				if !waitFor(t, 5*time.Second, func() bool {
					data, err := os.ReadFile(pidlog)
					if err != nil {
						return false
					}
					for _, line := range lines(string(data)) {
						if value, ok := strings.CutPrefix(line, "pid "); ok {
							pid, _ := strconv.Atoi(value)
							if pid > 0 && !processExited(pid) {
								return false
							}
						}
					}
					return true
				}) {
					t.Fatalf("deliver-alert process did not exit: pid log %q", readFile(t, pidlog))
				}
			} else if err := os.WriteFile(release, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if r.code != exitAllowed || strings.Contains(r.stderr, "хук не повторён") {
				t.Fatalf("code %d stdout %q stderr %q, want successful CLI retry", r.code, r.stdout, r.stderr)
			}
			if got := lines(readFile(t, alerts)); len(got) != 1 || got[0] != "daemon notification" {
				t.Fatalf("notification calls %q, want exactly one", got)
			}
		})
	}
}

// If the daemon dies before saving event state, the CLI retry performs the
// transition and delivers exactly one alert. This proves the retry actually
// ran, rather than merely observing state written by the daemon.
func TestProxyLostSessionEventRetriesBeforeStateSaved(t *testing.T) {
	tests := []struct {
		name, hook, input string
		state             core.SessionState
	}{
		{name: "stop", hook: hookStop, input: `{"session_id":"s","hook_event_name":"Stop","background_tasks":[],"session_crons":[]}`, state: core.StateDone},
		{name: "notification", hook: hookNotification, input: `{"session_id":"s","hook_event_name":"Notification","notification_type":"permission_prompt","message":"нужен ответ"}`, state: core.StateWaiting},
		{name: "stop-failure", hook: hookStopFailure, input: `{"session_id":"s","hook_event_name":"StopFailure","error":"rate_limit"}`, state: core.StateError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			config := strings.Replace(serveConfig, "notifier: {enabled: false}", "notifier: {enabled: true, desktop: true, sound: true}", 1)
			writeFiles(t, home, map[string]string{".config/chooks/config.yaml": config})
			socket := filepath.Join(socketDir(t), "retry-before-state.sock")
			listener, err := listenSocket(socket)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = listener.Close() })
			go func() {
				conn, err := listener.AcceptUnix()
				if err != nil {
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				buf := make([]byte, 64<<10)
				_, _ = conn.Read(buf)
				_, _ = conn.Write([]byte{hookwire.Accepted})
				_, _ = conn.Read(buf[:1])
			}()
			alerts := filepath.Join(home, "alerts")
			release, pidlog := filepath.Join(home, "release"), filepath.Join(home, "alert-pids")
			env := append(proxyEnv(t, absoluteEnv(home), socket, filepath.Join(home, "cli-runs")), alertLogEnv+"="+alerts, alertHoldEnv+"="+release, alertPIDLogEnv+"="+pidlog)
			r := runBinary(t, proxyHelper, []string{tt.hook}, env, home, []byte(tt.input))
			if r.code != exitAllowed || strings.Contains(r.stderr, "хук не повторён") {
				t.Fatalf("code %d stdout %q stderr %q, want successful retry", r.code, r.stdout, r.stderr)
			}
			if !waitFor(t, 5*time.Second, func() bool { return len(lines(readFile(t, alerts))) == 1 }) {
				t.Fatalf("CLI retry did not deliver exactly one alert: %q", readFile(t, alerts))
			}
			if err := os.WriteFile(release, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if !waitFor(t, 5*time.Second, func() bool {
				data, err := os.ReadFile(pidlog)
				if err != nil {
					return false
				}
				var pids []int
				for _, line := range lines(string(data)) {
					if value, ok := strings.CutPrefix(line, "pid "); ok {
						pid, _ := strconv.Atoi(value)
						if pid > 0 {
							pids = append(pids, pid)
						}
					}
				}
				if len(pids) == 0 {
					return false
				}
				for _, pid := range pids {
					if !processExited(pid) {
						return false
					}
				}
				return true
			}) {
				t.Fatalf("deliver-alert process did not exit: %q", readFile(t, pidlog))
			}
			if got := lines(readFile(t, alerts)); len(got) != 1 {
				t.Fatalf("notification calls %q, want exactly one", got)
			}
			got, err := core.LoadSessionStateIn(filepath.Join(home, "run", "chooks"), "s")
			if err != nil || got != tt.state {
				t.Fatalf("session state = %q, %v; want %q", got, err, tt.state)
			}
		})
	}
}

// Ревью 8f10b59, п. 4: причина с табуляцией и переводами строк не рвёт
// столбцы chooks-hooks.tsv
func TestDurationsReasonKeepsColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), durationsFile)
	timer := &hookTimer{hook: hookStop, durations: path, now: time.Now}
	timer.setExecutor(executorCLI, "ack: a\tb\nc\rd")
	timer.recordDuration(time.Now(), "s1", 12*time.Millisecond, "ok")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rows := lines(string(data))
	if len(rows) != 1 {
		t.Fatalf("%d lines, want 1: %q", len(rows), data)
	}
	fields := strings.Split(rows[0], "\t")
	if len(fields) != 8 || fields[6] != executorCLI || fields[7] != "ack: a b c d" {
		t.Fatalf("fields %q", fields)
	}
}
