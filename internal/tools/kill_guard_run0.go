package tools

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/aiseeq/akit/shellcmd"
	"mvdan.cc/sh/v3/syntax"
)

// run0 (man run0, systemd 259) запускает команду временной службой
// менеджера — тем же путём, что systemd-run --pipe без --scope: процесс —
// потомок PID 1, не агента, и ядро (agent-signal-guard) его сигналы не
// сверяет. Поэтому run0 и его опции в тексте команды заменяются на
// systemd-run --pipe с той же машиной и свойствами (rewriteRun0), и команда
// проверяется правилами systemd-run в режиме службы целиком: ввод, пайпы,
// heredoc, функции и coproc вокруг run0 разбор видит, как вокруг
// systemd-run. run0, которого нет в тексте словом (R=run0; $R …), и вызов,
// замена которого не разбирается, проверяются так же, но текстом за ним
// отдельно (run0Command, elevated)

// run0WordRe отдельное слово run0: под ним гасит контейнер и podman stop, а
// слова kill в команде может не быть
var run0WordRe = regexp.MustCompile(`(^|[^[:alnum:]_.-])run0([^[:alnum:]_.-]|$)`)

// run0LongValues длинные опции run0 со значением (run0 --help, systemd
// 259, required_argument)
var run0LongValues = []string{
	"machine", "unit", "property", "description", "slice", "user", "group", "nice", "chdir", "setenv",
	"background", "shell-prompt-prefix", "lightweight", "area",
}

// run0LongFlags длинные опции run0 без значения
var run0LongFlags = []string{
	"help", "version", "no-ask-password", "slice-inherit", "via-shell", "pty", "pty-late", "pipe", "empower",
}

// run0ShortValues, run0ShortFlags короткие опции run0 (getopt "+hVu:g:D:i"):
// -u, -g, -D со значением, -h, -V, -i без
const (
	run0ShortValues = "ugD"
	run0ShortFlags  = "hVi"
)

// run0Invocation разобранный вызов run0: опции, которые меняют, где и как
// исполнится команда, и сама команда (слова после опций)
type run0Invocation struct {
	machines, properties []string
	viaShell             bool
	// start — место первого слова команды в словах run0
	start   int
	command []string
}

// run0LongName полное имя длинной опции run0, как его находит getopt_long:
// точное совпадение или единственное имя, которое начинается с name; "" —
// нет такой или сокращение неоднозначно (run0 выйдет с ошибкой)
func run0LongName(name string) string {
	return uniqueLongOptionName(name, run0LongValues, run0LongFlags)
}

// uniqueLongOptionName разрешает полное имя или однозначное сокращение,
// как getopt_long для systemd-run и run0.
func uniqueLongOptionName(name string, values, flags []string) string {
	var found string
	for _, n := range slices.Concat(values, flags) {
		if len(n) < 2 {
			continue
		}
		switch {
		case n == name:
			return n
		case name != "" && strings.HasPrefix(n, name):
			if found != "" {
				return ""
			}
			found = n
		}
	}
	return found
}

// parseRun0 разбирает слова run0 так, как getopt_long с "+": опции до
// первого операнда или --, сокращения длинных (--us), связки коротких (-iu
// root, -iuroot). ok false — run0 ничего не запустит: --help, --version,
// неизвестная или неоднозначная опция, нет значения
func parseRun0(args []string) (inv run0Invocation, ok bool) {
	i := 0
	for ; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			i++
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			break
		}
		if !strings.HasPrefix(a, "--") {
			via, done, ok := run0Short(args, &i)
			if !ok || done {
				return run0Invocation{}, false
			}
			inv.viaShell = inv.viaShell || via
			continue
		}
		name, value, done, ok := run0Long(args, &i)
		if !ok || done {
			return run0Invocation{}, false
		}
		inv.apply(name, value)
	}
	inv.start, inv.command = i, args[i:]
	return inv, true
}

// run0Long длинная опция run0 в args[*i]: полное имя и значение; done —
// --help или --version (run0 печатает и выходит); ok false — ошибка getopt
func run0Long(args []string, i *int) (name, value string, done, ok bool) {
	n, v, inline := strings.Cut(args[*i][2:], "=")
	name = run0LongName(n)
	takes := slices.Contains(run0LongValues, name)
	switch {
	case name == "", inline && !takes:
		return "", "", false, false
	case takes && !inline:
		if *i+1 >= len(args) {
			return "", "", false, false
		}
		*i++
		v = args[*i]
	}
	return name, v, name == "help" || name == "version", true
}

