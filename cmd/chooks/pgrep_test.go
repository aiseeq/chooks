package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/aiseeq/chooks/internal/core"
)

// Monitor с pgrep -f получает updatedInput с -A, прочие поля входа целы
func TestRewritePgrepMonitor(t *testing.T) {
	config, logger := silentConfig(t)
	input := &core.ToolInput{
		ToolName:  "Monitor",
		ToolInput: json.RawMessage(`{"command":"until ! pgrep -f 'x.sh'; do sleep 5; done","description":"ждать x","timeout_ms":1800000}`),
	}

	var out bytes.Buffer
	rewritePgrep(input, config, logger, &out)

	var got preToolUseOutput
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output %q: %v", out.String(), err)
	}
	updated := got.HookSpecificOutput.UpdatedInput
	if got.HookSpecificOutput.HookEventName != "PreToolUse" ||
		updated["command"] != "until ! pgrep -A -f 'x.sh'; do sleep 5; done" ||
		updated["description"] != "ждать x" || updated["timeout_ms"] != float64(1800000) {
		t.Errorf("output = %s", out.String())
	}
}

// Без pgrep -f, в чужом инструменте и при выключенном ключе ответа нет
func TestRewritePgrepSilent(t *testing.T) {
	config, logger := silentConfig(t)
	off := &core.Config{Tools: map[string]core.ToolConfig{pgrepAncestorsTool: {Enabled: false}}}
	cases := map[string]struct {
		config *core.Config
		input  *core.ToolInput
	}{
		"без -f":   {config, &core.ToolInput{ToolName: "Bash", ToolInput: json.RawMessage(`{"command":"pgrep -x claude"}`)}},
		"Write":    {config, &core.ToolInput{ToolName: "Write", ToolInput: json.RawMessage(`{"command":"pgrep -f x"}`)}},
		"выключен": {off, &core.ToolInput{ToolName: "Bash", ToolInput: json.RawMessage(`{"command":"pgrep -f x"}`)}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			rewritePgrep(c.input, c.config, logger, &out)
			if out.Len() != 0 {
				t.Errorf("output = %q, want none", out.String())
			}
		})
	}
}

// Переписывание молчит: в ответе нет additionalContext
func TestRewritePgrepNoContext(t *testing.T) {
	config, logger := silentConfig(t)
	var out bytes.Buffer
	rewritePgrep(&core.ToolInput{ToolName: "Bash", ToolInput: json.RawMessage(`{"command":"pgrep -f x"}`)}, config, logger, &out)
	if out.Len() == 0 || bytes.Contains(out.Bytes(), []byte("additionalContext")) {
		t.Errorf("output = %q", out.String())
	}
}
