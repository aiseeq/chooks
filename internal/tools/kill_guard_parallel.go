package tools

import (
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"

	"github.com/aiseeq/akit/shellcmd"
	shExpand "mvdan.cc/sh/v3/expand"
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
	if source := slices.IndexFunc(p.words, func(w string) bool { return slices.Contains(parallelSources, w) }); source >= 0 {
		p.words = p.words[:source]
		p.listed = true
	}
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
	if p.failed || p.remote || len(p.words) == 0 {
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
	for i := range calls {
		calls[i].Via = slices.Concat(c.Via, []string{"parallel"}, calls[i].Via)
		calls[i].Offset = -1
	}
	// systemd-run и за run0, pkexec (parallel run0 systemd-run --pipe bash)
	expanded := s.elevatedCalls(calls)
	if !slices.ContainsFunc(expanded, func(x shellcmd.Call) bool { return x.Name == "systemd-run" }) {
		return "", expanded
	}
	if p.listed {
		return "", nil
	}
	if r, found := p.replacedName(expanded); found {
		return "systemd-run за parallel запускает команду, имя которой придёт из ввода (строка замены " + r + "): текст команды станет известен только при запуске" + hiddenTail, calls
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

// script склеивает argv parallel после снятия кавычек внешней оболочкой.
func (p parallelCommand) script() (string, error) {
	words, err := parallelArgv(p.words)
	if err != nil {
		return "", err
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

// parallelArgv снимает синтаксис внешних кавычек через mvdan shell AST;
// неизвестные раскрытия сохраняются текстом и не выполняются.
func parallelArgv(words []string) ([]string, error) {
	source := "__chooks_parallel_argv_probe__ " + strings.Join(words, " ")
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(source), "")
	if err != nil {
		return nil, fmt.Errorf("parse parallel argv: %w", err)
	}
	if len(file.Stmts) != 1 {
		return nil, fmt.Errorf("parallel argv probe: unexpected statements")
	}
	call, ok := file.Stmts[0].Cmd.(*syntax.CallExpr)
	if !ok || len(call.Args) == 0 {
		return nil, fmt.Errorf("parallel argv probe: missing command")
	}
	return shellFields(call.Args[1:])
}

func shellFields(words []*syntax.Word) ([]string, error) {
	config := &shExpand.Config{
		Env: shExpand.FuncEnviron(func(name string) string { return "$" + name }),
		CmdSubst: func(w io.Writer, cmd *syntax.CmdSubst) error {
			return syntax.NewPrinter().Print(w, cmd)
		},
		ProcSubst: func(proc *syntax.ProcSubst) (string, error) {
			var text strings.Builder
			err := syntax.NewPrinter().Print(&text, proc)
			return text.String(), err
		},
	}
	return shExpand.Fields(config, words...)
}

func parallelVariableSignal(call shellcmd.Call) bool {
	return slices.Contains(signalNames, call.Name) && strings.Contains(call.Raw, "$")
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
	return replacementRe.MatchString(script) || slices.ContainsFunc(p.replaces, func(r string) bool { return strings.Contains(script, r) })
}

// replacedName своя строка замены (-I XX, --er XX) в имени команды под
// systemd-run: имя придёт из ввода. Строки {…} ловит правило имени из
// подстановки (systemdRunHiddenReason)
func (p parallelCommand) replacedName(calls []shellcmd.Call) (string, bool) {
	for _, r := range p.replaces {
		if slices.ContainsFunc(calls, func(x shellcmd.Call) bool {
			return strings.Contains(x.Name, r) && slices.Contains(x.Via, "systemd-run")
		}) {
			return r, true
		}
	}
	return "", false
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
