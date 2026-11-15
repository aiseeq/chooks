package main

import (
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/aiseeq/akit/shellcmd"
	"mvdan.cc/sh/v3/syntax"
)

// Переменные оболочки по разбору mvdan.cc/sh: shellcmd.Calls подставляет переменную только на
// место имени программы и не отдаёт объявлений (`export F=…` — DeclClause, не вызов).
//
// Аргументы. Присваивания команды собираются по порядку текста (`F=…;`, `export F=…`, во
// вложенном `bash -c '…'`) с литеральным значением: литерал, кавычки, `$HOME`, `~/` и ранее
// присвоенные переменные. Значение с подстановкой команды или неизвестной переменной делает
// переменную неизвестной. Префикс вызова `F=x cmd $F` в аргументы не попадает (так и в bash).
// Здесь порядок только по тексту: присваивание в подоболочке и во вложенном скрипте видно и
// после них, вложенный скрипт видит все переменные внешнего, не только экспортированные, — запас в
// сторону попадания. Слово в одинарных кавычках shellcmd отдаёт значением, поэтому `'$F'` в
// аргументе тоже подставится.
//
// Окружение вызова (callEnv: XDG_CONFIG_HOME пробы, GIT_DIR) ведётся по процессам, как в bash:
// export и префикс действуют в той же оболочке и во вложенных в неё после них; подоболочка
// `( … )`, `$( … )`, `<( … )`, элемент конвейера и задание `&` — отдельный процесс, их присваивания
// наружу не выходят; вложенный `bash -c` получает только экспортированное. `env -i`, `env -u` и
// присваивания env действуют на программу за ним. Где окружение не ясно (ветка if, `&&`/`||`,
// цикл, тело функции, eval, source, sudo без -E, ssh, su), переменная неясна (unclear).

// assign — присваивание: смещение в исходной команде, имя и значение; ok=false — значение не литерал.
type assign struct {
	off  int
	name string
	val  string
	ok   bool
	temp bool // значение во временном каталоге: `$(mktemp …)`, `$TMPDIR/…`, /tmp/…, scratchpad
}

// shellVars — присваивания команды по порядку текста и окружения её простых команд.
type shellVars struct {
	list  []assign
	calls []callRec
	errs  []error // не разобранные оболочкой скрипты: их присваивания не видны
}

// envVar — переменная в окружении оболочки.
type envVar struct {
	val      string // значение, если lit
	lit      bool
	temp     bool
	set      bool // присвоена (export X без значения — exported без set)
	exported bool
	// unclear — значение или экспорт неизвестны; val и прочие поля — один из возможных вариантов
	unclear bool
}

// shellEnv — переменные оболочки. Переменная, которой нет, неизвестной быть не может: проба и
// GIT_DIR требуют присваивания, а неизвестное (eval, source) задаёт только unclear у названных.
type shellEnv struct {
	vars map[string]envVar
}

func newEnv() shellEnv { return shellEnv{vars: map[string]envVar{}} }

func (e shellEnv) clone() shellEnv { return shellEnv{vars: maps.Clone(e.vars)} }

// get — переменная; false — её нет.
func (e shellEnv) get(name string) (envVar, bool) {
	v, ok := e.vars[name]
	return v, ok
}

// blur делает неясными все переменные, кроме keep.
func (e *shellEnv) blur(keep map[string]bool) {
	for n, v := range e.vars {
		if !keep[n] {
			v.unclear = true
			e.vars[n] = v
		}
	}
}

// exported — окружение дочернего процесса: экспортированные переменные.
func (e shellEnv) exported() shellEnv {
	out := newEnv()
	for n, v := range e.vars {
		if v.exported {
			out.vars[n] = v
		}
	}
	return out
}

// mergeEnv — окружение после одного из двух путей: где пути расходятся, переменная неясна.
// Неясной переменной остаётся значение пути, где она присвоена и экспортирована (GIT_DIR
// конфиг-репо в одной ветке — запас в сторону попадания).
func mergeEnv(a, b shellEnv) shellEnv {
	out := newEnv()
	merge := func(n string) {
		va, oka := a.get(n)
		vb, okb := b.get(n)
		if oka == okb && va == vb {
			if oka {
				out.vars[n] = va
			}
			return
		}
		m := envVar{}
		switch {
		case vb.set && vb.exported:
			m = vb
		case va.set && va.exported:
			m = va
		}
		m.unclear = true
		out.vars[n] = m
	}
	for n := range a.vars {
		merge(n)
	}
	for n := range b.vars {
		merge(n)
	}
	return out
}