// run0Short связка коротких опций run0 в args[*i]: значение берёт первая
// буква со значением (-u, -g, -D) — остаток связки или следующее слово, и
// связка на ней кончается. via — в связке есть -i (--via-shell); done — -h
// или -V (run0 печатает и выходит); ok false — неизвестная буква, нет
// значения
func run0Short(args []string, i *int) (via, done, ok bool) {
	a := args[*i]
	for j := 1; j < len(a); j++ {
		l := a[j : j+1]
		switch {
		case l == "i":
			via = true
		case strings.Contains(run0ShortFlags, l):
			return via, true, true
		case strings.Contains(run0ShortValues, l):
			if j+1 == len(a) {
				if *i+1 >= len(args) {
					return false, false, false
				}
				*i++
			}
			return via, false, true
		default:
			return false, false, false
		}
	}
	return via, false, true
}

// apply запоминает опцию, от которой зависит проверка: машину (команда
// исполнится в контейнере — как systemd-run -M), свойства службы
// (SuccessAction= — как у systemd-run -p) и запуск через оболочку
func (inv *run0Invocation) apply(name, value string) {
	switch name {
	case "machine":
		inv.machines = append(inv.machines, value)
	case "property":
		inv.properties = append(inv.properties, value)
	case "via-shell":
		inv.viaShell = true
	}
}

// run0Command текст systemd-run, которым run0 исполнит команду: systemd-run
// --pipe с машиной и свойствами run0 и той же командой. run0 без команды
// запускает оболочку, читающую ввод; --via-shell (и -i) склеивает слова
// через пробел и отдаёт оболочке -c (systemd v259, exec-invoke.c:
// strv_join(argv, " ")). ok false — run0 ничего не запустит
func run0Command(args []string) (string, bool) {
	inv, ok := parseRun0(args)
	if !ok {
		return "", false
	}
	words := []string{"systemd-run", "--pipe"}
	for _, m := range inv.machines {
		words = append(words, "--machine="+m)
	}
	for _, p := range inv.properties {
		words = append(words, "--property="+p)
	}
	words = append(words, "--")
	text := shellWords(words)
	switch {
	case len(inv.command) == 0:
		return text + " sh", true
	case inv.viaShell:
		return text + " sh -c " + singleQuotedWord(strings.Join(inv.command, " ")), true
	}
	return text + " " + shellWords(inv.command), true
}

// maxRun0Rewrites предел замен run0 в одной команде
const maxRun0Rewrites = 16

// rewriteRun0 команда, где каждый вызов run0, стоящий в тексте словом
// (и во вложенном скрипте, heredoc), заменён на systemd-run --pipe
// (replaceRun0), и run0 за обёрткой, которого разбор не отдаёт (wrappedRun0).
// rewritten — замена была. Команду, которую shellcmd не разобрал, разбор
// всей команды (parsedCalls) потом отклонит сам; err — первая ошибка разбора
// слов вызова run0 (run0Words, wrappedRun0): такой вызов остаётся в тексте,
// и его проверяет текст run0Command (rewrittenCommand)
func rewriteRun0(command string) (text string, rewritten bool, err error) {
	text = command
	for range maxRun0Rewrites {
		calls, perr := shellcmd.Calls(text)
		if calls == nil && perr != nil {
			return text, rewritten, err
		}
		calls, serr := wrappedRun0Calls(text, calls)
		if err == nil {
			err = serr
		}
		next, ok := "", false
		for i := range calls {
			c := &calls[i]
			if c.Name != "run0" {
				continue
			}
			var werr error
			if next, ok, werr = replaceRun0(text, *c); ok {
				break
			}
			if err == nil {
				err = werr
			}
		}
		if !ok {
			return text, rewritten, err
		}
		text, rewritten = next, true
	}
	return text, rewritten, err
}

// safeValueRe значение опции run0, которое переносится в текст systemd-run
// без кавычек в любом месте команды (и внутри '…', "…" вложенного скрипта)
var safeValueRe = regexp.MustCompile(`^[A-Za-z0-9_.@=:/,+%-]*$`)

