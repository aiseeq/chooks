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
// вложенном `bash -c '…'`) с литеральным значением: литерал, кавычки, `$HOME`, `~/` и
// переменные, присвоенные в той же оболочке раньше (по окружению, как ниже). Значение с
// подстановкой команды или неизвестной переменной делает переменную неизвестной. Префикс вызова
// `F=x cmd $F` в аргументы не попадает (так и в bash). Аргументы видят присваивания только по
// порядку текста: присваивание в подоболочке и во вложенном скрипте видно и после них — запас в
// сторону попадания. Слово в одинарных кавычках shellcmd отдаёт значением, поэтому `'$F'` в
// аргументе тоже подставится.
//
// Окружение вызова (callEnv: XDG_CONFIG_HOME пробы, GIT_DIR; shellAt: переменная в пути cd) ведётся
// по процессам, как в bash: присваивание и export действуют в той же оболочке и во вложенных в неё
// после них; подоболочка `( … )`, `$( … )`, `<( … )`, элемент конвейера и задание `&` — отдельный
// процесс, их присваивания наружу не выходят; вложенный `bash -c` получает только
// экспортированное. `env -i`, `env -u`, присваивания и `-C` env действуют на программу за ним.
// Тело функции исполняется при её вызове; тело цикла — дважды, второй раз с окружением после
// первого. Где пути расходятся (ветка if и case, `&&`/`||`, цикл), переменная неясна (unclear);
// после eval, source, sudo без -E, ssh, su — неясно всё.
// assign — присваивание: смещение в исходной команде, имя и значение; ok=false — значение не литерал.
type assign struct {
	off  int
	name string
	val  string
	ok   bool
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
	temp     bool // значение во временном каталоге: `$(mktemp …)`, `$TMPDIR/…`, /tmp/…, scratchpad
	set      bool // присвоена (export X без значения — exported без set)
	exported bool
	// unclear — значение неизвестно (пути расходятся): val и прочие поля — один из вариантов
	unclear bool
	// loose — неизвестно и то, присвоена ли, экспортирована ли и во временном ли она каталоге
	loose bool
}

// shellEnv — переменные и функции оболочки. Переменной, которой нет в vars, команда не трогала:
// её значение пришло извне и неизвестно.
type shellEnv struct {
	vars map[string]envVar
	// funcs — тела функций по имени; nil в списке — функция могла быть не объявлена (ветка)
	funcs map[string][]*syntax.Stmt
	// chdir — каталоги `env -C`, `sudo -D` перед программой по порядку; слово без значения — каталог неизвестен
	chdir []wordRec
}

func newEnv() shellEnv {
	return shellEnv{vars: map[string]envVar{}, funcs: map[string][]*syntax.Stmt{}}
}

func (e shellEnv) clone() shellEnv {
	return shellEnv{vars: maps.Clone(e.vars), funcs: maps.Clone(e.funcs), chdir: slices.Clip(e.chdir)}
}

// get — переменная; false — команда её не трогала.
func (e shellEnv) get(name string) (envVar, bool) {
	v, ok := e.vars[name]
	return v, ok
}

// lits — литеральные значения присвоенных переменных: для подстановки в слова.
func (e shellEnv) lits() map[string]string {
	m := map[string]string{}
	for n, v := range e.vars {
		if v.lit && !v.unclear {
			m[n] = v.val
		}
	}
	return m
}

// blur делает неясными все переменные, кроме keep.
func (e *shellEnv) blur(keep map[string]bool) {
	for n, v := range e.vars {
		if !keep[n] {
			v.unclear, v.loose = true, true
			e.vars[n] = v
		}
	}
}

// exported — окружение дочернего процесса: экспортированные переменные, без функций. Каталог,
// сменённый обёрткой (`env -C`), ребёнку неизвестен: cd внутри него этот разбор не сопоставит.
func (e shellEnv) exported() shellEnv {
	out := newEnv()
	for n, v := range e.vars {
		if v.exported {
			out.vars[n] = v
		}
	}
	if len(e.chdir) > 0 {
		out.chdir = []wordRec{{}}
	}
	return out
}

