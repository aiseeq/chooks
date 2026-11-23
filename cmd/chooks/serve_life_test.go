package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/aiseeq/chooks/internal/core"
	"github.com/aiseeq/chooks/internal/fallbackwatch"
	"github.com/aiseeq/chooks/internal/hookctx"
	"github.com/aiseeq/chooks/internal/hookproxy"
	"github.com/aiseeq/chooks/internal/hookwire"
)

// Режим бинаря тестов «демон под надзором» (TestMain): версия — из хвоста
// своего исполняемого файла, старт отмечается в журнале теста
const (
	daemonHelper = "CHOOKS_TEST_DAEMON"
	// daemonStartsEnv — журнал стартов: «start <версия> <pid>», «hold <версия>»
	daemonStartsEnv = "CHOOKS_TEST_DAEMON_STARTS"
	// daemonHoldEnv — версия, которая на старте ждёт файла daemonReleaseEnv:
	// окно, в которое тест заменяет исполняемый файл второй раз
	daemonHoldEnv    = "CHOOKS_TEST_DAEMON_HOLD"
	daemonReleaseEnv = "CHOOKS_TEST_DAEMON_RELEASE"
	// alertLogEnv — журнал уведомлений: бинарь тестов в роли deliver-alert
	// пишет сюда свои аргументы
	alertLogEnv = "CHOOKS_TEST_ALERT_LOG"
	// alertHoldEnv — файл, до появления которого бинарь тестов в роли
	// deliver-alert не выходит (не дольше 30 с): живой фоновый процесс
	// хука. Свой pid он пишет в журнал строкой «pid N»
	alertHoldEnv = "CHOOKS_TEST_ALERT_HOLD"
	// versionTrailer — метка версии в хвосте копии бинаря тестов
	versionTrailer = "\nCHOOKS-TEST-VERSION="
)

func appendTestLine(path, line string) {
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, "test journal:", err)
		return
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		fmt.Fprintln(os.Stderr, "test journal:", err)
	}
}

func recordTestAlert() int {
	appendTestLine(os.Getenv(alertLogEnv), strings.Join(os.Args[1:], "\x1f"))
	release := os.Getenv(alertHoldEnv)
	if release == "" {
		return 0
	}
	appendTestLine(os.Getenv(alertLogEnv), fmt.Sprintf("pid %d", os.Getpid()))
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(release); err == nil {
			break
		}
	}
	return 0
}

// trailerVersion — версия из хвоста /proc/self/exe; нет метки — «test»
func trailerVersion() string {
	f, err := os.Open("/proc/self/exe")
	if err != nil {
		return "test"
	}
	defer f.Close()
	tail := make([]byte, 64)
	info, err := f.Stat()
	if err != nil || info.Size() < int64(len(tail)) {
		return "test"
	}
	if _, err := f.ReadAt(tail, info.Size()-int64(len(tail))); err != nil {
		return "test"
	}
	i := bytes.LastIndex(tail, []byte(versionTrailer))
	if i < 0 {
		return "test"
	}
	return strings.TrimSpace(string(tail[i+len(versionTrailer):]))
}

func runTestDaemon() int {
	Version = trailerVersion()
	starts := os.Getenv(daemonStartsEnv)
	appendTestLine(starts, fmt.Sprintf("start %s %d", Version, os.Getpid()))
	if os.Getenv(daemonHoldEnv) == Version {
		appendTestLine(starts, "hold "+Version)
		release := os.Getenv(daemonReleaseEnv)
		for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if _, err := os.Stat(release); err == nil {
				break
			}
		}
	}
	return runCLI()
}

var testBinaryBytes = sync.OnceValues(func() ([]byte, error) {
	exe, err := selfExe()
	if err != nil {
		return nil, err
	}
	return os.ReadFile(exe)
})

// installVersion кладёт в path копию бинаря тестов с версией version так же,
// как graft install: новый файл рядом и rename поверх. mtime — момент
// rename: от него демон считает окно обнаружения
func installVersion(t *testing.T, path, version string) time.Time {
	t.Helper()
	data, err := testBinaryBytes()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	tmp := path + ".new"
	if err := os.WriteFile(tmp, append(slices.Clone(data), []byte(versionTrailer+version+"\n")...), 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := os.Chtimes(tmp, now, now); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	return now
}

// supervised — демон под заглушкой systemd: сокет держит тест и отдаёт
// каждому запуску как fd 3 с LISTEN_FDS=1 и LISTEN_PID; вышедший демон
// запускается снова через 50 мс (как Restart=, без запуска по сокету)
type supervised struct {
	socket, log, starts string
	cmd                 *exec.Cmd
	stop                string
}

func startSupervised(t *testing.T, exe, home string, extraEnv ...string) *supervised {
	t.Helper()
	dir := socketDir(t)
	sv := &supervised{socket: filepath.Join(dir, "chooks.sock"), log: filepath.Join(home, ".claude", "logs", "chooks.log"),
		starts: filepath.Join(home, "starts"), stop: filepath.Join(home, "stop")}
	listener, err := listenSocket(sv.socket)
	if err != nil {
		t.Fatal(err)
	}
	file, err := listener.File()
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(home, "supervisor.sh")
	body := "#!/bin/sh\nwhile [ ! -e \"$STOP\" ]; do\n  sh -c 'export LISTEN_PID=$$; exec \"$0\" serve' \"$EXE\"\n  sleep 0.05\ndone\n"
	writeFiles(t, home, map[string]string{"supervisor.sh": body})
	cmd := exec.Command(script)
	cmd.Env = append(absoluteEnv(home), "LISTEN_FDS=1", daemonHelper+"=1", daemonStartsEnv+"="+sv.starts,
		"STOP="+sv.stop, "EXE="+exe)
	cmd.Env = append(cmd.Env, extraEnv...)
	cmd.Dir = home
	cmd.ExtraFiles = []*os.File{file}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stderr, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	sv.cmd = cmd
	t.Cleanup(func() {
		writeFiles(t, home, map[string]string{"stop": ""})
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-done
			t.Errorf("supervised daemon did not stop on SIGTERM; output:\n%s", stderr.String())
		}
		_ = listener.Close()
	})
	return sv
}

