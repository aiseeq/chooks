package accounts

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/chooks/internal/hookctx"
)

// Фоновая ротация получает окружение и cwd запроса строки статуса, а не
// исполнителя: в демоне это были бы его HOME и CLAUDE_CONFIG_DIR
func TestMaybeRotateRequestEnvDir(t *testing.T) {
	t.Setenv("CHOOKS_PROBE", "process")
	// Переменная есть только у процесса: в окружение ребёнка она не попадает
	t.Setenv("CHOOKS_PROCESS_ONLY", "leak")
	root := t.TempDir()
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	paths := Paths{ClaudeDir: filepath.Join(root, "claude"), GlobalConfig: filepath.Join(root, "claude.json"), StateDir: filepath.Join(root, "state")}
	if err := os.MkdirAll(paths.ClaudeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Файл токенов изменился после сверки и давно не меняется: ротации есть работа
	if err := os.WriteFile(paths.Credentials(), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := os.Chtimes(paths.Credentials(), now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(root, "seen")
	script := filepath.Join(root, "probe.sh")
	text := fmt.Sprintf("{ env; printf 'CWD=%%s\\n' \"$(pwd -P)\"; } > '%s.tmp' && mv '%s.tmp' '%s'\n", out, out, out)
	if err := os.WriteFile(script, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	env := []string{"CHOOKS_PROBE=request", "PATH=/usr/bin:/bin", "HOME=" + root}
	req := hookctx.New(nil, nil, nil, env, cwd, 7, 6)
	why, err := MaybeRotate(paths, &Store{}, nil, now, "/bin/sh", req, script)
	if err != nil || why == "" {
		t.Fatalf("rotation not started: why=%q err=%v", why, err)
	}
	deadline := now.Add(10 * time.Second)
	var data []byte
	for {
		if data, err = os.ReadFile(out); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("дочерний процесс не записал %s: %v", out, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	seen := map[string]string{}
	for line := range strings.Lines(string(data)) {
		if name, value, ok := strings.Cut(strings.TrimSuffix(line, "\n"), "="); ok {
			seen[name] = value
		}
	}
	if seen["CHOOKS_PROBE"] != "request" || seen["HOME"] != root || seen["CWD"] != cwd {
		t.Fatalf("child saw CHOOKS_PROBE=%q HOME=%q CWD=%q, want request, %q, %q", seen["CHOOKS_PROBE"], seen["HOME"], seen["CWD"], root, cwd)
	}
	if value, leaked := seen["CHOOKS_PROCESS_ONLY"]; leaked {
		t.Fatalf("child inherited the process-only CHOOKS_PROCESS_ONLY=%q", value)
	}
}

// Удалённый cwd запроса: у CLI ротация наследует удалённый cwd и стартует,
// у демона стартует из "/"
func TestMaybeRotateDeletedCwd(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "gone")
	if err := os.Mkdir(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(cwd); err != nil {
		t.Fatal(err)
	}
	paths := Paths{ClaudeDir: filepath.Join(root, "claude"), GlobalConfig: filepath.Join(root, "claude.json"), StateDir: filepath.Join(root, "state")}
	if err := os.MkdirAll(paths.ClaudeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.Credentials(), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := os.Chtimes(paths.Credentials(), now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(root, "probe.sh")
	if err := os.WriteFile(script, []byte("exit 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	req := hookctx.New(nil, nil, nil, []string{"PATH=/usr/bin:/bin", "HOME=" + root}, cwd, 7, 6)
	if why, err := MaybeRotate(paths, &Store{}, nil, now, "/bin/sh", req, script); err != nil || why == "" {
		t.Fatalf("rotation not started from a deleted cwd: why=%q err=%v", why, err)
	}
}