// tempText — текст значения (без кавычек в начале) во временном каталоге: `$TMPDIR/…`,
// `$(mktemp …)`, /tmp/… Ведущая переменная, которую команда присвоила, — по её присваиванию.
func (e shellEnv) tempText(raw string) bool {
	s := strings.TrimLeft(raw, `"'`)
	if m := reLeadVar.FindStringSubmatch(s); m != nil {
		if v, ok := e.get(m[1]); ok {
			return v.temp && !v.loose
		}
	}
	return reTempValue.MatchString(s)
}

// mergeEnv — окружение после одного из двух путей: где пути расходятся, переменная неясна.
// Неясной переменной остаётся значение пути, где она присвоена и экспортирована (GIT_DIR
// конфиг-репо в одной ветке — запас в сторону попадания).
func mergeEnv(a, b shellEnv) shellEnv {
	out := shellEnv{vars: mergeVars(a.vars, b.vars), funcs: mergeFuncs(a.funcs, b.funcs), chdir: a.chdir}
	if !slices.Equal(a.chdir, b.chdir) {
		out.chdir = []wordRec{{}}
	}
	return out
}

func mergeVars(a, b map[string]envVar) map[string]envVar {
	out := map[string]envVar{}
	merge := func(n string) {
		va, oka := a[n]
		vb, okb := b[n]
		if oka == okb && va == vb {
			out[n] = va
			return
		}
		m := envVar{}
		switch {
		case vb.set && vb.exported:
			m = vb
		case va.set && va.exported:
			m = va
		}
		// известны экспорт и временный каталог; присвоена ли — неважно: значение и так неясно
		same := va.exported == vb.exported && va.temp == vb.temp
		m.unclear, m.loose = true, !same || va.loose || vb.loose
		out[n] = m
	}
	for n := range a {
		merge(n)
	}
	for n := range b {
		if _, ok := a[n]; !ok {
			merge(n)
		}
	}
	return out
}

// mergeFuncs — функции после одного из путей: объявленная лишь на одном — с телом и без (nil).
func mergeFuncs(a, b map[string][]*syntax.Stmt) map[string][]*syntax.Stmt {
	out := map[string][]*syntax.Stmt{}
	maybe := []*syntax.Stmt{nil}
	for n, fa := range a {
		fb, ok := b[n]
		if !ok {
			fb = maybe
		}
		out[n] = unionStmts(fa, fb)
	}
	for n, fb := range b {
		if _, ok := a[n]; !ok {
			out[n] = unionStmts(maybe, fb)
		}
	}
	return out
}

