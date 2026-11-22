package tools

import (
	"context"
	"testing"

	"github.com/aiseeq/chooks/internal/core"
	"github.com/aiseeq/chooks/internal/hookctx"
)

func TestRmGuardRequestEnvironment(t *testing.T) {
	guard := newRmGuard(t)
	input := &core.ToolInput{ToolName: "Bash", Command: `false && rm -f $REQUEST_PREFIX/usr`, CWD: "/home/u/proj"}
	for _, tc := range []struct {
		env   []string
		valid bool
	}{{[]string{"REQUEST_PREFIX=/home/u/tmp"}, true}, {nil, false}} {
		req := hookctx.New(nil, nil, nil, tc.env, "/home/u/proj", 1, 2)
		result, err := guard.ValidateTool(hookctx.WithRequest(context.Background(), req), input)
		if err != nil || result.IsValid != tc.valid {
			t.Errorf("env %v: result=%+v err=%v", tc.env, result, err)
		}
	}
}
