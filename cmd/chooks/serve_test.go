package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/aiseeq/akit/proc"
	"github.com/spf13/pflag"

	"github.com/aiseeq/chooks/internal/core"
	"github.com/aiseeq/chooks/internal/fallbackwatch"
	"github.com/aiseeq/chooks/internal/hookctx"
	"github.com/aiseeq/chooks/internal/hookproxy"
	"github.com/aiseeq/chooks/internal/hookwire"
	"github.com/aiseeq/chooks/internal/modellaunch"
	"github.com/aiseeq/chooks/internal/tools/notifier"
)

// Режимы бинаря тестов (TestMain): CLI chooks с argv запуска, прокси
// chooks-hook и CLI, упавший паникой
const (
	argvCLIHelper = "CHOOKS_TEST_ARGV_CLI"
	proxyHelper   = "CHOOKS_TEST_PROXY"
	panicHelper   = "CHOOKS_TEST_PANIC"
)

func runServeHelper() (int, bool) {
	switch {
	// Уведомление (desktop.DeliverInBackgroundForRequest): бинарь тестов не
	// доставляет его, а пишет в журнал теста
	case len(os.Args) > 1 && os.Args[1] == notifier.WatchCommand:
		return recordTestAlert(), true
	case os.Getenv(daemonHelper) == "1":
		_ = os.Unsetenv(daemonHelper)
		return runTestDaemon(), true
	case os.Getenv(argvCLIHelper) == "1":
		_ = os.Unsetenv(argvCLIHelper)
		if os.Getenv("CHOOKS_TEST_NIGHT_LOCKED") == "1" {
			nightProbes.locked = func(context.Context) (bool, error) { return true, nil }
		}
		return runCLI(), true
	case os.Getenv(proxyHelper) == "1":
		_ = os.Unsetenv(proxyHelper)
		return hookproxy.Main(os.Args), true
	case os.Getenv(panicHelper) == "1":
		panic("boom")
	}
	return 0, false
}

func TestServeHelpKeepsStatsSentenceOnOneLine(t *testing.T) {
	cmd := newServeCmd()
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs([]string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Раз в час — строка stats в лог") {
		t.Fatalf("serve --help split the stats sentence:\n%s", output.String())
	}
}

func testBinary(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe
}

// socketDir — каталог сокета с коротким путём: sun_path не длиннее 107 байт
func socketDir(t *testing.T) string {
	t.Helper()
	base := testRoot
	if base == "" || len(base) > 60 {
		base = os.TempDir()
	}
	dir, err := os.MkdirTemp(base, "s")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if len(dir) > 90 {
		t.Fatalf("socket dir too long: %s", dir)
	}
	return dir
}

type testServer struct {
	path string
	log  string
	srv  *server
}

type serverOption func(*server)

// startServer поднимает демон в процессе теста на временном сокете
func startServer(t *testing.T, run hookRunner, options ...serverOption) *testServer {
	t.Helper()
	path := filepath.Join(socketDir(t), "d.sock")
	logPath := filepath.Join(t.TempDir(), "daemon.log")
	logger, err := core.NewLoggerTo(core.LoggerConfig{Level: "debug", Output: "file", LogFile: logPath}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	srv := newServer(logger)
	if run != nil {
		srv.run = run
	}
	for _, option := range options {
		option(srv)
	}
	listener, err := listenSocket(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.serve(ctx, listener) }()
	// Строка stats при старте — serve готов: ожидание accept и горутина stats
	// уже есть, база горутин и fd тестов не плывёт
	if !waitFor(t, 10*time.Second, func() bool { return strings.Contains(readFile(t, logPath), `msg="daemon: stats"`) }) {
		t.Fatalf("daemon did not start; log:\n%s", readFile(t, logPath))
	}
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("serve: %v", err)
			}
		case <-time.After(20 * time.Second):
			t.Error("serve did not stop")
		}
		_ = logger.Close()
	})
	return &testServer{path: path, log: logPath, srv: srv}
}

// freshStart — старт прокси «сейчас»: клиент — процесс теста, запущенный
// давно, и срок от его старта уже вышел бы
func freshStart(s *server) { s.start = func(int) (time.Time, error) { return time.Now(), nil } }

// sendRequest шлёт запрос как прокси и читает ответ приёма: 'A' или 'F' с
// причиной
func sendRequest(t *testing.T, path string, request hookwire.Request) (*net.UnixConn, byte, string) {
	t.Helper()
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := conn.Write(hookwire.EncodeRequest(request)); err != nil {
		t.Fatal(err)
	}
	ack, reason := readAck(t, conn)
	return conn, ack, reason
}

func readAck(t *testing.T, conn *net.UnixConn) (byte, string) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var b [1]byte
	if _, err := io.ReadFull(conn, b[:]); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if b[0] != hookwire.Fallback {
		return b[0], ""
	}
	reason, err := hookwire.ReadReason(conn)
	if err != nil {
		t.Fatalf("reason: %v", err)
	}
	return b[0], reason
}

func commitAndWait(t *testing.T, conn *net.UnixConn) (int, string, string) {
	t.Helper()
	if _, err := conn.Write([]byte{hookwire.Commit}); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	var b [1]byte
	if _, err := io.ReadFull(conn, b[:]); err != nil || b[0] != hookwire.Result {
		t.Fatalf("result: %v %q", err, b[:])
	}
	code, stdout, stderr, err := hookwire.ReadResult(conn)
	if err != nil {
		t.Fatal(err)
	}
	return code, string(stdout), string(stderr)
}

// selfRequest — запрос от процесса теста: его pid, ppid и cwd
func selfRequest(t *testing.T, hook string, env []string, stdin string) hookwire.Request {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return hookwire.Request{Subcommand: hook, Env: env, Cwd: cwd, PID: os.Getpid(), PPID: os.Getppid(), Stdin: []byte(stdin)}
}

func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o600)
		if strings.HasPrefix(name, "bin/") || strings.Contains(name, "/bin/") || strings.HasSuffix(name, ".sh") {
			mode = 0o700
		}
		if err := os.WriteFile(path, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
}

func absoluteEnv(home string) []string {
	return []string{
		"HOME=" + home, "XDG_RUNTIME_DIR=" + filepath.Join(home, "run"), "XDG_STATE_HOME=" + filepath.Join(home, "state"),
		"CLAUDE_CONFIG_DIR=" + filepath.Join(home, ".claude"), "TMPDIR=" + filepath.Join(home, "tmp"),
		"PATH=" + filepath.Join(home, "bin") + ":/usr/bin:/bin", "LANG=C.UTF-8",
		"DBUS_SESSION_BUS_ADDRESS=unix:path=" + filepath.Join(home, "no-bus"),
		"DBUS_SYSTEM_BUS_ADDRESS=unix:path=" + filepath.Join(home, "no-system-bus"),
	}
}

func replaceEnv(env []string, key, value string) []string {
	out := withoutEnv(env, key)
	if value == "\x00" {
		return out
	}
	return append(out, key+"="+value)
}

// countingRun — исполнитель, который только считает вызовы
func countingRun(calls *atomic.Int64) hookRunner {
	return func(ctx context.Context, request hookctx.Context, hookType string) int {
		calls.Add(1)
		return exitAllowed
	}
}

