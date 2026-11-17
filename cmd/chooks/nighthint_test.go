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
	"github.com/aiseeq/chooks/internal/skillreload"
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

// writeSkillState пишет запись skill-reload главного агента сессии s1 со
// скиллами skills в каталог под XDG_STATE_HOME
func writeSkillState(t *testing.T, skills ...string) {
	t.Helper()
	dir, err := skillreload.DefaultStateDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string]any{}
	for _, name := range skills {
		path := filepath.Join("/skills", name, "SKILL.md")
		files[path] = map[string]any{"kind": "skill", "name": name, "path": path}
	}
	data, err := json.Marshal(map[string]any{"schema": 2, "transcript": "/t/s1.jsonl", "offset": 10, "files": files})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "s1.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Ночь, /clear, ход по письму сессии: подсказка на каждом UserPromptSubmit,
// пока в записи skill-reload нет autonomous-work; после вызова — нет.
// Без записи, при выключенном skill_reload и при сбое чтения — раз за ночь
// (тикет night-hint-after-clear-first-turn)
func TestRunNightHintUntilSkillCalled(t *testing.T) {
	t.Setenv("CLAUDE_PID", "")
	night := time.Date(2026, 11, 17, 5, 6, 0, 0, time.Local)
	start := `{"hook_event_name":"SessionStart","session_id":"s1","source":"clear"}`
	prompt := `{"hook_event_name":"UserPromptSubmit","session_id":"s1","prompt":"письмо сессии"}`
	run := func(t *testing.T, config *core.Config, deps nighthint.Deps, input string) (bool, string) {
		t.Helper()
		logger, logPath := nightTestLogger(t)
		var out bytes.Buffer
		runNightHint(context.Background(), []byte(input), config, logger, &out, deps)
		return out.Len() > 0, readLog(t, logPath)
	}

	t.Run("repeat until called", func(t *testing.T) {
		t.Setenv("XDG_STATE_HOME", t.TempDir())
		config := core.DefaultConfig()
		deps := nightTestDeps(t, night, true, nil)
		deps.SkillCalled = autonomousSkillCalled(config)
		writeSkillState(t, "coordinator")
		if given, _ := run(t, config, deps, start); !given {
			t.Fatal("SessionStart(clear) без подсказки")
		}
		for i := range 2 {
			if given, log := run(t, config, deps, prompt); !given {
				t.Fatalf("ход %d без вызова скилла: подсказки нет, лог %s", i+1, log)
			}
		}
		writeSkillState(t, "coordinator", nighthint.AutonomousSkill)
		if given, log := run(t, config, deps, prompt); given || strings.Contains(log, "skill call unknown") {
			t.Errorf("после вызова скилла: подсказка %t, лог %s", given, log)
		}
	})

	for _, c := range []struct {
		name, level string
		prepare     func(t *testing.T, config *core.Config)
	}{
		{"no record", "level=DEBUG", func(*testing.T, *core.Config) {}},
		{"skill_reload disabled", "level=DEBUG", func(t *testing.T, config *core.Config) {
			writeSkillState(t)
			config.Tools[skillReloadTool] = core.ToolConfig{Enabled: false}
		}},
		{"broken record", "level=WARN", func(t *testing.T, _ *core.Config) {
			dir, err := skillreload.DefaultStateDir()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "s1.json"), []byte(`{"schema":2,"files":`), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			config := core.DefaultConfig()
			c.prepare(t, config)
			deps := nightTestDeps(t, night, true, nil)
			deps.SkillCalled = autonomousSkillCalled(config)
			if given, _ := run(t, config, deps, start); !given {
				t.Fatal("SessionStart(clear) без подсказки")
			}
			given, log := run(t, config, deps, prompt)
			if given {
				t.Error("повтор подсказки без признака вызова")
			}
			if !strings.Contains(log, c.level+` msg="night hint once per night: skill call unknown"`) {
				t.Errorf("в логе нет причины уровня %s: %s", c.level, log)
			}
		})
	}

	t.Run("day", func(t *testing.T) {
		t.Setenv("XDG_STATE_HOME", t.TempDir())
		config := core.DefaultConfig()
		writeSkillState(t)
		deps := nightTestDeps(t, time.Date(2026, 11, 17, 14, 0, 0, 0, time.Local), true, nil)
		deps.SkillCalled = autonomousSkillCalled(config)
		for i := range 2 {
			if given, _ := run(t, config, deps, prompt); given {
				t.Errorf("ход %d днём: подсказка", i+1)
			}
		}
	})
}