// replaceRun0 text, где вызов run0 c заменён на systemd-run: имя и опции —
// на systemd-run --pipe с машиной и свойствами run0 и --, слова команды и
// ввод остаются, у run0 без команды команда — sh. Текст замены — без
// кавычек и подстановок (safeValueRe): он верен и внутри '…', "…"
// вложенного скрипта. ok false — c нет в тексте словом (имя из переменной,
// текст вложенного скрипта иной, чем в команде), run0 ничего не запустит,
// значение опции требует кавычек или команда идёт через --via-shell (её
// слова — один скрипт sh -c, в кавычках, которые внутри '…' не поставить).
// err — текст вызова не разобран на слова (run0Words)
func replaceRun0(text string, c shellcmd.Call) (string, bool, error) {
	at, spans, err := run0Words(text, c)
	if err != nil {
		return "", false, err
	}
	if spans == nil {
		return "", false, nil
	}
	inv, ok := parseRun0(c.Args)
	if !ok || inv.viaShell && len(inv.command) > 0 {
		return "", false, nil
	}
	words := []string{"systemd-run", "--pipe"}
	for _, v := range slices.Concat(inv.machines, inv.properties) {
		if !safeValueRe.MatchString(v) {
			return "", false, nil
		}
	}
	for _, m := range inv.machines {
		words = append(words, "--machine="+m)
	}
	for _, p := range inv.properties {
		words = append(words, "--property="+p)
	}
	words = append(words, "--")
	end := spans[len(c.Args)][1]
	repl := strings.Join(words, " ")
	if len(inv.command) == 0 {
		repl += " sh"
	} else {
		end = spans[1+inv.start][0]
		repl += " "
	}
	return text[:c.Offset] + repl + text[at+end:], true, nil
}

// run0Words места слов вызова run0 c в text: at — начало текста вызова
// (c.Raw; у run0 за обёрткой — текст обёртки, nice run0 …), spans — слова
// от имени run0 (spans[0]) и его аргументов, относительно at. spans nil —
// c нет в тексте словом; err — c.Raw не разобран на слова
func run0Words(text string, c shellcmd.Call) (at int, spans [][2]int, err error) {
	if c.Offset < 0 {
		return 0, nil, nil
	}
	var words [][2]int
	err = syntax.NewParser().Words(strings.NewReader(c.Raw), func(w *syntax.Word) bool {
		words = append(words, [2]int{int(w.Pos().Offset()), int(w.End().Offset())})
		return true
	})
	if err != nil {
		return 0, nil, fmt.Errorf("слова run0 в %q: %w", c.Raw, err)
	}
	for k, w := range words {
		at = c.Offset - w[0]
		if at < 0 || at+len(c.Raw) > len(text) || text[at:at+len(c.Raw)] != c.Raw || len(words)-k-1 < len(c.Args) {
			continue
		}
		return at, words[k:], nil
	}
	return 0, nil, nil
}

// run0Hint подсказка к отказу в команде, где run0 заменён на systemd-run
const run0Hint = "run0 проверен как systemd-run --pipe: он запускает команду временной службой менеджера systemd (man run0), а не потомком агента. Сигнал шли без run0 (sudo kill 12345 оставляет процесс потомком агента)"

// singleQuotedWord текст одним словом в одинарных кавычках
func singleQuotedWord(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// scriptValues опции script (util-linux) со значением отдельным словом
var scriptValues = []string{"c", "command", "E", "echo", "I", "log-in", "O", "log-out", "B", "log-io", "T", "log-timing",
	"m", "logging-format", "o", "output-limit"}

// scriptShell — script без -c (--command) запускает оболочку ($SHELL),
// которая читает его ввод: script -q /dev/null
func scriptShell(args []string) bool {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return true
		case strings.HasPrefix(a, "--"):
			name, _, inline := strings.Cut(a[2:], "=")
			if name == "command" || name == "help" || name == "version" {
				return false
			}
			if !inline && slices.Contains(scriptValues, name) {
				i++
			}
		case strings.HasPrefix(a, "-") && a != "-":
			letter, rest, next := shortValue(a, scriptValues)
			if letter == "c" || strings.ContainsAny(strings.TrimSuffix(a[1:], letter+rest), "hV") {
				return false
			}
			if next {
				i++
			}
		}
	}
	return true
}

