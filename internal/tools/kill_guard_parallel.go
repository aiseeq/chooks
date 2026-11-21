package tools

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/aiseeq/akit/shellcmd"
	"mvdan.cc/sh/v3/syntax"
)

// replacementRe строка замены parallel ({}, {.}, {/}, {1}, {#}, {%}, {=…=}):
// аргумент встаёт на её место, а не в конец команды
var replacementRe = regexp.MustCompile(`\{[0-9]*[./#%]*[./]*\}|\{=`)
var parallelImplicitShellCommandRe = regexp.MustCompile(`(?:^|[[:space:]])(?:sudo|doas|pkexec|su|run0|systemd-run)(?:[[:space:]]+[^|;&]*)?[[:space:]]+(?:bash|sh|dash|zsh|ksh|mksh|ksh93|fish)(?:[[:space:]]+[^|;&]*)?[[:space:]]+-[a-zA-Z]*c[a-zA-Z]*[[:space:]]*$`)
var parallelProtectedCommandRe = regexp.MustCompile(`(^|[^[:alnum:]_.-])(systemd-run|run0|pkexec|sudo|doas|su)([^[:alnum:]_.-]|$)`)
var parallelShellCRe = regexp.MustCompile(`(^|[[:space:]])(?:bash|sh|dash|zsh|ksh|mksh|ksh93|fish)(?:[[:space:]]+[^|;&]*)?[[:space:]]+-[a-zA-Z]*c[a-zA-Z]*(?:[[:space:]]|$)`)

// parallelCommand хранит разобранные parallelGetopt опции и оставшиеся слова.
type parallelCommand struct {
	words                       []string
	pipe, quote, listed, remote bool
	cat, fifo                   bool
	argInput                    bool
	customArgSep                bool
	semaphore, wait             bool
	complexSource               bool
	sqlMode                     bool
	failed                      bool
	replaces                    []string
}

// parallelPipeOptions опции, с которыми ввод идёт командам в stdin
// (--round-robin, --fifo, --cat и --group-by включают --pipe)
var parallelPipeOptions = []string{"pipe", "pipe-part", "round-robin", "fifo", "cat", "group-by"}

const maxParallelSourceCombinations = 256

// parallelReplaceOptions опции, задающие свою строку замены вместо {}, {.},
// {/}, {//}, {/.}, {#}, {%}
var parallelReplaceOptions = []string{"I", "replace", "extensionreplace", "basenamereplace", "dirnamereplace",
	"basenameextensionreplace", "seqreplace", "slotreplace"}

// parseParallel разбирает слова parallel (parallelGetopt)
func parseParallel(args []string) parallelCommand {
	sets, words, ok := parallelGetopt(args)
	p := parallelCommand{words: words, failed: !ok}
	var logins []string
	files := false
	for _, o := range sets {
		p.applyOption(o, &logins, &files)
	}
	local, hosts := parallelLocalLogins(logins)
	p.remote = !local && (hosts || files)
	p.listed = slices.ContainsFunc(sets, func(o parallelSet) bool { return o.name == "arg-file" }) ||
		slices.ContainsFunc(p.words, p.sourceWord)
	p.argInput = p.argInput || parallelArgInput(p, p.words)
	p.complexSource = p.complexSource || parallelComplexSources(p, sets)
	p.semaphore = slices.ContainsFunc(sets, func(o parallelSet) bool {
		return slices.Contains([]string{"semaphore", "fg", "bg", "id", "semaphore-name", "semaphore-timeout"}, o.name)
	})
	return p
}

func (p *parallelCommand) applyOption(o parallelSet, logins *[]string, files *bool) {
	switch {
	case slices.Contains(parallelPipeOptions, o.name):
		p.setPipeOption(o.name)
	case o.name == "quote":
		p.quote = true
	case o.name == "rpl":
		p.complexSource = true
		if f := strings.Fields(o.value); len(f) > 0 {
			p.replaces = append(p.replaces, f[0])
		}
	case slices.Contains(parallelReplaceOptions, o.name) && o.value != "":
		p.complexSource = true
		p.replaces = append(p.replaces, o.value)
	case o.name == "sshlogin":
		*logins = append(*logins, o.value)
	case o.name == "sshloginfile":
		*files = true
	case o.name == "arg-sep" || o.name == "arg-file-sep":
		p.customArgSep = true
		p.complexSource = true
	case slices.Contains([]string{"sql", "sql-master", "sql-and-worker", "sql-worker"}, o.name):
		p.sqlMode = true
		p.complexSource = true
	case o.name == "arg-file" && parallelStdinFile(o.value):
		p.argInput = true
	case parallelComplexOption(o.name):
		p.complexSource = true
	case o.name == "wait":
		p.wait = true
	}
}

