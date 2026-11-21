package tools

import (
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/aiseeq/akit/shellcmd"
)

// Ядро (agent-signal-guard) не даёт агенту послать сигнал чужому процессу,
// но просьба к systemd идёт мимо него: юнит гасит или команду запускает сам
// systemd, и ядро не знает, кто попросил. Поэтому kill guard отказывает
// агенту в остановке юнитов других агентов и в сигналах из-под systemd-run

// agentUnits юниты, где идёт работа других агентов (имя с типом), — почему
// их не трогать. Своя сессия в agent-*.scope — юнит предка, его ловит
// unitReason раньше. run-* — юниты systemd-run без имени: служба, scope и
// таймер, путь, сокет при --on-*, --path-property, --socket-property;
// run-*.mount — точки монтирования под /run, не systemd-run
var agentUnits = []struct {
	re     *regexp.Regexp
	reason string
	worker bool
}{
	{regexp.MustCompile(`^agent-.+\.scope$`), "юнит другого агента (agents.slice/agent-*.scope): с ним гаснет его сессия Claude", false},
	{workerUnitRe, "юнит бойца (worker-*.service): с ним гаснет его работа, а мастер не узнает почему", true},
	{regexp.MustCompile(`^graft-heavy-.+\.scope$`), "тяжёлая команда graft другого агента (graft-heavy-*.scope): гаснут его сборка или тесты", false},
	{regexp.MustCompile(`^run-.+\.(service|scope|timer|path|socket)$`), "юнит systemd-run без имени (run-*): в нём может идти работа другого агента", false},
}

// workerUnitRe юнит бойца (worker run)
var workerUnitRe = regexp.MustCompile(`^worker-.+\.service$`)

// throttleProperties свойства ресурсов, которые только замедляют юнит:
// процесс не останавливается и сигналов не получает. Память (MemoryMax,
// MemoryHigh) и TasksMax сюда не входят: низкий предел убивает процесс
// (OOM) или ломает его работу (fork не проходит)
var throttleProperties = []string{
	"IOReadBandwidthMax", "IOWriteBandwidthMax", "IOReadIOPSMax", "IOWriteIOPSMax",
	"IOWeight", "StartupIOWeight", "IODeviceWeight", "IODeviceLatencyTargetSec",
	"CPUWeight", "StartupCPUWeight", "CPUQuota", "CPUQuotaPeriodSec", "AllowedCPUs", "StartupAllowedCPUs",
}

// throttleOnly все свойства set-property — литералы Имя=значение из
// throttleProperties. Пустой список (свойства не видны, вызов по D-Bus) — нет
func throttleOnly(props []word) bool {
	if len(props) == 0 {
		return false
	}
	for _, p := range props {
		name, _, ok := strings.Cut(p.text, "=")
		if !p.literal || !ok || !slices.Contains(throttleProperties, name) {
			return false
		}
	}
	return true
}

// agentUnitReason отказ на действие над юнитом другого агента: name — имя
// с типом (unitName). Общее для systemctl и вызова тех же методов по D-Bus
// (unitActionReason). hint — подсказка: у бойца — worker stop
func agentUnitReason(scope, name string) (reason, hint string) {
	for _, u := range agentUnits {
		if !u.re.MatchString(name) {
			continue
		}
		if u.worker {
			return u.reason, "Бойца снимает его мастер: worker stop <id> (id — из worker status), worker сам погасит юнит"
		}
		return u.reason, "Чужой юнит гасит его хозяин. Свой процесс в нём убей литералом: посмотри PID (" + scope + " status юнит) и kill 12345 — ядро не даст задеть чужой"
	}
	return "", ""
}

// podmanValues глобальные опции podman со значением отдельным словом
var podmanValues = []string{
	"c", "connection", "url", "identity", "config", "conmon", "cgroup-manager", "db-backend", "events-backend",
	"hooks-dir", "imagestore", "log-level", "module", "network-cmd-path", "network-config-dir", "out",
	"registries-conf", "root", "runroot", "runtime", "runtime-flag", "ssh", "storage-driver", "storage-opt",
	"tmpdir", "volumepath", "cni-config-dir",
}