// scriptCommandMissing says that script has -c/--command but no command
// operand yet; parallel can supply that operand as its implicit last argument.
func scriptCommandMissing(args []string) bool {
	for i, a := range args {
		if a == "--command" {
			return i+1 == len(args)
		}
		if after, ok := strings.CutPrefix(a, "--command="); ok {
			return after == ""
		}
		if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && a != "-" {
			if k := strings.IndexByte(a[1:], 'c'); k >= 0 {
				return k+2 == len(a) && i+1 == len(args)
			}
		}
	}
	return false
}

// parallelShell текст, который parallel без команды исполнит оболочкой:
// строки ввода (parallel <<< 'kill 1') — как sh, читающая ввод; аргументы
// списка ::: становятся строками скрипта, включая аргументы из файлового
// списка. В semaphore-режиме sem исполняет переданную команду напрямую.
// Файл аргументов из stdin без списка ::: читается как команды оболочки.
func parallelShell(args []string) (string, bool) {
	return parallelShellCall(args, nil)
}

// parallelShellCall учитывает статический ввод heredoc/here-string, который
// parallel читает как файл аргументов.
func parallelShellCall(args []string, call *shellcmd.Call) (string, bool) {
	p := parseParallel(args)
	input, inputKnown := "", false
	if call != nil {
		input, inputKnown = parallelLiteralInput(*call)
	}
	if p.failed || p.remote {
		return "", false
	}
	if p.semaphore {
		return parallelSemaphoreShell(p)
	}
	if len(p.words) == 0 && !p.listed {
		return "sh", true
	}
	if len(p.words) > 0 && !p.sourceWord(p.words[0]) {
		// A regular command is analyzed by parallelSystemdRun and the normal
		// call scanner; this rewrite is only for jobs whose command comes from
		// a source value.
		return "", false
	}
	return parallelSourcesShell(p, input, inputKnown)
}

func parallelSemaphoreShell(p parallelCommand) (string, bool) {
	if p.wait {
		return "true", true
	}
	return strings.Join(p.words, " "), len(p.words) > 0
}

// parallelSourcesShell builds candidate command lines from the parsed source
// model. Each source is one column; a row from every source is appended after
// the command words in GNU parallel source order.
func parallelSourcesShell(p parallelCommand, input string, inputKnown bool) (string, bool) {
	base := parallelCommandWords(p)
	if len(base) == 0 && parallelOpaqueCommandFirst(p, input, inputKnown) {
		return "true", true
	}
	columns := make([]parallelSourceColumn, 0, len(p.sources))
	unknownStdin := false
	for _, src := range p.sources {
		rows, unknown := parallelSourceRows(p, src, input, inputKnown)
		unknownStdin = unknownStdin || unknown
		if len(rows) > 0 {
			columns = append(columns, parallelSourceColumn{rows: rows, linked: src.linked})
		}
	}
	if unknownStdin {
		// Unknown stdin can occupy the command name or an argument position.
		return strings.Join(append(slices.Clone(base), "kill $PARALLEL_ARG"), " "), true
	}
	if len(columns) == 0 {
		return strings.Join(base, " "), len(base) > 0
	}
	candidates, bounded := parallelSourceCandidates(base, columns)
	if !bounded {
		return parallelUnknownSourceCommand(base), true
	}
	var lines []string
	for _, candidate := range candidates {
		line := candidate.words
		if p.quote {
			for j := range line {
				line[j] = parallelShellQuote(line[j])
			}
		}
		if len(line) > 0 {
			lines = append(lines, strings.Join(line, " "))
		}
	}
	if len(base) == 0 && len(lines) > 0 {
		return strings.Join(lines, "\n"), true
	}
	return strings.Join(lines, "\n"), len(lines) > 0
}

type parallelSourceColumn struct {
	rows   [][]string
	linked bool
}

type parallelSourceCandidate struct {
	words []string
	index int
}

func parallelCommandWords(p parallelCommand) []string {
	words := p.words
	if i := slices.IndexFunc(words, p.sourceWord); i >= 0 {
		words = words[:i]
	}
	return words
}