func parallelWordSourceCounts(p parallelCommand) (lists, stdin, files int, linked bool, combinations int) {
	combinations = 1
	for i, w := range p.words {
		if w == ":::" {
			lists++
			combinations = parallelListCombinationCount(p.words, i, combinations)
		}
		if w == ":::"+"+" || w == "::::+" {
			linked = true
		}
		if w == "::::" || w == "::::+" {
			in, file := parallelFilesAfter(p, i)
			stdin += in
			files += file
		}
	}
	return lists, stdin, files, linked, combinations
}

func parallelListCombinationCount(words []string, start, combinations int) int {
	end := start + 1
	for end < len(words) && words[end] != ":::" && words[end] != ":::"+"+" && words[end] != "::::" && words[end] != "::::+" {
		end++
	}
	count := end - start - 1
	if count == 0 || combinations > maxParallelSourceCombinations {
		return combinations
	}
	if combinations > maxParallelSourceCombinations/count {
		return maxParallelSourceCombinations + 1
	}
	return combinations * count
}

func parallelFilesAfter(p parallelCommand, start int) (stdin, files int) {
	for _, path := range p.words[start+1:] {
		if p.sourceWord(path) {
			break
		}
		if parallelStdinFile(path) {
			stdin++
		} else {
			files++
		}
	}
	return stdin, files
}

func (p *parallelCommand) setPipeOption(name string) {
	p.pipe = true
	p.cat = p.cat || name == "cat"
	p.fifo = p.fifo || name == "fifo"
}

// sourceWord разделитель списка parallel, включая --arg-file-sep
func (p parallelCommand) sourceWord(w string) bool {
	return w == ":::" || w == ":::+" || p.fileSource(w)
}

// fileSource разделитель файлов аргументов parallel
func (p parallelCommand) fileSource(w string) bool {
	return w == "::::" || w == "::::+"
}

func parallelArgInput(p parallelCommand, words []string) bool {
	for i, w := range words {
		if w != "::::" {
			continue
		}
		for _, path := range words[i+1:] {
			if p.sourceWord(path) {
				break
			}
			if parallelStdinFile(path) {
				return true
			}
		}
	}
	return false
}

// parallelStdinFile путь, которым parallel читает аргументы из stdin
func parallelStdinFile(path string) bool {
	switch path {
	case "-", "/dev/stdin", "/dev/fd/0", "/proc/self/fd/0":
		return true
	default:
		return false
	}
}

// parallelSystemdRunReason причина отказа на systemd-run в команде parallel
// (parallelSystemdRun)
func (s *killScan) parallelSystemdRunReason(c shellcmd.Call) string {
	reason, _ := s.parallelSystemdRun(c)
	return reason
}

func parallelComplexSourceReason(s *killScan, c shellcmd.Call) string {
	p := parseParallel(c.Args)
	if p.failed || p.remote {
		return ""
	}
	if p.argInput && (!parallelLiteralInputSource(s, c) || parallelInputRedirectCount(c) > 1) {
		p.complexSource = true
	}
	if parallelSourceWrapper(p) {
		p.complexSource = true
	}
	if !p.complexSource {
		return ""
	}
	const reason = "команду parallel под службой составляют источники в сложной форме (связанные, файловые, смешанные или изменяющие сборку строки): что запустится, не видно"
	defines, err := parallelSourcesDefineCommand(p)
	if err != nil {
		s.tool.Logger().Warn("kill_guard: источник parallel не разобран", "error", err)
		return reason
	}
	if !defines || !parallelComplexSourceNeedsRefusal(s, c, p) {
		return ""
	}
	return reason
}

