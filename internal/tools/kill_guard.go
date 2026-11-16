package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/aiseeq/akit/shellcmd"
	"github.com/aiseeq/chooks/internal/core"
	"mvdan.cc/sh/v3/syntax"
)

// protectedComms процессы, без которых падает сессия пользователя целиком:
// init и менеджеры служб, шина, графическая оболочка, звук, вход по ssh
var protectedComms = map[string]bool{
	"systemd": true, "init": true, "systemd-logind": true,
	"dbus-daemon": true, "dbus-broker": true, "sshd": true, "sshd-session": true,
	"Xwayland": true, "Xorg": true, "kwin_wayland": true, "kwin_x11": true,
	"plasmashell": true, "gnome-shell": true, "sway": true, "Hyprland": true,
	"sddm": true, "gdm": true, "pipewire": true, "wireplumber": true,
}

// KillGuardTool блокирует kill, pkill и killall, цель которых нельзя
// проверить до запуска или которые заденут саму сессию: PID 1, systemd,
// предков хука (claude, терминал), оболочку, исполняющую команду
type KillGuardTool struct {
	*BaseTool
	procRoot string
	selfPID  int
}

// NewKillGuardTool создает защиту от убийства сессии
func NewKillGuardTool(config core.ToolConfig, logger core.Logger) (*KillGuardTool, error) {
	return &KillGuardTool{
		BaseTool: NewBaseTool("kill_guard", config.Enabled, core.ShellTools, logger),
		procRoot: "/proc",
		selfPID:  os.Getpid(),
	}, nil
}

const killGuardHint = "Найди PID отдельной командой (ps -o pid,ppid,comm,args -p … или pgrep -a '[x]yz'), посмотри на вывод и убей литералом: kill 12345"

// killGroupHint подсказка к отказу на сигнал группе: отдельные PID группу не
// заменяют — процесс, который группа породит после перечисления, сигнал не получит
const killGroupHint = "Сигнал отдельным PID — не то же, что группе: процессы, которые появятся в группе после перечисления, его не получат. Перечисли группу отдельной командой (pgrep -a -g 12345), посмотри на вывод, убей её PID литералами (kill 12346 12347) и проверь группу снова"

// ValidateTool проверяет команды, посылающие сигналы процессам
func (t *KillGuardTool) ValidateTool(_ context.Context, input *core.ToolInput) (*core.ValidationResult, error) {
	if !t.IsEnabled() || !core.IsShellTool(input.ToolName) || !strings.Contains(unquoted(input.Command), "kill") {
		return &core.ValidationResult{IsValid: true}, nil
	}

	scan := killScan{tool: t, command: input.Command, signalWord: signalWordRe.MatchString(unquoted(input.Command))}
	if err := scan.parsedCalls(); err != nil {
		return nil, err
	}
	if len(scan.reasons) == 0 {
		return &core.ValidationResult{IsValid: true}, nil
	}

	violations := make([]core.Violation, 0, len(scan.reasons))
	for _, r := range scan.reasons {
		violations = append(violations, core.NewViolation("kill_guard",
			"Сигнал может убить не тот процесс или саму сессию:", r, core.LevelCritical, 0, 0))
	}
	return core.NewValidationResult(false, violations, append(scan.reasons, scan.hints()...)), nil
}

// hints подсказки к отказу: про группу — если есть отказ на сигнал группе,
// обычная про литеральный PID — если есть другие отказы
func (s *killScan) hints() []string {
	var out []string
	if len(s.groupReasons) > 0 {
		out = append(out, killGroupHint)
	}
	if len(s.reasons) > len(s.groupReasons) {
		out = append(out, killGuardHint)
	}
	return out
}

