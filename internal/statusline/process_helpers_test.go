package statusline

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/aiseeq/chooks/internal/core"
	"github.com/aiseeq/chooks/internal/hookctx"
)

func statusRequestContext(t *testing.T) context.Context {
	t.Helper()
	return hookctx.WithRequest(context.Background(), statusTestRequest(t))
}

func statusTestRequest(t *testing.T) hookctx.Context {
	t.Helper()
	request, err := hookctx.FromProcess()
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func build(input Input, used float64, bar weekBar, now time.Time, visible func() (bool, error), logger core.Logger) (string, string) {
	home, _ := os.UserHomeDir()
	return buildForRuntime(core.RuntimeDir(), &home, input, used, bar, now, visible, logger)
}

func terminalTitle(dir string, state core.SessionState) string {
	home, _ := os.UserHomeDir()
	return state.Marker() + " " + core.ProjectNameForDirWithHome(dir, home)
}

func projectBadge(dir string, state core.SessionState) string {
	home, _ := os.UserHomeDir()
	return projectBadgeForHome(dir, home, state)
}