// parallelInnerCalls вызовы команды parallel для проверки, как вызовы
// скрипта (parallelSystemdRun)
func (s *killScan) parallelInnerCalls(c shellcmd.Call) []shellcmd.Call {
	if c.Name == "sem" {
		if script, ok := parallelShellCall(append([]string{"--semaphore"}, c.Args...), &c); ok {
			if calls, err := shellcmd.Calls(script); err == nil {
				for i := range calls {
					calls[i].Via = slices.Concat(c.Via, []string{"parallel"}, calls[i].Via)
					calls[i].Offset = -1
				}
				return calls
			}
		}
	}
	if c.Name == "setsid" {
		parsed, err := shellcmd.Calls(shellWords(c.Args))
		if err == nil {
			for i := range parsed {
				if parsed[i].Name == "parallel" {
					parsed[i].Via = slices.Concat(c.Via, []string{"setsid"}, parsed[i].Via)
					parsed[i].Offset = c.Offset
					parsed[i].Redirs = c.Redirs
					_, calls := s.parallelSystemdRun(parsed[i])
					return calls
				}
			}
		}
	}
	_, calls := s.parallelSystemdRun(c)
	return calls
}

// parallelSystemdRun systemd-run в команде parallel (shellcmd её не
// снимает). Слова команды parallel склеивает в строку для оболочки (с -q —
// каждое в кавычках): строка разбирается как скрипт sh -c, а её вызовы
// возвращаются для проверки (с Via «parallel» и без места) — так имя из
// ввода (systemd-run --user {}) ловит то же правило имени под systemd-run.
// Строки ввода parallel ставит в конец команды (без строки замены): оболочка
// под systemd-run без скрипта (bash, bash -c, bash -s) получит их как
// скрипт или файл скрипта — отказ при любом вводе из пайпа, как за xargs. С
// --pipe ввод идёт в stdin команды: оболочка, читающая его, решается как у
// прямого пайпа (литеральный текст разбирается). Аргументы из списка (:::,
// -a) и команда на других машинах (-S host) — вне проверки
func (s *killScan) parallelSystemdRun(c shellcmd.Call) (string, []shellcmd.Call) {
	if c.Name != "parallel" {
		return "", nil
	}
	p := parseParallel(c.Args)
	if p.failed || p.remote {
		return "", nil
	}
	if reason := parallelComplexSourceReason(s, c); reason != "" {
		return reason, nil
	}
	if len(p.words) == 0 {
		return "", nil
	}
	commandOnly := p
	if end := slices.IndexFunc(p.words, p.sourceWord); end >= 0 {
		commandOnly.words = p.words[:end]
	}
	script, err := commandOnly.script()
	if err == nil {
		var calls []shellcmd.Call
		calls, err = shellcmd.Calls(script)
		if err == nil {
			return s.parallelCalls(c, p, script, calls)
		}
	}
	if parallelScriptNeedsImplicitInput(p) {
		in := s.inputOf(c)
		if (!p.pipe && (in.src == stdinPipe || in.src == stdinFile || in.src == stdinRedirect) && !knownHarmlessInput(in)) ||
			p.listed && !parallelSourcesHarmless(p) {
			return "parallel передаёт неизвестную строку команде оболочки за привилегированной обёрткой", nil
		}
	}
	s.tool.Logger().Warn("kill_guard: команда parallel не разобрана", "error", err)
	if strings.Contains(strings.Join(p.words, " "), "systemd-run") {
		return "systemd-run за parallel: команду parallel разобрать не удалось, что запустится, не видно" + hiddenTail, nil
	}
	return "", nil
}

func parallelInputRedirectCount(c shellcmd.Call) int {
	n := 0
	for _, r := range c.Redirs {
		if (r.N == "" || r.N == "0") && strings.HasPrefix(r.Op, "<") {
			n++
		}
	}
	return n
}

func parallelLiteralInputSource(s *killScan, c shellcmd.Call) bool {
	for _, r := range c.Redirs {
		if r.N != "" && r.N != "0" {
			continue
		}
		switch r.Op {
		case "<<<":
			return !strings.ContainsAny(r.Target, "$`\\")
		case "<<", "<<-":
			return !strings.ContainsAny(r.Body, "$`\\")
		case "<":
			return false
		}
	}
	in := s.inputOf(c)
	if in.feed == "" {
		return false
	}
	calls, err := shellcmd.Calls(in.feed)
	if err != nil {
		s.tool.Logger().Warn("kill_guard: литеральный источник parallel не разобран", "error", err)
		return false
	}
	if len(calls) != 2 || calls[1].Name != "sh" {
		return false
	}
	return (calls[0].Name == "echo" || calls[0].Name == "printf") && !strings.ContainsAny(echoedValue(calls[0]), "$`\\")
}

