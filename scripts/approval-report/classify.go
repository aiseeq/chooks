package main

// Классификатор действий «по да» (тикет subagent-approval-actions-report).
//
// Команды оболочки разбирает github.com/aiseeq/akit/shellcmd (mvdan.cc/sh): он снимает обёртки
// (`bash -c`, `graft exec --`, `timeout`, `sudo`), подставляет переменные на место имени программы
// (`G="git --git-dir=…"; $G commit`) и отдаёт перенаправления с телами heredoc. Переменные в
// аргументах и целях перенаправлений подставляет vars.go: литеральные значения присваиваний
// раньше в той же команде (`F=~/.claude/x; sub a b "$F"`).
// Ограничения разбора:
//   - Цель записи берётся из слов команды: путь в переменной из окружения или с подстановкой
//     команды (`D=$(mktemp -d)`) и записи из кода python/perl/node не видны. Записи из кода
//     выдаются отдельно как «возможные» (эвристика по тексту скрипта или путь класса 1 в
//     аргументах скрипта рядом со словом правки) и в числа классов не входят. Цели `sub -F файл`
//     берутся из вывода sub в транскрипте («sub: N replacements in M files: …», ResolveSpec);
//     вывод неполный (`| cut -c1-80`) — из файла спецификации, если он ещё есть (ResolveSpecFile).
//   - Команда, которая присваивает HOME (`export HOME=$W/h` в пробе), делает `$HOME` и `~`
//     неизвестными: запись туда не считается.
//   - Каталог для относительного пути — cwd записи транскрипта (у бойца — dir задачи) плюс
//     литеральные `cd` в той же команде по порядку; `cd` в переменную обнуляет каталог.
//   - Не разобранную оболочкой команду (синтаксическая ошибка) проверяют регулярки по сырому
//     тексту: только явные формы (`> ~/.claude/…`, `tee`, `lex build`, `cfg commit`,
//     `--git-dir=….claude-config.git … commit|push`, `graft deploy …`); такие попадания
//     помечены «(regex)».
//   - Класс 3: prod — цель, в имени которой есть «prod»; цель в переменной — «?». Выкатка с
//     временным XDG_CONFIG_HOME (`$TMPDIR`, `/tmp`, mktemp, scratchpad) в окружении вызова
//     (vars.go: префикс, env, export в той же или внешней оболочке) — проба: в число класса не
//     входит, печатается отдельной строкой. GIT_DIR класса 2 — из того же окружения.
//   - Класс 4: метод curl — `-X/--request`, иначе POST при `-d/--data*/-F/--json`, PUT при `-T`;
//     поиск через POST (`/search`, `/jql`, `bulkfetch`) — чтение. «Своя задача» — ключ задачи
//     из URL встречается в задании агента (первое сообщение суб-агента, brief*.md бойца).

import (
	"maps"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/aiseeq/akit/shellcmd"
)

// Event — один вызов инструмента агента.
type Event struct {
	Tool    string // Bash, Edit, Write, MultiEdit, NotebookEdit; у бойца — command_execution, file_change
	Command string // текст команды оболочки
	Paths   []string
	Cwd     string // у бойца — каталог вызова из rollout codex (CallCwd), иначе dir задачи
	Home    string
	CallCwd bool // у бойца: Cwd взят из rollout codex, а не из dir задачи
}

// cwdRecorded — Cwd события и есть каталог вызова: у Claude он в транскрипте, у бойца — в rollout
// codex (att*.jsonl его не пишет, dir задачи — не каталог вызова: 1113-1209-step5-plan шёл и из
// живого каталога).
func (ev Event) cwdRecorded() bool { return ev.Tool != codexCommand || ev.CallCwd }

// codexCommand — вызов оболочки бойца codex в его журнале.
const codexCommand = "command_execution"

// Hit — найденное действие класса 1–4.
type Hit struct {
	Class  int
	Verb   string // чем сделано: Edit, >, sub, lex build, git commit, deploy, POST …
	Target string // путь, цель выкатки или метод+хост+путь без query
	Prod   string // класс 3: "prod", "не-prod", "?"
	Kind   string // класс 4: comment, transition, other, bitbucket
	Key    string // класс 4: ключ задачи Jira из URL
}

// Verdict — итог разбора события.
type Verdict struct {
	Hits      []Hit
	Mentioned [5]bool // кандидат класса: в тексте есть признак класса (путь, cfg, graft deploy, curl+atlassian)
	// Possible — вне чисел классов: эвристика по коду скрипта (класс 1, 4) и выкатка в пробе
	// с временным конфигом graft (класс 3).
	Possible     []Hit
	Unparsed     bool     // оболочка не разобрала команду: работали регулярки
	VarsUnparsed bool     // вложенный скрипт не разобран: переменные из него не подставлены
	SpecFile     bool     // `sub -F файл`: цели в файле спецификации, ждут вывода sub (ResolveSpec)
	Spec         *SpecRef // файл спецификации `sub -F`
}

// SpecRef — файл спецификации `sub -F`: путь ("" — в неизвестной переменной) и каталог вызова
// для относительных путей в нём.
type SpecRef struct{ Path, Cwd string }

// Unhit — возможные записи вне классов, в которые событие уже засчитано.
func (v Verdict) Unhit() []Hit {
	var out []Hit
	for _, h := range v.Possible {
		if !v.HitClass(h.Class) {
			out = append(out, h)
		}
	}
	return out
}

// HitClass сообщает, есть ли попадание класса.
func (v Verdict) HitClass(c int) bool {
	for _, h := range v.Hits {
		if h.Class == c {
			return true
		}
	}
	return false
}

