package hookproxy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/aiseeq/akit/proc"
	"golang.org/x/sys/unix"

	"github.com/aiseeq/chooks/internal/hookwire"
)

// proxyHelper — бинарь тестов становится прокси (Main) в TestMain
const proxyHelper = "HOOKPROXY_TEST_MAIN"
const proxyCloseStdout = "HOOKPROXY_TEST_CLOSE_STDOUT"
const proxyMemfdFailureHelper = "HOOKPROXY_TEST_MEMFD_FAILURE"
const proxyMemfdFailureLost = "HOOKPROXY_TEST_MEMFD_FAILURE_LOST"

func TestMain(m *testing.M) {
	if os.Getenv(proxyMemfdFailureHelper) == "1" {
		var limit unix.Rlimit
		if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		limit.Cur = 3 // stdin/stdout/stderr remain open; memfd_create gets EMFILE
		if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		p := &proxy{args: []string{"chooks-hook", "pre-tool-use"}, fd: -1}
		reason := ackPrefix + "daemon closed before accept"
		if os.Getenv(proxyMemfdFailureLost) == "1" {
			reason = LostPrefix + "daemon closed after accept"
		}
		os.Exit(p.fallback([]byte(`{}`), true, reason))
	}
	if os.Getenv(proxyHelper) == "1" {
		_ = os.Unsetenv(proxyHelper)
		if os.Getenv(proxyCloseStdout) == "1" {
			_ = syscall.Close(1)
		}
		os.Exit(Main(os.Args))
	}
	os.Exit(m.Run())
}

// fakeDaemon — демон протокола в тесте: serve получает соединение и
// прочитанный запрос
type fakeDaemon struct {
	path     string
	listener *net.UnixListener
	wg       sync.WaitGroup
}

func startFake(t *testing.T, serve func(conn *net.UnixConn, request hookwire.Request)) *fakeDaemon {
	t.Helper()
	dir, err := os.MkdirTemp("", "hp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "d.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	d := &fakeDaemon{path: path, listener: listener}
	d.wg.Go(func() {
		for {
			conn, err := listener.AcceptUnix()
			if err != nil {
				return
			}
			d.wg.Go(func() {
				defer conn.Close()
				_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
				request, err := hookwire.ReadRequest(conn)
				if err != nil {
					t.Errorf("fake daemon: %v", err)
					return
				}
				serve(conn, request)
			})
		}
	})
	t.Cleanup(func() { _ = listener.Close(); d.wg.Wait() })
	return d
}

// cliStub — CLI запасного пути: пишет вызов (причину, argv, stdin) и
// печатает свой вывод с кодом 7
type cliStub struct {
	path, runs, stdin string
}

func newCLIStub(t *testing.T) cliStub {
	t.Helper()
	dir := t.TempDir()
	stub := cliStub{path: filepath.Join(dir, "chooks"), runs: filepath.Join(dir, "runs"), stdin: filepath.Join(dir, "stdin")}
	body := fmt.Sprintf("#!/bin/sh\ncat > %q\nprintf '%%s|%%s\\n' \"$%s\" \"$*\" >> %q\necho cli-out\necho cli-err >&2\nexit 7\n", stub.stdin, EnvFallback, stub.runs)
	if err := os.WriteFile(stub.path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return stub
}

func (s cliStub) calls(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(s.runs)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

type result struct {
	code           int
	stdout, stderr string
	elapsed        time.Duration
	pid            int
}

func runProxy(t *testing.T, socket string, stub cliStub, stdin []byte, args ...string) result {
	return runProxyWithEnv(t, socket, stub, stdin, nil, args...)
}

func runProxyWithEnv(t *testing.T, socket string, stub cliStub, stdin []byte, extraEnv []string, args ...string) result {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, args...)
	cmd.Env = append(os.Environ(), proxyHelper+"=1", EnvSocket+"="+socket, EnvCLI+"="+stub.path, "PROXY_TEST_MARK=1")
	cmd.Env = append(cmd.Env, extraEnv...)
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	start := time.Now()
	err = cmd.Run()
	r := result{stdout: stdout.String(), stderr: stderr.String(), elapsed: time.Since(start), pid: cmd.Process.Pid}
	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &exitErr):
		r.code = exitErr.ExitCode()
	case err != nil:
		t.Fatal(err)
	}
	return r
}

