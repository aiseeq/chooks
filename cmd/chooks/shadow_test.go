package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aiseeq/chooks/internal/desktop"
	"github.com/aiseeq/chooks/internal/hookctx"
	"github.com/aiseeq/chooks/internal/hookproxy"
	"github.com/aiseeq/chooks/internal/hookwire"
	"github.com/aiseeq/chooks/internal/nighthint"
	"github.com/aiseeq/chooks/internal/procfs"
	"github.com/aiseeq/chooks/internal/tools/notifier"
)

// countDeliveries считает исполненные доставки оповещения под тенью (CLI
// и демона). Deliver зовётся синхронно в работе хука, поэтому после возврата
// хука и теневого исполнения счёт окончателен: лишнее оповещение видно без
// паузы на фоновый процесс заглушки
func countDeliveries(t *testing.T) func() int64 {
	t.Helper()
	var n atomic.Int64
	orig := realNotifierEffects
	realNotifierEffects = func() notifier.Effects {
		effects := orig()
		deliver := effects.Deliver
		effects.Deliver = func(executable string, alert desktop.Alert, request hookctx.Context) error {
			n.Add(1)
			return deliver(executable, alert, request)
		}
		return effects
	}
	t.Cleanup(func() { realNotifierEffects = orig })
	return n.Load
}

// Тень, этап 2б-3а: противник 2б, сценарий 6 — один запрос с записью метки
// и управляемыми часами, независимые снимки состояния, заглушка уведомления

const shadowTestConfig = `logger:
  level: info
  output: file
  file: ~/log/chooks.log
daemon:
  shadow: %t
tools:
  notifier: {enabled: true, desktop: true, sound: false}
  skill_reload: {enabled: false}
  night_hint: {enabled: true}
  webfetch_fallback: {enabled: false}
  understand_check: {enabled: false}
  kill_guard: {enabled: true}
  wide_search_hint: {enabled: false}
`

// shadowAt — время запроса тестов: последняя миллисекунда суток, ночь
var shadowAt = time.Date(2026, 11, 22, 23, 59, 59, 999_000_000, time.FixedZone("CET", 3600))

// shadowHome — HOME с конфигом и состоянием сессии s6: «работает», фоновая
// задача t1 замечена за 3 ч до shadowAt (брошена к shadowAt)
func shadowHome(t *testing.T, shadow bool) string {
	t.Helper()
	home := t.TempDir()
	tasks := fmt.Sprintf(`{"tasks":[{"id":"t1","type":"shell","description":"old","since":%q}],"crons":0}`, shadowAt.Add(-3*time.Hour).Format(time.RFC3339Nano))
	writeFiles(t, home, map[string]string{
		".config/chooks/config.yaml":   fmt.Sprintf(shadowTestConfig, shadow),
		".config/models/approved":      "opus claude-opus-5-5\n",
		"run/chooks/sessions/s6":       "working",
		"run/chooks/sessions/s6.tasks": tasks,
		"work/.keep":                   "",
	})
	return home
}

type shadowCall struct {
	code           int
	stdout, stderr string
	elapsed        time.Duration
}

// runShadowCLI исполняет подкоманду как CLI (runHook с обработчиком выхода
// процесса) со временем запроса shadowAt и env с сокетом демона socket
func runShadowCLI(t *testing.T, home, hook, stdin, socket string, extra ...string) shadowCall {
	t.Helper()
	return runShadowCLIWith(t, home, hook, stdin, socket, nil, extra...)
}

