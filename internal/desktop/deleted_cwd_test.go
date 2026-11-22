package desktop

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aiseeq/chooks/internal/hookctx"
)

// Удалённый cwd запроса: у CLI процесс доставки наследует удалённый cwd и
// стартует, у демона стартует из "/"
func TestDeliverInBackgroundDeletedCwd(t *testing.T) {
	cwd := filepath.Join(t.TempDir(), "gone")
	if err := os.Mkdir(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(cwd); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "seen")
	req := hookctx.New(nil, nil, nil, []string{"PATH=/usr/bin:/bin"}, cwd, 7, 6)
	if err := DeliverInBackgroundForRequest("/bin/sh", envProbe(t, out), Alert{Title: "t"}, req); err != nil {
		t.Fatalf("watcher not started from a deleted cwd: %v", err)
	}
	if seen := waitProbe(t, out); seen["CWD"] != "/" {
		t.Fatalf("child CWD %q, want /", seen["CWD"])
	}
}
