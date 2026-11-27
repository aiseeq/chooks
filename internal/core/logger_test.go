package core

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

func TestAppendLineConcurrentRotationKeepsBothLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "durations.tsv")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}

	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	firstRenamed := make(chan struct{})
	loserAttempted := make(chan struct{})
	var first sync.Once
	rename := func(old, new string) error {
		arrived <- struct{}{}
		<-release
		isFirst := false
		first.Do(func() { isFirst = true })
		if isFirst {
			err := os.Rename(old, new)
			close(firstRenamed)
			<-loserAttempted
			return err
		}
		<-firstRenamed
		err := os.Rename(old, new)
		close(loserAttempted)
		return err
	}

	errs := make(chan error, 2)
	for _, line := range []string{"first", "second"} {
		go func(line string) { errs <- appendLineWithRename(path, 1, line, rename) }(line)
	}
	for range 2 {
		select {
		case <-arrived:
		case <-time.After(5 * time.Second):
			close(release)
			t.Fatal("both appenders did not reach rename")
		}
	}
	close(release)
	for range 2 {
		if err := <-errs; err != nil {
			t.Errorf("append failed during concurrent rotation: %v", err)
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"first\n", "second\n"} {
		if !strings.Contains(string(data), line) {
			t.Errorf("current log lacks %q: %q", line, data)
		}
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
