package main

import (
	"strings"
	"testing"

	"github.com/aiseeq/chooks/internal/processor"
)

// Справка pre-tool-use называет каждую проверку движка и обе проверки вне
// него (подмена pgrep и сверка понимания): новая проверка без строки в
// справке — красный тест
func TestPreToolUseHelpNamesEveryCheck(t *testing.T) {
	names := append(processor.ToolNames(), pgrepAncestorsTool, understandCheckTool)
	if len(names) < 9 {
		t.Fatalf("проверок меньше ожидаемого: %v", names)
	}
	for _, name := range names {
		// пункт перечня: «  имя (инструменты) — …»; у understand-check
		// подкоманда пишется через дефис, ключ — через подчёркивание
		item := "\n  " + strings.ReplaceAll(name, "understand_check", "understand-check") + " ("
		if !strings.Contains(preToolUseLong, item) {
			t.Errorf("в справке pre-tool-use нет пункта %q", strings.TrimSpace(item))
		}
	}
}
