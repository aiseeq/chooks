package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/aiseeq/chooks/internal/core"
	"github.com/aiseeq/chooks/internal/understandcheck"
)

// Отказ — JSON PreToolUse с permissionDecision deny: только он доносит
// поправку до агента вместо результата инструмента
func TestWriteUnderstandOutcome_DenyFormat(t *testing.T) {
	_, logger := silentConfig(t)
	var out bytes.Buffer
	input := &core.ToolInput{SessionID: "s", ToolName: "SendMessage"}
	writeUnderstandOutcome(understandcheck.Outcome{Deny: true, Reason: "поправка"}, input, logger, &out)
	var got map[string]map[string]string
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("deny output is not JSON: %v: %q", err, out.String())
	}
	want := map[string]string{"hookEventName": "PreToolUse", "permissionDecision": "deny", "permissionDecisionReason": "поправка"}
	for k, v := range want {
		if got["hookSpecificOutput"][k] != v {
			t.Errorf("%s = %q, want %q", k, got["hookSpecificOutput"][k], v)
		}
	}
}

func TestWriteUnderstandOutcome_PassPrintsNothing(t *testing.T) {
	_, logger := silentConfig(t)
	var out bytes.Buffer
	input := &core.ToolInput{SessionID: "s", ToolName: "SendMessage"}
	for _, outcome := range []understandcheck.Outcome{{Verdict: "ok"}, {Skip: "суб-агент"}, {Verdict: "timeout"}} {
		writeUnderstandOutcome(outcome, input, logger, &out)
	}
	if out.Len() != 0 {
		t.Fatalf("pass must print nothing, got %q", out.String())
	}
}
