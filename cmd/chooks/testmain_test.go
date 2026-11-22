package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestMain даёт процессу тестов временный HOME до первого теста. Тесты
// подменяют env процесса частично (t.Setenv XDG_STATE_HOME, XDG_RUNTIME_DIR)
// или env запроса, а HOME процесса оставался настоящим: при сбое подмены
// (мутант m1 ревью 2 — env первого запроса у всех) хуки сессии s1 писали
// skill-reload, night-hint и limits в состояние пользователя. Каталоги XDG и
// CLAUDE_CONFIG_DIR сняты: они выводятся из временного HOME
func TestMain(m *testing.M) {
	os.Exit(runIsolated(m))
}

func runIsolated(m *testing.M) int {
	home, err := os.MkdirTemp("", "chooks-test-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "test home:", err)
		return 1
	}
	defer os.RemoveAll(home)
	for _, name := range []string{"XDG_STATE_HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "CLAUDE_CONFIG_DIR", "MODELS_APPROVED"} {
		if err := os.Unsetenv(name); err != nil {
			fmt.Fprintln(os.Stderr, "unset", name+":", err)
			return 1
		}
	}
	for name, value := range map[string]string{"HOME": home, "XDG_RUNTIME_DIR": filepath.Join(home, "run")} {
		if err := os.Setenv(name, value); err != nil {
			fmt.Fprintln(os.Stderr, "set", name+":", err)
			return 1
		}
	}
	return m.Run()
}
