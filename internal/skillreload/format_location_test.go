package skillreload

import (
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/chooks/internal/core"
)

// Время в выдаче — в поясе запроса, а не процесса, и бюджет выдачи считается
// по тому же тексту
func TestFormatInRequestLocation(t *testing.T) {
	kathmandu, err := time.LoadLocation("Asia/Kathmandu")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	n := Notice{Kind: kindSkill, Name: "demo", LoadedAt: at, ChangedAt: at.Add(time.Minute)}
	got := FormatIn([]Notice{n}, kathmandu)
	if !strings.Contains(got, "22.11 15:45 +0545") || !strings.Contains(got, "22.11 15:46 +0545") {
		t.Errorf("время не в поясе запроса: %s", got)
	}
	if utc := FormatIn([]Notice{n}, time.UTC); !strings.Contains(utc, "22.11 10:00 UTC") {
		t.Errorf("время не в UTC: %s", utc)
	}
	fitted := fitBudget([]candidate{{notice: n}}, len(got)+len(noticeSeparator), kathmandu)
	if len(fitted) != 1 || fitted[0].Overflow {
		t.Errorf("выдача по размеру текста в поясе запроса не уложилась: %+v", fitted)
	}
}

// Checker меряет выдачу в поясе запроса (Location), а не в time.Local
// процесса: при бюджете ровно под текст в поясе запроса правка показана
// целиком. Тесты пакета не параллельны, time.Local подменяется на время теста
func TestCheckerBudgetInRequestLocation(t *testing.T) {
	kathmandu, err := time.LoadLocation("Asia/Kathmandu")
	if err != nil {
		t.Fatal(err)
	}
	defer func(old *time.Location) { time.Local = old }(time.Local)
	time.Local = kathmandu
	base := time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)
	run := func(budget int) []Notice {
		defer func(old int) { outputBudget = old }(outputBudget)
		outputBudget = budget
		f := newFixture(t)
		f.checker.Location = time.UTC
		f.writeSkill("# А\nкоротко.", base)
		f.load("# А\nкоротко.", base.Add(time.Minute))
		f.check(Event{})
		f.writeSkill("# А\nдлиннее и по-другому.", base.Add(5*time.Minute))
		return f.check(Event{})
	}
	first := run(outputBudget)
	if len(first) != 1 || first[0].Diff == "" {
		t.Fatalf("ждали правку с разницей, получили %+v", first)
	}
	text := FormatIn(first, time.UTC)
	if !strings.Contains(text, "UTC") {
		t.Fatalf("время не в поясе запроса: %s", text)
	}
	got := run(core.UTF16Len(text) + len(noticeSeparator))
	if len(got) != 1 || got[0].Diff == "" || got[0].Overflow {
		t.Errorf("при бюджете ровно под текст в поясе запроса правка не показана целиком: %+v", got)
	}
}
