package tools

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/aiseeq/akit/shellcmd"
	"github.com/aiseeq/chooks/internal/core"
	"github.com/aiseeq/chooks/internal/hookctx"
)

// RmGuardTool отклоняет rm и rmdir, на которые Claude Code сам остановит
// команду запросом «Dangerous rm operation on possibly-empty variable path»:
// этот запрос не снимается ни правилами, ни режимом bypassPermissions и ждёт
// пользователя. Хук отвечает раньше и даёт агенту переписанную форму.
//
// Правило повторяет Claude Code 2.1.330 (проверено прогоном команд через
// claude -p, таблица в тесте). Цель — переменная со слешем и глобом, слешем,
// ещё одной переменной или концом слова ($S/*, "$S/", $S/$N); позиционный
// параметр так же ("$1"/*); переменная перед каталогом верхнего уровня
// ($X/tmp), если её нет ни в команде, ни в окружении; переменная из $(pwd),
// $(git rev-parse …), $(dirname …), $(cd …). Переменную, которой команда
// в начале присвоила обычное значение, Claude Code подставляет и не спрашивает,
// но только пока команда простая: без ${…}, подоболочек, групп { }, функций,
// here-doc, аргумента из одной подстановки $(…) и без переприсваивания
// упомянутых переменных после первой команды.
//
// Вторая форма — относительная цель с глобом на конце (*, cl/*) в команде со
// сменой каталога: Claude Code спрашивает «Dangerous rm operation on
// statically-unresolvable target» (сверено с Claude Code 2.1.304 и 2.1.308,
// таблица в тесте), хук просит литеральный абсолютный путь.
//
// Третья форма — цель сама рабочая папка Bash или её родитель
// (workspaceReasons, «would remove a workspace directory»).
type RmGuardTool struct {
	*BaseTool
	lookupEnv func(string) (string, bool)
	// addDirs дополнительные рабочие папки сессии по CLAUDE_PID
	addDirs func(pid string) ([]string, error)
}

// NewRmGuardToolForRequest uses only the hook's environment.
func NewRmGuardToolForRequest(config core.ToolConfig, logger core.Logger, request hookctx.Context) (*RmGuardTool, error) {
	tool, err := newRmGuardTool(config, logger, request.LookupEnv)
	if err != nil {
		return nil, err
	}
	proc := request.ProcFS()
	tool.addDirs = func(pid string) ([]string, error) { return core.SessionAddDirsIn(proc, pid) }
	return tool, nil
}

func newRmGuardTool(config core.ToolConfig, logger core.Logger, lookupEnv func(string) (string, bool)) (*RmGuardTool, error) {
	return &RmGuardTool{
		BaseTool:  NewBaseTool("rm_guard", config.Enabled, core.ShellTools, logger),
		lookupEnv: lookupEnv,
		addDirs:   core.SessionAddDirs,
	}, nil
}

const rmGuardHeader = "rm по переменной без защиты: Claude Code спросит пользователя и будет ждать ответа. Перепиши команду:"

const rmCdGlobHeader = "rm с относительным глобом после cd: Claude Code спросит пользователя и будет ждать ответа. Перепиши команду:"

const (
	reName  = `[A-Za-z_][A-Za-z0-9_]*`
	reDflt  = `(?::?-(?:["']{2}|"?\$\{?` + reName + `\}?"?)?)?`
	reAfter = `[\x{E020}"']*\\?/(?:[*?\[{]|\$|/|["']|\x{E020}|$)`
	reTop   = `bin|boot|dev|etc|home|lib|lib32|lib64|libx32|media|mnt|opt|proc|root|run|sbin|srv|sys|tmp|usr|var|snap|nix|lost\+found|private|cores|Applications|Library|System|Users|Volumes|Windows|ProgramData|cygdrive`
)

var (
	// namedVarTarget $VAR/ перед глобом, слешем, переменной или концом слова
	namedVarTarget = regexp.MustCompile(`^[\x{E020}"']*\$(?:\{` + reName + reDflt + `\}|` + reName + `)` + reAfter)
	// positionalTarget то же для $1, $@, $*, $!
	positionalTarget = regexp.MustCompile(`^[\x{E020}"']*\$(?:\{(?:[0-9]+|[@*!])` + reDflt + `\}|[0-9@*!])` + reAfter)
	// topDirTarget $VAR/<каталог верхнего уровня>
	topDirTarget    = regexp.MustCompile(`(?i)^[\x{E020}"']*\$(?:\{(` + reName + `)(?::?-(?:["']{2}|"?\$\{?(` + reName + `)\}?"?)?)?\}|(` + reName + `))[\x{E020}"']*\\?/(` + reTop + `)(?:[\\/]+\*+)*/?[\x{E020}"']*$`)
	unreadVarTarget = regexp.MustCompile(`^[\x{E020}"']*\$\{(` + reName + `|[0-9]+|[@*])(?:[%#/,^?](?:[^{}]|\{[^{}]*\})*|:[0-9](?:[^{}]|\{[^{}]*\})*|:?\+(?:[^{}]|\{[^{}]*\})*|:?=(?:["']{2}|"?\$\{?` + reName + `\}?"?)?|:?[-=]["']?/[/.]*["']?)\}` + reAfter)
	// bareVar слово целиком — одна переменная: "$D", ${D}, $D/
	bareVar        = regexp.MustCompile(`^[\x{E020}"']*\$(?:\{(` + reName + `)(?::?[?-][^}]*)?\}|(` + reName + `))[\x{E020}"']*$`)
	assignWord     = regexp.MustCompile(`^(` + reName + `)\+?=`)
	plainName      = regexp.MustCompile(`^` + reName + `$`)
	braceExpansion = regexp.MustCompile(`\{[^{}'"]*(?:,|\.\.)[^{}'"]*\}`)
	lastPathPart   = regexp.MustCompile(`/+[^/]*/*$`)
	singleQuoted   = regexp.MustCompile(`^'[^']*\$`)
	positionalNm   = regexp.MustCompile(`\$\{?([0-9]+|[@*!])`)
	rmHomeGlob     = regexp.MustCompile(`^~/\*+/*$`)
	dotSlashes     = regexp.MustCompile(`/(?:\./)+`)
	manySlashes    = regexp.MustCompile(`/{2,}`)
	dirPrinting    = regexp.MustCompile(`^\s*(?:(cd|pwd|dirname)(?:[^A-Za-z0-9_-]|$)|git\s+(rev-parse)\b)`)
	dirnameOrCd    = regexp.MustCompile(`^\s*(?:cd|dirname)\s+(?:--?\w*\s+)*([^\s;&|]*)`)
	normallySet    = map[string]bool{"HOME": true, "USERPROFILE": true, "XDG_CONFIG_HOME": true, "XDG_DATA_HOME": true, "XDG_CACHE_HOME": true, "XDG_STATE_HOME": true, "XDG_RUNTIME_DIR": true, "PWD": true, "OLDPWD": true, "TMPDIR": true, "TMP": true, "TEMP": true}
	rmKeywords     = map[string]bool{"do": true, "then": true, "else": true, "elif": true, "if": true, "while": true, "until": true, "!": true, "coproc": true, "done": true, "fi": true, "esac": true}
	inlineShells   = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "ash": true, "mksh": true}
	declarers      = map[string]bool{"export": true, "declare": true, "typeset": true, "readonly": true, "local": true}
	dirChangers    = map[string]bool{"cd": true, "pushd": true, "popd": true, "chdir": true}
)

