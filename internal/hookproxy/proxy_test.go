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
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/aiseeq/chooks/internal/hookwire"
)

// proxyHelper — бинарь тестов становится прокси (Main) в TestMain
const proxyHelper = "HOOKPROXY_TEST_MAIN"

func TestMain(m *testing.M) {
	if os.Getenv(proxyHelper) == "1" {
		_ = os.Unsetenv(proxyHelper)
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
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, args...)
	cmd.Env = append(os.Environ(), proxyHelper+"=1", EnvSocket+"="+socket, EnvCLI+"="+stub.path, "PROXY_TEST_MARK=1")
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

// Сценарий 1: после «принято» обрыв — до результата или посреди него — код 1
// и строка в stderr; CLI не вызывается, неполный вывод не отдаётся
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
			r := runProxy(t, d.path, stub, []byte(`{}`), "webfetch-fallback")
			if r.code != 1 || r.stdout != "" || !strings.Contains(r.stderr, "после «принято», хук не повторён") {
				t.Fatalf("code %d stdout %q stderr %q", r.code, r.stdout, r.stderr)
			}
			if calls := stub.calls(t); len(calls) != 0 {
				t.Fatalf("CLI called after accept: %q", calls)
			}
		})
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
		return 0, err
	}
	return cred.Pid, nil
}