// podmanStopVerbs команды podman (и podman container, podman pod), которые
// гасят контейнер: restart сначала останавливает его (сигнал процессам)
var podmanStopVerbs = []string{"rm", "stop", "kill", "restart"}

// podmanStop — вызов podman останавливает или удаляет контейнер (pod):
// первая команда после глобальных опций, за container и pod — следующая
func podmanStop(args []string) (string, bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			continue
		case strings.HasPrefix(a, "-"):
			name := strings.TrimLeft(a, "-")
			if !strings.Contains(name, "=") && slices.Contains(podmanValues, name) {
				i++
			}
		case a == "container" || a == "pod":
		default:
			return a, slices.Contains(podmanStopVerbs, a)
		}
	}
	return "", false
}

// systemdRunSignalReason отказ на сигнал из-под systemd-run (в том числе
// через sh -c и обёртки, которые снимает разбор): kill, pkill, killall,
// podman rm/stop/kill, а также команда, которую до запуска не видно
// (systemdRunHiddenReason). Процесс, запущенный systemd-run, — потомок
// systemd, не агента, и ядро его сигналы не сверяет: он дотянется до чужих
// процессов. Вызов в скрипте ssh и systemd-run на другую машину
// (systemdRunRemote) исполнится не здесь. calls — все вызовы команды:
// среди них ищется сам systemd-run
func (s *killScan) systemdRunSignalReason(c shellcmd.Call, calls []shellcmd.Call, feedRead bool) (reason, hint string) {
	k := slices.Index(c.Via, "systemd-run")
	if slices.Contains(c.Via, "ssh") || systemdRunElsewhere(c, calls) {
		return "", ""
	}
	name, signal := signalCommand(c)
	run0 := slices.Contains(c.Via, "run0")
	if k < 0 && !run0 {
		return "", ""
	}
	if k < 0 {
		// run0 запускает команду временной службой, как systemd-run без --scope
		k = slices.Index(c.Via, "run0")
	}
	if !signal {
		if reason := s.systemdRunHiddenReason(c, calls, k, feedRead); reason != "" {
			return reason, systemdRunHiddenHint
		}
		return "", ""
	}
	if !run0 && systemdRunScopeOnly(c, calls) {
		// systemd-run --scope делает exec в своём процессе: сигнальная
		// команда остаётся потомком агента, ядро её сверяет, а цель
		// проверяется, как у прямого вызова
		return "", ""
	}
	return "systemd-run запускает " + name + " потомком systemd, а не агента: ядро (agent-signal-guard) его сигналы не сверяет, и он дотянется до процессов других агентов",
		"Шли сигнал прямо, без systemd-run: найди PID отдельной командой (pgrep -a '[x]yz') и убей литералом: kill 12345; контейнер — podman stop имя"
}

// hiddenTail конец причины отказа на невидимую команду под systemd-run
const hiddenTail = ", а исполнит его потомок systemd, не агента — ядро (agent-signal-guard) его сигналы не сверяет"

