package probe

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// mockHook — мок хука: пишет окружение в $HOME/env.txt и строку «лога» по
// пути из HOME, выходит с кодом 3
const mockHook = "#!/usr/bin/env bash\ncat >/dev/null\n" +
	"printf '%s|%s|%s|%s|%s\\n' \"$HOME\" \"$XDG_RUNTIME_DIR\" \"${KONSOLE_DBUS_SERVICE-unset}\" \"${CLAUDE_PID-unset}\" \"${DBUS_SESSION_BUS_ADDRESS-unset}\" >\"$HOME/env.txt\"\n" +
	"mkdir -p \"$HOME/.claude/logs\"\necho probe-line >>\"$HOME/.claude/logs/chooks.log\"\necho stderr-line >&2\nexit 3\n"

func setup(t *testing.T, config string) (realHome, bin string) {
	t.Helper()
	if _, err := exec.LookPath("yq"); err != nil {
		t.Skip("нет yq: скрипт без него отказывается работать")
	}
	root := t.TempDir()
	realHome = filepath.Join(root, "home")
	for _, dir := range []string{".claude", ".config/chooks"} {
		if err := os.MkdirAll(filepath.Join(realHome, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(realHome, ".claude/settings.json"), `{"hooks":{}}`)
	if config != "" {
		writeFile(t, filepath.Join(realHome, ".config/chooks/config.yaml"), config)
	}
	bin = filepath.Join(root, "mock.sh")
	writeFile(t, bin, mockHook)
	if err := os.Chmod(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	return realHome, bin
}

func runProbe(t *testing.T, realHome string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command("./hook-probe.sh", args...)
	cmd.Env = append(os.Environ(), "HOME="+realHome, "TMPDIR="+t.TempDir(),
		"KONSOLE_DBUS_SERVICE=must-be-removed", "CLAUDE_PID=must-be-removed", "DBUS_SESSION_BUS_ADDRESS=unix:path=/must-be-removed")
	cmd.Stdin = strings.NewReader(`{"session_id":"probe"}`)
	out, err := cmd.CombinedOutput()
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("ожидался код выхода хука, получено %v: %s", err, out)
	}
	return string(out), exitErr.ExitCode()
}

// Проба идёт в отдельном HOME: лог, конфиг, состояние — в её папке, а
// настоящий HOME (его лог считает Сон) не трогается
func TestHookProbeIsolation(t *testing.T) {
	realHome, bin := setup(t, "logger:\n  output: \"file\"\n  file: \"~/.claude/logs/chooks.log\"\n")
	dir := filepath.Join(t.TempDir(), "probe")
	out, code := runProbe(t, realHome, "--bin", bin, "--dir", dir, "user-prompt-submit")
	if code != 3 {
		t.Fatalf("код выхода хука должен пройти насквозь (3), получено %d: %s", code, out)
	}
	env, err := os.ReadFile(filepath.Join(dir, "env.txt"))
	if err != nil {
		t.Fatal(err)
	}
	want := dir + "|" + filepath.Join(dir, "run") + "|unset|unset|unset"
	if strings.TrimSpace(string(env)) != want {
		t.Errorf("окружение хука: %q, ожидалось %q", env, want)
	}
	if strings.Contains(out, "stderr-line") {
		t.Errorf("stderr хука (с заголовком окна) попал в терминал: %s", out)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "stderr")); err != nil || !strings.Contains(string(data), "stderr-line") {
		t.Errorf("stderr хука не в папке пробы: %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".claude/settings.json")); err != nil {
		t.Errorf("settings.json не скопирован (таймауты для «hook slow»): %v", err)
	}
	if _, err := os.Stat(filepath.Join(realHome, ".claude/logs")); !os.IsNotExist(err) {
		t.Errorf("проба тронула настоящий лог: %v", err)
	}
}

// Какой бы путь лога ни стоял в конфиге (одинарные кавычки, flow-стиль,
// относительный), копия ведёт лог в папку пробы, а звук и уведомления
// выключены
func TestHookProbeForcesLogAndQuietNotifier(t *testing.T) {
	for name, config := range map[string]string{
		"single quotes": "logger:\n  file: '/var/log/live.log'\ntools:\n  notifier:\n    enabled: true\n    sound: true\n    desktop: true\n",
		"flow style":    "logger: {output: file, file: /var/log/live.log}\n",
		"relative":      "logger:\n  file: logs/live.log\n",
		"no config":     "",
	} {
		t.Run(name, func(t *testing.T) {
			realHome, bin := setup(t, config)
			dir := filepath.Join(t.TempDir(), "probe")
			runProbe(t, realHome, "--bin", bin, "--dir", dir, "stop")
			got, err := exec.Command("yq", ".logger.file, .logger.output, .tools.notifier.sound, .tools.notifier.desktop",
				filepath.Join(dir, ".config/chooks/config.yaml")).Output()
			if err != nil {
				t.Fatal(err)
			}
			if want := "~/.claude/logs/chooks.log\nfile\nfalse\nfalse\n"; string(got) != want {
				t.Errorf("конфиг пробы: %q, ожидалось %q", got, want)
			}
		})
	}
}

// Без --dir папка пробы временная: её лог печатается, папка удаляется
func TestHookProbeTempDirRemoved(t *testing.T) {
	realHome, bin := setup(t, "")
	tmp := t.TempDir()
	cmd := exec.Command("./hook-probe.sh", "--bin", bin, "notification")
	cmd.Env = append(os.Environ(), "HOME="+realHome, "TMPDIR="+tmp)
	cmd.Stdin = strings.NewReader(`{}`)
	out, _ := cmd.CombinedOutput()
	if !strings.Contains(string(out), "probe-line") || !strings.Contains(string(out), "код выхода: 3") {
		t.Errorf("лог и код пробы не напечатаны: %s", out)
	}
	if entries, err := os.ReadDir(tmp); err != nil || len(entries) != 0 {
		t.Errorf("временная папка пробы не удалена: %v, %v", entries, err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
