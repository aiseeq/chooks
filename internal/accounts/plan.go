package accounts

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/aiseeq/akit/rates"
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
// нет, перевода нет. Колонка tier в курсах — rateLimitTier учёток, на
// которых мерили строку: тариф учёток не известен или не совпал — цена не
// подтверждена, перевода нет. Колонки нет — тариф не сверяется (план в
// курсах назван иначе)
func (s *Store) PlanUnit(now time.Time) Unit {
	path, err := RatesFile()
	return s.planUnit(now, path, err)
}

// PlanUnitForHome reads rates from the hook request's home directory.
func (s *Store) PlanUnitForHome(now time.Time, home string) Unit {
	if home == "" {
		return s.planUnit(now, "", fmt.Errorf("home dir: $HOME is not defined"))
	}
	return s.planUnit(now, filepath.Join(home, ".claude", "improve", "rates.tsv"), nil)
}

func (s *Store) planUnit(now time.Time, path string, pathErr error) Unit {
	if s != nil && s.MixedTiers() {
		return Unit{Why: "тарифы учёток разные, а цена плана в курсах одна"}
	}
	if pathErr != nil {
		return Unit{Why: pathErr.Error()}
	}
	plan, err := planPrice(path, ratesProvider, now)
	if err != nil {
		return Unit{Why: err.Error()}
	}
	if tier := s.Tier(); plan.tierKnown && (tier == "" || plan.tier != tier) {
		if tier == "" {
			tier = "не известен"
		}
		return Unit{Why: fmt.Sprintf("тариф учёток %s, а цена плана в курсах — для %s", tier, plan.tier)}
	}
	return Unit{Scale: plan.usd / UnitPlanUSD}
}

// ratePlan — цена плана из действующей строки курсов и тариф учёток, на
// которых её мерили (tierKnown — в шапке есть колонка tier)
type ratePlan struct {
	usd       float64
	tier      string
	tierKnown bool
}

// planPrice — plan_usd_month и tier поставщика по файлу курсов: действует
// самая поздняя строка (при равных — последняя в файле). Испорченная строка мешает, только если она и есть действующая:
// строки задним числом не правятся, и старая порча не должна ломать перевод
// навсегда
func planPrice(path, provider string, now time.Time) (ratePlan, error) {
	table, err := rates.Read(path, now)
	if err != nil {
		return ratePlan{}, fmt.Errorf("цены плана нет: %w", err)
	}
	if !table.HasColumn("plan_usd_month") {
		return ratePlan{}, fmt.Errorf("цены плана нет: в шапке %s нет plan_usd_month", path)
	}
	row, ok := table.Latest(provider, "")
	if !ok {
		return ratePlan{}, fmt.Errorf("цены плана нет: в %s нет действующей строки %s", path, provider)
	}
	if row.InvalidPlan != nil {
		return ratePlan{}, fmt.Errorf("цены плана нет: %w", row.InvalidPlan)
	}
	plan := ratePlan{usd: row.PlanUSDMonth, tierKnown: table.HasColumn("tier")}
	if plan.tierKnown {
		plan.tier, _ = row.Cell("tier")
	}
	return plan, nil
}
