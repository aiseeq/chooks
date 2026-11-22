package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"testing"

	"github.com/aiseeq/chooks/internal/hookctx"
)

// logDescriptors — открытые дескрипторы процесса и сколько из них на файл лога
func logDescriptors(t *testing.T, logFile string) (total, onLog int) {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join("/proc/self/fd", entry.Name()))
		if err == nil && target == logFile {
			onLog++
		}
	}
	return len(entries), onLog
}

// Запрос через executeHook закрывает свой файл лога по концу обработки, и на
// пути ошибки тоже. GC выключен: дескриптор не закроет финализатор os.File
func TestRequestLogFileClosed(t *testing.T) {
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	root := t.TempDir()
	logFile := filepath.Join(root, "logs", "chooks.log")
	config := filepath.Join(root, "config.yaml")
	text := fmt.Sprintf("logger:\n  level: info\n  output: file\n  file: %s\ntools:\n  notifier: {enabled: false}\n  skill_reload: {enabled: false}\n  night_hint: {enabled: false}\n  webfetch_fallback: {enabled: false}\n  understand_check: {enabled: false}\n", logFile)
	if err := os.WriteFile(config, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	requests := []struct{ hook, input string }{
		{hookNoPrompt, `{"session_id":"fd","hook_event_name":"Elicitation"}`},
		{hookPreToolUse, `{"session_id":"fd","tool_name":"Bash","tool_input":{"command":"git status"}}`},
		{hookPreToolUse, `не JSON`},
		{hookStop, `{"session_id":"fd"}`},
		{"statusline", `{"session_id":"fd","cwd":"/work/demo"}`},
	}
	run := func(i int) (int, string) {
		r := requests[i%len(requests)]
		dir := filepath.Join(root, fmt.Sprint(i))
		var stdout, stderr bytes.Buffer
		req := hookctx.New(strings.NewReader(r.input), &stdout, &stderr, []string{"HOME=" + dir, "XDG_RUNTIME_DIR=" + filepath.Join(dir, "run"), "XDG_STATE_HOME=" + filepath.Join(dir, "state"), "CLAUDE_CONFIG_DIR=" + filepath.Join(dir, "claude")}, root, 5000000+i, 6000000+i)
		req.ConfigPath = config
		return executeHook(context.Background(), req, r.hook), stderr.String()
	}
	// Первый запрос создаёт каталог лога и файл: дальше счёт от него
	run(0)
	total, onLog := logDescriptors(t, logFile)
	if onLog != 0 {
		t.Fatalf("log file still open after the first request: %d descriptors", onLog)
	}

	const count = 50
	failed := 0
	for i := range count {
		if code, _ := run(i); code == exitError {
			failed++
		}
	}
	var wg sync.WaitGroup
	codes := make([]int, count)
	for i := range count {
		wg.Go(func() { codes[i], _ = run(count + i) })
	}
	wg.Wait()
	for _, code := range codes {
		if code == exitError {
			failed++
		}
	}
	if failed == 0 {
		t.Fatal("no request took the error path: the test does not cover closing on error")
	}

	afterTotal, afterOnLog := logDescriptors(t, logFile)
	t.Logf("descriptors before=%d after=%d, on the log file %d, requests with error %d", total, afterTotal, afterOnLog, failed)
	if afterOnLog != 0 || afterTotal > total {
		t.Fatalf("request log files are not closed: %d descriptors on %s, total %d -> %d", afterOnLog, logFile, total, afterTotal)
	}
	if info, err := os.Stat(logFile); err != nil || info.Size() == 0 {
		t.Fatalf("log file is empty or missing: %v", err)
	}
}
