package tools

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/aiseeq/akit/shellcmd"
)

// Временная служба systemd-run (без --scope) исполняет не только свою
// команду: свойства Exec* (-p ExecStartPre=…, ExecStopPost=…) и --shell
// тоже запускает менеджер, потомком PID 1, а не агента. Их проверяют те же
// правила, что команду под службой, тем же приёмом, что run0
// (kill_guard_run0.go): вызов переписывается в равносильную форму, которую
// правила уже знают. --shell/-S — systemd-run --pipe bash в тексте команды
// (rewriteSystemdRunShell): heredoc, <<<, пайп и < <(…) идут путём оболочки
// под службой. Команда Exec-свойства — отдельный вызов systemd-run [--user]
// -- команда с вводом systemd-run (serviceExecProperties)

// systemdRunLevel systemd-run в словах вызова systemd-run: сам вызов или
// systemd-run в его команде (systemdRunLevels). args — его слова после
// имени, at — место args[0] в словах вызова, o — его опции. err — systemd-run
// в его команде не найти: слова не разобрать (nestedSystemdRun) или
// вложенность глубже предела; при kill в тексте — отказ
// (serviceExecProperties)
type systemdRunLevel struct {
	at   int
	args []string
	o    systemdRunOptions
	err  error
}

// systemdRunLevels вызов systemd-run со словами args и systemd-run в его
// команде, по вложенности: разбор отдаёт вызовом только внешний, а внутренний
// (systemd-run --pipe systemd-run -S, … setsid systemd-run -p
// ExecStartPre=…) — словами внешнего. Внутренний ищет nestedSystemdRun.
// Уровней не больше 1+maxRun0Rewrites: глубже — err последнего
func systemdRunLevels(args []string) []systemdRunLevel {
	levels := []systemdRunLevel{{args: args, o: parseSystemdRunOptions(plainWords(args))}}
	for {
		cur := &levels[len(levels)-1]
		if cur.o.start >= len(cur.args) {
			return levels
		}
		cmd := cur.args[cur.o.start:]
		inner, err := nestedSystemdRun(cmd)
		if err != nil {
			cur.err = err
			return levels
		}
		if inner == nil {
			return levels
		}
		if len(levels) > maxRun0Rewrites {
			cur.err = fmt.Errorf("systemd-run в systemd-run глубже %d", maxRun0Rewrites)
			return levels
		}
		at := cur.at + cur.o.start + len(cmd) - len(inner)
		levels = append(levels, systemdRunLevel{at: at, args: inner, o: parseSystemdRunOptions(plainWords(inner))})
	}
}

// nestedSystemdRunProbe имя команды, которым nestedSystemdRun заменяет
// слово systemd-run, чтобы спросить разбор, исполнят ли его слова перед ним
const nestedSystemdRunProbe = "chooks-nested-systemd-run"

// nestedSystemdRun аргументы systemd-run, которым оказалась команда cmd
// службы: прямо или за обёртками; слова — хвост cmd. nil — это не
// systemd-run. Обёртки снимает тот же разбор shellcmd, что для остальных
// правил: слово systemd-run в cmd — команда, если в словах до него на его
// месте разбор видит вызов (nestedSystemdRunProbe). Разбор
// отдаёт вызовом только самую внутреннюю команду, а systemd-run без неё
// (-S) не отдаёт и в Via — поэтому вопрос, а не поиск в вызовах cmd. Слова
// разбираются литералами, как их получит exec у службы. systemd-run в
// скрипте (sh -c '…') — не слово cmd: его отдаёт вызовом первый разбор; за
// ssh — исполнится не на этой машине. err — слова не разобраны (не UTF-8)
func nestedSystemdRun(cmd []string) ([]string, error) {
	for j := range cmd {
		if path.Base(cmd[j]) != "systemd-run" {
			continue
		}
		calls, err := shellcmd.Calls(shellWords(append(slices.Clone(cmd[:j]), nestedSystemdRunProbe)))
		if err != nil {
			return nil, fmt.Errorf("команда systemd-run %q: %w", cmd, err)
		}
		for i := range calls {
			x := &calls[i]
			if x.Name == nestedSystemdRunProbe && !slices.Contains(x.Via, "ssh") {
				return cmd[j+1:], nil
			}
		}
	}
	return nil, nil
}

