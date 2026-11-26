package tools

import (
	"strings"
	"testing"

	"github.com/aiseeq/chooks/internal/core"
)

func TestModelGuardAgentsJSONEscapedValues(t *testing.T) {
	tool := modelGuardForTest(t, "opus claude-opus-5-5\n")
	cases := []struct {
		name, command string
		allowed       bool
	}{
		{"escaped newline mcpServers", `claude -p --model opus --agents '{"x":{"description":"d","prompt":"a\nb","mcpServers":["m"]}}' hi`, false},
		{"escaped newline hooks", `claude -p --model opus --agents '{"x":{"description":"d","prompt":"a\nb","hooks":{}}}' hi`, false},
		{"escaped newline disallowed model", `claude -p --model opus --agents '{"x":{"description":"d","prompt":"a\nb","model":"haiku"}}' hi`, false},
		{"disallowed model without newline", `claude -p --model opus --agents '{"x":{"description":"d","prompt":"ab","model":"haiku"}}' hi`, false},
		{"quote backslash slash escapes keep hooks refusal", `claude -p --model opus --agents '{"x":{"description":"d","prompt":"a\"b\\c\/d\ne","hooks":{}}}' hi`, false},
		{"safe opus agent with escaped newline", `claude -p --model opus --agents '{"x":{"description":"d","prompt":"a\nb","model":"opus"}}' hi`, true},
		{"safe agent without model with escaped newline", `claude -p --model opus --agents '{"x":{"description":"d","prompt":"a\nb"}}' hi`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := tool.ValidateTool(gitGuardTestContext(t), &core.ToolInput{ToolName: "Bash", Command: tc.command, CWD: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			if result.IsValid != tc.allowed {
				t.Fatalf("allowed=%v, want %v; violations=%v", result.IsValid, tc.allowed, result.Violations)
			}
		})
	}
}

func TestModelGuardAgentsInvalidJSONRefused(t *testing.T) {
	tool := modelGuardForTest(t, "opus claude-opus-5-5\n")
	cases := []struct {
		name, command string
	}{
		{"V06 invalid JSON with raw newline and mcpServers", "claude -p --model opus --agents '{\"x\":{\"description\":\"d\",\"prompt\":\"a\nb\",\"mcpServers\":[\"m\"]}}' hi"},
		{"V07 invalid JSON with raw newline and hooks", "claude -p --model opus --agents '{\"x\":{\"description\":\"d\",\"prompt\":\"a\nb\",\"hooks\":{}}}' hi"},
		{"V09 invalid JSON with raw newline and disallowed model", "claude -p --model opus --agents '{\"x\":{\"description\":\"d\",\"prompt\":\"a\nb\",\"model\":\"haiku\"}}' hi"},
		{"trailing JSON data", `claude -p --model opus --agents '{"x":{"model":"opus"}} garbage' hi`},
		{"null agent", `claude -p --model opus --agents '{"x":null}' hi`},
		{"null top level", `claude -p --model opus --agents 'null' hi`},
		{"array top level", `claude -p --model opus --agents '[{"x":{"description":"d","prompt":"p"}}]' hi`},
		{"model is not a string", `claude -p --model opus --agents '{"x":{"description":"d","prompt":"p","model":5}}' hi`},
		{"inline JSON with unresolved variable", `claude -p --model opus --agents {\"x\":{\"description\":\"$UNSET_CHOOKS_TEST\",\"prompt\":\"p\"}} hi`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := tool.ValidateTool(gitGuardTestContext(t), &core.ToolInput{ToolName: "Bash", Command: tc.command, CWD: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			if result.IsValid {
				t.Fatalf("invalid --agents JSON was allowed: %s", tc.command)
			}
			if !strings.Contains(result.Violations[0].Message, "строку --agents не разобрать") {
				t.Fatalf("inline JSON refusal has wrong source label: %v", result.Violations)
			}
		})
	}
}

func TestModelGuardAgentsExactModelKey(t *testing.T) {
	tool := modelGuardForTest(t, "opus claude-opus-5-5\n")
	for _, tc := range []struct{ name, command string }{
		{"Model cannot override model", `claude -p --model opus --agents '{"x":{"description":"d","prompt":"p","model":"haiku","Model":"opus"}}' hi`},
		{"MODEL cannot override model", `claude -p --model opus --agents '{"x":{"description":"d","prompt":"p","model":"haiku","MODEL":"opus"}}' --agent x hi`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := tool.ValidateTool(gitGuardTestContext(t), &core.ToolInput{ToolName: "Bash", Command: tc.command, CWD: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			if result.IsValid {
				t.Fatalf("non-approved exact model was overridden: %v", result.Violations)
			}
		})
	}
}

func TestModelGuardAgentsInvalidJSONModelsOnly(t *testing.T) {
	tool := modelGuardForTest(t, "opus claude-opus-5-5\n").ModelsOnly()
	for _, tc := range []struct{ name, command string }{
		{"raw newline JSON rejected", "claude -p --model opus --agents '{\"x\":{\"prompt\":\"a\nb\",\"model\":\"opus\"}}' hi"},
		{"null top-level rejected", `claude -p --model opus --agents 'null' hi`},
		{"null agent rejected", `claude -p --model opus --agents '{"x":null}' hi`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := tool.ValidateTool(gitGuardTestContext(t), &core.ToolInput{ToolName: "Bash", Command: tc.command, CWD: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			if result.IsValid {
				t.Fatalf("unparseable --agents JSON was allowed: %v", result.Violations)
			}
			if !strings.Contains(result.Violations[0].Message, "строку --agents не разобрать") {
				t.Fatalf("inline JSON refusal has wrong source label: %v", result.Violations)
			}
		})
	}
}
