package hookctx

import (
	"path/filepath"
	"testing"
)

// The CLI's own request leaves relative paths to the OS (same cwd), so error
// texts name them as given; a daemon request resolves them from its cwd
func TestAbsProcessKeepsRelative(t *testing.T) {
	request, err := FromProcess()
	if err != nil {
		t.Fatal(err)
	}
	if got := request.Abs("./sub/../x"); got != "./sub/../x" {
		t.Errorf("process relative path = %q", got)
	}
	if got := request.Abs("/a//b/"); got != "/a/b" {
		t.Errorf("process absolute path = %q", got)
	}
	request.Env = []string{"XDG_RUNTIME_DIR=rf", "XDG_STATE_HOME=sf"}
	if got := request.RuntimeDir(); got != filepath.Join("rf", "chooks") {
		t.Errorf("process runtime dir = %q", got)
	}
	daemon := New(nil, nil, nil, request.Env, "/request", 1, 2)
	if got := daemon.Abs("./sub/../x"); got != "/request/x" {
		t.Errorf("daemon relative path = %q", got)
	}
	if got, err := daemon.StateDir(); err != nil || got != "/request/sf/chooks" {
		t.Errorf("daemon state dir = %q, %v", got, err)
	}
}
