package tools

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/aiseeq/akit/shellcmd"
	"mvdan.cc/sh/v3/syntax"
)

// parallelSources разделители списков аргументов: аргументы из них, не из
// ввода
var parallelSources = []string{":::", ":::+", "::::", "::::+"}

// replacementRe строка замены parallel ({}, {.}, {/}, {1}, {#}, {%}, {=…=}):
// аргумент встаёт на её место, а не в конец команды
var replacementRe = regexp.MustCompile(`\{[0-9]*[./#%]*[./]*\}|\{=`)

// parallelCommand команда parallel: слова после опций, режим --pipe (ввод
// идёт в stdin команды, аргументов нет), -q (слова в кавычках), аргументы
// из списка (:::, -a), строки замены (-I, --er, --rpl…) и машины -S: remote —
// все на другой машине (--sshlogin : — эта); failed — опции не разобраны,
// parallel выйдет, ничего не запустив
type parallelCommand struct {
	words                       []string
	pipe, quote, listed, remote bool
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
		case o.name == "arg-file":
			p.listed = true
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
		}
	}
	local, hosts := parallelLocalLogins(logins)
	p.remote = !local && (hosts || files)
	p.listed = p.listed || slices.ContainsFunc(p.words, func(w string) bool { return slices.Contains(parallelSources, w) })
	return p
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
	if p.failed || p.listed || p.remote || len(p.words) == 0 {
		return "", nil
	}
	script, err := p.script()
	if err == nil {
		var calls []shellcmd.Call
		calls, err = shellcmd.Calls(script)
		if err == nil {
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
		if pipeInput {
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
		// У вызовов parallel нет своего места в исходном тексте. Пометка
		// передаёт ввод --pipe в обычную проверку elevated оболочки.
		readsPipe := inputTaker(calls[i]) || shellcmd.IsShell(calls[i].Name) && shellReadsInput(calls[i].Args)
		wrapperOwnsInput := calls[i].Name == "systemd-run" || calls[i].Name == "run0" ||
			slices.Contains(calls[i].Via, "systemd-run") || slices.Contains(calls[i].Via, "run0")
		calls[i].ScriptInput = calls[i].ScriptInput || pipeInput && readsPipe && !wrapperOwnsInput
	}
	return in, pipeInput
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
	_, runner := scriptArgs(x)
	return !runner && shellReadsInput(x.Args)
}