func TestStdoutWriteFailurePreservesHookExitCode(t *testing.T) {
	d := startFake(t, func(conn *net.UnixConn, _ hookwire.Request) {
		_, _ = conn.Write([]byte{hookwire.Accepted})
		if b, err := readByte(conn, 5*time.Second); err != nil || b != hookwire.Commit {
			t.Errorf("commit: %q %v", b, err)
			return
		}
		_, _ = conn.Write(hookwire.EncodeResult(2, []byte("deny"), nil))
	})
	r := runProxyWithEnv(t, d.path, cliStub{}, []byte(`{}`), []string{proxyCloseStdout + "=1"}, "pre-tool-use")
	if r.code != 2 || !strings.Contains(r.stderr, "chooks-hook: stdout write failed:") {
		t.Fatalf("code %d stderr %q", r.code, r.stderr)
	}
}

// bigInput — stdin больше буфера pipe (64 КиБ)
var bigInput = bytes.Repeat([]byte("0123456789abcdef"), 40<<10)

func readByte(conn *net.UnixConn, limit time.Duration) (byte, error) {
	_ = conn.SetReadDeadline(time.Now().Add(limit))
	var b [1]byte
	_, err := io.ReadFull(conn, b[:])
	return b[0], err
}

// Без демона прокси сразу исполняет CLI с нетронутым stdin
func TestNoDaemonRunsCLI(t *testing.T) {
	stub := newCLIStub(t)
	r := runProxy(t, filepath.Join(t.TempDir(), "none.sock"), stub, bigInput, "pre-tool-use", "-v")
	if r.code != 7 || r.stdout != "cli-out\n" || r.stderr != "cli-err\n" {
		t.Fatalf("code %d stdout %q stderr %q", r.code, r.stdout, r.stderr)
	}
	calls := stub.calls(t)
	if len(calls) != 1 || calls[0] != "connect: no such file or directory|pre-tool-use -v" {
		t.Fatalf("CLI calls %q", calls)
	}
	if data, _ := os.ReadFile(stub.stdin); !bytes.Equal(data, bigInput) {
		t.Fatalf("CLI stdin %d bytes, want %d", len(data), len(bigInput))
	}
}

// Запрос несёт подкоманду, аргументы, env, cwd, pid и ppid прокси и stdin
// целиком; результат демона прокси отдаёт как есть
func TestRequestAndResult(t *testing.T) {
	var got hookwire.Request
	var peer int32
	d := startFake(t, func(conn *net.UnixConn, request hookwire.Request) {
		got = request
		raw, _ := conn.SyscallConn()
		_ = raw.Control(func(fd uintptr) {
			if cred, err := getPeerPID(int(fd)); err == nil {
				peer = cred
			}
		})
		_, _ = conn.Write([]byte{hookwire.Accepted})
		if b, err := readByte(conn, 5*time.Second); err != nil || b != hookwire.Commit {
			t.Errorf("commit: %q %v", b, err)
			return
		}
		_, _ = conn.Write(hookwire.EncodeResult(2, []byte("daemon-out"), []byte("daemon-err")))
	})
	stub := newCLIStub(t)
	r := runProxy(t, d.path, stub, bigInput, "pre-tool-use", "--timeout", "2s")
	if r.code != 2 || r.stdout != "daemon-out" || r.stderr != "daemon-err" {
		t.Fatalf("code %d stdout %q stderr %q", r.code, r.stdout, r.stderr)
	}
	if calls := stub.calls(t); len(calls) != 0 {
		t.Fatalf("CLI called: %q", calls)
	}
	cwd, _ := os.Getwd()
	if got.Subcommand != "pre-tool-use" || strings.Join(got.Args, " ") != "--timeout 2s" || got.Cwd != cwd ||
		got.PID != r.pid || int(peer) != r.pid || got.PPID != os.Getpid() || !bytes.Equal(got.Stdin, bigInput) {
		t.Fatalf("request %q %q cwd %q pid %d (peer %d, proxy %d) ppid %d stdin %d bytes", got.Subcommand, got.Args, got.Cwd, got.PID, peer, r.pid, got.PPID, len(got.Stdin))
	}
	if !contains(got.Env, "PROXY_TEST_MARK=1") || contains(got.Env, proxyHelper+"=1") {
		t.Fatalf("env %q", got.Env)
	}
}

