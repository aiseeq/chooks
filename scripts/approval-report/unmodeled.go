package main

import (
	"maps"
	"regexp"
	"slices"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// Конструкции оболочки, которые обход не моделирует (что с ними делает разбор — в шапке vars.go).

// set — запись переменной оболочки: имя попадает во все открытые учёты затронутого (track).
func (w *walker) set(env *shellEnv, name string, v envVar) {
	env.vars[name] = v
	w.touched(name)
}

func (w *walker) touched(names ...string) {
	for _, m := range w.touch {
		for _, n := range names {
			m[n] = true
		}
	}
}

// track — имена переменных, которые затронул f (присвоил, экспортировал, снял, сделал неясными);
// внешние учёты видят их сами: touched пишет во все открытые.
func (w *walker) track(f func()) map[string]bool {
	m := map[string]bool{}
	w.touch = append(w.touch, m)
	f()
	w.touch = w.touch[:len(w.touch)-1]
	return m
}

// obscure делает неясными переменные names: значение остаётся одним из вариантов (GIT_DIR — запас
// в сторону попадания), признаки — неизвестны.
func (w *walker) obscure(env *shellEnv, names map[string]bool) {
	for n := range names {
		v := env.vars[n]
		v.unclear, v.loose = true, true
		w.set(env, n, v)
	}
}

// fuzz — неясное перед каждой командой после trap, alias, `set -o posix`: переменные fuzzy (all — все).
type fuzz struct {
	names map[string]bool
	all   bool
}

func (f *fuzz) add(names map[string]bool, all bool) {
	if f.names == nil {
		f.names = map[string]bool{}
	}
	maps.Copy(f.names, names)
	f.all = f.all || all
}

func (w *walker) applyFuzz(env *shellEnv) {
	if w.fuzzy.all {
		env.blur(nil)
		return
	}
	w.obscure(env, w.fuzzy.names)
}

// scanScript — свойства скрипта целиком: что трогают тела его функций, есть ли
// `shopt` и `set -o` с ключом, меняющим конвейеры или правила присваиваний.
func (w *walker) scanScript(f *syntax.File) {
	if w.fnTouch == nil {
		w.fnTouch = map[string]bool{}
	}
	syntax.Walk(f, func(n syntax.Node) bool {
		switch n := n.(type) {
		case *syntax.FuncDecl:
			names, all := touchedBy(n.Body)
			maps.Copy(w.fnTouch, names)
			w.fnAll = w.fnAll || all
		case *syntax.CallExpr:
			w.scanOptions(n)
		}
		return true
	})
}

// safeShopt, safeSetO — ключи, которые не меняют ни конвейеров, ни присваиваний.
var (
	safeShopt = map[string]bool{"-s": true, "-u": true, "-q": true, "-p": true, "extglob": true, "nullglob": true, "globstar": true, "dotglob": true, "nocaseglob": true, "nocasematch": true, "failglob": true, "inherit_errexit": true}
	safeSetO  = map[string]bool{"errexit": true, "nounset": true, "pipefail": true, "xtrace": true, "verbose": true, "noclobber": true, "noglob": true, "errtrace": true}
)

func (w *walker) scanOptions(c *syntax.CallExpr) {
	words := litWords(c.Args)
	if len(words) == 0 {
		return
	}
	switch words[0] {
	case "shopt":
		for _, a := range words[1:] {
			w.pipesUnclear = w.pipesUnclear || !safeShopt[a]
		}
	case "set":
		for i, a := range words[1:] {
			if !reSetO.MatchString(a) || i+2 >= len(words) {
				continue
			}
			key := words[i+2]
			w.pipesUnclear = w.pipesUnclear || !safeSetO[key]
			w.fuzzy.all = w.fuzzy.all || key == "posix" || key == "<?>"
		}
	}
}

// reSetO — флаги set с ключом -o в конце: `-o`, `+o`, `-euo`.
var reSetO = regexp.MustCompile(`^[-+][a-zA-Z]*o$`)

// litWords — значения слов; слово не литерал — "<?>".
func litWords(args []*syntax.Word) []string {
	out := make([]string, len(args))
	for i, a := range args {
		v, ok := wordValue(a, "", nil)
		if !ok {
			v = "<?>"
		}
		out[i] = v
	}
	return out
}

// touchedBy — переменные, которые может затронуть узел, по тексту; all — неизвестно какие
// (eval, source, имя из подстановки, nameref).
func touchedBy(node syntax.Node) (map[string]bool, bool) {
	names, all := map[string]bool{}, false
	syntax.Walk(node, func(n syntax.Node) bool {
		switch n := n.(type) {
		case *syntax.Assign:
			if n.Name != nil {
				names[n.Name.Value] = true
			}
		case *syntax.DeclClause:
			all = all || declNameref(n)
		case *syntax.WordIter:
			names[n.Name.Value] = true
		case *syntax.CallExpr:
			ws := litWords(n.Args)
			w, a := callWrites(ws[builtinShiftLit(ws):])
			maps.Copy(names, w)
			all = all || a
		case *syntax.ArithmCmd, *syntax.ArithmExp, *syntax.LetClause, *syntax.CStyleLoop:
			all = true // значения имён в арифметике по тексту не известны: см. arith
			return false
		}
		return true
	})
	return names, all
}

func builtinShiftLit(ws []string) int {
	words := make([]wordRec, len(ws))
	for i, v := range ws {
		words[i] = wordRec{val: v, ok: true} // "<?>" не совпадёт ни с builtin, ни с command
	}
	return builtinShift(words)
}

// callWrites — переменные, которые пишет простая команда с этими словами (после builtin/command):
// unset, read, mapfile, getopts, `printf -v`, `wait -p`; all — неизвестно какие.
func callWrites(ws []string) (map[string]bool, bool) {
	names := map[string]bool{}
	if len(ws) == 0 {
		return names, false
	}
	switch ws[0] {
	case "<?>", "eval", "source", ".", "let", "alias", "trap":
		return names, true
	case "unset", "read", "mapfile", "readarray", "getopts":
		for _, a := range ws[1:] {
			if a == "<?>" {
				return names, true
			}
			names[a] = true // и флаги: лишнее имя ничего не меняет
		}
	case "printf", "wait":
		if i := slices.Index(ws, map[string]string{"printf": "-v", "wait": "-p"}[ws[0]]); i > 0 && i+1 < len(ws) {
			names[ws[i+1]] = true
			return names, ws[i+1] == "<?>"
		}
	}
	return names, false
}

// arith — арифметика `(( ))`, `$(( ))`, let, `for ((…))`: значение переменной в ней вычисляется
// как выражение (`i='X=1'; (( i ))` присвоит X). Имя, значение которого не известное целое, —
// неясно всё; иначе присваиваются только её имена, и только целые: ни временного каталога, ни
// GIT_DIR из них не выйдет.
func (w *walker) arith(n syntax.Node, env *shellEnv) {
	for name := range arithNames(n) {
		if v, _ := env.get(name); !v.lit || v.unclear || !reInt.MatchString(v.val) { // нет в окружении — не lit
			env.blur(nil)
			return
		}
	}
}

var reInt = regexp.MustCompile(`^-?[0-9]+$`)

// arithNames — имена в арифметике: литералы-имена и подстановки `$X`.
func arithNames(n syntax.Node) map[string]bool {
	names := map[string]bool{}
	syntax.Walk(n, func(m syntax.Node) bool {
		switch m := m.(type) {
		case *syntax.Word:
			if v := m.Lit(); validName(v) {
				names[v] = true
			}
		case *syntax.ParamExp:
			names[paramName(m)] = true
		}
		return true
	})
	return names
}

// declNameref — объявление ссылки (`declare -n`, `local -n`): присваивания через неё не видны.
func declNameref(d *syntax.DeclClause) bool {
	if d.Variant.Value == "export" {
		return false
	}
	for _, a := range d.Args {
		if a.Name == nil {
			f, ok := wordValue(a.Value, "", nil)
			if !ok || strings.Contains(f, "n") { // и +n: снятие ссылки — запас в сторону неясного
				return true
			}
		}
	}
	return false
}

// pathWord — в слове есть литеральная «/» (`"$D/bin/x"`): это путь к программе, не встроенная
// команда и не функция.
func pathWord(word *syntax.Word) bool {
	found := false
	syntax.Walk(word, func(n syntax.Node) bool {
		switch n := n.(type) {
		case *syntax.Lit:
			found = found || strings.Contains(n.Value, "/")
		case *syntax.ParamExp, *syntax.CmdSubst, *syntax.ArithmExp, *syntax.ProcSubst:
			return false
		}
		return true
	})
	return found
}

// earlyExit — в теле функции return, exit, break или continue: часть тела могла не исполниться.
func earlyExit(body syntax.Node) bool {
	found := false
	syntax.Walk(body, func(n syntax.Node) bool {
		if c, ok := n.(*syntax.CallExpr); ok && len(c.Args) > 0 {
			switch c.Args[0].Lit() {
			case "return", "exit", "break", "continue":
				found = true
			}
		}
		return !found
	})
	return found
}

// trap — `trap ДЕЙСТВИЕ СИГНАЛ…`: действие исполняется когда угодно дальше (DEBUG — перед каждой
// командой), поэтому то, что оно и функции скрипта трогают, неясно перед каждой командой.
func (w *walker) trap(args []wordRec) {
	if len(args) > 0 && args[0].ok && args[0].val == "--" {
		args = args[1:]
	}
	if len(args) < 2 || args[0].ok && (args[0].val == "" || strings.HasPrefix(args[0].val, "-")) {
		return // сброс, снятие, печать
	}
	var f fuzz
	f.add(w.fnTouch, w.fnAll || !args[0].ok)
	if args[0].ok {
		p, err := syntax.NewParser().Parse(strings.NewReader(args[0].val), "")
		f.all = f.all || err != nil // действие не разобрано: неизвестно, что оно трогает
		if err == nil {
			f.add(touchedBy(p))
		}
	}
	w.fuzzy.add(f.names, f.all) // действует с команды после trap
}
