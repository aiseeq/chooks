package tools

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/aiseeq/akit/shellcmd"
	"github.com/aiseeq/chooks/internal/core"
)

// protectedComms процессы, без которых падает сессия пользователя целиком:
// init и менеджеры служб, шина, графическая оболочка, звук, вход по ssh
var protectedComms = map[string]bool{
	"systemd": true, "init": true, "systemd-logind": true,
	"dbus-broker": true, "sshd": true, "sshd-session": true,
	"Xwayland": true, "Xorg": true, "kwin_wayland": true, "kwin_x11": true,
	"plasmashell": true, "gnome-shell": true, "sway": true, "Hyprland": true,
	"sddm": true, "gdm": true, "pipewire": true, "wireplumber": true,
}

// KillGuardTool блокирует kill, pkill и killall, цель которых нельзя
// проверить до запуска или которые заденут саму сессию: PID 1, systemd,
// предков хука (claude, терминал), оболочку, исполняющую команду
type KillGuardTool struct {
	*BaseTool
	procRoot    string
	selfPID     int
	resolvePath func(string) (string, error)
	lstatPath   func(string) (fs.FileInfo, error)
}

// NewKillGuardToolForPID protects the process tree of this hook request.
func NewKillGuardToolForPID(config core.ToolConfig, logger core.Logger, pid int) (*KillGuardTool, error) {
	return &KillGuardTool{
		BaseTool:    NewBaseTool("kill_guard", config.Enabled, core.ShellTools, logger),
		procRoot:    "/proc",
		selfPID:     pid,
		resolvePath: filepath.EvalSymlinks,
		lstatPath:   os.Lstat,
	}, nil
}

const killGuardHint = "Найди PID отдельной командой (ps -o pid,ppid,comm,args -p … или pgrep -a '[x]yz'), посмотри на вывод и убей литералом: kill 12345"
const protectedProcessHint = "Процесс защищён: не посылай ему сигнал. Если это тестовый процесс, заверши его штатным механизмом теста; для сессии используй её штатное управление"
const protectedProcessMarker = "\x00protected-process"

// killGroupHint подсказка к отказу на сигнал группе: отдельные PID группу не
// заменяют — процесс, который группа породит после перечисления, сигнал не получит
const killGroupHint = "Сигнал отдельным PID — не то же, что группе: процессы, которые появятся в группе после перечисления, его не получат. Перечисли группу отдельной командой (pgrep -a -g 12345), посмотри на вывод, убей её PID литералами (kill 12346 12347) и проверь группу снова"

