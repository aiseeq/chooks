package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aiseeq/chooks/internal/core"
	"github.com/aiseeq/chooks/internal/hookctx"
)

func TestJiraStyleRequestHome(t *testing.T) {
	root := t.TempDir()
	guard := newJiraStyleTool(t)
	input := &core.ToolInput{ToolName: "Bash", CWD: root, Command: "curl -X POST -T ~/body.json https://x.atlassian.net/rest/api/3/issue/PROJ-1/comment"}
	for _, tc := range []struct {
		name, body string
		valid      bool
	}{{"good", `{"body":"done"}`, true}, {"bad", `{"body":"done — now"}`, false}} {
		home := filepath.Join(root, tc.name)
		if err := os.MkdirAll(home, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, "body.json"), []byte(tc.body), 0600); err != nil {
			t.Fatal(err)
		}
		req := hookctx.New(nil, nil, nil, []string{"HOME=" + home}, root, 1, 2)
		result, err := guard.ValidateTool(hookctx.WithRequest(context.Background(), req), input)
		if err != nil || result.IsValid != tc.valid {
			t.Errorf("%s: result=%+v err=%v", tc.name, result, err)
		}
	}
}