// systemdRunShellWords слова вызова systemd-run, где --shell/-S последнего
// systemd-run во вложении (systemdRunLevels: только у него нет команды)
// переписан в --pipe bash: слово опции — --pipe, буква S в связке — P (тоже
// флаг без значения), в конце -- bash. at — место переписанного слова. ok
// false — --shell нет или задана команда (systemd-run с --shell и командой
// не запускается)
func systemdRunShellWords(args []string) (words []string, at int, ok bool) {
	levels := systemdRunLevels(args)
	last := &levels[len(levels)-1]
	if !last.o.shell || last.o.start < len(last.args) {
		return nil, 0, false
	}
	at = last.at + last.o.shellWord
	words = slices.Clone(args)
	words[at] = systemdRunPipeWord(args[at], last.o.shellLetter)
	if words[len(words)-1] != "--" {
		words = append(words, "--")
	}
	return append(words, "bash"), at, true
}

// systemdRunPipeWord слово опции --shell (letter -1) или связки с S на месте
// letter, где --shell заменён на --pipe
func systemdRunPipeWord(arg string, letter int) string {
	if letter < 0 {
		return "--pipe"
	}
	return arg[:letter] + "P" + arg[letter+1:]
}

// rewriteSystemdRunShell команда, где каждый systemd-run с --shell/-S,
// стоящий в тексте словом, переписан в systemd-run --pipe … -- bash
// (replaceSystemdRunShell). Вызов, который так не переписать (слово вне
// текста, значение в кавычках), проверяет rewrittenCommand. err — первая
// ошибка разбора слов вызова
func rewriteSystemdRunShell(command string) (text string, err error) {
	text = command
	for range maxRun0Rewrites {
		calls, perr := shellcmd.Calls(text)
		if calls == nil && perr != nil {
			return text, err
		}
		next, ok := "", false
		for i := range calls {
			c := &calls[i]
			if c.Name != "systemd-run" {
				continue
			}
			var werr error
			if next, ok, werr = replaceSystemdRunShell(text, c); ok {
				break
			}
			if err == nil {
				err = werr
			}
		}
		if !ok {
			return text, err
		}
		text = next
	}
	return text, err
}

// replaceSystemdRunShell text, где у вызова c слово --shell (буква S)
// заменено на --pipe (P), а за последним словом — -- bash. Прочие слова и
// перенаправления остаются как в тексте. ok false — у c нет --shell, c нет
// в тексте словом или новое слово требует кавычек (safeValueRe)
func replaceSystemdRunShell(text string, c *shellcmd.Call) (string, bool, error) {
	words, shellAt, ok := systemdRunShellWords(c.Args)
	if !ok {
		return "", false, nil
	}
	pipe := words[shellAt]
	if !safeValueRe.MatchString(pipe) {
		return "", false, nil
	}
	at, spans, err := run0Words(text, *c)
	if err != nil || spans == nil {
		return "", false, err
	}
	tail := " -- bash"
	if c.Args[len(c.Args)-1] == "--" {
		tail = " bash"
	}
	word := spans[1+shellAt]
	end := spans[len(c.Args)][1]
	return text[:at+word[0]] + pipe + text[at+word[1]:at+end] + tail + text[at+end:], true, nil
}

// systemdRunShellCommand текст, которым проверяется systemd-run с --shell,
// когда его нельзя переписать в тексте команды: systemd-run --pipe … --
// bash со словами вызова в кавычках
func systemdRunShellCommand(args []string) (string, bool) {
	words, _, ok := systemdRunShellWords(args)
	if !ok {
		return "", false
	}
	return "systemd-run " + shellWords(words), true
}

// systemdExecKeys свойства службы с командой (man systemd.service, «Command
// lines»), и у каждого вариант *Ex с теми же префиксами
var systemdExecKeys = []string{"ExecCondition", "ExecStartPre", "ExecStart", "ExecStartPost", "ExecReload", "ExecStop", "ExecStopPost"}

// execPropertyKey — key задаёт команду службы: Exec* или Exec*Ex
func execPropertyKey(key string) bool {
	return slices.Contains(systemdExecKeys, strings.TrimSuffix(key, "Ex"))
}