func parallelSourcesDefineCommand(p parallelCommand) (bool, error) {
	if p.sqlMode && p.semaphore && p.wait {
		return true, nil
	}
	if parallelSourceWrapper(p) {
		return true, nil
	}
	if !(p.listed || p.argInput || p.customArgSep || len(p.replaces) > 0) {
		return false, nil
	}
	if len(p.words) == 0 {
		return true, nil
	}
	commandText := strings.Join(p.words, " ")
	if parallelReplacementWord(p, commandText) {
		return true, nil
	}
	end := slices.IndexFunc(p.words, p.sourceWord)
	if end < 0 {
		return p.customArgSep || p.argInput && parallelShellCRe.MatchString(commandText), nil
	}
	return parallelSourceWordDefinesCommand(p, commandText, end)
}

func parallelReplacementWord(p parallelCommand, commandText string) bool {
	first := p.words[0]
	return replacementRe.MatchString(first) || slices.Contains(p.replaces, first) || parallelReplacementCommand(p, commandText)
}

func parallelSourceWordDefinesCommand(p parallelCommand, commandText string, end int) (bool, error) {
	prefix := strings.Join(p.words[:end], " ")
	if strings.TrimSpace(prefix) == "" {
		return true, nil
	}
	readsSource, err := parallelShellReadsSource(prefix)
	if err != nil {
		return false, fmt.Errorf("parallel shell source: %w", err)
	}
	if readsSource || !p.listed {
		return readsSource, nil
	}
	readsSource, err = parallelShellReadsSource(commandText)
	if err != nil {
		return false, fmt.Errorf("parallel listed shell source: %w", err)
	}
	return readsSource, nil
}

func parallelReplacementCommand(p parallelCommand, commandText string) bool {
	if p.listed && parallelProtectedCommandRe.MatchString(commandText) {
		return true
	}
	return len(p.replaces) > 0 && (parallelProtectedCommandRe.MatchString(commandText) || parallelShellCRe.MatchString(commandText))
}

func parallelShellReadsSource(text string) (bool, error) {
	calls, err := shellcmd.Calls(text)
	if err != nil {
		return false, fmt.Errorf("parallel shell command: %w", err)
	}
	for i := range calls {
		if shellcmd.IsShell(calls[i].Name) && (shellReadsInput(calls[i].Args) || scriptUsesImplicitArgument(calls[i])) {
			return true, nil
		}
	}
	return false, nil
}

func parallelComplexSourceNeedsRefusal(s *killScan, c shellcmd.Call, p parallelCommand) bool {
	return parallelServiceContext(c) || parallelCommandHasSensitiveText(p) || parallelInputHasSensitiveText(s, c) ||
		parallelSourceValuesHaveSensitiveText(p) || p.sqlMode && p.semaphore && p.wait && parallelSensitiveText(strings.Join(p.words, " "))
}

func parallelServiceContext(c shellcmd.Call) bool {
	return slices.Contains(c.Via, "systemd-run") || slices.Contains(c.Via, "run0")
}

func parallelCommandHasSensitiveText(p parallelCommand) bool {
	end := slices.IndexFunc(p.words, p.sourceWord)
	if end < 0 {
		return false
	}
	command := strings.Join(p.words[:end], " ")
	if parallelProtectedCommandRe.MatchString(command) || parallelShellCRe.MatchString(command) {
		return true
	}
	if !p.customArgSep {
		return false
	}
	return parallelWordsHaveSensitiveText(p.words)
}

func parallelInputHasSensitiveText(s *killScan, c shellcmd.Call) bool {
	in := s.inputOf(c)
	if in.feed != "" {
		calls, err := shellcmd.Calls(in.feed)
		if err != nil {
			s.tool.Logger().Warn("kill_guard: значения источника parallel не разобраны", "error", err)
			return true
		}
		for i := range calls {
			if (calls[i].Name == "echo" || calls[i].Name == "printf") && parallelSensitiveText(echoedValue(calls[i])) {
				return true
			}
		}
	}
	for _, redir := range c.Redirs {
		if redir.N != "" && redir.N != "0" {
			continue
		}
		if redir.Op == "<<<" && parallelSensitiveText(redir.Target) ||
			(redir.Op == "<<" || redir.Op == "<<-") && parallelSensitiveText(redir.Body) {
			return true
		}
	}
	return false
}

func parallelSourceValuesHaveSensitiveText(p parallelCommand) bool {
	for i, word := range p.words {
		if !p.sourceWord(word) {
			continue
		}
		for _, value := range p.words[i+1:] {
			if p.sourceWord(value) {
				break
			}
			if parallelSensitiveText(value) {
				return true
			}
		}
	}
	return false
}