func parallelOpaqueCommandFirst(p parallelCommand, input string, inputKnown bool) bool {
	for _, src := range p.sources {
		if src.kind == 'f' {
			return true
		}
		if src.kind == 'l' && len(src.values) > 0 {
			return false
		}
		if src.kind == 'i' && (!inputKnown || input != "") {
			return false
		}
	}
	return false
}

func parallelSourceRows(p parallelCommand, src parallelSource, input string, inputKnown bool) ([][]string, bool) {
	var values []string
	switch src.kind {
	case 'l':
		values = src.values
	case 'i':
		if !inputKnown {
			return nil, true
		}
		for row := range strings.SplitSeq(strings.TrimSuffix(input, "\n"), "\n") {
			if row != "" {
				values = append(values, row)
			}
		}
	}
	rows := make([][]string, 0, len(values))
	for _, value := range values {
		rows = append(rows, parallelFields(p, value))
	}
	return rows, false
}

func parallelSourceCandidates(base []string, columns []parallelSourceColumn) ([]parallelSourceCandidate, bool) {
	candidates := []parallelSourceCandidate{{words: slices.Clone(base)}}
	for i, col := range columns {
		candidates = parallelExtendSourceCandidates(candidates, col, i > 0)
		if len(candidates) > 256 {
			return nil, false
		}
	}
	return candidates, true
}

func parallelExtendSourceCandidates(previous []parallelSourceCandidate, column parallelSourceColumn, hasPrevious bool) []parallelSourceCandidate {
	var next []parallelSourceCandidate
	for _, candidate := range previous {
		if column.linked && hasPrevious {
			if candidate.index < len(column.rows) {
				words := append(slices.Clone(candidate.words), column.rows[candidate.index]...)
				next = append(next, parallelSourceCandidate{words: words, index: candidate.index})
			}
			continue
		}
		for row, values := range column.rows {
			words := append(slices.Clone(candidate.words), values...)
			next = append(next, parallelSourceCandidate{words: words, index: row})
		}
	}
	return next
}

func parallelUnknownSourceCommand(base []string) string {
	return strings.Join(append(slices.Clone(base), "kill $PARALLEL_ARG"), " ")
}

func parallelShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func parallelFields(p parallelCommand, value string) []string {
	fields := []string{value}
	if p.colsep != "" {
		if sep, err := regexp.Compile(p.colsep); err == nil {
			fields = sep.Split(value, -1)
		} else {
			return []string{"$PARALLEL_ARG"}
		}
	}
	return fields
}

// parallelLiteralInput возвращает статическое содержимое here-string;
// вычисляемый ввод и heredoc остаются неизвестными.
func parallelLiteralInput(c shellcmd.Call) (string, bool) {
	for _, r := range c.Redirs {
		if r.Op == "<<<" {
			if strings.ContainsAny(r.Target, "$`\\") {
				return "", false
			}
			return r.Target, true
		}
	}
	return "", false
}

// rewrittenCommand текст, которым проверяется вызов c вместо него самого:
// run0 — systemd-run в режиме службы (run0Command); pkexec и machinectl
// shell с командой — их команда (elevatedCommand); программа, которая
// запускает оболочку на своём вводе (script без -c, machinectl shell без
// команды, parallel без команды), — эта оболочка (sh); systemd-run --shell,
// не переписанный в тексте, — systemd-run --pipe bash; sem — parallel
// --semaphore.
func rewrittenCommand(c shellcmd.Call) (string, bool) {
	switch c.Name {
	case "run0":
		return run0Command(c.Args)
	case "systemd-run":
		return systemdRunShellCommand(c.Args)
	case "script":
		if scriptShell(c.Args) {
			return "sh", true
		}
		return "", false
	case "parallel":
		return parallelShellCall(c.Args, &c)
	case "sem":
		return parallelShellCall(append([]string{"--semaphore"}, c.Args...), &c)
	}
	return elevatedCommand(c)
}

