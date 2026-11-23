package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aiseeq/chooks/internal/understandcheck"
)

// Стенд без --prompt судит промптом хука, с --prompt — текстом файла
func TestUnderstandPrompt(t *testing.T) {
	if got, err := understandPrompt(""); err != nil || got != understandcheck.Prompt {
		t.Fatalf("default prompt: err %v, same as hook %v", err, got == understandcheck.Prompt)
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "p.md")
	if err := os.WriteFile(file, []byte("свой промпт\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := understandPrompt(file); err != nil || got != "свой промпт\n" {
		t.Fatalf("file prompt: %q %v", got, err)
	}
	empty := filepath.Join(dir, "empty.md")
	if err := os.WriteFile(empty, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := understandPrompt(empty); err == nil {
		t.Fatal("empty prompt file accepted")
	}
	if _, err := understandPrompt(filepath.Join(dir, "missing.md")); err == nil {
		t.Fatal("missing prompt file accepted")
	}
}
