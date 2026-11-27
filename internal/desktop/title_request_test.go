package desktop

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiseeq/chooks/internal/hookctx"
)

func TestTerminalTitleRequest(t *testing.T) {
	var stderr bytes.Buffer
	req := hookctx.New(nil, nil, &stderr, []string{"HOME=/test"}, "/test", 123, 12)
	if err := SetTerminalTitleForRequest(context.Background(), "project%\nline", req); err != nil {
		t.Fatal(err)
	}
	if got := stderr.String(); got != "\x1b]0;project line\a" {
		t.Fatalf("stderr=%q", got)
	}
}

func TestTabVisibleRequestBus(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing-bus")
	req := hookctx.New(nil, nil, io.Discard, []string{"KONSOLE_DBUS_SERVICE=org.example", "KONSOLE_DBUS_SESSION=/Sessions/1", "KONSOLE_DBUS_WINDOW=/Windows/1", "DBUS_SESSION_BUS_ADDRESS=unix:path=" + path}, "/", 1, 2)
	visible, err := TabVisible(hookctx.WithRequest(context.Background(), req))
	if visible || err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("visible=%v err=%v", visible, err)
	}
}