// systemdRunHiddenReason команда под systemd-run, которую до запуска не
// видно, — как юнит из переменной у systemctl: имя из переменной или
// подстановки ("$X" 1; путь с литеральным файлом — нет), скрипт, слово
// которого целиком одна подстановка (sh -c "$CMD", eval "$(cat f)"), и
// оболочка, читающая скрипт из ввода, которого не видно (… | systemd-run
// --pipe bash, … | sh -c 'systemd-run --pipe bash', sh < "$F"; cat build.sh
// | … — файл, как sh < build.sh). Подстановка в аргументах скрипта (sh -c "make -C
// $DIR") — не повод: команды скрипта видны, а на месте имени команды её
// ловит то же правило имени у вызова из скрипта
func (s *killScan) systemdRunHiddenReason(c shellcmd.Call, calls []shellcmd.Call, k int, feedRead bool) string {
	if (c.Name == "" || strings.ContainsAny(c.Name, "$`{}")) && !systemdRunPathName(c, calls, k) {
		return "systemd-run запускает команду, имя которой вычисляется при запуске ($X, $(…), `…`): текст команды станет известен только при запуске" + hiddenTail
	}
	script, runner := scriptArgs(c)
	switch {
	case slices.ContainsFunc(script, wholeSubstitution):
		return "systemd-run запускает " + c.Name + " со скриптом из одной подстановки (sh -c \"$CMD\", $(…), `…`): текст станет известен только при запуске" + hiddenTail
	case runner && slices.ContainsFunc(script, computedArg) && !scriptCallsParsed(c, calls):
		return "systemd-run запускает " + c.Name + " со скриптом с подстановкой, в котором разбор не видит ни одной команды (sh -c \"$CMD\"x, \"$A$B\"): текст станет известен только при запуске" + hiddenTail
	case runner || !shellcmd.IsShell(c.Name):
		return ""
	}
	if !shellReadsInput(c.Args) {
		// файл скрипта (sh x.sh) — вне этой проверки
		return ""
	}
	if reason := s.systemdRunInputReason(c, calls, feedRead); reason != "" {
		return reason
	}
	if s.killText && !feedRead && s.unreadInput(c, calls) {
		return "systemd-run запускает " + c.Name + ", который читает скрипт с ввода, а разбор этого скрипта не видит (ввод задан функции, exec <, циклу, xargs), и в команде есть kill: проверить скрипт до запуска нельзя" + hiddenTail
	}
	return ""
}

// unreadInput оболочка c под службой systemd-run (не --scope) читает скрипт
// с ввода, текста которого разбор не прочёл: форм подачи ввода много
// (функция, exec <, цикл с < <(…), xargs, coproc), и каждую разбор не
// знает. Прочитан — разбор нашёл вызовы скрипта (scriptBodyParsed,
// inputCallsParsed). Ввод, текст которого известен и без вызовов, — не
// повод: свой heredoc и <<< (пустое тело, комментарий) и литеральный файл
// (sh < build.sh — файл скрипта, как sh build.sh), кроме ввода xargs перед
// c: его строки станут аргументами c (-c и скрипт)
func (s *killScan) unreadInput(c shellcmd.Call, calls []shellcmd.Call) bool {
	switch {
	case systemdRunScopeOnly(c, calls) || scriptBodyParsed(c, calls) || inputCallsParsed(c, calls):
		return false
	case afterXargs(c):
		// ввод у xargs и parallel перед c — не скрипт c, а его аргументы
		return true
	}
	for _, r := range slices.Backward(c.Redirs) {
		if r.N == "" || r.N == "0" {
			switch r.Op {
			case "<<", "<<-", "<<<":
				return false
			case "<", "<>":
				return computedArg(r.Target) || strings.HasPrefix(r.Target, "<(")
			}
		}
	}
	return true
}

// inputCallsParsed — разбор нашёл вызовы скрипта, который оболочка c
// читает с ввода (тело heredoc у неё, у блока или функции вокруг): с Via
// цепочки c впереди и местом за c в тексте
func inputCallsParsed(c shellcmd.Call, calls []shellcmd.Call) bool {
	chain := append(slices.Clone(c.Via), c.Name)
	return callsInScript(c, calls, chain, false)
}

// callsInScript finds calls belonging to c's script or input stream. When
// withinRaw is set, the nested call must also fall inside c's source text.
func callsInScript(c shellcmd.Call, calls []shellcmd.Call, chain []string, withinRaw bool) bool {
	for i := range calls {
		x := &calls[i]
		if len(x.Via) < len(chain) || !slices.Equal(x.Via[:len(chain)], chain) {
			continue
		}
		if !withinRaw && (x.Offset < 0 || c.Offset < 0 || x.Offset > c.Offset) {
			return true
		}
		if withinRaw && (x.Offset < 0 || c.Offset >= 0 && x.Offset > c.Offset && x.Offset < c.Offset+len(c.Raw)) {
			return true
		}
	}
	return false
}