func parallelWordsHaveSensitiveText(words []string) bool {
	return slices.ContainsFunc(words, parallelSensitiveText)
}

// parallelCalls проверка разобранной команды parallel (parallelSystemdRun)
func (s *killScan) parallelCalls(c shellcmd.Call, p parallelCommand, script string, calls []shellcmd.Call) (string, []shellcmd.Call) {
	in, pipeInput := s.parallelCallsInput(c, p, calls)
	// Keep the fail-closed fallback only for an unknown listed value delivered
	// to an elevated shell script. A neighboring kill command is otherwise
	// checked on its own, as it is for commands without parallel.
	if p.listed && !parallelSourcesHarmless(p) && parallelHasElevatedScript(s.elevatedCalls(calls)) {
		hasSiblingSignal, parseErr := parallelHasSiblingSignal(s.command, p)
		if parseErr != nil {
			s.tool.Logger().Warn("kill_guard: соседние вызовы parallel не разобраны", "error", parseErr)
			return "parallel в команде с неразбираемым соседним вызовом; проверить сигнал нельзя", calls
		}
		if hasSiblingSignal {
			return "parallel с неизвестным значением за привилегированной оболочкой находится рядом с вызовом сигнала процесса", calls
		}
	}
	// systemd-run и за run0, pkexec (parallel run0 systemd-run --pipe bash)
	expanded := s.elevatedCalls(calls)
	markParallelScriptInput(expanded, p, in, pipeInput)
	if reason := parallelUnknownScriptInputReason(p, in, expanded); reason != "" {
		return reason, expanded
	}
	if !parallelHasSystemdRun(expanded) {
		return "", expanded
	}
	return s.parallelSystemdRunCalls(p, script, calls, expanded, in)
}

func markParallelScriptInput(calls []shellcmd.Call, p parallelCommand, in stdinSpan, pipeInput bool) {
	for i := range calls {
		c := &calls[i]
		readsPipe := inputTaker(*c) || shellcmd.IsShell(c.Name) && shellReadsInput(c.Args)
		wrapperOwnsInput := parallelWrapperOwnsInput(*c)
		c.ScriptInput = c.ScriptInput || pipeInput && readsPipe && !wrapperOwnsInput
		c.ScriptInput = c.ScriptInput || !p.pipe && parallelArgumentAffectsScript(p, *c) &&
			!parallelImplicitInputHarmless(p, in)
	}
}

func parallelUnknownScriptInputReason(p parallelCommand, in stdinSpan, calls []shellcmd.Call) string {
	if !p.pipe && p.listed && parallelHasElevatedScript(calls) &&
		parallelArgumentsAffectScript(p, calls) && !parallelSourcesHarmless(p) {
		return "parallel передаёт неизвестный аргумент скрипту за привилегированной командой"
	}
	if !p.pipe && (in.src == stdinPipe || in.src == stdinFile || in.src == stdinRedirect) &&
		!knownHarmlessInput(in) && parallelHasElevatedScript(calls) && parallelArgumentsAffectScript(p, calls) {
		return "оболочка -c за привилегированной командой получает строку parallel последним аргументом; неизвестный ввод может содержать сигнал"
	}
	return ""
}

func (s *killScan) parallelSystemdRunCalls(p parallelCommand, script string, calls, expanded []shellcmd.Call, in stdinSpan) (string, []shellcmd.Call) {
	if p.pipe && parallelPkexecSystemdRunPipe(calls) {
		return "parallel --pipe передаёт stdin через pkexec в systemd-run --pipe bash; ввод всё равно считается непросматриваемым для службы", calls
	}
	if r, found := p.replacedName(expanded); found {
		return "systemd-run за parallel запускает команду, имя которой придёт из ввода (строка замены " + r + "): текст команды станет известен только при запуске" + hiddenTail, calls
	}
	if in.src != stdinPipe && in.src != stdinFile {
		return "", calls
	}
	shells := parallelInputShells(expanded)
	if reason, done := parallelInputReason(p, script, in, shells); done {
		return reason, calls
	}
	feed := s.feedCalls(in, shells)
	if feed == nil {
		return "systemd-run за parallel --pipe: оболочка читает скрипт из ввода parallel (… | parallel --pipe systemd-run --pipe bash), текст не виден до запуска" + hiddenTail, calls
	}
	return "", append(calls, feed...)
}