// ValidateTool проверяет команды, посылающие сигналы процессам
func (t *KillGuardTool) ValidateTool(ctx context.Context, input *core.ToolInput) (*core.ValidationResult, error) {
	if !t.IsEnabled() || !core.IsShellTool(input.ToolName) {
		return &core.ValidationResult{IsValid: true}, nil
	}
	text := unquoted(input.Command)
	killText := strings.Contains(text, "kill")
	if !killText && !sessionToolWordRe.MatchString(text) && !run0WordRe.MatchString(text) && !strings.Contains(text, cgroupRoot) {
		return &core.ValidationResult{IsValid: true}, nil
	}

	command := input.Command
	if strings.Contains(unquoted(command), "systemd-run") {
		var err error
		if command, err = rewriteSystemdRunShell(command); err != nil {
			t.Logger().Warn("kill_guard: systemd-run --shell не переписан в тексте, проверен текстом systemdRunShellCommand", "error", err)
		}
	}
	busAddress := requestSessionBusAddress(ctx)
	scan := killScan{tool: t, command: command, source: input.Command, killText: killText, busAddress: busAddress,
		signalWord: signalWordRe.MatchString(text), zsh: zshWordRe.MatchString(text),
		anyBus: busAddressRe.MatchString(text)}
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

// hints подсказки к отказам без повторов, по порядку отказов: про группу —
// к отказу на сигнал группе, своя у средства (systemctl, tmux, fuser) — к
// его отказу, обычная про литеральный PID — к прочим
func (s *killScan) hints() []string {
	var out []string
	for _, r := range s.reasons {
		hint := killGuardHint
		switch {
		case slices.Contains(s.groupReasons, r):
			hint = killGroupHint
		case s.toolHints[r] != "":
			hint = s.toolHints[r]
		}
		if !slices.Contains(out, hint) {
			out = append(out, hint)
		}
	}
	return out
}

// unquoted команда без кавычек и \: k\ill и 'k'ill оболочка исполнит как kill
func unquoted(command string) string {
	return strings.NewReplacer(`\`, "", `'`, "", `"`, "").Replace(command)
}

// killScan проверка вызовов команды, найденных разбором оболочки
type killScan struct {
	tool       *KillGuardTool
	busAddress string
	// command — команда целиком, где systemd-run --shell заменён на
	// systemd-run --pipe bash (rewriteSystemdRunShell): её разбирают
	// правила; source — как её прислал агент: этот текст стоит в cmdline
	// оболочки, и pkill -f по нему убьёт эту оболочку
	command, source string
	procs           []procInfo
	reasons         []string
	// groupReasons — те из reasons, что про сигнал группе процессов (kill -N)
	groupReasons []string
	// toolHints — подсказка к отказу на средство с узкой целью не PID
	// (systemctl, tmux, fuser): причина → подсказка
	toolHints map[string]string
	// signalWord — в команде без кавычек есть отдельное слово kill, pkill
	// или killall: скрипт, который станет известен только при запуске, рядом
	// с ним — отказ. Подстрока kill (skills) — не повод
	signalWord bool
	// zsh — в команде есть слово zsh: подстановка процесса бывает и =(…)
	zsh bool
	// killText — в команде без кавычек есть подстрока kill. Без неё сторож
	// смотрит только loginctl, systemctl и fuser, и ошибка разбора — не отказ
	killText bool
	// anyBus — команда задаёт адрес шины D-Bus (busAddressRe): менеджер
	// systemd у systemctl и вызовов D-Bus проверяется по обоим
	anyBus bool
	// stdin — ввод участков текста, к которому отнесены места вызовов
	// (stdinOf): команды, а за pkexec — текста за обёрткой
	stdin *stdinScope
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
	case (c.Name == "sudo" || c.Name == "doas") && privilegedInteractiveShell(c.Args):
		return nil, true
	case c.Name == "script":
		return scriptCommandArgs(c.Args)
	case c.Name == "env" && slices.ContainsFunc(c.Args, envSplitOption):
		return c.Args, true
	case shellcmd.IsShell(c.Name):
		if script, ok := shellScript(c.Args); ok {
			return []string{script}, true
		}
		for _, a := range c.Args {
			if (strings.HasPrefix(a, "-") || strings.HasPrefix(a, "+")) && shellCommandOption(a) {
				return nil, true
			}
		}
	}
	return nil, false
}

func scriptCommandArgs(args []string) ([]string, bool) {
	for i, arg := range args {
		if arg == "--command" || arg == "-c" || strings.HasPrefix(arg, "--command=") {
			if strings.Contains(arg, "=") {
				return []string{strings.SplitN(arg, "=", 2)[1]}, true
			}
			if i+1 < len(args) {
				return []string{args[i+1]}, true
			}
			return nil, true
		}
		if strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") && strings.Contains(arg[1:], "c") {
			if i+1 < len(args) {
				return []string{args[i+1]}, true
			}
			return nil, true
		}
	}
	return nil, false
}

// parsedCalls проверяет вызовы, найденные полным разбором оболочки
// (akit/shellcmd): обёртки с опциями, sh -c, eval, watch, trap, su -c,
// here-doc и here-string на вход оболочке, find -exec, имя из переменной
// (K=kill; $K), $'kill', тела функций и coproc. Ошибка разбора — отказ:
// вызывается только для команд со словом kill, а kill за ошибкой (глубже
// предела вложенности akit, в неразобранном вложенном скрипте) не виден
func (s *killScan) parsedCalls() error {
	calls, err := shellcmd.Calls(s.command)
	if err != nil && s.killText {
		s.tool.Logger().Warn("kill_guard: shellcmd не разобрал команду со словом kill, отказ", "error", err)
		s.addReason("разбор оболочки не прошёл (" + err.Error() + "): проверить сигналы в команде нельзя")
	}
	calls = s.withWrappedFeeds(calls)
	if s.signalWord && s.procSubstAfterParam(s.command) {
		s.addReason(procSubstAfterParamReason)
	}
	for i := range calls {
		c := &calls[i]
		if s.signalWord && s.procSubstAfterParam(c.Raw) {
			s.addReason(procSubstAfterParamReason)
		}
		if err := s.call(*c, calls); err != nil {
			return err
		}
	}
	return nil
}

// procSubstAfterParamRe <( или >( где-либо после ${ в тексте команды.
// Временное правило до akit shellcmd-param-operand-procsubst: разбор
// оболочки держит операнд ${…} литералом и вызовы в нём (${X:-<(kill …)})
// не находит — ни на верхнем уровне, ни во вложенном скрипте (bash -c,
// eval, heredoc на вход оболочке). Точные проверки операнда пропускали
// } перед <( (${X:-${Z}<(…)}, "}", '}', \}), поэтому ищется текст целиком:
// ложный отказ на команде со словом kill, где <( просто стоит после ${,
// допустим. Снять после akit shellcmd-param-operand-procsubst
var procSubstAfterParamRe = regexp.MustCompile(`(?s)\$\{.*[<>]\(`)

// zshProcSubstAfterParamRe то же для команды со словом zsh: у zsh
// подстановка процесса — и =(…) (zsh -c 'echo ${X:-=(kill …)}'). Без zsh
// =( не ищется: a=(…) после ${ — частый массив bash
var zshProcSubstAfterParamRe = regexp.MustCompile(`(?s)\$\{.*[<>=]\(`)