// runShadowCLIWith — runShadowCLI с /proc запроса proc; nil — /proc машины
func runShadowCLIWith(t *testing.T, home, hook, stdin, socket string, proc procfs.FS, extra ...string) shadowCall {
	t.Helper()
	alerts := filepath.Join(home, "alerts")
	release, pidLog := shadowTempPath(t, home, "release-alert-"), shadowTempPath(t, home, "alert-pid-")
	env := append(absoluteEnv(home), hookproxy.EnvSocket+"="+socket, alertLogEnv+"="+alerts,
		"CLAUDE_PID="+strconv.Itoa(os.Getpid()), "TZ=Europe/Berlin")
	env = append(env, extra...)
	env = append(env, alertHoldEnv+"="+release, alertPIDLogEnv+"="+pidLog)
	t.Cleanup(func() {
		if err := os.WriteFile(release, nil, 0o600); err != nil {
			t.Errorf("release detached alert writer: %v", err)
		}
		if hook != hookNotification {
			return
		}

		var pid int
		startDeadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(startDeadline) {
			if data, err := os.ReadFile(pidLog); err == nil {
				pid = completeAlertWriterPID(string(data))
			}
			if pid != 0 {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if pid == 0 {
			t.Errorf("detached alert writer did not start; pid log %q", pidLog)
			return
		}

		exitDeadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(exitDeadline) {
			if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); os.IsNotExist(err) {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Errorf("detached alert writer pid %d did not exit", pid)
	})
	var stdout, stderr bytes.Buffer
	request := hookctx.New(strings.NewReader(stdin), &stdout, &stderr, env, filepath.Join(home, "work"), os.Getpid(), os.Getppid())
	request.Time = shadowAt
	request.Proc = proc
	ctx := context.WithValue(context.Background(), cliTerminationKey{}, (func(int))(func(int) {}))
	start := time.Now()
	code, err := runHook(hookctx.WithRequest(ctx, request), hook)
	elapsed := time.Since(start)
	if err != nil {
		fmt.Fprintf(&stderr, "chooks: %v\n", err)
	}
	return shadowCall{code: code, stdout: stdout.String(), stderr: stderr.String(), elapsed: elapsed}
}

func shadowTempPath(t *testing.T, dir, prefix string) string {
	t.Helper()
	f, err := os.CreateTemp(dir, prefix)
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	return path
}

// Входы сравниваемых подкоманд сессии s6
func shadowInputs(home string) map[string]string {
	transcript := filepath.Join(home, "transcript.jsonl")
	return map[string]string{
		hookStop: `{"session_id":"s6","hook_event_name":"Stop","transcript_path":"` + transcript + `",` +
			`"background_tasks":[{"id":"t1","type":"shell","description":"old"},{"id":"t2","type":"shell","description":"new"}],"session_crons":[]}`,
		hookNotification: `{"session_id":"s6","hook_event_name":"Notification","notification_type":"permission_prompt","message":"нужен ответ"}`,
		hookNightHint:    `{"session_id":"s6","hook_event_name":"SessionStart","source":"startup"}`,
		hookPreToolUse:   `{"session_id":"s6","hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"kill 999999"},"cwd":"` + filepath.Join(home, "work") + `"}`,
		hookNoPrompt:     `{"session_id":"s6","hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"ls"}}`,
		hookUnsentCheck:  `{"session_id":"s6","hook_event_name":"Stop","transcript_path":"` + transcript + `","last_assistant_message":"готово"}`,
		hookStopFailure:  `{"session_id":"s6","hook_event_name":"StopFailure","error":"rate_limit"}`,
	}
}

// shadowJournal — демон в процессе теста с журналом тени
func shadowJournal(t *testing.T, options ...serverOption) (*testServer, string) {
	t.Helper()
	journal := filepath.Join(t.TempDir(), shadowJournalFile)
	options = append([]serverOption{func(s *server) { s.shadowLog = journal }}, options...)
	return startServer(t, nil, options...), journal
}

func journalRows(t *testing.T, path string) [][]string {
	t.Helper()
	var rows [][]string
	for _, line := range lines(readFile(t, path)) {
		rows = append(rows, strings.Split(line, "\t"))
	}
	return rows
}

// Сценарий 6, прямой порядок: CLI исполняет хук с записью состояния, время
// запроса — 23:59:59.999, демон — позже по своим часам, но по записи; все
// сравниваемые подкоманды совпадают, уведомление ушло один раз, от CLI, а
// состояние пользователя — только запись CLI
func TestShadowMatchesCLI(t *testing.T) {
	sv, journal := shadowJournal(t)
	delivered := countDeliveries(t)
	home := shadowHome(t, true)
	inputs := shadowInputs(home)
	order := []string{hookStop, hookNotification, hookNightHint, hookPreToolUse, hookNoPrompt, hookUnsentCheck}
	for i, hook := range order {
		r := runShadowCLI(t, home, hook, inputs[hook], sv.path)
		if hook == hookNoPrompt && !strings.Contains(r.stdout, `"behavior":"deny"`) {
			t.Errorf("no-prompt stdout %q", r.stdout)
		}
		if !waitFor(t, 20*time.Second, func() bool { return len(journalRows(t, journal)) == i+1 }) {
			t.Fatalf("%s: no shadow line; journal %q; daemon log:\n%s", hook, readFile(t, journal), readFile(t, sv.log))
		}
	}
	for i, row := range journalRows(t, journal) {
		if len(row) != 5 || row[1] != order[i] || row[2] != "s6" || row[3] != shadowMatch {
			t.Errorf("shadow line %d: %q, want %s s6 match", i, row, order[i])
		}
		if row[0] != shadowAt.In(time.Local).Format(shadowTimeLayout) {
			t.Errorf("shadow line %d time %q, want the request time", i, row[0])
		}
	}
	// Метка CLI с управляемыми часами: t2 впервые замечена в shadowAt
	tasks := readFile(t, filepath.Join(home, "run/chooks/sessions/s6.tasks"))
	if !strings.Contains(tasks, `"id":"t2"`) || !strings.Contains(tasks, shadowAt.Format(time.RFC3339Nano)) {
		t.Errorf("tasks after CLI %q, want t2 since %s", tasks, shadowAt.Format(time.RFC3339Nano))
	}
	if got := readFile(t, filepath.Join(home, "run/chooks/sessions/s6")); got != "waiting" {
		t.Errorf("session state %q, want waiting (notification by CLI)", got)
	}
	// Оповещение исполнено ровно раз — CLI на permission_prompt: тень каждой
	// подкоманды уже в журнале, её доставка (если бы была) позади
	if n := delivered(); n != 1 {
		t.Errorf("%d alert deliveries, want exactly 1 from CLI", n)
	}
	if !waitFor(t, 10*time.Second, func() bool { return len(lines(readFile(t, filepath.Join(home, "alerts")))) >= 1 }) {
		t.Fatal("CLI alert not delivered")
	}
	if alerts := lines(readFile(t, filepath.Join(home, "alerts"))); len(alerts) != 1 {
		t.Errorf("%d alerts, want exactly 1 from CLI: %q", len(alerts), alerts)
	}
	// Временные каталоги тени убраны
	if left, _ := filepath.Glob(filepath.Join(os.TempDir(), "chooks-shadow-*")); len(left) > 0 {
		t.Errorf("shadow temp dirs left: %q", left)
	}
	// Демон не пишет chooks.log и файл длительностей за тень: строки там —
	// только шесть вызовов CLI
	if rows := lines(readFile(t, filepath.Join(home, "log", durationsFile))); len(rows) != len(order) {
		t.Errorf("%d duration rows, want %d (CLI only): %q", len(rows), len(order), rows)
	}
}

// captureFrames — сокет, который копит кадры тени CLI вместо демона
func captureFrames(t *testing.T) (string, func() []hookwire.Frame) {
	t.Helper()
	path := filepath.Join(socketDir(t), "c.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var frames []hookwire.Frame
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.AcceptUnix()
			if err != nil {
				return
			}
			frame, err := hookwire.ReadFrame(conn)
			_ = conn.Close()
			if err == nil {
				mu.Lock()
				frames = append(frames, frame)
				mu.Unlock()
			}
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		<-done
	})
	return path, func() []hookwire.Frame {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(frames)
	}
}

func onlyFrame(t *testing.T, frames func() []hookwire.Frame) (hookwire.Request, shadowRecord) {
	t.Helper()
	if !waitFor(t, 10*time.Second, func() bool { return len(frames()) == 1 }) {
		t.Fatalf("%d shadow frames, want 1", len(frames()))
	}
	frame := frames()[0]
	var rec shadowRecord
	if err := json.Unmarshal(frame.Record, &rec); err != nil {
		t.Fatal(err)
	}
	return frame.Request, rec
}

func stateTree(t *testing.T, home string) map[string]string {
	t.Helper()
	tree := map[string]string{}
	root := filepath.Join(home, "run", "chooks")
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		tree[strings.TrimPrefix(path, root)] = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

// Сценарий 6, обратный порядок: демон исполняет запись раньше CLI — на
// независимом одинаковом снимке (HOME b), — и состояние пользователя от
// этого не меняется; затем CLI в HOME a с тем же снимком — совпало. Демон,
// который видел бы состояние после CLI (запись «ждёт ответа»), решил бы
// иначе: человека «уже позвали»
func TestShadowOrderSwapped(t *testing.T) {
	stubNightProbes(t, true)
	for _, hook := range []string{hookNotification, hookStop, hookNightHint} {
		t.Run(hook, func(t *testing.T) {
			socket, frames := captureFrames(t)
			homeA, homeB := shadowHome(t, true), shadowHome(t, true)
			// Снимок b: запись CLI b — вход демона
			runShadowCLI(t, homeB, hook, shadowInputs(homeB)[hook], socket)
			wire, recB := onlyFrame(t, frames)
			// Состояние пользователя к исполнению демона уже другое (запись
			// следующего хука): демон его не читает и не пишет
			later := map[string]string{}
			for rel := range stateTree(t, homeB) {
				later[filepath.Join("run/chooks", rel)] = "later"
			}
			for _, f := range recB.After {
				if f.Root == shadowRootState {
					later[filepath.Join("state/chooks", f.Rel)] = "later"
				}
			}
			writeFiles(t, homeB, later)
			out, err := executeShadow(wire, recB, quietLogger(t))
			if err != nil {
				t.Fatal(err)
			}
			for rel, data := range stateTree(t, homeB) {
				if data != "later" {
					t.Errorf("daemon changed user state %s: %q", rel, data)
				}
			}
			for name := range later {
				if data := readFile(t, filepath.Join(homeB, name)); data != "later" {
					t.Errorf("daemon changed user state %s: %q", name, data)
				}
			}
			if status, detail := compareShadow(recB, out); status != shadowMatch {
				t.Errorf("user state changed after CLI: %s %s", status, detail)
			}
			before := stateTree(t, homeA)
			if fmt.Sprint(before) != fmt.Sprint(stateTree(t, shadowHome(t, true))) {
				t.Fatalf("home a is not the same snapshot: %v", before)
			}
			// Теперь CLI a: сравнение с исполнением демона, сделанным раньше
			socketA, framesA := captureFrames(t)
			runShadowCLI(t, homeA, hook, shadowInputs(homeB)[hook], socketA)
			_, recA := onlyFrame(t, framesA)
			if status, detail := compareShadow(recA, out); status != shadowMatch {
				t.Errorf("daemon first: %s %s", status, detail)
			}
			// И прямой порядок на тех же данных
			outA, err := executeShadow(wire, recA, quietLogger(t))
			if err != nil {
				t.Fatal(err)
			}
			if status, detail := compareShadow(recA, outA); status != shadowMatch {
				t.Errorf("CLI first: %s %s", status, detail)
			}
		})
	}
}

// Намеренное различие только в файловом эффекте, только в решении (оба
// поля решения), только в коде — каждое ловится своим полем
func TestShadowCatchesEachDifference(t *testing.T) {
	socket, frames := captureFrames(t)
	home := shadowHome(t, true)
	runShadowCLI(t, home, hookNotification, shadowInputs(home)[hookNotification], socket)
	wire, rec := onlyFrame(t, frames)
	out, err := executeShadow(wire, rec, quietLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	if status, detail := compareShadow(rec, out); status != shadowMatch {
		t.Fatalf("baseline: %s %s\nCLI stderr %q\ndaemon stderr %q", status, detail, rec.Stderr, out.Stderr)
	}
	if rec.Decision == nil || !rec.Decision.Alert || rec.Verdict.Decision != "alert" {
		t.Fatalf("baseline decision %+v verdict %+v, want alert", rec.Decision, rec.Verdict)
	}
	if len(rec.Effects) != 1 || !strings.HasPrefix(rec.Effects[0], "alert ") || !slices.Equal(out.Effects, rec.Effects) {
		t.Fatalf("effects CLI %q daemon %q, want one alert each", rec.Effects, out.Effects)
	}
	clone := func() shadowRecord {
		data, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		var c shadowRecord
		if err := json.Unmarshal(data, &c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	cases := []struct {
		name   string
		change func(*shadowRecord)
		want   string
	}{
		{"file effect", func(r *shadowRecord) { r.After[0].Data = []byte("done") }, "state runtime:sessions/s6"},
		{"file created", func(r *shadowRecord) { r.After[1].Exists, r.After[1].Data = !r.After[1].Exists, []byte("{}") }, "state runtime:sessions/s6.tasks"},
		{"notifier decision", func(r *shadowRecord) { r.Decision.Alert = false }, "notifier.decision"},
		{"notifier effect", func(r *shadowRecord) { r.Effects = nil }, "effects"},
		{"verdict decision", func(r *shadowRecord) { r.Verdict.Decision = "mute" }, "decision mute/alert"},
		{"verdict reason", func(r *shadowRecord) { r.Verdict.Reason = "другая причина" }, "reason"},
		{"code", func(r *shadowRecord) { r.Code = exitBlocked }, "code 2/0"},
		{"stdout", func(r *shadowRecord) { r.Stdout = []byte("x") }, "stdout"},
		{"stderr", func(r *shadowRecord) { r.Stderr = []byte("x") }, "stderr"},
		{"error", func(r *shadowRecord) { r.Err = "x" }, "error"},
	}
	for _, c := range cases {
		changed := clone()
		c.change(&changed)
		if status, detail := compareShadow(changed, out); status != shadowMismatch || detail != c.want {
			t.Errorf("%s: %s %q, want mismatch %q", c.name, status, detail, c.want)
		}
	}
	// Решение pre-tool-use: разрешение против отказа
	socket2, frames2 := captureFrames(t)
	runShadowCLI(t, home, hookPreToolUse, `{"session_id":"s6","tool_name":"Bash","tool_input":{"command":"kill -9 -1"}}`, socket2)
	wire, rec = onlyFrame(t, frames2)
	if out, err = executeShadow(wire, rec, quietLogger(t)); err != nil {
		t.Fatal(err)
	}
	if rec.Verdict.Decision != "block" || rec.Code != exitBlocked {
		t.Fatalf("pre-tool-use kill -9 -1: verdict %+v code %d, want block 2", rec.Verdict, rec.Code)
	}
	if status, detail := compareShadow(rec, out); status != shadowMatch {
		t.Fatalf("pre-tool-use baseline: %s %s", status, detail)
	}
	rec.Verdict.Decision = "allow"
	if status, detail := compareShadow(rec, out); status != shadowMismatch || detail != "decision allow/block" {
		t.Errorf("pre-tool-use decision only: %s %q", status, detail)
	}
}

// Внешний эффект в тени не исполняется: повторные теневые исполнения
// записи с уведомлением не зовут заглушку
func TestShadowNoExternalEffect(t *testing.T) {
	socket, frames := captureFrames(t)
	delivered := countDeliveries(t)
	home := shadowHome(t, true)
	runShadowCLI(t, home, hookNotification, shadowInputs(home)[hookNotification], socket)
	wire, rec := onlyFrame(t, frames)
	alerts := filepath.Join(home, "alerts")
	if !waitFor(t, 10*time.Second, func() bool { return len(lines(readFile(t, alerts))) == 1 }) {
		t.Fatal("CLI alert not delivered")
	}
	for range 3 {
		out, err := executeShadow(wire, rec, quietLogger(t))
		if err != nil {
			t.Fatal(err)
		}
		if out.Decision == nil || !out.Decision.Alert {
			t.Fatalf("shadow decision %+v, want alert decided", out.Decision)
		}
	}
	// Теневые исполнения вернулись: доставка в них синхронна, счёт окончателен
	if n := delivered(); n != 1 {
		t.Errorf("%d alert deliveries, want 1 (CLI only)", n)
	}
	if got := lines(readFile(t, alerts)); len(got) != 1 {
		t.Errorf("%d alerts, want 1 (CLI only): %q", len(got), got)
	}
	if log := readFile(t, filepath.Join(home, "log", "chooks.log")); strings.Count(log, `msg="alert allowed"`) != 1 {
		t.Errorf("chooks.log has the shadow's lines:\n%s", log)
	}
}

// Ответ /proc, которого CLI не читал, — «не сравнимо», а не сравнение с
// /proc момента демона
func TestShadowProcNotRecorded(t *testing.T) {
	socket, frames := captureFrames(t)
	home := shadowHome(t, true)
	runShadowCLI(t, home, hookPreToolUse, shadowInputs(home)[hookPreToolUse], socket)
	wire, rec := onlyFrame(t, frames)
	if len(rec.Proc) == 0 {
		t.Fatal("kill guard read no /proc in CLI")
	}
	rec.Proc = nil
	out, err := executeShadow(wire, rec, quietLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	if status, detail := compareShadow(rec, out); status != shadowIncomparable || !strings.Contains(detail, "/proc") {
		t.Errorf("%s %q, want incomparable on /proc", status, detail)
	}
}

// CLI запасного пути прокси тень не шлёт
func TestShadowNotSentOnProxyFallback(t *testing.T) {
	socket, frames := captureFrames(t)
	home := shadowHome(t, true)
	env := append(absoluteEnv(home), hookproxy.EnvSocket+"="+socket)
	request := hookctx.New(strings.NewReader(shadowInputs(home)[hookNoPrompt]), &bytes.Buffer{}, &bytes.Buffer{}, env, home, os.Getpid(), os.Getppid())
	ctx := context.WithValue(context.Background(), cliTerminationKey{}, (func(int))(func(int) {}))
	ctx = context.WithValue(ctx, fallbackReasonKey{}, "connect: no such file or directory")
	if code, err := runHook(hookctx.WithRequest(ctx, request), hookNoPrompt); code != exitAllowed || err != nil {
		t.Fatalf("code %d err %v", code, err)
	}
	runShadowCLI(t, home, hookNoPrompt, shadowInputs(home)[hookNoPrompt], socket)
	if !waitFor(t, 10*time.Second, func() bool { return len(frames()) >= 1 }) {
		t.Fatal("direct CLI sent no shadow")
	}
	// captureFrames принимает и читает соединения по одному в порядке
	// connect: кадр запасного пути, будь он, пришёл бы первым. Прямой CLI
	// узнаётся по журналу заглушки в env, которого у запасного пути нет
	first := frames()[0].Request
	if !slices.ContainsFunc(first.Env, func(e string) bool { return strings.HasPrefix(e, alertLogEnv+"=") }) {
		t.Errorf("first frame is not the direct CLI's: the proxy fallback CLI sent one (env %q)", first.Env)
	}
}

// Мера 3: демона нет; демон принимает соединение и не читает, а запись
// больше буфера сокета. CLI отвечает как без тени и без ошибки. Сроки
// проверяются по признаку в internal/hookproxy (TestSendShadow*), прирост в
// мс — замер scripts/daemon timing --mode shadow, не гейт
func TestShadowSendBounded(t *testing.T) {
	big := `{"session_id":"s6","hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"` + strings.Repeat("a", 4<<20) + `"}}`
	path := filepath.Join(socketDir(t), "deaf.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	held := make(chan *net.UnixConn, 16)
	go func() {
		for {
			conn, err := listener.AcceptUnix()
			if err != nil {
				return
			}
			held <- conn
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		for {
			select {
			case conn := <-held:
				_ = conn.Close()
			default:
				return
			}
		}
	})
	off := shadowHome(t, false)
	on := shadowHome(t, true)
	missing := filepath.Join(socketDir(t), "none.sock")
	for _, c := range []struct{ name, socket string }{{"no socket", missing}, {"daemon not reading", path}} {
		base := runShadowCLI(t, off, hookNoPrompt, big, c.socket)
		for range 3 {
			r := runShadowCLI(t, on, hookNoPrompt, big, c.socket)
			if r.code != base.code || r.stdout != base.stdout || r.stderr != base.stderr {
				t.Errorf("%s: code %d stderr %q, without shadow code %d stderr %q", c.name, r.code, r.stderr, base.code, base.stderr)
			}
		}
	}
	// Признак, что CLI дошёл до записи в нечитающий демон: три соединения
	// приняты (срок и errno этапов — internal/hookproxy, TestSendShadow*)
	if !waitFor(t, 10*time.Second, func() bool { return len(held) == 3 }) {
		t.Errorf("deaf daemon accepted %d connections, want 3", len(held))
	}
}

// Теневая запись проходит проверку uid отправителя, как обычный запрос
func TestShadowChecksUID(t *testing.T) {
	sv, journal := shadowJournal(t, func(s *server) { s.uid = os.Getuid() + 1 })
	home := shadowHome(t, true)
	runShadowCLI(t, home, hookNoPrompt, shadowInputs(home)[hookNoPrompt], sv.path)
	if !waitFor(t, 10*time.Second, func() bool { return strings.Contains(readFile(t, sv.log), "reason=uid") }) {
		t.Fatalf("shadow from another uid not refused; daemon log:\n%s", readFile(t, sv.log))
	}
	if rows := journalRows(t, journal); len(rows) != 0 {
		t.Errorf("shadow lines from another uid: %q", rows)
	}
}

// Сводка: охват, сравнения, несравнимые и пропущенные по chooks-hooks.tsv;
// ноль сравнений — не «без расхождений»
func TestShadowReport(t *testing.T) {
	dir := t.TempDir()
	at := func(d time.Duration) string { return shadowAt.Add(d).Format(shadowTimeLayout) }
	writeFiles(t, dir, map[string]string{
		shadowJournalFile: strings.Join([]string{
			at(-time.Hour) + "\tpre-tool-use\ts\tmatch\t",
			at(-time.Hour) + "\tpre-tool-use\ts\tmismatch\tstderr",
			at(-time.Hour) + "\tstop\ts\tincomparable\tbusy",
			at(-48*time.Hour) + "\tstop\ts\tmatch\t",
		}, "\n") + "\n",
		durationsFile: strings.Join([]string{
			at(-time.Hour) + "\tpre-tool-use\ts\t10\t5\tok",
			at(-time.Hour) + "\tpre-tool-use\ts\t10\t5\tok",
			at(-time.Hour) + "\tpre-tool-use\ts\t10\t5\tok",
			at(-time.Hour) + "\tpre-tool-use\ts\t10\t5\tok\tdaemon\t",
			at(-time.Hour) + "\tstop\ts\t10\t5\tok",
			at(-time.Hour) + "\tclaim-stop\ts\t10\t5\tok",
		}, "\n") + "\n",
	})
	report, err := buildShadowReport(filepath.Join(dir, shadowJournalFile), filepath.Join(dir, durationsFile), shadowAt.Add(-24*time.Hour), shadowAt)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	report.write(&out, time.UTC)
	text := out.String()
	for _, want := range []string{
		"pre-tool-use: вызовов CLI 3, сравнено 2 (совпало 1, расхождений 1), не сравнимо 0, пропущено 1",
		"stop: вызовов CLI 1, сравнено 0 (совпало 0, расхождений 0), не сравнимо 1, пропущено 0",
		"pre-tool-use: stderr — 1",
		"stop: busy — 1",
		"claim-stop — судья claude -p",
		"Итог: вызовов CLI 4, сравнено 2, расхождений 1, не сравнимо 1",
		"Пропущено (вызовы CLI без строки тени): 1",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("report lacks %q:\n%s", want, text)
		}
	}
	empty, err := buildShadowReport(filepath.Join(dir, "none.tsv"), filepath.Join(dir, durationsFile), shadowAt.Add(-24*time.Hour), shadowAt)
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	empty.write(&out, time.UTC)
	if empty.compared() != 0 || !strings.Contains(out.String(), "Сравнений нет: это не «без расхождений».") || !strings.Contains(out.String(), "журнала тени нет") {
		t.Errorf("empty report:\n%s", out.String())
	}
}

func TestShadowReportCountsUnpairedShadowRows(t *testing.T) {
	from := shadowAt.Add(-24 * time.Hour)
	to := shadowAt
	ln := func(at time.Time, fields ...string) string {
		return at.Format(shadowTimeLayout) + "\t" + strings.Join(fields, "\t") + "\n"
	}
	a, b := from.Add(time.Hour), from.Add(2*time.Hour)
	tests := []struct {
		name, shadow, durations string
		check                   func(*testing.T, *shadowReport, string)
	}{
		{
			name:      "lost final CLI row keeps mismatch",
			shadow:    ln(a, "stop", "s1", shadowMatch, "") + ln(b, "stop", "s1", shadowMismatch, "verdict"),
			durations: ln(a.Add(10*time.Millisecond), "stop", "s1", "30", "20", "ok"),
			check: func(t *testing.T, r *shadowReport, out string) {
				if r.mismatches() != 1 || r.shadowWithoutCall != 1 || !strings.Contains(out, "stop: verdict — 1") {
					t.Errorf("mismatch hidden or extra row uncounted:\n%s", out)
				}
				// Строка хука и итог тоже называют расхождение (ревью srns круг 2)
				if !strings.Contains(out, "сравнено 1 (совпало 1, расхождений 0), расхождений без вызова CLI 1,") || !strings.Contains(out, "Итог: вызовов CLI 1, сравнено 1, расхождений 1,") {
					t.Errorf("summary hides unpaired mismatch:\n%s", out)
				}
			},
		},
		{
			name:      "shadow without any CLI row",
			shadow:    ln(a, "pre-tool-use", "other", shadowMismatch, "action") + ln(a, "stop", "s1", shadowMatch, ""),
			durations: ln(a.Add(10*time.Millisecond), "stop", "s1", "30", "20", "ok"),
			check: func(t *testing.T, r *shadowReport, out string) {
				if r.mismatches() != 1 || r.shadowWithoutCall != 1 || !strings.Contains(out, "pre-tool-use: action — 1") {
					t.Errorf("mismatch hidden or extra row uncounted:\n%s", out)
				}
			},
		},
		{
			name:      "unparsed CLI record keeps incomparable row",
			shadow:    ln(a.Add(time.Millisecond), "stop", "", shadowIncomparable, "запись CLI не разобрана: x"),
			durations: ln(a, "stop", "s1", "30", "20", "ok"),
			check: func(t *testing.T, r *shadowReport, out string) {
				if r.counts["stop"].incomparable != 1 || r.shadowWithoutCall != 1 || !strings.Contains(out, "stop: запись CLI не разобрана: x — 1") {
					t.Errorf("incomparable row hidden or extra row uncounted:\n%s", out)
				}
			},
		},
		{
			name:      "lost CLI row is counted separately, not as a negative skip",
			shadow:    ln(a, "stop", "s1", shadowMatch, ""),
			durations: "",
			check: func(t *testing.T, r *shadowReport, out string) {
				if r.compared() != 0 || r.shadowWithoutCall != 1 || !strings.Contains(out, "Пропущено (вызовы CLI без строки тени): 0") || !strings.Contains(out, "Сравнений нет: это не «без расхождений».") {
					t.Errorf("lost CLI row accounting:\n%s", out)
				}
			},
		},
		{
			name:   "shadow outside period is ignored",
			shadow: ln(from.Add(-time.Second), "stop", "s1", shadowMismatch, "old"),
			check: func(t *testing.T, r *shadowReport, out string) {
				if r.mismatches() != 0 || r.shadowWithoutCall != 0 || strings.Contains(out, "stop: old") {
					t.Errorf("out-of-period shadow row was counted:\n%s", out)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tdir := t.TempDir()
			writeFiles(t, tdir, map[string]string{
				shadowJournalFile: tt.shadow,
				durationsFile:     ln(from.Add(-time.Hour), "statusline", "s0", "1", "1", "ok") + tt.durations,
			})
			r, err := buildShadowReport(filepath.Join(tdir, shadowJournalFile), filepath.Join(tdir, durationsFile), from, to)
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			r.write(&out, time.UTC)
			tt.check(t, r, out.String())
			if !strings.Contains(out.String(), "Строк тени без вызова CLI:") {
				t.Errorf("report lacks unpaired shadow count:\n%s", out.String())
			}
		})
	}
}

// Снимок «до» берётся до исполнения CLI: демон не видит записи CLI
func TestShadowSnapshotBeforeCLI(t *testing.T) {
	socket, frames := captureFrames(t)
	home := shadowHome(t, true)
	runShadowCLI(t, home, hookNotification, shadowInputs(home)[hookNotification], socket)
	_, rec := onlyFrame(t, frames)
	before := map[string]string{}
	for _, f := range rec.Before {
		before[f.Rel] = string(f.Data)
	}
	after := map[string]string{}
	for _, f := range rec.After {
		after[f.Rel] = string(f.Data)
	}
	if before["sessions/s6"] != "working" || after["sessions/s6"] != "waiting" {
		t.Errorf("before %q after %q, want working → waiting", before, after)
	}
	if !rec.Time.Equal(shadowAt) {
		t.Errorf("record time %s, want %s", rec.Time, shadowAt)
	}
}

// stubNightProbes — признак блокировки экрана — заглушка: locked, крышка
// открыта; счётчик вызовов обеих
func stubNightProbes(t *testing.T, locked bool) *atomic.Int32 {
	t.Helper()
	var calls atomic.Int32
	saved := nightProbes
	nightProbes.locked = func(context.Context) (bool, error) { calls.Add(1); return locked, nil }
	nightProbes.lidClosed = func(context.Context) (bool, error) { calls.Add(1); return false, nil }
	t.Cleanup(func() { nightProbes = saved })
	return &calls
}

// Сценарий 6 на ночной метке: в 23:59:59.999 CLI пишет метку ночи и даёт
// подсказку; демон исполняет позже по своим часам, но со временем записи —
// совпало, а признак блокировки (заглушка) вызван ровно раз, от CLI. Демон
// со своим временем (00:00:00.001) дал бы ложное расхождение
func TestShadowNightMark(t *testing.T) {
	calls := stubNightProbes(t, true)
	socket, frames := captureFrames(t)
	home := shadowHome(t, true)
	r := runShadowCLI(t, home, hookNightHint, shadowInputs(home)[hookNightHint], socket)
	if !strings.Contains(r.stdout, "Сейчас ночь (23:59)") {
		t.Fatalf("night-hint stdout %q stderr %q, want the hint at 23:59", r.stdout, r.stderr)
	}
	wire, rec := onlyFrame(t, frames)
	if len(rec.Before) != 1 || rec.Before[0].Exists || len(rec.After) != 1 || !strings.Contains(string(rec.After[0].Data), `"night":"2026-11-22"`) {
		t.Fatalf("night mark before %+v after %+v, want written by CLI", rec.Before, rec.After)
	}
	name, err := nighthint.RecordName("s6")
	if err != nil {
		t.Fatal(err)
	}
	if mark := readFile(t, filepath.Join(home, "state/chooks/night-hint", name)); mark != string(rec.After[0].Data) {
		t.Fatalf("user mark %q, record %q", mark, rec.After[0].Data)
	}
	out, err := executeShadow(wire, rec, quietLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	if status, detail := compareShadow(rec, out); status != shadowMatch {
		t.Errorf("night mark: %s %s", status, detail)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("lock probe called %d times, want 1 (CLI)", n)
	}
	late := rec
	late.Time = shadowAt.Add(2 * time.Millisecond)
	out, err = executeShadow(wire, late, quietLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	if status, detail := compareShadow(rec, out); status != shadowMismatch || !strings.Contains(detail, "stdout") {
		t.Errorf("daemon at 00:00:00.001: %s %q, want a stdout mismatch", status, detail)
	}
}

// countKonsoleTitles подменяет заголовок вкладки Konsole (D-Bus) под тенью
// счётчиком без D-Bus. Вызов синхронен: после возврата хука и теневого
// исполнения счёт окончателен
func countKonsoleTitles(t *testing.T) func() int64 {
	t.Helper()
	var n atomic.Int64
	orig := realNotifierEffects
	realNotifierEffects = func() notifier.Effects {
		effects := orig()
		effects.KonsoleTitle = func(ctx context.Context, service, session, title string, request hookctx.Context) error {
			n.Add(1)
			return nil
		}
		return effects
	}
	t.Cleanup(func() { realNotifierEffects = orig })
	return n.Load
}

// konsoleTestPID — pid вкладки Konsole в konsoleProc
const konsoleTestPID = 7777

// konsoleProc — /proc машины, где родитель процесса pid — konsole: вкладка
// принадлежит хуку, и заголовок ставится через D-Bus
type konsoleProc struct{ pid int }

func (k konsoleProc) ReadFile(name string) ([]byte, error) {
	switch name {
	case fmt.Sprintf("/proc/%d/stat", k.pid):
		return fmt.Appendf(nil, "%d (chooks) S %d %s", k.pid, konsoleTestPID, strings.Repeat("0 ", 48)), nil
	case fmt.Sprintf("/proc/%d/comm", k.pid):
		return []byte("chooks\n"), nil
	case fmt.Sprintf("/proc/%d/stat", konsoleTestPID):
		return fmt.Appendf(nil, "%d (konsole) S 1 %s", konsoleTestPID, strings.Repeat("0 ", 48)), nil
	case fmt.Sprintf("/proc/%d/comm", konsoleTestPID):
		return []byte("konsole\n"), nil
	}
	return procfs.OS{}.ReadFile(name)
}

func (konsoleProc) ReadDir(name string) ([]string, error) { return procfs.OS{}.ReadDir(name) }

// Мера 4, заголовок Konsole: CLI ставит его через D-Bus, тень только
// записывает эффект, и записи CLI и демона совпадают
func TestShadowNoKonsoleTitle(t *testing.T) {
	socket, frames := captureFrames(t)
	titles := countKonsoleTitles(t)
	home := shadowHome(t, true)
	runShadowCLIWith(t, home, hookNotification, shadowInputs(home)[hookNotification], socket, konsoleProc{pid: os.Getpid()},
		"KONSOLE_DBUS_SERVICE=org.kde.konsole-7777", "KONSOLE_DBUS_SESSION=/Sessions/1")
	if n := titles(); n != 1 {
		t.Fatalf("%d Konsole titles by CLI, want 1", n)
	}
	wire, rec := onlyFrame(t, frames)
	if !slices.ContainsFunc(rec.Effects, func(e string) bool { return strings.HasPrefix(e, "konsole title ") }) {
		t.Fatalf("CLI record lacks the Konsole title: %q", rec.Effects)
	}
	out, err := executeShadow(wire, rec, quietLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	if n := titles(); n != 1 {
		t.Errorf("%d Konsole titles, want 1 (CLI only): the shadow set one over D-Bus", n)
	}
	if status, detail := compareShadow(rec, out); status != shadowMatch {
		t.Errorf("%s %q, want match", status, detail)
	}
}

// Путь снимка из записи вне корня — ошибка, файл не трогается
func TestShadowPathOutsideRoot(t *testing.T) {
	request := hookctx.New(nil, nil, nil, absoluteEnv(t.TempDir()), "/", 0, 0)
	for _, p := range []shadowPath{
		{Root: shadowRootRuntime, Rel: "../escaped"},
		{Root: shadowRootState, Rel: "night-hint/../../escaped"},
		{Root: shadowRootRuntime, Rel: "/tmp/escaped"},
	} {
		if path, err := p.abs(request); err == nil {
			t.Errorf("%s:%s resolved to %s, want an error", p.Root, p.Rel, path)
		}
	}
}

// Кадр CHS1 от процесса того же uid с путём «до» вне каталога тени: демон
// ничего не пишет вне него, исход — «не сравнимо»
func TestShadowBeforeEscapes(t *testing.T) {
	sv, journal := shadowJournal(t)
	home := shadowHome(t, true)
	target := filepath.Join(t.TempDir(), "escaped.txt")
	rel := strings.Repeat("../", 30) + strings.TrimPrefix(target, "/")
	rec := shadowRecord{Session: "s6", Before: []shadowFile{{shadowPath: shadowPath{Root: shadowRootRuntime, Rel: rel}, Exists: true, Data: []byte("written by shadow\n")}}}
	blob, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	wire := hookwire.Request{Subcommand: hookStop, Env: absoluteEnv(home), Cwd: home, PID: 1, Stdin: []byte(shadowInputs(home)[hookStop])}
	if err := hookproxy.SendShadow(sv.path, 10*time.Second, func() ([]byte, error) { return hookwire.EncodeShadow(wire, blob), nil }); err != nil {
		t.Fatal(err)
	}
	if !waitFor(t, 20*time.Second, func() bool { return len(journalRows(t, journal)) == 1 }) {
		t.Fatalf("no shadow line; daemon log:\n%s", readFile(t, sv.log))
	}
	if row := journalRows(t, journal)[0]; len(row) != 5 || row[3] != shadowIncomparable || !strings.Contains(row[4], "вне охвата") {
		t.Errorf("shadow line %q, want incomparable: path out of scope", row)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("file outside the shadow dir: stat %v", err)
	}
}

// Путь записи внутри корня, но не из вычисленных демоном для подкоманды и
// сессии, — «не сравнимо»: демон берёт из записи только содержимое своих путей
func TestShadowForeignStatePath(t *testing.T) {
	socket, frames := captureFrames(t)
	home := shadowHome(t, true)
	runShadowCLI(t, home, hookStop, shadowInputs(home)[hookStop], socket)
	wire, rec := onlyFrame(t, frames)
	foreign := shadowFile{shadowPath: shadowPath{Root: shadowRootRuntime, Rel: "sessions/other"}, Exists: true, Data: []byte("idle")}
	for name, edit := range map[string]func(r *shadowRecord){
		"before extra": func(r *shadowRecord) { r.Before = append(slices.Clone(r.Before), foreign) },
		"after extra":  func(r *shadowRecord) { r.After = append(slices.Clone(r.After), foreign) },
		"before swapped": func(r *shadowRecord) {
			r.Before = slices.Clone(r.Before)
			r.Before[0] = foreign
		},
	} {
		r := rec
		edit(&r)
		if _, err := executeShadow(wire, r, quietLogger(t)); err == nil || !strings.Contains(err.Error(), "вне охвата") {
			t.Errorf("%s: err %v, want path out of scope", name, err)
		}
	}
	if _, err := executeShadow(wire, rec, quietLogger(t)); err != nil {
		t.Errorf("unchanged record: %v", err)
	}
}

// Ротация: строки ротированных .1 журнала тени и файла длительностей входят
// в счёт; файл длительностей начинается позже периода — период с его начала
func TestShadowReportRotated(t *testing.T) {
	dir := t.TempDir()
	at := func(d time.Duration) string { return shadowAt.Add(d).Format(shadowTimeLayout) }
	writeFiles(t, dir, map[string]string{
		shadowJournalFile + ".1": at(-20*time.Hour) + "\tpre-tool-use\ts\tmatch\t\n",
		shadowJournalFile:        at(-time.Hour) + "\tpre-tool-use\ts\tmismatch\tstderr\n",
		durationsFile + ".1": at(-20*time.Hour) + "\tpre-tool-use\ts\t10\t5\tok\n" +
			at(-19*time.Hour) + "\tpre-tool-use\ts\t10\t5\tok\n",
		durationsFile: at(-time.Hour) + "\tpre-tool-use\ts\t10\t5\tok\n",
	})
	report, err := buildShadowReport(filepath.Join(dir, shadowJournalFile), filepath.Join(dir, durationsFile), shadowAt.Add(-24*time.Hour), shadowAt)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	report.write(&out, time.UTC)
	for _, want := range []string{
		"pre-tool-use: вызовов CLI 3, сравнено 2 (совпало 1, расхождений 1), не сравнимо 0, пропущено 1",
		"период не покрыт файлом длительностей до " + shadowAt.Add(-20*time.Hour).UTC().Format("2006-01-02 15:04:05"),
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, out.String())
		}
	}
}

// Снимок «после» — сразу за записью состояния CLI: запись соседа (строка
// статуса) во время уведомления в сравнение не попадает
func TestShadowAfterSnapshotBeforeNeighbor(t *testing.T) {
	socket, frames := captureFrames(t)
	home := shadowHome(t, true)
	state := filepath.Join(home, "run/chooks/sessions/s6")
	orig := realNotifierEffects
	realNotifierEffects = func() notifier.Effects {
		effects := orig()
		deliver := effects.Deliver
		effects.Deliver = func(executable string, alert desktop.Alert, request hookctx.Context) error {
			if err := os.WriteFile(state, []byte("idle"), 0o600); err != nil {
				return err
			}
			return deliver(executable, alert, request)
		}
		return effects
	}
	t.Cleanup(func() { realNotifierEffects = orig })
	runShadowCLI(t, home, hookNotification, shadowInputs(home)[hookNotification], socket)
	if got := readFile(t, state); got != "idle" {
		t.Fatalf("session state %q: the neighbour did not write during the notification", got)
	}
	wire, rec := onlyFrame(t, frames)
	out, err := executeShadow(wire, rec, quietLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	if status, detail := compareShadow(rec, out); status != shadowMatch {
		t.Errorf("%s %q, want match: the neighbour's write got into the CLI snapshot", status, detail)
	}
}
