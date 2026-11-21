package tools

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/aiseeq/akit/shellcmd"
)

// Правило глубины. Граница kill guard — ошибки агента, как у ядерного
// agent-signal-guard, а не умышленный обход. Хук понимает команду и одну
// обёртку над ней (sudo, env, timeout, setsid, sh -c, systemd-run, run0,
// parallel, xargs). Сигнал или остановка под двумя и более обёртками, а
// также имя команды, которое там приходит не литералом, — отказ с
// подсказкой запустить напрямую (depthReason, feedCall)

// depthHint подсказка к отказу правила глубины
const depthHint = "Запусти напрямую: сигнал или остановку — отдельной командой без обёрток (kill 12345, systemctl --user stop имя.service), цель найди отдельной командой заранее (pgrep -a '[x]yz', systemctl --user list-units)"

// run0WordRe отдельное слово run0: под ним гасит контейнер и podman stop, а
// слова kill в команде может не быть
var run0WordRe = regexp.MustCompile(`(^|[^[:alnum:]_.-])run0([^[:alnum:]_.-]|$)`)

// notLevels слова Via, которые не уровень обёртки: pipe — пометка разбора,
// command, builtin, exec, time и eval снимает сама оболочка, -exec и -ok —
// часть уровня find
var notLevels = []string{"-c", "pipe", "command", "builtin", "exec", "time", "eval", "-exec", "-execdir", "-ok", "-okdir"}

// wrapperLevels число уровней обёрток в Via вызова. Оболочка в Via —
// всегда с текстом, который разбор прочёл литералом (bash -c, bash -euo
// pipefail -c, sh -ec, bash <<< '…', heredoc): уровнем она не считается,
// команда в ней судится как прямая
func wrapperLevels(via []string) int {
	n := 0
	for _, v := range via {
		if !slices.Contains(notLevels, v) && !inlineShells[path.Base(v)] && path.Base(v) != "fish" {
			n++
		}
	}
	return n
}

// stopVerbs глаголы systemctl и loginctl, которые останавливают юнит,
// гасят сеанс или машину
var stopVerbs = slices.Concat(systemctlStopVerbs, systemctlEndVerbs, loginctlEndVerbs)

// depthSignal — вызов способен послать сигнал или остановить юнит: kill,
// pkill, killall, killall5, podman stop/kill/rm, systemctl и loginctl с
// глаголом остановки, fuser -k, tmux kill-*. name — как назвать его в причине
func depthSignal(c shellcmd.Call) (name string, ok bool) {
	if name, ok := signalCommand(c); ok {
		return name, true
	}
	switch c.Name {
	case "systemctl", "loginctl":
		values := systemctlValues
		if c.Name == "loginctl" {
			values = loginctlValues
		}
		ops := optionOperands(plainWords(c.Args), values)
		if len(ops) > 0 && slices.Contains(stopVerbs, ops[0].text) {
			return c.Name + " " + ops[0].text, true
		}
	case "fuser":
		if slices.ContainsFunc(c.Args, func(a string) bool {
			return strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "k")
		}) {
			return "fuser -k", true
		}
	case "tmux":
		if slices.ContainsFunc(c.Args, func(a string) bool { return strings.HasPrefix(a, "kill-") }) {
			return "tmux kill-*", true
		}
	}
	return c.Name, false
}

// depthReason отказ правила глубины на вызов c: под двумя и более
// обёртками стоит сигнал или остановка либо имя, вычисляемое при запуске,
// а в тексте вызова видна команда сигнала ($(echo kill) 1). Вызов в
// скрипте ssh и за systemd-run на другую машину исполнится не здесь
func (s *killScan) depthReason(c shellcmd.Call, calls []shellcmd.Call) string {
	if wrapperLevels(c.Via) < 2 || slices.Contains(c.Via, "ssh") || systemdRunElsewhere(c, calls) {
		return ""
	}
	chain := strings.Join(slices.DeleteFunc(slices.Clone(c.Via), func(v string) bool { return slices.Contains(notLevels, v) }), " → ")
	if name, ok := depthSignal(c); ok && !harmlessKill(c) {
		return name + " под обёртками " + chain + ": глубже одной обёртки хук команду не разбирает"
	}
	if _, ok := signalWords(c.Raw); ok && (c.Name == "" || strings.ContainsAny(c.Name, "$`")) {
		return "имя команды под обёртками " + chain + " вычисляется при запуске, а в её тексте видна команда сигнала: глубже одной обёртки хук команду не разбирает"
	}
	return ""
}

