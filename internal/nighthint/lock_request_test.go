package nighthint

import (
	"context"
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
