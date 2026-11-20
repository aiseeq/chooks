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

// parallelCommand команда parallel: words — слова после опций; pipe — ввод
// идёт в stdin команды; quote — слова в кавычках; listed — заданы списки
// аргументов; argInput — список читает stdin, argInputFirst — stdin-файл
// стоит до списков в words; semaphore/wait — режим sem и --wait; remote —
// все машины удалённые; failed — опции не разобраны,
// parallel выйдет, ничего не запустив. argFileSep и colsep — разделители,
// заданные опциями.
type parallelCommand struct {
	words                       []string
	pipe, quote, listed, remote bool
	argInput, argInputFirst     bool
	argInputFromWords           bool
	semaphore, wait             bool
	argFileSep, colsep          string
	failed                      bool
	replaces                    []string
}

// parallelPipeOptions опции, с которыми ввод идёт командам в stdin
// (--round-robin, --fifo, --cat и --group-by включают --pipe)
var parallelPipeOptions = []string{"pipe", "pipe-part", "round-robin", "fifo", "cat", "group-by"}

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
		switch {
		case slices.Contains(parallelPipeOptions, o.name):
			p.pipe = true
		case o.name == "quote":
			p.quote = true
		case o.name == "rpl":
			// --rpl 'XX perl': строка замены — первое слово
			if f := strings.Fields(o.value); len(f) > 0 {
				p.replaces = append(p.replaces, f[0])
			}
		case slices.Contains(parallelReplaceOptions, o.name) && o.value != "":
			p.replaces = append(p.replaces, o.value)
		case o.name == "sshlogin":
			logins = append(logins, o.value)
		case o.name == "sshloginfile":
			files = true
		case o.name == "arg-file" && parallelStdinFile(o.value):
			p.argInputFirst = true
		case o.name == "arg-file-sep":
			p.argFileSep = o.value
		case o.name == "col-sep":
			p.colsep = o.value
		case o.name == "wait":
			p.wait = true
		}
	}
	local, hosts := parallelLocalLogins(logins)
	p.remote = !local && (hosts || files)
	p.listed = slices.ContainsFunc(sets, func(o parallelSet) bool { return o.name == "arg-file" }) ||
		slices.ContainsFunc(p.words, p.sourceWord)
	p.argInputFromWords = parallelArgInput(p, p.words)
	p.argInput = p.argInputFirst || p.argInputFromWords
	p.semaphore = slices.ContainsFunc(sets, func(o parallelSet) bool { return o.name == "semaphore" })
	return p
}

// sourceWord разделитель списка parallel, включая --arg-file-sep
func (p parallelCommand) sourceWord(w string) bool {
	return w == ":::" || w == ":::+" || p.fileSource(w)
}

