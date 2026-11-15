package main

import (
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/aiseeq/akit/shellcmd"
	"mvdan.cc/sh/v3/syntax"
)

// Переменные оболочки в аргументах. shellcmd.Calls подставляет переменную только на место имени
// программы; в аргументах и целях перенаправлений `$F` остаётся как есть. Здесь присваивания
// команды собираются по порядку текста (`F=…;`, `export F=…`, во вложенном `bash -c '…'`) с
// литеральным значением: литерал, кавычки, `$HOME`, `~/` и ранее присвоенные переменные.
// Значение с подстановкой команды или неизвестной переменной делает переменную неизвестной.
// Префикс вызова `F=x cmd $F` в аргументы не попадает (так и в bash). Вложенный скрипт видит все
// переменные внешнего, не только экспортированные: запас в сторону попадания. Слово в одинарных
// кавычках shellcmd отдаёт значением, поэтому `'$F'` в аргументе тоже подставится.

// assign — присваивание: смещение в исходной команде, имя и значение; ok=false — значение не литерал.
type assign struct {
	off  int
	name string
	val  string
	ok   bool
}

// shellVars — присваивания команды по порядку текста.
type shellVars struct {
	list []assign
	errs []error // не разобранные оболочкой скрипты: их присваивания не видны
}

// collectVars — присваивания команды; ошибка — часть команды (вложенный скрипт) не разобрана.
func collectVars(cmd, home string) (shellVars, error) {
	var sv shellVars
	if !strings.Contains(cmd, "=") {
		return sv, nil
	}
	sv.collect(cmd, 0, home, map[string]string{}, 0)
	sort.SliceStable(sv.list, func(i, j int) bool { return sv.list[i].off < sv.list[j].off })
	return sv, errors.Join(sv.errs...)
}

// at — значения переменных перед смещением off; off < 0 (вызов не в исходном тексте) — все присваивания.
func (sv shellVars) at(off int) map[string]string {
	m := map[string]string{}
	for _, a := range sv.list {
		if off >= 0 && a.off >= off {
			break
		}
		if a.ok {
			m[a.name] = a.val
		} else {
			delete(m, a.name)
		}
	}
	return m
}

func (sv *shellVars) collect(src string, base int, home string, cur map[string]string, depth int) {
	if depth > 3 {
		return
	}
	f, err := syntax.NewParser().Parse(strings.NewReader(src), "")
	if err != nil {
		sv.errs = append(sv.errs, fmt.Errorf("присваивания скрипта: %w", err))
		return
	}
	syntax.Walk(f, func(n syntax.Node) bool {
		switch n := n.(type) {
		case *syntax.CallExpr:
			if len(n.Args) == 0 {
				for _, a := range n.Assigns {
					sv.add(a, base, home, cur)
				}
				return true
			}
			sv.nested(n, base, home, cur, depth)
		case *syntax.DeclClause:
			for _, a := range n.Args {
				sv.add(a, base, home, cur)
			}
		}
		return true
	})
}

func (sv *shellVars) add(a *syntax.Assign, base int, home string, cur map[string]string) {
	if a.Name == nil || a.Naked || a.Index != nil || a.Array != nil || a.Name.Value == "HOME" {
		return
	}
	name := a.Name.Value
	val, ok := "", true
	if a.Value != nil {
		val, ok = wordValue(a.Value, home, cur)
	}
	if ok && (val == "~" || strings.HasPrefix(val, "~/")) {
		val, ok = home+val[1:], home != ""
	}
	if a.Append {
		old, known := cur[name]
		val, ok = old+val, ok && known
	}
	if ok {
		cur[name] = val
	} else {
		delete(cur, name)
	}
	sv.list = append(sv.list, assign{off: base + int(a.Pos().Offset()), name: name, val: val, ok: ok})
}

// nested — присваивания во вложенном скрипте `bash -c '…'` (`-lc`, `-euo pipefail -c` …).
func (sv *shellVars) nested(c *syntax.CallExpr, base int, home string, cur map[string]string, depth int) {
	name, ok := wordValue(c.Args[0], "", nil)
	if !ok || !shellcmd.IsShell(filepath.Base(name)) {
		return
	}
	for i := 1; i+1 < len(c.Args); i++ {
		flag, ok := wordValue(c.Args[i], "", nil)
		if !ok || !strings.HasPrefix(flag, "-") || strings.HasPrefix(flag, "--") || !strings.Contains(flag, "c") {
			continue
		}
		w := c.Args[i+1]
		script, ok := wordValue(w, "", nil)
		if !ok {
			return
		}
		// одна часть в одинарных кавычках — текст скрипта совпадает с исходным, смещения точные;
		// иначе все присваивания скрипта встают на начало слова, в порядке текста
		off := base + int(w.Pos().Offset())
		if len(w.Parts) == 1 {
			if _, sq := w.Parts[0].(*syntax.SglQuoted); sq {
				off++
			}
		}
		sv.collect(script, off, home, maps.Clone(cur), depth+1)
		return
	}
}

// wordValue — значение слова без исполнения; false — в нём подстановка команды, арифметика или
// неизвестная переменная. cur == nil — переменные не подставляются.
func wordValue(w *syntax.Word, home string, cur map[string]string) (string, bool) {
	var b strings.Builder
	for _, p := range w.Parts {
		if !partValue(&b, p, home, cur) {
			return "", false
		}
	}
	return b.String(), true
}

var dqUnescape = strings.NewReplacer(`\"`, `"`, `\\`, `\`, `\$`, `$`, "\\`", "`")

func partValue(b *strings.Builder, p syntax.WordPart, home string, cur map[string]string) bool {
	switch p := p.(type) {
	case *syntax.Lit:
		b.WriteString(p.Value)
	case *syntax.SglQuoted:
		if p.Dollar {
			return false
		}
		b.WriteString(p.Value)
	case *syntax.DblQuoted:
		for _, q := range p.Parts {
			if lit, ok := q.(*syntax.Lit); ok {
				b.WriteString(dqUnescape.Replace(lit.Value))
				continue
			}
			if !partValue(b, q, home, cur) {
				return false
			}
		}
	case *syntax.ParamExp:
		v, ok := paramValue(p, home, cur)
		if !ok {
			return false
		}
		b.WriteString(v)
	default:
		return false
	}
	return true
}

func paramValue(p *syntax.ParamExp, home string, cur map[string]string) (string, bool) {
	if cur == nil || p.Param == nil || p.Excl || p.Length || p.Width || p.Index != nil || p.Slice != nil || p.Repl != nil || p.Names != 0 || p.Exp != nil {
		return "", false
	}
	if p.Param.Value == "HOME" {
		return home, home != ""
	}
	v, ok := cur[p.Param.Value]
	return v, ok
}

var reVarRef = regexp.MustCompile(`\$\{(\w+)\}|\$(\w+)`)

// expandVars подставляет известные переменные (`$F`, `${F}`); неизвестные остаются, и normPath их отбрасывает.
func expandVars(s string, vars map[string]string) string {
	if len(vars) == 0 || !strings.Contains(s, "$") {
		return s
	}
	return reVarRef.ReplaceAllStringFunc(s, func(m string) string {
		if v, ok := vars[strings.Trim(m, "${}")]; ok {
			return v
		}
		return m
	})
}