// unquoted команда без кавычек и \: k\ill и 'k'ill оболочка исполнит как kill
func unquoted(command string) string {
	return strings.NewReplacer(`\`, "", `'`, "", `"`, "").Replace(command)
}

// maxScriptNesting предел вложенности sh -c и eval: глубже — отказ
const maxScriptNesting = 4

// killScan проверка вызовов команды, найденных разбором оболочки
type killScan struct {
	tool *KillGuardTool
	// command — команда целиком: её текст стоит в cmdline оболочки, и pkill -f
	// по нему убьёт эту оболочку
	command string
	procs   []procInfo
	reasons []string
	// groupReasons — те из reasons, что про сигнал группе процессов (kill -N)
	groupReasons []string
	// signalWord — в команде без кавычек есть отдельное слово kill, pkill
	// или killall: скрипт, который станет известен только при запуске, рядом
	// с ним — отказ. Подстрока kill (skills) — не повод
	signalWord bool
}

// signalWordRe отдельное слово kill, pkill, killall, killall5
var signalWordRe = regexp.MustCompile(`(^|[^[:alnum:]_.-])(p?kill|killall5?)([^[:alnum:]_.-]|$)`)

// signalNames утилиты, посылающие сигнал
var signalNames = []string{"kill", "pkill", "killall", "killall5"}

// scriptRunners команды, исполняющие свои аргументы как скрипт оболочки,
// кроме sh -c и env -S (их видно по опции): eval, watch, trap, su -c и
// runuser -c. Скрипт с подстановкой shellcmd не разбирает и об этом не
// сообщает (кроме eval) — признак берётся отсюда
var scriptRunners = []string{"eval", "watch", "trap", "su", "runuser"}

// shellCommandOption короткая связка опций оболочки (после - или +) с c:
// -c, -lc, -ec; bash исполняет скрипт и с +c (+xc). Длинные опции (--…)
// shellScript разбирает раньше
func shellCommandOption(arg string) bool {
	return strings.Contains(arg[1:], "c")
}

// shellScript скрипт оболочки с -c: первый операнд после опций. Без -c
// первый операнд — файл скрипта, а слова за ним — его аргументы
// (bash x.sh -count=1). Связка, кончающаяся на o или O (-o pipefail,
// -eO extglob), и --rcfile, --init-file берут значение следующим словом;
// операнд, похожий на опцию (bash -c -- -x), — редкость, он не различается
func shellScript(args []string) (string, bool) {
	command := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--rcfile" || a == "--init-file":
			i++
		case strings.HasPrefix(a, "--"):
		case strings.HasPrefix(a, "-") || strings.HasPrefix(a, "+"):
			command = command || shellCommandOption(a)
			if strings.HasSuffix(a, "o") || strings.HasSuffix(a, "O") {
				i++
			}
		default:
			return a, command
		}
	}
	return "", false
}

// envSplitOption опция env -S (--split-string): значение исполняется как
// командная строка; -S бывает в связке после -i, -0, -v (env -iS'…')
func envSplitOption(arg string) bool {
	return strings.HasPrefix(arg, "--split-string") ||
		strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") && strings.HasPrefix(strings.TrimLeft(arg[1:], "0iv"), "S")
}

// scriptArgs слова вызова, которые исполнятся как скрипт: аргументы eval,
// watch, trap, su, runuser и env с -S, скрипт оболочки с -c; false — вызов
// скрипт из аргументов не исполняет
func scriptArgs(c shellcmd.Call) ([]string, bool) {
	switch {
	case slices.Contains(scriptRunners, c.Name):
		return c.Args, true
	case c.Name == "env" && slices.ContainsFunc(c.Args, envSplitOption):
		return c.Args, true
	case shellcmd.IsShell(c.Name):
		if script, ok := shellScript(c.Args); ok {
			return []string{script}, true
		}
	}
	return nil, false
}

// scriptNesting вложенность скриптов вызова: sh -c (в Via — «-c») и
// eval, watch, trap, su, runuser
func scriptNesting(via []string) int {
	n := 0
	for _, v := range via {
		if v == "-c" || slices.Contains(scriptRunners, v) {
			n++
		}
	}
	return n
}