// systemdRunInputReason оболочка c под systemd-run читает скрипт из ввода,
// которого не видно: пайп, который разбор не прочёл (feedRead — прочёл
// literalFeed), пайп за xargs (слова ввода — аргументы c) или
// перенаправление из подстановки у самой c или у команды вокруг неё (< "$F",
// <<< "$CMD"; sh -c 'systemd-run --pipe bash' < "$F")
func (s *killScan) systemdRunInputReason(c shellcmd.Call, calls []shellcmd.Call, feedRead bool) string {
	for _, r := range c.Redirs {
		if r.N != "" && r.N != "0" {
			continue
		}
		if r.Op == "<" && computedArg(r.Target) || r.Op == "<<<" && wholeSubstitution(r.Target) {
			return "systemd-run запускает " + c.Name + " со скриптом на вводе из " + r.Op + " " + r.Target + ": текст не виден до запуска" + hiddenTail
		}
	}
	switch {
	case s.stdinOf(c).src != stdinPipe:
	case afterXargs(c):
		return "systemd-run запускает " + c.Name + " за xargs (… | xargs systemd-run --pipe " + c.Name + "): слова ввода станут его аргументами (-c и скрипт), их не видно до запуска" + hiddenTail
	case !scriptBodyParsed(c, calls) && !feedRead:
		return "systemd-run запускает " + c.Name + ", который читает скрипт из ввода (… | systemd-run --pipe " + c.Name + ", sh -c '…' < \"$F\"): текст не виден до запуска" + hiddenTail
	}
	return ""
}

// scriptCallsParsed — разбор нашёл вызовы в скрипте, который исполняет c
// (sh -c, eval): с Via цепочки c и местом в тексте за именем c. Нет ни
// одного — скрипт не разобран (sh -c "$CMD"x, "$A$B")
func scriptCallsParsed(c shellcmd.Call, calls []shellcmd.Call) bool {
	via := append(slices.Clone(c.Via), c.Name)
	return callsInScript(c, calls, via, true)
}

// shellReadsInput — оболочка без -c с аргументами args читает скрипт со
// своего ввода: без операнда, с -s (за ним операнды — аргументы скрипта:
// bash -s -- a b) или с операндом - и путём к вводу (stdinPath: /dev/stdin,
// /dev/fd/./0, /proc/$$/fd/0). Иной операнд — файл скрипта. Опции — как у shellScript
func shellReadsInput(args []string) bool {
	stdin := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return stdin || i+1 >= len(args) || stdinPath(args[i+1])
		case a == "--rcfile" || a == "--init-file":
			i++
		case strings.HasPrefix(a, "--"):
		case a == "-":
			return true
		case strings.HasPrefix(a, "-") || strings.HasPrefix(a, "+"):
			stdin = stdin || strings.HasPrefix(a, "-") && strings.Contains(a[1:], "s")
			if strings.HasSuffix(a, "o") || strings.HasSuffix(a, "O") {
				i++
			}
		default:
			return stdin || stdinPath(a)
		}
	}
	return true
}

// inputTaker — вызов, которому ввод обёртки (run0, pkexec) нужен как
// скрипт: оболочка без -c (её ввод не забирает xargs или parallel перед
// ней) или обёртка, за которой разбор проверит её команду
func inputTaker(c shellcmd.Call) bool {
	if _, ok := rewrittenCommand(c); ok {
		return true
	}
	if _, runner := scriptArgs(c); runner || !shellcmd.IsShell(c.Name) {
		return false
	}
	return !slices.Contains(c.Via, "xargs") && !slices.Contains(c.Via, "parallel")
}

