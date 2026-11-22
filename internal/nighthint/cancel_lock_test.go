package nighthint

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"
)

func TestRunCancelledLockReleasesResources(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s1.json")
	holder, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Close() })
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Flock(int(holder.Fd()), syscall.LOCK_UN) })
	deps := Deps{Store: Store{Dir: dir}, Now: func() time.Time { return time.Date(2026, 1, 1, 23, 0, 0, 0, time.UTC) }, Locked: func(context.Context) (bool, error) { return true, nil }}
	before, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	goroutines := runtime.NumGoroutine()
	for range 20 {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
		start := time.Now()
		_, err := Run(ctx, deps, Event{Name: EventSessionStart, SessionID: "s1"})
		cancel()
		if err == nil || time.Since(start) > 115*time.Millisecond {
			t.Fatalf("err=%v elapsed=%v", err, time.Since(start))
		}
	}
	after, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	if g := runtime.NumGoroutine(); g > goroutines || len(after) > len(before) {
		t.Fatalf("resources before %d/%d after %d/%d", goroutines, len(before), g, len(after))
	}
}