// parsedCalls проверяет вызовы, найденные полным разбором оболочки
// (akit/shellcmd): обёртки с опциями, sh -c, eval, watch, trap, su -c,
// here-doc и here-string на вход оболочке, find -exec, имя из переменной
// (K=kill; $K), $'kill', тела функций и coproc. Ошибка разбора — отказ:
// вызывается только для команд со словом kill, а kill за ошибкой (глубже
// предела вложенности akit, в неразобранном вложенном скрипте) не виден
func (s *killScan) parsedCalls() error {
	calls, err := shellcmd.Calls(s.command)
	if err != nil {
		s.tool.Logger().Warn("kill_guard: shellcmd не разобрал команду со словом kill, отказ", "error", err)
		s.addReason("разбор оболочки не прошёл (" + err.Error() + "): проверить сигналы в команде нельзя")
	}
	for _, c := range calls {
		if s.signalWord {
			s.paramProcSubst(c.Raw)
		}
		if err := s.call(c, calls); err != nil {
			return err
		}
	}
	return nil
}

// paramProcSubst отказ на <(…) или >(…) в операнде ${…} вызова; снимается,
// когда akit найдёт вызовы в операнде ${…}
func (s *killScan) paramProcSubst(raw string) {
	found, err := paramProcSubst(raw)
	if err != nil {
		s.tool.Logger().Warn("kill_guard: текст вызова не разобрался, отказ", "error", err)
		s.addReason("разбор вызова не прошёл (" + err.Error() + "): проверить <(…) внутри ${…} нельзя")
	}
	if found {
		s.addReason("<(…) или >(…) внутри ${…} (echo ${X:-<(kill …)}): разбор оболочки вызовы там не видит, проверить сигналы нельзя")
	}
}

// call проверяет один вызов из разбора оболочки; calls — все вызовы
// команды: среди них ищется xargs, подающий ввод в скрипт вызова
func (s *killScan) call(c shellcmd.Call, calls []shellcmd.Call) error {
	if scriptNesting(c.Via) > maxScriptNesting {
		s.addReason(fmt.Sprintf("вложенность sh -c и eval глубже %d: проверить сигналы внутри нельзя", maxScriptNesting))
		return nil
	}
	if !s.signalWord {
		return s.signal(c)
	}
	script, runner := scriptArgs(c)
	fed := ""
	if runner {
		fed = fedIntoScript(c, calls)
	}
	switch {
	case c.ScriptInput:
		// оболочка исполняет текст из ввода или <(…) (bash <(…), source
		// <(…), … | bash -): shellcmd его не читает, kill в нём не виден
		s.addReason(scriptInputReason(c.Name))
	case slices.ContainsFunc(script, computedArg):
		s.addReason(c.Name + " исполняет скрипт с подстановкой $ или ` (eval \"$(…)\", sh -c \"$X\"): текст станет известен только при запуске, проверить его нельзя")
	case fed != "":
		s.addReason(c.Name + " получает скрипт из ввода через " + fed + " (-I, {}): проверить его до запуска нельзя")
	case c.Name == "parallel":
		// цели parallel берёт из списка ::: или ввода, а команду без :::
		// shellcmd не снимает (pgrep foo | parallel kill): сигнал — слово
		// среди аргументов parallel
		for _, a := range c.Args {
			if err := s.check(filepath.Base(a), nil, "parallel"); err != nil {
				return err
			}
		}
	}
	return s.signal(c)
}

