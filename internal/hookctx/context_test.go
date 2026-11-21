package hookctx

import (
	"path/filepath"
	"testing"
)

func TestRequestEnvironmentSnapshot(t *testing.T) {
	env := []string{"HOME=/first", "EMPTY=", "VALUE=a=b"}
	ctx := New(nil, nil, nil, env, "/request", 123, 45)
	env[0] = "HOME=/changed"
	if got := ctx.Getenv("HOME"); got != "/first" {
		t.Fatalf("HOME = %q", got)
	}
	if value, ok := ctx.LookupEnv("EMPTY"); !ok || value != "" {
		t.Fatalf("EMPTY = %q, %v", value, ok)
	}
	if _, ok := ctx.LookupEnv("MISSING"); ok {
		t.Fatal("missing variable found")
	}
	if got := ctx.Getenv("VALUE"); got != "a=b" {
		t.Fatalf("VALUE = %q", got)
	}
	if got := ctx.Abs("a/../b"); got != filepath.Join("/request", "b") {
		t.Fatalf("relative path = %q", got)
	}
	if got := ctx.Abs("/other"); got != "/other" {
		t.Fatalf("absolute path = %q", got)
	}
}