// ValidateTool проверяет цели rm и rmdir в команде Bash
func (t *RmGuardTool) ValidateTool(ctx context.Context, input *core.ToolInput) (*core.ValidationResult, error) {
	request, ok := hookctx.Request(ctx)
	if !ok {
		return nil, fmt.Errorf("hook request is missing")
	}
	local := *t
	local.lookupEnv = request.LookupEnv
	t = &local
	cmd := input.Command
	if !t.IsEnabled() || !core.IsShellTool(input.ToolName) || !strings.Contains(cmd, "rm") {
		return &core.ValidationResult{IsValid: true}, nil
	}
	script := &shScript{}
	parseShell(cmd, false, script)
	var reasons []string
	for _, c := range script.cmds {
		words := commandWords(c.words)
		if len(words) == 0 || (commandName(words[0].raw) != "rm" && commandName(words[0].raw) != "rmdir") {
			continue
		}
		for k, w := range words[1:] {
			if !w.redirect && rmHomeGlob.MatchString(w.raw) {
				reasons = append(reasons, rmInvocation(words, k+1, "")+": удаление всего в HOME Claude Code остановит, укажи более узкий литеральный путь")
			}
		}
	}
	if strings.Contains(cmd, "$") {
		vars := t.trackVars(script, input.CWD)
		for _, c := range script.cmds {
			reasons = append(reasons, t.checkCommand(c.words, vars, inlineNone)...)
		}
	}
	workspace, err := t.workspaceReasons(script, input.CWD)
	if err != nil {
		return nil, err
	}
	header := rmGuardHeader
	for _, form := range []struct {
		header  string
		reasons []string
	}{
		{rmCdGlobHeader, cdGlobReasons(script, input.CWD)},
		{rmWorkspaceHeader, workspace},
	} {
		if len(reasons) == 0 {
			header = form.header
		}
		reasons = append(reasons, form.reasons...)
	}
	if len(reasons) == 0 {
		return &core.ValidationResult{IsValid: true}, nil
	}
	reasons = deduplicateStrings(reasons)
	t.Logger().Debug("rm Claude Code would ask about denied", "reasons", len(reasons))
	violation := core.NewViolation("rm_guard", header, reasons[0], core.LevelCritical, 0, 0)
	return core.NewValidationResult(false, []core.Violation{violation}, reasons), nil
}

// rmVars что Claude Code знает о переменных команды
type rmVars struct {
	cwd                 string
	pipeLostAssignments bool
	// tracked — Claude Code подставляет значения переменных; иначе любая
	// переменная в цели считается возможно пустой
	tracked bool
	// resolved переменные с известным значением
	resolved map[string]bool
	// assigned переменные, которым команда что-то присваивает
	assigned map[string]bool
	// bound — непустые постоянные присваивания в оболочке для vRr.
	bound map[string]bool
	// derived переменные из вывода pwd и подобных: имя → источник
	derived map[string]string
	// exported переменные, переданные export или declare -x дочерним процессам
	exported map[string]bool
	// positionalBound — $1… заданы (функция, set --, аргументы sh -c)
	positionalBound bool
	lookupEnv       func(string) (string, bool)
}

// binding одно присваивание переменной
type binding struct {
	name, value string
	leading     bool // в цепочке присваиваний до первой обычной команды
	bare        bool // простое присваивание, не prefix перед командой и не цикл
	unset       bool
}

// trackVars разбирает присваивания так же, как Claude Code решает, можно ли
// подставить значения переменных
func (t *RmGuardTool) trackVars(s *shScript, cwd string) *rmVars {
	v := &rmVars{
		cwd:      cwd,
		tracked:  !s.dollarBrace && !s.group && !s.function && !s.heredoc && !s.complexArith,
		resolved: map[string]bool{}, assigned: map[string]bool{}, bound: map[string]bool{}, derived: map[string]string{},
		exported:        map[string]bool{},
		positionalBound: s.function,
		lookupEnv:       t.lookupEnv,
	}
	refs := v.scanWords(s)
	// Claude Code разделяет команду по конвейеру и не переносит в часть с rm
	// значение присваивания из другой части.
	lastPipe := -1
	for i, c := range s.cmds {
		if c.sep == "|" || c.sep == "|&" {
			lastPipe = i
		}
		if lastPipe >= 0 {
			words := commandWords(c.words)
			if len(words) > 0 && (commandName(words[0].raw) == "rm" || commandName(words[0].raw) == "rmdir") {
				v.tracked = false
				v.pipeLostAssignments = true
				break
			}
		}
	}
	bindings := v.collectBindings(s)
	v.applyBindings(bindings, refs, cwd)
	return v
}