// harmlessKill — kill без сигнала: список сигналов (kill -l) или проверка,
// жив ли процесс (kill -0 без отрицательных целей и без целей из ввода)
func harmlessKill(c shellcmd.Call) bool {
	if c.Name != "kill" || len(c.Args) == 0 {
		return false
	}
	targets, zero, computed := killOptions(plainWords(c.Args))
	return computed == "" && (targets == nil && slices.Contains([]string{"-l", "-L", "--list", "--table"}, c.Args[0]) ||
		zero && len(targets) > 0 && !afterXargs(c) && !slices.ContainsFunc(targets, func(w word) bool { return strings.HasPrefix(w.text, "-") }))
}

// feedMarkers слова источников parallel
var feedMarkers = []string{":::", "::::", ":::+", "::::+"}

// feedSplitOptions опции parallel и xargs, которые делят ввод или списки не
// по строкам и словам или отдают его файлом (--cat, --fifo): значения из
// них хук не вычисляет
var feedSplitOptions = []string{"--pipe", "--pipepart", "--cat", "--fifo", "-C", "--colsep", "--arg-sep", "--arg-file-sep", "-d", "--delimiter", "-0", "--null"}

// feedWords слова parallel, sem или xargs: head — опции и команда, values —
// значения списков :::, stdin — задания или аргументы идут из ввода,
// literal — источник один и литерал: нет файлов, --pipe, деления ввода,
// подстановок в списках, сочетания нескольких источников
func feedWords(name string, args []string) (head, values []string, stdin, literal bool) {
	j := len(args)
	if k := slices.IndexFunc(args, func(a string) bool { return slices.Contains(feedMarkers, a) }); k >= 0 && name == "parallel" {
		j = k
	}
	head, literal = args[:j], true
	sources := 0
	file := func(f string) {
		in := f == "-" || stdinPathRe.MatchString(path.Clean(f))
		sources, stdin, literal = sources+1, stdin || in, literal && in
	}
	for i, a := range head {
		switch {
		case (a == "-a" || a == "--arg-file") && i+1 < len(head):
			file(head[i+1])
		case slices.Contains(feedSplitOptions, a):
			literal = false
		}
	}
	from := ""
	for _, a := range args[j:] {
		switch {
		case slices.Contains(feedMarkers, a):
			from = a
			if !strings.HasPrefix(a, "::::") {
				sources++
			}
		case strings.HasPrefix(from, "::::"):
			file(a)
		default:
			values, literal = append(values, a), literal && !computedArg(a)
		}
	}
	return head, values, stdin || name != "sem" && sources == 0, literal && sources <= 1
}

// feedInput строки ввода c, если он виден литералом (literal): <<< и heredoc
// без подстановок, пайп из echo или printf с литералом; ввод агента пуст.
// shown — видимый текст ввода и в нелитеральном случае: тело <<< и heredoc,
// команда echo или printf в пайпе
func (s *killScan) feedInput(c shellcmd.Call) (lines []string, shown string, literal bool) {
	redirected := false
	for _, r := range c.Redirs {
		if r.N != "" && r.N != "0" || !strings.HasPrefix(r.Op, "<") {
			continue
		}
		redirected, shown, literal = true, "", false
		switch r.Op {
		case "<<<":
			shown = r.Target
		case "<<", "<<-":
			shown = r.Body
		default:
			continue
		}
		literal = !strings.ContainsAny(shown, "$`\\")
	}
	if !redirected {
		in := s.inputOf(c)
		if in.src == stdinOwn {
			return nil, "", true
		}
		if in.src != stdinPipe {
			return nil, "", false
		}
		calls, err := shellcmd.Calls(strings.TrimSuffix(in.feed, " | sh"))
		if err != nil {
			s.tool.Logger().Debug("kill_guard: пайп в parallel или xargs не разобран, ввод не литерал", "feed", in.feed, "error", err)
			return nil, in.feed, false
		}
		if len(calls) != 1 || calls[0].Name != "echo" && calls[0].Name != "printf" {
			return nil, "", false
		}
		shown = echoedValue(calls[0])
		if literal = shown != ""; !literal {
			shown = in.feed
		}
	}
	if !literal {
		return nil, shown, false
	}
	return strings.FieldsFunc(shown, func(r rune) bool { return r == '\n' }), shown, true
}

// feeders команды, которые собирают команду из своих слов и источников
var feeders = []string{"parallel", "sem", "xargs"}