// timeoutFlagError — текст ошибки pflag на `--timeout bad`: он меняется от
// версии pflag (v1.1.0 — «must be a duration like…»), а тест проверяет, что
// демон отдаёт ошибку разбора argv в CLI, а не её текст
func timeoutFlagError(t *testing.T) string {
	t.Helper()
	flags := pflag.NewFlagSet("timeout", pflag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Duration("timeout", 0, "")
	err := flags.Parse([]string{"--timeout", "bad"})
	if err == nil {
		t.Fatal("pflag принял --timeout bad")
	}
	return err.Error()
}

// Сценарий 8 и проверки до «принято»: каждый запрос, который демон исполнил
// бы не так, как CLI, или пришедший не от своего процесса, получает «в CLI» с
// причиной, и исполнитель не вызывается
func TestServeRefusesBeforeAccept(t *testing.T) {
	var calls atomic.Int64
	ts := startServer(t, countingRun(&calls), freshStart)
	home := t.TempDir()
	fifo := filepath.Join(home, "t.jsonl")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	agentFifo := filepath.Join(home, "main", "subagents", "agent-a1.jsonl")
	if err := os.MkdirAll(filepath.Dir(agentFifo), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(agentFifo, 0o600); err != nil {
		t.Fatal(err)
	}
	// Чужой процесс того же uid с тем же родителем и cwd: проходит сверку
	// ppid и cwd, расходится только pid
	other := exec.Command("/bin/sleep", "30")
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := proc.StopChild(other.Process.Pid, 0, func() error { return other.Process.Kill() }); err != nil {
			t.Errorf("sleep %d не остановлен: %v", other.Process.Pid, err)
		}
		_ = other.Wait()
	})
	env := absoluteEnv(home)
	input := `{"session_id":"s","hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"ls"}}`
	cases := []struct {
		name   string
		edit   func(*hookwire.Request)
		reason string
	}{
		{"pid", func(r *hookwire.Request) { r.PID = other.Process.Pid; r.PPID = os.Getpid() }, "pid"},
		{"ppid", func(r *hookwire.Request) { r.PPID++ }, "ppid"},
		{"subcommand", func(r *hookwire.Request) { r.Subcommand = "version" }, "subcommand"},
		{"extra arg", func(r *hookwire.Request) { r.Args = []string{"extra"} }, "argv: positional"},
		{"unknown flag", func(r *hookwire.Request) { r.Subcommand, r.Args = hookNightHint, []string{"--probe"} }, "argv: unknown flag: --probe"},
		{"bad timeout", func(r *hookwire.Request) { r.Args = []string{"--timeout", "bad"} }, "argv: " + timeoutFlagError(t)},
		{"relative config", func(r *hookwire.Request) { r.Args = []string{"--config", "c.yaml"} }, "config: relative"},
		{"relative HOME", func(r *hookwire.Request) { r.Env = replaceEnv(r.Env, "HOME", "home") }, "env: HOME relative"},
		{"relative XDG_RUNTIME_DIR", func(r *hookwire.Request) { r.Env = replaceEnv(r.Env, "XDG_RUNTIME_DIR", "run") }, "env: XDG_RUNTIME_DIR relative"},
		{"relative MODELS_APPROVED", func(r *hookwire.Request) { r.Env = replaceEnv(r.Env, "MODELS_APPROVED", "a") }, "env: MODELS_APPROVED relative"},
		{"relative CLAUDE_CODE_TMPDIR", func(r *hookwire.Request) { r.Env = replaceEnv(r.Env, "CLAUDE_CODE_TMPDIR", "t") }, "env: CLAUDE_CODE_TMPDIR relative"},
		{"PATH empty component", func(r *hookwire.Request) { r.Env = replaceEnv(r.Env, "PATH", "/usr/bin::/bin") }, "env: PATH empty component"},
		{"PATH trailing colon", func(r *hookwire.Request) { r.Env = replaceEnv(r.Env, "PATH", "/usr/bin:") }, "env: PATH empty component"},
		{"PATH relative", func(r *hookwire.Request) { r.Env = replaceEnv(r.Env, "PATH", "bin:/usr/bin") }, "env: PATH relative component"},
		{"GODEBUG execerrdot", func(r *hookwire.Request) { r.Env = replaceEnv(r.Env, "GODEBUG", "execerrdot=0") }, "env: GODEBUG execerrdot"},
		{"D-Bus relative path", func(r *hookwire.Request) {
			r.Env = replaceEnv(r.Env, "DBUS_SESSION_BUS_ADDRESS", "unix:abstract=x;unix:path=rel/bus")
		}, "env: DBUS_SESSION_BUS_ADDRESS relative"},
		{"D-Bus relative escaped", func(r *hookwire.Request) {
			r.Env = replaceEnv(r.Env, "DBUS_SYSTEM_BUS_ADDRESS", "unix:path=%72el")
		}, "env: DBUS_SYSTEM_BUS_ADDRESS relative"},
		{"D-Bus relative noncefile", func(r *hookwire.Request) {
			r.Env = replaceEnv(r.Env, "DBUS_SESSION_BUS_ADDRESS", "nonce-tcp:host=localhost,port=1,noncefile=n")
		}, "env: DBUS_SESSION_BUS_ADDRESS relative"},
		{"cwd relative", func(r *hookwire.Request) { r.Cwd = "." }, "cwd: not absolute"},
		{"cwd deleted", func(r *hookwire.Request) { r.Cwd = "" }, "cwd: not absolute"},
		{"cwd elsewhere", func(r *hookwire.Request) { r.Cwd = home }, "cwd: not the proxy cwd"},
		{"transcript FIFO", func(r *hookwire.Request) {
			r.Stdin = []byte(`{"session_id":"s","transcript_path":"` + fifo + `"}`)
		}, "transcript: not a regular file"},
		{"agent transcript FIFO", func(r *hookwire.Request) {
			r.Stdin = []byte(`{"session_id":"s","agent_id":"a1","transcript_path":"` + filepath.Join(home, "main.jsonl") + `"}`)
		}, "transcript: not a regular file"},
		{"transcript directory", func(r *hookwire.Request) {
			r.Stdin = []byte(`{"session_id":"s","transcript_path":"` + home + `"}`)
		}, "transcript: not a regular file"},
		{"transcript relative", func(r *hookwire.Request) {
			r.Stdin = []byte(`{"session_id":"s","transcript_path":"t.jsonl"}`)
		}, "transcript: relative"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			request := selfRequest(t, hookPreToolUse, env, input)
			c.edit(&request)
			start := time.Now()
			_, ack, reason := sendRequest(t, ts.path, request)
			if ack != hookwire.Fallback || reason != c.reason {
				t.Fatalf("ack %q reason %q, want «в CLI: %s»", ack, reason, c.reason)
			}
			if elapsed := time.Since(start); elapsed > time.Second {
				t.Errorf("refusal took %s", elapsed)
			}
		})
	}
	accepted := 0
	// Допустимые формы проходят: абсолютные пути, абстрактный D-Bus, пустые
	// значения, transcript без файла
	for name, edit := range map[string]func(*hookwire.Request){
		"absolute": func(*hookwire.Request) {},
		"abstract D-Bus": func(r *hookwire.Request) {
			r.Env = replaceEnv(r.Env, "DBUS_SESSION_BUS_ADDRESS", "unix:abstract=/tmp/x;unix:path=@y")
		},
		"empty HOME": func(r *hookwire.Request) { r.Env = replaceEnv(r.Env, "HOME", "") },
		"no PATH":    func(r *hookwire.Request) { r.Env = replaceEnv(r.Env, "PATH", "\x00") },
		"flags": func(r *hookwire.Request) {
			r.Args = []string{"-v", "--timeout", "2s", "--config", filepath.Join(home, "c.yaml")}
		},
		"missing transcript": func(r *hookwire.Request) {
			r.Stdin = []byte(`{"session_id":"s","transcript_path":"` + filepath.Join(home, "none.jsonl") + `"}`)
		},
		"stdin not JSON": func(r *hookwire.Request) { r.Stdin = []byte("not json") },
	} {
		t.Run("accepted "+name, func(t *testing.T) {
			request := selfRequest(t, hookPreToolUse, env, input)
			edit(&request)
			_, ack, reason := sendRequest(t, ts.path, request)
			if ack != hookwire.Accepted {
				t.Fatalf("ack %q reason %q, want accepted", ack, reason)
			}
			accepted++
		})
	}
	// Без 'C' запрос не исполнялся ни разу: соединения закрыты (Cleanup
	// подтестов), и демон записал каждое как оставленное CLI до 'C'
	if !waitFor(t, 3*time.Second, func() bool {
		return strings.Count(readFile(t, ts.log), "before commit") >= accepted
	}) {
		t.Fatalf("daemon log: %s", readFile(t, ts.log))
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("runner called %d times without commit", n)
	}
}

// Кадр сверх предела, чужой протокол и запрос, оборванный посередине, — «в
// CLI» до «принято»
func TestServeRefusesUnreadRequest(t *testing.T) {
	var calls atomic.Int64
	ts := startServer(t, countingRun(&calls), freshStart)
	for name, c := range map[string]struct {
		raw    []byte
		reason string
	}{
		"too large": {append([]byte(hookwire.Magic), 0xff, 0xff, 0xff, 0xff), "size"},
		"magic":     {[]byte("HTTP/1.1"), "protocol"},
		"truncated": {append([]byte(hookwire.Magic), 0, 0, 0, 100, 1, 2, 3), "read: unexpected EOF"},
	} {
		t.Run(name, func(t *testing.T) {
			conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: ts.path, Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if _, err := conn.Write(c.raw); err != nil {
				t.Fatal(err)
			}
			if err := conn.CloseWrite(); err != nil {
				t.Fatal(err)
			}
			if ack, reason := readAck(t, conn); ack != hookwire.Fallback || reason != c.reason {
				t.Fatalf("ack %q reason %q, want %q", ack, reason, c.reason)
			}
		})
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("runner called %d times", n)
	}
}

