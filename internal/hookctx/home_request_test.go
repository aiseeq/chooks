package hookctx

import (
	"context"
	"path/filepath"
	"testing"
)

func TestUserHomeDirUsesRequest(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "process"))
	first := New(nil, nil, nil, []string{"HOME=first"}, root, 1, 2)
	second := New(nil, nil, nil, []string{"HOME=second"}, root, 3, 4)
	if first.Getenv("HOME") != "first" || second.Getenv("HOME") != "second" {
		t.Fatal("constructing another request changed the first request's environment")
	}
	for _, name := range []string{"one", "two"} {
		request := New(nil, nil, nil, []string{"HOME=" + name}, root, 1, 2)
		home, err := UserHomeDir(WithRequest(context.Background(), request))
		if want := filepath.Join(root, name); err != nil || home != want {
			t.Errorf("home=%q err=%v want %q", home, err, want)
		}
	}
	if _, err := UserHomeDir(context.Background()); err == nil {
		t.Fatal("missing request must return an error")
	}
}
