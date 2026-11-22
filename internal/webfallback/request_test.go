package webfallback

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiseeq/chooks/internal/hookctx"
)

func TestExecRunnerRequest(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	work := filepath.Join(dir, "work")
	for _, p := range []string{bin, work} {
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	script := "#!/bin/sh\nprintf '%s\\n%s\\n' \"$CHOOKS_TEST_REQUEST\" \"$PWD\"\n"
	if err := os.WriteFile(filepath.Join(bin, "webtext"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	request := hookctx.New(nil, nil, nil, []string{"PATH=" + bin, "CHOOKS_TEST_REQUEST=request-value"}, work, 1, 2)
	out, code, err := ExecRunner(hookctx.WithRequest(context.Background(), request), nil)
	if err != nil || code != 0 || string(out) != strings.Join([]string{"request-value", work, ""}, "\n") {
		t.Fatalf("out=%q code=%d err=%v", out, code, err)
	}
}

func TestExecRunnerRejectsMissingRequest(t *testing.T) {
	if _, _, err := ExecRunner(context.Background(), nil); err == nil {
		t.Fatal("webtext accepted a missing hook request")
	}
}

func TestOpenRequestRelativePath(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "page.txt"), []byte("request page"), 0600); err != nil {
		t.Fatal(err)
	}
	req := hookctx.New(nil, nil, nil, nil, dir, 1, 2)
	runner := func(context.Context, []string) ([]byte, int, error) {
		return []byte(`{"ok":true,"path":"page.txt"}`), 0, nil
	}
	_, text, err := open(hookctx.WithRequest(context.Background(), req), "https://example.org", false, runner)
	if err != nil || text != "request page" {
		t.Fatalf("text=%q err=%v", text, err)
	}
}

// Удалённый cwd запроса: у CLI webtext наследует удалённый cwd и стартует,
// у демона стартует из "/" — исход (вывод webtext) тот же
func TestExecRunnerDeletedCwd(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	work := filepath.Join(dir, "work")
	for _, p := range []string{bin, work} {
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(bin, "webtext"), []byte("#!/bin/sh\necho page\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(work); err != nil {
		t.Fatal(err)
	}
	request := hookctx.New(nil, nil, nil, []string{"PATH=" + bin}, work, 1, 2)
	out, code, err := ExecRunner(hookctx.WithRequest(context.Background(), request), nil)
	if err != nil || code != 0 || string(out) != "page\n" {
		t.Fatalf("out=%q code=%d err=%v, want webtext started as in the CLI", out, code, err)
	}
}