// zshWordRe отдельное слово zsh (не .zshrc)
var zshWordRe = regexp.MustCompile(`(^|[^[:alnum:]_.-])zsh([^[:alnum:]_.-]|$)`)

// procSubstAfterParam — в тексте без кавычек и \ после ${ стоит <( или >(.
// Текст без кавычек ловит ${ и <(, собранные кавычками во вложенном скрипте
// ($ и { в соседних кавычках в bash -c '…', "\${X:-<(…)}"), и всё, что ловит исходный
// текст: кавычки и \ из него только убираются. Текст вызова (c.Raw) —
// вложенный скрипт после раскрытия $'…' (bash -c $'echo ${X:-\x3c(…)}')
func (s *killScan) procSubstAfterParam(text string) bool {
	re := procSubstAfterParamRe
	if s.zsh {
		re = zshProcSubstAfterParamRe
	}
	return re.MatchString(unquoted(text))
}

// procSubstAfterParamReason причина временного отказа на <(…) после ${
const procSubstAfterParamReason = "в команде со словом kill после ${ стоит <(…) или >(…) (в zsh и =(…)): если они внутри ${…} (echo ${X:-<(kill …)}), разбор оболочки вызовы там не видит, проверить сигналы нельзя. Временное правило до починки разбора (akit shellcmd-param-operand-procsubst): вынеси <(…) из команды с kill или раздели команду"

// call проверяет один вызов из разбора оболочки; calls — все вызовы
// команды: среди них ищется xargs, подающий ввод в скрипт вызова
func (s *killScan) call(c shellcmd.Call, calls []shellcmd.Call) error {
	if sudoAssignment(c, calls) {
		return nil
	}
	if reason := s.depthReason(c, calls); reason != "" {
		s.addToolReason(reason, depthHint, false)
		return nil
	}
	if err := s.cgroupWrite(c); err != nil {
		return err
	}
	if rewritten, err := s.rewrittenCall(c, calls); rewritten {
		return err
	}
	if err := s.serviceCommands(c, calls); err != nil {
		return err
	}
	feed := s.literalFeed(c, calls)
	if reason, hint := s.systemdRunSignalReason(c, calls, feed != nil); reason != "" {
		s.addToolReason(reason, hint, false)
	}
	if err := s.callNested(calls, feed); err != nil {
		return err
	}
	if err := s.feedCall(c, calls); err != nil {
		return err
	}
	if !s.signalWord {
		if s.implicitElevatedScriptInput(c) {
			s.addReason(scriptInputReason(c.Name))
		}
		return s.signal(c, calls)
	}
	script, runner := scriptArgs(c)
	if err := s.checkScriptCall(c, calls, script, runner); err != nil {
		return err
	}
	return s.signal(c, calls)
}

func (s *killScan) callNested(calls, feed []shellcmd.Call) error {
	for i := range feed {
		if err := s.call(feed[i], slices.Concat(calls, feed)); err != nil {
			return err
		}
	}
	return nil
}

func (s *killScan) checkScriptCall(c shellcmd.Call, calls []shellcmd.Call, script []string, runner bool) error {
	fed := s.fedScriptInput(c, calls, runner)
	switch {
	case c.ScriptInput:
		if harmlessImplicitXargsScriptInput(c, s) {
			break
		}
		// оболочка исполняет текст из ввода или <(…) (bash <(…), source
		// <(…), … | bash -): shellcmd его не читает, kill в нём не виден
		s.addReason(scriptInputReason(c.Name))
	case slices.ContainsFunc(script, computedArg):
		s.addReason(c.Name + " исполняет скрипт с подстановкой $ или ` (eval \"$(…)\", sh -c \"$X\"): текст станет известен только при запуске, проверить его нельзя")
	case fed != "":
		s.addReason(c.Name + " получает скрипт из ввода через " + fed + " (-I, {}): проверить его до запуска нельзя")
	}
	return nil
}

func (s *killScan) fedScriptInput(c shellcmd.Call, calls []shellcmd.Call, runner bool) string {
	fed := ""
	if runner || (slices.Contains(c.Via, "xargs") || slices.Contains(c.Via, "parallel")) &&
		slices.Contains(c.Via, "-c") && implicitShellArgRe.MatchString(c.Raw) {
		fed = fedIntoScript(c, calls)
		if fed == "xargs" && !slices.ContainsFunc(xargsOptions(c, calls), replaceOption) &&
			harmlessImplicitXargsInput(c, s.inputOf(c)) {
			fed = ""
		}

	}
	return fed
}