// waitLog ждёт в логе демона строку с каждым из parts
func (sv *supervised) waitLog(t *testing.T, limit time.Duration, parts ...string) bool {
	t.Helper()
	return waitFor(t, limit, func() bool {
		return slices.ContainsFunc(lines(readFile(t, sv.log)), func(line string) bool {
			for _, p := range parts {
				if !strings.Contains(line, p) {
					return false
				}
			}
			return true
		})
	})
}

const preToolUseLS = `{"session_id":"s","hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"ls"}}`

// servedBy — версии демона, исполнившие подкоманду hook (строки «request
// done» лога)
func servedBy(text, hook string) []string {
	var versions []string
	re := regexp.MustCompile(`msg="daemon: request done" hook=` + regexp.QuoteMeta(hook) + ` .* version=(\S+)`)
	for _, m := range re.FindAllStringSubmatch(text, -1) {
		versions = append(versions, m[1])
	}
	return versions
}

// Сценарий 5, обновление после install: v1 исполняет долгий A, исполняемый
// файл заменён на v2, сразу за этим приходит B, и v2 заменён на v3 во время
// своего старта. A исполнен ровно раз (v1), B v1 и v2 не исполняют: v1,
// дообслуживая A, отвечает B «в CLI: upgrade» (ревью 8f10b59, п. 2). Сокет
// всё время доступен (ни одного «connect:»), следующий запрос v2 оставляет в
// очереди сокета, и его исполняет v3
func TestServeUpdateAfterInstall(t *testing.T) {
	home, work := webFetchHome(t, "2.5")
	exe := filepath.Join(home, "bin", "chooks")
	installVersion(t, exe, "v1")
	release := filepath.Join(home, "release")
	sv := startSupervised(t, exe, home, daemonHoldEnv+"=v2", daemonReleaseEnv+"="+release)
	if !sv.waitLog(t, 20*time.Second, `msg="daemon: serving"`, "version=v1", "socket=systemd:") {
		t.Fatalf("v1 not serving; log:\n%s", readFile(t, sv.log))
	}
	runs := filepath.Join(home, "cli-runs")
	env := proxyEnv(t, absoluteEnv(home), sv.socket, runs)
	resA := make(chan proxyRun, 1)
	go func() {
		resA <- runBinary(t, proxyHelper, []string{hookWebFetchFallback}, env, work, []byte(webFetchForbidden))
	}()
	if !waitFor(t, 20*time.Second, func() bool { return len(lines(readFile(t, filepath.Join(home, "effects")))) == 1 }) {
		t.Fatalf("A not started; log:\n%s", readFile(t, sv.log))
	}
	if fallbacks := lines(readFile(t, runs)); len(fallbacks) > 0 {
		t.Skipf("A went to CLI under load (%v): the update path is not exercised", fallbacks)
	}
	replaced := installVersion(t, exe, "v2")
	resB := make(chan proxyRun, 1)
	go func() {
		resB <- runBinary(t, proxyHelper, []string{hookPreToolUse}, env, home, []byte(preToolUseLS))
	}()
	// v1 дообслуживает A и выходит; v2 на старте ждёт, тест кладёт v3
	if !waitFor(t, 30*time.Second, func() bool { return strings.Contains(readFile(t, sv.starts), "hold v2") }) {
		t.Fatalf("v2 not started; starts:\n%s\nlog:\n%s", readFile(t, sv.starts), readFile(t, sv.log))
	}
	installVersion(t, exe, "v3")
	writeFiles(t, home, map[string]string{"release": ""})
	a, b := <-resA, <-resB
	if a.code != exitAllowed || !strings.Contains(a.stdout, "updatedToolOutput") {
		t.Errorf("A: code %d stdout %q stderr %q", a.code, a.stdout, a.stderr)
	}
	if effects := lines(readFile(t, filepath.Join(home, "effects"))); len(effects) != 1 {
		t.Errorf("A ran %d times, want 1", len(effects))
	}
	if b.code != exitAllowed {
		t.Errorf("B: code %d stderr %q", b.code, b.stderr)
	}
	if reasons := lines(readFile(t, runs)); !slices.Equal(reasons, []string{"upgrade"}) {
		t.Errorf("fallbacks %q, want B left to CLI by v1 with \"upgrade\"", reasons)
	}
	if !sv.waitLog(t, 20*time.Second, `msg="daemon: serving"`, "version=v2") {
		t.Fatalf("v2 not serving; starts:\n%s\nlog:\n%s", readFile(t, sv.starts), readFile(t, sv.log))
	}
	var c proxyRun
	for attempt := 1; ; attempt++ {
		before := len(lines(readFile(t, runs)))
		c = runBinary(t, proxyHelper, []string{hookPreToolUse}, env, home, []byte(preToolUseLS))
		if len(lines(readFile(t, runs))) == before {
			break
		}
		if attempt == 3 {
			t.Fatalf("C went to CLI 3 times: %v", lines(readFile(t, runs)))
		}
	}
	if c.code != exitAllowed {
		t.Errorf("C: code %d stderr %q", c.code, c.stderr)
	}
	if !sv.waitLog(t, 20*time.Second, `msg="daemon: serving"`, "version=v3") {
		t.Fatalf("v3 not serving; starts:\n%s\nlog:\n%s", readFile(t, sv.starts), readFile(t, sv.log))
	}
	log := readFile(t, sv.log)
	if got := servedBy(log, hookPreToolUse); !slices.Equal(got[len(got)-1:], []string{"v3"}) || slices.Contains(got, "v1") || slices.Contains(got, "v2") {
		t.Errorf("pre-tool-use served by %v, want only v3 (B not by v1 or v2); log:\n%s", got, log)
	}
	if got := servedBy(log, hookWebFetchFallback); !slices.Equal(got, []string{"v1"}) {
		t.Errorf("A served by %v, want [v1]", got)
	}
	for _, reason := range lines(readFile(t, runs)) {
		if strings.HasPrefix(reason, "connect:") {
			t.Errorf("socket unavailable during the update: %q", reason)
		}
	}
	if !strings.Contains(log, `msg="daemon: executable changed, stop accepting"`) || strings.Count(log, "executable changed") < 2 {
		t.Errorf("v1 and v2 did not both detect the replacement; log:\n%s", log)
	}
	// Окно: от замены на v2 до её обнаружения v1 — на первом accept после неё
	if m := regexp.MustCompile(`executable changed.*\((\d+) ms ago\).*version=v1`).FindStringSubmatch(log); m != nil {
		t.Logf("v1 detected the replacement %s ms after it (B connected right after the rename); A answered %s after the rename",
			m[1], time.Since(replaced).Round(time.Millisecond))
	} else {
		t.Errorf("no detection line of v1; log:\n%s", log)
	}
	t.Logf("starts:\n%s", readFile(t, sv.starts))
	// Демон, остановленный SIGTERM, не удаляет сокет systemd
	pid := 0
	for _, line := range lines(readFile(t, sv.starts)) {
		if fields := strings.Fields(line); len(fields) == 3 && fields[0] == "start" && fields[1] == "v3" {
			pid, _ = strconv.Atoi(fields[2])
		}
	}
	writeFiles(t, home, map[string]string{"stop": ""})
	if pid == 0 || syscall.Kill(pid, syscall.SIGTERM) != nil || !waitFor(t, 10*time.Second, func() bool { return processGone(pid) }) {
		t.Fatalf("v3 pid %d did not stop", pid)
	}
	if info, err := os.Lstat(sv.socket); err != nil || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("socket after the daemon stopped: %v %v", info, err)
	}
}