// inputRedirs перенаправления ввода вызова c текстом для разбора вместе с
// его командой: тело heredoc, <<< и < разбор прочтёт у оболочки за
// systemd-run так же, как в исходной команде. Тело heredoc — под
// ограничителем в кавычках: подстановки в нём оболочка за обёрткой видит
// текстом, как скрипт
func inputRedirs(c shellcmd.Call) (string, error) {
	var out strings.Builder
	var bodies []string
	for _, r := range c.Redirs {
		if r.N != "" && r.N != "0" || !strings.HasPrefix(r.Op, "<") {
			continue
		}
		switch r.Op {
		case "<<", "<<-":
			end := heredocEnd(r.Body)
			out.WriteString(" " + r.Op + "'" + end + "'")
			bodies = append(bodies, heredocText(r.Body, end))
		default:
			word, err := redirWord(r.Target)
			if err != nil {
				return "", err
			}
			out.WriteString(" " + r.Op + " " + word)
		}
	}
	for _, b := range bodies {
		out.WriteString("\n" + b)
	}
	return out.String(), nil
}

// heredocText тело heredoc со строкой ограничителя end в конце
func heredocText(body, end string) string {
	if body == "" || strings.HasSuffix(body, "\n") {
		return body + end
	}
	return body + "\n" + end
}

// heredocEnd ограничитель тела heredoc в переписанной команде: слово,
// которого нет в теле, — иначе тело кончилось бы раньше
func heredocEnd(body string) string {
	for n := 0; ; n++ {
		if end := "CHOOKS_END" + strings.Repeat("_", n); !strings.Contains(body, end) {
			return end
		}
	}
}

// redirWord цель перенаправления словом оболочки: исходный текст слова с
// подстановкой или кавычками ("$F", <(…)) — как есть, значение литерала — в
// кавычках. err — значение в кавычки не поставить (нулевой байт)
func redirWord(target string) (string, error) {
	if strings.ContainsAny(target, "$`\"'\\") || strings.HasPrefix(target, "<(") || strings.HasPrefix(target, ">(") {
		return target, nil
	}
	q, err := syntax.Quote(target, syntax.LangBash)
	if err != nil {
		return "", fmt.Errorf("цель %q перенаправления ввода: %w", target, err)
	}
	return q, nil
}

// systemdRunCommandStart место первого слова команды в словах systemd-run
// (опции до первого операнда или --, "+" в getopt); len(args) — команды нет
func systemdRunCommandStart(args []string) int {
	return parseSystemdRunOptions(plainWords(args)).start
}

// bareParallel вызов parallel без команды под systemd-run (systemd-run
// --pipe parallel <<< 'kill 1', systemd-run setsid parallel ::: 'kill 1'):
// разбирает команду службы отдельно, поскольку основной разбор shellcmd
// отдаёт только самый внутренний вызов, а у parallel без команды его нет,
// хотя parallel исполнит строки ввода или списка ::: оболочкой
// (parallelShell). nestedCommand находит parallel за обёртками тем же
// разбором, которым восстанавливаются systemd-run и run0. Вызов — с Via до
// systemd-run и внутренних обёрток, местом, вводом и перенаправлениями
// systemd-run. err — слова не разобраны (не UTF-8)
func bareParallel(c shellcmd.Call) (shellcmd.Call, bool, error) {
	start := systemdRunCommandStart(c.Args)
	if c.Name != "systemd-run" || start >= len(c.Args) {
		return shellcmd.Call{}, false, nil
	}
	words := c.Args[start:]
	if _, err := shellcmd.Calls(literalShellWords(words)); err != nil {
		return shellcmd.Call{}, false, fmt.Errorf("команда systemd-run %q: %w", words, err)
	}
	j, via, err := nestedCommand(words, "parallel")
	if err != nil {
		return shellcmd.Call{}, false, fmt.Errorf("команда systemd-run %q: %w", words, err)
	}
	sem := false
	if j < 0 {
		j, via, err = nestedCommand(words, "sem")
		if err != nil {
			return shellcmd.Call{}, false, fmt.Errorf("команда systemd-run %q: %w", words, err)
		}
		sem = j >= 0
	}
	if j >= 0 {
		args := words[j+1:]
		if sem {
			args = append([]string{"--semaphore"}, args...)
		}
		return shellcmd.Call{Name: "parallel", Args: args, Via: slices.Concat(c.Via, []string{"systemd-run"}, via),
			Raw: c.Raw, Offset: c.Offset, Redirs: c.Redirs, ScriptInput: c.ScriptInput}, true, nil
	}
	return shellcmd.Call{}, false, nil
}