// implicitElevatedScriptInput covers an unread argument stream that xargs or
// parallel appends to a privileged -c invocation. A visible literal stream
// is handled by the ordinary script check; unknown input fails closed.
func (s *killScan) implicitElevatedScriptInput(c shellcmd.Call) bool {
	usesInput := scriptCommandInput(c) && scriptUsesImplicitArgument(c)
	if !afterXargs(c) || c.ScriptInput || !usesInput {
		return false
	}
	in := s.inputOf(c)
	if in.src != stdinPipe && in.src != stdinFile && in.src != stdinRedirect {
		return false
	}
	if harmlessImplicitXargsInput(c, in) {
		return false
	}
	if c.Name == "script" {
		return true
	}
	for _, wrapper := range c.Via {
		switch wrapper {
		case "pkexec", "sudo", "doas", "su", "run0", "runuser", "systemd-run":
			return true
		}
	}
	return false
}

// knownHarmlessInput recognizes a literal echo/printf producer in the input
// scope, so a known benign argument does not become a blanket refusal.
func knownHarmlessInput(in stdinSpan) bool {
	if in.feed == "" {
		return false
	}
	calls, err := shellcmd.Calls(in.feed)
	if err != nil || len(calls) == 0 {
		return false
	}
	producer := calls[0]
	if producer.Name != "echo" && producer.Name != "printf" {
		return false
	}
	value := echoedValue(producer)
	if value == "" || signalWordRe.MatchString(unquoted(value)) {
		return false
	}
	return true
}

// harmlessImplicitXargsInput preserves the known xargs exception. With no
// script operand, run0 and systemd-run still refuse even literal input as a
// script; when -c already has a script, that same input is only $0/arguments.
func harmlessImplicitXargsInput(c shellcmd.Call, in stdinSpan) bool {
	if !knownHarmlessInput(in) {
		return false
	}
	args, hasScript := scriptArgs(c)
	if hasScript && len(args) > 0 {
		return true
	}
	return !slices.Contains(c.Via, "run0") && !slices.Contains(c.Via, "systemd-run")
}

func harmlessImplicitXargsScriptInput(c shellcmd.Call, s *killScan) bool {
	// This exception applies only to the current shell call. Looking through
	// the whole call list lets a safe xargs sibling mask unrelated ScriptInput
	// calls elsewhere in the command.
	return afterXargs(c) && scriptCommandInput(c) && scriptUsesImplicitArgument(c) &&
		knownHarmlessInput(s.inputOf(c))
}

func (s *killScan) rewrittenCall(c shellcmd.Call, _ []shellcmd.Call) (bool, error) {
	inner, ok := rewrittenCommand(c)
	if !ok {
		return false, nil
	}
	if c.Name == "run0" {
		if err := s.run0Service(c); err != nil {
			return true, err
		}
	}
	return true, s.elevated(c, inner)
}

// pkexecValues опции pkexec со значением отдельным словом
var pkexecValues = []string{"user"}

// elevatedCommand команда, которую pkexec и machinectl shell исполнят от
// другого пользователя (как sudo, которое снимает разбор оболочки): слова
// после их опций, собранные в текст для разбора. run0 разбор снимает сам,
// как systemd-run
func elevatedCommand(c shellcmd.Call) (string, bool) {
	switch c.Name {
	case "machinectl":
		return machinectlCommand(c.Args)
	case "pkexec":
	case "doas":
		return doasCommand(c.Args)
	default:
		return "", false
	}
	if c.Name == "doas" {
		return "", false
	}
	i := 0
	for ; i < len(c.Args); i++ {
		a := c.Args[i]
		if a == "--" {
			i++
			break
		}
		if !strings.HasPrefix(a, "-") {
			break
		}
		name := strings.TrimLeft(a, "-")
		if !strings.Contains(name, "=") && slices.Contains(pkexecValues, name) {
			i++
		}
	}
	if i >= len(c.Args) {
		return "", false
	}
	return shellWords(c.Args[i:]), true
}

func doasCommand(args []string) (string, bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			args = args[i+1:]
			break
		}
		if !strings.HasPrefix(a, "-") {
			args = args[i:]
			break
		}
		switch a {
		case "-u", "--user", "-a", "--auth-type", "-C", "--config":
			i++
		}
		if i == len(args)-1 {
			return "", false
		}
	}
	if len(args) == 0 {
		return "", false
	}
	return shellWords(args), true
}

// shellWords слова команды текстом для разбора: слово с подстановкой — как
// в исходнике, литеральное — в одинарных кавычках
func shellWords(args []string) string {
	quoted := make([]string, 0, len(args))
	for _, a := range args {
		if strings.ContainsAny(a, "$`") {
			quoted = append(quoted, a)
			continue
		}
		quoted = append(quoted, "'"+strings.ReplaceAll(a, "'", `'\''`)+"'")
	}
	return strings.Join(quoted, " ")
}

// machinectlValues опции machinectl со значением (getopt systemd v259
// "-hp:P:als:H:M:qn:o:E:V" и длинные с required_argument)
var machinectlValues = []string{
	"p", "P", "s", "H", "M", "n", "o", "E",
	"property", "kill-whom", "signal", "host", "machine", "lines", "output", "verify", "runner", "format", "uid",
	"setenv", "max-addresses",
}