// fakeExe — сверка исполняемого файла по очереди ответов
type fakeExe struct {
	mu      sync.Mutex
	answers []bool
}

func (f *fakeExe) changed() (bool, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.answers) == 0 {
		return false, ""
	}
	answer := f.answers[0]
	f.answers = f.answers[1:]
	return answer, "fake"
}

// watchFile — сверка файла path, как если бы демон исполнял его
func watchFile(t *testing.T, path string) *exeWatch {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		t.Fatal(err)
	}
	return &exeWatch{path: path, running: identityOf(st)}
}

// serveDirect — демон в процессе теста; done — итог serve
func serveDirect(t *testing.T, srv *server) (string, <-chan error) {
	t.Helper()
	path := filepath.Join(socketDir(t), "d.sock")
	listener, err := listenSocket(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- srv.serve(ctx, listener) }()
	return path, done
}

// Сценарий 5 в процессе: после замены файла следующее соединение демон не
// принимает (сверка до accept) и выходит; замена между сверкой и accept —
// «в CLI: upgrade». Исполнитель после замены не вызван ни разу. Окно от
// замены до выхода — в лог теста
func TestServeStopsOnExecutableChange(t *testing.T) {
	t.Run("before accept", func(t *testing.T) {
		var calls atomic.Int64
		bin := filepath.Join(t.TempDir(), "chooks")
		writeFiles(t, filepath.Dir(bin), map[string]string{"chooks": "v1"})
		srv := newServer(quietLogger(t))
		srv.run, srv.exe = countingRun(&calls), watchFile(t, bin)
		freshStart(srv)
		path, done := serveDirect(t, srv)
		env := absoluteEnv(t.TempDir())
		conn, ack, reason := sendRequest(t, path, selfRequest(t, hookPreToolUse, env, `{}`))
		if ack != hookwire.Accepted {
			t.Fatalf("before the replacement: ack %q %q", ack, reason)
		}
		if code, _, _ := commitAndWait(t, conn); code != exitAllowed {
			t.Fatalf("code %d", code)
		}
		writeFiles(t, filepath.Dir(bin), map[string]string{"chooks.new": "v2"})
		if err := os.Rename(bin+".new", bin); err != nil {
			t.Fatal(err)
		}
		replaced := time.Now()
		late, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
		if err != nil {
			t.Fatal(err)
		}
		defer late.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("serve: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("daemon kept serving after the replacement")
		}
		t.Logf("from the rename to the daemon stop: %s", time.Since(replaced))
		_, _ = late.Write(hookwire.EncodeRequest(selfRequest(t, hookPreToolUse, env, `{}`)))
		_ = late.SetReadDeadline(time.Now().Add(time.Second))
		// Тишина — соединение осталось в очереди сокета; «в CLI: upgrade» —
		// демон застал обработчик первого запроса ещё не снявшим счётчик
		// (клиент получает код раньше) и дообслуживал: старый код не исполняет
		// и так, и так
		reply := make([]byte, 64)
		if n, err := late.Read(reply); n != 0 && !(reply[0] == hookwire.Fallback && bytes.Contains(reply[:n], []byte(upgradeReason))) {
			t.Fatalf("connection after the replacement answered: %q %v", reply[:n], err)
		} else if n == 0 && err == nil {
			t.Fatal("connection after the replacement: empty read without error")
		}
		if n := calls.Load(); n != 1 {
			t.Fatalf("runner called %d times, want 1", n)
		}
	})
	t.Run("after accept", func(t *testing.T) {
		var calls atomic.Int64
		srv := newServer(quietLogger(t))
		srv.run, srv.exe = countingRun(&calls), &fakeExe{answers: []bool{false, true}}
		freshStart(srv)
		path, done := serveDirect(t, srv)
		_, ack, reason := sendRequest(t, path, selfRequest(t, hookPreToolUse, absoluteEnv(t.TempDir()), `{}`))
		if ack != hookwire.Fallback || reason != "upgrade" {
			t.Fatalf("ack %q reason %q, want «в CLI: upgrade»", ack, reason)
		}
		if err := <-done; err != nil {
			t.Fatalf("serve: %v", err)
		}
		if n := calls.Load(); n != 0 {
			t.Fatalf("runner called %d times", n)
		}
	})
	t.Run("same file", func(t *testing.T) {
		bin := filepath.Join(t.TempDir(), "chooks")
		writeFiles(t, filepath.Dir(bin), map[string]string{"chooks": "v1"})
		w := watchFile(t, bin)
		if changed, reason := w.changed(); changed {
			t.Fatalf("unchanged file reported changed: %s", reason)
		}
		if err := os.Chtimes(bin, time.Now(), time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if changed, _ := w.changed(); !changed {
			t.Fatal("mtime change not detected")
		}
	})
}

// Сокет от systemd: без LISTEN_FDS и без --socket — ошибка; LISTEN_PID
// чужого процесса — переменные не для демона
func TestServeActivationArguments(t *testing.T) {
	home := t.TempDir()
	writeFiles(t, home, map[string]string{".config/chooks/config.yaml": serveConfig})
	for name, c := range map[string]struct {
		env  []string
		args []string
		want string
	}{
		"no socket":          {nil, []string{"serve"}, "не задан --socket"},
		"foreign LISTEN_PID": {[]string{"LISTEN_PID=1", "LISTEN_FDS=1"}, []string{"serve"}, "не задан --socket"},
	} {
		t.Run(name, func(t *testing.T) {
			run := runBinary(t, argvCLIHelper, c.args, append(absoluteEnv(home), c.env...), home, nil)
			if run.code == 0 || !strings.Contains(run.stderr, c.want) {
				t.Fatalf("code %d stderr %q", run.code, run.stderr)
			}
		})
	}
}

// Сценарий 4: строка stats пишется при старте и по сроку и разбирается
func TestServeStatsLine(t *testing.T) {
	ts := startServer(t, nil, func(s *server) { s.statsEvery = 50 * time.Millisecond })
	re := regexp.MustCompile(`msg="daemon: stats" rss_kb=(\d+) fds=(\d+) heap_live_bytes=(\d+) goroutines=(\d+) served=\d+ refused=\d+ in_flight=\d+ stuck=\d+ uptime_s=\d+ version=\S+`)
	if !waitFor(t, 5*time.Second, func() bool { return len(re.FindAllString(readFile(t, ts.log), -1)) >= 2 }) {
		t.Fatalf("no two stats lines; log:\n%s", readFile(t, ts.log))
	}
	m := re.FindStringSubmatch(readFile(t, ts.log))
	for i, name := range []string{"rss_kb", "fds", "heap_live_bytes", "goroutines"} {
		if n, err := strconv.ParseInt(m[i+1], 10, 64); err != nil || n <= 0 {
			t.Errorf("%s=%q", name, m[i+1])
		}
	}
}

// settingsWith — settings.json Claude Code с таймаутом подкоманды в секундах
func settingsWith(t *testing.T, dir, hook string, seconds int) {
	t.Helper()
	body := fmt.Sprintf(`{"hooks":{"PreToolUse":[{"matcher":"","hooks":[{"type":"command","command":"$HOME/bin/chooks-hook %s","timeout":%d}]}]}}`, hook, seconds)
	writeFiles(t, dir, map[string]string{"settings.json": body})
}

// Ревью 2б-1, п. 3: срок запроса — из settings.json запроса
// (CLAUDE_CONFIG_DIR, иначе HOME/.claude), таблица — только запасной
func TestRequestHookTimeoutFromSettings(t *testing.T) {
	home := t.TempDir()
	config := filepath.Join(home, "cfg")
	settingsWith(t, config, hookPreToolUse, 7)
	settingsWith(t, filepath.Join(home, ".claude"), hookPreToolUse, 9)
	for name, c := range map[string]struct {
		env  []string
		hook string
		want time.Duration
	}{
		"CLAUDE_CONFIG_DIR": {[]string{"HOME=" + home, "CLAUDE_CONFIG_DIR=" + config}, hookPreToolUse, 7 * time.Second},
		"HOME":              {[]string{"HOME=" + home}, hookPreToolUse, 9 * time.Second},
		"not in settings":   {[]string{"HOME=" + home}, hookStop, hookTimeouts[hookStop]},
		"no settings":       {[]string{"HOME=" + t.TempDir()}, hookPreToolUse, hookTimeouts[hookPreToolUse]},
		"no HOME":           {nil, hookStatusLine, hookTimeouts[hookStatusLine]},
	} {
		if got := requestHookTimeout(c.env, c.hook); got != c.want {
			t.Errorf("%s: %s, want %s", name, got, c.want)
		}
	}
	srv := newServer(quietLogger(t))
	start := time.Now().Add(-time.Second)
	srv.start = func(int) (time.Time, error) { return start, nil }
	env := []string{"HOME=" + home, "CLAUDE_CONFIG_DIR=" + config}
	if got, want := srv.requestDeadline(1, hookPreToolUse, env, time.Now()), start.Add(7*time.Second-serveDeadlineMargin); !got.Equal(want) {
		t.Errorf("deadline %s, want %s", got, want)
	}
}

// Ревью 2б-1, п. 4: обработчик, не слушающий отмену, держит слот, но не
// молча: прокси получает ответ к сроку + stuckGrace, в логе Error, следующий
// запрос — «в CLI: busy: 1 stuck»; вернулся — слот свободен
func TestServeStuckHandler(t *testing.T) {
	release := make(chan struct{})
	var first atomic.Bool
	ts := startServer(t, func(ctx context.Context, request hookctx.Context, hookType string) int {
		if first.CompareAndSwap(false, true) {
			<-release
			fmt.Fprint(request.Stdout, "after the answer")
		}
		return exitAllowed
	}, freshStart, func(s *server) { s.sem = make(chan struct{}, 1) })
	home := t.TempDir()
	settingsWith(t, filepath.Join(home, ".claude"), hookPreToolUse, 1)
	env := absoluteEnv(home)
	conn, ack, reason := sendRequest(t, ts.path, selfRequest(t, hookPreToolUse, env, `{}`))
	if ack != hookwire.Accepted {
		t.Fatalf("ack %q %q", ack, reason)
	}
	sent := time.Now()
	code, stdout, stderr := commitAndWait(t, conn)
	elapsed := time.Since(sent)
	if code != exitError || stdout != "" || stderr != stuckMessage {
		t.Fatalf("code %d stdout %q stderr %q", code, stdout, stderr)
	}
	if limit := time.Second - serveDeadlineMargin + serveStuckGrace + 500*time.Millisecond; elapsed > limit {
		t.Errorf("answered after %s, want within %s", elapsed, limit)
	}
	if log := readFile(t, ts.log); !strings.Contains(log, "level=ERROR") || !strings.Contains(log, "stuck past its deadline") {
		t.Errorf("no stuck line; log:\n%s", log)
	}
	if _, ack, reason := sendRequest(t, ts.path, selfRequest(t, hookPreToolUse, env, `{}`)); ack != hookwire.Fallback || reason != "busy: 1 stuck" {
		t.Fatalf("while stuck: ack %q reason %q", ack, reason)
	}
	close(release)
	if !waitFor(t, 5*time.Second, func() bool { return strings.Contains(readFile(t, ts.log), "stuck hook handler returned") }) {
		t.Fatalf("no return line; log:\n%s", readFile(t, ts.log))
	}
	conn, ack, _ = sendRequest(t, ts.path, selfRequest(t, hookPreToolUse, env, `{}`))
	if ack != hookwire.Accepted {
		t.Fatalf("after return: ack %q", ack)
	}
	if code, _, _ := commitAndWait(t, conn); code != exitAllowed {
		t.Fatalf("after return: code %d", code)
	}
}

// Ревью 2б-1, п. 8: 'C', пришедший после срока ожидания, не теряет вызов —
// демон к сроку ответил 'F' «late», прокси уйдёт в CLI; исполнения нет
func TestServeLateCommit(t *testing.T) {
	var calls atomic.Int64
	ts := startServer(t, countingRun(&calls), freshStart, func(s *server) { s.commitTimeout = 100 * time.Millisecond })
	conn, ack, _ := sendRequest(t, ts.path, selfRequest(t, hookPreToolUse, absoluteEnv(t.TempDir()), `{}`))
	if ack != hookwire.Accepted {
		t.Fatalf("ack %q", ack)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var b [1]byte
	if _, err := io.ReadFull(conn, b[:]); err != nil || b[0] != hookwire.Fallback {
		t.Fatalf("after the commit window: %q %v", b[:], err)
	}
	if reason, err := hookwire.ReadReason(conn); err != nil || reason != "late" {
		t.Fatalf("reason %q %v", reason, err)
	}
	_, _ = conn.Write([]byte{hookwire.Commit})
	waitFor(t, time.Second, func() bool { return strings.Contains(readFile(t, ts.log), "before commit") })
	if n := calls.Load(); n != 0 {
		t.Fatalf("runner called %d times", n)
	}
}

// durationLines — строки chooks-hooks.tsv, разбитые на столбцы
func durationLines(t *testing.T, home string) [][]string {
	t.Helper()
	var out [][]string
	for _, line := range lines(readFile(t, filepath.Join(home, ".claude", "logs", durationsFile))) {
		out = append(out, strings.Split(line, "\t"))
	}
	return out
}

// startKilled запускает бинарь тестов в режиме helper, ждёт старта заглушки
// webtext (pid в home/webtext.pid) и шлёт процессу SIGTERM — как Claude Code
// по таймауту хука
func startKilled(t *testing.T, helper string, args, env []string, home, dir string) int {
	t.Helper()
	exe, err := selfExe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, args...)
	cmd.Env = append(slices.Clone(env), helper+"=1")
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(webFetchForbidden)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if !waitFor(t, 20*time.Second, func() bool { return readFile(t, filepath.Join(home, "webtext.pid")) != "" }) {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal("webtext stub not started")
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	err = cmd.Wait()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("after SIGTERM: %v", err)
	}
	return exitErr.ExitCode()
}

func killedHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	writeFiles(t, home, map[string]string{
		".config/chooks/config.yaml": serveConfig,
		"bin/webtext":                "#!/bin/sh\necho $$ > \"$HOME/webtext.pid.tmp\" && mv \"$HOME/webtext.pid.tmp\" \"$HOME/webtext.pid\"\nexec sleep 30\n",
	})
	return home
}