// scriptBodyParsed — разбор прочёл скрипт, который оболочка c получает на
// ввод (echo 'make' | sh): его вызовы идут с Via цепочки c, где «pipe»
// стоит на уровне пайпа: впереди у пайпа в команде, после sh -c у пайпа в
// скрипте (sh -c 'echo make | systemd-run --pipe bash' — sh -c pipe
// systemd-run bash)
func scriptBodyParsed(c shellcmd.Call, calls []shellcmd.Call) bool {
	chain := append(slices.Clone(c.Via), c.Name)
	for j := range chain {
		via := slices.Concat(chain[:j], []string{"pipe"}, chain[j:])
		for i := range calls {
			x := &calls[i]
			if len(x.Via) >= len(via) && slices.Equal(x.Via[len(x.Via)-len(via):], via) {
				return true
			}
		}
	}
	return false
}

// systemdRunHiddenHint подсказка к отказу на невидимую команду под
// systemd-run: служба окружение агента не наследует, значения передаются
// опциями
const systemdRunHiddenHint = "Назови команду под systemd-run литералом, а значения передай опциями: -E VAR=\"$VAR\" (окружение службы) и --working-directory=\"$DIR\": systemd-run --user -E DIR=\"$DIR\" sh -c 'make -C \"$DIR\"'"

// systemdRunRemote — вызов c исполнит systemd-run на другой машине: -H/--host
// (по ssh) или -M/--machine с литеральным именем чужой машины или
// контейнера. -M .host, пустое имя, user@ и user@.host — эта машина, как у
// machinectl shell (localMachine). k — место systemd-run в c.Via. Сам
// systemd-run — вызов с Via до него; если таких несколько и текст c ни
// с одним не совпадает (скрипт sh -c под ним), на другую машину должны
// идти все — иначе проверка как здесь
func systemdRunRemote(c shellcmd.Call, calls []shellcmd.Call, k int) bool {
	runs, own := systemdRunCalls(c, calls, k)
	if own {
		return systemdRunHostOption(runs[0].Args)
	}
	if len(runs) == 0 {
		return false
	}
	for i := range runs {
		if !systemdRunHostOption(runs[i].Args) {
			return false
		}
	}
	return true
}

// systemdRunElsewhere — какой-то systemd-run над c исполнит его на другой
// машине (systemdRunRemote): run0 systemd-run -H host kill 1 — служба
// здесь запускает systemd-run, а kill идёт на host. systemd-run прямо под
// systemd-run разбор вызовом не отдаёт: его опции — слова внешнего после
// его опций (nestedSystemdRunRemote)
func systemdRunElsewhere(c shellcmd.Call, calls []shellcmd.Call) bool {
	for k, v := range c.Via {
		if v != "systemd-run" {
			continue
		}
		if systemdRunRemote(c, calls, k) {
			return true
		}
		if runs, own := systemdRunCalls(c, calls, k); own && nestedSystemdRunRemote(runs[0].Args) {
			return true
		}
	}
	return false
}

// nestedSystemdRunRemote — команда systemd-run с опциями args — снова
// systemd-run (и так далее), и один из них идёт на другую машину
func nestedSystemdRunRemote(args []string) bool {
	for range maxNestedRewrites {
		start := systemdRunCommandStart(args)
		if start >= len(args) || path.Base(args[start]) != "systemd-run" {
			return false
		}
		args = args[start+1:]
		if systemdRunHostOption(args) {
			return true
		}
	}
	return false
}

// signalCommand c шлёт сигнал или гасит контейнер: kill, pkill, killall,
// podman rm/stop/kill; name — как назвать её в причине
func signalCommand(c shellcmd.Call) (name string, ok bool) {
	switch {
	case slices.Contains(signalNames, c.Name):
		return c.Name, true
	case c.Name == "podman" || c.Name == "podman-remote":
		verb, ok := podmanStop(c.Args)
		return c.Name + " " + verb, ok
	}
	return c.Name, false
}