var (
	reMention1  = regexp.MustCompile(`\.claude/(CLAUDE\.md|decisions\.md|skills\b|rules\b|agents\b|codex/(frag|manifest)\b|settings[\w.-]*\.json)|\blex\s+(build|absorb|import)\b`)
	reMention2  = regexp.MustCompile(`claude-config\.git|\bcfg\s+(commit|push|add|status|diff|log|show|rm|reset|restore|checkout)\b`)
	reMention3  = regexp.MustCompile(`\bgraft\s+(?:-C\s+\S+\s+)*deploy\b`)
	reCurl      = regexp.MustCompile(`\bcurl\b`)
	reAtl       = regexp.MustCompile(`(?i)atlassian|bitbucket`)
	reIssueKey  = regexp.MustCompile(`\b[A-Z][A-Z0-9]+-\d+\b`)
	reGitDirEnv = regexp.MustCompile(`GIT_DIR=(\S+)`)
	reDynGit    = regexp.MustCompile(`claude-config\.git.*\b(commit|push)\b`)
)

// Classify разбирает событие.
func Classify(ev Event) Verdict {
	var v Verdict
	switch ev.Tool {
	case "Edit", "Write", "MultiEdit", "NotebookEdit", "file_change":
		for _, p := range ev.Paths {
			if reMention1.MatchString(p) {
				v.Mentioned[1] = true
			}
			if np, ok := normPath(p, ev.Home, ev.Cwd); ok && protected(np, ev.Home) {
				v.Hits = append(v.Hits, Hit{Class: 1, Verb: ev.Tool, Target: np})
			}
		}
	case "Bash", codexCommand:
		classifyShell(ev, &v)
	}
	for _, h := range v.Hits {
		v.Mentioned[h.Class] = true
	}
	return v
}

func classifyShell(ev Event, v *Verdict) {
	cmd := ev.Command
	v.Mentioned[1] = reMention1.MatchString(cmd)
	v.Mentioned[2] = reMention2.MatchString(cmd)
	v.Mentioned[3] = reMention3.MatchString(cmd)
	v.Mentioned[4] = reCurl.MatchString(cmd) && reAtl.MatchString(cmd)
	if !v.Mentioned[1] && !v.Mentioned[2] && !v.Mentioned[3] && !v.Mentioned[4] && !mayWriteRelative(ev) {
		return
	}
	calls, err := shellcmd.Calls(cmd)
	if calls == nil && err != nil {
		v.Unparsed = true
		regexFallback(ev, v)
		return
	}
	vars, err := collectVars(cmd, homeVar(ev), calls)
	v.VarsUnparsed = err != nil
	start := dirMark{path: ev.Cwd, temp: ev.cwdRecorded() && ev.Cwd != "" && tempDir(ev.Cwd)}
	w := shellWalk{ev: ev, v: v, home: homeVar(ev), cwd: ev.Cwd, dirs: dirState{cur: start, allTemp: start.temp}, vars: vars}
	for _, c := range calls {
		w.call(c)
	}
}

// shellWalk — состояние разбора одной команды: каталог меняется по ходу вызовов.
type shellWalk struct {
	ev   Event
	v    *Verdict
	home string
	cwd  string
	dirs dirState
	vars shellVars
}

func (w *shellWalk) call(c shellcmd.Call) {
	vars := w.vars.at(c.Offset)
	args := make([]string, len(c.Args))
	for i, a := range c.Args {
		args[i] = expandVars(unquote(a), vars)
	}
	if w.track(c, args) {
		return
	}
	x := callCtx{ev: w.ev, home: w.home, cwd: w.cwd, dirs: w.dirs, v: w.v, vars: vars, sv: &w.vars}
	x.redirects(c)
	if h, ok := callHandlers[c.Name]; ok {
		h(x, c, args)
	}
	if strings.HasPrefix(c.Name, "python") || c.Name == "node" {
		x.scriptWrites(c, args)
	}
}

// track ведёт cd/pushd/popd (true: больше вызов ничего не делает).
func (w *shellWalk) track(c shellcmd.Call, args []string) bool {
	switch c.Name {
	case "cd", "pushd":
		// cd во вложенном скрипте (`bash -c 'cd …'`) действует только в нём
		if len(c.Via) > 0 {
			w.dirs.restorable = true
		}
		w.cd(c, args)
		return true
	case "popd":
		w.dirs.restorable = true
		w.dirs.move(w.dirs.prev)
		w.cwd = w.dirs.cur.path
		return true
	}
	return false
}

func (w *shellWalk) cd(c shellcmd.Call, args []string) {
	switch {
	case len(args) == 0:
		w.dirs.move(dirMark{path: w.home})
	case args[0] == "-":
		// OLDPWD — прошлый каталог: модель повторяет его точно
		w.dirs.move(w.dirs.prev)
	default:
		np, ok := normPath(args[0], w.home, w.cwd)
		if !ok {
			w.dirs.move(dirMark{temp: tempArg(args[0], w.vars, c.Offset)})
			break
		}
		w.dirs.move(dirMark{path: np, temp: tempDir(np)})
	}
	w.cwd = w.dirs.cur.path
}

// dirMark — каталог, в котором может идти вызов: path "" — неизвестен; temp — временный.
type dirMark struct {
	path string
	temp bool
}

// dirState — каталоги команды до вызова. Через popd (стек pushd не ведётся), конец подоболочки
// или вложенного скрипта команда может вернуться в каталог, которого модель не знает (restorable): тогда выкатка — проба, только если
// временные все каталоги, где команда побывала (allTemp).
type dirState struct {
	cur, prev  dirMark
	allTemp    bool
	restorable bool
}

func (d *dirState) move(to dirMark) {
	d.prev, d.cur = d.cur, to
	d.allTemp = d.allTemp && to.temp
}

type callHandler func(x callCtx, c shellcmd.Call, args []string)

func inPlaceCall(x callCtx, c shellcmd.Call, args []string) { x.inPlace(c.Name, args) }
func copyCall(x callCtx, c shellcmd.Call, args []string)    { x.copyDest(c.Name, args) }
func removeCall(x callCtx, c shellcmd.Call, args []string) {
	x.writeTargets(c.Name, positional(args, map[string]bool{"-s": true, "--size": true}))
}

