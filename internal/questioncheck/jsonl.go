package questioncheck

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Вердикты в журнале
const (
	VerdictPass  = "pass"
	VerdictBlock = "block"
	VerdictError = "error"
)

// StateDir — $XDG_STATE_HOME/chooks: журналы судей рядом с
// limits-history.jsonl и skill-reload
func StateDir() (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot resolve home dir: %w", err)
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "chooks"), nil
}

// AppendJSONL дописывает запись строкой JSON в журнал, создавая каталог
func AppendJSONL(path string, entry any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create journal dir: %w", err)
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("encode journal entry: %w", err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open journal: %w", err)
	}
	// Одна запись write в O_APPEND: строки параллельных сессий не перемешиваются
	if _, err := file.Write(append(data, '\n')); err != nil {
		file.Close()
		return fmt.Errorf("append journal: %w", err)
	}
	return file.Close()
}
