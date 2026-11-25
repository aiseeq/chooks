package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aiseeq/chooks/internal/core"
	"github.com/aiseeq/chooks/internal/hookctx"
	"github.com/aiseeq/chooks/internal/tools"
)

// Обход большого дерева получает additionalContext, pgrep -f в той же
// команде переписан в том же ответе
func TestWideSearchHint(t *testing.T) {
	t.Setenv("HOME", "/home/u")
	config, logger := silentConfig(t)
	cases := map[string]struct {
		tool, command, updated string
	}{
		"Bash":           {"Bash", "grep -r x ~/.cache", ""},
		"Monitor":        {"Monitor", "rg --files /", ""},
		"вместе с pgrep": {"Bash", "pgrep -f x; du -sh ~/.cache/claude-tmp", "pgrep -A -f x; du -sh ~/.cache/claude-tmp"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			raw, err := json.Marshal(map[string]string{"command": c.command})
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			writeAllowedOutput(&core.ToolInput{ToolName: c.tool, CWD: "/tmp", ToolInput: raw}, config, logger, &out)
			var got preToolUseOutput
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatalf("output %q: %v", out.String(), err)
			}
			h := got.HookSpecificOutput
			if h.HookEventName != "PreToolUse" || !strings.HasSuffix(h.AdditionalContext, ": "+tools.WideSearchHintText) {
				t.Errorf("output = %s", out.String())
			}
			if c.updated == "" && h.UpdatedInput != nil || c.updated != "" && h.UpdatedInput["command"] != c.updated {
				t.Errorf("updatedInput = %v, want %q", h.UpdatedInput, c.updated)
			}
		})
	}
}

func TestWideSearchHintXargsFileArguments(t *testing.T) {
	t.Setenv("HOME", "/home/aisee")
	config, logger := silentConfig(t)
	req := hookctx.New(nil, nil, nil, []string{"HOME=/home/aisee"}, "/home/aisee", 1, 2)
	ctx := hookctx.WithRequest(context.Background(), req)
	cases := []struct {
		name, command string
		hint          bool
	}{
		{"xargs list of files", "cut -d: -f1 hits | xargs rg -c -F -f pat", false},
		{"xargs -r list", "printf '%s\\n' a b | xargs -r rg foo", false},
		{"xargs -r without input", "xargs -r rg foo", false},
		{"xargs -r empty list", "printf '' | xargs -r rg foo", false},
		{"implicit cwd", "rg foo", true},
		{"explicit wide path", "rg foo /home/aisee", true},
		{"xargs explicit wide path", "xargs rg foo /home/aisee", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(map[string]string{"command": tc.command})
			if err != nil {
				t.Fatal(err)
			}
			input := &core.ToolInput{ToolName: "Bash", CWD: "/home/aisee", ToolInput: raw}
			got, err := wideSearchContext(ctx, input, config, logger)
			if err != nil {
				t.Fatal(err)
			}
			if (got != "") != tc.hint {
				t.Errorf("wideSearchContext() = %q; want hint=%v", got, tc.hint)
			}
		})
	}
}

// Узкий поиск, чужой инструмент и выключенный ключ ответа не дают
func TestWideSearchHintSilent(t *testing.T) {
	t.Setenv("HOME", "/home/u")
	config, logger := silentConfig(t)
	off := &core.Config{Tools: map[string]core.ToolConfig{wideSearchHintTool: {Enabled: false}}}
	cases := map[string]struct {
		config *core.Config
		tool   string
	}{
		"Write":    {config, "Write"},
		"выключен": {off, "Bash"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			writeAllowedOutput(&core.ToolInput{ToolName: c.tool, CWD: "/tmp", ToolInput: json.RawMessage(`{"command":"grep -r x ~/.cache"}`)}, c.config, logger, &out)
			if out.Len() != 0 {
				t.Errorf("output = %q, want none", out.String())
			}
		})
	}
	var out bytes.Buffer
	writeAllowedOutput(&core.ToolInput{ToolName: "Bash", CWD: "/home/u/git/chooks", ToolInput: json.RawMessage(`{"command":"grep -r x ."}`)}, config, logger, &out)
	if out.Len() != 0 {
		t.Errorf("narrow search output = %q, want none", out.String())
	}
}
