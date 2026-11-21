package tools

import (
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

// feedLinesMark пометка в Via вызова из строки задания parallel или sem
// простой формы: дописанный ввод уже стоит в тексте строки
const feedLinesMark = "feed-lines"

// notLevels слова Via, которые не уровень обёртки: pipe и feed-lines —
// пометки разбора, command, builtin, exec и time снимает сама оболочка
var notLevels = []string{"-c", "pipe", "command", "builtin", "exec", "time", feedLinesMark}

// wrapperLevels число уровней обёрток в Via вызова. Оболочка с литеральным
// текстом (bash -c, bash -euo pipefail -c, sh -ec: akit пишет их как
// «оболочка, -c») уровнем не считается — команда в ней судится как прямая
func wrapperLevels(via []string) int {
	n := 0
	for i, v := range via {
		if !slices.Contains(notLevels, v) && (i+1 == len(via) || via[i+1] != "-c") {
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
// рядом со словом сигнала. Вызов в скрипте ssh и за systemd-run на другую
// машину исполнится не здесь
func (s *killScan) depthReason(c shellcmd.Call, calls []shellcmd.Call) string {
	if wrapperLevels(c.Via) < 2 || slices.Contains(c.Via, "ssh") || systemdRunElsewhere(c, calls) {
		return ""
	}
	chain := strings.Join(slices.DeleteFunc(slices.Clone(c.Via), func(v string) bool { return slices.Contains(notLevels, v) }), " → ")
	if name, ok := depthSignal(c); ok && !harmlessKill(c) {
		return name + " под обёртками " + chain + ": глубже одной обёртки хук команду не разбирает"
	}
	if (c.Name == "" || strings.ContainsAny(c.Name, "$`{}")) && s.signalWord {
		return "имя команды под обёртками " + chain + " вычисляется при запуске, а в команде есть слово kill: глубже одной обёртки хук команду не разбирает"
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

// parallelValueOptions частые опции parallel со значением отдельным словом.
// Прочие считаются флагами: ошибка в месте команды бывает только в сложной
// форме, а её слова проверяет signalWords
var parallelValueOptions = []string{"-j", "--jobs", "-n", "--max-args", "-P", "-L", "-l", "-d", "--delimiter",
	"-S", "--sshlogin", "--joblog", "--results", "--workdir", "--env", "--block", "--tagstring", "--halt",
	"--timeout", "--delay", "-C", "--colsep", "--retries", "--tmpdir", "--header", "--arg-sep", "-s", "-e", "-E", "-I"}

// feedForm слова parallel или sem: команда и источники
type feedForm struct {
	cmd []string
	// lists — значения списков :::; stdin — источник — ввод (:::: -, -a -
	// или без источников)
	lists [][]string
	// simple — опций и подстановок {} нет, источники — списки ::: или один
	// ввод: строки заданий видны (feedLines); wait — sem --wait без --sql*;
	// files — источник — файл
	stdin, simple, wait, files bool
}

// parseFeed слова parallel (sem — с semaphore)
func parseFeed(args []string, semaphore bool) feedForm {
	f := feedForm{simple: true}
	i, semaphore, wait := f.options(args, semaphore)
	if semaphore {
		f.cmd, f.wait = args[i:], wait
		return f
	}
	j := i
	for j < len(args) && !slices.Contains(feedMarkers, args[j]) {
		j++
	}
	f.cmd = args[i:j]
	f.sources(args[j:])
	f.stdin = f.stdin || len(f.lists) == 0 && !f.files
	if f.files || f.stdin && len(f.lists) > 0 || slices.ContainsFunc(f.cmd, func(w string) bool { return strings.Contains(w, "{") }) ||
		slices.ContainsFunc(f.lists, func(l []string) bool { return slices.ContainsFunc(l, computedArg) }) {
		f.simple = false
	}
	return f
}

// options разбирает опции в начале args: i — место первого слова после
// них; semaphore — sem или --semaphore; wait — --wait без --sql*
func (f *feedForm) options(args []string, semaphore bool) (i int, sem, wait bool) {
	sql := false
	for ; i < len(args) && strings.HasPrefix(args[i], "-") && args[i] != "-"; i++ {
		a := args[i]
		if a == "--" {
			return i + 1, semaphore, wait && !sql
		}
		switch {
		case (a == "-a" || a == "--arg-file") && i+1 < len(args):
			i++
			f.source(args[i])
			continue
		case a == "--semaphore":
			semaphore = true
		case a == "--wait" || a == "-W":
			wait = true
		case strings.HasPrefix(a, "--sql"):
			sql = true
		}
		f.simple = false
		if slices.Contains(parallelValueOptions, a) {
			i++
		}
	}
	return min(i, len(args)), semaphore, wait && !sql
}

// sources разбирает источники после команды: списки ::: и файлы ::::
func (f *feedForm) sources(words []string) {
	from := ""
	for _, w := range words {
		switch {
		case slices.Contains(feedMarkers, w):
			from = w
			if w == ":::" {
				f.lists = append(f.lists, nil)
			}
			f.simple = f.simple && !strings.HasSuffix(w, "+")
		case from == ":::":
			f.lists[len(f.lists)-1] = append(f.lists[len(f.lists)-1], w)
		case from == "::::":
			f.source(w)
		}
	}
}

// source источник-файл: - — ввод
func (f *feedForm) source(file string) {
	f.stdin, f.files = f.stdin || file == "-", f.files || file != "-"
}

// maxFeedLines предел строк заданий простой формы: больше — сложная форма
const maxFeedLines = 64

// feedInput ввод вызова c текстом: <<< и heredoc без подстановок, пайп из
// echo или printf с литералом. bodies — тексты всех перенаправлений ввода
// и echo или printf в пайпе (слова сигнала ищутся во всех); literal — последний
// ввод известен, text — он
func (s *killScan) feedInput(c shellcmd.Call) (text string, literal bool, bodies []string) {
	redirected := false
	for _, r := range c.Redirs {
		if r.N != "" && r.N != "0" || !strings.HasPrefix(r.Op, "<") {
			continue
		}
		redirected = true
		switch r.Op {
		case "<<<":
			bodies = append(bodies, r.Target)
			text, literal = r.Target+"\n", !strings.ContainsAny(r.Target, "$`\\")
		case "<<", "<<-":
			bodies = append(bodies, r.Body)
			text, literal = r.Body, !strings.ContainsAny(r.Body, "$`\\")
		default:
			text, literal = "", false
		}
	}
	if redirected {
		return text, literal, bodies
	}
	in := s.inputOf(c)
	if in.src != stdinPipe || in.feed == "" {
		return "", in.src == stdinOwn, nil
	}
	calls, err := shellcmd.Calls(strings.TrimSuffix(in.feed, " | sh"))
	if err != nil {
		s.tool.Logger().Debug("kill_guard: пайп в parallel не разобран, ввод не литерал", "feed", in.feed, "error", err)
		return "", false, nil
	}
	if len(calls) != 1 || calls[0].Name != "echo" && calls[0].Name != "printf" {
		return "", false, nil
	}
	if v := echoedValue(calls[0]); v != "" {
		return v + "\n", true, []string{in.feed}
	}
	return "", false, []string{in.feed}
}

// feedLines строки заданий простой формы: значения ввода или произведение
// списков :::; без команды строка — сама команда, с командой — её
// аргументы в кавычках, как их дописывает parallel. ok false — строк
// больше предела
func feedLines(f feedForm, input string) ([]string, bool) {
	rows := [][]string{{}}
	if f.stdin {
		rows = nil
		for line := range strings.SplitSeq(strings.TrimSuffix(input, "\n"), "\n") {
			if line != "" {
				rows = append(rows, []string{line})
			}
		}
	}
	for _, l := range f.lists {
		var next [][]string
		for _, r := range rows {
			for _, v := range l {
				next = append(next, append(slices.Clone(r), v))
			}
		}
		if rows = next; len(rows) > maxFeedLines {
			return nil, false
		}
	}
	lines := make([]string, 0, len(rows))
	for _, r := range rows {
		if len(f.cmd) == 0 {
			lines = append(lines, strings.Join(r, " "))
			continue
		}
		words := slices.Clone(f.cmd)
		for _, v := range r {
			words = append(words, singleQuotedWord(v))
		}
		lines = append(lines, strings.Join(words, " "))
	}
	return lines, len(rows) <= maxFeedLines
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

// feedCall проверяет parallel и sem: команду и аргументы они собирают из
// своих слов и источников. Простая форма с литеральным вводом — строки
// заданий проверяются вызовами под parallel, как прямые. Прочее хук не
// вычисляет: отказ, если в словах и вводе есть сигнал или остановка, или
// если имя команды приходит из источников под второй обёрткой
func (s *killScan) feedCall(c shellcmd.Call, calls []shellcmd.Call) error {
	if c.Name != "parallel" && c.Name != "sem" || slices.Contains(c.Via, "ssh") || systemdRunElsewhere(c, calls) {
		return nil
	}
	f := parseFeed(c.Args, c.Name == "sem")
	if f.wait {
		return nil
	}
	input, literal, bodies := s.feedInput(c)
	if f.simple && (literal || !f.stdin) {
		if lines, ok := feedLines(f, input); ok {
			return s.feedLinesCalls(c, calls, lines)
		}
	}
	via := strings.Join(append(slices.Clone(c.Via), c.Name), " → ") + ": "
	if name, ok := signalWords(strings.Join(slices.Concat(c.Args, bodies), "\n")); ok {
		s.addToolReason(via+name+" с целями или именем из источников и опций "+c.Name+": такую форму хук не вычисляет", depthHint, false)
	} else if len(f.cmd) == 0 && (wrapperLevels(c.Via) >= 1 || !literal && s.signalWord) {
		s.addToolReason(via+"имя команды "+c.Name+" берёт из источников: под второй обёрткой или рядом со словом kill хук его не вычисляет", depthHint, false)
	}
	return nil
}

// expandedFeed — c из слов команды parallel или sem простой формы с
// литеральным вводом: её строки заданий проверены вызовами (feedLinesCalls),
// дописанный ввод c уже виден в них
func (s *killScan) expandedFeed(c shellcmd.Call, calls []shellcmd.Call) bool {
	for i := range calls {
		p := &calls[i]
		if p.Name != "parallel" && p.Name != "sem" || p.Offset < 0 || c.Offset < p.Offset || c.Offset >= p.Offset+len(p.Raw) || c.Name == p.Name && slices.Equal(c.Args, p.Args) {
			continue
		}
		f := parseFeed(p.Args, p.Name == "sem")
		input, literal, _ := s.feedInput(*p)
		if _, ok := feedLines(f, input); ok && f.simple && (literal || !f.stdin) {
			return true
		}
	}
	return false
}

// feedLinesCalls проверяет строки заданий простой формы вызовами под c
func (s *killScan) feedLinesCalls(c shellcmd.Call, calls []shellcmd.Call, lines []string) error {
	for _, line := range lines {
		inner, err := shellcmd.Calls(line)
		if err != nil {
			if s.killText {
				s.addToolReason(c.Name+": строка задания «"+line+"» не разобрана ("+err.Error()+"): проверить её нельзя", depthHint, false)
			}
			continue
		}
		for i := range inner {
			inner[i].Via = slices.Concat(c.Via, []string{c.Name, feedLinesMark}, inner[i].Via)
			inner[i].Offset = -1
		}
		for i := range inner {
			if err := s.call(inner[i], slices.Concat(calls, inner)); err != nil {
				return err
			}
		}
	}
	return nil
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
			s.addReason("parallel или run0 за обёрткой: " + err.Error() + ": проверить их команду нельзя")
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