// serviceCommands проверяет команды, которые служба systemd-run c исполнит
// помимо видимой разбору: Exec-свойства (serviceExecProperties) и parallel
// без команды (serviceParallel). systemd-run --shell, не переписанный в
// тексте, сюда не доходит: его проверяет rewrittenCommand вместе со
// свойствами
func (s *killScan) serviceCommands(c shellcmd.Call, calls []shellcmd.Call) error {
	if c.Name == "systemd-run" {
		if err := s.serviceExecProperties(c, calls); err != nil {
			return err
		}
	} else if c.Name == "run0" {
		if run, ok := run0ServiceCall(c); ok {
			if err := s.serviceExecProperties(run, calls); err != nil {
				return err
			}
		}
	} else if run, ok := wrappedSystemdRun(c, calls); ok {
		if err := s.serviceExecProperties(run, calls); err != nil {
			return err
		}
	}
	return s.serviceParallel(c, calls)
}

func run0ServiceCall(c shellcmd.Call) (shellcmd.Call, bool) {
	inv, ok := parseRun0(c.Args)
	if !ok {
		return shellcmd.Call{}, false
	}
	args := []string{"--pipe"}
	for _, machine := range inv.machines {
		args = append(args, "--machine="+machine)
	}
	for _, property := range inv.properties {
		args = append(args, "--property="+property)
	}
	args = append(args, "--")
	switch {
	case len(inv.command) == 0:
		args = append(args, "sh")
	case inv.viaShell:
		args = append(args, "sh", "-c", strings.Join(inv.command, " "))
	default:
		args = append(args, inv.command...)
	}
	return shellcmd.Call{Name: "systemd-run", Args: args, Raw: c.Raw, Via: c.Via, Line: c.Line, Offset: c.Offset, Redirs: c.Redirs}, true
}

// wrappedSystemdRun находит systemd-run, который shellcmd показал как
// запущенный из c. Via у вложенного вызова — источник истины для обёртки;
// этот код не дублирует список команд-launcher.
func wrappedSystemdRun(c shellcmd.Call, calls []shellcmd.Call) (shellcmd.Call, bool) {
	via := append(slices.Clone(c.Via), c.Name)
	found := false
	for i := range calls {
		child := &calls[i]
		if child.Raw == c.Raw && len(child.Via) > len(via) && slices.Equal(child.Via[:len(via)], via) && child.Via[len(via)] == "systemd-run" {
			found = true
			break
		}
	}
	if !found {
		return shellcmd.Call{}, false
	}
	for i, arg := range c.Args {
		if path.Base(arg) != "systemd-run" {
			continue
		}
		parsed, err := shellcmd.Calls(shellWords(c.Args[i:]))
		if err == nil && len(parsed) > 0 && parsed[0].Name == "systemd-run" {
			return shellcmd.Call{Name: "systemd-run", Args: c.Args[i+1:], Raw: c.Raw, Via: via, Line: c.Line, Offset: c.Offset, Redirs: c.Redirs}, true
		}
	}
	return shellcmd.Call{}, false
}

// serviceExecProperties проверяет main-команду и Exec-свойства systemd-run c
// общим разбором systemd Exec: служба исполнит их как потомков PID 1. Вычисляемую
// команду или подкоманду нельзя пропускать при пустом killText. systemd-run прямо под
// systemd-run разбор вызовом не отдаёт — его свойства берутся из слов
// внешнего. Не проверяются --scope (свойства команды scope не запускает) и
// systemd-run на другой машине. Вычисляемые свойства Exec, которые могут
// задать команду, отклоняются; ошибки разбора отклоняются при killText.
func (s *killScan) serviceExecProperties(c shellcmd.Call, calls []shellcmd.Call) error {
	if slices.Contains(c.Via, "ssh") || systemdRunElsewhere(c, calls) {
		return nil
	}
	fed := xargsReplaced(c, calls)
	levels := systemdRunLevels(c.Args)
	for i := range levels {
		l := &levels[i]
		if l.o.remote {
			return nil
		}
		if !l.o.scope {
			if err := s.unitExecProperties(c, l.args, &l.o, fed); err != nil {
				return err
			}
		}
		if l.err != nil {
			s.tool.Logger().Debug("kill_guard: systemd-run в команде systemd-run не найти", "error", l.err)
			if s.killText {
				s.addReason("systemd-run: " + l.err.Error() + ": свойства и --shell systemd-run в его команде проверить нельзя")
			}
		}
	}
	return nil
}

