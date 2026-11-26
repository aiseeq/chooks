package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aiseeq/akit/proc"
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

// Обрыв после 'C': pre-tool-use повторяет проверку, остальные хуки сохраняют
// прежнее поведение. Демон — заглушка: «принято», 'C', закрытие
func TestProxyLostPreToolUse(t *testing.T) {
	tests := []struct {
		name, hook, input, stderr string
		code                      int
	}{
		{name: "denied model", hook: hookPreToolUse, input: `{"session_id":"s","hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"claude -p --model haiku hi"}}`, code: exitBlocked, stderr: `модель "haiku" не одобрена`},
		{name: "allowed command", hook: hookPreToolUse, input: `{"session_id":"s","hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"git status"}}`, code: exitAllowed},
		{name: "check interrupted", hook: hookPreToolUse, input: `not json`, code: exitBlocked, stderr: "проверка прервана, повтори команду"},
		{name: "stop is not repeated", hook: hookStop, input: `{}`, code: exitError, stderr: "после «принято», хук не повторён"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			writeFiles(t, home, map[string]string{
				".config/chooks/config.yaml": serveConfig,
				".config/models/approved":    "opus claude-opus-5-5\nsonnet claude-sonnet-5-5\n",
			})
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
			r := runBinary(t, proxyHelper, []string{tt.hook}, proxyEnv(t, absoluteEnv(home), socket, runs), home, []byte(tt.input))
			if r.code != tt.code || (tt.stderr != "" && !strings.Contains(r.stderr, tt.stderr)) {
				t.Fatalf("code %d stdout %q stderr %q, want code %d containing %q", r.code, r.stdout, r.stderr, tt.code, tt.stderr)
			}
			reasons := lines(readFile(t, runs))
			if len(reasons) != 1 || !hookproxy.Lost(reasons[0]) {
				t.Fatalf("fallback reasons %q", reasons)
			}
			if tt.hook == hookPreToolUse {
				rows := durationLines(t, home)
				if len(rows) != 1 || len(rows[0]) != 8 || rows[0][1] != tt.hook || rows[0][5] != statusLostRetried || rows[0][6] != executorCLI || rows[0][7] != reasons[0] {
					t.Fatalf("duration rows %q", rows)
				}
				if events := fallbackEvents(t, home); len(events) != 1 || events[0].Hook != tt.hook || events[0].Reason != reasons[0] {
					t.Fatalf("fallback count %+v", events)
				}
				if tt.name == "check interrupted" {
					log := readFile(t, filepath.Join(home, ".claude", "logs", "chooks.log"))
					if !strings.Contains(log, "pre-tool-use check failed after daemon connection loss") || !strings.Contains(log, "invalid character") {
						t.Fatalf("original check error missing from log: %s", log)
					}
				}
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