// systemdRunScopeOnly каждый systemd-run над c найден и запускает его в
// режиме --scope (процесс остаётся потомком агента), а не службой
func systemdRunScopeOnly(c shellcmd.Call, calls []shellcmd.Call) bool {
	for k, via := range c.Via {
		if via != "systemd-run" {
			continue
		}
		runs, own := systemdRunCalls(c, calls, k)
		if !own || !systemdRunScopeOption(runs[0].Args) {
			return false
		}
	}
	return true
}

// systemdRunScopeOption среди опций systemd-run (до команды) есть --scope
func systemdRunScopeOption(args []string) bool {
	return parseSystemdRunOptions(plainWords(args)).scope
}

// systemdRunCalls вызовы systemd-run, под которым может идти c: с Via до
// него (k — его место в c.Via). own — найден тот самый (тот же текст, что
// у c): он один в ответе
func systemdRunCalls(c shellcmd.Call, calls []shellcmd.Call, k int) (runs []shellcmd.Call, own bool) {
	for i := range calls {
		x := &calls[i]
		if x.Name == "systemd-run" && slices.Equal(x.Via, c.Via[:k]) {
			if x.Raw == c.Raw {
				return []shellcmd.Call{*x}, true
			}
			runs = append(runs, *x)
		}
	}
	return runs, false
}

// systemdRunPathName — c идёт прямо под systemd-run, и имя его — путь с
// подстановкой каталога и литеральным файлом ("$(go env GOPATH)/bin/tool"):
// программа известна до запуска, как у substitutedPathName. Слово имени —
// первый операнд самого systemd-run
func systemdRunPathName(c shellcmd.Call, calls []shellcmd.Call, k int) bool {
	runs, own := systemdRunCalls(c, calls, k)
	if !own || k != len(c.Via)-1 {
		return false
	}
	args := runs[0].Args
	start := parseSystemdRunOptions(plainWords(args)).start
	return start < len(args) && substitutedPathName(shellcmd.Call{Raw: args[start]}) != ""
}

// plainWords слова вызова как word
func plainWords(args []string) []word {
	out := make([]word, 0, len(args))
	for _, a := range args {
		out = append(out, word{text: a, literal: !strings.ContainsAny(a, "$`")})
	}
	return out
}

// systemdRunLongFlags длинные опции systemd-run без значения (getopt
// systemd v259, src/run/run.c); со значением — длинные из systemdRunValues.
// Вместе — имена, до которых getopt_long дополняет сокращение (--mach)
var systemdRunLongFlags = []string{
	"help", "version", "user", "system", "scope", "slice-inherit", "remain-after-exit", "send-sighup", "wait",
	"tty", "pty", "pty-late", "pipe", "quiet", "verbose", "on-timezone-change", "on-clock-change", "no-block",
	"no-ask-password", "collect", "same-dir", "same-root-dir", "shell", "ignore-failure", "no-pager",
}

// systemdRunLongName полное имя длинной опции systemd-run, как его находит
// getopt_long: точное совпадение или единственное, которое начинается с
// name; "" — нет такой или сокращение неоднозначно (systemd-run не
// запустится)
func systemdRunLongName(name string) string {
	return uniqueLongOptionName(name, systemdRunValues, systemdRunLongFlags)
}

// systemdRunHostOption — systemd-run исполнит команду на другой машине.
// Транспорт задаёт последняя из -H/--host, -M/--machine и -C/--capsule: каждая
// перезаписывает arg_transport и arg_host (src/run/run.c). -H — другая
// машина по ssh; -M — другая, если имя литеральное и не эта машина
// (localMachine); -C — капсула на этой машине. Опции кончаются на первом
// слове команды и на --
func systemdRunHostOption(args []string) bool {
	return parseSystemdRunOptions(plainWords(args)).remote
}