// Ревью 2б-1, п. 2: SIGTERM прокси после 'C' даёт в chooks-hooks.tsv тот же
// статус, что SIGTERM CLI (killed), с исполнителем daemon; код прокси — 143,
// как у CLI
func TestProxySIGTERMRecordsKilled(t *testing.T) {
	cliHome := killedHome(t)
	cliCode := startKilled(t, argvCLIHelper, []string{hookWebFetchFallback}, absoluteEnv(cliHome), cliHome, cliHome)
	var cliStatus string
	if !waitFor(t, 5*time.Second, func() bool {
		rows := durationLines(t, cliHome)
		if len(rows) == 1 && len(rows[0]) == 6 {
			cliStatus = rows[0][5]
			return true
		}
		return false
	}) {
		t.Fatalf("CLI tsv: %v", durationLines(t, cliHome))
	}
	ts := startServer(t, nil, freshStart)
	for attempt := 1; ; attempt++ {
		home := killedHome(t)
		runs := filepath.Join(home, "cli-runs")
		code := startKilled(t, proxyHelper, []string{hookWebFetchFallback}, proxyEnv(t, absoluteEnv(home), ts.path, runs), home, home)
		if fallbacks := lines(readFile(t, runs)); len(fallbacks) > 0 {
			if attempt == 3 {
				t.Fatalf("proxy went to CLI 3 times: %v", fallbacks)
			}
			continue
		}
		if code != cliCode || code != hookproxy.KilledExitCode {
			t.Fatalf("proxy code %d, CLI %d", code, cliCode)
		}
		var row []string
		if !waitFor(t, 5*time.Second, func() bool {
			rows := durationLines(t, home)
			if len(rows) == 1 {
				row = rows[0]
				return true
			}
			return false
		}) {
			t.Fatalf("daemon tsv: %v; log:\n%s", durationLines(t, home), readFile(t, ts.log))
		}
		if len(row) != 8 || row[5] != cliStatus || cliStatus != statusKilled || row[6] != executorDaemon || row[7] != "" {
			t.Fatalf("daemon row %q, CLI status %q", row, cliStatus)
		}
		if !waitFor(t, 3*time.Second, func() bool { return strings.Contains(readFile(t, ts.log), "proxy got SIGTERM") }) {
			t.Errorf("daemon log lacks the SIGTERM line:\n%s", readFile(t, ts.log))
		}
		return
	}
}

