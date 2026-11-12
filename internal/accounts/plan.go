package accounts

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// UnitPlanUSD — цена подписки, в % недели которой показывается расход
// учётки (пользователь 12.11: «пока что показывается в % от $200, а надо от
// $100»; так же считает страница Сна)
const UnitPlanUSD = 100

// ratesProvider — поставщик учёток Claude в файле курсов
const ratesProvider = "anthropic"

// OtherUnitMark — пометка процента своей недели учётки, когда перевести его
// в неделю подписки за $100 нечем
const OtherUnitMark = "уч"

// Unit — перевод доли своей недели учётки в % недели подписки за $100.
// Scale 0 — перевода нет, Why — почему
type Unit struct {
	Scale float64
	Why   string
}

// OK — перевод есть
func (u Unit) OK() bool { return u.Scale > 0 }

// Pct — доля своей недели учётки в процентах недели подписки за $100; без
// перевода — как есть
func (u Unit) Pct(used float64) float64 {
	if !u.OK() {
		return used
	}
	return used * u.Scale
}

// Name — единица процента для текста
func (u Unit) Name() string {
	if !u.OK() {
		return "своей недели учётки"
	}
	return fmt.Sprintf("недели подписки за $%d", UnitPlanUSD)
}

// Percent — доля для текста, целым: «146%», без перевода — «73%уч»
func (u Unit) Percent(used float64) string {
	text := fmt.Sprintf("%.0f%%", u.Pct(used))
	if !u.OK() {
		text += OtherUnitMark
	}
	return text
}

// RatesFile — файл курсов подписок (~/.claude/improve/rates.tsv, его шапка):
// оттуда цена плана учёток Claude
func RatesFile() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w", err)
	}
	return filepath.Join(home, ".claude", "improve", "rates.tsv"), nil
}

// PlanUnit — перевод для учёток хранилища: цена плана — из действующей
// строки поставщика в файле курсов. Тарифы учёток разные — одной цены у них
// нет, перевода нет. Тариф учётки с ценой строки не сверяется: в курсах
// план назван иначе, чем rateLimitTier
func (s *Store) PlanUnit(now time.Time) Unit {
	if s != nil && s.MixedTiers() {
		return Unit{Why: "тарифы учёток разные, а цена плана в курсах одна"}
	}
	path, err := RatesFile()
	if err != nil {
		return Unit{Why: err.Error()}
	}
	price, err := planPrice(path, ratesProvider, now)
	if err != nil {
		return Unit{Why: err.Error()}
	}
	return Unit{Scale: price / UnitPlanUSD}
}

// planPrice — plan_usd_month поставщика по файлу курсов: действует строка с
// наибольшим valid_from не позже now, при равных — нижняя (шапка файла).
// Испорченная строка мешает, только если она и есть действующая: строки
// задним числом не правятся, и старая порча не должна ломать перевод навсегда
func planPrice(path, provider string, now time.Time) (float64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("цены плана нет: %w", err)
	}
	defer f.Close()
	var head []string
	var since time.Time
	found := false
	price, priceErr := 0.0, error(nil)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		cols := strings.Split(line, "\t")
		if head == nil {
			head = cols
			for _, need := range []string{"valid_from", "provider", "plan_usd_month"} {
				if !slices.Contains(head, need) {
					return 0, fmt.Errorf("цены плана нет: в шапке %s нет %s", path, need)
				}
			}
			continue
		}
		row := map[string]string{}
		for i, h := range head {
			if i < len(cols) {
				row[h] = cols[i]
			}
		}
		if row["provider"] != provider {
			continue
		}
		from, err := validFrom(row["valid_from"])
		if err != nil {
			return 0, fmt.Errorf("цены плана нет: %s: %w", path, err)
		}
		if from.After(now) || (found && from.Before(since)) {
			continue
		}
		found, since = true, from
		price, priceErr = rowPrice(row)
		if priceErr != nil {
			priceErr = fmt.Errorf("цены плана нет: %s: %w", path, priceErr)
		}
	}
	if err := sc.Err(); err != nil {
		return 0, fmt.Errorf("цены плана нет: %s: %w", path, err)
	}
	switch {
	case head == nil:
		return 0, errors.New("цены плана нет: в " + path + " нет шапки")
	case !found:
		return 0, errors.New("цены плана нет: в " + path + " нет действующей строки " + provider)
	case priceErr != nil:
		return 0, priceErr
	}
	return price, nil
}

// validFrom — начало действия строки курсов: дата (местная полночь) или
// RFC3339
func validFrom(s string) (time.Time, error) {
	from, err := time.ParseInLocation(time.DateOnly, s, time.Local)
	if err != nil {
		if from, err = time.Parse(time.RFC3339, s); err != nil {
			return time.Time{}, fmt.Errorf("valid_from %q: %w", s, err)
		}
	}
	return from, nil
}

// rowPrice — plan_usd_month строки курсов, положительный
func rowPrice(row map[string]string) (float64, error) {
	usd, err := strconv.ParseFloat(row["plan_usd_month"], 64)
	if err != nil || usd <= 0 {
		return 0, fmt.Errorf("plan_usd_month %q строки %s", row["plan_usd_month"], row["valid_from"])
	}
	return usd, nil
}