func unionStmts(a, b []*syntax.Stmt) []*syntax.Stmt {
	out := slices.Clone(a)
	for _, s := range b {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// callRec — простая команда: её слова, окружение программы (с префиксами `X=… cmd`) и
// переменные оболочки, в которой раскрываются слова (без префиксов).
type callRec struct {
	words []wordRec
	env   shellEnv
	sh    shellEnv
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
	w := &walker{sv: &sv, home: home, shells: shells}
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

// callEnv — окружение программы, имя которой стоит в исходной команде на смещении off: после
// присваиваний и export перед ней, префиксов и обёрток env/sudo/ssh в той же простой команде.
// Команда, исполненная не раз (тело цикла, функция), — слитое окружение всех исполнений.
// false — простая команда с таким словом не разобрана (в команде нет «=», вложенный скрипт в
// heredoc или ssh, имя из переменной, функцию не вызывали): окружение неизвестно. Слово
// `bash -c graft\ …` — и слово внешней команды, и имя во вложенном скрипте: их окружения сливаются.
func (sv shellVars) callEnv(off int) (shellEnv, bool) {
	return sv.find(off, func(r callRec, k int) shellEnv { return wrapEnv(r.env, r.words[:k]) })
}

// shellAt — переменные оболочки, раскрывающей слова простой команды со словом на смещении off;
// команда не разобрана — пустые.
func (sv shellVars) shellAt(off int) shellEnv {
	e, ok := sv.find(off, func(r callRec, _ int) shellEnv { return r.sh })
	if !ok {
		return newEnv()
	}
	return e
}

func (sv shellVars) find(off int, env func(r callRec, k int) shellEnv) (shellEnv, bool) {
	var out shellEnv
	found := false
	for _, r := range sv.calls {
		k := slices.IndexFunc(r.words, func(wd wordRec) bool { return wd.off == off })
		switch {
		case k < 0:
		case found:
			out = mergeEnv(out, env(r, k))
		default:
			out, found = env(r, k), true
		}
	}
	return out, found
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
			e.chdir = append(e.chdir, wordRec{})
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
			v.temp = e.tempText(raw)
		}
		e.vars[name] = v
	}
	return i
}

// envOption — опция env в words[i]; возвращает индекс её последнего слова.
func (e *shellEnv) envOption(words []wordRec, i int) int {
	a := words[i].val
	next := func() wordRec {
		if i+1 < len(words) {
			i++
			return words[i]
		}
		return wordRec{}
	}
	if long, ok := strings.CutPrefix(a, "--"); ok {
		name, val, hasVal := strings.Cut(long, "=")
		switch {
		case name == "ignore-environment":
			*e = newEnv()
		case name == "unset" && hasVal:
			e.vars[val] = envVar{}
		case name == "unset":
			e.vars[next().val] = envVar{}
		case name == "chdir" && hasVal:
			e.chdir = append(e.chdir, wordRec{val: val, ok: true})
		case name == "chdir":
			e.chdir = append(e.chdir, next())
		case name == "null", name == "debug", strings.HasSuffix(name, "-signal"):
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
				name = next().val
			}
			e.vars[name] = envVar{}
			return i
		case 'C':
			dir := wordRec{val: a[j+1:], ok: true}
			if dir.val == "" {
				dir = next()
			}
			e.chdir = append(e.chdir, dir)
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
			case name == "chdir":
				e.chdir = append(e.chdir, wordRec{})
				i += boolInt(!hasVal)
			case !hasVal && slices.Contains([]string{"user", "group", "host", "prompt", "role", "type", "command-timeout", "other-user", "close-from"}, name):
				i++
			}
			continue
		}
		if strings.HasPrefix(a, "-") && len(a) > 1 {
			p, valued, dir := sudoShort(a)
			preserve = preserve || p
			if dir {
				e.chdir = append(e.chdir, wordRec{})
			}
			i += boolInt(valued)
			continue
		}
		name, _, ok := strings.Cut(a, "=")
		if !ok || !validName(name) {
			break
		}
		e.vars[name] = envVar{set: true, exported: true, unclear: true, loose: true}
	}
	if !preserve {
		e.blur(keep)
	}
	return i
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// sudoShort — связка коротких опций sudo: есть ли -E, ждёт ли последняя опция значения в
// следующем слове и есть ли смена каталога -D.
func sudoShort(a string) (preserve, valued, dir bool) {
	for j := 1; j < len(a); j++ {
		if a[j] == 'E' {
			preserve = true
		}
		if strings.IndexByte(sudoValued, a[j]) >= 0 {
			return preserve, j+1 == len(a), a[j] == 'D'
		}
	}
	return preserve, false, false
}

var reName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func validName(s string) bool { return reName.MatchString(s) }

// walker обходит скрипт: src — его текст, offs — смещения его байтов в исходной команде
// (nil — src и есть исходная команда).
type walker struct {
	sv     *shellVars
	src    string
	offs   []int
	home   string
	shells map[int]bool
	depth  int
	// quiet — второй проход цикла: присваивания по тексту — с первого (запас в сторону попадания:
	// значение второго прохода бывает неясным). Вызов функции пишет присваивания тела со значениями
	// на момент вызова: по тексту они раньше объявления не видны, после — значение последнего вызова.
	quiet bool

	// locals — по вызову функции: прежние значения переменных, объявленных в ней local
	locals []map[string]prior
	calls  int // глубина вызовов функций: рекурсия
}