func contains(list []string, item string) bool {
	return slices.Contains(list, item)
}

// Сценарий 1: демон принял соединение, но не ответил ни байта — прокси к
// сроку «принято» уходит в CLI со всем stdin и без 'C': демон запрос не
// исполнит
func TestNoAckFallsBackWithoutCommit(t *testing.T) {
	sawCommit := make(chan bool, 1)
	d := startFake(t, func(conn *net.UnixConn, request hookwire.Request) {
		b, err := readByte(conn, 5*time.Second)
		sawCommit <- err == nil && b == hookwire.Commit
	})
	stub := newCLIStub(t)
	r := runProxy(t, d.path, stub, bigInput, "stop")
	if r.code != 7 || r.stdout != "cli-out\n" {
		t.Fatalf("code %d stdout %q stderr %q", r.code, r.stdout, r.stderr)
	}
	calls := stub.calls(t)
	if len(calls) != 1 || !strings.HasPrefix(calls[0], "ack: ") || !strings.HasSuffix(calls[0], "|stop") {
		t.Fatalf("CLI calls %q", calls)
	}
	if data, _ := os.ReadFile(stub.stdin); !bytes.Equal(data, bigInput) {
		t.Fatalf("CLI stdin %d bytes, want %d", len(data), len(bigInput))
	}
	if <-sawCommit {
		t.Fatal("proxy sent commit after giving up")
	}
	if r.elapsed > 5*time.Second {
		t.Fatalf("fallback after %s", r.elapsed)
	}
}

// Отказ демона «в CLI: причина» — CLI с этой причиной в CHOOKS_FALLBACK
func TestRefusalPassesReason(t *testing.T) {
	d := startFake(t, func(conn *net.UnixConn, request hookwire.Request) {
		_, _ = conn.Write(hookwire.EncodeFallback("env: HOME relative"))
	})
	stub := newCLIStub(t)
	r := runProxy(t, d.path, stub, []byte(`{"x":1}`), "notification")
	if calls := stub.calls(t); r.code != 7 || len(calls) != 1 || calls[0] != "env: HOME relative|notification" {
		t.Fatalf("code %d calls %q", r.code, calls)
	}
	if data, _ := os.ReadFile(stub.stdin); string(data) != `{"x":1}` {
		t.Fatalf("CLI stdin %q", data)
	}
}

// Сценарий 1: после «принято» обрыв — до результата или посреди него — CLI
// зовётся один раз с причиной «lost: …» (pre-tool-use повторяет проверку,
// остальные хуки записывают
// вызов: ревью 8f10b59, п. 3), неполный вывод не отдаётся
func TestDropAfterAcceptDoesNotRetry(t *testing.T) {
	for name, tail := range map[string][]byte{
		"before result": nil,
		"mid result":    hookwire.EncodeResult(0, []byte("partial-out-that-is-long"), nil)[:12],
	} {
		t.Run(name, func(t *testing.T) {
			d := startFake(t, func(conn *net.UnixConn, request hookwire.Request) {
				_, _ = conn.Write([]byte{hookwire.Accepted})
				if b, err := readByte(conn, 5*time.Second); err != nil || b != hookwire.Commit {
					t.Errorf("commit: %q %v", b, err)
				}
				_, _ = conn.Write(tail)
			})
			stub := newCLIStub(t)
			r := runProxy(t, d.path, stub, []byte(`{"x":1}`), "webfetch-fallback")
			if r.code != 7 || r.stdout != "cli-out\n" {
				t.Fatalf("code %d stdout %q stderr %q", r.code, r.stdout, r.stderr)
			}
			calls := stub.calls(t)
			if len(calls) != 1 || !Lost(calls[0]) || !strings.HasSuffix(calls[0], "|webfetch-fallback") {
				t.Fatalf("CLI calls %q, want one with the lost reason", calls)
			}
			if data, err := os.ReadFile(stub.stdin); err != nil || string(data) != `{"x":1}` {
				t.Fatalf("CLI stdin %q %v", data, err)
			}
		})
	}
}