// scanWords имена всех упомянутых переменных; аргумент из одной подстановки
// $(…) останавливает отслеживание
func (v *rmVars) scanWords(s *shScript) map[string]bool {
	refs := map[string]bool{}
	for _, c := range s.cmds {
		for _, w := range c.words {
			for _, name := range varRefs(w.text) {
				refs[name] = true
			}
			if !w.redirect && (w.raw == string(substMark) || w.raw == `"`+string(substMark)+`"`) {
				v.tracked = false
			}
		}
	}
	return refs
}

// collectBindings присваивания команд верхнего уровня по порядку
func (v *rmVars) collectBindings(s *shScript) []binding {
	var bindings []binding
	leading := true
	for _, c := range s.cmds {
		if c.nested {
			continue
		}
		var b []binding
		b, leading = commandBindings(c.words, leading, v)
		for _, x := range b {
			// S="$S" значения не меняет, и Claude Code его не замечает
			if m := bareVar.FindStringSubmatch(x.value); m != nil && m[1]+m[2] == x.name {
				continue
			}
			bindings = append(bindings, x)
		}
	}
	return bindings
}

// applyBindings отслеживание ломает упомянутая переменная, присвоенная не в
// начале, не простым присваиванием или больше одного раза
func (v *rmVars) applyBindings(bindings []binding, refs map[string]bool, cwd string) {
	count := map[string]int{}
	for _, b := range bindings {
		count[b.name]++
		v.assigned[b.name] = true
		if b.unset {
			v.bound[b.name] = false
		} else if b.bare {
			v.bound[b.name] = unquoteWord(b.value) != "" && v.valueResolved(b.value) && !v.rootValued(b.value)
		}
		if refs[b.name] && (!b.leading || !b.bare) {
			v.tracked = false
		}
	}
	for name, n := range count {
		if n > 1 && refs[name] {
			v.tracked = false
		}
	}
	for _, b := range bindings {
		if b.leading && b.bare && count[b.name] == 1 && v.valueResolved(b.value) {
			v.resolved[b.name] = true
		}
		v.trackDerived(b, cwd)
	}
}

// commandBindings присваивания одной простой команды; второй результат —
// остаётся ли цепочка ведущих присваиваний
func commandBindings(words []shWord, leading bool, v *rmVars) ([]binding, bool) {
	words = stripKeywords(words)
	var out []binding
	i := 0
	for i < len(words) && assignWord.MatchString(words[i].raw) {
		out = append(out, assignment(words[i], leading, true))
		i++
	}
	if i == len(words) {
		if len(out) > 1 {
			v.tracked = false // S=a T=b одной командой
		}
		return out, leading
	}
	if i > 0 {
		for k := range out {
			out[k].bare = false // S=a cmd: S задана только для cmd
		}
	}
	name := commandName(words[i].raw)
	args := words[i+1:]
	if declarers[name] {
		return append(out, declBindings(name, args, leading, v)...), leading
	}
	return append(out, commandSideBindings(name, args, v)...), false
}

// declBindings присваивания export, declare, local и подобных
func declBindings(name string, args []shWord, leading bool, v *rmVars) []binding {
	var out []binding
	exporting := name == "export"
	for _, w := range args {
		switch {
		case strings.HasPrefix(w.raw, "-") && strings.Contains(w.raw, "x"):
			exporting = true
		case assignWord.MatchString(w.raw):
			b := assignment(w, leading, true)
			if exporting {
				v.exported[b.name] = true
			}
			out = append(out, b)
		case exporting && assignName(w.raw):
			v.exported[w.raw] = true
		}
	}
	return out
}

// commandSideBindings переменные, которые задаёт или снимает сама команда:
// for, select, read, unset; set -- задаёт позиционные параметры
func commandSideBindings(name string, args []shWord, v *rmVars) []binding {
	var out []binding
	switch name {
	case "for", "select":
		if len(args) > 0 {
			out = append(out, binding{name: args[0].raw})
		}
	case "read", "unset":
		for _, w := range args {
			if !strings.HasPrefix(w.raw, "-") && assignName(w.raw) {
				out = append(out, binding{name: w.raw, unset: name == "unset"})
			}
		}
	case "set":
		if len(args) > 1 && args[0].raw == "--" {
			v.positionalBound = true
		}
	}
	return out
}

// rootValued соответствует Pst: корень, критический каталог или путь,
// заканчивающийся на . либо ... Такие значения исключаются из assigned в WOe.
func (v *rmVars) rootValued(value string) bool {
	p := unquoteWord(value)
	trimmed := strings.TrimRight(p, "/")
	if p != "" && trimmed == "" || trimmed == "." || trimmed == ".." || strings.HasSuffix(trimmed, "/.") || strings.HasSuffix(trimmed, "/..") {
		return true
	}
	if !path.IsAbs(p) || strings.ContainsAny(p, "$`") {
		return false
	}
	clean := path.Clean(p)
	home, ok := v.lookupEnv("HOME")
	return path.Dir(clean) == "/" || ok && home != "" && clean == path.Clean(home)
}

func assignment(w shWord, leading, bare bool) binding {
	m := assignWord.FindStringSubmatch(w.raw)
	value := w.text[strings.IndexByte(w.text, '=')+1:]
	return binding{name: m[1], value: value, leading: leading, bare: bare}
}

func assignName(s string) bool {
	return plainName.MatchString(s)
}

// valueResolved значение известно до запуска: без подстановок, ~ и
// переменных с неизвестным значением
func (v *rmVars) valueResolved(value string) bool {
	if strings.Contains(value, "$(") || strings.Contains(value, "`") || strings.HasPrefix(unquoteWord(value), "~") {
		return false
	}
	for _, name := range varRefs(value) {
		if !v.resolved[name] {
			return false
		}
	}
	return true
}