// Ревью 2б-1, п. 5: паника горутины запроса, которую запрос ждёт, — ответ
// как у CLI (код 2, «panic: …» с исходным стеком); после ответа — строка
// Error в лог, процесс демона жив
func TestServeGoroutinePanic(t *testing.T) {
	late := make(chan struct{})
	ts := startServer(t, func(ctx context.Context, request hookctx.Context, hookType string) int {
		data, _ := io.ReadAll(request.Stdin)
		switch string(data) {
		case "waited":
			task := goRequest(ctx, func() { panic("in goroutine") })
			task.wait()
		case "orphan":
			task := goRequest(ctx, func() {
				<-late
				panic("after the answer")
			})
			task.leave()
			fmt.Fprint(request.Stdout, "answered")
		}
		return exitAllowed
	}, freshStart)
	env := absoluteEnv(t.TempDir())
	conn, ack, _ := sendRequest(t, ts.path, selfRequest(t, hookPreToolUse, env, "waited"))
	if ack != hookwire.Accepted {
		t.Fatalf("ack %q", ack)
	}
	code, _, stderr := commitAndWait(t, conn)
	if code != exitBlocked || !strings.HasPrefix(stderr, "panic: in goroutine\n\ngoroutine ") || !strings.Contains(stderr, "TestServeGoroutinePanic.func") {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
	conn, ack, _ = sendRequest(t, ts.path, selfRequest(t, hookPreToolUse, env, "orphan"))
	if ack != hookwire.Accepted {
		t.Fatalf("ack %q", ack)
	}
	if code, stdout, _ := commitAndWait(t, conn); code != exitAllowed || stdout != "answered" {
		t.Fatalf("orphan: code %d stdout %q", code, stdout)
	}
	close(late)
	if !waitFor(t, 3*time.Second, func() bool {
		return strings.Contains(readFile(t, ts.log), `panic in hook goroutine after its request answered" hook=pre-tool-use`)
	}) {
		t.Fatalf("no orphan line; log:\n%s", readFile(t, ts.log))
	}
}

// Ревью 2б-1, п. 7: каждая переменная-путь таблицы с относительным значением —
// «в CLI» со своим именем; таблица — ровно этот список
func TestDaemonAbsoluteEnvTable(t *testing.T) {
	keys := []string{"HOME", "XDG_RUNTIME_DIR", "TMPDIR", "XDG_STATE_HOME", "CLAUDE_CONFIG_DIR", "MODELS_APPROVED",
		"CLAUDE_CODE_TMPDIR", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "ZONEINFO", "CLAUDE_PROJECT_DIR"}
	if got := slices.Sorted(slices.Values(daemonAbsoluteEnv)); !slices.Equal(got, slices.Sorted(slices.Values(keys))) {
		t.Fatalf("daemonAbsoluteEnv %v, want %v", got, keys)
	}
	env := absoluteEnv(t.TempDir())
	if reason := daemonEnvRefusal(env); reason != "" {
		t.Fatalf("absolute env refused: %s", reason)
	}
	for _, key := range keys {
		if got := daemonEnvRefusal(replaceEnv(env, key, "rel/dir")); got != key+" relative" {
			t.Errorf("%s=rel/dir: %q", key, got)
		}
	}
}

// fallbackMode — неисправность демона сценария 7 и причина, с которой прокси
// уходит в CLI
type fallbackMode struct {
	name   string
	socket func(t *testing.T) string
	reason func(string) bool
	// threshold — ключ daemon.fallback_alert; 0 — без ключа (10)
	threshold int
	// expired — в состоянии вызовы и уведомление старше часа
	expired bool
}

// Сценарий 7: демон остановлен, убит или принимает без ответа — каждый из
// N+1 вызовов отдаёт верный результат CLI и строку tsv «cli» с причиной;
// уведомление ровно одно и только после N+1-го; вызовы старше часа не в счёт
func TestCLIFallbackAlert(t *testing.T) {
	baseHome := t.TempDir()
	writeFiles(t, baseHome, map[string]string{".config/chooks/config.yaml": serveConfig})
	base := runBinary(t, argvCLIHelper, []string{hookPreToolUse}, absoluteEnv(baseHome), baseHome, []byte(preToolUseLS))
	if rows := durationLines(t, baseHome); len(rows) != 1 || len(rows[0]) != 6 {
		t.Fatalf("plain CLI tsv %q, want one row of 6 columns", rows)
	}
	modes := []fallbackMode{
		{name: "stopped", socket: func(t *testing.T) string { return filepath.Join(socketDir(t), "none.sock") },
			reason: func(r string) bool { return r == "connect: no such file or directory" }, expired: true},
		{name: "killed", socket: func(t *testing.T) string {
			path := filepath.Join(socketDir(t), "dead.sock")
			listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			listener.SetUnlinkOnClose(false)
			_ = listener.Close()
			return path
		}, reason: func(r string) bool { return r == "connect: connection refused" }, threshold: 3},
		{name: "accepts without answering", socket: func(t *testing.T) string {
			path := filepath.Join(socketDir(t), "hung.sock")
			listener, err := listenSocket(path)
			if err != nil {
				t.Fatal(err)
			}
			var held []net.Conn
			var mu sync.Mutex
			go func() {
				for {
					conn, err := listener.Accept()
					if err != nil {
						return
					}
					mu.Lock()
					held = append(held, conn)
					mu.Unlock()
				}
			}()
			t.Cleanup(func() {
				_ = listener.Close()
				mu.Lock()
				defer mu.Unlock()
				for _, c := range held {
					_ = c.Close()
				}
			})
			return path
		}, reason: func(r string) bool { return strings.HasPrefix(r, "ack: ") }, threshold: 3},
	}
	for _, mode := range modes {
		t.Run(mode.name, func(t *testing.T) {
			home := t.TempDir()
			config := serveConfig
			n := core.DefaultFallbackAlert
			if mode.threshold > 0 {
				n = mode.threshold
				config += fmt.Sprintf("daemon:\n  fallback_alert: %d\n", n)
			}
			writeFiles(t, home, map[string]string{".config/chooks/config.yaml": config})
			if mode.expired {
				old := time.Now().Add(-61 * time.Minute)
				var events []string
				for range n + 5 {
					events = append(events, fmt.Sprintf(`{"at":%q,"hook":"stop","reason":"old"}`, old.Format(time.RFC3339Nano)))
				}
				writeFiles(t, filepath.Join(home, "state", "chooks"), map[string]string{
					fallbackwatch.FileName: fmt.Sprintf(`{"events":[%s],"alerted_at":%q}`, strings.Join(events, ","), old.Format(time.RFC3339Nano)),
				})
			}
			alerts := filepath.Join(home, "alerts")
			runs := filepath.Join(home, "cli-runs")
			env := append(proxyEnv(t, absoluteEnv(home), mode.socket(t), runs), alertLogEnv+"="+alerts)
			for i := 1; i <= n+2; i++ {
				run := runBinary(t, proxyHelper, []string{hookPreToolUse}, env, home, []byte(preToolUseLS))
				if run.code != base.code || run.stdout != base.stdout || run.stderr != base.stderr {
					t.Fatalf("call %d: code %d stdout %q stderr %q, plain CLI code %d stdout %q stderr %q",
						i, run.code, run.stdout, run.stderr, base.code, base.stdout, base.stderr)
				}
				reasons := lines(readFile(t, runs))
				if len(reasons) != i || !mode.reason(reasons[i-1]) {
					t.Fatalf("call %d: fallback reasons %q", i, reasons)
				}
				rows := durationLines(t, home)
				if len(rows) != i || len(rows[i-1]) != 8 || rows[i-1][6] != executorCLI || rows[i-1][7] != reasons[i-1] {
					t.Fatalf("call %d: tsv rows %q", i, rows)
				}
				// Решение об уведомлении CLI пишет в лог до выхода, доставка —
				// отдельным процессом: до N+1 решений нет, с N+1 — одно
				decided := strings.Count(readFile(t, filepath.Join(home, ".claude", "logs", "chooks.log")), `msg="daemon fallback alert"`)
				if want := min(max(i-n, 0), 1); decided != want {
					t.Fatalf("call %d of N=%d: %d alert decisions, want %d", i, n, decided, want)
				}
				if i == n+1 && !waitFor(t, 5*time.Second, func() bool { return len(lines(readFile(t, alerts))) == 1 }) {
					t.Fatalf("alert decided but not delivered; log:\n%s", readFile(t, filepath.Join(home, ".claude", "logs", "chooks.log")))
				}
			}
			got := lines(readFile(t, alerts))
			if len(got) != 1 {
				t.Fatalf("%d alerts, want 1: %q", len(got), got)
			}
			if !strings.Contains(got[0], fmt.Sprintf("За час %d вызовов", n+1)) || !strings.Contains(got[0], reasonsOf(t, runs)) || strings.Contains(got[0], "old") {
				t.Fatalf("alert %q", got[0])
			}
		})
	}
}

// reasonsOf — причина вызовов (у режима она одна)
func reasonsOf(t *testing.T, runs string) string {
	t.Helper()
	reasons := lines(readFile(t, runs))
	if len(reasons) == 0 {
		t.Fatal("no fallback reasons")
	}
	return reasons[0] + " ×"
}

// quietLogger — лог демона в файл теста
func quietLogger(t *testing.T) core.Logger {
	t.Helper()
	logger, err := core.NewLoggerTo(core.LoggerConfig{Level: "debug", Output: "file", LogFile: filepath.Join(t.TempDir(), "daemon.log")}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logger.Close() })
	return logger
}