// feedSignals вызовы сигнала или остановки (depthSignal, кроме harmlessKill)
// и вложенные feeders в команде head с каждым значением values: строка задания — команда и
// значение одним текстом, без команды — само значение. Начало команды —
// start (место, найденное разбором akit, feedStart); без него — первое
// слово head не опция, а если слово сразу за опцией (может быть её
// значением) — пробуется и следующее. err — текст заданий не разобран
func feedSignals(head, values []string, start int) ([]shellcmd.Call, error) {
	for i := max(start, 0); i <= len(head); i++ {
		if i < len(head) && strings.HasPrefix(head[i], "-") {
			continue
		}
		cmd := strings.Join(head[i:], " ")
		lines := []string{}
		for _, v := range values {
			lines = append(lines, strings.TrimSpace(cmd+" "+v))
		}
		if len(values) == 0 {
			lines = []string{cmd}
		}
		calls, err := shellcmd.Calls(strings.Join(lines, "\n"))
		if err != nil {
			return nil, fmt.Errorf("строки заданий %q: %w", lines, err)
		}
		var sig []shellcmd.Call
		for j := range calls {
			if _, ok := depthSignal(calls[j]); ok && !harmlessKill(calls[j]) || slices.Contains(feeders, calls[j].Name) {
				sig = append(sig, calls[j])
			}
		}
		if len(sig) > 0 || start >= 0 || i == len(head) || i == 0 || !strings.HasPrefix(head[i-1], "-") {
			return sig, nil
		}
	}
	return nil, nil
}

// signalWords — в словах текста (разбит по всему, кроме букв, цифр и
// _.-/@) есть вызов, способный послать сигнал или остановить юнит
// (depthSignal): kill, systemctl stop и т. п.
func signalWords(text string) (string, bool) {
	words := strings.FieldsFunc(unquoted(text), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_.-/@", r))
	})
	for i, w := range words {
		if name, ok := depthSignal(shellcmd.Call{Name: path.Base(w), Args: words[i+1:]}); ok {
			return name, true
		}
	}
	return "", false
}

// feedCall проверяет parallel, sem и xargs: команду и аргументы они
// собирают из своих слов и источников. Команда с литеральными аргументами
// и значениями ::: (и литеральным вводом) судится одним текстом как
// команда под одной обёрткой. Источник не литерал (::::, -a, ввод, --pipe),
// а в тексте видна команда сигнала или остановки — отказ: такую форму хук
// не вычисляет. Без видимого сигнала — пропуск: граница защиты — ошибки
// агента, содержимое файла хук не видит
func (s *killScan) feedCall(c shellcmd.Call, calls []shellcmd.Call) error {
	if !slices.Contains(feeders, c.Name) || slices.Contains(c.Via, "ssh") || systemdRunElsewhere(c, calls) {
		return nil
	}
	head, values, stdin, literal := feedWords(c.Name, c.Args)
	if semWaits(c.Name, head) {
		return nil
	}
	lines, shown, ok := s.feedInput(c)
	if stdin {
		values, literal = append(values, lines...), literal && ok
	}
	text := feedText(head, values, shown)
	sig, err := feedSignals(head, values, feedStart(&c, calls))
	if err != nil {
		// текст заданий не разобран: судится как нелитеральный источник
		s.tool.Logger().Debug("kill_guard: строки заданий не разобраны", "call", c.Name, "error", err)
		s.feedDeny(c, nil, text)
		return nil
	}
	if !literal {
		s.feedDeny(c, sig, text)
		return nil
	}
	if len(sig) == 0 && slices.ContainsFunc(head, func(a string) bool {
		return strings.Contains(a, "{") || a == "-I" || a == "-i" || strings.HasPrefix(a, "--replace")
	}) {
		// значения встают в команду подстановкой (-I, {}), а не словами в
		// конце: сигнал в них хук к команде не приложит
		s.feedDeny(c, nil, strings.Join(values, "\n"))
	}
	for i := range sig {
		sig[i].Via = slices.Concat(c.Via, []string{c.Name}, sig[i].Via)
		sig[i].Offset = -1
		if err := s.call(sig[i], slices.Concat(calls, sig)); err != nil {
			return err
		}
	}
	return nil
}

// semWaits — sem --wait без --sql*: ждёт заданий и команду не запускает
func semWaits(name string, head []string) bool {
	return name == "sem" && slices.ContainsFunc(head, func(a string) bool { return a == "--wait" || a == "-W" }) &&
		!slices.ContainsFunc(head, func(a string) bool { return strings.HasPrefix(a, "--sql") })
}

// feedText текст, где ищутся слова сигнала нелитеральной формы: значения
// и ввод; слова команды — только у --arg-sep, где они тоже значения: grep
// pkill в команде xargs — не сигнал
func feedText(head, values []string, shown string) string {
	words := slices.Concat(values, []string{shown})
	if slices.ContainsFunc(head, func(a string) bool { return a == "--arg-sep" || a == "--arg-file-sep" }) {
		words = append(words, head...)
	}
	return strings.Join(words, "\n")
}

