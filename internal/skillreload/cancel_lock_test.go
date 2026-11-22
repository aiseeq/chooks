package skillreload

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"
)

func TestCheckCancelledLockReleasesResources(t *testing.T) {
	dir := t.TempDir()
	holder, err := os.OpenFile(filepath.Join(dir, "s1.json"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(holder.Fd()), syscall.LOCK_UN)
	counts := func() (int, int) {
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Fatal(err)
		}
		return runtime.NumGoroutine(), len(entries)
	}
	goroutines, fds := counts()
	for range 20 {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
		start := time.Now()
		checker := Checker{Context: ctx, StateDir: dir, Deadline: start.Add(time.Second)}
		_, err := checker.Check(Event{SessionID: "s1", TranscriptPath: filepath.Join(dir, "unused")})
		cancel()
		if err == nil || time.Since(start) > 115*time.Millisecond {
			t.Fatalf("err=%v elapsed=%v", err, time.Since(start))
		}
	}
	g, f := counts()
	if g > goroutines || f > fds {
		t.Fatalf("resources before %d/%d after %d/%d", goroutines, fds, g, f)
	}
}
