package core

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNewLogger_WritesToFile(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "logs", "hooks.log")

	logger, err := NewLogger(LoggerConfig{Level: "info", Format: "text", Output: "file", LogFile: logFile})
	if err != nil {
		t.Fatalf("не удалось создать логгер: %v", err)
	}
	logger.Info("test message", "key", "value")

	content, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("лог-файл не создан: %v", err)
	}
	if !strings.Contains(string(content), "test message") {
		t.Errorf("сообщение не записано: %s", content)
	}
}

// Без ротации лог-файл растет неограниченно: хук вызывается на каждую операцию
func TestNewLogger_RotatesOversizedFile(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "hooks.log")
	oversized := strings.Repeat("x", 2*1024*1024)
	if err := os.WriteFile(logFile, []byte(oversized), 0o644); err != nil {
		t.Fatalf("не удалось создать лог-файл: %v", err)
	}

	logger, err := NewLogger(LoggerConfig{Level: "info", Format: "text", Output: "file", LogFile: logFile, MaxSizeMB: 1})
	if err != nil {
		t.Fatalf("не удалось создать логгер: %v", err)
	}
	logger.Info("after rotation")

	if _, err := os.Stat(logFile + ".1"); err != nil {
		t.Errorf("предыдущий лог должен быть сохранен как .1: %v", err)
	}

	info, err := os.Stat(logFile)
	if err != nil {
		t.Fatalf("новый лог не создан: %v", err)
	}
	if info.Size() >= int64(len(oversized)) {
		t.Error("после ротации лог должен начинаться заново")
	}
}

func TestNewLogger_KeepsFileWithinLimit(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "hooks.log")
	if err := os.WriteFile(logFile, []byte("existing entry\n"), 0o644); err != nil {
		t.Fatalf("не удалось создать лог-файл: %v", err)
	}

	logger, err := NewLogger(LoggerConfig{Level: "info", Format: "text", Output: "file", LogFile: logFile, MaxSizeMB: 1})
	if err != nil {
		t.Fatalf("не удалось создать логгер: %v", err)
	}
	logger.Info("appended entry")

	content, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("не удалось прочитать лог: %v", err)
	}
	if !strings.Contains(string(content), "existing entry") {
		t.Error("небольшой лог не должен ротироваться")
	}
}

// stdout занят протоколом обмена с Claude Code
func TestNewLogger_StdoutFallsBackToStderr(t *testing.T) {
	if _, err := NewLogger(LoggerConfig{Level: "info", Format: "text", Output: "stdout"}); err != nil {
		t.Fatalf("не удалось создать логгер: %v", err)
	}
}

func TestNewLogger_FileOutputRequiresPath(t *testing.T) {
	if _, err := NewLogger(LoggerConfig{Level: "info", Format: "text", Output: "file"}); err == nil {
		t.Error("output: file без пути должен приводить к ошибке")
	}
}

func TestAppendLine_Rotates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "durations.tsv")
	if err := AppendLine(path, 1, "first"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, 1024*1024), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := AppendLine(path, 1, "second"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "second\n" {
		t.Errorf("после ротации в файле %q", data)
	}
	if info, err := os.Stat(path + ".1"); err != nil || info.Size() != 1024*1024 {
		t.Errorf("прежний файл не ушёл в .1: %v", err)
	}
}

// Сбой ротации строку не роняет: она дописывается в текущий файл, ошибка
// ротации возвращается вызывающему
func TestAppendLineWritesWhenRotationFails(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T, path string) func(string, string) error{
		"rename": func(t *testing.T, path string) func(string, string) error {
			return func(string, string) error { return errors.New("rename refused") }
		},
		"lock file": func(t *testing.T, path string) func(string, string) error {
			if err := os.WriteFile(path+".lock", nil, 0o000); err != nil {
				t.Fatal(err)
			}
			return os.Rename
		},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "durations.tsv")
			if err := os.WriteFile(path, make([]byte, 1024*1024), 0o644); err != nil {
				t.Fatal(err)
			}
			rename := setup(t, path)
			if err := appendLineWithRename(context.Background(), path, 1, "kept", rename); err == nil {
				t.Error("ошибка ротации не возвращена")
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasSuffix(string(data), "kept\n") {
				t.Errorf("строка не дописана: хвост %q", data[max(0, len(data)-16):])
			}
		})
	}
}