// signal проверяет вызов как сигнал: kill, pkill, killall или имя,
// вычисляемое при запуске. Такое имя — отказ, если в тексте вызова есть
// слово kill ($(echo kill) 1, $X kill 1); kill внутри чужого слова
// (-r raw-process-kill) — не повод. Подстановку команды в имени ($(…),
// `…`) shellcmd отдаёт пустым именем, раскрытие параметра — его текстом с
// $, без значения (K=kill; sudo $K -9 -1 даёт вызов «$K» за sudo). Имя из
// раскрытия переменной или массива ($K, "${a[@]}") — отказ, если в
// команде им присвоено слово сигнала (a=(kill -9 -1)): слово kill стоит в
// присваивании, а не в тексте вызова
func (s *killScan) signal(c shellcmd.Call) error {
	if c.Name == "sudo" && sudoDashAssignment(c.Args) && signalWordRe.MatchString(unquoted(c.Raw)) {
		// снимается, когда akit снимет присваивание за sudo --
		s.addReason("присваивание за sudo -- (sudo -- X=1 kill …): разбор оболочки принял его за имя команды, проверить сигналы нельзя")
	}
	if c.Name == "" || strings.Contains(c.Name, "$") {
		switch {
		case signalWordRe.MatchString(unquoted(c.Raw)):
			s.addReason("имя команды вычисляется при запуске ($(…), `…` или $X) рядом с kill: проверить, что исполнится, нельзя")
		case arraySignal(c.Name, s.command):
			s.addReason("имя команды — раскрытие переменной или массива, которым в команде присвоено слово kill (a=(kill …); \"${a[@]}\"): проверить, что исполнится, нельзя")
		}
		return nil
	}
	args := make([]word, 0, len(c.Args))
	for _, a := range c.Args {
		args = append(args, word{text: a, literal: !strings.ContainsAny(a, "$`")})
	}
	via := ""
	if slices.Contains(c.Via, "xargs") {
		via = "xargs"
	}
	return s.check(c.Name, args, via)
}

// fedIntoScript обёртка, которая подставляет ввод в скрипт вызова c:
// parallel всегда ({} или ввод в конце), xargs — с -I, -i или --replace
func fedIntoScript(c shellcmd.Call, calls []shellcmd.Call) string {
	if slices.Contains(c.Via, "parallel") {
		return "parallel"
	}
	if slices.ContainsFunc(xargsOptions(c, calls), replaceOption) {
		return "xargs"
	}
	return ""
}

// replaceOption опция xargs, подставляющая ввод в аргументы команды
func replaceOption(opt string) bool {
	return strings.HasPrefix(opt, "-I") || strings.HasPrefix(opt, "-i") || strings.HasPrefix(opt, "--replace")
}

// xargsOptions опции xargs, за которым исполняется c. Обёртки shellcmd
// снимает, но отдельным вызовом отдаёт только голову цепочки (sudo xargs
// nice sh -c: вызовы sudo и sh, а xargs и nice — в Via вызова sh). Опции —
// слова головы между xargs и следующей за ним командой цепочки (в Via или
// сам c); голова — вызов с тем же текстом и самым коротким Via
func xargsOptions(c shellcmd.Call, calls []shellcmd.Call) []string {
	k := slices.Index(c.Via, "xargs")
	if k < 0 {
		return nil
	}
	next := c.Name
	if k+1 < len(c.Via) {
		next = c.Via[k+1]
	}
	head := c
	for _, x := range calls {
		if x.Raw == c.Raw && len(x.Via) < len(head.Via) {
			head = x
		}
	}
	args := head.Args
	if head.Name != "xargs" {
		args = args[slices.Index(args, "xargs")+1:]
	}
	i := slices.IndexFunc(args, func(a string) bool { return filepath.Base(a) == next })
	if i < 0 {
		return nil
	}
	return args[:i]
}

// scriptInputReason причина отказа для оболочки, исполняющей текст из ввода
func scriptInputReason(name string) string {
	return name + " читает скрипт из ввода (… | " + name + " или <(…)): проверить его до запуска нельзя"
}

// addReason добавляет причину отказа без повторов: один сигнал виден и
// вызовом, и через обёртку (parallel)
func (s *killScan) addReason(reason string) {
	if !slices.Contains(s.reasons, reason) {
		s.reasons = append(s.reasons, reason)
	}
}

