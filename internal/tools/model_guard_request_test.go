package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/aiseeq/chooks/internal/core"
	"github.com/aiseeq/chooks/internal/hookctx"
)

func TestModelGuardRequestApproved(t *testing.T) {
	root := t.TempDir()
	input := &core.ToolInput{ToolName: "Agent", ToolInput: []byte(`{"model":"sonnet"}`), CWD: root}
	guard, err := NewModelGuardTool(core.ToolConfig{Enabled: true}, testLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, contents string
		valid          bool
	}{
		{"allowed", "sonnet claude-sonnet-5-5\n", true},
		{"denied", "opus claude-opus-5-5\n", false},
	} {
		path := filepath.Join(root, tc.name)
		if err := os.WriteFile(path, []byte(tc.contents), 0600); err != nil {
			t.Fatal(err)
		}
		req := hookctx.New(nil, nil, nil, []string{"HOME=" + root, "MODELS_APPROVED=" + path}, root, 1, 2)
		result, err := guard.ValidateTool(hookctx.WithRequest(context.Background(), req), input)
		if err != nil || result.IsValid != tc.valid {
			t.Errorf("%s: result=%+v err=%v", tc.name, result, err)
		}
	}
}

func TestModelGuardParallel(t *testing.T) {
	const count = 50
	root := t.TempDir()
	guard, err := NewModelGuardTool(core.ToolConfig{Enabled: true}, testLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	input := &core.ToolInput{ToolName: "Agent", ToolInput: []byte(`{"model":"sonnet"}`), CWD: root}
	for i := range count {
		contents := "opus claude-opus-5-5\n"
		if i%2 == 0 {
			contents = "sonnet claude-sonnet-5-5\n"
		}
		if err := os.WriteFile(filepath.Join(root, fmt.Sprint(i)), []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for i := range count {
		wg.Go(func() {
			req := hookctx.New(nil, nil, nil, []string{"HOME=" + root, "MODELS_APPROVED=" + filepath.Join(root, fmt.Sprint(i))}, root, 1000+i, 500+i)
			result, err := guard.ValidateTool(hookctx.WithRequest(context.Background(), req), input)
			if err != nil || result.IsValid != (i%2 == 0) {
				t.Errorf("request %d: result=%+v err=%v", i, result, err)
			}
		})
	}
	wg.Wait()
}
