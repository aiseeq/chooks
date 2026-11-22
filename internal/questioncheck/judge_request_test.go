package questioncheck

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiseeq/chooks/internal/hookctx"
)

// A local shell stub stands in for the model CLI; no model is invoked.
func TestJudgeRequestEnvironment(t *testing.T) {
	root := t.TempDir()
	bin, work := filepath.Join(root, "bin"), filepath.Join(root, "work")
	for _, dir := range []string{bin, work} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	stub := `#!/bin/sh
printf '{"result":"%s|%s|%s","is_error":false}' "$REQUEST_MARK" "$PWD" "${CLAUDE_PID-unset}"
`
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(stub), 0700); err != nil {
		t.Fatal(err)
	}
	req := hookctx.New(nil, nil, nil, []string{"PATH=" + bin, "REQUEST_MARK=one", "CLAUDE_PID=123"}, root, 456, 123)
	judge := CLIJudge{Name: "opus", Effort: "medium", Dir: work, Prompt: "test"}
	got, _, err := judge.Ask(hookctx.WithRequest(context.Background(), req), "test")
	if err != nil || got != strings.Join([]string{"one", work, "unset"}, "|") {
		t.Fatalf("result=%q err=%v", got, err)
	}
}