// callRec — простая команда: её слова и окружение программы (с префиксами `X=… cmd`).
type callRec struct {
	words []wordRec
	env   shellEnv
}

// wordRec — слово команды: смещение в исходной команде, литеральное значение (ok) и текст в скрипте.
type wordRec struct {
	off int
	val string
	ok  bool
	raw string
}

// collectVars — присваивания и окружения команды; calls — её вызовы по shellcmd: вложенная
// оболочка за обёрткой (`timeout 60 bash -c`) узнаётся по смещению вызова оболочки. Ошибка —
// часть команды (вложенный скрипт) не разобрана.
func collectVars(cmd, home string, calls []shellcmd.Call) (shellVars, error) {
	var sv shellVars
	if !strings.Contains(cmd, "=") {
		return sv, nil
	}
	shells := map[int]bool{}
	for _, c := range calls {
		if c.Offset >= 0 && shellcmd.IsShell(c.Name) {
			shells[c.Offset] = true
		}
	}
	w := &walker{sv: &sv, home: home, shells: shells, cur: map[string]string{}, tmp: map[string]bool{}}
	w.script(cmd, nil, newEnv())
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

// tempVar — последнее присваивание name перед off — во временный каталог.
func (sv shellVars) tempVar(name string, off int) bool {
	temp := false
	for _, a := range sv.list {
		if off >= 0 && a.off >= off {
			break
		}
		if a.name == name {
			temp = a.temp
		}
	}
	return temp
}

// callEnv — окружение программы, имя которой стоит в исходной команде на смещении off: после
// присваиваний и export перед ней, префиксов и обёрток env/sudo/ssh в той же простой команде.
// false — простая команда с таким словом не разобрана (в команде нет «=», вложенный скрипт в
// heredoc или ssh, имя из переменной): окружение неизвестно. Слово `bash -c graft\ …` — и слово
// внешней команды, и имя во вложенном скрипте; окружения у них одни: вложенный скрипт получает
// экспортированное окружение за теми же обёртками.
func (sv shellVars) callEnv(off int) (shellEnv, bool) {
	for _, r := range sv.calls {
		for k, wd := range r.words {
			if wd.off == off {
				return wrapEnv(r.env, r.words[:k]), true
			}
		}
	}
	return shellEnv{}, false
}

// wrapEnv — окружение программы за обёртками words: env меняет его, sudo без -E, ssh, su — сбрасывают.
func wrapEnv(env shellEnv, words []wordRec) shellEnv {
	e := env.clone()
	for i := 0; i < len(words); i++ {
		if !words[i].ok {
			continue
		}
		switch filepath.Base(words[i].val) {
		case "env":
			i = e.envArgs(words, i+1) - 1
		case "sudo":
			i = e.sudoArgs(words, i+1) - 1
		case "ssh", "su", "runuser", "doas", "tmux":
			e.blur(nil)
		}
	}
	return e
}

// envArgs — опции и присваивания `env` с words[i]; возвращает индекс программы. GNU env кончает
// опции на первом не-опционном слове: `env A=1 -u X` запускает программу «-u».
func (e *shellEnv) envArgs(words []wordRec, i int) int {
	opts := true
	for ; i < len(words); i++ {
		wd := words[i]
		a := wd.val
		if !wd.ok {
			a = wd.raw
		}
		if opts && wd.ok && strings.HasPrefix(a, "-") {
			if a == "--" {
				opts = false
				continue
			}
			i = e.envOption(words, i)
			continue
		}
		name, val, ok := strings.Cut(a, "=")
		if !ok || !validName(name) {
			return i
		}
		opts = false
		v := envVar{set: true, exported: true}
		if wd.ok {
			v.val, v.lit, v.temp = val, true, tempDir(val)
		} else {
			_, raw, _ := strings.Cut(wd.raw, "=")
			v.temp = tempRaw(raw)
		}
		e.vars[name] = v
	}
	return i
}

// envOption — опция env в words[i]; возвращает индекс её последнего слова.
func (e *shellEnv) envOption(words []wordRec, i int) int {
	a := words[i].val
	next := func() string {
		if i+1 < len(words) {
			i++
			return words[i].val
		}
		return ""
	}
	if long, ok := strings.CutPrefix(a, "--"); ok {
		name, val, hasVal := strings.Cut(long, "=")
		switch {
		case name == "ignore-environment":
			*e = newEnv()
		case name == "unset" && hasVal:
			e.vars[val] = envVar{}
		case name == "unset":
			e.vars[next()] = envVar{}
		case name == "chdir" && !hasVal:
			next()
		case name == "chdir", name == "null", name == "debug", strings.HasSuffix(name, "-signal"):
		default:
			e.blur(nil) // --split-string и прочее: окружение не разобрано
		}
		return i
	}
	if a == "-" {
		*e = newEnv()
		return i
	}
	for j := 1; j < len(a); j++ {
		switch a[j] {
		case 'i':
			*e = newEnv()
		case '0', 'v':
		case 'u':
			name := a[j+1:]
			if name == "" {
				name = next()
			}
			e.vars[name] = envVar{}
			return i
		case 'C':
			if j+1 == len(a) {
				next()
			}
			return i
		default:
			e.blur(nil)
			return i
		}
	}
	return i
}

// sudoValued — короткие опции sudo со значением (как в shellcmd).
const sudoValued = "ugCDhprtTU"

// sudoArgs — опции sudo с words[i]; возвращает индекс программы. Без -E (--preserve-env) sudo
// сбрасывает окружение (env_reset), присваивания в его словах проходят по политике sudoers: всё неясно.
func (e *shellEnv) sudoArgs(words []wordRec, i int) int {
	preserve, keep := false, map[string]bool{}
	for ; i < len(words); i++ {
		a := words[i].val
		if !words[i].ok {
			break
		}
		if a == "--" {
			i++
			break
		}
		if long, ok := strings.CutPrefix(a, "--"); ok {
			name, val, hasVal := strings.Cut(long, "=")
			switch {
			case name == "preserve-env" && hasVal:
				for n := range strings.SplitSeq(val, ",") {
					keep[n] = true
				}
			case name == "preserve-env":
				preserve = true
			case !hasVal && slices.Contains([]string{"user", "group", "chdir", "host", "prompt", "role", "type", "command-timeout", "other-user", "close-from"}, name):
				i++
			}
			continue
		}
		if strings.HasPrefix(a, "-") && len(a) > 1 {
			p, valued := sudoShort(a)
			preserve = preserve || p
			if valued {
				i++
			}
			continue
		}
		name, _, ok := strings.Cut(a, "=")
		if !ok || !validName(name) {
			break
		}
		e.vars[name] = envVar{set: true, exported: true, unclear: true}
	}
	if !preserve {
		e.blur(keep)
	}
	return i
}

// sudoShort — связка коротких опций sudo: есть ли -E и ждёт ли последняя опция значения в следующем слове.
func sudoShort(a string) (preserve, valued bool) {
	for j := 1; j < len(a); j++ {
		if a[j] == 'E' {
			preserve = true
		}
		if strings.IndexByte(sudoValued, a[j]) >= 0 {
			return preserve, j+1 == len(a)
		}
	}
	return preserve, false
}

var reName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func validName(s string) bool { return reName.MatchString(s) }

// tempRaw — текст значения (без кавычек в начале) во временном каталоге или в переменной, заданной так.
func tempRaw(raw string) bool { return reTempValue.MatchString(strings.TrimLeft(raw, `"'`)) }

// walker обходит скрипт: src — его текст, offs — смещения его байтов в исходной команде
// (nil — src и есть исходная команда).
type walker struct {
	sv     *shellVars
	src    string
	offs   []int
	home   string
	shells map[int]bool
	cur    map[string]string // литеральные значения по порядку текста (для аргументов)
	tmp    map[string]bool   // переменная по порядку текста — во временном каталоге
	depth  int
}

// abs — смещение позиции скрипта в исходной команде (offs вложенного скрипта — на байт длиннее
// его текста: конец последнего слова).
func (w *walker) abs(o int) int {
	if w.offs == nil {
		return o
	}
	return w.offs[o]
}

func (w *walker) pos(p syntax.Pos) int { return w.abs(int(p.Offset())) }

func (w *walker) text(n syntax.Node) string {
	return w.src[n.Pos().Offset():n.End().Offset()]
}

func (w *walker) script(src string, offs []int, env shellEnv) {
	if w.depth > 3 {
		return
	}
	f, err := syntax.NewParser().Parse(strings.NewReader(src), "")
	if err != nil {
		w.sv.errs = append(w.sv.errs, fmt.Errorf("присваивания скрипта: %w", err))
		return
	}
	w.src, w.offs = src, offs
	w.stmts(f.Stmts, &env)
}

func (w *walker) stmts(list []*syntax.Stmt, env *shellEnv) {
	for _, s := range list {
		w.stmt(s, env)
	}
}

func (w *walker) stmt(s *syntax.Stmt, env *shellEnv) {
	if s.Background {
		e := env.clone()
		env = &e
	}
	if s.Cmd != nil {
		w.cmd(s.Cmd, env)
	}
	for _, r := range s.Redirs {
		w.subs(r, env)
	}
}

// subs обходит подстановки команд и процессов в n: каждая — отдельный процесс.
func (w *walker) subs(n syntax.Node, env *shellEnv) {
	syntax.Walk(n, func(m syntax.Node) bool {
		var list []*syntax.Stmt
		switch m := m.(type) {
		case *syntax.CmdSubst:
			list = m.Stmts
		case *syntax.ProcSubst:
			list = m.Stmts
		default:
			return true
		}
		e := env.clone()
		w.stmts(list, &e)
		return false
	})
}

func (w *walker) cmd(c syntax.Command, env *shellEnv) {
	switch c := c.(type) {
	case *syntax.CallExpr:
		w.call(c, env)
	case *syntax.DeclClause:
		w.decl(c, env)
	case *syntax.Block:
		w.stmts(c.Stmts, env)
	case *syntax.Subshell:
		e := env.clone()
		w.stmts(c.Stmts, &e)
	case *syntax.BinaryCmd:
		if c.Op == syntax.Pipe || c.Op == syntax.PipeAll {
			x, y := env.clone(), env.clone()
			w.stmt(c.X, &x)
			w.stmt(c.Y, &y)
			return
		}
		_, *env = w.andOr(c, *env)
	case *syntax.IfClause:
		*env = w.ifClause(c, *env)
	case *syntax.WhileClause:
		b := env.clone()
		w.stmts(c.Cond, &b)
		w.stmts(c.Do, &b)
		*env = mergeEnv(*env, b)
	case *syntax.ForClause:
		w.subs(c.Loop, env)
		b := env.clone()
		w.stmts(c.Do, &b)
		*env = mergeEnv(*env, b)
	case *syntax.CaseClause:
		w.subs(c.Word, env)
		res := env.clone()
		for _, it := range c.Items {
			for _, p := range it.Patterns {
				w.subs(p, env)
			}
			b := env.clone()
			w.stmts(it.Stmts, &b)
			res = mergeEnv(res, b)
		}
		*env = res
	case *syntax.FuncDecl:
		// тело исполняется при вызове функции: что оно меняет, неясно и после объявления
		b := env.clone()
		w.stmt(c.Body, &b)
		*env = mergeEnv(*env, b)
	case *syntax.TimeClause:
		if c.Stmt != nil {
			w.stmt(c.Stmt, env)
		}
	case *syntax.CoprocClause:
		e := env.clone()
		w.stmt(c.Stmt, &e)
	default:
		w.subs(c, env)
	}
}

// andOr — список `&&`/`||`: окружение, при котором исполняется следующий операнд `&&` (все
// операнды цепочки `&&` до него исполнены), и окружение после списка (часть операндов могла не
// исполниться — где пути расходятся, переменная неясна).
func (w *walker) andOr(c *syntax.BinaryCmd, env shellEnv) (straight, after shellEnv) {
	sx, ax := w.operand(c.X, env)
	if c.Op == syntax.AndStmt {
		sy, ay := w.operand(c.Y, sx.clone())
		return sy, mergeEnv(ax, ay)
	}
	// правый операнд `||` исполняется, когда левый не удался: после него — любое из окружений левого
	sy, ay := w.operand(c.Y, ax.clone())
	return mergeEnv(sx, sy), mergeEnv(ax, ay)
}

func (w *walker) operand(s *syntax.Stmt, env shellEnv) (straight, after shellEnv) {
	// операнд списка не бывает заданием `&` и не несёт перенаправлений: они у его последней команды
	if b, ok := s.Cmd.(*syntax.BinaryCmd); ok && (b.Op == syntax.AndStmt || b.Op == syntax.OrStmt) {
		return w.andOr(b, env)
	}
	w.stmt(s, &env)
	return env, env
}

func (w *walker) ifClause(c *syntax.IfClause, env shellEnv) shellEnv {
	if len(c.Cond) == 0 { // else
		w.stmts(c.Then, &env)
		return env
	}
	w.stmts(c.Cond, &env)
	t := env.clone()
	w.stmts(c.Then, &t)
	if c.Else == nil {
		return mergeEnv(env, t)
	}
	return mergeEnv(t, w.ifClause(c.Else, env.clone()))
}

func (w *walker) call(c *syntax.CallExpr, env *shellEnv) {
	for _, a := range c.Assigns {
		w.subs(a, env)
	}
	for _, a := range c.Args {
		w.subs(a, env)
	}
	if len(c.Args) == 0 {
		for _, a := range c.Assigns {
			w.assign(a, env, 0)
		}
		return
	}
	pe := env.clone()
	for _, a := range c.Assigns {
		if v, ok := w.value(a); ok {
			v.exported = true
			pe.vars[a.Name.Value] = v
		}
	}
	words := make([]wordRec, len(c.Args))
	for i, a := range c.Args {
		val, ok := wordValue(a, w.home, w.cur)
		words[i] = wordRec{off: w.pos(a.Pos()), val: val, ok: ok, raw: w.text(a)}
	}
	w.sv.calls = append(w.sv.calls, callRec{words: words, env: pe})
	if words[0].ok {
		switch words[0].val {
		case "unset":
			for _, wd := range words[1:] {
				if wd.val == "-f" {
					break
				}
				if !strings.HasPrefix(wd.val, "-") {
					env.vars[wd.val] = envVar{}
				}
			}
		case "eval", "source", ".", "export", "declare", "typeset", "local", "readonly":
			env.blur(nil)
		}
	}
	w.nested(c, words, pe)
}

// value — присваивание a: значение по порядку текста и признак временного каталога. false — не
// присваивание скалярной переменной (массив, индекс, `export X` без значения) или HOME.
func (w *walker) value(a *syntax.Assign) (envVar, bool) {
	if a.Name == nil || a.Naked || a.Index != nil || a.Array != nil || a.Name.Value == "HOME" {
		return envVar{}, false
	}
	name := a.Name.Value
	val, ok := "", true
	raw := ""
	if a.Value != nil {
		val, ok = wordValue(a.Value, w.home, w.cur)
		raw = w.text(a.Value)
	}
	if ok && (val == "~" || strings.HasPrefix(val, "~/")) {
		val, ok = w.home+val[1:], w.home != ""
	}
	temp := false
	if a.Append {
		old, known := w.cur[name]
		val, ok = old+val, ok && known
		temp = w.tmp[name]
	}
	switch {
	case ok:
		temp = tempDir(val)
	case !a.Append:
		temp = tempRaw(raw)
		if m := reLeadVar.FindStringSubmatch(strings.TrimLeft(raw, `"`)); m != nil && w.tmp[m[1]] {
			temp = true
		}
	}
	return envVar{val: val, lit: ok, temp: temp, set: true}, true
}

// assign — присваивание переменной оболочки; export: 1 — экспортировать, -1 — снять экспорт.
func (w *walker) assign(a *syntax.Assign, env *shellEnv, export int) {
	v, ok := w.value(a)
	if !ok {
		return
	}
	name := a.Name.Value
	if v.lit {
		w.cur[name] = v.val
	} else {
		delete(w.cur, name)
	}
	w.tmp[name] = v.temp
	w.sv.list = append(w.sv.list, assign{off: w.pos(a.Pos()), name: name, val: v.val, ok: v.lit, temp: v.temp})
	old, _ := env.get(name)
	v.exported, v.unclear = old.exported, old.unclear
	switch export {
	case 1:
		v.exported, v.unclear = true, false
	case -1:
		v.exported = false
	}
	env.vars[name] = v
}

// decl — export, declare, typeset, local, readonly: присваивания и флаг экспорта (-x, +x, export -n).
func (w *walker) decl(d *syntax.DeclClause, env *shellEnv) {
	for _, a := range d.Args {
		w.subs(a, env)
	}
	export := 0
	if d.Variant.Value == "export" {
		export = 1
	}
	for _, a := range d.Args {
		if a.Name == nil {
			f, ok := wordValue(a.Value, "", nil)
			switch {
			case !ok:
				env.blur(nil)
				return
			case strings.Contains(f, "f") || strings.Contains(f, "p"):
				return // функции или печать
			case d.Variant.Value == "export" && strings.Contains(f, "n"), strings.HasPrefix(f, "+") && strings.Contains(f, "x"):
				export = -1
			case strings.HasPrefix(f, "-") && strings.Contains(f, "x"):
				export = 1
			}
			continue
		}
		if !a.Naked {
			w.assign(a, env, export)
			continue
		}
		v, _ := env.get(a.Name.Value)
		switch export {
		case 1:
			v.exported = true
		case -1:
			v.exported = false
		}
		env.vars[a.Name.Value] = v
	}
}

// nested — вложенный скрипт `bash -c '…'` (`-lc`, `-euo pipefail -c` …), в том числе за обёрткой
// (`timeout 60 bash -c`): он получает экспортированное окружение вызова, его присваивания наружу
// не выходят.
func (w *walker) nested(c *syntax.CallExpr, words []wordRec, env shellEnv) {
	si := -1
	if name, ok := wordValue(c.Args[0], "", nil); ok && shellcmd.IsShell(filepath.Base(name)) {
		si = 0
	} else {
		for i, wd := range words[1:] {
			if w.shells[wd.off] {
				si = i + 1
				break
			}
		}
	}
	if si < 0 {
		return
	}
	for i := si + 1; i+1 < len(c.Args); i++ {
		flag, ok := wordValue(c.Args[i], "", nil)
		if !ok || !strings.HasPrefix(flag, "-") || strings.HasPrefix(flag, "--") || !strings.Contains(flag, "c") {
			continue
		}
		script, offs, ok := w.scriptWord(c.Args[i+1])
		if !ok {
			return
		}
		nw := &walker{sv: w.sv, home: w.home, shells: w.shells, cur: maps.Clone(w.cur), tmp: maps.Clone(w.tmp), depth: w.depth + 1}
		nw.script(script, offs, wrapEnv(env, words[:si]).exported())
		return
	}
}

// scriptWord — текст скрипта в слове после снятия кавычек и смещения его байтов в исходной
// команде. Переменная в двойных кавычках (`"… $TMPDIR …"`) остаётся своим текстом: внешняя
// оболочка подставит то же значение, что вложенная. false — в слове подстановка команды и прочее.
func (w *walker) scriptWord(word *syntax.Word) (string, []int, bool) {
	var b []byte
	var offs []int
	emit := func(s string, start int, unescape func(next byte) bool) {
		for i := 0; i < len(s); i++ {
			if unescape != nil && s[i] == '\\' && i+1 < len(s) && unescape(s[i+1]) {
				i++
				if s[i] == '\n' {
					continue
				}
			}
			b = append(b, s[i])
			offs = append(offs, w.abs(start+i))
		}
	}
	anyEscape := func(byte) bool { return true }
	dq := func(c byte) bool { return strings.IndexByte("\"\\$`\n", c) >= 0 }
	for _, p := range word.Parts {
		switch p := p.(type) {
		case *syntax.Lit:
			emit(p.Value, int(p.Pos().Offset()), anyEscape)
		case *syntax.SglQuoted:
			if p.Dollar {
				return "", nil, false
			}
			emit(p.Value, int(p.Pos().Offset())+1, nil)
		case *syntax.DblQuoted:
			if p.Dollar {
				return "", nil, false
			}
			for _, q := range p.Parts {
				switch q := q.(type) {
				case *syntax.Lit:
					emit(q.Value, int(q.Pos().Offset()), dq)
				case *syntax.ParamExp:
					emit(w.text(q), int(q.Pos().Offset()), nil)
				default:
					return "", nil, false
				}
			}
		default:
			return "", nil, false
		}
	}
	offs = append(offs, w.abs(int(word.End().Offset())))
	return string(b), offs, true
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