func TestLostPreToolUseExecFailureBlocks(t *testing.T) {
	d := startFake(t, func(conn *net.UnixConn, _ hookwire.Request) {
		_, _ = conn.Write([]byte{hookwire.Accepted})
		if b, err := readByte(conn, 5*time.Second); err != nil || b != hookwire.Commit {
			t.Errorf("commit: %q %v", b, err)
		}
	})
	stub := newCLIStub(t)
	r := runProxyWithEnv(t, d.path, stub, []byte(`{}`), []string{EnvCLI + "=/nonexistent/chooks"}, "pre-tool-use")
	if r.code != 2 || !strings.Contains(r.stderr, "проверка прервана, повтори команду") || strings.Contains(r.stderr, "хук не повторён") {
		t.Fatalf("code %d stderr %q", r.code, r.stderr)
	}
}

func TestLostStopExecFailureKeepsErrorCode(t *testing.T) {
	d := startFake(t, func(conn *net.UnixConn, _ hookwire.Request) {
		_, _ = conn.Write([]byte{hookwire.Accepted})
		if b, err := readByte(conn, 5*time.Second); err != nil || b != hookwire.Commit {
			t.Errorf("commit: %q %v", b, err)
		}
	})
	stub := newCLIStub(t)
	r := runProxyWithEnv(t, d.path, stub, []byte(`{}`), []string{EnvCLI + "=/nonexistent/chooks"}, "stop")
	if r.code != 1 || !strings.Contains(r.stderr, "запасной путь /nonexistent/chooks") || strings.Contains(r.stderr, "проверка прервана") {
		t.Fatalf("code %d stderr %q; want ordinary exec failure", r.code, r.stderr)
	}
}

func TestNonLostPreToolUseStdinReplacementFailureKeepsErrorCode(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^$")
	cmd.Env = append(os.Environ(), proxyMemfdFailureHelper+"=1", EnvCLI+"=/nonexistent/chooks")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 || !strings.Contains(stderr.String(), "stdin для /nonexistent/chooks") || strings.Contains(stderr.String(), "проверка прервана") {
		t.Fatalf("error %v stdout %q stderr %q; want ordinary fallback error", err, stdout.String(), stderr.String())
	}
}

func TestNonLostPreToolUseExecFailureKeepsErrorCode(t *testing.T) {
	stub := newCLIStub(t)
	r := runProxyWithEnv(t, filepath.Join(t.TempDir(), "no-daemon.sock"), stub, []byte(`{}`), []string{EnvCLI + "=/nonexistent/chooks"}, "pre-tool-use")
	if r.code != 1 || strings.Contains(r.stderr, "проверка прервана, повтори команду") {
		t.Fatalf("code %d stderr %q", r.code, r.stderr)
	}
}

func TestLostPreToolUseStdinReplacementFailureBlocks(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^$")
	cmd.Env = append(os.Environ(), proxyMemfdFailureHelper+"=1", proxyMemfdFailureLost+"=1", EnvCLI+"=/unused/chooks")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 || !strings.Contains(stderr.String(), "проверка прервана, повтори команду") {
		t.Fatalf("error %v stdout %q stderr %q", err, stdout.String(), stderr.String())
	}
}

// Сценарий 1: результат дольше окна «принято» прокси ждёт, не уходя в CLI
func TestSlowResultAfterAccept(t *testing.T) {
	d := startFake(t, func(conn *net.UnixConn, request hookwire.Request) {
		_, _ = conn.Write([]byte{hookwire.Accepted})
		if b, err := readByte(conn, 5*time.Second); err != nil || b != hookwire.Commit {
			t.Errorf("commit: %q %v", b, err)
			return
		}
		time.Sleep(2100 * time.Millisecond)
		_, _ = conn.Write(hookwire.EncodeResult(0, []byte("late"), nil))
	})
	stub := newCLIStub(t)
	r := runProxy(t, d.path, stub, []byte(`{}`), "webfetch-fallback")
	if r.code != 0 || r.stdout != "late" || r.elapsed < 2100*time.Millisecond {
		t.Fatalf("code %d stdout %q stderr %q after %s", r.code, r.stdout, r.stderr, r.elapsed)
	}
	if calls := stub.calls(t); len(calls) != 0 {
		t.Fatalf("CLI called: %q", calls)
	}
}

