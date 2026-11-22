package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiseeq/chooks/internal/core"
	"github.com/aiseeq/chooks/internal/hookctx"
)

func TestPreToolUseModelOverrides(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	cwd := filepath.Join(root, "project")
	for _, dir := range []string{home, filepath.Join(cwd, ".git")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	approved := filepath.Join(root, "approved")
	if err := os.WriteFile(approved, []byte("opus claude-opus-5-5\n"), 0600); err != nil {
		t.Fatal(err)
	}
	env := []string{"HOME=" + home, "MODELS_APPROVED=" + approved, "CLAUDE_CONFIG_DIR="}
	config := &core.Config{Tools: map[string]core.ToolConfig{"model_guard": {Enabled: true}}}
	cases := []struct {
		name        string
		command     string
		blocked     bool
		wantMessage string
	}{
		{"override number", `claude --settings '{"modelOverrides":{"claude-opus-5-5":1}}' --model opus`, true, "модель не проверить"},
		{"override object", `claude --settings '{"modelOverrides":{"claude-opus-5-5":{}}}' --model opus`, true, "модель не проверить"},
		{"heredoc brace in key", "cat > .claude/settings.json <<'EOF'\n{\"modelOverrides\":{\"ignore}\":\"opus\",\"claude-opus-5-5\":\"fable\"}}\nEOF\nclaude --model opus", true, "не одобрена"},
		{"heredoc nested object", "cat > .claude/settings.json <<'EOF'\n{\"modelOverrides\":{\"other\":{\"nested\":{}},\"claude-opus-5-5\":\"fable\"}}\nEOF\nclaude --model opus", true, "не одобрена"},
		{"heredoc multiple values", "cat > .claude/settings.json <<'EOF'\n{\"modelOverrides\":{\"a\":\"opus\",\"claude-opus-5-5\":\"fable\"}}\nEOF\nclaude --model opus", true, "не одобрена"},
		// запись без запуска claude не судится: имя settings.json носят и чужие файлы
		{"foreign settings write", "cat > app/settings.json <<'EOF'\n{\"model\":\"gpt-4o\"}\nEOF", false, ""},
		{"approved override", `claude --settings '{"modelOverrides":{"claude-opus-5-5":"opus"}}' --model opus`, false, ""},
		{"without overrides", `claude --settings '{"model":"opus"}' --model opus`, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logger, logPath := fileLogger(t)
			data, err := json.Marshal(map[string]any{
				"session_id": "model-overrides-test",
				"tool_name":  "Bash",
				"tool_input": map[string]string{"command": tc.command},
			})
			if err != nil {
				t.Fatal(err)
			}
			request := hookctx.New(strings.NewReader(""), io.Discard, io.Discard, env, cwd, 1, 1)
			ctx := hookctx.WithRequest(context.Background(), request)
			code, err := runPreToolUse(ctx, data, config, logger)
			if err != nil {
				t.Fatal(err)
			}
			if blocked := code == exitBlocked; blocked != tc.blocked {
				t.Fatalf("runPreToolUse code=%d, blocked=%v; want blocked=%v", code, blocked, tc.blocked)
			}
			if tc.wantMessage != "" && !strings.Contains(readLog(t, logPath), tc.wantMessage) {
				t.Fatalf("hook log does not contain %q: %s", tc.wantMessage, readLog(t, logPath))
			}
		})
	}
}
