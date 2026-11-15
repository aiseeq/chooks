package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/akit/proc"
	"github.com/aiseeq/chooks/internal/core"
	"github.com/aiseeq/chooks/internal/nighthint"
)

func nightTestDeps(t *testing.T, now time.Time, locked bool, lockErr error) nighthint.Deps {
	t.Helper()
	return nighthint.Deps{
		Now:    func() time.Time { return now },
		Locked: func(context.Context) (bool, error) { return locked, lockErr },
		Store:  nighthint.Store{Dir: filepath.Join(t.TempDir(), "night-hint")},
	}
}

func nightTestLogger(t *testing.T) (core.Logger, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "chooks.log")
	logger, err := core.NewLogger(core.LoggerConfig{Level: "debug", Output: "file", LogFile: path})
	if err != nil {
		t.Fatal(err)
	}
	return logger, path
}

func TestRunNightHintOutput(t *testing.T) {
	t.Setenv("CLAUDE_PID", "")
	night := time.Date(2026, 11, 15, 23, 41, 0, 0, time.Local)
	cases := []struct {
		name, input string
	}{
		{"SessionStart", `{"hook_event_name":"SessionStart","session_id":"s1","source":"resume"}`},
		{"UserPromptSubmit", `{"hook_event_name":"UserPromptSubmit","session_id":"s1","prompt":"письмо"}`},
		{"PreToolUse", `{"hook_event_name":"PreToolUse","session_id":"s1","tool_name":"AskUserQuestion","tool_input":{}}`},
		{"Stop", `{"hook_event_name":"Stop","session_id":"s1","stop_hook_active":false,"last_assistant_message":"❓ берём вариант A?"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			logger, logPath := nightTestLogger(t)
			var out bytes.Buffer
			runNightHint(context.Background(), []byte(c.input), core.DefaultConfig(), logger, &out, nightTestDeps(t, night, true, nil))
			var got hookContextOutput
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatalf("вывод %q: %v", out.String(), err)
			}
			if got.HookSpecificOutput.HookEventName != c.name || got.HookSpecificOutput.AdditionalContext != nighthint.Text(night) {
				t.Errorf("вывод = %+v", got)
			}
			// Не отказ: ни decision, ни permissionDecision
			if strings.Contains(out.String(), "decision") || strings.Contains(out.String(), "Decision") {
				t.Errorf("в выводе решение: %s", out.String())
			}
			if log := readLog(t, logPath); !strings.Contains(log, "night hint given") {
				t.Errorf("в логе нет строки подсказки: %s", log)
			}
		})
	}
}

// source из stdin доходит до решения: после сжатия той же ночью подсказка
// снова, после resume — нет
func TestRunNightHintAfterCompact(t *testing.T) {
	t.Setenv("CLAUDE_PID", "")
	night := time.Date(2026, 11, 15, 23, 41, 0, 0, time.Local)
	deps := nightTestDeps(t, night, true, nil)
	for _, c := range []struct {
		source string
		want   bool
	}{{"startup", true}, {"resume", false}, {"compact", true}} {
		logger, _ := nightTestLogger(t)
		var out bytes.Buffer
		input := `{"hook_event_name":"SessionStart","session_id":"s1","source":"` + c.source + `"}`
		runNightHint(context.Background(), []byte(input), core.DefaultConfig(), logger, &out, deps)
		if got := out.Len() > 0; got != c.want {
			t.Errorf("source %s: подсказка %v, нужно %v (%q)", c.source, got, c.want, out.String())
		}
	}
}

func TestRunNightHintSilent(t *testing.T) {
	t.Setenv("CLAUDE_PID", "")
	night := time.Date(2026, 11, 15, 23, 41, 0, 0, time.Local)
	ask := `{"hook_event_name":"PreToolUse","session_id":"s1","tool_name":"AskUserQuestion"}`

	t.Run("lock error", func(t *testing.T) {
		logger, logPath := nightTestLogger(t)
		var out bytes.Buffer
		runNightHint(context.Background(), []byte(ask), core.DefaultConfig(), logger, &out, nightTestDeps(t, night, true, errors.New("no system bus")))
		if out.Len() != 0 {
			t.Errorf("подсказка при ошибке признака: %s", out.String())
		}
		log := readLog(t, logPath)
		if !strings.Contains(log, "level=WARN") || !strings.Contains(log, "screen lock state unknown") || !strings.Contains(log, "no system bus") {
			t.Errorf("в логе нет предупреждения с причиной: %s", log)
		}
	})

	t.Run("damaged record", func(t *testing.T) {
		logger, logPath := nightTestLogger(t)
		deps := nightTestDeps(t, night, true, nil)
		if err := os.MkdirAll(deps.Store.Dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(deps.Store.Dir, "s1.json"), []byte(`{"night`), 0o600); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		runNightHint(context.Background(), []byte(`{"hook_event_name":"SessionStart","session_id":"s1"}`), core.DefaultConfig(), logger, &out, deps)
		log := readLog(t, logPath)
		if out.Len() == 0 || !strings.Contains(log, "level=WARN") || !strings.Contains(log, "night hint record replaced") {
			t.Errorf("вывод %q, лог %s", out.String(), log)
		}
	})

	t.Run("disabled", func(t *testing.T) {
		logger, _ := nightTestLogger(t)
		config := core.DefaultConfig()
		config.Tools[nightHintTool] = core.ToolConfig{Enabled: false}
		var out bytes.Buffer
		runNightHint(context.Background(), []byte(ask), config, logger, &out, nightTestDeps(t, night, true, nil))
		if out.Len() != 0 {
			t.Errorf("выключенный хук дал подсказку: %s", out.String())
		}
	})

	t.Run("unreadable input", func(t *testing.T) {
		logger, logPath := nightTestLogger(t)
		var out bytes.Buffer
		runNightHint(context.Background(), []byte(`{`), core.DefaultConfig(), logger, &out, nightTestDeps(t, night, true, nil))
		log := readLog(t, logPath)
		if out.Len() != 0 || !strings.Contains(log, "night hint input unreadable") || strings.Contains(log, "night hint failed") {
			t.Errorf("нечитаемый вход: вывод %q, лог %s", out.String(), log)
		}
	})

	for name, input := range map[string]string{
		"stop without question": `{"hook_event_name":"Stop","session_id":"s1","last_assistant_message":"Сделано."}`,
		"other tool":            `{"hook_event_name":"PreToolUse","session_id":"s1","tool_name":"Bash"}`,
	} {
		t.Run(name, func(t *testing.T) {
			logger, _ := nightTestLogger(t)
			var out bytes.Buffer
			runNightHint(context.Background(), []byte(input), core.DefaultConfig(), logger, &out, nightTestDeps(t, night, true, nil))
			if out.Len() != 0 {
				t.Errorf("подсказка: %s", out.String())
			}
		})
	}

	t.Run("unsupported event", func(t *testing.T) {
		logger, logPath := nightTestLogger(t)
		var out bytes.Buffer
		runNightHint(context.Background(), []byte(`{"hook_event_name":"PostToolUse","session_id":"s1"}`), core.DefaultConfig(), logger, &out, nightTestDeps(t, night, true, nil))
		log := readLog(t, logPath)
		if out.Len() != 0 || !strings.Contains(log, "level=WARN") || !strings.Contains(log, "night hint failed") {
			t.Errorf("незнакомое событие: вывод %q, лог %s", out.String(), log)
		}
	})

	t.Run("unlocked", func(t *testing.T) {
		logger, logPath := nightTestLogger(t)
		var out bytes.Buffer
		runNightHint(context.Background(), []byte(ask), core.DefaultConfig(), logger, &out, nightTestDeps(t, night, false, nil))
		if out.Len() != 0 || !strings.Contains(readLog(t, logPath), "экран не заблокирован") {
			t.Errorf("разблокированный экран: вывод %q", out.String())
		}
	})

	t.Run("print mode", func(t *testing.T) {
		pid := printModeProcess(t)
		t.Setenv("CLAUDE_PID", pid)
		logger, logPath := nightTestLogger(t)
		var out bytes.Buffer
		runNightHint(context.Background(), []byte(ask), core.DefaultConfig(), logger, &out, nightTestDeps(t, night, true, nil))
		if out.Len() != 0 || !strings.Contains(readLog(t, logPath), "неинтерактивная сессия") {
			t.Errorf("claude -p: вывод %q", out.String())
		}
	})
}

// printModeProcess — живой процесс с -p в командной строке, как у claude -p.
// Строка ready приходит после exec: до него в cmdline — бинарь теста
func printModeProcess(t *testing.T) string {
	t.Helper()
	cmd := exec.Command("sh", "-c", "echo ready; sleep 30; :", "-p")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := bufio.NewReader(stdout).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := proc.StopChild(cmd.Process.Pid, 0, func() error { return cmd.Process.Kill() }); err != nil {
			t.Errorf("процесс %d не остановлен: %v", cmd.Process.Pid, err)
		}
		_ = cmd.Wait()
	})
	return strconv.Itoa(cmd.Process.Pid)
}
