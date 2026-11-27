package statusline

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aiseeq/chooks/internal/core"
	"github.com/aiseeq/chooks/internal/hookctx"
)

func TestBuildRequestRuntime(t *testing.T) {
	root := t.TempDir()
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	if err := core.SaveSessionStateIn(context.Background(), a, "shared", core.StateWorking); err != nil {
		t.Fatal(err)
	}
	if err := core.SaveSessionStateIn(context.Background(), b, "shared", core.StateDone); err != nil {
		t.Fatal(err)
	}
	input := Input{CWD: "/home/user/demo", SessionID: "shared"}
	_, first := buildForRuntime(context.Background(), a, nil, input, 10, weekBar{}, time.Now(), hidden, testLogger(t))
	_, second := buildForRuntime(context.Background(), b, nil, input, 10, weekBar{}, time.Now(), hidden, testLogger(t))
	if first != "🔄 demo" || second != "❇️ demo" {
		t.Fatalf("titles %q and %q", first, second)
	}
}

func TestLimitsRequestState(t *testing.T) {
	root := t.TempDir()
	now := time.Unix(1700000000, 0)
	for i := range 2 {
		dir := filepath.Join(root, fmt.Sprint(i))
		req := hookctx.New(nil, nil, nil, []string{"HOME=" + dir, "XDG_STATE_HOME=" + dir}, dir, 1, 2)
		ctx := hookctx.WithRequest(context.Background(), req)
		if _, err := recordLimitsForContext(ctx, sample{week: weeklyUsage{used: float64(10 + i), resetsAt: now.Add(24 * time.Hour)}, session: "shared"}, now); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(dir, "chooks", "limits.json")); err != nil {
			t.Fatalf("request %d: limits file: %v", i, err)
		}
		snapshot, err := diskLimitsForContext(ctx)
		if err != nil || len(snapshot.Weeks) != 1 || snapshot.Weeks[0].SevenDay != float64(10+i) {
			t.Fatalf("request %d: snapshot=%+v err=%v", i, snapshot, err)
		}
	}
}

func TestRenderParallel(t *testing.T) {
	const count = 50
	root := t.TempDir()
	for i := range count {
		dir := filepath.Join(root, fmt.Sprint(i))
		state := core.StateWorking
		if i%2 == 0 {
			state = core.StateDone
		}
		if err := core.SaveSessionStateIn(context.Background(), filepath.Join(dir, "run", "chooks"), "shared", state); err != nil {
			t.Fatal(err)
		}
	}
	type outcome struct{ line, stderr, err string }
	render := func(i int) outcome {
		dir := filepath.Join(root, fmt.Sprint(i))
		var stderr bytes.Buffer
		req := hookctx.New(nil, nil, &stderr, []string{"HOME=" + dir, "XDG_RUNTIME_DIR=" + filepath.Join(dir, "run"), "XDG_STATE_HOME=" + filepath.Join(dir, "state"), "CLAUDE_CONFIG_DIR=" + filepath.Join(dir, "claude"), fmt.Sprintf("CLAUDE_PID=%d", i+1000)}, dir, i+2000, i+3000)
		logger, err := core.NewLoggerTo(core.LoggerConfig{Level: "error"}, &stderr)
		if err != nil {
			return outcome{err: err.Error()}
		}
		data := fmt.Sprintf(`{"session_id":"shared","cwd":%q}`, dir)
		line, err := Render(hookctx.WithRequest(context.Background(), req), strings.NewReader(data), logger)
		res := outcome{line: line, stderr: stderr.String()}
		if err != nil {
			res.err = err.Error()
		}
		return res
	}
	var got [count]outcome
	var wg sync.WaitGroup
	for i := range got {
		wg.Go(func() { got[i] = render(i) })
	}
	wg.Wait()
	for i, result := range got {
		color, marker := badgeBlue, "🔄"
		if i%2 == 0 {
			color, marker = badgeGreen, "❇️"
		}
		if result.err != "" || result.line != color+" ~ "+reset || result.stderr != "\033]0;"+marker+" ~\007" {
			t.Errorf("request %d: %+v", i, result)
		}
	}
}

func TestRenderRejectsMissingRequest(t *testing.T) {
	if _, err := Render(context.Background(), strings.NewReader(`{}`), testLogger(t)); err == nil {
		t.Fatal("statusline accepted a missing hook request")
	}
}
