package tools

import (
	"context"
	"testing"

	"github.com/aiseeq/chooks/internal/core"
	"github.com/aiseeq/chooks/internal/hookctx"
)

func TestGitGuardRequestScratch(t *testing.T) {
	input := &core.ToolInput{ToolName: "Bash", Command: "git reset --hard", CWD: "/var/scratch-x/repo", AgentID: "agent"}
	for _, tc := range []struct {
		env   []string
		valid bool
	}{
		{[]string{"HOME=/home/u", "CLAUDE_CODE_TMPDIR=/var/scratch-x"}, true},
		{[]string{"HOME=/home/u", "CLAUDE_CODE_TMPDIR=/var/other"}, false},
	} {
		request := hookctx.New(nil, nil, nil, tc.env, "/var/scratch-x/repo", 1, 2)
		guard, err := NewGitGuardToolForRequest(core.ToolConfig{Enabled: true}, testLogger(t), request)
		if err != nil {
			t.Fatal(err)
		}
		result, err := guard.ValidateTool(hookctx.WithRequest(context.Background(), request), input)
		if err != nil || result.IsValid != tc.valid {
			t.Errorf("env=%v result=%+v err=%v", tc.env, result, err)
		}
	}
}

// Относительный $TMPDIR или $CLAUDE_CODE_TMPDIR — корень, с которым
// сравнивается абсолютный каталог git. У CLI такой корень не совпадает
// никогда (filepath.Rel относительного с абсолютным отказывает), у запроса
// демона Path сделал бы его абсолютным от cwd запроса и пустил бы git
// reset --hard. Оба отказывают; абсолютный корень пускает у обоих
func TestGitGuardRelativeTempRootCLIAndRequest(t *testing.T) {
	cwd := "/var/chooks-request"
	for _, key := range []string{"TMPDIR", "CLAUDE_CODE_TMPDIR"} {
		for _, tc := range []struct {
			value string
			valid bool
		}{{"scratch", false}, {cwd + "/scratch", true}} {
			env := []string{"HOME=/home/u", key + "=" + tc.value}
			input := &core.ToolInput{ToolName: "Bash", Command: "git -C " + cwd + "/scratch/repo reset --hard", CWD: cwd, AgentID: "agent"}
			cli, err := hookctx.FromProcess()
			if err != nil {
				t.Fatal(err)
			}
			cli.Env = env
			for mode, request := range map[string]hookctx.Context{"CLI": cli, "request": hookctx.New(nil, nil, nil, env, cwd, 1, 2)} {
				guard, err := NewGitGuardToolForRequest(core.ToolConfig{Enabled: true}, testLogger(t), request)
				if err != nil {
					t.Fatal(err)
				}
				result, err := guard.ValidateTool(hookctx.WithRequest(context.Background(), request), input)
				if err != nil || result.IsValid != tc.valid {
					t.Errorf("%s %s=%s: result=%+v err=%v, want valid=%v", mode, key, tc.value, result, err, tc.valid)
				}
			}
		}
	}
}