// unitExecProperties проверяет main-команду и Exec-свойства одной службы
// systemd-run тем же анализом. Непрозрачная команда или подкоманда означает
// отказ и без killText. systemd-run за xargs/parallel без команды получает
// опции и команду из ввода; при killText это тоже отказ.
func (s *killScan) unitExecProperties(c shellcmd.Call, args []string, o *systemdRunOptions, fed func(string) bool) error {
	if s.killText && afterXargs(c) && o.start == len(args) {
		s.addToolReason("systemd-run за xargs или parallel без команды: слова ввода станут его опциями и командой (-p ExecStartPre=…), их не видно до запуска"+hiddenTail, systemdRunHiddenHint, false)
	}
	if err := s.unitMainCommand(args, o); err != nil {
		return err
	}
	return s.unitExecPropertyValues(c, o.properties, fed)
}

func (s *killScan) unitMainCommand(args []string, o *systemdRunOptions) error {
	if o.start < len(args) {
		main := systemdArgsCommand(args[o.start:])
		_, _, computedCommand, ok, parseErr := systemdExecCommandInfo(main)
		if parseErr != nil {
			s.tool.Logger().Warn("kill_guard: не разобрана shell-команда systemd-run", "error", parseErr)
			s.addToolReason("команда systemd-run содержит shell-команду, которую не удалось разобрать"+hiddenTail, systemdRunHiddenHint, false)
		} else if ok && computedCommand {
			s.addToolReason("команда systemd-run содержит подстановку, которая может выбрать исполняемую подкоманду"+hiddenTail, systemdRunHiddenHint, false)
		}
	}
	return nil
}

func (s *killScan) unitExecPropertyValues(c shellcmd.Call, properties []word, fed func(string) bool) error {
	for _, p := range properties {
		inner, reason, unconditional, parseErr := execPropertyCommand(p.text, fed)
		if shellANSIExecProperty(c.Raw, p.text) {
			inner, reason, unconditional, parseErr = "", "значение Exec-свойства вычисляется ANSI-C кавычками оболочки", true, nil
		}
		if parseErr != nil {
			s.tool.Logger().Warn("kill_guard: не разобрана shell-команда Exec-свойства", "error", parseErr)
			reason, unconditional = "команда "+p.text+" не разбирается: "+parseErr.Error(), true
		}
		switch {
		case reason != "" && (s.killText || unconditional):
			s.addToolReason("systemd-run -p "+p.text+": "+reason+hiddenTail, systemdRunHiddenHint, false)
		case reason == "" && inner != "":
			if !s.killText && systemdGeneratedShellHasDynamic(inner) {
				continue
			}
			if err := s.elevated(c, inner); err != nil {
				return err
			}
		}
	}
	return nil
}

func shellANSIExecProperty(raw, property string) bool {
	key, _, found := strings.Cut(unquoted(property), "=")
	return found && execPropertyKey(key) && strings.Contains(raw, "$'") && strings.Contains(property, `\`)
}

func systemdArgsCommand(args []string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = singleQuotedWord(unquoted(arg))
	}
	return strings.Join(quoted, " ")
}

func systemdGeneratedShellHasDynamic(inner string) bool {
	for _, command := range []string{"sh", "bash", "dash", "zsh", "/bin/sh", "/bin/bash", "/usr/bin/sh", "/usr/bin/bash"} {
		for _, flag := range []string{"'-c'", "'--command'"} {
			prefix := singleQuotedWord(command) + " " + flag + " "
			if strings.HasPrefix(inner, prefix) && strings.ContainsAny(strings.TrimPrefix(inner, prefix), "$`") {
				return true
			}
		}
	}
	if strings.HasPrefix(inner, "sh -c ") && strings.ContainsAny(strings.TrimPrefix(inner, "sh -c "), "$`") {
		return true
	}
	return false
}

// execPropertyCommand разбирает Exec-свойство arg (слово -p из shellcmd).
// Пустые inner и reason означают не Exec-свойство или сброс списка. computed
// означает, что значение или имя может задать неизвестную Exec-команду.
func execPropertyCommand(arg string, fed func(string) bool) (inner, reason string, unconditional bool, parseErr error) {
	computed := computedArg(arg) || fed(arg)
	text := arg
	if computedArg(arg) {
		text = unquoted(arg)
	}
	key, value, found := strings.Cut(text, "=")
	if computed && (strings.ContainsAny(key, "$`") || fed(key)) {
		return "", "имя свойства вычисляется при запуске: оно может задать Exec-команду, проверить её нельзя", true, nil
	}
	if !found || !execPropertyKey(key) {
		return "", "", false, nil
	}
	if computed {
		return "", "команда " + key + " вычисляется оболочкой при запуске: проверить её нельзя", true, nil
	}
	inner, computedExecutable, computedCommand, ok, parseErr := systemdExecCommandInfo(value)
	if parseErr != nil {
		return "", "", false, parseErr
	}
	if computedCommand || computedExecutable {
		return "", "команда " + key + " содержит подстановку выбора исполняемой команды или подкоманды", true, nil
	}
	if !ok {
		return "", "команда " + key + " не разбирается (незакрытая кавычка, @ без argv[0])", false, nil
	}
	return inner, "", false, nil
}