// trackDerived помечает переменную, заданную выводом pwd, git rev-parse,
// dirname или cd: защита ${D:?} не помогает, значение не пустое
func (v *rmVars) trackDerived(b binding, cwd string) {
	value := strings.TrimSuffix(strings.TrimPrefix(b.value, `"`), `"`)
	value = strings.TrimRight(value, "/")
	if name := bareVar.FindStringSubmatch(value); name != nil {
		n := name[1] + name[2]
		switch {
		case n == "PWD":
			v.derived[b.name] = "$PWD"
		case v.derived[n] != "":
			v.derived[b.name] = v.derived[n]
		default:
			delete(v.derived, b.name)
		}
		return
	}
	inner, ok := substBody(value)
	home, _ := v.lookupEnv("HOME")
	if !ok || !printsDir(inner, cwd, home) {
		delete(v.derived, b.name)
		return
	}
	v.derived[b.name] = value
}

// substBody тело $(…) или `…`, если значение целиком из подстановки
func substBody(value string) (string, bool) {
	if strings.HasPrefix(value, "`") && strings.HasSuffix(value, "`") && len(value) > 1 {
		return value[1 : len(value)-1], true
	}
	if !strings.HasPrefix(value, "$(") {
		return "", false
	}
	src := []rune(value)
	end := matchParen(src, 1)
	if end != len(src)-1 {
		return "", false
	}
	return string(src[2:end]), true
}

// printsDir подстановка печатает каталог, который может оказаться рабочей
// папкой, корнем репозитория или системной папкой
func printsDir(inner, cwd, home string) bool {
	m := dirPrinting.FindStringSubmatch(inner)
	if m == nil {
		return false
	}
	if m[2] == "rev-parse" || m[1] == "pwd" {
		return true
	}
	arg := dirnameOrCd.FindStringSubmatch(inner)
	if arg == nil || arg[1] == "" || strings.ContainsAny(arg[1], "$`") || arg[1] == `""` || arg[1] == "''" {
		return true
	}
	if m[1] == "dirname" {
		return criticalDir(parentDir(unquoteWord(arg[1])), cwd, home)
	}
	parts := strings.Split(inner[len(arg[0]):], "&&")
	if strings.TrimSpace(parts[0]) != "" {
		return true
	}
	for _, p := range parts[1:] {
		if strings.ContainsAny(p, ";|&") {
			return true
		}
	}
	return false
}

// parentDir то, что напечатает dirname
func parentDir(p string) string {
	trimmed := lastPathPart.ReplaceAllString(p, "")
	if trimmed != "" {
		return trimmed
	}
	if strings.HasPrefix(p, "/") {
		return "/"
	}
	return "."
}

// criticalDir корень, каталог верхнего уровня, домашний, рабочий или его предок
func criticalDir(p, cwd, home string) bool {
	if !path.IsAbs(p) {
		p = path.Join(cwd, p)
	}
	p = path.Clean(p)
	if home != "" && p == path.Clean(home) {
		return true
	}
	return p == "/" || path.Dir(p) == "/" || p == path.Clean(cwd) || strings.HasPrefix(path.Clean(cwd)+"/", p+"/")
}

// inline откуда взят текст команды
type inline int

const (
	inlineNone     inline = iota
	inlineSingle          // sh -c '…': переменные внешней оболочки не видны
	inlineReparsed        // eval или sh -c "…": текст разбирается повторно
)

// commandWords слова простой команды начиная с имени утилиты: без then/do,
// присваиваний, перенаправлений и обёрток sudo, timeout, xargs; пусто — команды нет
func commandWords(words []shWord) []shWord {
	words = stripKeywords(words)
	i := 0
	for i < len(words) && (assignWord.MatchString(words[i].raw) || words[i].redirect ||
		strings.HasPrefix(words[i].raw, "$") || strings.HasPrefix(words[i].raw, string(substMark))) {
		i++
	}
	return wrappedCommandWords(words, i, false)
}

// checkCommand возвращает причины отказа для одной простой команды
func (t *RmGuardTool) checkCommand(words []shWord, v *rmVars, src inline) []string {
	words = commandWords(words)
	if len(words) == 0 {
		return nil
	}
	name := commandName(words[0].raw)
	args := words[1:]
	switch {
	case name == "rm" || name == "rmdir":
		return t.checkTargets(words, v, src)
	case name == "eval":
		return t.checkInline(joinText(args), v, inlineReparsed)
	case name == "trap":
		if len(args) > 0 && args[0].raw == "--" {
			args = args[1:]
		}
		if len(args) > 0 {
			return t.checkInline(unquoteWord(args[0].text), v, src)
		}
	case inlineShells[name]:
		return t.checkShellC(args, v)
	case name == "find":
		return t.checkFindExec(args, v, src)
	}
	return nil
}