// callHandlers — что ищется в вызове программы по её имени; "" — имя из подстановки.
var callHandlers = map[string]callHandler{
	"tee":      func(x callCtx, _ shellcmd.Call, args []string) { x.writeTargets("tee", positional(args, nil)) },
	"sub":      func(x callCtx, c shellcmd.Call, args []string) { x.sub(c, args) },
	"sed":      inPlaceCall,
	"perl":     inPlaceCall,
	"cp":       copyCall,
	"mv":       copyCall,
	"install":  copyCall,
	"ln":       copyCall,
	"rsync":    copyCall,
	"rm":       removeCall,
	"truncate": removeCall,
	"unlink":   removeCall,
	"dd":       func(x callCtx, _ shellcmd.Call, args []string) { x.dd(args) },
	"lex":      func(x callCtx, _ shellcmd.Call, args []string) { x.lex(args) },
	"git":      func(x callCtx, c shellcmd.Call, args []string) { x.gitCall(c, args) },
	"cfg":      func(x callCtx, _ shellcmd.Call, args []string) { x.cfg(args) },
	"graft":    func(x callCtx, c shellcmd.Call, args []string) { x.graft(c, args) },
	"curl":     func(x callCtx, _ shellcmd.Call, args []string) { x.curl(args, x.ev.Command) },
	"":         func(x callCtx, c shellcmd.Call, _ []string) { x.dynamic(c) },
}

func (x callCtx) dd(args []string) {
	for _, a := range args {
		if p, ok := strings.CutPrefix(a, "of="); ok {
			x.writeTargets("dd", []string{p})
		}
	}
}

// gitCall — git с GIT_DIR окружения вызова (export или префикс, в том числе через env). Неясный
// GIT_DIR (присвоен в одной ветке) берётся своим значением: запас в сторону попадания. Вызов,
// которого нет в разборе окружения, — по регулярке у префикса.
func (x callCtx) gitCall(c shellcmd.Call, args []string) {
	gd := ""
	if env, ok := x.sv.callEnv(c.Offset); ok {
		if v, _ := env.get("GIT_DIR"); v.exported {
			gd = v.val // не литерал — "", normPath его отбросит
		}
	} else if m := reGitDirEnv.FindStringSubmatch(assignPrefix(x.ev.Command, c.Offset) + " " + c.Raw); m != nil {
		gd = unquote(m[1])
	}
	x.git(args, gd)
}

func (x callCtx) cfg(args []string) {
	if len(args) > 0 && (args[0] == "commit" || args[0] == "push") {
		x.v.Hits = append(x.v.Hits, Hit{Class: 2, Verb: "cfg " + args[0], Target: "~/.claude-config.git"})
	}
}

func (x callCtx) graft(c shellcmd.Call, args []string) {
	dirs := x.dirs
	for len(args) > 1 && args[0] == "-C" {
		np, ok := normPath(args[1], x.home, dirs.cur.path)
		if ok {
			dirs.move(dirMark{path: np, temp: tempDir(np)})
		} else {
			dirs.move(dirMark{temp: tempArg(args[1], *x.sv, c.Offset)})
		}
		args = args[2:]
	}
	if len(args) == 0 || args[0] != "deploy" {
		return
	}
	h, ok := graftDeploy(args[1:])
	if !ok {
		return
	}
	if x.tempConfig(c) {
		if note, probe := probeDir(dirs, x.ev.Command, c.Offset); probe {
			h.Prod = "проба: " + note
			x.v.Possible = append(x.v.Possible, h)
			return
		}
	}
	x.v.Hits = append(x.v.Hits, h)
}

// tempConfig — у вызова временный XDG_CONFIG_HOME в окружении (vars.go, callEnv): префикс, env
// или export в той же оболочке или во внешней раньше вызова. Неясное окружение (ветка, sudo без
// -E, вызов не найден в разборе) — не проба.
func (x callCtx) tempConfig(c shellcmd.Call) bool {
	env, ok := x.sv.callEnv(c.Offset)
	if !ok {
		return false
	}
	v, _ := env.get("XDG_CONFIG_HOME")
	return v.exported && v.temp && !v.unclear
}

// probeDir — проба ли выкатка с временным конфигом по каталогу вызова: только из временного
// каталога. Живой или неизвестный каталог с временным конфигом — выкатка: выкатывается проект
// каталога. Подоболочка до вызова (скобка, кроме `$(` и `<(`) может вернуть прежний каталог.
func probeDir(d dirState, cmd string, off int) (string, bool) {
	if !d.cur.temp {
		return "", false
	}
	if (d.restorable || reSubshell.MatchString(cmd[:off])) && !d.allTemp {
		return "", false
	}
	if d.cur.path == "" {
		return "каталог из mktemp или $TMPDIR", true
	}
	return "каталог " + d.cur.path, true
}

// reSubshell — открывающая скобка подоболочки или группы `( … )`, не подстановка.
var reSubshell = regexp.MustCompile(`(^|[^$<>=])\(`)

// tempArg — неразобранный путь cd во временном каталоге: `$(mktemp …)`, `$TMPDIR/…` или
// переменная, последнее присваивание которой перед вызовом (off) — из них.
func tempArg(arg string, sv shellVars, off int) bool {
	arg = unquote(arg)
	if reTempValue.MatchString(arg) {
		return true
	}
	m := reLeadVar.FindStringSubmatch(arg)
	return m != nil && sv.tempVar(m[1], off)
}

// tempValue — начало значения во временном каталоге.
const tempValue = `(\$\{?TMPDIR\b|\$\(mktemp|/tmp/|/var/tmp/|[^\s;&|]*(claude-tmp|scratchpad)/)`

