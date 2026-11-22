package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aiseeq/chooks/internal/hookctx"
)

// У CLI обработчик SIGTERM подписан до выхода процесса: SIGTERM после
// возврата runHook пишет строку в лог, как в базе, где файл открыт до выхода
// (проба ревью 7 TestReview7SignalAfterClose, ожидание обращено)
func TestReview7SignalAfterClose(t *testing.T) {
	root := t.TempDir()
	logFile := filepath.Join(root, "hook.log")
	config := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(config, fmt.Appendf(nil, "logger:\n  output: file\n  file: %s\n", logFile), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	req := hookctx.New(strings.NewReader(`{"hook_event_name":"Elicitation"}`), &out, &stderr, []string{"HOME=" + root}, root, os.Getpid(), os.Getppid())
	req.ConfigPath = config
	exited := make(chan int, 1)
	ctx := context.WithValue(context.Background(), cliTerminationKey{}, (func(int))(func(code int) { exited <- code }))
	code, err := runHook(hookctx.WithRequest(ctx, req), hookNoPrompt)
	if err != nil || code != exitAllowed {
		t.Fatalf("runHook: code=%d err=%v stderr=%q", code, err, stderr.String())
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-exited:
		if code != killedExitCode {
			t.Fatalf("signal handler exit code %d, want %d", code, killedExitCode)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("signal handler did not run")
	}
	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "by SIGTERM") {
		t.Fatalf("SIGTERM line missing from the log after runHook returned:\n%s", data)
	}
	durations, err := os.ReadFile(filepath.Join(root, durationsFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(durations), "\tkilled") && !strings.Contains(string(durations), "\tcancelled") {
		t.Fatalf("termination status missing from durations:\n%s", durations)
	}
}