// Неизвестный байт вместо ответа приёма — CLI: исполнение демону не отдано
func TestUnexpectedAckFallsBack(t *testing.T) {
	d := startFake(t, func(conn *net.UnixConn, request hookwire.Request) {
		_, _ = conn.Write([]byte{'?'})
	})
	stub := newCLIStub(t)
	r := runProxy(t, d.path, stub, []byte(`{}`), "stop")
	if calls := stub.calls(t); r.code != 7 || len(calls) != 1 || calls[0] != "ack: unexpected byte|stop" {
		t.Fatalf("code %d calls %q", r.code, calls)
	}
}

func getPeerPID(fd int) (int32, error) {
	cred, err := unix.GetsockoptUcred(fd, unix.SOL_SOCKET, unix.SO_PEERCRED)
	if err != nil {
		return 0, fmt.Errorf("get peer credentials: %w", err)
	}
	return cred.Pid, nil
}

// Ревью 2б-1, п. 8: 'C' пришёл к демону после его срока ожидания — демон
// отвечает 'F' и запрос не исполняет; прокси уходит в CLI со всем stdin и
// причиной «commit: …», вызов не теряется
func TestLateCommitFallsBack(t *testing.T) {
	d := startFake(t, func(conn *net.UnixConn, request hookwire.Request) {
		_, _ = conn.Write([]byte{hookwire.Accepted})
		if b, err := readByte(conn, 5*time.Second); err != nil || b != hookwire.Commit {
			t.Errorf("commit: %q %v", b, err)
			return
		}
		_, _ = conn.Write(hookwire.EncodeFallback("late"))
	})
	stub := newCLIStub(t)
	r := runProxy(t, d.path, stub, bigInput, "stop")
	if calls := stub.calls(t); r.code != 7 || len(calls) != 1 || calls[0] != "commit: late|stop" {
		t.Fatalf("code %d stderr %q calls %q", r.code, r.stderr, calls)
	}
	if data, _ := os.ReadFile(stub.stdin); !bytes.Equal(data, bigInput) {
		t.Fatalf("CLI stdin %d bytes, want %d", len(data), len(bigInput))
	}
}

// Ревью 2б-1, п. 2: SIGTERM прокси после 'C' (таймаут хука у Claude Code) —
// демон получает 'T', прокси выходит с кодом 143, как CLI, и CLI не зовёт
func TestSIGTERMAfterCommitTellsDaemon(t *testing.T) {
	committed := make(chan struct{})
	got := make(chan byte, 1)
	d := startFake(t, func(conn *net.UnixConn, request hookwire.Request) {
		_, _ = conn.Write([]byte{hookwire.Accepted})
		if b, err := readByte(conn, 5*time.Second); err != nil || b != hookwire.Commit {
			t.Errorf("commit: %q %v", b, err)
			return
		}
		close(committed)
		b, err := readByte(conn, 10*time.Second)
		if err != nil {
			t.Errorf("after commit: %v", err)
		}
		got <- b
	})
	stub := newCLIStub(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "webfetch-fallback")
	cmd.Env = append(os.Environ(), proxyHelper+"=1", EnvSocket+"="+d.path, EnvCLI+"="+stub.path)
	cmd.Stdin = strings.NewReader(`{}`)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-committed:
	case <-time.After(10 * time.Second):
		if _, err := proc.StopChild(cmd.Process.Pid, 0, func() error { return cmd.Process.Kill() }); err != nil {
			t.Errorf("stop proxy: %v", err)
		}
		if err := cmd.Wait(); err != nil {
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Errorf("wait for proxy: %v", err)
			}
		}
		t.Fatal("no commit")
	}
	if _, err := proc.StopChild(cmd.Process.Pid, time.Second, func() error { return cmd.Process.Signal(syscall.SIGTERM) }); err != nil {
		t.Fatal(err)
	}
	err = cmd.Wait()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != KilledExitCode {
		t.Fatalf("proxy exit %v, want code %d", err, KilledExitCode)
	}
	if b := <-got; b != hookwire.Terminate {
		t.Fatalf("daemon got %q, want 'T'", b)
	}
	if calls := stub.calls(t); len(calls) != 0 {
		t.Fatalf("CLI called: %q", calls)
	}
}