var (
	reTempValue = regexp.MustCompile(`^` + tempValue)
	reLeadVar   = regexp.MustCompile(`^\$\{?([A-Za-z_][A-Za-z0-9_]*)`)
)

// tempDir — каталог во временном: /tmp, /var/tmp, claude-tmp, scratchpad.
func tempDir(p string) bool {
	p += "/"
	return strings.HasPrefix(p, "/tmp/") || strings.HasPrefix(p, "/var/tmp/") || strings.Contains(p, "/claude-tmp/") || strings.Contains(p, "/scratchpad/")
}

func (x callCtx) dynamic(c shellcmd.Call) {
	if reDynGit.MatchString(c.Raw) {
		x.v.Hits = append(x.v.Hits, Hit{Class: 2, Verb: "git (имя из подстановки)", Target: "~/.claude-config.git"})
	}
}

// mayWriteRelative: относительная запись внутри ~/.claude возможна с cwd там же или после cd туда;
// `sub -F файл` пишет по путям из спецификации, их видно только в выводе.
func mayWriteRelative(ev Event) bool {
	return strings.HasPrefix(ev.Cwd+"/", ev.Home+"/.claude/") || strings.Contains(ev.Command, ".claude") || strings.Contains(ev.Command, "sub ")
}

var (
	reSubDone = regexp.MustCompile(`(?m)^sub: \d+ replacements? in (\d+) files?: (.*)$`)
	reSubFile = regexp.MustCompile(`(?:^|, )(\S+) \d+ \(lines? `)
	reSubMsg  = regexp.MustCompile(`(?m)^sub: `)
)

// ResolveSpec дополняет событие `sub -F файл` путями из вывода sub
// («sub: 9 replacements in 8 files: /путь 1 (line 4), …»; относительный путь — от cwd события).
// Другая строка «sub: …» (несовпадение, ошибка) значит, что sub ничего не записал.
// Без строк «sub: » (вывод не дошёл) или с неполным списком файлов (вывод обрезан `cut`)
// цели остаются невидимыми: SpecFile остаётся, их ищет ResolveSpecFile.
func (v *Verdict) ResolveSpec(out, home, cwd string) {
	if !v.SpecFile || !reSubMsg.MatchString(out) {
		return
	}
	complete := true
	for _, d := range reSubDone.FindAllStringSubmatch(out, -1) {
		files := reSubFile.FindAllStringSubmatch(d[2], -1)
		if n, err := strconv.Atoi(d[1]); err != nil || len(files) < n {
			complete = false
		}
		for _, m := range files {
			v.addSpecHit("sub -F (по выводу)", m[1], home, cwd)
		}
	}
	v.SpecFile = !complete
}

// specBodyVerb — цель `sub -F файл` из нынешнего содержимого файла спецификации.
const specBodyVerb = "sub -F (по файлу спецификации)"

// ResolveSpecFile — цели `sub -F файл` из файла спецификации, когда вывод их не показал. Файл
// читается при разборе отчёта, а не в момент события: он мог измениться после вызова.
func (v *Verdict) ResolveSpecFile(body, home string) {
	if !v.SpecFile {
		return
	}
	v.SpecFile = false
	for _, p := range subSpecPaths(body) {
		v.addSpecHit(specBodyVerb, p, home, v.Spec.Cwd)
	}
}

func (v *Verdict) addSpecHit(verb, p, home, cwd string) {
	np, ok := normPath(p, home, cwd)
	if !ok || !protected(np, home) {
		return
	}
	for _, h := range v.Hits {
		if h.Class == 1 && h.Target == np {
			return
		}
	}
	v.Hits = append(v.Hits, Hit{Class: 1, Verb: verb, Target: np})
	v.Mentioned[1] = true
}

// assignPrefix — текст простой команды перед именем программы: там стоят присваивания `GIT_DIR=…`.
func assignPrefix(cmd string, off int) string {
	if off <= 0 || off > len(cmd) {
		return ""
	}
	pre := cmd[:off]
	if i := strings.LastIndexAny(pre, "\n;&|("); i >= 0 {
		pre = pre[i+1:]
	}
	return pre
}

type callCtx struct {
	ev   Event
	home string // значение $HOME в команде, "" — переприсвоено
	cwd  string
	dirs dirState
	v    *Verdict
	vars map[string]string // переменные, присвоенные раньше в команде
	sv   *shellVars        // присваивания и окружения вызовов команды
}

var reHomeAssign = regexp.MustCompile(`(^|[\s;&|(])(export\s+)?HOME=`)

// homeVar — $HOME команды: присваивание HOME= в ней (временный дом теста) делает его неизвестным.
func homeVar(ev Event) string {
	if reHomeAssign.MatchString(ev.Command) {
		return ""
	}
	return ev.Home
}

func (x callCtx) writeTargets(verb string, paths []string) {
	for _, p := range paths {
		np, ok := normPath(p, x.home, x.cwd)
		if ok && protected(np, x.ev.Home) {
			x.v.Hits = append(x.v.Hits, Hit{Class: 1, Verb: verb, Target: np})
		}
	}
}

func (x callCtx) redirects(c shellcmd.Call) {
	for _, r := range c.Redirs {
		switch r.Op {
		case ">", ">>", ">|", "&>", "&>>", ">&":
			t := expandVars(unquote(r.Target), x.vars)
			if t == "-" || t == "" || strings.Trim(t, "0123456789") == "" {
				continue
			}
			x.writeTargets(r.Op, []string{t})
		}
	}
}

// positional — слова без флагов; valued — флаги со значением отдельным словом.
func positional(args []string, valued map[string]bool) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			return append(out, args[i+1:]...)
		}
		if strings.HasPrefix(a, "-") && a != "-" {
			if valued[a] {
				i++
			}
			continue
		}
		out = append(out, a)
	}
	return out
}

