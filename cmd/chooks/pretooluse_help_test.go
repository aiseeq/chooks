package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/aiseeq/chooks/internal/processor"
)

// Справка pre-tool-use называет каждую проверку движка и обе проверки вне
// него (подмена pgrep и сверка понимания): новая проверка без строки в
// справке — красный тест
func TestPreToolUseHelpNamesEveryCheck(t *testing.T) {
	names := append(processor.ToolNames(), pgrepAncestorsTool, wideSearchHintTool, understandCheckTool)
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

// Строка справки «Без ключа включены …» перечисляет ровно проверки,
// включённые без ключа: у движка — onWhenAbsent, вне него — pgrep и сверка
// понимания (выключает только ключ с enabled: false)
func TestPreToolUseHelpDefaultOn(t *testing.T) {
	start := strings.Index(preToolUseLong, "Без ключа включены ")
	end := strings.Index(preToolUseLong, ", остальные выключены.")
	if start < 0 || end < start {
		t.Fatalf("в справке нет строки «Без ключа включены …, остальные выключены.»")
	}
	list := strings.NewReplacer("\n", " ", " и ", ", ").Replace(preToolUseLong[start+len("Без ключа включены ") : end])
	var got []string
	for name := range strings.SplitSeq(list, ",") {
		if name = strings.TrimSpace(name); name != "" {
			got = append(got, name)
		}
	}
	want := append(processor.DefaultOnNames(), pgrepAncestorsTool, wideSearchHintTool, understandCheckTool)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("без ключа в справке %v, в коде %v", got, want)
	}
}
