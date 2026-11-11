package approved

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "approved")
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// Комментарий в конце строки — не третье поле: так список читает model_guard,
// а судья understand-check раньше падал на такой строке
func TestReadInlineComment(t *testing.T) {
	models, err := Read(write(t, "# шапка\nopus claude-opus-5-5  # заметка\n\nluna gpt-6-luna\n"))
	if err != nil || len(models) != 2 || models[0] != (Model{"opus", "claude-opus-5-5", 2}) || models[1].Name != "luna" || models[1].Line != 4 {
		t.Fatalf("models = %+v, err = %v", models, err)
	}
}

func TestReadErrors(t *testing.T) {
	if _, err := Read(write(t, "opus\n")); err == nil || !strings.Contains(err.Error(), "строка 1") {
		t.Errorf("одно поле: %v", err)
	}
	if _, err := Read(write(t, "# только шапка\n")); !errors.Is(err, ErrEmpty) {
		t.Errorf("пустой: %v", err)
	}
	if _, err := Read(filepath.Join(t.TempDir(), "нет")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("нет файла: %v", err)
	}
}

func TestPathEnv(t *testing.T) {
	t.Setenv("MODELS_APPROVED", "/x/approved")
	if got, err := Path(); err != nil || got != "/x/approved" {
		t.Fatalf("Path = %q, %v", got, err)
	}
	t.Setenv("MODELS_APPROVED", "")
	t.Setenv("HOME", "/h")
	if got, err := Path(); err != nil || got != "/h/.config/models/approved" {
		t.Fatalf("Path = %q, %v", got, err)
	}
}
