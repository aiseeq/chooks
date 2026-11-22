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
// правила, что команду под службой: вызов переписывается в равносильную
// форму, которую правила уже знают. --shell/-S — systemd-run --pipe bash в тексте команды
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
// Уровней не больше 1+maxNestedRewrites: глубже — err последнего
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
		if len(levels) > maxNestedRewrites {
			cur.err = fmt.Errorf("systemd-run в systemd-run глубже %d", maxNestedRewrites)
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
	j, _, err := nestedCommand(cmd, "systemd-run")
	if err != nil {
		return nil, err
	}
	if j < 0 {
		return nil, nil
	}
	return cmd[j+1:], nil
}

// nestedCommand место первого слова cmd с программой name, которое разбор
// видит командой (nestedSystemdRun), и Via до него внутри cmd (обёртки:
// nice, systemd-run). Все слова — уже значения аргументов exec, поэтому при
// склейке каждое берётся целиком в кавычки: $ и # внутри слова — литералы.
// j -1 — такого нет. err — слова не разобраны
func nestedCommand(cmd []string, name string) (j int, via []string, err error) {
	for j := range cmd {
		if path.Base(cmd[j]) != name {
			continue
		}
		calls, err := shellcmd.Calls(literalShellWords(append(slices.Clone(cmd[:j]), nestedSystemdRunProbe)))
		if err != nil {
			return -1, nil, fmt.Errorf("слова %q: %w", cmd, err)
		}
		for i := range calls {
			x := &calls[i]
			if x.Name == nestedSystemdRunProbe && !slices.Contains(x.Via, "ssh") {
				return j, x.Via, nil
			}
		}
	}
	return -1, nil, nil
}