func (x callCtx) sub(c shellcmd.Call, args []string) {
	spec, dry := "", false
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			pos = append(pos, args[i+1:]...)
			i = len(args)
		case a == "--dry-run":
			dry = true
		case a == "-n":
			i++
		case a == "-F":
			if i+1 < len(args) {
				spec = args[i+1]
				i++
			}
		case strings.HasPrefix(a, "-F"):
			spec = a[2:]
		case strings.HasPrefix(a, "-") && len(pos) == 0:
		default:
			pos = append(pos, a)
		}
	}
	if dry {
		return
	}
	switch {
	case spec == "" && len(pos) >= 2:
		x.writeTargets("sub", pos[2:])
	case len(pos) == 1:
		x.writeTargets("sub", pos)
	case spec == "" || spec == "-":
		for _, r := range c.Redirs {
			if r.Op == "<<" || r.Op == "<<-" {
				x.writeTargets("sub", subSpecPaths(x.heredocBody(r)))
			}
			if r.Op == "<<<" {
				x.writeTargets("sub", subSpecPaths(expandVars(unquote(r.Target), x.vars)))
			}
		}
	default:
		x.specFile(spec)
	}
}

// specFile — `sub -F файл`: цели ждут вывода sub или самого файла.
func (x callCtx) specFile(spec string) {
	x.v.SpecFile = true
	x.v.Spec = &SpecRef{Cwd: x.cwd}
	if np, ok := normPath(spec, x.home, x.cwd); ok {
		x.v.Spec.Path = np
	}
}

// heredocBody — тело heredoc; ограничитель без кавычек (видно по строке с `<<` в исходной
// команде) — с подстановкой известных переменных. Тела нет в исходном тексте — без подстановки.
func (x callCtx) heredocBody(r shellcmd.Redir) string {
	cmd := x.ev.Command
	if r.BodyStart <= 0 || r.BodyStart > len(cmd) {
		return r.Body
	}
	head := cmd[:r.BodyStart-1]
	if i := strings.LastIndex(head, "\n"); i >= 0 {
		head = head[i+1:]
	}
	i := strings.LastIndex(head, "<<")
	if i < 0 {
		return r.Body
	}
	if d := strings.TrimLeft(head[i+2:], "- \t"); d == "" || strings.ContainsAny(d[:1], `'"\`) {
		return r.Body
	}
	return expandVars(r.Body, x.vars)
}

// subSpecPaths — строки путей спецификации sub: непустые строки вне блоков SEARCH/REPLACE.
// Без маркеров SEARCH (старая форма «путь, OLD, NEW» с разделителем «=») путь — только первая строка.
func subSpecPaths(body string) []string {
	if !strings.Contains(body, "<<<<<<< SEARCH") {
		for line := range strings.SplitSeq(body, "\n") {
			if t := strings.TrimSpace(line); t != "" {
				return []string{t}
			}
		}
		return nil
	}
	var out []string
	in := false
	for line := range strings.SplitSeq(body, "\n") {
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, "<<<<<<< SEARCH"):
			in = true
		case t == ">>>>>>> REPLACE":
			in = false
		case !in && t != "" && t != "=======":
			out = append(out, t)
		}
	}
	return out
}

func (x callCtx) inPlace(name string, args []string) {
	inplace, script := false, false
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		f := sedArg(name, a)
		inplace = inplace || f.inplace
		script = script || f.script
		if f.skip {
			i++
		}
		if f.pos {
			pos = append(pos, a)
		}
	}
	if !inplace {
		return
	}
	if !script && len(pos) > 0 {
		pos = pos[1:]
	}
	x.writeTargets(name+" -i", pos)
}

// sedFlag — одно слово команды sed/perl: правка на месте (-i), скрипт (-e/-f; skip — значение
// следующим словом) или позиционное слово.
type sedFlag struct{ inplace, script, skip, pos bool }

func sedArg(name, a string) sedFlag {
	switch {
	case a == "--in-place" || strings.HasPrefix(a, "--in-place="):
		return sedFlag{inplace: true}
	case a == "--expression" || a == "--file":
		return sedFlag{script: true, skip: true}
	case strings.HasPrefix(a, "--"):
		return sedFlag{}
	case strings.HasPrefix(a, "-") && len(a) > 1:
		return sedShort(name, a)
	}
	return sedFlag{pos: true}
}

// sedShort разбирает группу коротких флагов: первый из -i или -e/-f решает исход.
func sedShort(name, a string) sedFlag {
	for j := 1; j < len(a); j++ {
		ch := a[j]
		if ch == 'i' {
			return sedFlag{inplace: true}
		}
		if ch == 'e' || ch == 'f' || (name == "perl" && (ch == 'E' || ch == 'M')) {
			return sedFlag{script: true, skip: j == len(a)-1}
		}
	}
	return sedFlag{}
}

func (x callCtx) copyDest(name string, args []string) {
	for i, a := range args {
		if (a == "-t" || a == "--target-directory") && i+1 < len(args) {
			x.writeTargets(name, []string{args[i+1]})
			return
		}
		if d, ok := strings.CutPrefix(a, "--target-directory="); ok {
			x.writeTargets(name, []string{d})
			return
		}
	}
	pos := positional(args, map[string]bool{"-S": true, "--suffix": true, "-m": true, "--mode": true, "-o": true, "-g": true, "-e": true, "--exclude": true, "--include": true, "-f": false})
	if len(pos) < 2 {
		return
	}
	dest := pos[len(pos)-1]
	if name == "rsync" && strings.Contains(dest, ":") {
		return
	}
	x.writeTargets(name, []string{dest})
}

// lexWrites — подкоманды lex, которые пишут в кодекс или собранные файлы.
var lexWrites = map[string]bool{"build": true, "absorb": true, "import": true, "quotes-to-basis": true, "code-confirm": true, "override-confirm": true, "code-debt": true}

func (x callCtx) lex(args []string) {
	if len(args) == 0 || !lexWrites[args[0]] {
		return
	}
	sub := args[0]
	o := parseLex(args[1:])
	if o.check || (sub == "quotes-to-basis" && !o.apply) || (sub == "code-debt" && !o.drop) {
		return
	}
	np, ok := normPath(o.codex, x.home, x.cwd)
	if !ok || np != x.ev.Home+"/.claude/codex" {
		return
	}
	target := "все файлы манифестов"
	if len(o.pos) > 0 {
		target = o.pos[0]
		if p, ok := normPath(o.pos[0], x.home, x.cwd); ok {
			target = p
		}
	}
	x.v.Hits = append(x.v.Hits, Hit{Class: 1, Verb: "lex " + sub, Target: target})
}

// lexOpts — флаги lex после подкоманды.
type lexOpts struct {
	codex              string
	check, apply, drop bool
	pos                []string
}

func parseLex(args []string) lexOpts {
	var o lexOpts
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--codex" && i+1 < len(args):
			o.codex = args[i+1]
			i++
		case strings.HasPrefix(a, "--codex="):
			o.codex = strings.TrimPrefix(a, "--codex=")
		case a == "--check", a == "-h", a == "--help":
			o.check = true
		case a == "--apply":
			o.apply = true
		case a == "--drop":
			o.drop = true
			i++
		case a == "--report" || a == "--outputs-under" || a == "--tree":
			i++
		case strings.HasPrefix(a, "-"):
		default:
			o.pos = append(o.pos, a)
		}
	}
	return o
}

func (x callCtx) git(args []string, gitDir string) {
	sub := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--git-dir" || a == "-C":
			if i+1 < len(args) {
				gitDir = args[i+1]
				i++
			}
		case strings.HasPrefix(a, "--git-dir="):
			gitDir = strings.TrimPrefix(a, "--git-dir=")
		case a == "--work-tree" || a == "-c" || a == "--namespace" || a == "--exec-path":
			i++
		case strings.HasPrefix(a, "-"):
		default:
			sub = a
			i = len(args)
		}
	}
	if sub != "commit" && sub != "push" {
		return
	}
	np, ok := normPath(gitDir, x.home, x.cwd)
	if !ok || np != x.ev.Home+"/.claude-config.git" {
		return
	}
	x.v.Hits = append(x.v.Hits, Hit{Class: 2, Verb: "git " + sub, Target: np})
}

var deployRead = map[string]bool{"status": true, "logs": true, "check": true, "versions": true, "cutoff": true, "check-head": true, "check-integrations": true}

// graftDeploy разбирает слова после `graft deploy`.
func graftDeploy(args []string) (Hit, bool) {
	valued := map[string]bool{"past-freeze": true, "glint-ticket": true, "reason": true, "lines": true, "grep": true}
	var pos []string
	apply := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			break
		}
		if strings.HasPrefix(a, "-") {
			name := strings.TrimLeft(a, "-")
			if name == "h" || name == "help" {
				return Hit{}, false
			}
			if name == "apply" {
				apply = true
			}
			if valued[name] {
				i++
			}
			continue
		}
		pos = append(pos, a)
	}
	if len(pos) == 0 || deployRead[pos[0]] {
		return Hit{}, false
	}
	target := func(i int) string {
		if i < len(pos) {
			return pos[i]
		}
		return ""
	}
	h := Hit{Class: 3}
	switch pos[0] {
	case "freeze":
		if len(pos) < 3 {
			return Hit{}, false
		}
		h.Verb, h.Target = "freeze", target(1)
	case "thaw", "handover":
		h.Verb, h.Target = pos[0], target(1)
	case "sweep":
		if !apply {
			return Hit{}, false
		}
		h.Verb, h.Target = "sweep --apply", target(1)
	default:
		h.Verb, h.Target = "deploy", pos[0]
	}
	switch {
	case h.Target == "" || strings.ContainsAny(h.Target, "$`"):
		h.Prod = "?"
	case strings.Contains(strings.ToLower(h.Target), "prod"):
		h.Prod = "prod"
	default:
		h.Prod = "не-prod"
	}
	return h, true
}