// wrappedCommandWords пропускает только ведущие слова оболочки, после которых
// shellcmd сообщает реальный вызов и его смещение. Слова остаются исходными
// shWord, чтобы проверки целей использовали прежнее представление кавычек и
// подстановок.
func wrappedCommandWords(words []shWord, start int, cdRule bool) []shWord {
	if start >= len(words) {
		return nil
	}
	var source strings.Builder
	offsets := make(map[int]int, len(words)-start)
	for i := start; i < len(words); i++ {
		if i > start {
			source.WriteByte(' ')
		}
		offsets[source.Len()] = i
		source.WriteString(words[i].raw)
	}
	calls, err := shellcmd.Calls(source.String())
	if err != nil {
		return words[start:]
	}
	// shellcmd также возвращает вызовы из `sh -c` и `find -exec`. Эти
	// вложенные команды rm guard разбирает собственными проверками и не должен
	// принимать за снятие внешней обёртки.
	var root *shellcmd.Call
	for i := range calls {
		call := &calls[i]
		if call.Offset < 0 || len(call.Via) != 0 {
			continue
		}
		if root == nil || call.Offset < root.Offset {
			root = call
		}
	}
	if root != nil && (shellcmd.IsShell(root.Name) || root.Name == "find") {
		return words[start:]
	}
	var first *shellcmd.Call
	for i := range calls {
		call := &calls[i]
		if len(call.Via) == 0 || call.Offset < 0 {
			continue
		}
		if cdRule {
			preserve := false
			for _, wrapper := range call.Via {
				if wrapper == "sudo" || wrapper == "xargs" {
					preserve = true
					break
				}
			}
			if preserve {
				return words[start:]
			}
		}
		if first == nil || len(call.Via) < len(first.Via) || len(call.Via) == len(first.Via) && call.Offset < first.Offset {
			first = call
		}
	}
	if first == nil {
		return words[start:]
	}
	wordIndex, ok := offsets[first.Offset]
	if !ok {
		// Offset может указывать внутрь токена после раскрытия shellcmd;
		// сопоставляем его с началом содержащего слова.
		bestOffset := -1
		for offset, index := range offsets {
			if offset <= first.Offset && offset > bestOffset {
				wordIndex, ok = index, true
				bestOffset = offset
			}
		}
	}
	if !ok || wordIndex < start {
		return words[start:]
	}
	return words[wordIndex:]
}

// checkInline разбирает строку eval, trap или sh -c как отдельную команду
func (t *RmGuardTool) checkInline(text string, v *rmVars, src inline) []string {
	inner := &shScript{}
	parseShell(text, false, inner)
	var reasons []string
	for _, c := range inner.cmds {
		reasons = append(reasons, t.checkCommand(c.words, v, src)...)
	}
	return reasons
}

// checkShellC проверяет скрипт sh -c: в одинарных кавычках переменные
// внешней оболочки ему не видны, в двойных их раскрывает внешняя оболочка
func (t *RmGuardTool) checkShellC(args []shWord, v *rmVars) []string {
	for k, w := range args {
		if !strings.HasPrefix(w.raw, "-") || !strings.Contains(w.raw[1:], "c") || k+1 >= len(args) {
			continue
		}
		script := args[k+1]
		if !strings.HasPrefix(script.raw, "'") {
			return t.checkInline(unquoteWord(script.text), v, inlineReparsed)
		}
		// внутренней оболочке видны только экспортированные переменные
		fresh := &rmVars{
			cwd:     v.cwd,
			tracked: v.tracked, resolved: map[string]bool{}, assigned: map[string]bool{}, bound: map[string]bool{}, derived: map[string]string{},
			exported: v.exported, positionalBound: k+2 < len(args),
			lookupEnv: v.lookupEnv,
		}
		for name := range v.exported {
			fresh.resolved[name] = v.resolved[name]
			fresh.assigned[name] = v.assigned[name]
			fresh.bound[name] = v.bound[name]
		}
		return t.checkInline(unquoteWord(script.text), fresh, inlineSingle)
	}
	return nil
}

// checkFindExec проверяет команду после -exec, -execdir, -ok, -okdir
func (t *RmGuardTool) checkFindExec(args []shWord, v *rmVars, src inline) []string {
	var reasons []string
	start := -1
	for k, w := range args {
		switch r := w.raw; {
		case start < 0 && (r == "-exec" || r == "-execdir" || r == "-ok" || r == "-okdir"):
			start = k + 1
		case start >= 0 && (r == ";" || r == `\;` || r == `';'` || (r == "+" && k > 0 && args[k-1].raw == "{}")):
			reasons = append(reasons, t.checkCommand(args[start:k], v, src)...)
			start = -1
		}
	}
	if start >= 0 {
		reasons = append(reasons, t.checkCommand(args[start:], v, src)...)
	}
	return reasons
}

// checkTargets проверяет аргументы rm или rmdir; words[0] — сама утилита
func (t *RmGuardTool) checkTargets(words []shWord, v *rmVars, src inline) []string {
	var reasons []string
	for k, w := range words[1:] {
		raw := w.raw
		if raw == "" || w.redirect || strings.HasPrefix(raw, "-") || singleQuoted.MatchString(raw) {
			continue
		}
		if reason := t.checkTarget(w, v, src); reason != nil {
			reasons = append(reasons, rmInvocation(words, k+1, "")+": "+reason(words, k+1))
		}
	}
	return reasons
}

// hint собирает подсказку по команде и номеру цели
type hint func(words []shWord, target int) string

// checkTarget возвращает подсказку, если Claude Code спросит про эту цель
func (t *RmGuardTool) checkTarget(w shWord, v *rmVars, src inline) hint {
	raw := w.raw
	if result := t.checkHomeGlobTarget(w, v); result != nil {
		return result
	}
	if m := unreadVarTarget.FindStringSubmatch(raw); m != nil && m[1] != "HOME" && (!v.bound[m[1]] || v.pipeLostAssignments) {
		return guardHint(src, []string{m[1]}, "")
	}
	if result := derivedTargetHint(raw, v); result != nil {
		return result
	}
	if result := topDirectoryTargetHint(raw, v, src); result != nil {
		return result
	}
	if result := positionalTargetHint(raw, v); result != nil {
		return result
	}
	return namedVariableTargetHint(raw, w.text, v, src)
}

func (t *RmGuardTool) checkHomeGlobTarget(w shWord, v *rmVars) hint {
	raw := w.raw
	if names := varRefs(w.text); len(names) == 1 && names[0] == "HOME" {
		homeTarget := strings.ReplaceAll(trimQuotes(raw), `"`, "")
		homeTarget = regexp.MustCompile(`\$\{HOME(?::\?[^}]*)?\}`).ReplaceAllStringFunc(homeTarget, func(string) string { return "$HOME" })
		if after, ok := strings.CutPrefix(homeTarget, "$HOME/"); ok && globTail.MatchString("/"+after) {
			home, present := v.lookupEnv("HOME")
			dir := path.Join(home, globTail.ReplaceAllString("/"+after, ""))
			if present && home != "" && !strings.ContainsAny(dir, "*?[$`") && (pathWithin(dir, v.cwd) || dir == path.Clean(home)) {
				return func([]shWord, int) string {
					return "удаление всего в HOME Claude Code остановит, укажи более узкий литеральный путь"
				}
			}
		}
	}
	return nil
}

