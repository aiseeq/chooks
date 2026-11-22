package nighthint

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiseeq/chooks/internal/hookctx"
)

func TestSystemBusRequestAddress(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing-bus")
	req := hookctx.New(nil, nil, nil, []string{"DBUS_SYSTEM_BUS_ADDRESS=unix:path=" + path}, "/", 1, 2)
	conn, err := connectSystemBus(hookctx.WithRequest(context.Background(), req))
	if conn != nil {
		conn.Close()
		t.Fatal("unexpected bus connection")
	}
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("error=%v", err)
	}
}

// Относительный unix:path= системной шины — от cwd запроса, не исполнителя:
// godbus открывает его через ОС от cwd процесса
func TestSystemBusRelativeAddressFromRequestCwd(t *testing.T) {
	root := t.TempDir()
	request, executor := filepath.Join(root, "request"), filepath.Join(root, "executor")
	for _, dir := range []string{request, executor} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(executor)
	req := hookctx.New(nil, nil, nil, []string{"DBUS_SYSTEM_BUS_ADDRESS=unix:path=missing-bus"}, request, 1, 2)
	conn, err := connectSystemBus(hookctx.WithRequest(context.Background(), req))
	if conn != nil {
		conn.Close()
		t.Fatal("unexpected bus connection")
	}
	real, evalErr := filepath.EvalSymlinks(request)
	if evalErr != nil {
		t.Fatal(evalErr)
	}
	if want := filepath.Join(real, "missing-bus"); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error=%v, want the socket %s under the request cwd", err, want)
	}
}