// machinectlCommand команда machinectl shell [[USER]@MACHINE [COMMAND …]] на
// этой машине (MACHINE пусто или .host, без -H и -M к другой машине):
// machined исполняет её слова без оболочки, как sudo. Опции после имени
// машины — уже команды (machinectl не переставляет их за глаголом shell).
// Без команды machinectl shell запускает оболочку, которая читает его ввод:
// текст — sh
func machinectlCommand(args []string) (string, bool) {
	var pos []string
	remote, done := false, false
	for i := 0; i < len(args) && len(pos) < 2; i++ {
		a := args[i]
		switch {
		case done || !strings.HasPrefix(a, "-") || a == "-":
			pos = append(pos, a)
		case a == "--":
			done = true
		default:
			name, value := machinectlOption(args, &i)
			remote = remote || name == "H" || name == "host" || (name == "M" || name == "machine") && !localMachine(value)
		}
		if len(pos) == 2 {
			args = args[i+1:]
		}
	}
	if len(pos) == 0 || pos[0] != "shell" || remote || len(pos) == 2 && !localMachine(pos[1]) {
		return "", false
	}
	if len(pos) == 1 || len(args) == 0 {
		return "sh", true
	}
	return shellWords(args), true
}

// machinectlOption опция machinectl в args[*i]: имя (длинное или буква со
// значением) и значение; *i сдвигается за значение отдельным словом
func machinectlOption(args []string, i *int) (name, value string) {
	a := args[*i]
	if strings.HasPrefix(a, "--") {
		n, v, inline := strings.Cut(a[2:], "=")
		if !inline && slices.Contains(machinectlValues, n) && *i+1 < len(args) {
			*i++
			v = args[*i]
		}
		return n, v
	}
	letter, rest, next := shortValue(a, machinectlValues)
	if next && *i+1 < len(args) {
		*i++
		rest = args[*i]
	}
	return letter, rest
}

// localMachine — машина machinectl ([USER@]MACHINE) — эта: .host или пусто
func localMachine(spec string) bool {
	_, machine, found := strings.Cut(spec, "@")
	if !found {
		machine = spec
	}
	return machine == "" || machine == ".host"
}

// elevated проверяет вызовы команды inner, которой проверяется вызов c
// (rewrittenCommand: pkexec, оболочка script, machinectl shell, Exec-свойство
// службы): Via у них — с c впереди. Перенаправления ввода c
// разбираются вместе с inner (inputRedirs): тело heredoc и <<< оболочки за
// обёрткой разбор читает как её скрипт. Ошибка разбора — как у всей команды
func (s *killScan) elevated(c shellcmd.Call, inner string) error {
	redirs, err := inputRedirs(c)
	if err != nil {
		s.tool.Logger().Warn("kill_guard: ввод вызова за обёрткой не перенесён в текст для разбора", "wrapper", c.Name, "error", err)
		if s.killText {
			s.addReason(c.Name + ": ввод за ним не переносится в текст для разбора (" + err.Error() + "): проверить сигналы в нём нельзя")
		}
	}
	text := inner + redirs
	calls, err := shellcmd.Calls(text)
	if err != nil && s.killText {
		s.addReason(c.Name + ": разбор команды за ним не прошёл (" + err.Error() + "): проверить сигналы в ней нельзя")
	}
	calls = s.withWrappedFeeds(calls)
	// Via у всех вызовов — с обёрткой впереди: вызовы ищут друг друга по Via
	// (systemdRunRemote). Вызовы самой команды за обёрткой (их текст — весь
	// inner) получают её ввод и перенаправления: разбор заново их не знает
	// (… | pkexec bash)
	src := s.stdinOf(c)
	piped := src.src == stdinPipe || src.src == stdinFile
	for i := range calls {
		ic := &calls[i]
		if ic.Raw == inner {
			ic.ScriptInput = ic.ScriptInput || piped && inputTaker(*ic)
			if len(ic.Redirs) == 0 {
				ic.Redirs = c.Redirs
			}
		}
		ic.Via = append(append(slices.Clone(c.Via), c.Name), ic.Via...)
	}
	// места вызовов за обёрткой — в её тексте, ввод текста — её ввод
	outer := s.stdinScopeOf()
	defer func() { s.stdin = outer }()
	s.stdin = &stdinScope{text: text, in: src}
	if spans, err := stdinSpans(s.tool.Logger(), text, 0, 4); err == nil {
		s.stdin.spans = spans
	} else {
		s.tool.Logger().Warn("kill_guard: ввод вызовов за обёрткой не проверен, разбор bash не прошёл", "wrapper", c.Name, "error", err)
	}
	for i := range calls {
		ic := &calls[i]
		if err := s.call(*ic, calls); err != nil {
			return err
		}
	}
	return nil
}