// systemdExecCommandInfo командная строка Exec-свойства (man systemd.service,
// «Command lines») текстом оболочки. Префиксы -@:+!| перед путём: @ —
// второе слово становится argv[0] и выбрасывается (с | — нет: он делает
// оболочку login); | — команда идёт оболочке -c словами через пробел, | без
// команды — оболочка, читающая ввод; : — без подстановки окружения. Слова —
// по кавычкам ' и " (systemd.syntax), # — литерал. ${VAR} и $VAR (их
// раскрывает systemd из окружения службы, -E и Environment=) — вычисляемые
// слова: в тексте — "${VAR}"; $VAR внутри слова systemd оставляет литералом,
// здесь он тоже вычисляемый. $$ — литеральный $. Спецификаторы %n, %N у
// временной службы systemd 259 не раскрывает (проба 19.11): % — литерал.
// Текст без команды — "". computedExecutable — вычисляемый путь argv[0];
// computedCommand — вычисляемая подкоманда или shell-команда; ok false —
// ошибка синтаксиса или у @ нет argv[0].
func systemdExecCommandInfo(value string) (inner string, computedExecutable, computedCommand, ok bool, parseErr error) {
	i := 0
	var at, viaShell, noEnv bool
	for ; i < len(value) && strings.ContainsRune("-@:+!|", rune(value[i])); i++ {
		switch value[i] {
		case '@':
			at = true
		case '|':
			viaShell = true
		case ':':
			noEnv = true
		}
	}
	words, ok := systemdExecWords(value[i:], !noEnv)
	if !ok {
		return "", false, false, false, nil
	}
	if viaShell {
		return systemdExecShellCommand(words)
	}
	switch {
	case len(words) == 0:
		return "", false, false, true, nil
	case at && len(words) < 2:
		return "", false, false, false, nil
	case at:
		words = slices.Delete(words, 1, 2)
	}
	out := make([]string, 0, len(words))
	for _, w := range words {
		out = append(out, w.shell())
	}
	inner = strings.Join(out, " ")
	scriptComputed, err := systemdShellScriptComputedCommand(words)
	if err != nil {
		return inner, false, false, true, err
	}
	return inner, execWordComputed(words[0]), systemdExecSubcommand(words) || scriptComputed, true, nil
}

func systemdExecShellCommand(words []execWord) (inner string, computedExecutable, computedCommand, ok bool, parseErr error) {
	if len(words) == 0 {
		return "sh", false, false, true, nil
	}
	var script execWord
	for i, word := range words {
		if i > 0 {
			script = append(script, execPart{text: " "})
		}
		script = append(script, word...)
	}
	inner = "sh -c " + script.shell()
	scriptComputed, err := systemdShellScriptComputedText(execWordRaw(script))
	if err != nil {
		return inner, false, false, true, err
	}
	return inner, false, systemdExecSubcommand(words) || scriptComputed, true, nil
}

// systemdShellScriptComputedCommand detects substitutions in shell scripts
// when they choose the command word or a subcommand selected by a launcher.
// Substitutions in ordinary arguments (for example echo ${X}) remain allowed.
func systemdShellScriptComputedCommand(words []execWord) (bool, error) {
	if len(words) < 3 {
		return false, nil
	}
	command, literal := execWordText(words[0])
	if !literal || !slices.Contains([]string{"sh", "bash", "dash", "zsh"}, path.Base(command)) {
		return false, nil
	}
	args := words[1:]
	for i := range args {
		arg, ok := execWordText(args[i])
		if !ok {
			continue
		}
		if arg == "-c" || arg == "--command" {
			if i+1 >= len(args) {
				return false, nil
			}
			return systemdShellScriptComputedText(execWordRaw(args[i+1]))
		}
	}
	return false, nil
}

func execWordRaw(word execWord) string {
	var b strings.Builder
	for _, part := range word {
		if part.computed {
			b.WriteString("${")
			b.WriteString(part.name)
			b.WriteByte('}')
		} else {
			b.WriteString(part.text)
		}
	}
	return b.String()
}

