package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiseeq/chooks/internal/core"
)

func TestRunUnsentCheck(t *testing.T) {
	logger, err := core.NewLogger(core.LoggerConfig{Level: "debug"})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "t.jsonl")
	human := `{"type":"user","uuid":"u1","timestamp":"2026-10-29T10:00:00Z","origin":{"kind":"human"},"message":{"content":"что с ответом?"}}`
	if err := os.WriteFile(path, []byte(human+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runUnsentCheck(&core.ToolInput{SessionID: "s1", TranscriptPath: path, LastAssistantMessage: "Ещё не отправлено: ответ Оле."}, logger, &out)
	var got stopBlockOutput
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("вывод %q: %v", out.String(), err)
	}
	if got.Decision != "block" || !strings.Contains(got.Reason, "reddy history") {
		t.Errorf("вывод = %+v", got)
	}

	out.Reset()
	runUnsentCheck(&core.ToolInput{SessionID: "s1", TranscriptPath: filepath.Join(t.TempDir(), "нет.jsonl"), LastAssistantMessage: "Ещё не отправлено: ответ Оле."}, logger, &out)
	if out.Len() != 0 {
		t.Errorf("сбой чтения держит ход: %q", out.String())
	}
}
