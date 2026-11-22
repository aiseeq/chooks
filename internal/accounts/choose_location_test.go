package accounts

import (
	"strings"
	"testing"
	"time"
)

// Время в причине выбора и конец входа — в поясе now (у хука — пояс
// запроса), а не в time.Local процесса
func TestChooseReasonInRequestLocation(t *testing.T) {
	kathmandu, err := time.LoadLocation("Asia/Kathmandu")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC).In(kathmandu)
	s := &Store{Active: "a", Accounts: []Account{account("a", 20*day, now), account("b", 20*day, now), account("c", 30*time.Minute, now)}}
	usage := map[string]Usage{
		"a@example.com": {Week: Window{Used: 30, ResetsAt: now.Add(5 * day)}},
		"b@example.com": {Week: Window{Used: 90, ResetsAt: now.Add(2 * day).UTC()}},
	}
	result := chooseResultWithUnit(s, usage, now, Unit{})
	// сброс b — 24.10 10:00 UTC, в Катманду 15:45
	if !strings.Contains(result.reason, "сб 24.10 15:45") {
		t.Errorf("причина %q, ожидалось время сброса в поясе запроса", result.reason)
	}
	reasons := strings.Join(noSwitchReasons(s, usage, now), "\n")
	if !strings.Contains(reasons, "сб 24.10 15:45") || !strings.Contains(reasons, "+05:45") {
		t.Errorf("причины %q, ожидались время сброса и конец входа в поясе запроса", reasons)
	}
	if got := ShortTimeIn(time.Date(2026, 10, 26, 11, 0, 0, 0, time.UTC), kathmandu); got != "пн 26.10 16:45" {
		t.Errorf("ShortTimeIn: %q", got)
	}
}
