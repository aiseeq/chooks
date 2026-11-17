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
// podman rm/stop/kill. Процесс, запущенный systemd-run, — потомок systemd,
// не агента, и ядро его сигналы не сверяет: он дотянется до чужих
// процессов. Вызов в скрипте ssh исполнится на другой машине
func systemdRunSignalReason(c shellcmd.Call) (reason, hint string) {
	if !slices.Contains(c.Via, "systemd-run") || slices.Contains(c.Via, "ssh") {
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
	default:
		return "", ""
	}
	return "systemd-run запускает " + name + " потомком systemd, а не агента: ядро (agent-signal-guard) его сигналы не сверяет, и он дотянется до процессов других агентов",
		"Шли сигнал прямо, без systemd-run: найди PID отдельной командой (pgrep -a '[x]yz') и убей литералом: kill 12345; контейнер — podman stop имя"
}
