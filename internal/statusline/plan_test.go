package statusline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeRates кладёт файл курсов во временный HOME: действующая строка
// anthropic с ценой плана price; пустая price — файла нет
func writeRates(t *testing.T, price string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if price == "" {
		return
	}
	dir := filepath.Join(home, ".claude", "improve")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	rates := "# шапка\n" +
		"valid_from\tprovider\tplan\tplan_usd_month\tapi_usd_week\n" +
		"2026-01-01\tanthropic\tmax-5x\t100\t800\n" +
		"2026-02-01\topenai\tpro\t300\t200\n" +
		"2026-02-01\tanthropic\tmax-20x\t50\t1600\n" +
		"2026-02-01\tanthropic\tultra\t" + price + "\t1600\n" +
		"2999-01-01\tanthropic\tfuture\t400\t1600\n"
	if err := os.WriteFile(filepath.Join(dir, "rates.tsv"), []byte(rates), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Цена плана — строка anthropic с наибольшим valid_from не позже now, при
// равных — нижняя; будущая и чужого поставщика не действуют
func TestPlanPrice(t *testing.T) {
	writeRates(t, "200")
	path, err := RatesFile()
	if err != nil {
		t.Fatal(err)
	}
	price, err := planPrice(path, ratesProvider, time.Now())
	if err != nil || price != 200 {
		t.Fatalf("цена %v, ошибка %v; ожидалось 200", price, err)
	}
	if _, err := planPrice(path, "deepseek", time.Now()); err == nil {
		t.Error("нет строки поставщика — ожидалась ошибка")
	}
	if _, err := planPrice(filepath.Join(t.TempDir(), "none.tsv"), ratesProvider, time.Now()); err == nil {
		t.Error("нет файла — ожидалась ошибка")
	}
}

// Полоса недели — от ёмкости учётки, число — в % недели подписки за $100;
// цены плана нет — % своей недели с пометкой (пользователь 12.11)
func TestWeeklyBarPlanUnit(t *testing.T) {
	resets := time.Now().Add(6 * 24 * time.Hour)
	tests := []struct {
		name string
		unit planUnit
		used float64
		want string
	}{
		{"план $200", planUnit{scale: 2}, 73, "нед ▓▓▓▓▓▓▓░░░ 146%"},
		{"план $100", planUnit{scale: 1}, 40, "нед ▓▓▓▓░░░░░░ 40%"},
		{"цены плана нет", planUnit{why: "нет файла"}, 73, "нед ▓▓▓▓▓▓▓░░░ 73%" + otherUnitMark},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := plain(weeklyBar(weekBar{label: "нед", unit: tt.unit}, weeklyUsage{used: tt.used, resetsAt: resets}, time.Now()))
			if got != tt.want {
				t.Errorf("получено %q, ожидалось %q", got, tt.want)
			}
		})
	}
}

// Статусная строка и chooks limits берут цену плана из файла курсов
func TestPlanUnitRenderAndLimits(t *testing.T) {
	for _, tt := range []struct {
		price, line, limits string
	}{
		{"200", "▓▓▓▓▓▓▓░░░ 146%", "неделя 146%"},
		{"100", "▓▓▓▓▓▓▓░░░ 73%", "неделя 73%"},
		{"", "▓▓▓▓▓▓▓░░░ 73%" + otherUnitMark, "неделя 73%"},
	} {
		t.Run("цена "+tt.price, func(t *testing.T) {
			isolateRender(t)
			writeRates(t, tt.price)
			writeStore(t, false)
			resets := time.Now().Add(5 * 24 * time.Hour).Truncate(time.Hour).Unix()
			input := fmt.Sprintf(`{"session_id":"s","cwd":%q,"rate_limits":{"seven_day":{"used_percentage":73,"resets_at":%d}}}`, t.TempDir(), resets)
			line, err := Render(context.Background(), strings.NewReader(input), testLogger(t))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(plain(line), tt.line) {
				t.Errorf("строка %q, ожидалось %q", plain(line), tt.line)
			}
			var out strings.Builder
			if err := WriteLimits(&out, false); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), tt.limits) {
				t.Errorf("в chooks limits нет %q:\n%s", tt.limits, out.String())
			}
		})
	}
}