// check проверяет одну простую команду, если она шлёт сигнал; via — обёртка,
// подающая аргументы из ввода (xargs, parallel)
func (s *killScan) check(name string, args []word, via string) error {
	if !slices.Contains(signalNames, name) {
		return nil
	}
	if s.procs == nil {
		procs, err := s.tool.listProcs()
		if err != nil {
			return err
		}
		s.procs = procs
	}
	var reason string
	group := false
	switch {
	case via != "":
		reason = name + " через " + via + ": цели приходят из ввода или списка, проверить их до запуска нельзя"
	case name == "killall5":
		reason = "killall5 шлёт сигнал всем процессам системы вне своей сессии: погасит все сессии Claude"
	case name == "kill":
		reason, group = s.tool.checkKill(args, s.procs)
	default:
		reason = s.tool.checkPattern(name, args, s.command, s.procs)
	}
	if reason != "" {
		s.addReason(reason)
	}
	if group && !slices.Contains(s.groupReasons, reason) {
		s.groupReasons = append(s.groupReasons, reason)
	}
	return nil
}

// checkKill проверяет цели kill: только литеральные PID и job spec, ни один
// не задевает сессию. Сигнал 0 лишь проверяет, жив ли процесс, — цели любые.
// group — отказ из-за сигнала группе процессов (PID меньше -1)
func (t *KillGuardTool) checkKill(args []word, procs []procInfo) (reason string, group bool) {
	// сигнал 0 пропускается без отрицательных целей: kill -s 0 -9 -1
	// встроенный kill bash шлёт 0, а /bin/kill может прочесть -9 сигналом
	targets, zero := killOptions(args)
	if zero && !slices.ContainsFunc(targets, func(w word) bool { return strings.HasPrefix(w.text, "-") }) {
		return "", false
	}
	ancestors := t.ancestors(procs)
	for _, w := range targets {
		if !w.literal {
			return fmt.Sprintf("kill %s: PID вычисляется при запуске, проверить его заранее нельзя", w.text), false
		}
		if strings.HasPrefix(w.text, "%") {
			continue
		}
		pid, err := strconv.Atoi(w.text)
		if err != nil {
			return fmt.Sprintf("kill %s: цель не PID (%v)", w.text, err), false
		}
		switch {
		case pid < -1:
			return fmt.Sprintf("kill %d: сигнал группе процессов %d целиком; кто в ней, проверка не сверяет", pid, -pid), true
		case pid == -1:
			return "kill -1: сигнал всем процессам пользователя — всем сессиям Claude и терминалам", false
		case pid == 0:
			return "kill 0: сигнал своей группе процессов — той, где идёт сама эта команда", false
		}
		if reason := t.dangerous(pid, procs, ancestors); reason != "" {
			return fmt.Sprintf("kill %d: %s", pid, reason), false
		}
	}
	return "", false
}

// patternArgs разобранные аргументы pkill или killall
type patternArgs struct {
	full, exact, isRegex bool
	// noSignal — справка, версия, список сигналов или сигнал 0: никто не
	// будет завершён
	noSignal bool
	patterns []string
}

// parsePatternArgs разбирает флаги и шаблоны; непустая строка — причина отказа
func parsePatternArgs(name string, args []word) (patternArgs, string) {
	pa := patternArgs{isRegex: name == "pkill"}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !a.literal {
			return pa, fmt.Sprintf("%s %s: шаблон вычисляется при запуске, проверить его заранее нельзя", name, a.text)
		}
		switch {
		case a.text == "-f" || a.text == "--full":
			pa.full = true
		case a.text == "-x" || a.text == "--exact" || a.text == "-e":
			pa.exact = true
		case a.text == "-r" || a.text == "--regexp":
			pa.isRegex = true
		case noSignalOption(name, a.text):
			pa.noSignal = true
		case optionTakesValue(name, a.text):
			i++
			pa.noSignal = pa.noSignal || signalZero(name, a.text, args, i)
		case strings.HasPrefix(a.text, "-"):
			// сигнал или флаг без значения
		default:
			pa.patterns = append(pa.patterns, a.text)
		}
	}
	if name == "pkill" && len(pa.patterns) > 1 {
		pa.patterns = pa.patterns[len(pa.patterns)-1:]
	}
	if len(pa.patterns) == 0 && !pa.noSignal {
		return pa, name + " без шаблона выбирает процессы только по фильтру (-u, -P, -t, -g, -s): pkill -u $USER гасит все сессии разом"
	}
	return pa, ""
}