func TestAppendLineConcurrentRotationPreservesHistoryAndLines(t *testing.T) {
	for _, lines := range [][]string{{"line-A", "line-B"}, {"line-A", "line-B", "line-C"}} {
		t.Run(strings.Join(lines, "-"), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "durations.tsv")
			original := bytes.Repeat([]byte("x"), 1<<20)
			original = append(original, []byte("\nOLD\n")...)
			if err := os.WriteFile(path, original, 0o644); err != nil {
				t.Fatal(err)
			}

			arrived := make(chan struct{}, len(lines))
			release := make(chan struct{})
			rename := func(old, new string) error {
				arrived <- struct{}{}
				<-release
				return os.Rename(old, new)
			}
			start := make(chan struct{})
			errs := make(chan error, len(lines))
			for _, line := range lines {
				go func(line string) {
					<-start
					errs <- appendLineWithRename(context.Background(), path, 1, line, rename)
				}(line)
			}
			close(start)
			arrivals := 0
			timer := time.NewTimer(100 * time.Millisecond)
		waitForRenameCalls:
			for arrivals < len(lines) {
				select {
				case <-arrived:
					arrivals++
				case <-timer.C:
					break waitForRenameCalls
				}
			}
			timer.Stop()
			close(release)
			for range lines {
				if err := <-errs; err != nil {
					t.Errorf("append failed during concurrent rotation: %v", err)
				}
			}

			current, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			previous, err := os.ReadFile(path + ".1")
			if err != nil {
				t.Fatal(err)
			}
			combined := string(current) + string(previous)
			for _, line := range append(append([]string{}, lines...), "OLD") {
				if count := strings.Count(combined, line+"\n"); count != 1 {
					t.Errorf("line %q occurs %d times across current and rotated logs", line, count)
				}
			}
			if !strings.Contains(string(previous), "OLD\n") {
				t.Errorf(".1 must retain the previous oversized log, got tail %q", previous[len(previous)-min(len(previous), 32):])
			}
			if len(previous) < 1<<20 {
				t.Errorf(".1 contains %d bytes; expected the original oversized log", len(previous))
			}
		})
	}
}

// Маска стоит в логгере: поле с куском команды (message, error, value) не проходит мимо неё
func TestLogger_HidesSecretsInEveryField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chooks.log")
	logger, err := NewLogger(LoggerConfig{Level: "debug", Output: "file", LogFile: path})
	if err != nil {
		t.Fatal(err)
	}
	logger.Warn("hook blocked", "message", `модель "password=hunter2pass" не одобрена`, "count", 7)
	logger.Warn("hook blocked", "message", `модель "x\ncurl -u admin:hunter2pass https://x" не одобрена`)
	logger.Error("broken", "error", errors.New("PGPASSWORD=hunter2pass psql"))
	logger.With("value", `{"password":"hunter2pass"}`).Info("with field")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	log := string(data)
	if strings.Contains(log, "hunter2pass") {
		t.Errorf("secret value in the log:\n%s", log)
	}
	for _, want := range []string{"level=WARN", "level=ERROR", "level=INFO", "count=7", "[скрыто:password]", "hook blocked", "with field"} {
		if !strings.Contains(log, want) {
			t.Errorf("no %q in the log:\n%s", want, log)
		}
	}
}

// Ревью 2б-1, п. 1: логгер демона после ротации, сделанной другим процессом
// (CLI переименовал chooks.log в chooks.log.1), пишет в новый chooks.log, а
// сам ротирует файл по порогу, как AppendLine
func TestLongLivedLoggerFollowsRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chooks.log")
	logger, err := NewLongLivedLoggerIn(LoggerConfig{Level: "info", Output: "file", LogFile: path, MaxSizeMB: 1}, io.Discard, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer logger.Close()
	logger.Info("before rotation")
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	logger.Info("after rotation")
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	old, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(current), "after rotation") || strings.Contains(string(current), "before rotation") || strings.Contains(string(old), "after rotation") {
		t.Fatalf("current %q\nold %q", current, old)
	}
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	logger.Info("over the threshold")
	if current, err = os.ReadFile(path); err != nil || strings.TrimSpace(string(current)) == "" || !strings.Contains(string(current), "over the threshold") || len(current) > 1<<10 {
		t.Fatalf("after own rotation: %d bytes %v", len(current), err)
	}
}