// fileSource разделитель файлов аргументов parallel
func (p parallelCommand) fileSource(w string) bool {
	if p.argFileSep != "" {
		return w == p.argFileSep || w == p.argFileSep+"+"
	}
	return w == "::::" || w == "::::+"
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

// parallelArgInput показывает, читает ли parallel список аргументов из stdin.
// После разделителя файла могут идти несколько имён файлов до следующего
// разделителя списка; stdin может быть указан любым из поддержанных путей.
func parallelArgInput(p parallelCommand, words []string) bool {
	for i, w := range words {
		if !p.fileSource(w) {
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

// parallelSystemdRunReason причина отказа на systemd-run в команде parallel
// (parallelSystemdRun)
func (s *killScan) parallelSystemdRunReason(c shellcmd.Call) string {
	reason, _ := s.parallelSystemdRun(c)
	return reason
}

// parallelInnerCalls вызовы команды parallel для проверки, как вызовы
// скрипта (parallelSystemdRun)
func (s *killScan) parallelInnerCalls(c shellcmd.Call) []shellcmd.Call {
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
	if p.failed || p.remote || len(p.words) == 0 {
		return "", nil
	}
	script, err := p.script()
	if err == nil {
		var calls []shellcmd.Call
		calls, err = shellcmd.Calls(script)
		if err == nil {
			if p.listed && !p.argInput {
				expanded := s.elevatedCalls(calls)
				if !parallelHasSystemdRun(expanded) && !parallelHasElevatedScript(expanded) {
					return "", nil
				}
			}
			return s.parallelCalls(c, p, script, calls)
		}
	}
	s.tool.Logger().Warn("kill_guard: команда parallel не разобрана", "error", err)
	if strings.Contains(strings.Join(p.words, " "), "systemd-run") {
		return "systemd-run за parallel: команду parallel разобрать не удалось, что запустится, не видно" + hiddenTail, nil
	}
	return "", nil
}

// parallelCalls проверка разобранной команды parallel (parallelSystemdRun)
func (s *killScan) parallelCalls(c shellcmd.Call, p parallelCommand, script string, calls []shellcmd.Call) (string, []shellcmd.Call) {
	in, pipeInput := s.parallelCallsInput(c, p, calls)
	// systemd-run и за run0, pkexec (parallel run0 systemd-run --pipe bash)
	expanded := s.elevatedCalls(calls)
	if !parallelHasSystemdRun(expanded) {
		// Без --pipe строки stdin и ::: становятся аргументами команды. Если
		// оболочка с -c находится за обёрткой, последняя строка становится
		// текстом скрипта; fedIntoScript применит ту же проверку, что и для {}.
		if pipeInput || parallelHasElevatedScript(expanded) && (p.listed || in.src == stdinPipe || in.src == stdinFile) {
			return "", calls
		}
		return "", nil
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

// parallelCallsInput отмечает поток, который parallel --pipe передаёт читающей
// оболочке. У systemd-run/run0 вход проверяет их собственная логика.
func (s *killScan) parallelCallsInput(c shellcmd.Call, p parallelCommand, calls []shellcmd.Call) (stdinSpan, bool) {
	in := s.inputOf(c)
	pipeInput := p.pipe && (in.src == stdinPipe || in.src == stdinFile || in.src == stdinRedirect)
	for i := range calls {
		calls[i].Via = slices.Concat(c.Via, []string{"parallel"}, calls[i].Via)
		calls[i].Offset = -1
	}
	expanded := s.elevatedCalls(calls)
	for i := range calls {
		// У вызовов parallel нет своего места в исходном тексте. Пометка
		// передаёт ввод --pipe в обычную проверку elevated оболочки.
		calls[i].ScriptInput = calls[i].ScriptInput || pipeInput && parallelReadsPipe(calls[i]) &&
			!parallelInputOwnedByService(calls[i], expanded)
	}
	return in, pipeInput
}

func parallelReadsPipe(c shellcmd.Call) bool {
	return inputTaker(c) || shellcmd.IsShell(c.Name) && shellReadsInput(c.Args) ||
		c.Name == "parallel" && parseParallel(c.Args).pipe
}

func parallelInputOwnedByService(c shellcmd.Call, expanded []shellcmd.Call) bool {
	if c.Name == "systemd-run" || c.Name == "run0" ||
		slices.Contains(c.Via, "systemd-run") || slices.Contains(c.Via, "run0") {
		return true
	}
	for i := range expanded {
		x := &expanded[i]
		if x.Raw == c.Raw && (x.Name == "systemd-run" || x.Name == "run0" ||
			slices.Contains(x.Via, "systemd-run") || slices.Contains(x.Via, "run0")) {
			return true
		}
	}
	return false
}

// parallelHasElevatedScript — в команде parallel есть оболочка с -c за
// обёрткой, которая запускает её с правами или как команду script -c.
func parallelHasElevatedScript(calls []shellcmd.Call) bool {
	for i := range calls {
		x := &calls[i]
		if x.Name == "script" && scriptCommandMissing(x.Args) {
			return true
		}
		if !shellcmd.IsShell(x.Name) || !slices.ContainsFunc(x.Via, func(v string) bool {
			return v == "pkexec" || v == "sudo" || v == "run0" || v == "script"
		}) {
			continue
		}
		if shellCommandMissing(x.Args) {
			return true
		}
		if _, runner := scriptArgs(*x); runner {
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
	words := p.words
	if p.listed && !p.argInput {
		if i := slices.IndexFunc(words, p.sourceWord); i >= 0 {
			words = words[:i]
		}
	}
	if !p.quote {
		return strings.Join(words, " "), nil
	}
	out := make([]string, 0, len(words))
	for _, w := range words {
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
	_, runner := scriptArgs(x)
	return !runner && shellReadsInput(x.Args)
}