// signalZero — опция сигнала opt со значением args[i] задаёт сигнал 0 (у
// pkill -s — сессия, не сигнал)
func signalZero(name, opt string, args []word, i int) bool {
	isSignal := opt == "--signal" || name == "killall" && opt == "-s"
	return isSignal && i < len(args) && args[i].text == "0"
}

// noSignalOption флаг справки, версии, списка сигналов или сигнала 0:
// утилита ничего не завершит
func noSignalOption(name, opt string) bool {
	switch opt {
	case "--help", "-V", "--version", "-0", "--signal=0":
		return true
	case "-l", "--list":
		return name == "killall"
	}
	return false
}

// regexp шаблон в том виде, как его сопоставляет утилита: killall без -r
// сравнивает имя целиком, pkill -x — тоже
func (pa patternArgs) regexp(p string) (*regexp.Regexp, error) {
	expr, exact := p, pa.exact
	if !pa.isRegex {
		expr, exact = regexp.QuoteMeta(p), true
	}
	if exact {
		expr = "^(?:" + expr + ")$"
	}
	return regexp.Compile(expr)
}

// checkPattern прикидывает, что заденет pkill или killall сейчас: шаблон
// сверяется с теми же полями процессов, что у самой утилиты
func (t *KillGuardTool) checkPattern(name string, args []word, command string, procs []procInfo) string {
	pa, reason := parsePatternArgs(name, args)
	if reason != "" || pa.noSignal {
		return reason
	}
	ancestors := t.ancestors(procs)
	for _, p := range pa.patterns {
		re, err := pa.regexp(p)
		if err != nil {
			// утилита сама откажется от такого шаблона
			t.Logger().Debug("kill pattern does not compile", "pattern", p, "error", err)
			continue
		}
		if pa.full && re.MatchString(command) {
			return fmt.Sprintf("%s -f %q совпадает с текстом самой команды и убьёт оболочку, которая её исполняет; шаблон вида '[x]yz' себя не ловит", name, p)
		}
		for _, pr := range procs {
			subject := pr.comm
			if pa.full {
				subject = pr.cmdline
			}
			if !re.MatchString(subject) {
				continue
			}
			if reason := t.dangerous(pr.pid, procs, ancestors); reason != "" {
				return fmt.Sprintf("%s %q задевает PID %d: %s", name, p, pr.pid, reason)
			}
		}
	}
	return ""
}

// dangerous возвращает причину, по которой процесс трогать нельзя
func (t *KillGuardTool) dangerous(pid int, procs []procInfo, ancestors map[int]bool) string {
	if pid <= 1 {
		return "PID 1 — init (systemd), на нём держится система"
	}
	if pid == t.selfPID || ancestors[pid] {
		return "предок этой сессии Claude (claude, оболочка, терминал или менеджер сессии)"
	}
	for _, p := range procs {
		if p.pid == pid && protectedComms[p.comm] {
			return fmt.Sprintf("%s — на нём держится сессия пользователя", p.comm)
		}
	}
	return ""
}

// procInfo процесс по /proc
type procInfo struct {
	pid, ppid     int
	comm, cmdline string
}

