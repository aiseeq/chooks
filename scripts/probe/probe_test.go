package probe

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Проба идёт в отдельном HOME: лог, конфиг, состояние — в её папке, а
// настоящий HOME (его считает Сон) не трогается
func TestHookProbeIsolation(t *testing.T) {
	root := t.TempDir()
	realHome := filepath.Join(root, "home")
	for _, dir := range []string{".claude", ".config/chooks"} {
		if err := os.MkdirAll(filepath.Join(realHome, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(realHome, ".claude/settings.json"), `{"hooks":{}}`)
	writeFile(t, filepath.Join(realHome, ".config/chooks/config.yaml"), "logger:\n  output: \"file\"\n  file: \"~/.claude/logs/chooks.log\"\n")

	// Мок хука: пишет окружение и строку «лога» по пути из HOME
	bin := filepath.Join(root, "mock.sh")
	writeFile(t, bin, "#!/usr/bin/env bash\ncat >/dev/null\nprintf '%s|%s|%s|%s\\n' \"$HOME\" \"$XDG_RUNTIME_DIR\" \"${KONSOLE_DBUS_SERVICE-unset}\" \"${CLAUDE_PID-unset}\" >\"$HOME/env.txt\"\necho line >>\"$HOME/.claude/logs/chooks.log\"\nexit 3\n")
	if err := os.Chmod(bin, 0o700); err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(root, "probe")
	cmd := exec.Command("./hook-probe.sh", "--bin", bin, "--dir", dir, "user-prompt-submit")
	cmd.Env = append(os.Environ(), "HOME="+realHome, "KONSOLE_DBUS_SERVICE=must-be-removed", "CLAUDE_PID=must-be-removed")
	cmd.Stdin = strings.NewReader(`{"session_id":"probe"}`)
	out, err := cmd.CombinedOutput()
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 3 {
		t.Fatalf("код выхода хука должен пройти насквозь (3), получено %v: %s", err, out)
	}

	env, err := os.ReadFile(filepath.Join(dir, "env.txt"))
	if err != nil {
		t.Fatal(err)
	}
	want := dir + "|" + filepath.Join(dir, "run") + "|unset|unset"
	if strings.TrimSpace(string(env)) != want {
		t.Errorf("окружение хука: %q, ожидалось %q", env, want)
	}
	if _, err := os.Stat(filepath.Join(dir, ".claude/logs/chooks.log")); err != nil {
		t.Errorf("лог пробы не в её папке: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".claude/settings.json")); err != nil {
		t.Errorf("settings.json не скопирован (таймауты для «hook slow»): %v", err)
	}
	if _, err := os.Stat(filepath.Join(realHome, ".claude/logs")); !os.IsNotExist(err) {
		t.Errorf("проба тронула настоящий лог: %v", err)
	}
}

// Абсолютный путь лога в конфиге увёл бы пробу в живой лог — отказ
func TestHookProbeRefusesAbsoluteLog(t *testing.T) {
	root := t.TempDir()
	realHome := filepath.Join(root, "home")
	if err := os.MkdirAll(filepath.Join(realHome, ".config/chooks"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(realHome, ".config/chooks/config.yaml"), "logger:\n  file: \"/var/log/chooks.log\"\n")
	bin := filepath.Join(root, "mock.sh")
	writeFile(t, bin, "#!/usr/bin/env bash\ntouch \"$HOME/ran\"\n")
	if err := os.Chmod(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "probe")
	cmd := exec.Command("./hook-probe.sh", "--bin", bin, "--dir", dir, "stop")
	cmd.Env = append(os.Environ(), "HOME="+realHome)
	out, err := cmd.CombinedOutput()
	if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 2 {
		t.Fatalf("ожидался отказ с кодом 2, получено %v: %s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "ran")); !os.IsNotExist(err) {
		t.Error("хук запущен, хотя конфиг вёл бы в живой лог")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