// sudoAssignment — c не программа, а присваивание после sudo -- (sudo --
// PATH=$PATH kill 1): shellcmd отдаёт его вызовом с присваиванием вместо
// имени (PATH=$PATH, у X=a/b — b, у X=$(…) — пустым), а программу за ним —
// следующим вызовом с теми же текстом и Via. Присваиваний подряд бывает
// несколько (sudo -- PATH=$PATH LANG=C kill 1): вызов первого несёт
// остальные в аргументах, а программа — первое слово после них. Программу
// проверяет её вызов; присваивание с $ — не имя, вычисляемое при запуске, а
// у оболочки за ним (… | sudo -- X=1 bash) ввод читает она, а не X=1
func sudoAssignment(c shellcmd.Call, calls []shellcmd.Call) bool {
	if !slices.Contains(c.Via, "sudo") {
		return false
	}
	args := c.Args
	for len(args) > 0 && assignWord.MatchString(args[0]) {
		args = args[1:]
	}
	if len(args) == 0 {
		return false
	}
	for i := range calls {
		x := &calls[i]
		if x.Raw == c.Raw && slices.Equal(x.Via, c.Via) && x.Name == filepath.Base(args[0]) && slices.Equal(x.Args, args[1:]) {
			return true
		}
	}
	return false
}

// signal проверяет вызов как сигнал: kill, pkill, killall или имя,
// вычисляемое при запуске. Такое имя — отказ, если в тексте вызова есть
// слово kill ($(echo kill) 1, $X kill 1); kill внутри чужого слова
// (-r raw-process-kill) — не повод. Подстановку команды в имени ($(…),
// `…`) shellcmd отдаёт пустым именем, раскрытие параметра — его текстом с
// $, без значения (K=kill; sudo $K -9 -1 даёт вызов «$K» за sudo). Имя из
// раскрытия переменной или массива ($K, "${a[@]}") — отказ, если в
// команде им присвоено слово сигнала (a=(kill -9 -1)): слово kill стоит в
// присваивании, а не в тексте вызова. Значение по умолчанию в имени
// (${X:-kill}) — то же слово kill. Имя из позиционных параметров ("$@",
// $1) исполняет слова, переданные в другом месте команды (set -- kill -9
// -1, f kill -9 -1, bash -c '"$@"' _ kill …): слово сигнала среди
// аргументов любого вызова проверяется как сигнал с аргументами за ним.
// Прочее вычисляемое имя, связанное со средством (имя systemctl, busctl…
// в подстановке имени или в значении, присвоенном его переменной),
// проверяется по аргументам как это средство (computedTool)
func (s *killScan) signal(c shellcmd.Call, calls []shellcmd.Call) error {
	args := make([]word, 0, len(c.Args))
	for _, a := range c.Args {
		args = append(args, word{text: a, literal: !strings.ContainsAny(a, "$`")})
	}
	via := ""
	if slices.Contains(c.Via, "xargs") {
		via = "xargs"
	}
	if name := substitutedPathName(c); name != "" {
		// $(npm bin)/pm2 kill — программа pm2, kill — её аргумент
		c.Name = name
	}
	// имя без каталога от ${X-/bin/kill} — «kill}»: скобка — тоже раскрытие
	if c.Name == "" || strings.ContainsAny(c.Name, "${}") {
		switch {
		case signalWordRe.MatchString(withoutParamOperators(unquoted(c.Raw))):
			s.addReason("имя команды вычисляется при запуске ($(…), `…` или $X) рядом с kill: проверить, что исполнится, нельзя")
		case arraySignal(c.Name, s.command):
			s.addReason("имя команды — раскрытие переменной или массива, которым в команде присвоено слово kill (a=(kill …); \"${a[@]}\"): проверить, что исполнится, нельзя")
		case positionalRe.MatchString(unquoted(c.Name)):
			return s.positionalSignal(calls)
		case !slices.Contains(c.Via, "ssh"):
			return s.computedTool(c, args, via, computedNames(c, s.command))
		}
		return nil
	}
	if err := s.sessionTool(c, args, via); err != nil {
		return err
	}
	return s.check(c.Name, args, via)
}

// paramOperatorRe начало раскрытия с операндом: ${X:-, ${X-, ${X:=, ${X+, ${X?
var paramOperatorRe = regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*:?[-=+?]`)

// withoutParamOperators текст, где начало раскрытия с операндом заменено
// пробелом: слово kill в ${X:-kill} становится отдельным словом
func withoutParamOperators(text string) string {
	return paramOperatorRe.ReplaceAllString(text, " ")
}

// positionalRe имя из позиционных параметров: $@, "$@", $*, ${@}, $1, ${1}
var positionalRe = regexp.MustCompile(`^\$\{?[@*0-9]`)

// positionalSignal проверяет как сигнал слова за словом kill, pkill или
// killall в аргументах вызовов команды: их исполнит имя из позиционных
// параметров
func (s *killScan) positionalSignal(calls []shellcmd.Call) error {
	for i := range calls {
		x := &calls[i]
		i := slices.IndexFunc(x.Args, func(a string) bool { return slices.Contains(signalNames, filepath.Base(a)) })
		if i < 0 {
			continue
		}
		args := make([]word, 0, len(x.Args)-i-1)
		for _, a := range x.Args[i+1:] {
			args = append(args, word{text: a, literal: !strings.ContainsAny(a, "$`")})
		}
		if err := s.check(filepath.Base(x.Args[i]), args, ""); err != nil {
			return err
		}
	}
	return nil
}