// listProcs читает все процессы из procRoot. Процесс, завершившийся между
// ReadDir и чтением его файлов, пропускается
func (t *KillGuardTool) listProcs() ([]procInfo, error) {
	entries, err := os.ReadDir(t.procRoot)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", t.procRoot, err)
	}
	procs := make([]procInfo, 0, len(entries))
	for _, e := range entries {
		if name := e.Name(); name[0] < '0' || name[0] > '9' {
			continue // не процесс: self, sys, meminfo
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			t.Logger().Debug("proc entry skipped", "name", e.Name(), "error", err)
			continue
		}
		p, err := readProc(filepath.Join(t.procRoot, e.Name()), pid)
		if err != nil {
			t.Logger().Debug("process skipped", "pid", pid, "error", err)
			continue
		}
		procs = append(procs, p)
	}
	return procs, nil
}

// readProc читает stat и cmdline одного процесса
func readProc(dir string, pid int) (procInfo, error) {
	stat, err := os.ReadFile(filepath.Join(dir, "stat"))
	if err != nil {
		return procInfo{}, fmt.Errorf("read stat: %w", err)
	}
	// comm в stat может содержать пробелы и скобки: поля после последней ')'
	s := string(stat)
	start, end := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
	if start < 0 || end < start {
		return procInfo{}, fmt.Errorf("stat без comm: %q", s)
	}
	fields := strings.Fields(s[end+1:])
	if len(fields) < 2 {
		return procInfo{}, fmt.Errorf("stat без ppid: %q", s)
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return procInfo{}, fmt.Errorf("ppid: %w", err)
	}
	cmdline, err := os.ReadFile(filepath.Join(dir, "cmdline"))
	if err != nil {
		return procInfo{}, fmt.Errorf("read cmdline: %w", err)
	}
	return procInfo{
		pid:     pid,
		ppid:    ppid,
		comm:    s[start+1 : end],
		cmdline: strings.TrimSpace(strings.ReplaceAll(string(cmdline), "\x00", " ")),
	}, nil
}

// ancestors цепочка родителей хука до init
func (t *KillGuardTool) ancestors(procs []procInfo) map[int]bool {
	parent := make(map[int]int, len(procs))
	for _, p := range procs {
		parent[p.pid] = p.ppid
	}
	out := map[int]bool{}
	for pid := parent[t.selfPID]; pid > 1 && !out[pid]; pid = parent[pid] {
		out[pid] = true
	}
	return out
}

// optionTakesValue флаги pkill и killall, за которыми идёт отдельное значение
func optionTakesValue(name, opt string) bool {
	if name == "pkill" {
		switch opt {
		case "-u", "-U", "-g", "-G", "-P", "-s", "-t", "--signal", "--euid", "--uid",
			"--pgroup", "--group", "--parent", "--session", "--terminal", "--ns", "--nslist":
			return true
		}
		return false
	}
	switch opt {
	case "-u", "--user", "-s", "--signal", "-o", "--older-than", "-y", "--younger-than", "-n", "--ns":
		return true
	}
	return false
}

// nameVarRe переменная, которую раскрывает имя вызова: ${a[@]}, ${a[0]},
// $a, ${a}, $P$a
var nameVarRe = regexp.MustCompile(`\$\{?([A-Za-z_][A-Za-z0-9_]*)`)

// arraySignal — имя вызова раскрывает переменную или массив, а в команде
// им присвоено значение со словом сигнала: a=(kill -9 -1), a+=(…),
// a[0]=kill, declare -a a=(…); a=(kill); $a -9 -1 и K=kill; $P$K 1
// исполнят kill. shellcmd значение массива в имя не подставляет.
// Присваивание ищется в тексте всей команды: оно бывает и в теле heredoc
// для оболочки
func arraySignal(name, command string) bool {
	for _, m := range nameVarRe.FindAllStringSubmatch(name, -1) {
		assign := regexp.MustCompile(`(^|[^A-Za-z0-9_])` + m[1] + `(\[[^\]]*\])?\+?=(\([^)]*\)|\S*)`)
		for _, a := range assign.FindAllStringSubmatch(command, -1) {
			if signalWordRe.MatchString(unquoted(a[3])) {
				return true
			}
		}
	}
	return false
}

