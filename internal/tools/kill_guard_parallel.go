package tools

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/aiseeq/akit/shellcmd"
	"mvdan.cc/sh/v3/syntax"
)

// parallelValues опции GNU parallel со значением отдельным словом (или
// слитно: -j4, --jobs=4)
var parallelValues = []string{"a", "arg-file", "arg-file-sep", "arg-sep", "basefile", "bf", "block", "block-size",
	"colsep", "C", "d", "delimiter", "E", "env", "I", "joblog", "j", "P", "jobs", "max-procs", "J", "profile",
	"L", "max-lines", "n", "max-args", "N", "load", "memfree", "nice", "results", "res", "retries", "return",
	"S", "H", "sshlogin", "slf", "sshloginfile", "s", "max-chars", "tag-string", "tagstring", "termseq",
	"timeout", "tmpdir", "transferfile", "tf", "workdir", "wd", "delay", "halt", "halt-on-error", "header",
	"limit", "rpl", "ssh", "sshdelay", "template", "seqreplace", "compress-program", "decompress-program",
	"group-by", "W"}

// parallelSources разделители списков аргументов: аргументы из них, не из
// ввода
var parallelSources = []string{":::", ":::+", "::::", "::::+"}

// replacementRe строка замены parallel ({}, {.}, {/}, {1}, {#}, {%}, {=…=}):
// аргумент встаёт на её место, а не в конец команды
var replacementRe = regexp.MustCompile(`\{[0-9]*[./#%]*[./]*\}|\{=`)

// parallelCommand команда parallel: слова после опций, режим --pipe (ввод
// идёт в stdin команды, аргументов нет), -q (слова в кавычках), аргументы
// из списка (:::, -a), строка замены -I и машины -S: remote — все на другой
// машине (--sshlogin : — эта)
type parallelCommand struct {
	words               []string
	pipe, quote, listed bool
	remote              bool
	replace             string
}

// parseParallel разбирает слова parallel
func parseParallel(args []string) parallelCommand {
	var p parallelCommand
	logins, local := false, false
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
		name, value, inline := parallelOption(a)
		if !inline && slices.Contains(parallelValues, name) && i+1 < len(args) {
			i++
			value = args[i]
		}
		switch name {
		case "pipe", "pipepart", "pipe-part":
			p.pipe = true
		case "q", "quote":
			p.quote = true
		case "a", "arg-file":
			p.listed = true
		case "I":
			p.replace = value
		case "S", "H", "sshlogin":
			// машины копятся по всем -S; «:» — эта машина
			logins = true
			local = local || slices.Contains(strings.Split(value, ","), ":")
		case "slf", "sshloginfile":
			logins = true
		}
	}
	p.remote = logins && !local
	p.words = args[i:]
	p.listed = p.listed || slices.ContainsFunc(p.words, func(w string) bool { return slices.Contains(parallelSources, w) })
	return p
}

// parallelOption имя опции и значение, если оно слитно (--jobs=4, -j4)
func parallelOption(a string) (name, value string, inline bool) {
	if strings.HasPrefix(a, "--") {
		name, value, inline = strings.Cut(a[2:], "=")
		return name, value, inline
	}
	name = a[1:2]
	if len(a) > 2 && slices.Contains(parallelValues, name) {
		return name, a[2:], true
	}
	return name, "", false
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
	if p.listed || p.remote || len(p.words) == 0 {
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
	if !slices.ContainsFunc(calls, func(x shellcmd.Call) bool { return x.Name == "systemd-run" }) {
		return "", nil
	}
	for i := range calls {
		calls[i].Via = slices.Concat(c.Via, []string{"parallel"}, calls[i].Via)
		calls[i].Offset = -1
	}
	expanded := s.elevatedCalls(calls)
	if p.replace != "" && slices.ContainsFunc(expanded, func(x shellcmd.Call) bool {
		return strings.Contains(x.Name, p.replace) && slices.Contains(x.Via, "systemd-run")
	}) {
		return "systemd-run за parallel запускает команду, имя которой придёт из ввода (-I " + p.replace + "): текст команды станет известен только при запуске" + hiddenTail, calls
	}
	in := s.inputOf(c)
	if in.src != stdinPipe && in.src != stdinFile {
		return "", calls
	}
	shells := slices.DeleteFunc(slices.Clone(expanded), func(x shellcmd.Call) bool { return !inputShell(x, expanded) })
	switch {
	case len(shells) == 0:
		return "", calls
	case !p.pipe && !p.replaced(script):
		return "systemd-run за parallel (… | parallel systemd-run --pipe bash): строки ввода станут аргументами оболочки (-c и скрипт), их не видно до запуска" + hiddenTail, calls
	case !p.pipe || in.src == stdinFile:
		return "", calls
	}
	feed := s.feedCalls(in, shells)
	if feed == nil {
		return "systemd-run за parallel --pipe: оболочка читает скрипт из ввода parallel (… | parallel --pipe systemd-run --pipe bash), текст не виден до запуска" + hiddenTail, calls
	}
	return "", append(calls, feed...)
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
// (systemd-run --pipe run0 bash): shellcmd их не снимает, а оболочку за ними
// ищет inputShell. Via — с обёрткой впереди, места нет
func (s *killScan) elevatedCalls(calls []shellcmd.Call) []shellcmd.Call {
	out := slices.Clone(calls)
	for i := 0; i < len(out) && i < 64; i++ {
		inner, ok := elevatedCommand(out[i])
		if !ok {
			continue
		}
		parsed, err := shellcmd.Calls(inner)
		if err != nil {
			// такую команду отвергнет и s.call (elevated) при проверке
			s.tool.Logger().Debug("kill_guard: команда за обёрткой в parallel не разобрана", "wrapper", out[i].Name, "error", err)
			continue
		}
		for _, x := range parsed {
			x.Via = slices.Concat(out[i].Via, []string{out[i].Name}, x.Via)
			x.Offset = -1
			out = append(out, x)
		}
	}
	return out
}

// replaced — в команде есть строка замены: аргументы встанут на её место
func (p parallelCommand) replaced(script string) bool {
	return replacementRe.MatchString(script) || p.replace != "" && strings.Contains(script, p.replace)
}

// inputShell оболочка под systemd-run на этой машине (на любой глубине),
// которая читает скрипт из ввода или из слов, добавленных в конец (bash,
// bash -s, bash -c)
func inputShell(x shellcmd.Call, calls []shellcmd.Call) bool {
	k := slices.Index(x.Via, "systemd-run")
	if k < 0 || !shellcmd.IsShell(x.Name) || systemdRunRemote(x, calls, k) {
		return false
	}
	_, runner := scriptArgs(x)
	return !runner && shellReadsInput(x.Args)
}