// Ревью 8f10b59, п. 2: в счёт уведомления — только недоступность демона
// (connect, ack, обрыв); обновление и уходы по замыслу — нет
func TestUnavailableReasons(t *testing.T) {
	for reason, want := range map[string]bool{
		"connect: no such file or directory": true,
		"connect: connection refused":        true,
		"ack: connection timed out":          true,
		"ack: unexpected byte":               true,
		"send: broken pipe":                  true,
		"commit: broken pipe":                true,
		LostPrefix + "connection reset":      true,
		commitPrefix + LateReason:            false,
		"upgrade":                            false,
		"env: HOME relative":                 false,
		"subcommand":                         false,
		"transcript: not a regular file":     false,
		"size":                               false,
		"argv":                               false,
		"stdin too large":                    false,
		"busy: 2 stuck":                      false,
	} {
		if got := Unavailable(reason); got != want {
			t.Errorf("Unavailable(%q) = %v, want %v", reason, got, want)
		}
	}
}

// Ревью 8f10b59, п. 2: демон ответил «в CLI», не дочитав запрос, и закрыл
// сокет (отказ «upgrade» на дообслуживании). Запись прокси кончается EPIPE,
// но причина — та, что прислал демон, а не «send: broken pipe»
func TestRefusalBeforeRequestRead(t *testing.T) {
	dir, err := os.MkdirTemp("", "hp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "d.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
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
			_, _ = conn.Write(hookwire.EncodeFallback("upgrade"))
			_ = conn.Close()
		}
	}()
	stub := newCLIStub(t)
	r := runProxy(t, path, stub, bigInput, "stop")
	if calls := stub.calls(t); r.code != 7 || len(calls) != 1 || calls[0] != "upgrade|stop" {
		t.Fatalf("code %d calls %q stderr %q", r.code, calls, r.stderr)
	}
	if data, _ := os.ReadFile(stub.stdin); !bytes.Equal(data, bigInput) {
		t.Fatalf("CLI stdin %d bytes, want %d", len(data), len(bigInput))
	}
}

// Каталог процесса удалён: прокси не соединяется с демоном, CLI получает
// нетронутый stdin и причину cwd
func TestDeletedCwdFallsBackBeforeDial(t *testing.T) {
	var requests atomic.Int32
	d := startFake(t, func(conn *net.UnixConn, r hookwire.Request) {
		requests.Add(1)
	})
	stub := newCLIStub(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	gone := filepath.Join(t.TempDir(), "gone")
	if err := os.Mkdir(gone, 0o700); err != nil {
		t.Fatal(err)
	}
	stdin := `{"session_id":"s"}`
	cmd := exec.Command("/bin/sh", "-c", `cd "$D" && rmdir "$D" && exec "$EXE" pre-tool-use`)
	cmd.Env = append(os.Environ(), proxyHelper+"=1", EnvSocket+"="+d.path, EnvCLI+"="+stub.path, "D="+gone, "EXE="+exe)
	cmd.Stdin = strings.NewReader(stdin)
	if err := cmd.Run(); err == nil {
		t.Fatal("proxy exited 0, want the CLI stub code 7")
	}
	if n := requests.Load(); n != 0 {
		t.Fatalf("daemon got %d requests, want 0", n)
	}
	calls := stub.calls(t)
	if len(calls) != 1 || !strings.HasPrefix(calls[0], "cwd: ") {
		t.Fatalf("CLI calls %q, want one with reason cwd: …", calls)
	}
	if got, err := os.ReadFile(stub.stdin); err != nil || string(got) != stdin {
		t.Fatalf("CLI stdin %q (%v), want %q", got, err, stdin)
	}
}