// sudoDashAssignment — за -- в аргументах sudo стоит присваивание (sudo --
// X=1 kill): shellcmd (akit v1.76.2) его не снимает и отдаёт именем вызова
// X=1 (X=a/b — именем b), а kill — аргументом
func sudoDashAssignment(args []string) bool {
	i := slices.Index(args, "--")
	return i >= 0 && i+1 < len(args) && assignmentRe.MatchString(args[i+1])
}

var assignmentRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// paramProcSubst — в операнде ${…} вызова стоит <(…) или >(…): ${X:-<(…)},
// ${X#<(…)}, ${X/a/<(…)}. mvdan/sh держит операнд литералом, и shellcmd
// вызовы в нём не находит (akit shellcmd-param-operand-procsubst). Разбирается
// текст вызова, а не всей команды: тело heredoc в него не входит, а вызовы
// из sh -c и heredoc на вход оболочке shellcmd отдаёт отдельно
func paramProcSubst(raw string) (bool, error) {
	file, err := syntax.NewParser().Parse(strings.NewReader(raw), "")
	if err != nil {
		return false, fmt.Errorf("разбор текста вызова: %w", err)
	}
	found := false
	syntax.Walk(file, func(node syntax.Node) bool {
		if param, ok := node.(*syntax.ParamExp); ok && slices.ContainsFunc(paramOperands(param), literalProcSubst) {
			found = true
		}
		return true
	})
	return found, nil
}

// paramOperands слова-операнды раскрытия: ${X:-слово}, ${X/образец/замена}
func paramOperands(param *syntax.ParamExp) []*syntax.Word {
	var operands []*syntax.Word
	if param.Exp != nil {
		operands = append(operands, param.Exp.Word)
	}
	if param.Repl != nil {
		operands = append(operands, param.Repl.Orig, param.Repl.With)
	}
	return operands
}

// literalProcSubst — в литерале слова есть <( или >(
func literalProcSubst(w *syntax.Word) bool {
	return w != nil && slices.ContainsFunc(w.Parts, func(p syntax.WordPart) bool {
		lit, ok := p.(*syntax.Lit)
		return ok && (strings.Contains(lit.Value, "<(") || strings.Contains(lit.Value, ">("))
	})
}

// killOptions отделяет опции kill от целей так же, как встроенный kill
// bash: -s и -n со значением повторяются, и сигнал задаёт последний
// (kill -0 -s 9 -- -1 шлёт 9, не 0); -SIG (-9, -KILL) — сигнал только
// первым таким словом, дальше -N — цель (группа процессов); -- кончает
// опции. zero — сигнал задан и равен 0. -l, -L, --list и --table печатают
// список сигналов: целей нет
func killOptions(args []word) (targets []word, zero bool) {
	targets = args
	sawSignal := false
	record := func(isZero bool) {
		sawSignal = true
		zero = isZero
	}
options:
	for len(targets) > 0 && targets[0].literal && strings.HasPrefix(targets[0].text, "-") {
		opt := targets[0].text
		switch {
		case opt == "-l" || opt == "-L" || opt == "--list" || opt == "--table":
			return nil, false
		case opt == "--":
			targets = targets[1:]
			break options
		case opt == "-s" || opt == "-n" || opt == "--signal":
			if len(targets) < 2 {
				return nil, false
			}
			record(targets[1].text == "0")
			targets = targets[2:]
		case strings.HasPrefix(opt, "--signal="):
			record(opt == "--signal=0")
			targets = targets[1:]
		case !sawSignal:
			record(opt == "-0")
			targets = targets[1:]
		default:
			break options
		}
	}
	return targets, zero
}