// feedStart место команды среди слов c по разбору akit: вызов под c с тем
// же текстом; -1 — разбор его не отдал (sem, команда из ::: )
func feedStart(c *shellcmd.Call, calls []shellcmd.Call) int {
	via := append(slices.Clone(c.Via), c.Name)
	for i := range calls {
		if o := &calls[i]; o.Raw == c.Raw && slices.Equal(o.Via, via) {
			return slices.Index(c.Args, o.Name)
		}
	}
	return -1
}

// signalIn имя команды сигнала среди sig, у вложенных feeders — в их
// заданиях (в словах значений, если задания не разобраны)
func signalIn(sig []shellcmd.Call) (string, bool) {
	for i := range sig {
		if n, ok := depthSignal(sig[i]); ok {
			return n, true
		}
		if !slices.Contains(feeders, sig[i].Name) {
			continue
		}
		head, values, _, _ := feedWords(sig[i].Name, sig[i].Args)
		sub, err := feedSignals(head, values, -1)
		n, ok := signalIn(sub)
		if err != nil {
			n, ok = signalWords(feedText(head, values, ""))
		}
		if ok {
			return n, true
		}
	}
	return "", false
}

// feedDeny отказ на c с нелитеральным источником, если видна команда
// сигнала: среди вызовов sig (signalIn) или в словах текста shown
func (s *killScan) feedDeny(c shellcmd.Call, sig []shellcmd.Call, shown string) {
	name, ok := signalWords(shown)
	if n, yes := signalIn(sig); yes {
		name, ok = n, true
	}
	if ok {
		via := strings.Join(append(slices.Clone(c.Via), c.Name), " → ")
		s.addToolReason(via+": "+name+" с целями или командой из источников "+c.Name+" (файл, ввод, --pipe): такую форму хук не вычисляет", depthHint, false)
	}
}

// wrappedFeeds вызовы parallel, sem и run0, которые обёртки среди calls
// исполнят своей командой, а разбор отдельным вызовом не отдал
// (systemd-run --pipe parallel :::: -, setsid env run0 -p …): с Via до
// них, текстом, местом и перенаправлениями обёртки. Слово ищет тот же
// разбор, что systemd-run за обёрткой (nestedCommand). За systemd-run на
// другую машину — не здесь. err — слова обёртки не разобраны
func wrappedFeeds(calls []shellcmd.Call) ([]shellcmd.Call, error) {
	var out []shellcmd.Call
	for i := range calls {
		c := &calls[i]
		words := append([]string{c.Name}, c.Args...)
		for _, name := range []string{"parallel", "sem", "run0"} {
			if c.Name == name || !slices.ContainsFunc(c.Args, func(a string) bool { return path.Base(a) == name }) {
				continue
			}
			j, via, err := nestedCommand(words, name)
			if err != nil {
				return out, err
			}
			if j <= 0 || remoteBefore(words[:j]) {
				continue
			}
			x := shellcmd.Call{Name: name, Args: words[j+1:], Via: slices.Concat(c.Via, via),
				Raw: c.Raw, Offset: c.Offset, Redirs: c.Redirs, ScriptInput: c.ScriptInput}
			if !hasCall(calls, &x) {
				out = append(out, x)
			}
		}
	}
	return out, nil
}

// withWrappedFeeds calls и вызовы wrappedFeeds. Слова обёртки не разобраны
// — при kill в тексте отказ: parallel или run0 за ней не проверить
func (s *killScan) withWrappedFeeds(calls []shellcmd.Call) []shellcmd.Call {
	more, err := wrappedFeeds(calls)
	if err != nil {
		s.tool.Logger().Debug("kill_guard: parallel или run0 за обёрткой не найти", "error", err)
		if s.killText {
			s.addToolReason("parallel или run0 за обёрткой: "+err.Error()+": проверить их команду нельзя", depthHint, false)
		}
	}
	return append(calls, more...)
}

// hasCall — среди calls есть вызов с именем, текстом и словами x
func hasCall(calls []shellcmd.Call, x *shellcmd.Call) bool {
	for i := range calls {
		if o := &calls[i]; o.Name == x.Name && o.Raw == x.Raw && slices.Equal(o.Args, x.Args) {
			return true
		}
	}
	return false
}

// remoteBefore — среди слов перед командой есть systemd-run на другую
// машину (-H, -M чужой машины): команда исполнится не здесь
func remoteBefore(words []string) bool {
	for i, w := range words {
		if path.Base(w) == "systemd-run" && systemdRunHostOption(words[i+1:]) {
			return true
		}
	}
	return false
}

// singleQuotedWord текст одним словом в одинарных кавычках
func singleQuotedWord(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
