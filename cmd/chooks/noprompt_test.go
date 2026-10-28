package main

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/aiseeq/chooks/internal/core"
)

func TestRunNoPrompt(t *testing.T) {
	logger, err := core.NewLogger(core.LoggerConfig{Level: "debug"})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		input map[string]any
		want  map[string]any
	}{
		{
			name:  "AskUserQuestion",
			input: map[string]any{"session_id": "s1", "hook_event_name": "PreToolUse", "tool_name": "AskUserQuestion", "tool_input": map[string]any{"questions": []any{}}},
			want:  map[string]any{"hookEventName": "PreToolUse", "permissionDecision": "deny", "permissionDecisionReason": askDeniedReason},
		},
		{
			name:  "PermissionRequest",
			input: map[string]any{"session_id": "s1", "hook_event_name": "PermissionRequest", "permission_mode": "bypassPermissions", "tool_name": "Bash", "tool_input": map[string]any{"command": "rm -rf /"}},
			want: map[string]any{"hookEventName": "PermissionRequest", "decision": map[string]any{
				"behavior": "deny", "message": permissionDeniedMessage, "interrupt": false,
			}},
		},
		{
			name:  "Elicitation",
			input: map[string]any{"session_id": "s1", "hook_event_name": "Elicitation", "mcp_server_name": "claude_ai_Atlassian_MCP", "mode": "url", "message": "Please authenticate"},
			want:  map[string]any{"hookEventName": "Elicitation", "action": "decline"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(tc.input)
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			if err := runNoPrompt(data, logger, &out); err != nil {
				t.Fatal(err)
			}
			var got struct {
				HookSpecificOutput map[string]any `json:"hookSpecificOutput"`
			}
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatalf("output %q: %v", out.String(), err)
			}
			if !reflect.DeepEqual(got.HookSpecificOutput, tc.want) {
				t.Errorf("got %v, want %v", got.HookSpecificOutput, tc.want)
			}
		})
	}
}

// Регистрация не на том событии или инструменте — ошибка, а не отказ
// первому попавшемуся вызову
func TestRunNoPromptRejectsOtherEvents(t *testing.T) {
	logger, err := core.NewLogger(core.LoggerConfig{Level: "debug"})
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{
		`{"hook_event_name":"PreToolUse","tool_name":"Bash"}`,
		`{"hook_event_name":"Stop"}`,
		`not json`,
	} {
		var out bytes.Buffer
		if err := runNoPrompt([]byte(input), logger, &out); err == nil {
			t.Errorf("%s: want error, got output %q", input, out.String())
		}
		if out.Len() != 0 {
			t.Errorf("%s: want no output, got %q", input, out.String())
		}
	}
}
