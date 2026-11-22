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
