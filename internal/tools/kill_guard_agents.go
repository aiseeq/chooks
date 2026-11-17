package tools

import (
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
	{regexp.MustCompile(`^worker-.+\.service$`), "юнит бойца (worker-*.service): с ним гаснет его работа, а мастер не узнает почему", true},
	{regexp.MustCompile(`^graft-heavy-.+\.scope$`), "тяжёлая команда graft другого агента (graft-heavy-*.scope): гаснут его сборка или тесты", false},
	{regexp.MustCompile(`^run-.+\.(service|scope|timer|path|socket)$`), "юнит systemd-run без имени (run-*): в нём может идти работа другого агента", false},
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
// гасят контейнер
var podmanStopVerbs = []string{"rm", "stop", "kill"}

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
// podman rm/stop/kill, а также команда, которую до запуска не видно: имя
// из переменной или подстановки ("$X" 1), скрипт sh -c из них (sh -c
// "$CMD") — как юнит из переменной у systemctl. Процесс, запущенный
// systemd-run, — потомок systemd, не агента, и ядро его сигналы не
// сверяет: он дотянется до чужих процессов. Вызов в скрипте ssh и
// systemd-run на другую машину (systemdRunRemote) исполнится не здесь.
// calls — все вызовы команды: среди них ищется сам systemd-run
func systemdRunSignalReason(c shellcmd.Call, calls []shellcmd.Call) (reason, hint string) {
	k := slices.Index(c.Via, "systemd-run")
	if k < 0 || slices.Contains(c.Via, "ssh") || systemdRunRemote(c, calls, k) {
		return "", ""
	}
	name := c.Name
	switch {
	case slices.Contains(signalNames, c.Name):
	case c.Name == "podman" || c.Name == "podman-remote":
		verb, ok := podmanStop(c.Args)
		if !ok {
			return "", ""
		}
		name = c.Name + " " + verb
	case (c.Name == "" || strings.ContainsAny(c.Name, "$`{}")) && !systemdRunPathName(c, calls, k):
		return "systemd-run запускает команду, имя которой вычисляется при запуске ($X, $(…), `…`): что исполнится потомком systemd, а не агента, проверить нельзя — ядро (agent-signal-guard) его сигналы не сверяет", systemdRunHiddenHint
	default:
		if script, _ := scriptArgs(c); slices.ContainsFunc(script, computedArg) {
			return "systemd-run запускает " + c.Name + " со скриптом из подстановки $ или ` (sh -c \"$CMD\"): текст станет известен только при запуске, а исполнит его потомок systemd, не агента — ядро (agent-signal-guard) его сигналы не сверяет", systemdRunHiddenHint
		}
		return "", ""
	}
	return "systemd-run запускает " + name + " потомком systemd, а не агента: ядро (agent-signal-guard) его сигналы не сверяет, и он дотянется до процессов других агентов",
		"Шли сигнал прямо, без systemd-run: найди PID отдельной командой (pgrep -a '[x]yz') и убей литералом: kill 12345; контейнер — podman stop имя"
}

// systemdRunHiddenHint подсказка к отказу на невидимую команду под systemd-run
const systemdRunHiddenHint = "Назови команду под systemd-run литералом; переменную раскрывай внутри скрипта, в одинарных кавычках: systemd-run --user sh -c 'make -C \"$DIR\"'"

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
	return len(runs) > 0 && !slices.ContainsFunc(runs, func(x shellcmd.Call) bool { return !systemdRunHostOption(x.Args) })
}

// systemdRunCalls вызовы systemd-run, под которым может идти c: с Via до
// него (k — его место в c.Via). own — найден тот самый (тот же текст, что
// у c): он один в ответе
func systemdRunCalls(c shellcmd.Call, calls []shellcmd.Call, k int) (runs []shellcmd.Call, own bool) {
	for _, x := range calls {
		if x.Name == "systemd-run" && slices.Equal(x.Via, c.Via[:k]) {
			if x.Raw == c.Raw {
				return []shellcmd.Call{x}, true
			}
			runs = append(runs, x)
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
	ops := optionOperands(plainWords(runs[0].Args), systemdRunValues)
	return len(ops) > 0 && substitutedPathName(shellcmd.Call{Raw: ops[0].text}) != ""
}

// plainWords слова вызова как word
func plainWords(args []string) []word {
	out := make([]word, 0, len(args))
	for _, a := range args {
		out = append(out, word{text: a, literal: !strings.ContainsAny(a, "$`")})
	}
	return out
}

// systemdRunHostOption — среди опций systemd-run (до первого слова команды)
// есть -H/--host или -M/--machine с литеральным именем другой машины
func systemdRunHostOption(args []string) bool {
	for i := 0; i < len(args); i++ {
		t := args[i]
		if t == "--" || !strings.HasPrefix(t, "-") || t == "-" {
			return false
		}
		name, value := "", ""
		if n, v, inline := strings.Cut(strings.TrimPrefix(t, "--"), "="); strings.HasPrefix(t, "--") {
			name, value = n, v
			if !inline && slices.Contains(systemdRunValues, n) && i+1 < len(args) {
				i++
				value = args[i]
			}
		} else {
			var next bool
			name, value, next = shortValue(t, systemdRunValues)
			if next && i+1 < len(args) {
				i++
				value = args[i]
			}
		}
		switch {
		case name == "H" || name == "host":
			return true
		case name == "M" || name == "machine":
			if !strings.ContainsAny(value, "$`\"'") && !localMachine(value) {
				return true
			}
		}
	}
	return false
}
