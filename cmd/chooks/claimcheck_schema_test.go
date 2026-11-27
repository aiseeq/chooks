package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/aiseeq/chooks/internal/claimcheck"
)

// Схема ответа судьи по версии промпта: v7 — восемь полей с вопросом
func TestWriteClaimSchemaVersion(t *testing.T) {
	for _, tc := range []struct {
		version, name string
		schema        []byte
	}{
		{"", "verdict-schema.json", claimcheck.Schema},
		{"v6", "verdict-schema-v6.json", claimcheck.SchemaV6},
		{"v7", "verdict-schema-v7.json", claimcheck.SchemaV7},
	} {
		t.Run(tc.version, func(t *testing.T) {
			dir := t.TempDir()
			path, err := writeClaimSchemaVersion(dir, tc.version)
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if path != filepath.Join(dir, tc.name) || !bytes.Equal(got, tc.schema) {
				t.Fatalf("path %s, schema %s", path, got)
			}
		})
	}
}