// Прокси, не дождавшийся «принято», закрывает соединение без 'C': демон
// запрос не исполняет — CLI исполнит его один раз
func TestServeNoExecutionWithoutCommit(t *testing.T) {
	var calls atomic.Int64
	ts := startServer(t, countingRun(&calls), freshStart)
	request := selfRequest(t, hookPreToolUse, absoluteEnv(t.TempDir()), `{}`)
	conn, ack, _ := sendRequest(t, ts.path, request)
	if ack != hookwire.Accepted {
		t.Fatalf("ack %q", ack)
	}
	_ = conn.Close()
	waitFor(t, 3*time.Second, func() bool { return strings.Contains(readFile(t, ts.log), "before commit") })
	if n := calls.Load(); n != 0 {
		t.Fatalf("runner called %d times", n)
	}
}

// Предел одновременных: сверх него — «в CLI: busy», а не ожидание
func TestServeBusy(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	ts := startServer(t, func(ctx context.Context, request hookctx.Context, hookType string) int {
		started <- struct{}{}
		<-release
		return exitAllowed
	}, freshStart, func(s *server) { s.sem = make(chan struct{}, 1) })
	env := absoluteEnv(t.TempDir())
	first, ack, _ := sendRequest(t, ts.path, selfRequest(t, hookPreToolUse, env, `{}`))
	if ack != hookwire.Accepted {
		t.Fatalf("first ack %q", ack)
	}
	if _, err := first.Write([]byte{hookwire.Commit}); err != nil {
		t.Fatal(err)
	}
	<-started
	if _, ack, reason := sendRequest(t, ts.path, selfRequest(t, hookPreToolUse, env, `{}`)); ack != hookwire.Fallback || reason != "busy" {
		t.Fatalf("second: ack %q reason %q", ack, reason)
	}
	close(release)
}

// Паника обработчика: ответ как у CLI на панике — «panic: …» со стеком в
// stderr и код 2 — и строка Error в логе демона; демон обслуживает дальше
func TestServePanicAnswersLikeCLI(t *testing.T) {
	cli := exec.Command(testBinary(t))
	cli.Env = append(os.Environ(), panicHelper+"=1")
	var cliStderr bytes.Buffer
	cli.Stderr = &cliStderr
	err := cli.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("CLI panic: %v", err)
	}
	var panicked atomic.Bool
	ts := startServer(t, func(ctx context.Context, request hookctx.Context, hookType string) int {
		fmt.Fprint(request.Stdout, "partial")
		if panicked.CompareAndSwap(false, true) {
			panic("boom")
		}
		return exitAllowed
	}, freshStart)
	env := absoluteEnv(t.TempDir())
	conn, ack, _ := sendRequest(t, ts.path, selfRequest(t, hookPreToolUse, env, `{}`))
	if ack != hookwire.Accepted {
		t.Fatalf("ack %q", ack)
	}
	code, stdout, stderr := commitAndWait(t, conn)
	if code != exitErr.ExitCode() || stdout != "partial" {
		t.Fatalf("daemon code %d stdout %q, CLI code %d", code, stdout, exitErr.ExitCode())
	}
	for name, text := range map[string]string{"daemon": stderr, "CLI": cliStderr.String()} {
		if !strings.HasPrefix(text, "panic: boom\n\ngoroutine ") {
			t.Errorf("%s stderr %q", name, text)
		}
	}
	if log := readFile(t, ts.log); !strings.Contains(log, "level=ERROR") || !strings.Contains(log, "panic=boom") {
		t.Errorf("daemon log lacks panic line:\n%s", log)
	}
	conn, ack, _ = sendRequest(t, ts.path, selfRequest(t, hookPreToolUse, env, `{}`))
	if ack != hookwire.Accepted {
		t.Fatalf("after panic: ack %q", ack)
	}
	if code, _, _ := commitAndWait(t, conn); code != exitAllowed {
		t.Fatalf("after panic: code %d", code)
	}
}

func countFDs(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

func processGone(pid int) bool {
	err := syscall.Kill(pid, 0)
	return errors.Is(err, syscall.ESRCH)
}

func waitFor(t *testing.T, limit time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cond()
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Error(err)
	}
	return string(data)
}

const webFetchForbidden = `{"session_id":"s","hook_event_name":"PostToolUse","tool_name":"WebFetch","tool_input":{"url":"https://example.org/"},"tool_response":{"url":"https://example.org/","code":403,"result":"Forbidden"}}`

const serveConfig = `logger:
  level: debug
  output: file
  file: ~/.claude/logs/chooks.log
tools:
  notifier: {enabled: false}
  skill_reload: {enabled: false}
  night_hint: {enabled: false}
  webfetch_fallback: {enabled: true}
  understand_check: {enabled: false}
  model_guard: {enabled: true}
  git_guard: {enabled: true}
  jira_style: {enabled: true}
  kill_guard: {enabled: true}
  rm_guard: {enabled: true}
`

// Сценарий 2: прокси ушёл посреди работы — ctx запроса отменён, заглушка
// webtext убита, горутины и дескрипторы демона вернулись к базе за 3 с
func TestServeCancelsOnClientClose(t *testing.T) {
	home := t.TempDir()
	writeFiles(t, home, map[string]string{
		".config/chooks/config.yaml": serveConfig,
		"bin/webtext":                "#!/bin/sh\necho $$ > \"$HOME/webtext.pid.tmp\" && mv \"$HOME/webtext.pid.tmp\" \"$HOME/webtext.pid\"\nexec sleep 30\n",
	})
	ts := startServer(t, nil, freshStart)
	baseGoroutines, baseFDs := runtime.NumGoroutine(), countFDs(t)
	conn, ack, reason := sendRequest(t, ts.path, selfRequest(t, hookWebFetchFallback, absoluteEnv(home), webFetchForbidden))
	if ack != hookwire.Accepted {
		t.Fatalf("ack %q %q", ack, reason)
	}
	if _, err := conn.Write([]byte{hookwire.Commit}); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(home, "webtext.pid")
	if !waitFor(t, 20*time.Second, func() bool { return readFile(t, pidFile) != "" }) {
		t.Fatalf("webtext stub not started; daemon log:\n%s", readFile(t, ts.log))
	}
	var pid int
	if _, err := fmt.Sscan(readFile(t, pidFile), &pid); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	closed := time.Now()
	ok := waitFor(t, 3*time.Second, func() bool {
		return processGone(pid) && runtime.NumGoroutine() <= baseGoroutines && countFDs(t) <= baseFDs
	})
	if !ok {
		t.Fatalf("after %s: stub gone %v, goroutines %d (base %d), fds %d (base %d)",
			time.Since(closed).Round(time.Millisecond), processGone(pid), runtime.NumGoroutine(), baseGoroutines, countFDs(t), baseFDs)
	}
	t.Logf("released in %s", time.Since(closed).Round(time.Millisecond))
	if log := readFile(t, ts.log); !strings.Contains(log, "request cancelled, proxy gone") {
		t.Errorf("daemon log lacks cancellation:\n%s", log)
	}
}

// proxyRun — один вызов прокси (бинарь тестов в режиме chooks-hook)
type proxyRun struct {
	code           int
	stdout, stderr string
	elapsed        time.Duration
	pid            int
}

