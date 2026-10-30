package perf

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func run(t *testing.T, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = append(os.Environ(), "TMPDIR="+t.TempDir())
	out, err := cmd.Output()
	if err == nil {
		return string(out), 0
	}
	if exit, ok := err.(*exec.ExitError); ok {
		return string(out), exit.ExitCode()
	}
	t.Fatalf("запуск скрипта: %v", err)
	return "", -1
}

func writeTranscript(t *testing.T, root string, lines ...string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "session.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
}

func stopSummary(ts string, entries ...string) string {
	return fmt.Sprintf(`{"type":"system","subtype":"stop_hook_summary","timestamp":%q,"hookInfos":[%s]}`, ts, strings.Join(entries, ","))
}

func hookInfo(command string, duration int) string {
	return fmt.Sprintf(`{"command":%q,"durationMs":%d}`, command, duration)
}

func TestHookMSPercentileAndFiltering(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	lines := make([]string, 0, 28)
	for i := 1; i <= 25; i++ {
		lines = append(lines, stopSummary(now.Add(-time.Hour).Format(time.RFC3339Nano), hookInfo("/home/test/bin/chooks stop", i)))
	}
	lines = append(lines,
		stopSummary(now.Add(-time.Hour).Format(time.RFC3339Nano), hookInfo("/bin/chooks stop-failure", 9999)),
		stopSummary(now.Add(time.Second).Format(time.RFC3339Nano), hookInfo("/bin/chooks stop", 9999)),
	)
	writeTranscript(t, root, lines...)
	out, code := run(t, "./hook-ms.sh", "--root", root, "--until", strconv.FormatInt(now.Unix(), 10), "--q", "95", "stop")
	if code != 0 || strings.TrimSpace(out) != "24" {
		t.Fatalf("p95: stdout+stderr=%q code=%d, ожидалось 24 и 0", out, code)
	}
}

func TestHookMSLowCountAndMedian(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	ts := now.Add(-time.Hour).Format(time.RFC3339Nano)
	writeTranscript(t, root,
		stopSummary(ts, hookInfo("/bin/chooks stop", 80)),
		stopSummary(ts, hookInfo("/bin/chooks stop", 10)),
		stopSummary(ts, hookInfo("/bin/chooks stop", 30)),
		stopSummary(ts, hookInfo("/bin/chooks stop", 50)),
		stopSummary(ts, hookInfo("/bin/chooks stop", 90)),
	)
	out, code := run(t, "./hook-ms.sh", "--root", root, "--until", strconv.FormatInt(now.Unix(), 10), "stop")
	if code != 3 || strings.TrimSpace(out) != "" {
		t.Fatalf("малое число: stdout+stderr=%q code=%d, ожидалось пусто и 3", out, code)
	}
	out, code = run(t, "./hook-ms.sh", "--root", root, "--until", strconv.FormatInt(now.Unix(), 10), "--q", "50", "stop")
	if code != 0 || strings.TrimSpace(out) != "50" {
		t.Fatalf("медиана: stdout+stderr=%q code=%d, ожидалось 50 и 0", out, code)
	}
}

func TestHookMSLegacyAndHookSuccess(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	ts := now.Add(-time.Hour).Format(time.RFC3339Nano)
	lines := []string{
		fmt.Sprintf(`{"type":"attachment","timestamp":%q,"attachment":{"type":"hook_success","command":"/x/claude-hooks stop","durationMs":14}}`, ts),
		fmt.Sprintf(`{"type":"attachment","timestamp":%q,"attachment":{"type":"hook_success","command":"/x/chooks stop","durationMs":25}}`, ts),
		stopSummary(ts, hookInfo("/x/chooks stop", 36)),
	}
	writeTranscript(t, root, lines...)
	out, code := run(t, "./hook-ms.sh", "--q", "50", "--root", root, "--until", strconv.FormatInt(now.Unix(), 10), "stop")
	if code != 0 || strings.TrimSpace(out) != "25" {
		t.Fatalf("legacy/hook_success: stdout+stderr=%q code=%d, ожидалось 25 и 0", out, code)
	}
}

func TestExecMSIsolationAndFailure(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "mock.sh")
	result := filepath.Join(root, "env.txt")
	content := "#!/usr/bin/env bash\necho 1\nprintf '%s|%s|%s\\n' \"$HOME\" \"${KONSOLE_DBUS_SERVICE-unset}\" \"${CLAUDE_PID-unset}\" >\"$CHECK_FILE\"\nexit \"${MOCK_EXIT:-0}\"\n"
	if err := os.WriteFile(bin, []byte(content), 0700); err != nil {
		t.Fatal(err)
	}
	input := "testdata/statusline.json"
	if _, err := os.Stat(input); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("./exec-ms.sh", "--runs", "1", "--bin", bin, "statusline", input)
	cmd.Env = append(os.Environ(), "TMPDIR="+filepath.Join(root, "tmp"), "CHECK_FILE="+result, "KONSOLE_DBUS_SERVICE=must-be-removed", "CLAUDE_PID=must-be-removed")
	if err := os.Mkdir(filepath.Join(root, "tmp"), 0700); err != nil {
		t.Fatal(err)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("успешный замер: %v: %s", err, out)
	}
	if _, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64); err != nil {
		t.Fatalf("не число в stdout: %q", out)
	}
	data, err := os.ReadFile(result)
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Split(strings.TrimSpace(string(data)), "|")
	if len(fields) != 3 || fields[0] == os.Getenv("HOME") || fields[1] != "unset" || fields[2] != "unset" {
		t.Fatalf("окружение mock бинаря: %q", data)
	}

	cmd = exec.Command("./exec-ms.sh", "--runs", "1", "--bin", bin, "--", "statusline", input)
	cmd.Env = append(os.Environ(), "TMPDIR="+filepath.Join(root, "tmp"), "CHECK_FILE="+result, "MOCK_EXIT=7")
	out, err = cmd.CombinedOutput()
	exit, ok := err.(*exec.ExitError)
	if !ok || exit.ExitCode() != 2 || strings.TrimSpace(string(out)) != "Ошибка запуска подкоманды: statusline" {
		t.Fatalf("ошибка подкоманды: out=%q err=%v", out, err)
	}
}