var (
	reJiraRead  = regexp.MustCompile(`/search\b|/jql\b|/bulkfetch\b|/expression/eval\b|/picker\b`)
	reComment   = regexp.MustCompile(`/issue/([A-Z][A-Z0-9]+-\d+)/comment`)
	reTrans     = regexp.MustCompile(`/issue/([A-Z][A-Z0-9]+-\d+)/transitions`)
	reIssuePath = regexp.MustCompile(`/issue/([A-Z][A-Z0-9]+-\d+)`)
)

func (x callCtx) curl(args []string, raw string) {
	method, urls := parseCurl(args)
	if method == "" || method == "GET" || method == "HEAD" || method == "OPTIONS" {
		return
	}
	for _, u := range urls {
		if h, ok := curlHit(method, u, raw); ok {
			x.v.Hits = append(x.v.Hits, h)
		}
	}
}

// curlValued — флаги curl со значением отдельным словом (значение — не URL).
var curlValued = map[string]bool{"-H": true, "--header": true, "-u": true, "--user": true, "-o": true, "--output": true, "-K": true, "--config": true, "-A": true, "-e": true, "-b": true, "-c": true, "-w": true, "--write-out": true, "-m": true, "--max-time": true, "--connect-timeout": true, "--retry": true, "--cacert": true, "--cert": true, "-x": true, "--proxy": true}

// parseCurl — метод запроса (явный или по флагам данных) и слова, похожие на URL.
func parseCurl(args []string) (method string, urls []string) {
	implied := ""
	for i := 0; i < len(args); i++ {
		a, next := args[i], ""
		if i+1 < len(args) {
			next = args[i+1]
		}
		if m, n, ok := curlMethod(a, next); ok {
			method, i = m, i+n
			continue
		}
		if m, n, ok := curlImplied(a); ok {
			if implied == "" || m != "POST" {
				implied = m
			}
			i += n
			continue
		}
		switch {
		case a == "--url" && next != "":
			urls = append(urls, next)
			i++
		case curlValued[a]:
			i++
		case strings.HasPrefix(a, "-"):
		case strings.Contains(a, "://") || reAtl.MatchString(a) || strings.Contains(a, "/rest/"):
			urls = append(urls, a)
		}
	}
	if method == "" {
		method = implied
	}
	return method, urls
}

