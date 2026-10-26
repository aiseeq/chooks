package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiseeq/chooks/internal/core"
)

// fakeWebtext кладёт в PATH скрипт webtext: он отмечает вызов и отдаёт
// страницу из архива
func fakeWebtext(t *testing.T) (calls string) {
	t.Helper()
	dir := t.TempDir()
	calls = filepath.Join(dir, "calls")
	page := filepath.Join(dir, "page.txt")
	if err := os.WriteFile(page, []byte("Заголовок\nsource: wayback\n\nТекст страницы."), 0o600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho \"$@\" >> " + calls + "\n" +
		`echo '{"ok":true,"source":"wayback","snapshot":"20260208211225","kind":"html","path":"` + page + `"}'` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "webtext"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":/usr/bin:/bin")
	return calls
}

func webFetchInput(t *testing.T, code int, result string) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"session_id": "s1", "hook_event_name": "PostToolUse", "tool_name": "WebFetch",
		"tool_input":    map[string]any{"url": "https://www.lb.lt/x", "prompt": "quote"},
		"tool_response": map[string]any{"bytes": 0, "code": code, "codeText": "Forbidden", "result": result, "durationMs": 314, "url": "https://www.lb.lt/x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestRunWebFetchFallback_SubstitutesBlockedPage(t *testing.T) {
	config, logger := silentConfig(t)
	calls := fakeWebtext(t)

	var out bytes.Buffer
	runWebFetchFallback(context.Background(), webFetchInput(t, 403, "The server returned HTTP 403 Forbidden."), config, logger, &out)

	var output postToolUseOutput
	if err := json.Unmarshal(out.Bytes(), &output); err != nil {
		t.Fatalf("вывод хука: %v\n%s", err, out.String())
	}
	if output.HookSpecificOutput.HookEventName != "PostToolUse" || output.HookSpecificOutput.UpdatedToolOutput == nil {
		t.Fatalf("нет подмены: %s", out.String())
	}
	if result := output.HookSpecificOutput.UpdatedToolOutput.Result; !strings.Contains(result, "Текст страницы.") {
		t.Errorf("в ответе нет текста страницы: %s", result)
	}
	if args, _ := os.ReadFile(calls); string(args) != "--json --timeout 45s https://www.lb.lt/x\n" {
		t.Errorf("аргументы webtext: %q", args)
	}
}

func TestRunWebFetchFallback_LeavesResponse(t *testing.T) {
	config, logger := silentConfig(t)

	t.Run("обычная страница — webtext не зовётся", func(t *testing.T) {
		calls := fakeWebtext(t)
		var out bytes.Buffer
		runWebFetchFallback(context.Background(), webFetchInput(t, 200, "**Key facts** about the regulation"), config, logger, &out)
		if out.Len() != 0 {
			t.Errorf("обычный ответ подменён: %s", out.String())
		}
		if _, err := os.Stat(calls); err == nil {
			t.Error("webtext вызван на обычный ответ")
		}
	})
	t.Run("webtext не установлен", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		t.Setenv("HOME", t.TempDir())
		var out bytes.Buffer
		runWebFetchFallback(context.Background(), webFetchInput(t, 403, "The server returned HTTP 403 Forbidden."), config, logger, &out)
		if out.Len() != 0 {
			t.Errorf("без webtext ответ должен остаться как был: %s", out.String())
		}
	})
	t.Run("выключен в config.yaml", func(t *testing.T) {
		calls := fakeWebtext(t)
		off := *config
		off.Tools = map[string]core.ToolConfig{webFallbackTool: {Enabled: false}}
		var out bytes.Buffer
		runWebFetchFallback(context.Background(), webFetchInput(t, 403, "The server returned HTTP 403 Forbidden."), &off, logger, &out)
		if _, err := os.Stat(calls); out.Len() != 0 || err == nil {
			t.Errorf("выключенный хук сработал: %s", out.String())
		}
	})
}