func derivedTargetHint(raw string, v *rmVars) hint {
	if m := bareVar.FindStringSubmatch(trimGlobTail(raw)); m != nil {
		if from := v.derived[m[1]+m[2]]; from != "" {
			name := m[1] + m[2]
			return func([]shWord, int) string {
				return fmt.Sprintf("$%s задан из `%s` и может оказаться рабочей папкой, защита ${%s:?} не поможет, укажи литеральный путь", name, from, name)
			}
		}
	}
	return nil
}

func topDirectoryTargetHint(raw string, v *rmVars, src inline) hint {
	if m := topDirTarget.FindStringSubmatch(normalizeSlashes(raw)); m != nil {
		names := []string{m[1] + m[3]}
		if m[2] != "" {
			names = append(names, m[2])
		}
		if v.allMayBeUnset(names) {
			return guardHint(src, names, "при пустой $"+names[0]+" удалит /"+m[4]+", ")
		}
	}
	return nil
}

func positionalTargetHint(raw string, v *rmVars) hint {
	if positionalTarget.MatchString(raw) && !v.positionalBound {
		name := positionalNm.FindStringSubmatch(raw)[1]
		return func([]shWord, int) string {
			return fmt.Sprintf("$%s в команде Bash пуст, укажи литеральный путь", name)
		}
	}
	return nil
}

func namedVariableTargetHint(raw, text string, v *rmVars, src inline) hint {
	if !namedVarTarget.MatchString(raw) {
		return nil
	}
	names := varRefs(text)
	// "$S"/{a,b}: раскрытие скобок Claude Code не подставляет
	if v.tracked && v.allResolved(names) && !braceExpansion.MatchString(raw) {
		return nil
	}
	for _, n := range names {
		if normallySet[n] {
			return func([]shWord, int) string {
				return fmt.Sprintf("$%s обычно задана, защита не поможет, укажи литеральный путь", n)
			}
		}
	}
	return guardHint(src, names, "")
}

// guardHint просит назвать конкретный путь, не меняя раскрытие и разбиение Bash.
func guardHint(_ inline, _ []string, prefix string) hint {
	return func(words []shWord, target int) string {
		example := "rm -f /abs/dir/*.tmp"
		if commandName(words[0].raw) == "rmdir" {
			example = "rmdir /abs/dir/*"
		}
		return fmt.Sprintf("%sподставь путь каталога буквально, например: `%s`", prefix, example)
	}
}

// cdGlobReasons rm с относительной целью, которая после сложения с рабочей
// папкой кончается на /* (*, cl/*, ./*, ../*), в команде со сменой каталога.
// Claude Code не знает, в каком каталоге раскроется глоб, и спрашивает
// «Dangerous rm operation on statically-unresolvable target» даже в
// bypassPermissions. Сменой каталога он считает cd, pushd, popd и chdir в любом
// месте команды: после rm, после then и do, внутри $(…) и sh -c '…'. rm внутри
// строки (node -e '…', echo "…", eval '…') и после sudo, xargs, find -exec он
// не проверяет
func cdGlobReasons(s *shScript, cwd string) []string {
	cmds := withInlineScripts(s.cmds)
	if !changesDir(cmds) {
		return nil
	}
	var reasons []string
	for rmCommandIndex, c := range cmds {
		words := cdCommandWords(c.words)
		if len(words) == 0 {
			continue
		}
		if name := commandName(words[0].raw); name != "rm" && name != "rmdir" {
			continue
		}
		for k, w := range words[1:] {
			if w.redirect || strings.HasPrefix(w.raw, "-") || !relativeDirGlob(w.text) {
				continue
			}
			if !multiLevelDirGlob(w.text) && claude330AllowsCdGlob(cmds, w, cwd, rmCommandIndex) {
				continue
			}
			absolute := "/полный/путь/" + strings.TrimPrefix(unquoteWord(w.text), "./")
			reasons = append(reasons, fmt.Sprintf("%s: после cd каталог глоба %s заранее не известен, укажи литеральный абсолютный путь: %s",
				rmInvocation(words, k+1, ""), w.text, rmInvocation(words, k+1, absolute)))
		}
	}
	return reasons
}

// claude330AllowsCdGlob воспроизводит исключения Claude Code 2.1.330 для
// cd внутрь cwd и простого cd ./X или cd . непосредственно через &&.
func claude330AllowsCdGlob(cmds []shCommand, target shWord, cwd string, rmIndex int) bool {
	if cwd == "" || rmIndex < 0 {
		return false
	}
	base := path.Clean(cwd)
	cdCount := countDirChanges(cmds)
	if !validCdGlobCommandChain(cmds, rmIndex, cdCount) {
		return false
	}
	current, mode, known, ok := cdGlobPathState(cmds, rmIndex, cdCount, base)
	if !ok {
		return false
	}
	return cdGlobTargetAllowed(cmds, target, rmIndex, base, current, mode, known, cdCount)
}

func countDirChanges(cmds []shCommand) int {
	count := 0
	for _, c := range cmds {
		if commandChangesDir(c) {
			count++
		}
	}
	return count
}

// commandChangesDir — команда может сменить каталог: после снятия обёрток
// это cd, pushd или popd, или такое имя стоит в ней любым словом. Второе
// ловит обёртку, которую shellcmd не снимает (`time -- cd .`): лучше лишний
// отказ на `echo cd`, чем глоб rm в неизвестном каталоге
func commandChangesDir(c shCommand) bool {
	if words := cdCommandWords(c.words); len(words) > 0 && dirChangers[commandName(words[0].raw)] {
		return true
	}
	for _, w := range c.words {
		if !w.redirect && dirChangers[commandName(unquoteWord(w.raw))] {
			return true
		}
	}
	return false
}

