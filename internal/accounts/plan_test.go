package accounts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ratesHead — шапка файла курсов с нужными колонками
const ratesHead = "# шапка\nvalid_from\tprovider\tplan\tplan_usd_month\tapi_usd_week\n"

// homeRates кладёт файл курсов с телом body во временный HOME и
// возвращает его путь
func homeRates(t *testing.T, body string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, err := RatesFile()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// Цена плана — строка anthropic с наибольшим valid_from не позже now, при
// равных — нижняя; будущая и чужого поставщика не действуют; порча старой
// строки не мешает, порча действующей — ошибка
func TestPlanPrice(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.Local)
	tests := []struct {
		name, body string
		want       float64
	}{
		{"действующая — последняя по дате", ratesHead +
			"2026-01-01\tanthropic\tmax-5x\t100\t800\n" +
			"2026-02-01\topenai\tpro\t300\t200\n" +
			"2026-02-01\tanthropic\tmax-20x\t50\t1600\n" +
			"2026-02-01\tanthropic\tultra\t200\t1600\n" +
			"2999-01-01\tanthropic\tfuture\t400\t1600\n", 200},
		{"не по порядку дат", ratesHead +
			"2026-03-01\tanthropic\tultra\t200\t1600\n" +
			"2026-01-01\tanthropic\tmax-5x\t100\t800\n", 200},
		{"порча старой строки выше действующей", ratesHead +
			"2026-01-01\tanthropic\tmax-5x\t\t800\n" +
			"2026-03-01\tanthropic\tultra\t200\t1600\n", 200},
		{"концы строк CRLF, цена — последняя колонка", "valid_from\tprovider\tplan\tplan_usd_month\r\n2026-03-01\tanthropic\tultra\t200\r\n", 200},
		{"порча действующей строки", ratesHead +
			"2026-01-01\tanthropic\tmax-5x\t100\t800\n" +
			"2026-03-01\tanthropic\tultra\t\t1600\n", 0},
		{"нет строки поставщика", ratesHead + "2026-03-01\topenai\tpro\t100\t200\n", 0},
		{"пустой файл", "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := planPrice(homeRates(t, tt.body), ratesProvider, now)
			if tt.want == 0 {
				if err == nil {
					t.Errorf("ожидалась ошибка, цена %v", got)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Errorf("цена %v, ошибка %v; ожидалось %v", got, err, tt.want)
			}
		})
	}
	if _, err := planPrice(filepath.Join(t.TempDir(), "none.tsv"), ratesProvider, now); err == nil {
		t.Error("нет файла — ожидалась ошибка")
	}
}

// tiered — учётка с тарифом tier
func tiered(t *testing.T, uuid, tier string, now time.Time) Account {
	t.Helper()
	a := account(uuid, 20*day, now)
	var m map[string]any
	if err := json.Unmarshal(a.OAuth, &m); err != nil {
		t.Fatal(err)
	}
	m["rateLimitTier"] = tier
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	a.OAuth = raw
	return a
}

// Перевод — цена плана / $100; разные тарифы учёток — перевода нет
func TestPlanUnit(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.Local)
	homeRates(t, ratesHead+"2026-03-01\tanthropic\tultra\t200\t1600\n")
	s := &Store{Accounts: []Account{tiered(t, "a", "default_claude_max_20x", now), tiered(t, "b", "default_claude_max_20x", now)}}
	if u := s.PlanUnit(now); u.Percent(73) != "146%" {
		t.Errorf("план $200, 73: %q (%s)", u.Percent(73), u.Why)
	}
	var none *Store
	if u := none.PlanUnit(now); u.Percent(40) != "80%" {
		t.Errorf("без хранилища: %q (%s)", u.Percent(40), u.Why)
	}
	s.Accounts = append(s.Accounts, tiered(t, "c", "default_claude_max_5x", now))
	if u := s.PlanUnit(now); u.OK() || u.Percent(73) != "73%"+OtherUnitMark || !strings.Contains(u.Why, "тарифы") {
		t.Errorf("разные тарифы: %q (%s)", u.Percent(73), u.Why)
	}
}

// Причина ротации — в % недели подписки за $100 (лид 12.11 17:39)
func TestChooseReasonUnit(t *testing.T) {
	now := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	homeRates(t, ratesHead+"2026-03-01\tanthropic\tultra\t200\t1600\n")
	s := &Store{Active: "a", Accounts: []Account{account("a", 20*day, now), account("b", 20*day, now)}}
	usage := map[string]Usage{
		"a@example.com": {Week: Window{Used: 99, ResetsAt: now.Add(2 * day)}},
		"b@example.com": {Week: Window{Used: 40, ResetsAt: now.Add(3 * day)}},
	}
	_, reason := choose(s, usage, now)
	if !strings.Contains(reason, "пуста (неделя 198%)") || !strings.Contains(reason, "неделя 80%") {
		t.Errorf("причина %q", reason)
	}
}
