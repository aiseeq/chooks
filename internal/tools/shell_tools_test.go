package tools

import (
	"context"
	"slices"
	"testing"

	"github.com/aiseeq/chooks/internal/core"
)

// Проверки команд оболочки подписаны на Monitor: движок зовёт проверку только
// для инструментов из SupportedTools, а Monitor исполняет command, как Bash
// (тикет kill-guard-gaps: kill и claude --model в Monitor проходили)
func TestShellGuardsSupportMonitor(t *testing.T) {
	config, logger := core.ToolConfig{Enabled: true}, testLogger(t)
	kill, err := NewKillGuardTool(config, logger)
	if err != nil {
		t.Fatal(err)
	}
	rm, err := NewRmGuardTool(config, logger)
	if err != nil {
		t.Fatal(err)
	}
	write, err := NewWriteGuardTool(config, logger)
	if err != nil {
		t.Fatal(err)
	}
	model, err := NewModelGuardTool(config, logger)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range []core.ToolValidator{kill, rm, write, model} {
		for _, name := range []string{"Bash", "Monitor"} {
			if !slices.Contains(tool.SupportedTools(), name) {
				t.Errorf("%s не подписан на %s: %v", tool.Name(), name, tool.SupportedTools())
			}
		}
	}

	// и та же команда в Monitor получает тот же отказ, что в Bash
	for _, tc := range []struct {
		tool    core.ToolValidator
		command string
	}{
		{newRmGuard(t), `rm -rf "$NOPE/"`},
		{newWriteGuard(t), "{ cat <<'EOF'\nrm -rf $NOPE/*\nEOF\n} > x"},
	} {
		result, err := tc.tool.ValidateTool(context.Background(),
			&core.ToolInput{ToolName: "Monitor", Command: tc.command, CWD: "/home/u/proj"})
		if err != nil {
			t.Fatalf("%s: %v", tc.tool.Name(), err)
		}
		if result.IsValid {
			t.Errorf("%s пропустил в Monitor: %s", tc.tool.Name(), tc.command)
		}
	}
}