func validCdGlobCommandChain(cmds []shCommand, rmIndex, cdCount int) bool {
	if cdCount > 1 {
		for _, c := range cmds[1:] {
			if c.sep != "&&" {
				return false
			}
		}
	}
	if cdCount == 1 {
		if cmds[rmIndex].subshell || rmIndex+1 < len(cmds) && (cmds[rmIndex+1].sep == "|" || cmds[rmIndex+1].sep == "|&") {
			return false
		}
	}
	// dIe проверяет всю цепочку, в том числе операции после rm. Для
	// абсолютного cd отдельная проверка может пропустить форму с одним ';'
	// между cd и rm, но не восстанавливает цепочку cd && rm; следующая команда.
	if cdCount == 1 && cmds[rmIndex].sep == "&&" {
		for _, c := range cmds[rmIndex+1:] {
			if c.sep == ";" || c.sep == "\n" || c.sep == "||" || c.sep == "&" {
				return false
			}
		}
	}
	return true
}

func cdGlobPathState(cmds []shCommand, rmIndex, cdCount int, base string) (current, mode string, known, ok bool) {
	state := cdGlobPath{current: base, known: true}
	for i := range rmIndex {
		c := cmds[i]
		words := cdCommandWords(c.words)
		if len(words) == 0 || commandName(words[0].raw) != "cd" {
			continue
		}
		var valid bool
		state, valid = advanceCdGlobPath(cmds, i, rmIndex, cdCount, base, state, c)
		if !valid {
			return "", "", false, false
		}
	}
	if state.mode == "" || !state.known {
		return "", "", false, false
	}
	return state.current, state.mode, state.known, true
}

type cdGlobPath struct {
	current string
	mode    string
	known   bool
}

func advanceCdGlobPath(cmds []shCommand, index, rmIndex, cdCount int, base string, state cdGlobPath, command shCommand) (cdGlobPath, bool) {
	cdArg, valid := cdLiteralArgument(command, cdCount > 1)
	if !valid || cdCount == 1 && index != 0 {
		return cdGlobPath{}, false
	}
	cdPath := strings.Trim(unquoteWord(cdArg.text), "\"'")
	if strings.HasPrefix(cdPath, "/") {
		return absoluteCdGlobPath(cmds, rmIndex, cdCount, base, state, cdPath)
	}
	return relativeCdGlobPath(cmds, index, rmIndex, cdCount, base, state, cdPath)
}

func absoluteCdGlobPath(cmds []shCommand, rmIndex, cdCount int, base string, state cdGlobPath, cdPath string) (cdGlobPath, bool) {
	if hasParentSegment(cdPath) {
		state.known, state.mode = false, ""
		return state, true
	}
	state.current = path.Clean(cdPath)
	if cdCount == 1 && cmds[rmIndex].sep != "&&" && state.current != base || !cdRealWithin(base, state.current) {
		return cdGlobPath{}, false
	}
	state.known = state.known && pathWithin(base, state.current)
	state.mode = "absolute"
	return state, true
}

func relativeCdGlobPath(cmds []shCommand, index, rmIndex, cdCount int, base string, state cdGlobPath, cdPath string) (cdGlobPath, bool) {
	if !state.known || hasParentSegment(cdPath) || strings.ContainsAny(cdPath, "*?[]$`") {
		state.known, state.mode = false, ""
		return state, true
	}
	dotRelative := cdPath == "." || strings.HasPrefix(cdPath, "./")
	if !dotRelative && (state.mode == "" || cdCount == 1) {
		state.known = false
		return state, true
	}
	for j := index + 1; j <= rmIndex; j++ {
		if cmds[j].sep != "&&" {
			state.known = false
			break
		}
	}
	if !state.known {
		return state, true
	}
	state.current = path.Clean(path.Join(state.current, cdPath))
	if !cdRealWithin(base, state.current) {
		return cdGlobPath{}, false
	}
	state.known = pathWithin(base, state.current)
	state.mode = "relative"
	return state, true
}

func cdGlobTargetAllowed(cmds []shCommand, target shWord, rmIndex int, base, current, mode string, known bool, cdCount int) bool {
	if mode == "" || !known {
		return false
	}
	globPath := unquoteWord(target.text)
	// В подкаталоге цель остаётся относительной для проверки g0; cd в
	// исходный cwd удаляется из цепочки и сохраняет прежнее исключение.
	if cdCount == 1 && mode == "absolute" && current != base && (strings.HasSuffix(globPath, "/") || hasParentSegment(globPath)) {
		return false
	}
	if mode == "relative" && !pathWithin(base, path.Clean(path.Join(current, globPath))) {
		return false
	}
	if mode == "relative" && (strings.HasSuffix(target.text, "/") || hasParentSegment(globPath)) {
		return false
	}
	if mode == "absolute" && cmds[rmIndex].sep != "&&" && strings.HasSuffix(globPath, "/") {
		return false
	}
	return !(mode == "absolute" && strings.HasSuffix(target.text, "/") && hasParentSegment(globPath))
}

// cdLiteralArgument проверяет форму cd до фильтрации префиксов; при нескольких
// cd локальная ветка Claude Code допускает перенаправления и необязательное --.
// Исключение Claude Code — только для буквального первого слова cd: обёртка,
// ключевое слово, \cd, путь или другой регистр его не получают
func cdLiteralArgument(c shCommand, multi bool) (shWord, bool) {
	words := c.words
	if c.subshell || len(words) == 0 || words[0].raw != "cd" {
		return shWord{}, false
	}
	var args []shWord
	for _, w := range words[1:] {
		if w.redirect {
			if !multi {
				return shWord{}, false
			}
			continue
		}
		args = append(args, w)
	}
	if multi && len(args) > 0 && args[0].raw == "--" {
		args = args[1:]
	}
	if len(args) != 1 {
		return shWord{}, false
	}
	return args[0], true
}