// literalShellWords — слова, полученные программой после разбора внешней
// команды, как буквальные операнды для shellcmd. shellWords оставляет $ и `
// активными для вызовов, где нужно сохранить семантику исходной оболочки.
func literalShellWords(args []string) string {
	words := make([]string, len(args))
	for i, arg := range args {
		words[i] = singleQuotedWord(arg)
	}
	return strings.Join(words, " ")
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
	for range maxNestedRewrites {
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
	at, spans, err := callWords(text, *c)
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

// serviceCommands проверяет команды, которые служба systemd-run или run0 c
// исполнит помимо видимой разбору: Exec-свойства (serviceExecProperties,
// run0Service). systemd-run --shell, не переписанный в тексте, сюда не
// доходит: его проверяет rewrittenCommand вместе со свойствами
func (s *killScan) serviceCommands(c shellcmd.Call, calls []shellcmd.Call) error {
	switch c.Name {
	case "run0":
		return s.run0Service(c)
	case "systemd-run":
		return s.serviceExecProperties(c, calls)
	}
	if run, err := s.wrappedSystemdRun(c); err != nil {
		// как ошибка уровня в serviceExecProperties: вызов-обёртка — любой
		// nice, timeout, setsid, отказ без kill в тексте задел бы обычные команды
		s.tool.Logger().Debug("kill_guard: systemd-run за обёрткой не разобрать", "error", err)
		if s.killText {
			s.addReason("systemd-run за обёрткой: " + err.Error() + ": свойства и команду временной службы проверить нельзя")
		}
	} else if run != nil {
		return s.serviceExecProperties(*run, calls)
	}
	return nil
}

// wrappedSystemdRun systemd-run, который вызов-обёртка c (setsid, flock,
// chrt, nice и их цепочки) исполнит своей командой, отдельным вызовом с
// Via c. Слово systemd-run ищет в словах самого c тот же разбор, что для
// systemd-run в команде службы (nestedSystemdRun): systemd-run операндом
// (echo systemd-run …) — не команда; nil — его нет. err — вызов не
// разобрать заново
func (s *killScan) wrappedSystemdRun(c shellcmd.Call) (*shellcmd.Call, error) {
	inner, err := nestedSystemdRun(append([]string{c.Name}, c.Args...))
	if err != nil {
		return nil, fmt.Errorf("найти обёрнутый systemd-run: %w", err)
	}
	if inner == nil {
		return nil, nil
	}
	i := len(c.Args) - len(inner) - 1
	fallback := shellcmd.Call{Name: "systemd-run", Args: c.Args[i+1:], Via: append(slices.Clone(c.Via), c.Name)}
	// ArgLiteral относится ко всему скрипту: в && и конвейере он не
	// отмечает даже литеральные слова. Повторный разбор самого вызова
	// восстанавливает эти признаки, если слово осталось тем же.
	calls, err := shellcmd.Calls(c.Raw)
	if err != nil {
		return nil, fmt.Errorf("разобрать вызов отдельно от скрипта: %w", err)
	}
	var isolated shellcmd.Call
	for j := range calls {
		call := &calls[j]
		if call.Name == c.Name && slices.Equal(call.Args, c.Args) {
			isolated = *call
			break
		}
	}
	words := make([]string, len(c.Args)-i)
	for j, word := range c.Args[i:] {
		if c.ArgLiteral(i+j) || isolated.ArgLiteral(i+j) {
			words[j] = singleQuotedWord(word)
		} else {
			words[j] = word
		}
	}
	parsed, err := shellcmd.Calls(strings.Join(words, " "))
	// слово без признака литеральности ("a\"b" рядом с "$X") ломает склейку; исходные слова без признаков — строгая сторона: Exec-значение с \ или $ получает отказ, обычная команда проходит
	if err != nil {
		s.tool.Logger().Debug("kill_guard: обёрнутый systemd-run не разобрать заново, проверка по исходным словам", "error", err)
		// то же: проверка по исходным словам — строгая сторона
		return &fallback, nil
	}
	for j := range parsed {
		call := &parsed[j]
		if call.Name == "systemd-run" && slices.Equal(call.Args, c.Args[i+1:]) {
			call.Via = fallback.Via
			return call, nil
		}
	}
	// При неизвестном слове ArgLiteral не гарантирован даже для отдельного
	// вызова. Проверяем исходные аргументы, не приписывая им признаки
	// другого разбора.
	return &fallback, nil
}

// serviceExecProperties проверяет команду и Exec-свойства временной службы.
// Вычисляемое Exec-свойство или значение с $, обратной косой либо обратной
// кавычкой отклоняется безусловно; литеральная команда идёт через elevated.
// В главной команде systemd-run учитываются литеральные $ в позициях селектора.
// --scope и удалённая служба здесь не исполняются.
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
			if err := s.unitExecProperties(c, l.args, l.at, &l.o, fed); err != nil {
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

// unitExecProperties проверяет команду и Exec-свойства одной службы.
// Литеральный $ в позиции команды или подкоманды блокируется, если systemd
// раскроет окружение. Вызов за xargs/parallel без команды при killText
// отклоняется, поскольку его свойства задаст ввод.
func (s *killScan) unitExecProperties(c shellcmd.Call, args []string, offset int, o *systemdRunOptions, fed func(string) bool) error {
	if s.killText && afterXargs(c) && o.start == len(args) {
		s.addToolReason("systemd-run за xargs или parallel без команды: слова ввода станут его опциями и командой (-p ExecStartPre=…), их не видно до запуска"+hiddenTail, systemdRunHiddenHint, false)
	}
	if !o.expandEnvironmentNo && s.serviceCommandHasDynamicSelector(c, args, o.start, offset) {
		s.addToolReason("команда службы содержит литеральную подстановку systemd, которая может выбрать подкоманду", systemdRunHiddenHint, false)
	}
	return s.unitExecPropertyValues(c, o.properties, fed)
}

// serviceCommandHasDynamicSelector — в команде службы args[start:] литеральный
// $ на месте подкоманды podman, systemctl или loginctl; args — слова уровня
// systemd-run, offset — место args[0] в c.Args
func (s *killScan) serviceCommandHasDynamicSelector(c shellcmd.Call, args []string, start, offset int) bool {
	if start >= len(args) {
		return false
	}
	words := make([]string, len(args)-start)
	for i, arg := range args[start:] {
		if c.ArgLiteral(offset + start + i) {
			words[i] = singleQuotedWord(arg)
		} else {
			words[i] = arg
		}
	}
	calls, err := shellcmd.Calls(strings.Join(words, " "))
	if err != nil {
		s.tool.Logger().Debug("kill_guard: команду службы не разобрать для systemd-подстановок", "error", err)
		return false
	}
	for i := range calls {
		call := &calls[i]
		base := path.Base(call.Name)
		switch base {
		case "podman", "podman-remote":
			verb, _ := podmanStop(call.Args)
			if i := slices.Index(call.Args, verb); i >= 0 && call.ArgLiteral(i) && strings.Contains(verb, "$") {
				return true
			}
		case "systemctl", "loginctl":
			values := systemctlValues
			if base == "loginctl" {
				values = loginctlValues
			}
			operands := optionOperands(serviceCallWords(call), values)
			if len(operands) > 0 && operands[0].literal && strings.Contains(operands[0].text, "$") {
				return true
			}
		}
	}
	return false
}

func serviceCallWords(c *shellcmd.Call) []word {
	words := make([]word, len(c.Args))
	for i, arg := range c.Args {
		words[i] = word{text: arg, literal: c.ArgLiteral(i)}
	}
	return words
}

func (s *killScan) unitExecPropertyValues(c shellcmd.Call, properties []word, fed func(string) bool) error {
	for _, p := range properties {
		inner, reason, reject := execPropertyCommand(p.text, fed)
		if reject {
			s.addToolReason("systemd-run -p "+p.text+": "+reason+hiddenTail, systemdRunHiddenHint, false)
			continue
		}
		if inner != "" {
			if err := s.elevated(c, inner); err != nil {
				return err
			}
		}
	}
	return nil
}

// systemdExpansionChars — символы раскрытия systemd в значении Exec.
const systemdExpansionChars = "$\x60"

// execPropertyCommand разбирает Exec-свойство arg (слово -p из shellcmd).
// Пустые inner и reason означают не Exec-свойство или сброс списка.
func execPropertyCommand(arg string, fed func(string) bool) (inner, reason string, reject bool) {
	computed := computedArg(arg) || fed(arg)
	text := arg
	if computedArg(arg) {
		text = unquoted(arg)
	}
	key, value, found := strings.Cut(text, "=")
	if computed && (!found || strings.ContainsAny(key, systemdExpansionChars) || fed(key)) {
		return "", "имя свойства вычисляется при запуске и может задать Exec-команду", true
	}
	if !found || !execPropertyKey(key) {
		return "", "", false
	}
	if computed || strings.ContainsAny(value, "\\"+systemdExpansionChars) {
		return "", "значение " + key + " содержит вычисление или C-экранирование; команду нельзя проверить", true
	}
	inner, ok := systemdExecCommand(value)
	if !ok {
		return "", "команда " + key + " не разбирается", true
	}
	return inner, "", false
}

// systemdExecCommand разбирает командную строку Exec-свойства по правилам
// systemd.service («Command lines») и возвращает её для общей проверки.
func systemdExecCommand(value string) (string, bool) {
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
	switch {
	case !ok:
		return "", false
	case viaShell && len(words) == 0:
		return "sh", true
	case viaShell:
		var script execWord
		for k, w := range words {
			if k > 0 {
				script = append(script, execPart{text: " "})
			}
			script = append(script, w...)
		}
		return "sh -c " + script.shell(), true
	case len(words) == 0:
		return "", true
	case at && len(words) < 2:
		return "", false
	case at:
		words = slices.Delete(words, 1, 2)
	}
	out := make([]string, 0, len(words))
	for _, w := range words {
		out = append(out, w.shell())
	}
	return strings.Join(out, " "), true
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
		switch {
		case quote != 0 && ch == quote:
			quote = 0
			continue
		case quote == 0 && (ch == '\'' || ch == '"'):
			quote, inWord = ch, true
			continue
		case quote == 0 && strings.IndexByte(" \t\n\r", ch) >= 0:
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

// run0ValueOptions опции run0 со значением отдельным словом (run0 --help,
// systemd 259)
var run0ValueOptions = []string{"-u", "--user", "-g", "--group", "-D", "--chdir", "--machine", "--unit", "--property",
	"--description", "--slice", "--nice", "--setenv", "--background", "--shell-prompt-prefix", "--area", "--lightweight"}

// run0Options свойства --property run0, место первого слова команды и
// viaShell — --via-shell или -i: слова команды run0 склеит в скрипт sh -c.
// run0 запускает службу, как systemd-run, и Exec-свойства исполнит менеджер
func run0Options(args []string) (props []word, start int, viaShell bool) {
	i := 0
	for ; i < len(args) && strings.HasPrefix(args[i], "-") && args[i] != "-"; i++ {
		switch a := args[i]; {
		case a == "--":
			return props, i + 1, viaShell
		case a == "--via-shell" || a == "-i":
			viaShell = true
		case strings.HasPrefix(a, "--") && strings.Contains(a, "=") && uniqueLongOptionName(strings.SplitN(a, "=", 2)[0], run0ValueOptions, []string{"--via-shell", "--help", "--version"}) == "--property":
			v := strings.SplitN(a, "=", 2)[1]
			props = append(props, word{text: v, literal: !computedArg(v)})
		case uniqueLongOptionName(a, run0ValueOptions, []string{"--via-shell", "--help", "--version"}) == "--property" && i+1 < len(args):
			i++
			props = append(props, word{text: args[i], literal: !computedArg(args[i])})
		case slices.Contains(run0ValueOptions, a):
			i++
		}
	}
	return props, i, viaShell
}

// run0Service проверяет то, что служба run0 исполнит помимо видимой
// разбору команды: SuccessAction= и FailureAction=, как у systemd-run,
// Exec-свойства и подстановку окружения systemd в имени команды или её
// глаголе (run0 --setenv=K=ki podman '${K}ll')
func (s *killScan) run0Service(c shellcmd.Call) error {
	props, start, _ := run0Options(c.Args)
	if reason, hint := unitEndActionReason(props, (unitAction{}).scope(), "run0"); reason != "" {
		s.addToolReason(reason, hint, false)
	}
	if s.serviceCommandHasDynamicSelector(c, c.Args, start, 0) {
		s.addToolReason("команда службы run0 содержит литеральную подстановку systemd, которая может выбрать подкоманду", systemdRunHiddenHint, false)
	}
	return s.unitExecPropertyValues(c, props, func(string) bool { return false })
}
