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
	"github.com/aiseeq/chooks/internal/understandcheck"
)

// fileLogger — настоящий логгер в файл: журнал отказов читают как файл
func fileLogger(t *testing.T) (core.Logger, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "chooks.log")
	logger, err := core.NewLogger(core.LoggerConfig{Level: "debug", Output: "file", LogFile: path})
	if err != nil {
		t.Fatal(err)
	}
	return logger, path
}

func readLog(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// fakeGitHubToken собран из частей: целиком в исходнике его не должно быть
func fakeGitHubToken() string { return "ghp" + "_" + "abcDEF123456789xyzQWEabcDEF12345678" }

func assertDenyLine(t *testing.T, log string, want ...string) {
	t.Helper()
	for _, part := range want {
		if !strings.Contains(log, part) {
			t.Errorf("no %q in the log:\n%s", part, log)
		}
	}
	if strings.Contains(log, fakeGitHubToken()) {
		t.Errorf("secret value in the log:\n%s", log)
	}
}

func TestRunPreToolUse_DenyLogsSessionToolMaskedCommand(t *testing.T) {
	config := &core.Config{Tools: map[string]core.ToolConfig{"kill_guard": {Enabled: true}}}
	logger, path := fileLogger(t)
	data, err := json.Marshal(map[string]any{
		"session_id": "sess-1",
		"tool_name":  "Bash",
		"tool_input": map[string]string{"command": `TOKEN=` + fakeGitHubToken() + `; pkill -f "$NAME"`},
	})
	if err != nil {
		t.Fatal(err)
	}

	code, err := runPreToolUse(context.Background(), data, config, logger)
	if err != nil {
		t.Fatal(err)
	}
	if code != exitBlocked {
		t.Fatalf("exit code %d, want the call denied", code)
	}
	assertDenyLine(t, readLog(t, path), "hook blocked operation", "session=sess-1", "tool=Bash", "pkill", "[скрыто:github]")
}

func TestWriteUnderstandOutcome_DenyLogsMaskedCommand(t *testing.T) {
	logger, path := fileLogger(t)
	input := &core.ToolInput{SessionID: "sess-2", ToolName: "Bash", Command: "echo " + fakeGitHubToken()}

	writeUnderstandOutcome(understandcheck.Outcome{Deny: true, Reason: "поправка"}, input, logger, &bytes.Buffer{})

	assertDenyLine(t, readLog(t, path), "understand check denied call", "session=sess-2", "tool=Bash", "[скрыто:github]")
}

func TestRunNoPrompt_DenyLogsMaskedCommand(t *testing.T) {
	logger, path := fileLogger(t)
	data, err := json.Marshal(map[string]any{
		"session_id":      "sess-3",
		"hook_event_name": "PermissionRequest",
		"tool_name":       "Bash",
		"tool_input":      map[string]string{"command": "echo " + fakeGitHubToken()},
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := runNoPrompt(data, logger, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	assertDenyLine(t, readLog(t, path), "prompt denied", "session=sess-3", "tool=Bash", "[скрыто:github]")
}

// Окно отказывает и при неразборчивом tool_input: строка остаётся, без команды
func TestRunNoPrompt_UnreadableToolInputStillDenies(t *testing.T) {
	logger, path := fileLogger(t)
	data := []byte(`{"session_id":"s","hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":42}`)

	var out bytes.Buffer
	if err := runNoPrompt(data, logger, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"deny"`) {
		t.Errorf("no deny in the output: %s", out.String())
	}
	assertDenyLine(t, readLog(t, path), "command unreadable", "prompt denied")
}

func TestDeniedCommand(t *testing.T) {
	long := strings.Repeat("я", deniedCommandRunes+10)
	tests := map[string]struct {
		input *core.ToolInput
		want  string
	}{
		"bash — сама команда": {
			&core.ToolInput{ToolName: "Bash", Command: "ls", ToolInput: json.RawMessage(`{"command":"ls","timeout":5}`)},
			"ls",
		},
		"другой инструмент — tool_input": {
			&core.ToolInput{ToolName: "Write", ToolInput: json.RawMessage(`{"file_path":"a"}`)},
			`{"file_path":"a"}`,
		},
		"срез с пометкой": {
			&core.ToolInput{ToolName: "Bash", Command: long},
			long[:deniedCommandRunes*len("я")] + " …[обрезано]",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := deniedCommand(tt.input); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