// proxyEnv — env прокси: сокет демона и CLI запасного пути — обёртка,
// которая пишет причину в runs и исполняет бинарь тестов как CLI
func proxyEnv(t *testing.T, env []string, socket, runs string) []string {
	t.Helper()
	wrapper := filepath.Join(filepath.Dir(runs), "cli.sh")
	if _, err := os.Stat(wrapper); err != nil {
		body := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$%s\" >> %q\n%s=1 exec %q \"$@\"\n", hookproxy.EnvFallback, runs, argvCLIHelper, testBinary(t))
		if err := os.WriteFile(wrapper, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return append(slices.Clone(env), hookproxy.EnvSocket+"="+socket, hookproxy.EnvCLI+"="+wrapper)
}

// selfExe — бинарь тестов; читается в TestMain-процессе один раз, чтобы
// горутины тестов не звали t.Fatal
var selfExe = sync.OnceValues(os.Executable)

func runBinary(t *testing.T, helper string, args []string, env []string, dir string, stdin []byte) proxyRun {
	t.Helper()
	exe, err := selfExe()
	if err != nil {
		t.Errorf("executable: %v", err)
		return proxyRun{code: -1}
	}
	cmd := exec.Command(exe, args...)
	cmd.Env = append(slices.Clone(env), helper+"=1")
	cmd.Dir = dir
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	start := time.Now()
	err = cmd.Run()
	run := proxyRun{stdout: stdout.String(), stderr: stderr.String(), elapsed: time.Since(start)}
	if cmd.Process != nil {
		run.pid = cmd.Process.Pid
	}
	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &exitErr):
		run.code = exitErr.ExitCode()
	case err != nil:
		// Error, не Fatal: вызывают и из горутин теста
		t.Errorf("run %v: %v", args, err)
		run.code = -1
	}
	return run
}

func lines(text string) []string {
	return strings.FieldsFunc(text, func(r rune) bool { return r == '\n' })
}

// Сценарий 1, короткий хук: обработчик, который сам не кончается, отменён
// сроком запроса — таймаут pre-tool-use 3 с минус запас от старта прокси, —
// и прокси отдаёт ответ до 3 с, без запасного пути
func TestProxyShortHookWithinTimeout(t *testing.T) {
	ts := startServer(t, func(ctx context.Context, request hookctx.Context, hookType string) int {
		<-ctx.Done()
		fmt.Fprintf(request.Stderr, "stopped: %v\n", context.Cause(ctx))
		return exitError
	})
	dir := t.TempDir()
	runs := filepath.Join(dir, "cli-runs")
	env := proxyEnv(t, absoluteEnv(dir), ts.path, runs)
	for attempt := 1; ; attempt++ {
		run := runBinary(t, proxyHelper, []string{hookPreToolUse}, env, dir, []byte(`{}`))
		if fallbacks := lines(readFile(t, runs)); len(fallbacks) > 0 {
			if attempt == 3 {
				t.Fatalf("proxy went to CLI 3 times: %v", fallbacks)
			}
			_ = os.Remove(runs)
			continue
		}
		if run.code != exitError || !strings.Contains(run.stderr, "deadline exceeded") {
			t.Fatalf("code %d stderr %q", run.code, run.stderr)
		}
		if run.elapsed >= hookTimeouts[hookPreToolUse] || run.elapsed < hookTimeouts[hookPreToolUse]-2*serveDeadlineMargin {
			t.Fatalf("answered after %s, want within %s and not before the request deadline", run.elapsed, hookTimeouts[hookPreToolUse])
		}
		t.Logf("answered after %s", run.elapsed.Round(time.Millisecond))
		return
	}
}

// webFetchHome — фикстура WebFetch: заглушка webtext пишет эффект и ждёт delay
func webFetchHome(t *testing.T, delay string) (home, work string) {
	t.Helper()
	home = t.TempDir()
	work = filepath.Join(home, "work")
	writeFiles(t, home, map[string]string{
		".config/chooks/config.yaml": serveConfig,
		"bin/webtext":                "#!/bin/sh\necho \"$$\" >> \"$HOME/effects\"\nsleep " + delay + "\nprintf '%s\\n' '{\"ok\":true,\"source\":\"direct\",\"path\":\"page.txt\"}'\n",
		"work/page.txt":              "Offline webtext fixture.\n",
	})
	return home, work
}

// Сценарий 1, долгий хук: обработчик дольше окна «принято» (2,1 с) не
// повторяется прокси — один эффект webtext, CLI запасного пути не вызван,
// вывод тот же, что у отдельного CLI
func TestProxySlowHookRunsOnce(t *testing.T) {
	ts := startServer(t, nil)
	var served proxyRun
	for attempt := 1; ; attempt++ {
		home, work := webFetchHome(t, "2.1")
		runs := filepath.Join(home, "cli-runs")
		served = runBinary(t, proxyHelper, []string{hookWebFetchFallback}, proxyEnv(t, absoluteEnv(home), ts.path, runs), work, []byte(webFetchForbidden))
		effects := lines(readFile(t, filepath.Join(home, "effects")))
		if len(effects) != 1 {
			t.Fatalf("attempt %d: %d webtext effects, want 1; stderr %q", attempt, len(effects), served.stderr)
		}
		if fallbacks := lines(readFile(t, runs)); len(fallbacks) > 0 {
			if attempt == 3 {
				t.Fatalf("proxy went to CLI 3 times: %v", fallbacks)
			}
			continue
		}
		if served.elapsed < 2100*time.Millisecond {
			t.Fatalf("answered after %s, before the stub finished", served.elapsed)
		}
		break
	}
	home, work := webFetchHome(t, "2.1")
	cli := runBinary(t, argvCLIHelper, []string{hookWebFetchFallback}, absoluteEnv(home), work, []byte(webFetchForbidden))
	if served.code != cli.code || served.stdout != cli.stdout || served.stderr != cli.stderr {
		t.Fatalf("proxy code %d stdout %q stderr %q\nCLI code %d stdout %q stderr %q", served.code, served.stdout, served.stderr, cli.code, cli.stdout, cli.stderr)
	}
	if !strings.Contains(served.stdout, "updatedToolOutput") {
		t.Fatalf("stdout %q", served.stdout)
	}
}

// bigStdin — вход pre-tool-use больше буфера pipe (64 КиБ)
func bigStdin() []byte {
	command := "git status # " + strings.Repeat("x", 200<<10)
	data, _ := json.Marshal(map[string]any{"session_id": "big", "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_input": map[string]string{"command": command}})
	return data
}

// Сценарий 1, большой stdin: доходит целиком до демона и по запасному пути —
// и при отказе демона (memfd), и без демона (stdin не тронут)
func TestProxyBigStdin(t *testing.T) {
	stdin := bigStdin()
	want := sha256.Sum256(stdin)
	var got atomic.Value
	ts := startServer(t, func(ctx context.Context, request hookctx.Context, hookType string) int {
		data, err := io.ReadAll(request.Stdin)
		if err != nil {
			return exitError
		}
		got.Store(sha256.Sum256(data))
		request.Stdin = bytes.NewReader(data)
		return executeHook(ctx, request, hookType)
	})
	t.Run("daemon", func(t *testing.T) {
		for attempt := 1; ; attempt++ {
			home := t.TempDir()
			writeFiles(t, home, map[string]string{".config/chooks/config.yaml": serveConfig})
			runs := filepath.Join(home, "cli-runs")
			run := runBinary(t, proxyHelper, []string{hookPreToolUse}, proxyEnv(t, absoluteEnv(home), ts.path, runs), home, stdin)
			if fallbacks := lines(readFile(t, runs)); len(fallbacks) > 0 {
				if attempt == 3 {
					t.Fatalf("proxy went to CLI 3 times: %v", fallbacks)
				}
				continue
			}
			if run.code != exitAllowed || got.Load() != want {
				t.Fatalf("code %d stderr %q, stdin digest match %v", run.code, run.stderr, got.Load() == want)
			}
			return
		}
	})
	for name, c := range map[string]struct {
		socket, reason string
		env            func([]string) []string
	}{
		"refused":   {ts.path, "env: PATH empty component", func(env []string) []string { return replaceEnv(env, "PATH", "/usr/bin::/bin") }},
		"no daemon": {filepath.Join(socketDir(t), "none.sock"), "connect: no such file or directory", func(env []string) []string { return env }},
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			writeFiles(t, home, map[string]string{".config/chooks/config.yaml": serveConfig})
			runs := filepath.Join(home, "cli-runs")
			captured := filepath.Join(home, "cli-stdin")
			env := proxyEnv(t, c.env(absoluteEnv(home)), c.socket, runs)
			// Обёртка CLI сохраняет stdin, который получила, и отдаёт его CLI
			wrapper := filepath.Join(home, "cli-capture.sh")
			body := fmt.Sprintf("#!/bin/sh\ncat > %q\nprintf '%%s\\n' \"$%s\" >> %q\n%s=1 exec %q \"$@\" < %q\n", captured, hookproxy.EnvFallback, runs, argvCLIHelper, testBinary(t), captured)
			writeFiles(t, home, map[string]string{"cli-capture.sh": body})
			env = replaceEnv(env, hookproxy.EnvCLI, wrapper)
			run := runBinary(t, proxyHelper, []string{hookPreToolUse}, env, home, stdin)
			if fallbacks := lines(readFile(t, runs)); len(fallbacks) != 1 || fallbacks[0] != c.reason {
				t.Fatalf("fallbacks %q, want [%q]", fallbacks, c.reason)
			}
			if data := readFile(t, captured); sha256.Sum256([]byte(data)) != want {
				t.Fatalf("CLI stdin %d bytes, want %d", len(data), len(stdin))
			}
			if run.code != exitAllowed {
				t.Fatalf("code %d stderr %q", run.code, run.stderr)
			}
		})
	}
}