// curlMethod — явный метод: `-X M`, `-XM`, `--request M`, `--request=M`; n — сколько слов ещё занято.
func curlMethod(a, next string) (method string, n int, ok bool) {
	switch {
	case (a == "-X" || a == "--request") && next != "":
		return strings.ToUpper(next), 1, true
	case strings.HasPrefix(a, "-X") && len(a) > 2:
		return strings.ToUpper(a[2:]), 0, true
	case strings.HasPrefix(a, "--request="):
		return strings.ToUpper(strings.TrimPrefix(a, "--request=")), 0, true
	}
	return "", 0, false
}

// curlImplied — метод по флагу данных: POST у -d/--data*/-F/--json, PUT у -T, GET у -G.
func curlImplied(a string) (method string, n int, ok bool) {
	switch {
	case a == "-d" || strings.HasPrefix(a, "--data") || a == "-F" || a == "--form" || a == "--json":
		return "POST", 1, true
	case a == "-T" || a == "--upload-file":
		return "PUT", 1, true
	case a == "-G" || a == "--get":
		return "GET", 0, true
	}
	return "", 0, false
}

// curlHit — запись на atlassian или bitbucket; хост в переменной — «?», если atlassian есть в команде.
func curlHit(method, u, raw string) (Hit, bool) {
	host, path := splitURL(u)
	atl := reAtl.MatchString(host)
	if !atl && strings.ContainsAny(host, "$`") && reAtl.MatchString(raw) {
		atl, host = true, "?"
	}
	if !atl || (method == "POST" && reJiraRead.MatchString(path)) {
		return Hit{}, false
	}
	h := Hit{Class: 4, Verb: method, Target: host + path}
	h.Kind, h.Key = jiraKind(host, path)
	return h, true
}

func jiraKind(host, path string) (kind, key string) {
	switch {
	case strings.Contains(strings.ToLower(host), "bitbucket") || strings.Contains(path, "/repositories/"):
		return "bitbucket", ""
	case reComment.MatchString(path):
		return "comment", reComment.FindStringSubmatch(path)[1]
	case reTrans.MatchString(path):
		return "transition", reTrans.FindStringSubmatch(path)[1]
	}
	if m := reIssuePath.FindStringSubmatch(path); m != nil {
		return "other", m[1]
	}
	return "other", ""
}

// splitURL — хост без учётных данных и путь без query.
func splitURL(u string) (host, path string) {
	if !strings.Contains(u, "://") {
		u = "https://" + u
	}
	if p, err := url.Parse(u); err == nil && p.Host != "" {
		return p.Hostname(), p.Path
	}
	rest := u[strings.Index(u, "://")+3:]
	if i := strings.IndexAny(rest, "?#"); i >= 0 {
		rest = rest[:i]
	}
	if at := strings.LastIndex(rest, "@"); at >= 0 && at < strings.Index(rest+"/", "/") {
		rest = rest[at+1:]
	}
	host, path, _ = strings.Cut(rest, "/")
	return host, "/" + path
}

var (
	rePyWrite = regexp.MustCompile(`write_text|write_bytes|open\([^)]*['"][wa]b?\+?['"]|shutil\.(copy|move)|os\.(replace|rename)|writeFileSync|fs\.writeFile`)
	rePyHTTP  = regexp.MustCompile(`(?i)requests\.(post|put|delete|patch)|method\s*=\s*['"](POST|PUT|DELETE|PATCH)|urlopen\(.*data=|Request\([^)]*data=`)
	reStrLit  = regexp.MustCompile(`(?:\b(\w+)\s*=\s*)?(?:Path\(|expanduser\()?\s*['"]((?:~|/home/[^/'"\s]+)/\.claude/[^'"\n]+)['"]`)
)

// scriptProtectedWrite — путь класса 1, в который пишет строка скрипта: литерал пути или
// переменная, которой он присвоен, стоят в одной строке с записью (write_text, open(…,'w') …).
func scriptProtectedWrite(s, home string) string {
	type lit struct{ raw, path string }
	var lits []lit
	vars := map[string]string{}
	for _, m := range reStrLit.FindAllStringSubmatch(s, -1) {
		np, ok := normPath(m[2], home, "")
		if !ok || !protected(np, home) {
			continue
		}
		lits = append(lits, lit{m[2], np})
		if m[1] != "" {
			vars[m[1]] = np
		}
	}
	if len(lits) == 0 {
		return ""
	}
	for line := range strings.SplitSeq(s, "\n") {
		if !rePyWrite.MatchString(line) {
			continue
		}
		for _, l := range lits {
			if strings.Contains(line, l.raw) {
				return l.path
			}
		}
		for _, v := range slices.Sorted(maps.Keys(vars)) {
			if regexp.MustCompile(`\b` + regexp.QuoteMeta(v) + `\b`).MatchString(line) {
				return vars[v]
			}
		}
	}
	return ""
}

// scriptEditVerbs — слова правки в аргументах скрипта: `python3 fx.py sub <путь> …`.
var scriptEditVerbs = map[string]bool{"sub": true, "replace": true, "write": true, "edit": true, "patch": true, "apply": true, "fix": true, "set": true, "add": true, "append": true}

