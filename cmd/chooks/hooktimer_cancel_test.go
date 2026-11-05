package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/chooks/internal/core"
)

// Отмена — INFO и cancelled, таймаут — WARN и killed: dreamstats считает убитых
// в логе по подстроке сообщения, а отмена не должна заводить ночное задание
func TestHookTimer_ReportTermination(t *testing.T) {
	cases := []struct {
		name         string
		hook         string
		elapsed      time.Duration
		status       string
		wantInfo     string
		wantWarnPart string
	}{
		{"отмена", "question-check", 1055 * time.Millisecond, statusCancelled, "hook cancelled by SIGTERM before timeout", ""},
		{"таймаут", "user-prompt-submit", 3100 * time.Millisecond, statusKilled, "", "hook killed by SIGTERM (timeout)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			timer, clock, logger := newTestTimer(t, c.hook, upsSettings)
			logDir := t.TempDir()
			timer.setDurations(core.LoggerConfig{Output: "file", LogFile: filepath.Join(logDir, "chooks.log")})
			clock.advance(c.elapsed)
			timer.reportTermination()

			if got := strings.Join(logger.infos, "|"); got != c.wantInfo {
				t.Errorf("INFO %q, ожидался %q", got, c.wantInfo)
			}
			got := strings.Join(logger.warns, "|")
			if c.wantWarnPart == "" && got != "" || !strings.Contains(got, c.wantWarnPart) {
				t.Errorf("WARN %q, ожидалось %q", got, c.wantWarnPart)
			}
			line, err := os.ReadFile(filepath.Join(logDir, durationsFile))
			if err != nil {
				t.Fatal(err)
			}
			fields := strings.Split(strings.TrimSpace(string(line)), "\t")
			if len(fields) != 6 || fields[5] != c.status {
				t.Errorf("строка длительности %q, ожидался статус %s", line, c.status)
			}
		})
	}
}

// Claude Code шлёт SIGTERM и на отмену хода (Esc), не только по таймауту:
// 04.11 understand-check получил его на 1055 мс при таймауте 40 с
func TestHookTimer_TerminationOutcome(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.json")
	body := `{"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"$HOME/bin/chooks understand-check","timeout":40}]}],` +
		`"PostToolBatch":[{"hooks":[{"type":"command","command":"$HOME/bin/chooks post-tool-batch","timeout":3}]}]}}`
	if err := os.WriteFile(settings, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	found := func() (string, error) { return settings, nil }
	cases := []struct {
		name    string
		hook    string
		path    func() (string, error)
		elapsed time.Duration
		status  string
		wantErr bool
	}{
		{"отмена задолго до таймаута", "understand-check", found, 1055 * time.Millisecond, statusCancelled, false},
		{"таймаут", "post-tool-batch", found, 3100 * time.Millisecond, statusKilled, false},
		{"таймаут пришёл раньше номинала: таймер Claude Code стартует раньше процесса", "post-tool-batch", found, 2500 * time.Millisecond, statusKilled, false},
		{"отмена короткого хука", "post-tool-batch", found, 1500 * time.Millisecond, statusCancelled, false},
		{"хука нет в settings", "no-such-hook", found, time.Second, statusKilled, true},
		{"settings не найден", "post-tool-batch", func() (string, error) { return "", errors.New("no home") }, time.Second, statusKilled, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			timer := &hookTimer{hook: c.hook, settingsPath: c.path}
			status, err := timer.terminationStatus(c.elapsed)
			if status != c.status {
				t.Errorf("статус %q, ожидался %q", status, c.status)
			}
			if (err != nil) != c.wantErr {
				t.Errorf("ошибка %v, ожидалась: %v", err, c.wantErr)
			}
		})
	}
}