// fedIntoScript обёртка, которая подставляет ввод в скрипт вызова c:
// parallel всегда ({} или ввод в конце), xargs — с -I, -i или --replace
func fedIntoScript(c shellcmd.Call, calls []shellcmd.Call) string {
	if slices.Contains(c.Via, "parallel") {
		return "parallel"
	}
	options := xargsOptions(c, calls)
	if slices.ContainsFunc(options, replaceOption) {
		return "xargs"
	}
	// xargs без -I дописывает входные слова в конец вызова. Для -c они
	// остаются значимыми и при уже заданном скрипте (как $0), поэтому не
	// снимай отказ, который основная проверка даёт по тексту этого ввода.
	if slices.Contains(c.Via, "xargs") && scriptUsesImplicitArgument(c) {
		return "xargs"
	}
	if slices.Contains(c.Via, "xargs") && slices.Contains(c.Via, "-c") && implicitShellArgRe.MatchString(c.Raw) {
		return "xargs"
	}
	return ""
}

// scriptCommandInput сообщает, что вызов исполняет операнд команды как
// скрипт: оболочка с -c или script -c. Сюда xargs может дописать ввод как
// скрипт либо как $0; оба случая требуют проверки.
func scriptCommandInput(c shellcmd.Call) bool {
	_, ok := scriptArgs(c)
	if !ok {
		return false
	}
	if shellcmd.IsShell(c.Name) || c.Name == "script" {
		return true
	}
	if c.Name == "sudo" || c.Name == "doas" {
		return privilegedInteractiveShell(c.Args)
	}
	if c.Name == "su" || c.Name == "runuser" {
		return slices.ContainsFunc(c.Args, func(a string) bool {
			return strings.HasPrefix(a, "-") && shellCommandOption(a)
		})
	}
	return false
}

func privilegedInteractiveShell(args []string) bool {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" || !strings.HasPrefix(a, "-") {
			return false
		}
		if a == "-s" || a == "-i" || a == "--shell" || a == "--login" || a == "-si" || a == "-is" {
			return true
		}
		if a == "-u" || a == "--user" || a == "-g" || a == "--group" || a == "-a" || a == "--auth-type" {
			i++
		}
	}
	return false
}

var implicitShellArgRe = regexp.MustCompile(`\$(?:[0-9]+|[@*])|\$\{(?:[0-9]+|[@*])\}`)

