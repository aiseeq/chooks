package tools

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// patternArgs разобранные аргументы pkill или killall
type patternArgs struct {
	full, exact, isRegex, ignoreCase bool
	// info — справка, версия или список сигналов: никто не будет завершён
	info bool
	// signals — значения всех опций сигнала по порядку (-9, --signal 0, -s 0
	// у killall). Какой из них победит, у pkill зависит от вида опции
	// (--signal перекрывает -SIG), поэтому сигнал 0 засчитывается, только
	// когда нулевые все
	signals []string
	// group — killall -g: сигнал всей группе процессов каждого найденного
	group bool
	// inverse — pkill --inverse: сигнал всем, кроме совпавших
	inverse bool
	// pids — PID из pkill -p (--pid): цели названы прямо
	pids     []string
	patterns []string
}

// noSignal справка, версия, список сигналов или сигнал 0 во всех опциях
// сигнала: никто не будет завершён
func (pa patternArgs) noSignal() bool {
	return pa.info || len(pa.signals) > 0 && !slices.ContainsFunc(pa.signals, func(s string) bool { return s != "0" })
}

// patternOption опция pkill или killall: value — берёт значение, apply —
// что она меняет в разборе (nil — фильтр или флаг без влияния на цели)
type patternOption struct {
	value bool
	apply func(pa *patternArgs, value string)
}

func setFull(pa *patternArgs, _ string)       { pa.full = true }
func setExact(pa *patternArgs, _ string)      { pa.exact = true }
func setIgnoreCase(pa *patternArgs, _ string) { pa.ignoreCase = true }
func setRegex(pa *patternArgs, _ string)      { pa.isRegex = true }
func setInfo(pa *patternArgs, _ string)       { pa.info = true }
func setGroup(pa *patternArgs, _ string)      { pa.group = true }
func setInverse(pa *patternArgs, _ string)    { pa.inverse = true }
func addSignal(pa *patternArgs, v string)     { pa.signals = append(pa.signals, v) }
func addPids(pa *patternArgs, v string)       { pa.pids = append(pa.pids, strings.Split(v, ",")...) }

var (
	flag      = patternOption{}
	withValue = patternOption{value: true}
)

// pkillOptions опции pkill (procps-ng 4.0.6, pkill --help): короткие —
// одной буквой, длинные — с --. -e у pkill — echo (печать убитых), а не
// точное совпадение; -r — состояния процессов со значением, не regexp;
// --inverse (у pkill только длинная, -v — у pgrep) обращает выбор
var pkillOptions = map[string]patternOption{
	"inverse": {apply: setInverse},
	"f":       {apply: setFull}, "full": {apply: setFull},
	"x": {apply: setExact}, "exact": {apply: setExact},
	"i": {apply: setIgnoreCase}, "ignore-case": {apply: setIgnoreCase},
	"p": {value: true, apply: addPids}, "pid": {value: true, apply: addPids},
	"signal": {value: true, apply: addSignal},
	"h":      {apply: setInfo}, "help": {apply: setInfo}, "V": {apply: setInfo}, "version": {apply: setInfo},
	"H": flag, "require-handler": flag, "e": flag, "echo": flag, "m": flag, "mrelease": flag,
	"c": flag, "count": flag, "n": flag, "newest": flag, "o": flag, "oldest": flag,
	"L": flag, "logpidfile": flag, "A": flag, "ignore-ancestors": flag, "Q": flag, "shell-quote": flag,
	"q": withValue, "queue": withValue, "g": withValue, "pgroup": withValue, "G": withValue, "group": withValue,
	"O": withValue, "older": withValue, "P": withValue, "parent": withValue, "s": withValue, "session": withValue,
	"t": withValue, "terminal": withValue, "u": withValue, "euid": withValue, "U": withValue, "uid": withValue,
	"F": withValue, "pidfile": withValue, "r": withValue, "runstates": withValue, "cgroup": withValue,
	"ns": withValue, "nslist": withValue, "env": withValue,
}

// killallOptions опции killall (psmisc 23.7, killall --help): -e — точное
// совпадение длинных имён (без -r killall и так сравнивает имя целиком),
// -i — вопрос перед каждым, -I — без учёта регистра
var killallOptions = map[string]patternOption{
	"r": {apply: setRegex}, "regexp": {apply: setRegex},
	"I": {apply: setIgnoreCase}, "ignore-case": {apply: setIgnoreCase},
	"g": {apply: setGroup}, "process-group": {apply: setGroup},
	"s": {value: true, apply: addSignal}, "signal": {value: true, apply: addSignal},
	"l": {apply: setInfo}, "list": {apply: setInfo}, "V": {apply: setInfo}, "version": {apply: setInfo}, "help": {apply: setInfo},
	"e": flag, "exact": flag, "i": flag, "interactive": flag, "q": flag, "quiet": flag,
	"v": flag, "verbose": flag, "w": flag, "wait": flag,
	"y": withValue, "younger-than": withValue, "o": withValue, "older-than": withValue,
	"u": withValue, "user": withValue, "n": withValue, "ns": withValue, "Z": withValue, "context": withValue,
}

// signalNameRe имя или номер сигнала после - (-9, -KILL, -SIGTERM, -RTMIN+1)
var signalNameRe = regexp.MustCompile(`^((?:SIG)?(?:HUP|INT|QUIT|ILL|TRAP|ABRT|IOT|BUS|FPE|KILL|USR1|SEGV|USR2|PIPE|ALRM|TERM|STKFLT|CHLD|CLD|CONT|STOP|TSTP|TTIN|TTOU|URG|XCPU|XFSZ|VTALRM|PROF|WINCH|IO|POLL|PWR|SYS|RTMIN|RTMAX)(?:[+-][0-9]+)?|[0-9]+)$`)

