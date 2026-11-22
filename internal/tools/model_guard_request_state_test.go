package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiseeq/chooks/internal/core"
	"github.com/aiseeq/chooks/internal/hookctx"
)

// model_guard берёт cwd, HOME и CLAUDE_CONFIG_DIR из запроса, а не из
// процесса хука: у процесса другие HOME, CLAUDE_CONFIG_DIR и cwd, и модель
// haiku находится только по данным запроса
func TestModelGuardUsesRequestState(t *testing.T) {
	write := func(path, contents string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	processHome := t.TempDir()
	t.Setenv("HOME", processHome)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("MODELS_APPROVED", "")

	home := t.TempDir()
	approved := filepath.Join(home, "approved")
	write(approved, "opus claude-opus-5-5\n")
	project := filepath.Join(home, "project")
	write(filepath.Join(project, ".git", "HEAD"), "ref: refs/heads/main\n")
	write(filepath.Join(project, ".claude", "settings.json"), `{"model":"haiku"}`)
	write(filepath.Join(home, "extra", ".claude", "agents", "rev.md"), "---\nname: rev\nmodel: haiku\n---\n")
	write(filepath.Join(home, "extra2", "sub", ".claude", "agents", "rev3.md"), "---\nname: rev3\nmodel: haiku\n---\n")
	config := filepath.Join(home, "cfg")
	write(filepath.Join(config, "agents", "conf.md"), "---\nname: conf\nmodel: haiku\n---\n")
	write(filepath.Join(project, "relcfg", "agents", "rel.md"), "---\nname: rel\nmodel: haiku\n---\n")
	// cwd события Bash без настроек проекта: haiku в них нашёлся бы и мимо
	// проверяемого пути
	neutral := filepath.Join(home, "neutral")
	write(filepath.Join(neutral, ".git", "HEAD"), "ref: refs/heads/main\n")

	logger, err := core.NewLogger(core.LoggerConfig{Level: "error", Output: "stderr"})
	if err != nil {
		t.Fatal(err)
	}
	tool, err := NewModelGuardTool(core.ToolConfig{Enabled: true}, logger)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, configDir, cwd, tool, command, raw string
	}{
		// пустой cwd события — cwd запроса: настройки проекта в нём
		{"cwd", "", "", "Bash", "claude -p hi", ""},
		// «~» в --add-dir — HOME запроса
		{"home", "", neutral, "Bash", "claude --add-dir ~/extra --agent rev -p hi", ""},
		// «cd ~» — HOME запроса: от него --add-dir sub
		{"cd home", "", neutral, "Bash", "cd ~/extra2 && claude --add-dir sub --agent rev3 -p hi", ""},
		{"config dir", config, "", "Agent", "", `{"subagent_type":"conf","prompt":"x"}`},
		{"config dir bash", config, neutral, "Bash", "claude --agent conf -p hi", ""},
		// относительный CLAUDE_CONFIG_DIR — от cwd запроса
		{"relative config dir", "relcfg", "", "Agent", "", `{"subagent_type":"rel","prompt":"x"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := []string{"HOME=" + home, "MODELS_APPROVED=" + approved}
			if tc.configDir != "" {
				env = append(env, "CLAUDE_CONFIG_DIR="+tc.configDir)
			}
			request := hookctx.New(strings.NewReader(""), nil, nil, env, project, 4242, 4241)
			ctx := hookctx.WithRequest(context.Background(), request)
			input := &core.ToolInput{ToolName: tc.tool, CWD: tc.cwd, Command: tc.command, ToolInput: []byte(tc.raw)}
			result, err := tool.ValidateTool(ctx, input)
			if err != nil {
				t.Fatal(err)
			}
			if result.IsValid || len(result.Violations) == 0 || !strings.Contains(result.Violations[0].Message, `модель "haiku" не одобрена`) {
				t.Fatalf("ValidateTool = %+v; want haiku rejection from request state", result)
			}
		})
	}
}