func systemdShellScriptComputedText(script string) (bool, error) {
	calls, err := shellcmd.Calls(script)
	if err != nil {
		return false, fmt.Errorf("разбор shell-команды systemd Exec: %w", err)
	}
	for i := range calls {
		call := &calls[i]
		if strings.ContainsAny(call.Name, "$`") {
			return true, nil
		}
		launcher := path.Base(call.Name)
		words := make([]execWord, 1, len(call.Args)+1)
		words[0] = execWord{{text: call.Name}}
		for _, arg := range call.Args {
			part := execPart{text: arg, computed: strings.ContainsAny(arg, "$`")}
			words = append(words, execWord{part})
		}
		if launcher == "eval" || launcher == "exec" || launcher == "command" {
			if slices.ContainsFunc(words[1:], execWordComputed) {
				return true, nil
			}
		} else if systemdExecSubcommand(words) {
			return true, nil
		}
	}
	return false, nil
}

func execWordComputed(w execWord) bool {
	return slices.ContainsFunc(w, func(p execPart) bool { return p.computed })
}

// systemdExecSubcommand сообщает, может ли переменная окружения выбрать
// исполняемую команду после мультикомандного launcher. Обычные аргументы
// echo/make не считаются командой.
func systemdExecSubcommand(words []execWord) bool {
	if len(words) == 0 {
		return false
	}
	command, _ := execWordText(words[0])
	base := path.Base(command)
	args := words[1:]
	switch base {
	case "podman", "podman-remote":
		return systemdPodmanSubcommand(args)
	case "systemctl", "loginctl":
		values := systemctlValues
		if base == "loginctl" {
			values = loginctlValues
		}
		return systemdSelectedOperandComputed(args, values)
	case "env":
		return systemdEnvSubcommand(args)
	case "xargs":
		values := []string{"a", "arg-file", "d", "delimiter", "E", "eof", "I", "L", "max-lines", "n", "max-args", "P", "max-procs", "s", "max-chars"}
		return systemdSelectedOperandComputed(args, values)
	}
	return false
}

func systemdPodmanSubcommand(args []execWord) bool {
	for i := 0; i < len(args); i++ {
		arg, literal := execWordText(args[i])
		if !literal {
			return true
		}
		if arg == "--" {
			continue
		}
		if strings.HasPrefix(arg, "-") {
			name := strings.TrimLeft(arg, "-")
			if !strings.Contains(name, "=") && slices.Contains(podmanValues, name) {
				i++
			}
			continue
		}
		if arg == "container" || arg == "pod" {
			continue
		}
		return execWordComputed(args[i])
	}
	return false
}

func systemdSelectedOperandComputed(args []execWord, values []string) bool {
	if i := systemdOperandIndex(args, values); i < len(args) {
		return execWordComputed(args[i])
	}
	return false
}

func systemdEnvSubcommand(args []execWord) bool {
	for i := 0; i < len(args); i++ {
		arg, literal := execWordText(args[i])
		if !literal {
			return true
		}
		if arg == "-S" || arg == "--split-string" || strings.HasPrefix(arg, "-S") || strings.HasPrefix(arg, "--split-string=") {
			return execWordComputed(args[i]) || i+1 < len(args) && execWordComputed(args[i+1])
		}
		if strings.HasPrefix(arg, "-") {
			if arg == "-u" || arg == "--unset" || arg == "-C" || arg == "--chdir" || strings.HasPrefix(arg, "--unset=") || strings.HasPrefix(arg, "--chdir=") {
				i++
			}
			continue
		}
		if strings.Contains(arg, "=") {
			continue
		}
		return execWordComputed(args[i])
	}
	return false
}

func systemdOperandIndex(args []execWord, values []string) int {
	for i := 0; i < len(args); i++ {
		arg, literal := execWordText(args[i])
		if !literal {
			return i
		}
		if arg == "--" {
			return i + 1
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			return i
		}
		name, _, inline := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if !inline && slices.Contains(values, name) && i+1 < len(args) {
			i++
		}
	}
	return len(args)
}

func execWordText(w execWord) (string, bool) {
	var b strings.Builder
	for _, p := range w {
		if p.computed {
			return b.String(), false
		}
		b.WriteString(p.text)
	}
	return b.String(), true
}