// prior — значение переменной до local: ok=false — команда её не трогала.
type prior struct {
	v  envVar
	ok bool
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
		w.loop(env, func(b *shellEnv) {
			w.stmts(c.Cond, b)
			w.stmts(c.Do, b)
		})
	case *syntax.ForClause:
		w.subs(c.Loop, env)
		w.loop(env, func(b *shellEnv) { w.stmts(c.Do, b) })
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
		// тело исполнится при вызове (call); функцию, которую не вызвали, разбор не обходит
		env.funcs[c.Name.Value] = []*syntax.Stmt{c.Body}
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

// loop — тело цикла дважды: второй проход — с окружением, слитым из исходного и после первого
// (присваивание в конце тела действует в начале следующего прохода). После цикла — слитое.
func (w *walker) loop(env *shellEnv, body func(*shellEnv)) {
	b := env.clone()
	body(&b)
	m := mergeEnv(*env, b)
	quiet := w.quiet
	w.quiet = true
	b = m.clone()
	body(&b)
	w.quiet = quiet
	*env = mergeEnv(m, b)
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
	var prefix []string
	for _, a := range c.Assigns {
		if v, ok := w.value(a, pe); ok {
			v.exported = true
			pe.vars[a.Name.Value] = v
			prefix = append(prefix, a.Name.Value)
		}
	}
	lits := env.lits()
	words := make([]wordRec, len(c.Args))
	for i, a := range c.Args {
		val, ok := wordValue(a, w.home, lits)
		words[i] = wordRec{off: w.pos(a.Pos()), val: val, ok: ok, raw: w.text(a)}
	}
	w.sv.calls = append(w.sv.calls, callRec{words: words, env: pe, sh: env.clone()})
	if bodies, ok := env.funcs[words[0].val]; ok && words[0].ok {
		w.callFunc(bodies, env, pe, prefix)
		return
	}
	s := builtinShift(words)
	if s < len(words) && words[s].ok {
		switch words[s].val {
		case "unset":
			unset(words[s+1:], env)
		case "export", "declare", "typeset", "local", "readonly":
			w.declWords(c, s, env)
		case "eval", "source", ".":
			env.blur(nil)
		}
	}
	w.nested(c, words, pe, *env)
}

// builtinShift — индекс слова за `builtin` и `command` (`command -p`): `builtin unset X` — тот же unset.
func builtinShift(words []wordRec) int {
	s := 0
	for s < len(words) && words[s].ok && (words[s].val == "builtin" || words[s].val == "command") {
		s++
		for s < len(words) && words[s].ok && words[s].val == "-p" {
			s++
		}
	}
	return s
}

// unset — `unset [-v] X…` снимает переменные, `unset -f f…` — функции; имя из подстановки неизвестно.
func unset(args []wordRec, env *shellEnv) {
	funcs := false
	for _, wd := range args {
		switch {
		case !wd.ok:
			env.blur(nil)
		case wd.val == "-f":
			funcs = true
		case strings.HasPrefix(wd.val, "-"):
		case funcs:
			delete(env.funcs, wd.val)
		default:
			env.vars[wd.val] = envVar{}
		}
	}
}

// callFunc — вызов функции: её тело исполняется в той же оболочке с префиксами вызова, которые
// после вызова возвращаются к прежним значениям; local внутри — до конца вызова. Функция,
// объявленная лишь на одном пути, — слитое окружение с её телом и без. Рекурсия — окружение неясно.
func (w *walker) callFunc(bodies []*syntax.Stmt, env *shellEnv, pe shellEnv, prefix []string) {
	if w.calls > 4 {
		env.blur(nil)
		return
	}
	var res shellEnv
	for i, body := range bodies {
		e := w.funcBody(body, pe)
		if i == 0 {
			res = e
		} else {
			res = mergeEnv(res, e)
		}
	}
	for _, n := range prefix {
		if v, ok := env.get(n); ok {
			res.vars[n] = v
		} else {
			delete(res.vars, n)
		}
	}
	*env = res
}

func (w *walker) funcBody(body *syntax.Stmt, pe shellEnv) shellEnv {
	e := pe.clone()
	if body == nil { // функцию не объявили: команды нет
		return e
	}
	w.calls++
	w.locals = append(w.locals, map[string]prior{})
	w.stmt(body, &e)
	for n, p := range w.locals[len(w.locals)-1] {
		if p.ok {
			e.vars[n] = p.v
		} else {
			delete(e.vars, n)
		}
	}
	w.locals = w.locals[:len(w.locals)-1]
	w.calls--
	return e
}

// value — присваивание a: значение в окружении env и признак временного каталога. false — не
// присваивание скалярной переменной (массив, индекс, `export X` без значения) или HOME.
func (w *walker) value(a *syntax.Assign, env shellEnv) (envVar, bool) {
	if a.Name == nil || a.Naked || a.Index != nil || a.Array != nil || a.Name.Value == "HOME" {
		return envVar{}, false
	}
	name := a.Name.Value
	lits := env.lits()
	val, ok := "", true
	raw := ""
	if a.Value != nil {
		val, ok = wordValue(a.Value, w.home, lits)
		raw = w.text(a.Value)
	}
	if ok && (val == "~" || strings.HasPrefix(val, "~/")) {
		val, ok = w.home+val[1:], w.home != ""
	}
	temp := false
	if a.Append {
		old, known := lits[name]
		val, ok = old+val, ok && known
		v, _ := env.get(name)
		temp = v.temp && !v.loose
	}
	switch {
	case ok:
		temp = tempDir(val)
	case !a.Append:
		temp = env.tempText(raw)
	}
	return envVar{val: val, lit: ok, temp: temp, set: true}, true
}

// assign — присваивание переменной оболочки; export: 1 — экспортировать, -1 — снять экспорт.
func (w *walker) assign(a *syntax.Assign, env *shellEnv, export int) {
	v, ok := w.value(a, *env)
	if !ok {
		return
	}
	name := a.Name.Value
	if !w.quiet {
		w.sv.list = append(w.sv.list, assign{off: w.pos(a.Pos()), name: name, val: v.val, ok: v.lit})
	}
	old, _ := env.get(name)
	v.exported, v.loose = old.exported, old.loose
	switch export {
	case 1:
		v.exported, v.loose = true, false
	case -1:
		v.exported = false
	}
	env.vars[name] = v
}

// decl — export, declare, typeset, local, readonly: присваивания и флаг экспорта (-x, +x, export -n).
// local, а в функции и declare/typeset без -g, — до конца вызова функции.
func (w *walker) decl(d *syntax.DeclClause, env *shellEnv) {
	for _, a := range d.Args {
		w.subs(a, env)
	}
	export := 0
	if d.Variant.Value == "export" {
		export = 1
	}
	local := d.Variant.Value == "local" || d.Variant.Value == "declare" || d.Variant.Value == "typeset"
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
			local = local && !strings.Contains(f, "g")
			continue
		}
		if local {
			w.local(a.Name.Value, *env)
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

// local запоминает значение name до local в вызове функции (вне функции local не действует).
func (w *walker) local(name string, env shellEnv) {
	if len(w.locals) == 0 {
		return
	}
	top := w.locals[len(w.locals)-1]
	if _, ok := top[name]; !ok {
		v, ok := env.get(name)
		top[name] = prior{v: v, ok: ok}
	}
}

// declWords — export, declare … как слова простой команды (`builtin export X=…`): текст от
// слова s разбирается заново объявлением. Не разобралось — окружение неясно.
func (w *walker) declWords(c *syntax.CallExpr, s int, env *shellEnv) {
	start, end := int(c.Args[s].Pos().Offset()), int(c.Args[len(c.Args)-1].End().Offset())
	src := w.src[start:end]
	f, err := syntax.NewParser().Parse(strings.NewReader(src), "")
	var d *syntax.DeclClause
	if err == nil && len(f.Stmts) == 1 {
		d, _ = f.Stmts[0].Cmd.(*syntax.DeclClause)
	}
	if d == nil {
		env.blur(nil)
		return
	}
	offs := make([]int, len(src)+1)
	for i := range offs {
		offs[i] = w.abs(start + i)
	}
	sw := *w
	sw.src, sw.offs = src, offs
	sw.decl(d, env)
}

// nested — вложенный скрипт `bash -c '…'` (`-lc`, `-euo pipefail -c` …), в том числе за обёрткой
// (`timeout 60 bash -c`): он получает экспортированное окружение вызова env, его присваивания
// наружу не выходят; sh — переменные внешней оболочки для подстановок в двойных кавычках.
func (w *walker) nested(c *syntax.CallExpr, words []wordRec, env, sh shellEnv) {
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
		script, offs, ok := w.scriptWord(c.Args[i+1], sh)
		if !ok {
			return
		}
		nw := &walker{sv: w.sv, home: w.home, shells: w.shells, depth: w.depth + 1, quiet: w.quiet}
		nw.script(script, offs, wrapEnv(env, words[:si]).exported())
		return
	}
}

// scriptWord — текст скрипта в слове после снятия кавычек и подстановок внешней оболочки sh и
// смещения его байтов в исходной команде. Переменная в двойных кавычках подставляется значением,
// если её присвоила команда; иначе остаётся своим текстом (`"… $TMPDIR …"`): значение пришло
// извне, вложенная оболочка получит то же. false — в слове подстановка команды, неизвестное
// значение присвоенной переменной и прочее.
func (w *walker) scriptWord(word *syntax.Word, sh shellEnv) (string, []int, bool) {
	sb := scriptBuf{w: w}
	lits := sh.lits()
	for _, p := range word.Parts {
		switch p := p.(type) {
		case *syntax.Lit:
			sb.emit(p.Value, int(p.Pos().Offset()), anyEscape)
		case *syntax.SglQuoted:
			if p.Dollar {
				return "", nil, false
			}
			sb.emit(p.Value, int(p.Pos().Offset())+1, nil)
		case *syntax.DblQuoted:
			if p.Dollar || !sb.dblQuoted(p.Parts, sh, lits) {
				return "", nil, false
			}
		default:
			return "", nil, false
		}
	}
	sb.offs = append(sb.offs, w.abs(int(word.End().Offset())))
	return string(sb.b), sb.offs, true
}

// scriptBuf — текст вложенного скрипта и смещения его байтов в исходной команде.
type scriptBuf struct {
	w    *walker
	b    []byte
	offs []int
}

func anyEscape(byte) bool { return true }

func dqEscape(c byte) bool { return strings.IndexByte("\"\\$`\n", c) >= 0 }

// emit — текст s со смещения start скрипта; unescape — какие символы после \ снимают его.
func (sb *scriptBuf) emit(s string, start int, unescape func(next byte) bool) {
	for i := 0; i < len(s); i++ {
		if unescape != nil && s[i] == '\\' && i+1 < len(s) && unescape(s[i+1]) {
			i++
			if s[i] == '\n' {
				continue
			}
		}
		sb.b = append(sb.b, s[i])
		sb.offs = append(sb.offs, sb.w.abs(start+i))
	}
}

// dblQuoted — части в двойных кавычках; false — в них подстановка, которую не раскрыть.
func (sb *scriptBuf) dblQuoted(parts []syntax.WordPart, sh shellEnv, lits map[string]string) bool {
	for _, q := range parts {
		switch q := q.(type) {
		case *syntax.Lit:
			sb.emit(q.Value, int(q.Pos().Offset()), dqEscape)
		case *syntax.ParamExp:
			v, ok := sb.w.dqParam(q, sh, lits)
			if !ok {
				return false
			}
			// значение — не текст скрипта: все его байты — на месте подстановки
			sb.b = append(sb.b, v...)
			sb.offs = append(sb.offs, slices.Repeat([]int{sb.w.abs(int(q.Pos().Offset()))}, len(v))...)
		default:
			return false
		}
	}
	return true
}

// dqParam — подстановка внешней оболочки sh в двойных кавычках вложенного скрипта: значение
// переменной, присвоенной командой, или текст подстановки, если её значение пришло извне.
func (w *walker) dqParam(q *syntax.ParamExp, sh shellEnv, lits map[string]string) (string, bool) {
	if _, set := sh.get(paramName(q)); !set {
		return w.text(q), true
	}
	return paramValue(q, w.home, lits)
}

func paramName(p *syntax.ParamExp) string {
	if p.Param == nil {
		return ""
	}
	return p.Param.Value
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
