package tools

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/aiseeq/akit/shellcmd"
)

const depthHint = "Запусти напрямую: сигнал или остановку — отдельной командой без обёрток (kill 12345, systemctl --user stop имя.service), цель найди отдельной командой заранее (pgrep -a '[x]yz', systemctl --user list-units)"

var run0WordRe = regexp.MustCompile(`(^|[^[:alnum:]_.-])run0([^[:alnum:]_.-]|$)`)
var notLevels = []string{"-c", "--command", "pipe", "command", "builtin", "exec", "time", "eval", "-exec", "-execdir", "-ok", "-okdir"}

func wrapperLevels(via []string) int {
	n := 0
	for i, v := range via {
		base := path.Base(v)
		busyboxShell := base == "busybox" && i+1 < len(via) && slices.Contains([]string{"sh", "ash", "bash"}, path.Base(via[i+1]))
		if !busyboxShell && !slices.Contains(notLevels, v) && !inlineShells[base] && base != "fish" {
			n++
		}
	}
	return n
}

var stopVerbs = slices.Concat(systemctlStopVerbs, systemctlEndVerbs, loginctlEndVerbs)

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
func (s *killScan) depthReason(c shellcmd.Call, calls []shellcmd.Call) string {
	if c.Name == "run0" && c.Offset < 0 && strings.ContainsAny(c.Raw, "$`") && visibleRun0Signal(c) {
		return "имя команды вычисляется при запуске, а в тексте видна команда сигнала: такую форму хук не разбирает"
	}
	if wrapperLevels(c.Via) < 2 || slices.Contains(c.Via, "ssh") || systemdRunElsewhere(c, calls) {
		return ""
	}
	chain := strings.Join(slices.DeleteFunc(slices.Clone(c.Via), func(v string) bool { return slices.Contains(notLevels, v) }), " → ")
	if name, ok := depthSignal(c); ok && !harmlessDepthSignal(c) {
		return name + " под обёртками " + chain + ": глубже одной обёртки хук команду не разбирает"
	}
	if _, ok := signalWords(c.Raw); ok && (c.Name == "" || strings.ContainsAny(c.Name, "$`")) {
		return "имя команды под обёртками " + chain + " вычисляется при запуске, а в её тексте видна команда сигнала: глубже одной обёртки хук команду не разбирает"
	}
	return ""
}
func harmlessKill(c shellcmd.Call) bool {
	if c.Name != "kill" || len(c.Args) == 0 {
		return false
	}
	targets, zero, computed := killOptions(plainWords(c.Args))
	return computed == "" && (targets == nil && slices.Contains([]string{"-l", "-L", "--list", "--table"}, c.Args[0]) ||
		zero && len(targets) > 0 && !afterXargs(c) && !slices.ContainsFunc(targets, func(w word) bool { return strings.HasPrefix(w.text, "-") }))
}
func harmlessDepthSignal(c shellcmd.Call) bool {
	if harmlessKill(c) {
		return true
	}
	switch c.Name {
	case "killall":
		return slices.Contains(c.Args, "-l") || slices.Contains(c.Args, "--help")
	case "pkill":
		return slices.Contains(c.Args, "-V") || slices.Contains(c.Args, "--help")
	case "systemctl":
		return slices.Contains(c.Args, "--help") || slices.Contains(c.Args, "-h")
	}
	return false
}

var feedMarkers = []string{":::", "::::", ":::+", "::::+"}
var feedSplitOptions = []string{"--pipe", "--pipepart", "--cat", "--fifo", "-C", "--colsep", "--arg-sep", "--arg-file-sep", "-d", "--delimiter", "-0", "--null"}

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
			sources++
		case strings.HasPrefix(from, "::::"):
			file(a)
		default:
			values, literal = append(values, a), literal && !computedArg(a)
		}
	}
	return head, values, stdin || name != "sem" && sources == 0, literal && sources <= 1
}