// execPart часть слова командной строки systemd: литерал text или
// вычисляемое значение (name — имя переменной для текста оболочки)
type execPart struct {
	text, name string
	computed   bool
}

// execWord слово командной строки systemd частями
type execWord []execPart

// shell слово текстом оболочки: литералы подряд — одним словом в одинарных
// кавычках, вычисляемое — "${VAR}"
func (w execWord) shell() string {
	var b, lit strings.Builder
	flush := func() {
		if lit.Len() > 0 {
			b.WriteString(singleQuotedWord(lit.String()))
			lit.Reset()
		}
	}
	for _, p := range w {
		if p.computed {
			flush()
			b.WriteString(`"${` + p.name + `}"`)
			continue
		}
		lit.WriteString(p.text)
	}
	flush()
	if b.Len() == 0 {
		return "''"
	}
	return b.String()
}

// envNameRe имя переменной окружения в ${…} и $NAME командной строки systemd
var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*`)

// systemdExecWords слова командной строки systemd (без префиксов): пробелы
// разделяют слова, кавычки ' и " держат их части вместе и снимаются, # —
// литерал. env — раскрывается ли окружение: ${VAR} в любом месте слова,
// $VAR — только словом целиком (после снятия кавычек), $$ — литерал $;
// $VAR внутри слова остаётся текстом (sh -c "echo $X" получит $X). ok
// false — кавычка не закрыта. Обратной косой черты в value нет (её
// проверяет вызывающий)
func systemdExecWords(value string, env bool) (words []execWord, ok bool) {
	var cur execWord
	var lit, raw strings.Builder
	inWord := false
	var quote byte
	flushLit := func() {
		if lit.Len() > 0 {
			cur = append(cur, execPart{text: lit.String()})
			lit.Reset()
		}
	}
	endWord := func() {
		flushLit()
		if env {
			if part, whole := systemdWholeEnvWord(raw.String()); whole {
				cur = execWord{part}
			}
		}
		words = append(words, cur)
		cur, inWord = nil, false
		raw.Reset()
	}
	for i := 0; i < len(value); i++ {
		ch := value[i]
		if ch == '\\' {
			decoded, width, ok := systemdCUnescape(value[i:])
			if !ok {
				return nil, false
			}
			raw.WriteString(decoded)
			lit.WriteString(decoded)
			i += width - 1
			inWord = true
			continue
		}
		if nextQuote, handled := systemdExecQuote(quote, ch); handled {
			quote = nextQuote
			if quote != 0 {
				inWord = true
			}
			continue
		}
		if quote == 0 && strings.IndexByte(" \t\n\r", ch) >= 0 {
			if inWord {
				endWord()
			}
			continue
		}
		inWord = true
		if env && ch == '$' {
			if part, width := systemdEnvRef(value[i:]); width > 0 {
				raw.WriteString(value[i : i+width])
				if part.computed {
					flushLit()
					cur = append(cur, part)
				} else {
					lit.WriteString(part.text)
				}
				i += width - 1
				continue
			}
		}
		raw.WriteByte(ch)
		lit.WriteByte(ch)
	}
	if quote != 0 {
		return nil, false
	}
	if inWord {
		endWord()
	}
	return words, true
}

func systemdExecQuote(quote, ch byte) (next byte, handled bool) {
	if quote != 0 && ch == quote {
		return 0, true
	}
	if quote == 0 && (ch == '\'' || ch == '"') {
		return ch, true
	}
	return quote, false
}

func systemdCUnescape(s string) (decoded string, width int, ok bool) {
	if len(s) < 2 || s[0] != '\\' {
		return "", 0, false
	}
	if decoded, found := systemdSimpleCEscapes[s[1]]; found {
		return decoded, 2, true
	}
	switch s[1] {
	case 'x':
		if len(s) < 4 {
			return "", 0, false
		}
		n, digits, ok := systemdEscapeNumber(s[2:4], 16, 2)
		return string(rune(n)), digits + 2, ok && digits == 2
	case 'u', 'U':
		length := 4
		if s[1] == 'U' {
			length = 8
		}
		if len(s) < length+2 {
			return "", 0, false
		}
		n, digits, ok := systemdEscapeNumber(s[2:length+2], 16, length)
		if !ok || digits != length || n > 0x10ffff || n >= 0xd800 && n <= 0xdfff {
			return "", 0, false
		}
		return string(rune(n)), length + 2, true
	default:
		if s[1] < '0' || s[1] > '7' {
			return "", 0, false
		}
		n, digits, ok := systemdEscapeNumber(s[1:], 8, 3)
		return string(rune(n)), digits + 1, ok && n <= 0xff
	}
}

var systemdSimpleCEscapes = map[byte]string{
	'a': "\a", 'b': "\b", 'f': "\f", 'n': "\n", 'r': "\r", 't': "\t",
	'v': "\v", '\\': `\`, '"': `"`, '\'': `'`, 's': " ",
}