func cdRealWithin(base, dir string) bool {
	realBase, baseErr := realPath(base)
	realDir, dirErr := realPath(dir)
	return baseErr == nil && dirErr == nil && pathWithin(realBase, realDir)
}

// multiLevelDirGlob соответствует g0: к числу завершающих /* прибавляется
// число компонентов с глобом в оставшейся относительной части пути.
func multiLevelDirGlob(text string) bool {
	p := path.Clean(unquoteWord(text))
	base := globTail.ReplaceAllString(p, "")
	if base == p {
		return false
	}
	for part := range strings.SplitSeq(base, "/") {
		if strings.ContainsAny(part, "*?[") {
			return true
		}
	}
	return strings.Count(strings.TrimPrefix(p, base), "/") > 1
}

func pathWithin(base, p string) bool {
	if base == "/" {
		return strings.HasPrefix(p, "/")
	}
	return p == base || strings.HasPrefix(p, base+"/")
}

func hasParentSegment(p string) bool {
	return strings.Contains("/"+p+"/", "/../")
}

func trimQuotes(s string) string { return strings.Trim(s, "\"'") }

// cdCommandWords слова команды с имени утилиты так, как их видит проверка
// Claude Code: без then/do/!, присваиваний, перенаправлений и обёрток, которые
// он снимает (time, timeout 5, nice, nohup, env, command, builtin, stdbuf, noglob);
// sudo и xargs он не снимает. Пусто — команды нет
func cdCommandWords(words []shWord) []shWord {
	words = stripKeywords(words)
	i := 0
	for i < len(words) && (assignWord.MatchString(words[i].raw) || words[i].redirect) {
		i++
	}
	return wrappedCommandWords(words, i, true)
}

// withInlineScripts команды вместе с командами скриптов sh -c '…': Claude Code
// разбирает их строку так же, как саму команду
func withInlineScripts(cmds []shCommand) []shCommand {
	var out []shCommand
	for _, c := range cmds {
		out = append(out, c)
		words := cdCommandWords(c.words)
		if len(words) == 0 || !inlineShells[commandName(words[0].raw)] {
			continue
		}
		args := words[1:]
		for k, w := range args {
			if strings.HasPrefix(w.raw, "-") && strings.Contains(w.raw[1:], "c") && k+1 < len(args) {
				inner := &shScript{}
				parseShell(unquoteWord(args[k+1].text), false, inner)
				out = append(out, withInlineScripts(inner.cmds)...)
				break
			}
		}
	}
	return out
}

// changesDir в команде есть смена каталога
func changesDir(cmds []shCommand) bool {
	return slices.ContainsFunc(cmds, commandChangesDir)
}

// relativeDirGlob цель относительная (не /…, ~…, $…) и, сложенная с рабочей
// папкой, кончается на /*
func relativeDirGlob(text string) bool {
	target := unquoteWord(text)
	if target == "" || strings.HasPrefix(target, "/") || strings.HasPrefix(target, "~") || strings.ContainsAny(text, "$`") {
		return false
	}
	return strings.HasSuffix(path.Clean("/cwd/"+target), "/*")
}

// allResolved все переменные цели известны (HOME — из окружения)
func (v *rmVars) allResolved(names []string) bool {
	for _, n := range names {
		if v.resolved[n] {
			continue
		}
		if n == "HOME" && !v.assigned[n] {
			if val, ok := v.lookupEnv(n); ok && val != "" {
				continue
			}
		}
		return false
	}
	return true
}

// allMayBeUnset переменные не присвоены командой и пусты в окружении
func (v *rmVars) allMayBeUnset(names []string) bool {
	for _, n := range names {
		if v.assigned[n] || n == "HOME" || n == "PWD" {
			return false
		}
		if val, ok := v.lookupEnv(n); ok && val != "" {
			return false
		}
	}
	return true
}

// rmInvocation текст вызова rm; replacement заменяет цель target
func rmInvocation(words []shWord, target int, replacement string) string {
	parts := make([]string, 0, len(words))
	for k, w := range words {
		if k == target && replacement != "" {
			parts = append(parts, replacement)
			continue
		}
		parts = append(parts, w.text)
	}
	return "`" + strings.Join(parts, " ") + "`"
}

// stripKeywords снимает do, then, if, ! и подобные в начале команды
func stripKeywords(words []shWord) []shWord {
	for len(words) > 0 && rmKeywords[words[0].raw] {
		words = words[1:]
	}
	return words
}

// commandName имя утилиты: без \ в начале, пути и регистра
func commandName(raw string) string {
	name := strings.TrimPrefix(raw, `\`)
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	return strings.ToLower(name)
}

func joinText(words []shWord) string {
	parts := make([]string, 0, len(words))
	for _, w := range words {
		parts = append(parts, unquoteWord(w.text))
	}
	return strings.Join(parts, " ")
}

// trimGlobTail снимает хвост из / * . перед закрывающими кавычками: "$D/" → "$D"
func trimGlobTail(raw string) string {
	r := []rune(raw)
	end := len(r)
	for end > 0 && (r[end-1] == '"' || r[end-1] == '\'' || r[end-1] == substMark) {
		end--
	}
	start := end
	for start > 0 && strings.ContainsRune("/*.", r[start-1]) {
		start--
	}
	if start < end && r[start] == '/' {
		return string(r[:start]) + string(r[end:])
	}
	return raw
}

// normalizeSlashes /./ и // → /
func normalizeSlashes(s string) string {
	return manySlashes.ReplaceAllString(dotSlashes.ReplaceAllString(s, "/"), "/")
}

func deduplicateStrings(items []string) []string {
	seen := make(map[string]bool, len(items))
	out := items[:0]
	for _, s := range items {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