// systemdUser — env для systemctl и systemd-run --user: настоящий
// XDG_RUNTIME_DIR (TestMain подменил его на временный); нет менеджера —
// пропуск
func systemdUser(t *testing.T) []string {
	t.Helper()
	if _, err := exec.LookPath("systemd-run"); err != nil {
		t.Skip("systemd-run not found: the supervisor stub covers activation (TestServeUpdateAfterInstall)")
	}
	env := append(withoutEnv(os.Environ(), "XDG_RUNTIME_DIR"), fmt.Sprintf("XDG_RUNTIME_DIR=/run/user/%d", os.Getuid()))
	cmd := exec.Command("systemctl", "--user", "is-system-running")
	cmd.Env = env
	out, _ := cmd.Output()
	if state := strings.TrimSpace(string(out)); state != "running" && state != "degraded" {
		t.Skipf("systemd --user is %q: the supervisor stub covers activation", state)
	}
	return env
}

// unitProperties — «ключ=значение» секции section unit-файла
// configs/systemd/name, кроме ключей skip: тест поднимает unit с теми же
// свойствами, что ставит graft install
func unitProperties(t *testing.T, name, section string, skip ...string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "configs", "systemd", name))
	if err != nil {
		t.Fatal(err)
	}
	var props []string
	current := ""
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "" || strings.HasPrefix(line, "#"):
		case strings.HasPrefix(line, "["):
			current = strings.Trim(line, "[]")
		case current == section:
			key, _, ok := strings.Cut(line, "=")
			if !ok {
				t.Fatalf("%s: line %q is not key=value", name, line)
			}
			if !slices.Contains(skip, key) {
				props = append(props, line)
			}
		}
	}
	return props
}