// parsePatternArgs разбирает флаги и шаблоны; непустая строка — причина отказа
func parsePatternArgs(name string, args []word) (patternArgs, string) {
	pa := patternArgs{isRegex: name == "pkill"}
	options := pkillOptions
	if name == "killall" {
		options = killallOptions
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case !a.literal:
			return pa, fmt.Sprintf("%s %s: шаблон или опция вычисляется при запуске, проверить заранее нельзя", name, a.text)
		case a.text == "--":
			for _, p := range args[i+1:] {
				pa.patterns = append(pa.patterns, p.text)
			}
			i = len(args)
		case len(a.text) > 1 && a.text[0] == '-':
			pa.option(options, args, &i)
		default:
			pa.patterns = append(pa.patterns, a.text)
		}
	}
	if name == "pkill" && len(pa.patterns) > 1 {
		pa.patterns = pa.patterns[len(pa.patterns)-1:]
	}
	if len(pa.patterns) == 0 && len(pa.pids) == 0 && !pa.noSignal() {
		return pa, name + " без шаблона выбирает процессы только по фильтру (-u, -P, -t, -g, -s, --env, -F): pkill -u $USER гасит все сессии разом"
	}
	return pa, ""
}

// option разбирает опцию args[*i]: длинную (--signal 9, --signal=9),
// сигнал (-9, -KILL) или связку коротких (-fi, -fu aisee, -s9) — буква со
// значением берёт остаток слова или следующее слово; i сдвигается на слово
// значения
func (pa *patternArgs) option(options map[string]patternOption, args []word, i *int) {
	a := args[*i].text
	if strings.HasPrefix(a, "--") {
		long, v, hasValue := strings.Cut(a[2:], "=")
		opt := options[long]
		if opt.value && !hasValue {
			v = optionValue(args, i, "").text
		}
		opt.run(pa, v)
		return
	}
	if signalNameRe.MatchString(a[1:]) {
		addSignal(pa, a[1:])
		return
	}
	for k := 1; k < len(a); k++ {
		opt := options[a[k:k+1]]
		if !opt.value {
			opt.run(pa, "")
			continue
		}
		opt.run(pa, optionValue(args, i, a[k+1:]).text)
		return
	}
}

// run применяет опцию к разбору, если она что-то в нём меняет
func (o patternOption) run(pa *patternArgs, value string) {
	if o.apply != nil {
		o.apply(pa, value)
	}
}

// regexp шаблон в том виде, как его сопоставляет утилита: killall без -r
// сравнивает имя целиком, pkill -x — тоже; -i у pkill и -I у killall —
// без учёта регистра
func (pa patternArgs) regexp(p string) (*regexp.Regexp, error) {
	expr, exact := p, pa.exact
	if !pa.isRegex {
		expr, exact = regexp.QuoteMeta(p), true
	}
	if exact {
		expr = "^(?:" + expr + ")$"
	}
	if pa.ignoreCase {
		expr = "(?i)" + expr
	}
	return regexp.Compile(expr)
}

// checkPattern прикидывает, что заденет pkill или killall сейчас: шаблон
// сверяется с теми же полями процессов, что у самой утилиты, PID из -p — с
// сессией. group — отказ из-за сигнала группе (killall -g)
func (t *KillGuardTool) checkPattern(name string, args []word, command string, procs []procInfo) (reason string, group bool) {
	pa, reason := parsePatternArgs(name, args)
	if reason != "" || pa.noSignal() {
		return reason, false
	}
	if pa.group {
		return name + " -g шлёт сигнал всей группе процессов каждого найденного: кто ещё в группе, проверка не сверяет", true
	}
	if pa.inverse {
		return name + " --inverse шлёт сигнал всем процессам, кроме совпавших: сессиям Claude, терминалу и оболочке тоже", false
	}
	ancestors := t.ancestors(procs)
	for _, p := range pa.pids {
		pid, err := strconv.Atoi(p)
		if err != nil {
			return fmt.Sprintf("%s -p %s: цель не PID (%v)", name, p, err), false
		}
		if reason := t.dangerous(pid, procs, ancestors); reason != "" {
			return fmt.Sprintf("%s -p %d: %s", name, pid, reason), false
		}
	}
	for _, p := range pa.patterns {
		re, err := pa.regexp(p)
		if err != nil {
			// glibc читает шаблон иначе, чем Go (pkill 'claud\e' совпадает с
			// claude): кого он заденет, не видно
			return fmt.Sprintf("%s %q: шаблон не разбирается так, как его читает утилита (%v), проверить, кого он заденет, нельзя; упрости шаблон", name, p, err), false
		}
		if pa.full && re.MatchString(command) {
			return fmt.Sprintf("%s -f %q совпадает с текстом самой команды и убьёт оболочку, которая её исполняет; шаблон вида '[x]yz' себя не ловит", name, p), false
		}
		for _, pr := range procs {
			subject := pr.comm
			if pa.full {
				subject = pr.cmdline
			}
			if !re.MatchString(subject) {
				continue
			}
			if reason := t.dangerous(pr.pid, procs, ancestors); reason != "" {
				return fmt.Sprintf("%s %q задевает PID %d: %s", name, p, pr.pid, reason), false
			}
		}
	}
	return "", false
}
