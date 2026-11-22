package desktop

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"
)

func TestDetachedChildIsReaped(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := StartDetached(cmd); err != nil {
		t.Fatal(err)
	}
	if cmd.Process.Pid <= 0 {
		t.Fatalf("detached process handle was released before reaping")
	}
	path := "/proc/" + strconv.Itoa(cmd.Process.Pid)
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("detached child was not reaped")
		}
		time.Sleep(time.Millisecond)
	}
}