// processAlive — процесс pid жив и это не зомби
func processAlive(pid int) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	_, rest, ok := strings.Cut(string(data), ") ")
	return ok && !strings.HasPrefix(rest, "Z")
}

// Unit systemd на деле: временный сокет-юнит со своим именем и сокетом в
// каталоге теста (systemd-run --socket-property), свойства — из
// configs/systemd/chooks.service и chooks.socket, сервис — копия бинаря
// тестов. Первый запрос исполняет v1; после замены файла v1 выходит, systemd
// поднимает v2 на том же сокете, следующий запрос исполняет v2, «connect:»
// и «ack:» не бывает. Наблюдатель уведомления, запущенный обработчиком v1 через
// desktop.StartDetached, переживает выход v1 (ревью 8f10b59, п. 1:
// KillMode=control-group убил бы его вместе с демоном). Настоящий
// chooks.service не трогается
func TestSystemdSocketUnit(t *testing.T) {
	env := systemdUser(t)
	home := t.TempDir()
	writeFiles(t, home, map[string]string{".config/chooks/config.yaml": strings.Replace(serveConfig,
		"notifier: {enabled: false}", "notifier: {enabled: true, desktop: true, sound: false}", 1)})
	exe := filepath.Join(home, "bin", "chooks")
	installVersion(t, exe, "v1")
	socket := filepath.Join(socketDir(t), "chooks.sock")
	unit := fmt.Sprintf("chooks-test-%d-%d", os.Getpid(), time.Now().UnixNano()%1_000_000)
	args := []string{"--user", "--unit=" + unit, "--quiet", "--collect", "--socket-property=ListenStream=" + socket}
	for _, prop := range unitProperties(t, "chooks.socket", "Socket", "ListenStream") {
		args = append(args, "--socket-property="+prop)
	}
	for _, prop := range append(unitProperties(t, "chooks.service", "Unit", "Description", "Requires", "After"),
		unitProperties(t, "chooks.service", "Service", "ExecStart")...) {
		args = append(args, "-p", prop)
	}
	for _, kv := range append(absoluteEnv(home), daemonHelper+"=1", daemonStartsEnv+"="+filepath.Join(home, "starts")) {
		args = append(args, "--setenv="+kv)
	}
	args = append(args, "--working-directory="+home, exe, "serve")
	run := exec.Command("systemd-run", args...)
	run.Env = env
	if out, err := run.CombinedOutput(); err != nil {
		t.Fatalf("systemd-run --user: %v: %s", err, out)
	}
	alerts, release := filepath.Join(home, "alerts"), filepath.Join(home, "release")
	t.Cleanup(func() {
		writeFiles(t, home, map[string]string{"release": ""})
		stop := exec.Command("systemctl", "--user", "stop", unit+".socket", unit+".service")
		stop.Env = env
		if out, err := stop.CombinedOutput(); err != nil {
			t.Errorf("stop %s: %v: %s", unit, err, out)
		}
		reset := exec.Command("systemctl", "--user", "reset-failed", unit+".socket", unit+".service")
		reset.Env = env
		_ = reset.Run()
	})
	if info, err := os.Lstat(socket); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("socket %v %v, want mode 0600", info, err)
	}
	log := filepath.Join(home, ".claude", "logs", "chooks.log")
	runs := filepath.Join(home, "cli-runs")
	proxy := append(proxyEnv(t, absoluteEnv(home), socket, runs), alertLogEnv+"="+alerts, alertHoldEnv+"="+release)
	serveAs := func(version string) {
		t.Helper()
		ok := waitFor(t, 20*time.Second, func() bool {
			r := runBinary(t, proxyHelper, []string{hookPreToolUse}, proxy, home, []byte(preToolUseLS))
			if r.code != exitAllowed {
				t.Errorf("code %d stderr %q", r.code, r.stderr)
			}
			got := servedBy(readFile(t, log), hookPreToolUse)
			return len(got) > 0 && got[len(got)-1] == version
		})
		if !ok {
			t.Fatalf("no request served by %s; starts:\n%s\nlog:\n%s", version, readFile(t, filepath.Join(home, "starts")), readFile(t, log))
		}
	}
	serveAs("v1")
	// Уведомление от v1: его наблюдатель — фоновый процесс хука
	notify := runBinary(t, proxyHelper, []string{hookNotification}, proxy, home,
		[]byte(`{"session_id":"n1","hook_event_name":"Notification","notification_type":"permission_prompt","message":"Claude needs your permission"}`))
	if got := servedBy(readFile(t, log), hookNotification); notify.code != exitAllowed || !slices.Equal(got, []string{"v1"}) {
		t.Fatalf("notification code %d stderr %q served by %v", notify.code, notify.stderr, got)
	}
	watcher := 0
	if !waitFor(t, 10*time.Second, func() bool {
		for _, line := range lines(readFile(t, alerts)) {
			if pid, ok := strings.CutPrefix(line, "pid "); ok {
				watcher, _ = strconv.Atoi(pid)
			}
		}
		return watcher > 0
	}) {
		t.Fatalf("notification watcher not started; alerts %q; log:\n%s", lines(readFile(t, alerts)), readFile(t, log))
	}
	v1 := servedPID(t, filepath.Join(home, "starts"), "v1")
	installVersion(t, exe, "v2")
	// Проверяем обновление несколькими одновременными вызовами. При обычном
	// запуске v2 на очереди сокета возможен единичный таймаут ack (100 мс)
	// под нагрузкой. При Restart=always RestartSec=200 мс держит сервис
	// недоступным целое окно, поэтому в CLI уходят сразу несколько вызовов.
	const updateProbes = 3
	probes := make(chan proxyRun, updateProbes)
	for range updateProbes {
		go func() {
			probes <- runBinary(t, proxyHelper, []string{hookPreToolUse}, proxy, home, []byte(preToolUseLS))
		}()
	}
	for range updateProbes {
		result := <-probes
		if result.code != exitAllowed {
			t.Errorf("update probe code %d stderr %q", result.code, result.stderr)
		}
	}
	serveAs("v2")
	if !waitFor(t, 10*time.Second, func() bool { return !processAlive(v1) }) {
		t.Fatalf("v1 (pid %d) still alive after v2 served", v1)
	}
	if !processAlive(watcher) {
		t.Fatalf("notification watcher %d died with v1 (pid %d)", watcher, v1)
	}
	if got := servedBy(readFile(t, log), hookPreToolUse); slices.Contains(got[slices.Index(got, "v2"):], "v1") {
		t.Errorf("v1 served after v2 started: %v", got)
	}
	var unavailable []string
	for _, reason := range lines(readFile(t, runs)) {
		if strings.HasPrefix(reason, "connect:") {
			t.Errorf("socket unavailable during the update: %q", reason)
		}
		if hookproxy.Unavailable(reason) {
			unavailable = append(unavailable, reason)
		}
	}
	// Один ack может истечь под нагрузкой; несколько одновременных уходов
	// означают, что v2 не поднялся на очереди сокета, а ждёт RestartSec.
	if len(unavailable) > 1 {
		t.Errorf("calls left to CLI as daemon unavailable during the update: %q; want at most one transient ack timeout", unavailable)
	}
	writeFiles(t, home, map[string]string{"release": ""})
	if !waitFor(t, 10*time.Second, func() bool { return !processAlive(watcher) }) {
		t.Errorf("notification watcher %d did not exit after release", watcher)
	}
	t.Logf("starts:\n%s\nfallbacks: %q; counted as daemon unavailable: %d", readFile(t, filepath.Join(home, "starts")), lines(readFile(t, runs)), len(unavailable))
}

// servedPID — pid запуска version из журнала стартов
func servedPID(t *testing.T, starts, version string) int {
	t.Helper()
	for _, line := range lines(readFile(t, starts)) {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == "start" && fields[1] == version {
			pid, err := strconv.Atoi(fields[2])
			if err != nil {
				t.Fatal(err)
			}
			return pid
		}
	}
	t.Fatalf("no start of %s in %q", version, readFile(t, starts))
	return 0
}
