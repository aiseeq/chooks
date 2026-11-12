package statusline

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

// unitPlanUSD — цена подписки, в % недели которой показывается расход
// учётки (пользователь 12.11: «пока что показывается в % от $200, а надо от
// $100»; так же считает страница Сна)
const unitPlanUSD = 100

// ratesProvider — поставщик учёток Claude в файле курсов
const ratesProvider = "anthropic"

// otherUnitMark — пометка процента своей недели учётки, когда перевести его
// в неделю подписки за $100 нечем
const otherUnitMark = "уч"

// planUnit — перевод доли своей недели учётки в % недели подписки за $100.
// scale 0 — перевода нет, why — почему
type planUnit struct {
	scale float64
	why   string
}

// ok — перевод есть
func (u planUnit) ok() bool { return u.scale > 0 }

// pct — доля своей недели учётки в процентах недели подписки за $100; без
// перевода — как есть
func (u planUnit) pct(used float64) float64 {
	if !u.ok() {
		return used
	}
	return used * u.scale
}

// name — единица процента для текста
func (u planUnit) name() string {
	if !u.ok() {
		return "своей недели учётки"
	}
	return fmt.Sprintf("недели подписки за $%d", unitPlanUSD)
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

// accountUnit — перевод для учёток Claude: цена плана — из действующей
// строки поставщика в файле курсов. Тарифы учёток разные — одной цены у них
// нет, перевода нет
func accountUnit(mixedTiers bool, now time.Time) planUnit {
	if mixedTiers {
		return planUnit{why: "тарифы учёток разные, а цена плана в курсах одна"}
	}
	path, err := RatesFile()
	if err != nil {
		return planUnit{why: err.Error()}
	}
	price, err := planPrice(path, ratesProvider, now)
	if err != nil {
		return planUnit{why: err.Error()}
	}
	return planUnit{scale: price / unitPlanUSD}
}

// planPrice — plan_usd_month поставщика по файлу курсов: действует строка с
// наибольшим valid_from не позже now, при равных — нижняя (шапка файла)
func planPrice(path, provider string, now time.Time) (float64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("цены плана нет: %w", err)
	}
	defer f.Close()
	var head []string
	var since time.Time
	price := 0.0
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
		from, err := time.ParseInLocation(time.DateOnly, row["valid_from"], time.Local)
		if err != nil {
			if from, err = time.Parse(time.RFC3339, row["valid_from"]); err != nil {
				return 0, fmt.Errorf("цены плана нет: %s valid_from %q: %w", path, row["valid_from"], err)
			}
		}
		if from.After(now) || from.Before(since) {
			continue
		}
		usd, err := strconv.ParseFloat(row["plan_usd_month"], 64)
		if err != nil || usd <= 0 {
			return 0, fmt.Errorf("цены плана нет: %s plan_usd_month %q строки %s", path, row["plan_usd_month"], row["valid_from"])
		}
		since, price = from, usd
	}
	if err := sc.Err(); err != nil {
		return 0, fmt.Errorf("цены плана нет: %s: %w", path, err)
	}
	if price == 0 {
		return 0, errors.New("цены плана нет: в " + path + " нет действующей строки " + provider)
	}
	return price, nil
}