var feeders = []string{"parallel", "sem", "xargs"}

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
			if _, ok := depthSignal(calls[j]); ok && !harmlessDepthSignal(calls[j]) || slices.Contains(feeders, calls[j].Name) {
				sig = append(sig, calls[j])
			}
		}
		if len(sig) > 0 || start >= 0 || i == len(head) || i == 0 || !strings.HasPrefix(head[i-1], "-") {
			return sig, nil
		}
	}
	return nil, nil
}
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
func visibleRun0Signal(c shellcmd.Call) bool {
	calls, _ := shellcmd.Calls(literalShellWords(c.Args))
	for _, r := range c.Redirs {
		more, _ := shellcmd.Calls(r.Target + "\n" + r.Body)
		calls = append(calls, more...)
	}
	return slices.ContainsFunc(calls, func(c shellcmd.Call) bool { _, ok := depthSignal(c); return ok && !harmlessDepthSignal(c) })
}
func (s *killScan) feedCall(c shellcmd.Call, calls []shellcmd.Call) error {
	if !slices.Contains(feeders, c.Name) || slices.Contains(c.Via, "ssh") || systemdRunElsewhere(c, calls) {
		return nil
	}
	head, values, stdin, literal := feedWords(c.Name, c.Args)
	if semWaits(c.Name, head) {
		return nil
	}
	shown := s.feedShownInput(c)
	if stdin {
		literal = false
	}
	text := feedText(head, values, shown)
	sig, err := feedSignals(head, values, feedStart(&c, calls))
	if err != nil {
		s.tool.Logger().Debug("kill_guard: строки заданий не разобраны", "call", c.Name, "error", err)
		s.feedDeny(c, nil, text)
		return nil
	}
	if !literal && len(feedTemplateArgs(c)) == 0 {
		source, parseErr := shellcmd.Calls(shown)
		if parseErr != nil {
			s.tool.Logger().Debug("kill_guard: команда из ввода feeder не разобрана", "call", c.Name, "error", parseErr)
			if name, ok := signalWords(shown); ok {
				via := strings.Join(append(slices.Clone(c.Via), c.Name), " → ")
				s.addToolReason(via+": видимая команда "+name+" из ввода feeder не разобрана", depthHint, false)
			}
		} else {
			sourceSignals := slices.DeleteFunc(source, func(c shellcmd.Call) bool {
				_, ok := depthSignal(c)
				return !ok || harmlessDepthSignal(c)
			})
			if err := s.callFeedSignals(c, calls, sourceSignals); err != nil {
				return err
			}
			if len(sourceSignals) > 0 {
				s.feedDeny(c, sourceSignals, text)
			}
		}
	}
	return s.scanFeederCalls(c, calls, head, values, sig, literal, text)
}
func (s *killScan) feedShownInput(c shellcmd.Call) string {
	shown := ""
	input := s.inputOf(c)
	if input.src == stdinPipe {
		source, err := shellcmd.Calls(strings.TrimSuffix(input.feed, " | sh"))
		if err != nil {
			s.tool.Logger().Warn("kill_guard: ввод feeder не разобран", "call", c.Name, "error", err)
		} else if len(source) == 1 && (source[0].Name == "echo" || source[0].Name == "printf") {
			shown = echoedValue(source[0])
		}
	} else if input.src == stdinRedirect {
		for _, r := range c.Redirs {
			if r.N == "" || r.N == "0" {
				shown = strings.Join([]string{r.Target, r.Body}, "\n")
			}
		}
		if shown == "" && slices.ContainsFunc(c.Args, func(a string) bool { return a == "-a" || a == "--arg-file" }) {
			shown = s.command
		}
	}
	argFile := slices.Contains(c.Args, "-a") || slices.Contains(c.Args, "--arg-file")
	if shown == "" && argFile && strings.Contains(s.command, "<<") {
		shown = s.command
	}
	return shown
}
func (s *killScan) scanFeederCalls(c shellcmd.Call, calls []shellcmd.Call, head, values []string, sig []shellcmd.Call, literal bool, text string) error {
	if !literal {
		if err := s.callFeedSignals(c, calls, sig); err != nil {
			return err
		}
		s.feedDeny(c, sig, text)
		return nil
	}
	if len(sig) == 0 && slices.ContainsFunc(head, func(a string) bool {
		return strings.Contains(a, "{") || a == "-I" || a == "-i" || strings.HasPrefix(a, "--replace")
	}) {
		s.feedDeny(c, nil, strings.Join(values, "\n"))
	}
	return s.callFeedSignals(c, calls, sig)
}
func (s *killScan) callFeedSignals(c shellcmd.Call, calls, sig []shellcmd.Call) error {
	for i := range sig {
		sig[i].Via = slices.Concat(c.Via, []string{c.Name}, sig[i].Via)
		sig[i].Offset = -1
		if err := s.call(sig[i], slices.Concat(calls, sig)); err != nil {
			return err
		}
	}
	return nil
}
func semWaits(name string, head []string) bool {
	return name == "sem" && slices.ContainsFunc(head, func(a string) bool { return a == "--wait" || a == "-W" }) && !slices.ContainsFunc(head, func(a string) bool { return strings.HasPrefix(a, "--sql") })
}
func feedText(head, values []string, shown string) string {
	words := slices.Concat(values, []string{shown})
	if slices.ContainsFunc(head, func(a string) bool { return a == "--arg-sep" || a == "--arg-file-sep" }) {
		words = append(words, head...)
	}
	return strings.Join(words, "\n")
}
func feedStart(c *shellcmd.Call, calls []shellcmd.Call) int {
	via := append(slices.Clone(c.Via), c.Name)
	for i := range calls {
		if o := &calls[i]; o.Raw == c.Raw && slices.Equal(o.Via, via) {
			return slices.Index(c.Args, o.Name)
		}
	}
	return -1
}
func (s *killScan) signalIn(sig []shellcmd.Call) (string, bool) {
	for i := range sig {
		if n, ok := depthSignal(sig[i]); ok {
			return n, true
		}
		if !slices.Contains(feeders, sig[i].Name) {
			continue
		}
		head, values, _, _ := feedWords(sig[i].Name, sig[i].Args)
		sub, err := feedSignals(head, values, -1)
		n, ok := s.signalIn(sub)
		if err != nil {
			s.tool.Logger().Debug("kill_guard: вложенные задания feeder не разобраны", "call", sig[i].Name, "error", err)
			n, ok = signalWords(feedText(head, values, ""))
		}
		if ok {
			return n, true
		}
	}
	return "", false
}
func (s *killScan) feedDeny(c shellcmd.Call, sig []shellcmd.Call, shown string) {
	name, ok := signalWords(shown)
	commandSignal := false
	if n, yes := s.signalIn(sig); yes {
		name, ok = n, true
		commandSignal = true
	}
	via := slices.Concat(slices.Clone(c.Via), []string{c.Name})
	argFile := slices.Contains(c.Args, "-a") || slices.Contains(c.Args, "--arg-file")
	if ok && (commandSignal || feedShellConsumesInput(c) &&
		(argFile || slices.Contains(c.Args, ":::") || slices.Contains(c.Args, ":::+") || s.feedWrapperLevels(c, via) >= 2)) {
		s.addToolReason(strings.Join(via, " → ")+": "+name+" с целями или командой из источников "+c.Name+" (файл, ввод, --pipe): такую форму хук не вычисляет", depthHint, false)
	}
}
func (s *killScan) feedWrapperLevels(c shellcmd.Call, via []string) int {
	head := feedTemplateArgs(c)
	parsed, err := shellcmd.Calls(strings.Join(head, " "))
	if err == nil && len(parsed) > 0 {
		via = slices.Concat(via, parsed[0].Via, []string{parsed[0].Name})
	} else if err != nil {
		s.tool.Logger().Debug("kill_guard: шаблон feeder не разобран", "call", c.Name, "error", err)
		text := strings.Join(head, " ")
		for _, wrapper := range []string{"sudo", "doas", "pkexec"} {
			if strings.Contains(text, wrapper) && !slices.Contains(via, wrapper) {
				via = append(via, wrapper)
			}
		}
		if strings.Contains(text, "-c") && slices.ContainsFunc(head, func(a string) bool {
			return inputShellName(a)
		}) {
			via = append(via, "shell -c")
		}
	}
	if at := strings.Index(c.Raw, c.Name); at >= 0 {
		prefix := c.Raw[:at]
		for _, wrapper := range []string{"sudo", "doas", "pkexec", "systemd-run", "run0", "setsid", "timeout", "nohup", "nice", "unshare"} {
			if strings.Contains(prefix, wrapper) && !slices.Contains(via, wrapper) {
				via = append(via, wrapper)
			}
		}
	}
	if slices.Contains(head, "-c") && slices.ContainsFunc(head, func(a string) bool {
		return inputShellName(a)
	}) {
		via = append(via, "shell -c")
	}
	return wrapperLevels(via)
}
func inputShellName(name string) bool {
	return inlineShells[path.Base(name)] || path.Base(name) == "fish"
}
func feedTemplateArgs(c shellcmd.Call) []string {
	head, _, _, _ := feedWords(c.Name, c.Args)
	for len(head) > 0 && strings.HasPrefix(head[0], "-") {
		value := slices.Contains([]string{"-a", "--arg-file", "-I", "-i", "-j", "--jobs", "--joblog", "--colsep", "-d", "--delimiter", "--arg-sep", "--arg-file-sep", "-n", "--max-args", "-L", "--max-lines"}, head[0])
		head = head[1:]
		if value && len(head) > 0 {
			head = head[1:]
		}
	}
	return head
}
func feedShellConsumesInput(c shellcmd.Call) bool {
	parsed, err := shellcmd.Calls(strings.Join(feedTemplateArgs(c), " "))
	if err == nil {
		for i := range parsed {
			call := &parsed[i]
			for i, a := range call.Args {
				if shellCOption.MatchString(a) && i+1 < len(call.Args) {
					body := strings.Join(call.Args[i+1:], " ")
					return body != "true" && !strings.HasPrefix(body, "echo")
				}
			}
		}
	}
	return true
}
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
func (s *killScan) withWrappedFeeds(calls []shellcmd.Call) []shellcmd.Call {
	more, err := wrappedFeeds(calls)
	if err != nil {
		s.tool.Logger().Debug("kill_guard: parallel или run0 за обёрткой не найти", "error", err)
		s.addWrappedFeedParseError(err)
	}
	return append(calls, more...)
}
func (s *killScan) addWrappedFeedParseError(err error) {
	if s.killText {
		s.addToolReason("parallel или run0 за обёрткой: "+err.Error()+": проверить их команду нельзя", depthHint, false)
	}
}
func hasCall(calls []shellcmd.Call, x *shellcmd.Call) bool {
	for i := range calls {
		if o := &calls[i]; o.Name == x.Name && o.Raw == x.Raw && slices.Equal(o.Args, x.Args) {
			return true
		}
	}
	return false
}
func remoteBefore(words []string) bool {
	for i, w := range words {
		if path.Base(w) == "systemd-run" && systemdRunHostOption(words[i+1:]) {
			return true
		}
	}
	return false
}
func singleQuotedWord(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
