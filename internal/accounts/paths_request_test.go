package accounts

import (
	"path/filepath"
	"testing"

	"github.com/aiseeq/chooks/internal/hookctx"
)

func TestPathsForRequest(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"first", "second"} {
		home := filepath.Join(root, name)
		req := hookctx.New(nil, nil, nil, []string{"HOME=" + home, "CLAUDE_CONFIG_DIR=" + filepath.Join(home, "claude"), "XDG_STATE_HOME=" + filepath.Join(home, "state")}, home, 1, 2)
		paths, err := DefaultPaths(req)
		if err != nil {
			t.Fatal(err)
		}
		if paths.ClaudeDir != filepath.Join(home, "claude") || paths.GlobalConfig != filepath.Join(home, "claude", ".claude.json") || paths.StateDir != filepath.Join(home, "state", "chooks") {
			t.Errorf("%s: %+v", name, paths)
		}
	}
}
