package understandcheck

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/chooks/internal/hookctx"
)

func TestRequestParseWarning(t *testing.T) {
	var warning string
	h := Hook{Now: time.Now, Warn: func(message string, _ ...any) { warning = message }}
	req := hookctx.New(nil, nil, nil, []string{"HOME=/request"}, "/request", 1, 2)
	raw, err := json.Marshal(map[string]string{"command": "echo '"})
	if err != nil {
		t.Fatal(err)
	}
	outcome := h.Run(hookctx.WithRequest(context.Background(), req), Call{Tool: "Bash", Input: raw})
	if outcome.Deny || !strings.Contains(warning, "не удалось разобрать") {
		t.Fatalf("outcome=%+v warning=%q", outcome, warning)
	}
}