// fixture — дерево одного запроса сценария 3 и его env
type fixture struct {
	root, work string
	env        []string
}

const fixtureAConfig = `logger:
  level: debug
  output: file
  file: ~/.claude/logs/chooks.log
tools:
  notifier: {enabled: false}
  skill_reload: {enabled: false}
  night_hint: {enabled: false}
  model_guard: {enabled: true}
  jira_style: {enabled: true}
  git_guard: {enabled: true}
`

const fixtureBConfig = `logger:
  level: info
  output: file
  file: ~/logs/b.log
tools:
  notifier: {enabled: false}
  skill_reload: {enabled: false}
  night_hint: {enabled: false}
  model_guard: {enabled: true}
  jira_style: {enabled: false}
`

// newFixture — фикстура A или B: свои HOME, cwd, пояс (у A — из своего
// ZONEINFO), список моделей и config с противоположными решениями
func newFixture(t *testing.T, kind string) fixture {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	files := map[string]string{
		"home/bin/claude":  "#!/bin/sh\necho \"claude $*\" >> \"$HOME/stub-calls\"\ncat >/dev/null\necho '{}'\n",
		"work/body.json":   `{"body":"готово — сейчас"}`,
		"home/run/.keep":   "",
		"home/state/.keep": "",
	}
	env := absoluteEnv(home)
	switch kind {
	case "A":
		zone, err := os.ReadFile("/usr/share/zoneinfo/Asia/Tokyo")
		if err != nil {
			t.Fatal("no zoneinfo:", err)
		}
		files["home/.config/chooks/config.yaml"] = fixtureAConfig
		files["home/approved"] = "opus claude-opus-5-5\n"
		files["zones/Test/Zone"] = string(zone)
		env = append(env, "TZ=Test/Zone", "ZONEINFO="+filepath.Join(root, "zones"))
	case "B":
		files["home/.config/chooks/config.yaml"] = fixtureBConfig
		files["home/approved"] = "opus claude-opus-5-5\nsonnet claude-sonnet-5-5\n"
		files["work/body.json"] = `{"body":"готово"}`
		env = append(env, "TZ=America/New_York")
	}
	writeFiles(t, root, files)
	env = append(env, "MODELS_APPROVED="+filepath.Join(home, "approved"))
	return fixture{root: root, work: filepath.Join(root, "work"), env: env}
}

// fixtureCommands — команды pre-tool-use: модель (A отказывает, B пускает) и
// тело Jira из файла cwd (A с jira_style отказывает, B пускает)
var fixtureCommands = []string{
	"claude --model sonnet -p hi",
	"curl -X POST -d @body.json https://x.atlassian.net/rest/api/3/issue/PROJ-1/comment",
}

func fixtureInput(f fixture, i int) []byte {
	data, _ := json.Marshal(map[string]any{"session_id": "s3", "cwd": f.work, "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_input": map[string]string{"command": fixtureCommands[i%len(fixtureCommands)]}})
	return data
}

var (
	fixtureClock  = regexp.MustCompile(`\d{4}-\d\d-\d\d[ T]\d\d:\d\d:\d\d(\.\d+)?`)
	fixtureTSVNum = regexp.MustCompile(`(?m)^([^\t]*\t[^\t]*\t[^\t]*)\t\d+\t\d+`)
	fixtureStages = regexp.MustCompile(`=\d+(ms)?`)
	fixtureMS     = regexp.MustCompile(`(elapsed_ms|timeout_ms|ms)=\d+`)
)

// normalizeFixture — текст без корня фикстуры, pid вызова, времени и
// длительностей. Смещение пояса остаётся: по нему виден пояс запроса
func normalizeFixture(text, root string, pid int) string {
	text = strings.ReplaceAll(text, root, "@ROOT@")
	// Время раньше pid: pid, равный году (2026), иначе съедает дату
	text = fixtureClock.ReplaceAllString(text, "@TIME@")
	text = regexp.MustCompile(fmt.Sprintf(`\b%d\b`, pid)).ReplaceAllString(text, "@PID@")
	text = fixtureTSVNum.ReplaceAllString(text, "$1\tN\tN")
	text = fixtureMS.ReplaceAllString(text, "$1=N")
	var kept []string
	for line := range strings.SplitSeq(text, "\n") {
		// Строка запасного пути — его признак, у отдельного CLI её нет
		if strings.Contains(line, "hook slow") || strings.Contains(line, `msg="process skipped"`) || strings.Contains(line, `msg="hook via CLI fallback"`) {
			continue
		}
		if i := strings.Index(line, "stages="); i >= 0 {
			line = line[:i] + fixtureStages.ReplaceAllString(line[i:], "=N")
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// sixColumns — строки chooks-hooks.tsv без столбцов исполнителя и причины:
// они есть только у вызова через прокси
func sixColumns(text string) string {
	rows := strings.Split(text, "\n")
	for i, row := range rows {
		if fields := strings.Split(row, "\t"); len(fields) > 6 {
			rows[i] = strings.Join(fields[:6], "\t")
		}
	}
	return strings.Join(rows, "\n")
}

// fixtureTree — файлы фикстуры после вызова, нормализованные
func fixtureTree(t *testing.T, root string, pid int) map[string]string {
	t.Helper()
	tree := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		// Счёт ухода в CLI (fallbackwatch) пишет только CLI запасного пути
		base := filepath.Base(rel)
		if rel == "home/cli-runs" || strings.HasSuffix(rel, ".sh") || base == fallbackwatch.FileName || base == fallbackwatch.LockName {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := string(data)
		if base == durationsFile {
			text = sixColumns(text)
		}
		tree[rel] = normalizeFixture(text, root, pid)
		return nil
	})
	if err != nil {
		t.Error(err)
	}
	return tree
}

type fixtureOutcome struct {
	code           int
	stdout, stderr string
	tree           map[string]string
}

func outcomeOf(t *testing.T, f fixture, run proxyRun) fixtureOutcome {
	return fixtureOutcome{code: run.code, stdout: normalizeFixture(run.stdout, f.root, run.pid), stderr: normalizeFixture(run.stderr, f.root, run.pid), tree: fixtureTree(t, f.root, run.pid)}
}

func diffOutcome(a, b fixtureOutcome) string {
	var out []string
	if a.code != b.code || a.stdout != b.stdout || a.stderr != b.stderr {
		out = append(out, fmt.Sprintf("code %d/%d stdout %q/%q stderr %q/%q", a.code, b.code, a.stdout, b.stdout, a.stderr, b.stderr))
	}
	for _, name := range relativeEnvKeys(a.tree, b.tree) {
		if a.tree[name] != b.tree[name] {
			out = append(out, fmt.Sprintf("file %s:\n%s\n---\n%s", name, a.tree[name], b.tree[name]))
		}
	}
	return strings.Join(out, "\n")
}

// Сценарий 3: запросы A и B вперемешку, по 200 каждого, через прокси к
// одному демону. Вывод и файлы каждого совпадают с отдельным CLI на той же
// фикстуре; решения A и B противоположны
func TestProxyInterleavedFixturesMatchCLI(t *testing.T) {
	const perFixture = 200
	var served atomic.Int64
	ts := startServer(t, func(ctx context.Context, request hookctx.Context, hookType string) int {
		served.Add(1)
		return executeHook(ctx, request, hookType)
	})
	runs := filepath.Join(t.TempDir(), "cli-runs")
	reference := map[string]fixtureOutcome{}
	for _, kind := range []string{"A", "B"} {
		for i := range fixtureCommands {
			f := newFixture(t, kind)
			run := runBinary(t, argvCLIHelper, []string{hookPreToolUse}, proxyEnv(t, f.env, ts.path, runs), f.work, fixtureInput(f, i))
			reference[fmt.Sprint(kind, i)] = outcomeOf(t, f, run)
		}
	}
	for i := range fixtureCommands {
		a, b := reference[fmt.Sprint("A", i)], reference[fmt.Sprint("B", i)]
		if a.code != exitBlocked || b.code != exitAllowed {
			t.Fatalf("command %d: CLI A code %d, B code %d; want A blocked, B allowed\nA stderr %q", i, a.code, b.code, a.stderr)
		}
	}
	// Фикстуры и env — до горутин: в них t.Fatal нельзя
	fixtures := make([]fixture, 2*perFixture)
	envs := make([][]string, len(fixtures))
	for n := range fixtures {
		fixtures[n] = newFixture(t, []string{"A", "B"}[n%2])
		envs[n] = proxyEnv(t, fixtures[n].env, ts.path, runs)
	}
	var mu sync.Mutex
	var mismatches []string
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for n := range jobs {
				kind := []string{"A", "B"}[n%2]
				i := (n / 2) % len(fixtureCommands)
				f := fixtures[n]
				run := runBinary(t, proxyHelper, []string{hookPreToolUse}, envs[n], f.work, fixtureInput(f, i))
				if diff := diffOutcome(reference[fmt.Sprint(kind, i)], outcomeOf(t, f, run)); diff != "" {
					mu.Lock()
					mismatches = append(mismatches, fmt.Sprintf("request %d (%s, command %d): %s", n, kind, i, diff))
					mu.Unlock()
				}
			}
		})
	}
	for n := range 2 * perFixture {
		jobs <- n
	}
	close(jobs)
	wg.Wait()
	fallbacks := lines(readFile(t, runs))
	t.Logf("served by daemon %d, CLI fallbacks %d", served.Load(), len(fallbacks))
	if len(mismatches) > 0 {
		t.Fatalf("%d of %d requests differ from CLI; first:\n%s", len(mismatches), 2*perFixture, mismatches[0])
	}
	if served.Load() < perFixture {
		t.Fatalf("daemon served %d of %d requests; fallbacks %v", served.Load(), 2*perFixture, fallbacks[:min(5, len(fallbacks))])
	}
}