func parallelArgumentsAffectScript(p parallelCommand, calls []shellcmd.Call) bool {
	for i := range calls {
		if parallelArgumentAffectsScript(p, calls[i]) {
			return true
		}
	}
	return false
}

func parallelSourcesHarmless(p parallelCommand) bool {
	start := slices.IndexFunc(p.words, p.sourceWord)
	if start < 0 {
		return false
	}
	hasValue := false
	for i := start; i < len(p.words); i++ {
		word := p.words[i]
		if p.fileSource(word) {
			return false
		}
		if p.sourceWord(word) {
			continue
		}
		hasValue = true
		if strings.ContainsAny(word, "$`") || signalWordRe.MatchString(unquoted(word)) {
			return false
		}
	}
	return hasValue
}

func parallelPkexecSystemdRunPipe(calls []shellcmd.Call) bool {
	for i := range calls {
		if calls[i].Name != "pkexec" {
			continue
		}
		inner, ok := rewrittenCommand(calls[i])
		if !ok {
			continue
		}
		parsed, err := shellcmd.Calls(inner)
		if err != nil || len(parsed) == 0 {
			continue
		}
		if parsed[0].Name == "run0" {
			return true
		}
		if parsed[0].Name != "systemd-run" {
			continue
		}
		if slices.Contains(parsed[0].Args, "--pipe") && slices.Contains(parsed[0].Args, "bash") {
			return true
		}
	}
	return false
}

func parallelWrapperOwnsInput(c shellcmd.Call) bool {
	if c.Name == "systemd-run" || c.Name == "run0" || slices.Contains(c.Via, "systemd-run") || slices.Contains(c.Via, "run0") {
		return true
	}
	inner, ok := rewrittenCommand(c)
	if !ok {
		return false
	}
	calls, err := shellcmd.Calls(inner)
	if err != nil || len(calls) == 0 {
		return false
	}
	return calls[0].Name == "systemd-run" || calls[0].Name == "run0"
}

func parallelHasElevatedScript(calls []shellcmd.Call) bool {
	for i := range calls {
		x := calls[i]
		if !scriptCommandInput(x) {
			continue
		}
		if x.Name == "script" || (x.Name == "sudo" || x.Name == "doas") && privilegedInteractiveShell(x.Args) {
			return true
		}
		for _, wrapper := range x.Via {
			switch wrapper {
			case "pkexec", "sudo", "doas", "su", "run0", "runuser":
				return true
			}
		}
	}
	return false
}

func parallelArgumentAffectsScript(p parallelCommand, c shellcmd.Call) bool {
	if scriptUsesImplicitArgument(c) || scriptCommandInput(c) && implicitShellArgRe.MatchString(c.Raw) {
		return true
	}
	command := p
	if end := slices.IndexFunc(p.words, p.sourceWord); end >= 0 {
		command.words = p.words[:end]
	}
	script, err := command.script()
	return err == nil && p.replaced(script) && (scriptCommandInput(c) || slices.Contains(c.Via, "-c"))
}

func parallelImplicitInputHarmless(p parallelCommand, in stdinSpan) bool {
	if p.pipe {
		return false
	}
	if p.listed {
		return parallelSourcesHarmless(p)
	}
	return (in.src == stdinPipe || in.src == stdinFile || in.src == stdinRedirect) && knownHarmlessInput(in)
}

func parallelScriptNeedsImplicitInput(p parallelCommand) bool {
	text := strings.Join(p.words, " ")
	if strings.Contains(text, "sudo -s") || strings.Contains(text, "sudo -i") ||
		strings.Contains(text, "doas -s") {
		return true
	}
	return parallelImplicitShellCommandRe.MatchString(text)
}

// parallelCallsInput отмечает поток, который parallel --pipe передаёт читающей
// оболочке. У systemd-run/run0 вход проверяет их собственная логика.
func (s *killScan) parallelCallsInput(c shellcmd.Call, p parallelCommand, calls []shellcmd.Call) (stdinSpan, bool) {
	in := s.inputOf(c)
	pipeInput := p.pipe && (in.src == stdinPipe || in.src == stdinFile || in.src == stdinRedirect)
	via := "parallel"
	canIgnoreImplicitInput := !slices.Contains(c.Via, "systemd-run") && !parallelHasSystemdRun(s.elevatedCalls(calls))
	harmlessInput := canIgnoreImplicitInput && parallelImplicitInputHarmless(p, in)
	if !p.pipe && p.listed && parallelHasExplicitSafeScript(p, calls) {
		via = "parallel-args"
	}
	for i := range calls {
		s.markParallelInputCall(&calls[i], c, p, via, canIgnoreImplicitInput, harmlessInput, pipeInput)
	}
	return in, pipeInput
}

