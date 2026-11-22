package desktop

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aiseeq/chooks/internal/hookctx"
)

// fakeProc строит дерево /proc: pid → (comm, ppid)
func fakeProc(t *testing.T, tree map[int]struct {
	comm string
	ppid int
}) string {
	t.Helper()
	root := t.TempDir()
	for pid, p := range tree {
		dir := filepath.Join(root, fmt.Sprint(pid))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		stat := fmt.Sprintf("%d (%s) S %d 0 0 0\n", pid, p.comm, p.ppid)
		if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "comm"), []byte(p.comm+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// Вкладку Konsole решает дерево процессов от PID запроса: у запроса из
// konsole заголовок идёт через D-Bus (шины нет — ошибка), у запроса из tmux —
// OSC в его stderr. Запросы идут вместе, каждый со своим PID
func TestTerminalTitleRequestProcessTree(t *testing.T) {
	procDir = fakeProc(t, map[int]struct {
		comm string
		ppid int
	}{
		510: {"claude", 410}, 410: {"zsh", 310}, 310: {"konsole", 1},
		520: {"claude", 420}, 420: {"tmux: server", 1},
	})
	t.Cleanup(func() { procDir = "/proc" })
	bus := "unix:path=" + filepath.Join(t.TempDir(), "no-bus")
	const count = 20
	type result struct {
		stderr string
		err    error
	}
	var got [count]result
	var wg sync.WaitGroup
	for i := range got {
		wg.Go(func() {
			var stderr bytes.Buffer
			pid := 510 + 10*(i%2)
			env := []string{"KONSOLE_DBUS_SERVICE=org.kde.konsole-1", "KONSOLE_DBUS_SESSION=/Sessions/1", "DBUS_SESSION_BUS_ADDRESS=" + bus}
			req := hookctx.New(nil, nil, &stderr, env, "/", pid, pid-90)
			err := SetTerminalTitleForRequest(fmt.Sprint("p", i), req)
			got[i] = result{stderr: stderr.String(), err: err}
		})
	}
	wg.Wait()
	for i, r := range got {
		if i%2 == 0 {
			if r.err == nil || !strings.Contains(r.err.Error(), "session bus unavailable") || r.stderr != "" {
				t.Errorf("request %d (konsole tree): err=%v stderr=%q", i, r.err, r.stderr)
			}
			continue
		}
		if want := fmt.Sprintf("\x1b]0;p%d\a", i); r.err != nil || r.stderr != want {
			t.Errorf("request %d (tmux tree): err=%v stderr=%q want %q", i, r.err, r.stderr, want)
		}
	}
}

// envProbe — скрипт для /bin/sh: пишет окружение и cwd, которые увидел
// дочерний процесс, в out
func envProbe(t *testing.T, out string) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "probe.sh")
	text := fmt.Sprintf("{ env; printf 'CWD=%%s\\n' \"$(pwd -P)\"; } > '%s.tmp' && mv '%s.tmp' '%s'\n", out, out, out)
	if err := os.WriteFile(script, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return script
}

func waitProbe(t *testing.T, out string) map[string]string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		data, err := os.ReadFile(out)
		if err == nil {
			seen := map[string]string{}
			for line := range strings.Lines(string(data)) {
				if name, value, ok := strings.Cut(strings.TrimSuffix(line, "\n"), "="); ok {
					seen[name] = value
				}
			}
			return seen
		}
		if time.Now().After(deadline) {
			t.Fatalf("дочерний процесс не записал %s: %v", out, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Процесс доставки оповещения получает окружение и cwd запроса, а не
// исполнителя хука
func TestDeliverInBackgroundRequestEnvDir(t *testing.T) {
	t.Setenv("CHOOKS_PROBE", "process")
	// Переменная есть только у процесса: в окружение ребёнка она не попадает
	t.Setenv("CHOOKS_PROCESS_ONLY", "leak")
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "seen")
	script := envProbe(t, out)
	req := hookctx.New(nil, nil, nil, []string{"CHOOKS_PROBE=request", "PATH=/usr/bin:/bin"}, cwd, 7, 6)
	if err := DeliverInBackgroundForRequest("/bin/sh", script, Alert{Title: "t"}, req); err != nil {
		t.Fatal(err)
	}
	seen := waitProbe(t, out)
	if seen["CHOOKS_PROBE"] != "request" || seen["PATH"] != "/usr/bin:/bin" || seen["CWD"] != cwd {
		t.Fatalf("child saw CHOOKS_PROBE=%q PATH=%q CWD=%q, want request, /usr/bin:/bin, %q", seen["CHOOKS_PROBE"], seen["PATH"], seen["CWD"], cwd)
	}
	if value, leaked := seen["CHOOKS_PROCESS_ONLY"]; leaked {
		t.Fatalf("child inherited the process-only CHOOKS_PROCESS_ONLY=%q", value)
	}
}