// TestDaemonOutsideShadowHooksMatchCLI covers the three hooks whose decisions
// are not part of shadow-mode parity. The fixture's claude executable is a
// deterministic local stub; no model process is started.
func TestDaemonOutsideShadowHooksMatchCLI(t *testing.T) {
	encode := func(v any) []byte {
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	tests := []struct {
		name, command string
		input         func(f fixture, transcript string) []byte
	}{
		{
			name: "understand-check", command: hookUnderstandCheck,
			input: func(f fixture, transcript string) []byte {
				return encode(map[string]any{"session_id": "s3", "cwd": f.work, "transcript_path": transcript,
					"hook_event_name": "PreToolUse", "tool_name": "Write", "tool_input": map[string]string{"file_path": filepath.Join("..", "home", ".claude", "CLAUDE.md"), "content": "change the rule"}, "tool_use_id": "tool-1"})
			},
		},
		{
			name: "post-tool-batch", command: hookPostToolBatch,
			input: func(f fixture, transcript string) []byte {
				return encode(map[string]any{"session_id": "s3", "cwd": f.work, "transcript_path": transcript, "hook_event_name": "PostToolBatch"})
			},
		},
		{
			name: "user-prompt-submit", command: hookUserPromptSubmit,
			input: func(f fixture, transcript string) []byte {
				return encode(map[string]any{"session_id": "s3", "cwd": f.work, "transcript_path": transcript, "hook_event_name": "UserPromptSubmit", "prompt": "Continue the fixture task."})
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Reuse the exact sandbox root so path-derived idempotency keys are
			// comparable. Clear hook state between runs so the CLI cannot mask a
			// daemon omission or mutation.
			cliFixture := newFixture(t, "A")
			proxyFixture := cliFixture
			config := strings.ReplaceAll(fixtureAConfig, `\n`, "\n")
			writeFiles(t, cliFixture.root, map[string]string{"home/.config/chooks/config.yaml": config})
			writeFiles(t, proxyFixture.root, map[string]string{"home/.config/chooks/config.yaml": config})
			judgeStub := "#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' '{\"type\":\"result\",\"result\":\"{\\\"verdict\\\":\\\"fix\\\",\\\"fixes\\\":[{\\\"quote\\\":\\\"Continue the fixture task.\\\",\\\"action\\\":\\\"change the rule\\\",\\\"why\\\":\\\"recorded fixture disagreement\\\",\\\"fix\\\":\\\"review the instruction\\\"}]}\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}'\n"
			if tc.command == hookUnderstandCheck {
				writeFiles(t, cliFixture.root, map[string]string{"home/bin/claude": judgeStub})
				writeFiles(t, proxyFixture.root, map[string]string{"home/bin/claude": judgeStub})
			}
			transcript := func(f fixture) string { return filepath.Join(f.work, "transcript.jsonl") }
			instructionPath := filepath.Join(cliFixture.root, "home", ".claude", "CLAUDE.md")
			transcriptLine := func(path string) string {
				toolName, toolInput := "Write", any(map[string]string{"file_path": path, "content": "change the rule"})
				if tc.command == hookUnderstandCheck {
					toolInput = map[string]string{"file_path": filepath.Join("..", "home", ".claude", "CLAUDE.md"), "content": "change the rule"}
				}
				if tc.command == hookPostToolBatch {
					// Bash edits are not a tracked evEdit: the agent has not read the
					// resulting skill contents, so skill-reload should remind it.
					toolName, toolInput = "Bash", map[string]string{"command": "external skill update fixture"}
				}
				data, err := json.Marshal(map[string]any{"type": "assistant", "sessionId": "s3", "uuid": "assistant-1", "parentUuid": "user-1", "timestamp": "2999-11-26T10:00:03Z", "message": map[string]any{"role": "assistant", "stop_reason": "tool_use", "content": []any{map[string]any{"type": "tool_use", "id": "tool-1", "name": toolName, "input": toolInput}}}})
				if err != nil {
					t.Fatal(err)
				}
				return string(data) + "\n"
			}
			previousUser := `{"type":"user","sessionId":"s3","uuid":"user-0","timestamp":"2999-11-26T09:59:58Z","origin":{"kind":"human"},"message":{"role":"user","content":"Earlier fixture request."}}` + "\n"
			previousAnswer := `{"type":"assistant","sessionId":"s3","uuid":"assistant-0","parentUuid":"user-0","timestamp":"2999-11-26T09:59:59Z","message":{"role":"assistant","content":"Earlier fixture response.","stop_reason":"end_turn"}}` + "\n"
			userLine := `{"type":"user","sessionId":"s3","uuid":"user-1","timestamp":"2999-11-26T10:00:01Z","origin":{"kind":"human"},"message":{"role":"user","content":"Continue the fixture task."}}` + "\n"
			cliTranscriptData := previousUser + previousAnswer + userLine + transcriptLine(instructionPath)
			proxyTranscriptData := previousUser + previousAnswer + userLine + transcriptLine(strings.Replace(instructionPath, cliFixture.root, proxyFixture.root, 1))
			if tc.command == hookPostToolBatch {
				cliSkill := filepath.Join(cliFixture.root, "home", ".claude", "skills", "demo", "SKILL.md")
				proxySkill := filepath.Join(proxyFixture.root, "home", ".claude", "skills", "demo", "SKILL.md")
				writeFiles(t, cliFixture.root, map[string]string{"home/.claude/skills/demo/SKILL.md": "new skill text\n"})
				writeFiles(t, proxyFixture.root, map[string]string{"home/.claude/skills/demo/SKILL.md": "new skill text\n"})
				metaLine := func(path string) string {
					data, err := json.Marshal(map[string]any{"type": "user", "isMeta": true, "sessionId": "s3", "uuid": "skill-load", "timestamp": "2999-11-26T10:00:00Z", "message": map[string]any{"role": "user", "content": "Base directory for this skill: " + filepath.Dir(path) + "\n\nold skill text\n"}})
					if err != nil {
						t.Fatal(err)
					}
					return string(data) + "\n"
				}
				cliTranscriptData = metaLine(cliSkill) + previousUser + previousAnswer + userLine + transcriptLine(cliSkill)
				proxyTranscriptData = metaLine(proxySkill) + previousUser + previousAnswer + userLine + transcriptLine(proxySkill)
			}
			writeFiles(t, cliFixture.root, map[string]string{"work/transcript.jsonl": cliTranscriptData})
			writeFiles(t, proxyFixture.root, map[string]string{"work/transcript.jsonl": proxyTranscriptData})
			cliInput, proxyInput := tc.input(cliFixture, transcript(cliFixture)), tc.input(proxyFixture, transcript(proxyFixture))
			// Use a separate daemon process here, just like `chooks serve`: the
			// process boundary is part of these outside-shadow parity decisions.
			sv := startSupervised(t, testBinary(t), cliFixture.root)
			if tc.command == hookPostToolBatch {
				config = strings.Replace(config, "skill_reload: {enabled: false}", "skill_reload: {enabled: true}", 1)
				writeFiles(t, cliFixture.root, map[string]string{"home/.config/chooks/config.yaml": config})
				writeFiles(t, proxyFixture.root, map[string]string{"home/.config/chooks/config.yaml": config})
				writeFiles(t, cliFixture.root, map[string]string{"home/state/chooks/skill-reload/.last-cleanup": ""})
			}
			cliRuns, proxyRuns := filepath.Join(t.TempDir(), "cli-runs"), filepath.Join(t.TempDir(), "proxy-runs")
			cliEnv := proxyEnv(t, cliFixture.env, sv.socket, cliRuns)
			cli := runBinary(t, argvCLIHelper, []string{tc.command}, cliEnv, cliFixture.work, cliInput)
			var cliDenied, cliRepeat proxyRun
			if tc.command == hookUnderstandCheck {
				cliDenied = cli
				// The second CLI call must see the first denial's stored key. The
				// daemon then receives the same request against that same state.
				cliRepeatRun := runBinary(t, argvCLIHelper, []string{tc.command}, cliEnv, cliFixture.work, cliInput)
				cliRepeat = cliRepeatRun
				if cliDenied.stdout == cliRepeat.stdout || !strings.Contains(cliDenied.stdout, `"permissionDecision":"deny"`) || strings.Contains(cliRepeat.stdout, `"permissionDecision":"deny"`) {
					t.Fatal("understand-check fixture did not exercise the recorded-denial repeat")
				}
			}
			if tc.command != hookUnderstandCheck {
				if err := os.RemoveAll(filepath.Join(cliFixture.root, "home", "state", "chooks")); err != nil {
					t.Fatal(err)
				}
			}
			if tc.command == hookPostToolBatch {
				writeFiles(t, cliFixture.root, map[string]string{"home/state/chooks/skill-reload/.last-cleanup": ""})
			}
			proxyEnv := proxyEnv(t, proxyFixture.env, sv.socket, proxyRuns)
			proxied := runBinary(t, proxyHelper, []string{tc.command}, proxyEnv, proxyFixture.work, proxyInput)
			if tc.command == hookUnderstandCheck {
				deniedKeys := filepath.Join(cliFixture.root, "home", "state", "chooks", "understand-check", "s3")
				if cliRepeat.code != proxied.code || cliRepeat.stdout != proxied.stdout {
					t.Fatalf("daemon repeat differs from CLI repeat: cli=(%d %q) daemon=(%d %q)", cliRepeat.code, cliRepeat.stdout, proxied.code, proxied.stdout)
				}
				if count := len(lines(readFile(t, deniedKeys))); count != 1 {
					t.Fatalf("recorded-denial repeat stored %d keys; want one shared key", count)
				}
				// Clear the one denied-key record and exercise the fresh judge
				// denial in the daemon process as a second parity observation.
				if err := os.Remove(deniedKeys); err != nil {
					t.Fatal(err)
				}
				proxyDeniedRun := runBinary(t, proxyHelper, []string{tc.command}, proxyEnv, proxyFixture.work, proxyInput)
				if cliDenied.code != proxyDeniedRun.code || cliDenied.stdout != proxyDeniedRun.stdout || !strings.Contains(proxyDeniedRun.stdout, `"permissionDecision":"deny"`) {
					t.Fatalf("daemon fresh denial differs from CLI: cli=(%d %q) daemon=(%d %q)", cliDenied.code, cliDenied.stdout, proxyDeniedRun.code, proxyDeniedRun.stdout)
				}
				proxied = proxyDeniedRun
			} else if diff := diffOutcome(outcomeOf(t, cliFixture, cli), outcomeOf(t, proxyFixture, proxied)); diff != "" {
				t.Fatalf("daemon differs from CLI: %s", diff)
			}
			if tc.command == hookPostToolBatch && (!strings.Contains(cli.stdout, "Скилл demo") || !strings.Contains(proxied.stdout, "Скилл demo")) {
				t.Fatalf("skill reminder not exercised: cli=(code %d stdout %q stderr %q log %q) proxy=(code %d stdout %q stderr %q log %q)", cli.code, cli.stdout, cli.stderr, readFile(t, filepath.Join(cliFixture.root, "home", ".claude", "logs", "chooks.log")), proxied.code, proxied.stdout, proxied.stderr, readFile(t, filepath.Join(proxyFixture.root, "home", ".claude", "logs", "chooks.log")))
			}
			if fallback := lines(readFile(t, proxyRuns)); len(fallback) != 0 {
				t.Fatalf("proxy fell back to CLI: %q", fallback)
			}
		})
	}
}

func TestDaemonUserPromptSubmitBlocksFromRequestProcReader(t *testing.T) {
	config, _ := silentConfig(t)
	home := t.TempDir()
	approved := filepath.Join(home, "approved")
	if err := os.WriteFile(approved, []byte("opus claude-opus-5-5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	request := runtimeGuardRequest(t, home, "MODELS_APPROVED="+approved)
	data := []byte(`{"hook_event_name":"UserPromptSubmit","session_id":"daemon-proc-reader","prompt":"continue"}`)
	ctx := hookctx.WithRequest(context.Background(), request)
	ctx = context.WithValue(ctx, runtimeGuardKey{}, runtimeGuardDeps{proc: runtimeProc{hook: request.PID}})
	ctx = context.WithValue(ctx, daemonCallKey{}, &daemonCall{})
	logger, _ := fileLogger(t)
	code, err := dispatchHook(ctx, hookUserPromptSubmit, data, config, logger, newHookTimerForRequest(hookUserPromptSubmit, request), request)
	if code != exitBlocked || err == nil || !strings.Contains(err.Error(), `модель "haiku" не одобрена`) {
		t.Fatalf("daemon UserPromptSubmit = %d, %v; want blocked by injected request ProcReader", code, err)
	}
}

func TestDaemonUserPromptSubmitSavesWorkingState(t *testing.T) {
	config, _ := silentConfig(t)
	runtimeDir, work := t.TempDir(), t.TempDir()
	request := hookctx.New(bytes.NewReader(nil), io.Discard, io.Discard,
		[]string{"XDG_RUNTIME_DIR=" + runtimeDir}, work, 12001, 12000)
	data := []byte(`{"hook_event_name":"UserPromptSubmit","session_id":"daemon-state-save","prompt":"continue"}`)
	ctx := hookctx.WithRequest(context.Background(), request)
	ctx = context.WithValue(ctx, daemonCallKey{}, &daemonCall{})
	logger, _ := fileLogger(t)
	code, err := dispatchHook(ctx, hookUserPromptSubmit, data, config, logger, newHookTimerForRequest(hookUserPromptSubmit, request), request)
	if code != exitAllowed || err != nil {
		t.Fatalf("daemon UserPromptSubmit = %d, %v; want allowed", code, err)
	}
	state, recorded, err := core.LookupSessionStateIn(filepath.Join(runtimeDir, "chooks"), "daemon-state-save")
	if err != nil || !recorded || state != core.StateWorking {
		t.Fatalf("session state = %q, recorded %v, %v; want recorded working state", state, recorded, err)
	}
}

func TestDaemonRequestUsesWireCwd(t *testing.T) {
	wantCwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	gotCwd := make(chan string, 1)
	ts := startServer(t, func(_ context.Context, request hookctx.Context, _ string) int {
		gotCwd <- request.Cwd
		return exitAllowed
	})
	freshStart(ts.srv)
	request := selfRequest(t, hookUserPromptSubmit, absoluteEnv(t.TempDir()), `{"hook_event_name":"UserPromptSubmit","session_id":"cwd-wire","prompt":"continue"}`)
	conn, ack, reason := sendRequest(t, ts.path, request)
	if ack != hookwire.Accepted {
		t.Fatalf("request ack = %q, %q; want accepted", ack, reason)
	}
	if code, _, _ := commitAndWait(t, conn); code != exitAllowed {
		t.Fatalf("daemon exit code = %d; want allowed", code)
	}
	select {
	case got := <-gotCwd:
		if got != wantCwd {
			t.Fatalf("daemon request cwd = %q; want wire cwd %q", got, wantCwd)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not run hook")
	}
}

// Сценарий 3, неподходящий env: относительный HOME и PATH с пустой
// компонентой исполняет CLI — причина у запасного пути, вывод как у CLI
func TestProxyRelativeEnvGoesToCLI(t *testing.T) {
	var served atomic.Int64
	ts := startServer(t, func(ctx context.Context, request hookctx.Context, hookType string) int {
		served.Add(1)
		return executeHook(ctx, request, hookType)
	})
	for name, c := range map[string]struct{ key, value, reason string }{
		// HOME от cwd запроса (work): ../home
		"relative HOME":        {"HOME", "../home", "env: HOME relative"},
		"PATH empty component": {"PATH", "@ROOT@/home/bin::/usr/bin:/bin", "env: PATH empty component"},
	} {
		t.Run(name, func(t *testing.T) {
			outcome := func(helper string) (fixtureOutcome, []string) {
				f := newFixture(t, "B")
				runs := filepath.Join(f.root, "home", "cli-runs")
				env := replaceEnv(f.env, c.key, strings.ReplaceAll(c.value, "@ROOT@", f.root))
				run := runBinary(t, helper, []string{hookPreToolUse}, proxyEnv(t, env, ts.path, runs), f.work, fixtureInput(f, 0))
				return outcomeOf(t, f, run), lines(readFile(t, runs))
			}
			proxied, fallbacks := outcome(proxyHelper)
			cli, _ := outcome(argvCLIHelper)
			if len(fallbacks) != 1 || fallbacks[0] != c.reason {
				t.Fatalf("fallbacks %q, want [%q]", fallbacks, c.reason)
			}
			if diff := diffOutcome(cli, proxied); diff != "" {
				t.Fatalf("proxy differs from CLI: %s", diff)
			}
		})
	}
	if n := served.Load(); n != 0 {
		t.Fatalf("daemon served %d requests with unsuitable env", n)
	}
}

// fakeClaudeMarker — $0 оболочки, которая в фикстуре дерева процессов
// играет claude сессии: модель — после «=»
const fakeClaudeMarker = "chooks-fake-claude="

// markerProc — /proc машины, где процесс с fakeClaudeMarker в argv — claude
// агента с моделью из маркера, а его родитель — init. Предки идут по
// настоящему /proc: решение зависит от того, с какого pid проверка начала
// подъём
type markerProc struct{ real modellaunch.ProcReader }

func (p markerProc) model(pid int) (string, bool) {
	argv, err := p.real.Cmdline(pid)
	if err != nil {
		return "", false
	}
	for _, arg := range argv {
		if model, ok := strings.CutPrefix(arg, fakeClaudeMarker); ok {
			return model, true
		}
	}
	return "", false
}

func (p markerProc) Parent(pid int) (int, error) {
	if _, ok := p.model(pid); ok {
		return 1, nil
	}
	return p.real.Parent(pid)
}

func (p markerProc) TTYNr(pid int) (int64, error) { return p.real.TTYNr(pid) }

func (p markerProc) Cgroup(pid int) (string, error) { return p.real.Cgroup(pid) }

func (p markerProc) Exe(pid int) (string, error) {
	if _, ok := p.model(pid); ok {
		return "/opt/claude/claude", nil
	}
	return p.real.Exe(pid)
}

func (p markerProc) Comm(pid int) (string, error) {
	if _, ok := p.model(pid); ok {
		return "claude", nil
	}
	return p.real.Comm(pid)
}

func (p markerProc) Environ(pid int) ([]string, error) {
	if _, ok := p.model(pid); ok {
		return []string{"CLAUDECODE=1"}, nil
	}
	return p.real.Environ(pid)
}

func (p markerProc) Cmdline(pid int) ([]string, error) {
	if model, ok := p.model(pid); ok {
		return []string{"claude", "--model", model}, nil
	}
	return p.real.Cmdline(pid)
}

func (p markerProc) Cwd(pid int) (string, error) { return p.real.Cwd(pid) }

// Сценарий 8: проверка модели claude в демоне поднимается по /proc от pid
// прокси — его родитель, а не процесс демона. Две цепочки вперемешку:
// родитель-«claude» на opus пропускает реплику, на haiku — отказ
func TestProxyRuntimeGuardFollowsProxyParent(t *testing.T) {
	var served atomic.Int64
	ts := startServer(t, func(ctx context.Context, request hookctx.Context, hookType string) int {
		served.Add(1)
		ctx = context.WithValue(ctx, runtimeGuardKey{}, runtimeGuardDeps{proc: markerProc{real: modellaunch.ProcReader{}}})
		return executeHook(ctx, request, hookType)
	})
	const perChain = 10
	type result struct {
		model string
		run   proxyRun
		cli   bool
	}
	results := make(chan result, 2*perChain)
	exe := testBinary(t)
	var wg sync.WaitGroup
	for n := range 2 * perChain {
		model := []string{"opus", "haiku"}[n%2]
		f := newFixture(t, "A")
		runs := filepath.Join(f.root, "home", "cli-runs")
		env := proxyEnv(t, f.env, ts.path, runs)
		wg.Go(func() {
			// Оболочка-родитель прокси не exec'ает его: «; exit $?»
			cmd := exec.Command("/bin/sh", "-c", `"$1" user-prompt-submit; exit $?`, fakeClaudeMarker+model, exe)
			cmd.Env = append(env, proxyHelper+"=1")
			cmd.Dir = f.work
			cmd.Stdin = strings.NewReader(`{"session_id":"guard","hook_event_name":"UserPromptSubmit","prompt":"x"}`)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			run := proxyRun{stdout: stdout.String(), stderr: stderr.String()}
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				run.code = exitErr.ExitCode()
			} else if err != nil {
				t.Error(err)
				return
			}
			results <- result{model: model, run: run, cli: len(lines(readFile(t, runs))) > 0}
		})
	}
	wg.Wait()
	close(results)
	checked := 0
	for r := range results {
		if r.cli {
			// Запасной путь судит настоящее дерево процессов: фикстура к нему
			// не относится
			continue
		}
		checked++
		switch r.model {
		case "opus":
			if r.run.code != exitAllowed {
				t.Errorf("opus chain: code %d stderr %q", r.run.code, r.run.stderr)
			}
		case "haiku":
			if r.run.code != exitBlocked || !strings.Contains(r.run.stderr, `модель "haiku" не одобрена`) {
				t.Errorf("haiku chain: code %d stderr %q", r.run.code, r.run.stderr)
			}
		}
	}
	if checked < perChain {
		t.Fatalf("daemon served %d of %d", checked, 2*perChain)
	}
}

// CLI запасного пути пишет причину строкой в chooks.log; в env хука и его
// детей CHOOKS_FALLBACK не остаётся
func TestCLIFallbackLogLine(t *testing.T) {
	home := t.TempDir()
	writeFiles(t, home, map[string]string{".config/chooks/config.yaml": serveConfig})
	env := append(absoluteEnv(home), hookproxy.EnvFallback+"=connect: no such file or directory")
	run := runBinary(t, argvCLIHelper, []string{hookPreToolUse}, env, home, []byte(`{"session_id":"s","tool_name":"Bash","tool_input":{"command":"ls"}}`))
	if run.code != exitAllowed {
		t.Fatalf("code %d stderr %q", run.code, run.stderr)
	}
	log := readFile(t, filepath.Join(home, ".claude", "logs", "chooks.log"))
	if !strings.Contains(log, `msg="hook via CLI fallback" hook=pre-tool-use reason="connect: no such file or directory"`) {
		t.Fatalf("log lacks fallback line:\n%s", log)
	}
	request := hookctx.New(nil, nil, nil, env, home, 1, 1)
	if takeFallbackReason(&request) != "connect: no such file or directory" || slices.ContainsFunc(request.Env, func(e string) bool { return strings.HasPrefix(e, hookproxy.EnvFallback+"=") }) {
		t.Fatalf("env after take: %v", request.Env)
	}
}