func (s *killScan) markParallelInputCall(call *shellcmd.Call, parallel shellcmd.Call, p parallelCommand, via string, canIgnoreImplicitInput, harmlessInput, pipeInput bool) {
	call.Via = slices.Concat(parallel.Via, []string{via}, call.Via)
	if harmlessInput {
		call.Via = append(call.Via, "parallel-safe-input")
	} else if canIgnoreImplicitInput && !p.pipe && !p.listed &&
		scriptCommandInput(*call) && !parallelArgumentAffectsScript(p, *call) {
		call.Via = append(call.Via, "parallel-arg-unused")
	}
	call.Offset = -1
	// У вызовов parallel нет своего места в исходном тексте. Пометка
	// передаёт ввод --pipe в обычную проверку elevated оболочки.
	readsPipe := inputTaker(*call) || shellcmd.IsShell(call.Name) && shellReadsInput(call.Args) ||
		call.Name == "parallel" && p.pipe && s.parallelReadsInputAsScript(*call, 0)
	call.ScriptInput = call.ScriptInput || pipeInput && readsPipe && !parallelWrapperOwnsInput(*call)
	call.ScriptInput = call.ScriptInput || !p.pipe && parallelArgumentAffectsScript(p, *call) && !harmlessInput
}

func (s *killScan) parallelReadsInputAsScript(c shellcmd.Call, depth int) bool {
	if depth >= maxScriptNesting {
		return false
	}
	p := parseParallel(c.Args)
	if p.failed || !p.pipe || len(p.words) == 0 {
		return false
	}
	command := p
	if end := slices.IndexFunc(p.words, p.sourceWord); end >= 0 {
		command.words = p.words[:end]
	}
	script, err := command.script()
	if err != nil {
		s.tool.Logger().Warn("kill_guard: nested parallel command is not parseable", "error", err)
		return false
	}
	calls, err := shellcmd.Calls(script)
	if err != nil {
		s.tool.Logger().Warn("kill_guard: nested parallel shell calls are not parseable", "error", err)
		return false
	}
	expanded := s.elevatedCalls(calls)
	for i := range expanded {
		call := expanded[i]
		if shellcmd.IsShell(call.Name) && (shellReadsInput(call.Args) ||
			parallelShellExecutesReplacement(p, call)) {
			return true
		}
		if call.Name == "parallel" && s.parallelReadsInputAsScript(call, depth+1) {
			return true
		}
	}
	return false
}

func parallelShellExecutesReplacement(p parallelCommand, c shellcmd.Call) bool {
	if !p.cat && !p.fifo || !shellcmd.IsShell(c.Name) {
		return false
	}
	isReplacement := func(arg string) bool {
		return replacementRe.MatchString(arg) || slices.ContainsFunc(p.replaces, func(r string) bool {
			return strings.Contains(arg, r)
		})
	}
	script, command := shellScript(c.Args)
	return !command && isReplacement(script)
}

func parallelHasSiblingSignal(command string, p parallelCommand) (bool, error) {
	if script, err := p.script(); err == nil {
		if inner, err := shellcmd.Calls(script); err == nil {
			for i := range inner {
				if slices.Contains(signalNames, inner[i].Name) {
					return false, nil
				}
			}
		}
	}
	calls, err := shellcmd.Calls(command)
	if err != nil {
		return false, fmt.Errorf("parse calls around parallel: %w", err)
	}
	for i := range calls {
		call := &calls[i]
		if call.Name == "parallel" {
			continue
		}
		if !slices.Contains(signalNames, call.Name) {
			continue
		}
		if call.Name == "kill" && (slices.Contains(call.Args, "-l") || slices.Contains(call.Args, "-L") || slices.Contains(call.Args, "--list")) {
			continue
		}
		if call.Name == "pkill" && (slices.Contains(call.Args, "-l") || slices.Contains(call.Args, "--list-name")) {
			continue
		}
		if slices.Contains(signalNames, call.Name) {
			return true, nil
		}
	}
	return false, nil
}

