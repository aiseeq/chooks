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
		{"концы строк CRLF", "valid_from\tprovider\tplan\tplan_usd_month\tapi_usd_week\r\n2026-03-01\tanthropic\tultra\t200\t1600\r\n", 200},
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
					t.Errorf("ожидалась ошибка, цена %v", got.usd)
				}
				return
			}
			if err != nil || got.usd != tt.want {
				t.Errorf("цена %v, ошибка %v; ожидалось %v", got.usd, err, tt.want)
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

// ratesTierHead — шапка курсов с колонкой tier: rateLimitTier учёток, на
// которых мерили строку
const ratesTierHead = "valid_from\tprovider\tplan\tplan_usd_month\tapi_usd_week\ttier\n"

// Тариф учёток сверяется с tier действующей строки: учётки max_5x при
// строке $200 для max_20x — не удвоенные доли, а пометка «уч»; tier строки
// «-» тоже не подтверждает цену; без колонки tier — как прежде
func TestPlanUnitTier(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.Local)
	five := &Store{Accounts: []Account{tiered(t, "a", "default_claude_max_5x", now), tiered(t, "b", "default_claude_max_5x", now)}}
	twenty := &Store{Accounts: []Account{tiered(t, "a", "default_claude_max_20x", now)}}
	tests := []struct {
		name, body string
		store      *Store
		want       string
	}{
		{"max_5x при строке $200 для max_20x", ratesTierHead +
			"2026-03-01\tanthropic\tultra\t200\t1600\tdefault_claude_max_20x\n", five, "73%" + OtherUnitMark},
		{"тариф совпал", ratesTierHead +
			"2026-01-01\tanthropic\tmax-5x\t100\t800\tdefault_claude_max_5x\n" +
			"2026-03-01\tanthropic\tultra\t200\t1600\tdefault_claude_max_20x\n", twenty, "146%"},
		{"совпал тариф старой, а действует другая", ratesTierHead +
			"2026-01-01\tanthropic\tmax-5x\t100\t800\tdefault_claude_max_5x\n" +
			"2026-03-01\tanthropic\tultra\t200\t1600\tdefault_claude_max_20x\n", five, "73%" + OtherUnitMark},
		{"tier строки не указан", ratesTierHead +
			"2026-03-01\tanthropic\tultra\t200\t1600\t-\n", twenty, "73%" + OtherUnitMark},
		{"колонки tier нет", ratesHead + "2026-03-01\tanthropic\tultra\t200\t1600\n", five, "146%"},
		{"хранилища нет — тариф не известен", ratesTierHead +
			"2026-03-01\tanthropic\tultra\t200\t1600\tdefault_claude_max_20x\n", nil, "73%" + OtherUnitMark},
		{"поле tier пустое, хранилища нет", ratesTierHead +
			"2026-03-01\tanthropic\tultra\t200\t1600\t\n", nil, "73%" + OtherUnitMark},
		{"строка короче шапки, хранилища нет", ratesTierHead +
			"2026-03-01\tanthropic\tultra\t200\t1600\n", nil, "73%" + OtherUnitMark},
		{"мёртвая учётка с другим тарифом не мешает", ratesTierHead +
			"2026-03-01\tanthropic\tultra\t200\t1600\tdefault_claude_max_20x\n",
			&Store{Accounts: []Account{tiered(t, "a", "default_claude_max_20x", now), dead(tiered(t, "b", "default_claude_max_5x", now))}}, "146%"},
		{"токены живой учётки не разобрать", ratesTierHead +
			"2026-03-01\tanthropic\tultra\t200\t1600\tdefault_claude_max_20x\n",
			&Store{Accounts: []Account{tiered(t, "a", "default_claude_max_20x", now), {UUID: "b", OAuth: json.RawMessage(`{"claudeAiOauth":`)}}}, "73%" + OtherUnitMark},
		{"у живой учётки тарифа нет", ratesTierHead +
			"2026-03-01\tanthropic\tultra\t200\t1600\tdefault_claude_max_20x\n",
			&Store{Accounts: []Account{tiered(t, "a", "default_claude_max_20x", now), tiered(t, "b", "", now)}}, "73%" + OtherUnitMark},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			homeRates(t, tt.body)
			u := tt.store.PlanUnit(now)
			if got := u.Percent(73); got != tt.want {
				t.Errorf("73: %q, ожидалось %q (%s)", got, tt.want, u.Why)
			}
			if strings.HasPrefix(tt.name, "строка короче") {
				if !strings.HasPrefix(u.Why, "цены плана нет:") {
					t.Errorf("ошибка ширины без пользовательского префикса: %q", u.Why)
				}
				return
			}
			if !u.OK() && !strings.Contains(u.Why, "тариф учёток") {
				t.Errorf("причина без тарифа: %q", u.Why)
			}
		})
	}
}

// dead — учётка, помеченная мёртвой
func dead(a Account) Account {
	a.Dead = "токен отозван"
	return a
}

// Дата без времени разбирается akit/rates в локальном поясе процесса.
func TestPlanPriceDateInLocalLocation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rates.tsv")
	rates := "valid_from\tprovider\tplan\tplan_usd_month\tapi_usd_week\n2026-01-01\tanthropic\tmax\t100\t800\n2026-03-01\tanthropic\tmax\t200\t1600\n"
	if err := os.WriteFile(path, []byte(rates), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 3, 1, 0, 30, 0, 0, time.Local)
	plan, err := planPrice(path, ratesProvider, now)
	if err != nil {
		t.Fatal(err)
	}
	if plan.usd != 200 {
		t.Errorf("plan_usd_month %v, want 200 from the row valid since midnight local time", plan.usd)
	}
}

// Дата без времени — в поясе запроса, а не процесса: хук считает в поясе
// вызывающего (hookctx), и на границе суток действует строка его даты.
func TestPlanPriceDateInRequestLocation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rates.tsv")
	rates := "valid_from\tprovider\tplan\tplan_usd_month\tapi_usd_week\n2026-02-01\tanthropic\tmax\t100\t800\n2026-03-01\tanthropic\tmax\t200\t1600\n"
	if err := os.WriteFile(path, []byte(rates), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TZ", "UTC")
	tokyo := time.FixedZone("UTC+9", 9*3600)
	plan, err := planPrice(path, ratesProvider, time.Date(2026, 3, 1, 0, 30, 0, 0, tokyo))
	if err != nil {
		t.Fatal(err)
	}
	if plan.usd != 200 {
		t.Errorf("plan_usd_month %v, want 200: 1 марта 00:30 в поясе запроса", plan.usd)
	}
}

// Колонка api_usd_week chooks не нужна: файл без неё читается.
func TestPlanPriceWithoutWeekColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rates.tsv")
	if err := os.WriteFile(path, []byte("valid_from\tprovider\tplan_usd_month\n2026-01-01\tanthropic\t200\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := planPrice(path, ratesProvider, time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if plan.usd != 200 {
		t.Errorf("plan_usd_month %v, want 200", plan.usd)
	}
}