func scriptUsesImplicitArgument(c shellcmd.Call) bool {
	if c.Name == "su" || c.Name == "runuser" {
		for i, a := range c.Args {
			if strings.HasPrefix(a, "-") && shellCommandOption(a) {
				return i+1 >= len(c.Args)
			}
		}
		return false
	}
	args, ok := scriptArgs(c)
	if !ok {
		return false
	}
	if len(args) == 0 {
		return true
	}
	return slices.ContainsFunc(args, func(a string) bool { return implicitShellArgRe.MatchString(a) })
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
	for i := range calls {
		x := &calls[i]
		if x.Raw == c.Raw && len(x.Via) < len(head.Via) {
			head = *x
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

// loadProcs читает процессы один раз на команду
func (s *killScan) loadProcs() error {
	if s.procs != nil {
		return nil
	}
	procs, err := s.tool.listProcs()
	if err != nil {
		return err
	}
	for i := range procs {
		bus, err := dbusSessionBus(procs[i], s.busAddress, s.tool.resolvePath, s.tool.lstatPath)
		if err != nil {
			s.tool.Logger().Debug("D-Bus address path could not be resolved; process remains protected", "pid", procs[i].pid, "error", err)
			procs[i].protected = true
			continue
		}
		procs[i].protected = protectedComms[procs[i].comm] || bus
	}
	s.procs = procs
	return nil
}

// check проверяет одну простую команду, если она шлёт сигнал; via — обёртка,
// подающая аргументы из ввода (xargs, parallel)
func (s *killScan) check(name string, args []word, via string) error {
	if !slices.Contains(signalNames, name) {
		return nil
	}
	if err := s.loadProcs(); err != nil {
		return err
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
		reason, group = s.tool.checkPattern(name, args, s.source, s.procs)
	}
	if reason != "" {
		if before, _, ok := strings.Cut(reason, protectedProcessMarker); ok {
			s.addToolReason(before, protectedProcessHint, false)
		} else {
			s.addReason(reason)
		}
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
	targets, zero, computed := killOptions(args)
	if computed != "" {
		return fmt.Sprintf("kill %s: сигнал вычисляется при запуске, а без кавычек значение несёт и цели (S='9 -1' даст kill -s 9 -1), проверить его заранее нельзя", computed), false
	}
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
			return fmt.Sprintf("kill %d: %s", pid, reason) + protectedProcessMarker, false
		}
	}
	return "", false
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
		if p.pid == pid && (p.protected || protectedComms[p.comm]) {
			return fmt.Sprintf("%s — на нём держится сессия пользователя", p.comm)
		}
	}
	return ""
}

// procInfo процесс по /proc
type procInfo struct {
	pid, ppid     int
	comm, cmdline string
	argv          []string
	protected     bool
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
		argv:    processArgs(cmdline),
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

// substitutedPathName имя программы у вызова, чьё имя — путь с
// подстановкой каталога и литеральным файлом: $(npm bin)/pm2,
// "$(go env GOPATH)/bin/tool". Файл известен до запуска, и слово kill в
// аргументах — аргумент этой программы, а не сигнал. Подстановка со словом
// сигнала ($(echo kill -9 -1 x)/pm2: без кавычек вывод делится на слова) —
// не такое имя; "" — не такое имя
func substitutedPathName(c shellcmd.Call) string {
	if c.Name != "" {
		return ""
	}
	raw := strings.TrimPrefix(c.Raw, `"`)
	sub := nameSubstitution(raw)
	if sub == "" || !strings.HasPrefix(raw, sub) || signalWordRe.MatchString(unquoted(sub)) {
		return ""
	}
	tail := strings.TrimPrefix(raw[len(sub):], `"`)
	if end := strings.IndexAny(tail, " \t\n\"';|&<>()"); end >= 0 {
		tail = tail[:end]
	}
	name := filepath.Base(tail)
	if !strings.HasPrefix(tail, "/") || strings.ContainsAny(tail, "$`\\*?[") || name == "/" {
		return ""
	}
	return name
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
	return slices.ContainsFunc(assignedValues(name, command), func(v string) bool {
		return signalWordRe.MatchString(unquoted(v))
	})
}

// assignedValues значения, которые команда присваивает переменным из имени
// вызова (a=(…), a+=(…), a[0]=…, X="…", export X=…), без снятия кавычек
func assignedValues(name, command string) []string {
	var out []string
	for _, m := range nameVarRe.FindAllStringSubmatch(name, -1) {
		assign := regexp.MustCompile(`(^|[^A-Za-z0-9_])` + m[1] + `(\[[^\]]*\])?\+?=(\([^)]*\)|"[^"]*"|'[^']*'|\S*)`)
		for _, a := range assign.FindAllStringSubmatch(command, -1) {
			out = append(out, a[3])
		}
	}
	return out
}

// killOptions отделяет опции kill от целей так же, как встроенный kill
// bash: -s и -n со значением повторяются, и сигнал задаёт последний
// (kill -0 -s 9 -- -1 шлёт 9, не 0); -SIG (-9, -KILL) — сигнал только
// первым таким словом, дальше -N — цель (группа процессов); -- кончает
// опции. zero — сигнал задан и равен 0. -l, -L, --list и --table печатают
// список сигналов: целей нет. computed — значение -s, -n или --signal,
// вычисляемое при запуске (kill -s $S): без кавычек оно несёт и цели
// (S='9 -1'), как kill -$S. Значение в двойных кавычках ("$SIG") — одно
// слово: только сигнал, не 0 заранее
func killOptions(args []word) (targets []word, zero bool, computed string) {
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
			return nil, false, ""
		case opt == "--":
			targets = targets[1:]
			break options
		case opt == "-s" || opt == "-n" || opt == "--signal":
			if len(targets) < 2 {
				return nil, false, ""
			}
			if !targets[1].literal && !oneWord(targets[1].text) {
				return targets[2:], false, opt + " " + targets[1].text
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
	return targets, zero, ""
}

// oneWord — слово оболочки останется одним словом при запуске: раскрытия
// ($, `) только в двойных кавычках ("$SIG", x"${S}"). "$@" и "${a[@]}"
// дают несколько слов, $'…' и прочее вне кавычек — неизвестно: false.
// Текст — исходник слова из разбора: кавычки в нём закрыты
func oneWord(text string) bool {
	if strings.Contains(text, "@") {
		return false
	}
	double := false
	for i := 0; i < len(text); i++ {
		switch c := text[i]; {
		case c == '\\':
			i++
		case c == '"':
			double = !double
		case c == '\'' && !double:
			i += strings.IndexByte(text[i+1:], '\'') + 1
		case (c == '$' || c == '`') && !double:
			return false
		}
	}
	return true
}
