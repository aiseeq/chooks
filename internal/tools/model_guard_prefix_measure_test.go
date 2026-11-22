package tools

// Мера мастера (model-guard-detached-launch, круг 2): CLAUDE_CODE_SHELL_PREFIX
// не путём к исполняемому файлу гасит все хуки (127). Боец этот файл не правит.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiseeq/chooks/internal/core"
)

func TestMeasureShellPrefix(t *testing.T) {
	tool := modelGuardForTest(t, "opus claude-opus-5-5\n")
	home := t.TempDir()
	t.Setenv("HOME", home)
	exe := filepath.Join(home, "bin", "prefix")
	if err := os.MkdirAll(filepath.Dir(exe), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nexec \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(home, "plain")
	if err := os.WriteFile(plain, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	const bad = `CLAUDE_CODE_SHELL_PREFIX='timeout 60'`
	for _, tc := range []struct {
		command string
		allowed bool
	}{
		{bad + " bash launch.sh", false},
		{"export " + bad, false},
		{"env " + bad + " bash launch.sh", false},
		{"declare -x " + bad, false},
		{"typeset -x " + bad, false},
		{"readonly " + bad, false},
		{"env -i HOME=/home/x PATH=/usr/bin " + bad + " bash launch.sh", false},
		{"env -u FOO " + bad + " bash launch.sh", false},
		{`env HOME="$HOME" ` + bad + " bash launch.sh", false},
		{"/usr/bin/env " + bad + " bash launch.sh", false},
		{"env -- " + bad + " bash launch.sh", false},
		{"setsid -f env " + bad + " bash launch.sh", false},
		{"nohup env " + bad + " bash launch.sh &", false},
		{`bash -c "` + bad + ` bash launch.sh"`, false},
		{"CLAUDE_CODE_SHELL_PREFIX=" + plain + " bash launch.sh", false},
		{"CLAUDE_CODE_SHELL_PREFIX=nice bash launch.sh", false},
		{"CLAUDE_CODE_SHELL_PREFIX=" + exe + " bash launch.sh", true},
		{"env -i HOME=/home/x CLAUDE_CODE_SHELL_PREFIX=" + exe + " bash launch.sh", true},
		{"CLAUDE_CODE_SHELL_PREFIX=~/bin/prefix bash launch.sh", true},
		{"CLAUDE_CODE_SHELL_PREFIX= bash launch.sh", true},
		{"export CLAUDE_CODE_SHELL_PREFIX=", true},
		{"export CLAUDE_CODE_SHELL_PREFIX", true},
		{"unset CLAUDE_CODE_SHELL_PREFIX", true},
		{"echo " + bad, true},
	} {
		t.Run(tc.command, func(t *testing.T) {
			result, err := tool.ValidateTool(gitGuardTestContext(t), &core.ToolInput{ToolName: "Bash", Command: tc.command})
			if err != nil {
				t.Fatal(err)
			}
			if result.IsValid != tc.allowed {
				t.Fatalf("allowed=%v, want %v; violations=%v", result.IsValid, tc.allowed, result.Violations)
			}
			// отказ по префиксу — без подсказки про список моделей
			if !tc.allowed && strings.Contains(strings.Join(result.Suggestions, "\n"), "models/approved") {
				t.Fatalf("подсказка про список моделей к отказу по префиксу: %q", result.Suggestions)
			}
		})
	}
}