func parallelHasExplicitSafeScript(p parallelCommand, calls []shellcmd.Call) bool {
	for i := range calls {
		c := calls[i]
		args, runner := scriptArgs(c)
		if runner && len(args) > 0 && !parallelArgumentAffectsScript(p, c) {
			return true
		}
	}
	return false
}

// parallelHasSystemdRun reports whether the expanded call tree contains systemd-run.
func parallelHasSystemdRun(calls []shellcmd.Call) bool {
	for i := range calls {
		if calls[i].Name == "systemd-run" {
			return true
		}
	}
	return false
}

// parallelInputShells returns input-reading shells that run under systemd-run.
func parallelInputShells(calls []shellcmd.Call) []shellcmd.Call {
	shells := make([]shellcmd.Call, 0, len(calls))
	for i := range calls {
		x := &calls[i]
		if inputShell(*x, calls) {
			shells = append(shells, *x)
		}
	}
	return shells
}

// parallelInputReason reports when input to a systemd-run shell cannot be checked.
func parallelInputReason(p parallelCommand, script string, in stdinSpan, shells []shellcmd.Call) (string, bool) {
	switch {
	case len(shells) == 0:
		return "", true
	case !p.pipe && !p.replaced(script):
		return "systemd-run за parallel (… | parallel systemd-run --pipe bash): строки ввода станут аргументами оболочки (-c и скрипт), их не видно до запуска" + hiddenTail, true
	case !p.pipe || in.src == stdinFile:
		return "", true
	}
	return "", false
}

// script строка команды для оболочки: слова через пробел, с -q — каждое в
// кавычках
func (p parallelCommand) script() (string, error) {
	if !p.quote {
		return strings.Join(p.words, " "), nil
	}
	out := make([]string, 0, len(p.words))
	for _, w := range p.words {
		q, err := syntax.Quote(w, syntax.LangBash)
		if err != nil {
			return "", fmt.Errorf("parallel -q: слово в кавычки: %w", err)
		}
		out = append(out, q)
	}
	return strings.Join(out, " "), nil
}

// elevatedCalls calls и вызовы за run0, pkexec и machinectl shell среди них
// (parallel run0 bash): run0 — как systemd-run (rewrittenCommand), а
// оболочку за ними ищет inputShell. Via — с обёрткой впереди, места нет
func (s *killScan) elevatedCalls(calls []shellcmd.Call) []shellcmd.Call {
	out := slices.Clone(calls)
	for i := 0; i < len(out) && i < 64; i++ {
		inner, ok := rewrittenCommand(out[i])
		if !ok {
			continue
		}
		parsed, err := shellcmd.Calls(inner)
		if err != nil {
			// такую команду отвергнет и s.call (elevated) при проверке
			s.tool.Logger().Debug("kill_guard: команда за обёрткой в parallel не разобрана", "wrapper", out[i].Name, "error", err)
			continue
		}
		for j := range parsed {
			x := parsed[j]
			x.Via = slices.Concat(out[i].Via, []string{out[i].Name}, x.Via)
			x.Offset = -1
			out = append(out, x)
		}
	}
	return out
}

// replaced — в команде есть строка замены: аргументы встанут на её место
func (p parallelCommand) replaced(script string) bool {
	return replacementRe.MatchString(script) || slices.ContainsFunc(p.replaces, func(r string) bool { return strings.Contains(script, r) })
}

// replacedName своя строка замены (-I XX, --er XX) в имени команды под
// systemd-run: имя придёт из ввода. Строки {…} ловит правило имени из
// подстановки (systemdRunHiddenReason)
func (p parallelCommand) replacedName(calls []shellcmd.Call) (string, bool) {
	for _, r := range p.replaces {
		for i := range calls {
			x := &calls[i]
			if strings.Contains(x.Name, r) && slices.Contains(x.Via, "systemd-run") {
				return r, true
			}
		}
	}
	return "", false
}

// inputShell оболочка под systemd-run в команде parallel (на любой глубине),
// которая читает скрипт из ввода или из слов, добавленных в конец (bash,
// bash -s, bash -c); systemd-run на другой машине исключается.
func inputShell(x shellcmd.Call, calls []shellcmd.Call) bool {
	k := slices.Index(x.Via, "systemd-run")
	if k < 0 || !shellcmd.IsShell(x.Name) || systemdRunRemote(x, calls, k) {
		return false
	}
	script, runner := scriptArgs(x)
	return runner && len(script) == 0 || !runner && shellReadsInput(x.Args)
}