// scriptArgWrite — путь класса 1 в аргументах скрипта, если рядом слово правки; запуск без файла скрипта — "".
func (x callCtx) scriptArgWrite(args []string) (script, target string) {
	pos := positional(args, map[string]bool{"-m": true, "-W": true, "-X": true})
	if len(pos) < 2 || slices.Contains(args, "-c") || slices.Contains(args, "-e") {
		return "", ""
	}
	edit := false
	for _, a := range pos[1:] {
		edit = edit || scriptEditVerbs[a]
		if np, ok := normPath(a, x.home, x.cwd); ok && protected(np, x.ev.Home) && target == "" {
			target = np
		}
	}
	if !edit {
		return "", ""
	}
	return filepath.Base(pos[0]), target
}

// scriptWrites — эвристика по тексту скрипта python/node и по его аргументам: в числа классов не входит.
func (x callCtx) scriptWrites(c shellcmd.Call, args []string) {
	if script, t := x.scriptArgWrite(args); t != "" {
		x.v.Possible = append(x.v.Possible, Hit{Class: 1, Verb: c.Name + " " + script + " (аргумент)", Target: t})
	}
	var body strings.Builder
	for i, a := range args {
		if (a == "-c" || a == "-e") && i+1 < len(args) {
			body.WriteString(args[i+1])
		}
	}
	for _, r := range c.Redirs {
		if r.Op == "<<" || r.Op == "<<-" {
			body.WriteString(r.Body)
		}
	}
	s := body.String()
	if s == "" {
		return
	}
	if t := scriptProtectedWrite(s, x.ev.Home); t != "" {
		x.v.Possible = append(x.v.Possible, Hit{Class: 1, Verb: c.Name, Target: t})
	}
	if rePyHTTP.MatchString(s) && reAtl.MatchString(s) {
		x.v.Possible = append(x.v.Possible, Hit{Class: 4, Verb: c.Name, Target: "atlassian/bitbucket"})
	}
}

var (
	reFbRedir  = regexp.MustCompile(`(?:>>?|\btee(?:\s+-a)?)\s*["']?((?:~|\$HOME|\$\{HOME\}|/home/[^/\s]+)/\.claude/[^\s"';|&)]+)`)
	reFbLex    = regexp.MustCompile(`\blex\s+(build|absorb)\b[^\n;&|]*`)
	reFbGit    = regexp.MustCompile(`claude-config\.git[^\n;&|]*\b(commit|push)\b|\bcfg\s+(commit|push)\b`)
	reFbDeploy = regexp.MustCompile(`\bgraft\s+(?:-C\s+\S+\s+)*deploy\b([^\n;&|]*)`)
)

func regexFallback(ev Event, v *Verdict) {
	cmd := ev.Command
	for _, m := range reFbRedir.FindAllStringSubmatch(cmd, -1) {
		if np, ok := normPath(m[1], homeVar(ev), ev.Cwd); ok && protected(np, ev.Home) {
			v.Hits = append(v.Hits, Hit{Class: 1, Verb: "> (regex)", Target: np})
		}
	}
	for _, m := range reFbLex.FindAllString(cmd, -1) {
		x := callCtx{ev: ev, home: homeVar(ev), cwd: ev.Cwd, v: v}
		n := len(v.Hits)
		x.lex(strings.Fields(m)[1:])
		for i := n; i < len(v.Hits); i++ {
			v.Hits[i].Verb = regexVerb(v.Hits[i].Verb)
		}
	}
	if m := reFbGit.FindStringSubmatch(cmd); m != nil {
		v.Hits = append(v.Hits, Hit{Class: 2, Verb: "git " + m[1] + m[2] + " (regex)", Target: "~/.claude-config.git"})
	}
	for _, m := range reFbDeploy.FindAllStringSubmatch(cmd, -1) {
		if h, ok := graftDeploy(strings.Fields(m[1])); ok {
			h.Verb = regexVerb(h.Verb)
			v.Hits = append(v.Hits, h)
		}
	}
}

// regexVerb помечает попадание, найденное регулярками без разбора оболочкой.
func regexVerb(verb string) string { return verb + " (regex)" }

// unquote снимает кавычки оболочки из исходного текста слова, которое парсер не свёл к значению.
func unquote(s string) string {
	return strings.NewReplacer(`"`, "", `'`, "").Replace(s)
}

// normPath приводит путь к абсолютному; false — путь зависит от неизвестной переменной.
// home — значение $HOME и ~ в команде; "" — команда переприсвоила HOME, значение неизвестно.
func normPath(p, home, cwd string) (string, bool) {
	p = unquote(strings.TrimSpace(p))
	if p == "" {
		return "", false
	}
	if home == "" && (strings.Contains(p, "$HOME") || strings.Contains(p, "${HOME}") || strings.HasPrefix(p, "~")) {
		return "", false
	}
	p = strings.NewReplacer("${HOME}", home, "$HOME", home).Replace(p)
	if p == "~" || strings.HasPrefix(p, "~/") {
		p = home + p[1:]
	}
	if strings.ContainsAny(p, "$`") {
		return "", false
	}
	if !filepath.IsAbs(p) {
		if cwd == "" {
			return "", false
		}
		p = filepath.Join(cwd, p)
	}
	return filepath.Clean(p), true
}

// protected — путь из класса 1: инструкции и настройки в живом ~/.claude.
func protected(p, home string) bool {
	rel, ok := strings.CutPrefix(p, home+"/.claude/")
	if !ok {
		return false
	}
	if rel == "CLAUDE.md" || rel == "decisions.md" {
		return true
	}
	for _, d := range []string{"skills", "rules", "agents", "codex/frag", "codex/manifest"} {
		if rel == d || strings.HasPrefix(rel, d+"/") {
			return true
		}
	}
	// settings*.json прямо в ~/.claude
	return !strings.Contains(rel, "/") && strings.HasPrefix(rel, "settings") && strings.HasSuffix(rel, ".json")
}

// issueKeys — ключи задач Jira в тексте задания агента.
func issueKeys(text string) map[string]bool {
	out := map[string]bool{}
	for _, k := range reIssueKey.FindAllString(text, -1) {
		out[k] = true
	}
	return out
}