func systemdEscapeNumber(s string, base, limit int) (uint32, int, bool) {
	var n uint32
	for i := 0; i < len(s) && i < limit; i++ {
		digit, ok := systemdEscapeDigit(s[i])
		if !ok || digit >= base {
			return n, i, i > 0
		}
		n = n*uint32(base) + uint32(digit)
	}
	return n, min(len(s), limit), true
}

func systemdEscapeDigit(ch byte) (int, bool) {
	switch {
	case ch >= '0' && ch <= '9':
		return int(ch - '0'), true
	case ch >= 'a' && ch <= 'f':
		return int(ch-'a') + 10, true
	case ch >= 'A' && ch <= 'F':
		return int(ch-'A') + 10, true
	default:
		return 0, false
	}
}

// systemdWholeEnvWord слово raw (кавычки сняты, $ не раскрыты) целиком —
// ссылка $NAME: systemd заменит его значением переменной, разбитым по
// пробелам, или уберёт, если её нет. Имя не по envNameRe — SYSTEMD_ENV.
// whole false — слово начинается не с $ или с ${, $$ (их раскрывает
// systemdEnvRef)
func systemdWholeEnvWord(raw string) (part execPart, whole bool) {
	if !strings.HasPrefix(raw, "$") || strings.HasPrefix(raw, "${") || strings.HasPrefix(raw, "$$") {
		return execPart{}, false
	}
	name := raw[1:]
	if name == "" || envNameRe.FindString(name) != name {
		name = "SYSTEMD_ENV"
	}
	return execPart{name: name, computed: true}, true
}

// systemdEnvRef ссылка на окружение в начале s (s[0] — $) внутри слова: $$
// — литерал $, ${NAME} — вычисляемая часть с этим именем (имя не по
// envNameRe и ${ без } — SYSTEMD_ENV). width — длина ссылки в s, 0 — $
// здесь литерал ($NAME внутри слова systemd не раскрывает)
func systemdEnvRef(s string) (part execPart, width int) {
	switch {
	case strings.HasPrefix(s, "$$"):
		return execPart{text: "$"}, 2
	case strings.HasPrefix(s, "${"):
		end := strings.IndexByte(s, '}')
		if end < 0 {
			return execPart{name: "SYSTEMD_ENV", computed: true}, len(s)
		}
		name := s[2:end]
		if name == "" || envNameRe.FindString(name) != name {
			name = "SYSTEMD_ENV"
		}
		return execPart{name: name, computed: true}, end + 1
	}
	return execPart{}, 0
}

// xargsReplaced признак слова systemd-run, на место которого xargs или
// parallel подставят строку ввода: строка замены xargs -I, -i, --replace
// ({} по умолчанию) или {…} у parallel. Без подстановки — всегда false
func xargsReplaced(c shellcmd.Call, calls []shellcmd.Call) func(string) bool {
	switch fedIntoScript(c, calls) {
	case "parallel":
		return func(w string) bool { return strings.Contains(w, "{") && strings.Contains(w, "}") }
	case "xargs":
		repl := xargsReplaceString(xargsOptions(c, calls))
		return func(w string) bool { return repl == "" || strings.Contains(w, repl) }
	}
	return func(string) bool { return false }
}

// xargsReplaceString строка замены xargs по его опциям: -I R, -IR,
// --replace=R, -iR; -i и --replace без значения — {}. "" — не найдена
func xargsReplaceString(opts []string) string {
	repl := ""
	for i, o := range opts {
		switch {
		case o == "-I" && i+1 < len(opts):
			repl = opts[i+1]
		case strings.HasPrefix(o, "-I") && o != "-I":
			repl = o[2:]
		case o == "-i" || o == "--replace":
			repl = "{}"
		case strings.HasPrefix(o, "--replace="):
			repl = strings.TrimPrefix(o, "--replace=")
		case strings.HasPrefix(o, "-i"):
			repl = o[2:]
		}
	}
	return repl
}
