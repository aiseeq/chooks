package tools

import (
	"slices"
	"strings"
	"testing"
)

// Тикет kill-guard-foreign-agent-units, а: stop/kill/restart/freeze юнита
// другого агента через systemctl и D-Bus — отказ. Своя сессия — agent-x-1.scope
// (в нём claude), чужая — agent-y-2.scope
func TestKillGuardForeignUnitsBlock(t *testing.T) {
	var commands []string
	for _, unit := range []string{"agent-y-2.scope", "worker-x.service", "graft-heavy-x.scope", "run-x.service"} {
		for _, verb := range []string{"stop", "kill", "restart", "freeze", "try-restart", "kill -s 9"} {
			commands = append(commands, "systemctl --user "+verb+" "+unit)
		}
		commands = append(commands,
			userMgr+`StopUnit ss `+unit+` replace`,
			userMgr+`KillUnit ssi `+unit+` all 15`,
			userMgr+`RestartUnit ss `+unit+` replace`,
			userMgr+`FreezeUnit s `+unit,
			gdbusMgr+`StopUnit "'`+unit+`'" "'replace'"`,
			sendMgr+`KillUnit string:`+unit+` string:all int32:9`,
			qdbusMgr+`StopUnit `+unit+` replace`,
		)
	}
	commands = append(commands,
		// имя без типа systemctl дополняет .service; прочие типы run-*
		`systemctl --user stop worker-job-1117-1029-audit-thunk`,
		`systemctl --user stop run-u123`,
		`systemctl --user stop run-sleep.scope`,
		`systemctl --user stop run-r0123abcd.timer`,
		`systemctl --user stop agent-chooks-master-1331786-r1.scope worker-x.service`,
		`sudo systemctl stop run-u5.service`,
		`systemctl --user --now disable worker-x.service`,
		// методы объекта юнита: имя из пути
		userUnit+`agent_2dy_2d2_2escope org.freedesktop.systemd1.Unit Stop s replace`,
		userUnit+`worker_2dx_2eservice org.freedesktop.systemd1.Unit Kill si all 15`,
		// юнит из переменной, ввода, шаблоном
		`systemctl --user stop "$W"`,
		`systemctl --user stop 'worker-*'`,
		`systemctl --user kill 'agent-*.scope'`,
		`systemctl --user stop graft-heavy-{1,2}.scope`,
		`echo worker-x.service | xargs systemctl --user stop`,
		userMgr+`StopUnit ss "$U" replace`,
	)
	for _, command := range commands {
		if validateKill(t, command).IsValid {
			t.Errorf("должно блокироваться: %s", command)
		}
	}
}

// Своя пустышка вне этих имён, сигнал 0, чтение — пропуск
func TestKillGuardForeignUnitsAllow(t *testing.T) {
	for _, command := range []string{
		`systemctl --user stop asg-dummy-kill.service`,
		`systemctl --user restart asg-dummy-kill`,
		userMgr + `StopUnit ss asg-dummy-kill.service replace`,
		// сигнал 0 лишь проверяет, жив ли процесс
		`systemctl --user kill -s 0 worker-x.service`,
		`systemctl --user kill --signal=0 agent-y-2.scope`,
		userMgr + `KillUnit ssi graft-heavy-x.scope all 0`,
		qdbusMgr + `KillUnit run-x.service all 0`,
		// чтение
		`systemctl --user status worker-x.service`,
		`systemctl --user status agent-y-2.scope graft-heavy-x.scope run-x.service`,
		`systemctl --user show -p MainPID worker-x.service`,
		`systemctl --user is-active agent-y-2.scope`,
		`systemctl --user list-units 'worker-*' 'agent-*'`,
		`journalctl --user -u worker-x.service`,
		userMgr + `GetUnit s worker-x.service`,
		// запуск — не остановка; похожие имена другого вида
		`systemctl --user start worker-x.service`,
		`systemctl --user stop agent-y-2.service`,
		`systemctl --user stop run-docker-netns-x.mount`,
		`systemctl --user stop myworker-x.service`,
	} {
		if r := validateKill(t, command); !r.IsValid {
			t.Errorf("не должно блокироваться: %s: %v", command, r.Suggestions)
		}
	}
}

// Тикет, г: systemd-run с любыми опциями, если внутри kill, pkill, killall
// или podman rm/stop/kill — отказ, и через sh -c и обёртки. PID 455274
// (opencode) без systemd-run убить можно — отказ только из-за systemd-run
func TestKillGuardSystemdRunSignalBlocks(t *testing.T) {
	for _, command := range []string{
		`systemd-run --user kill 1`,
		`systemd-run --user kill 455274`,
		`systemd-run --scope sh -c 'pkill opencode'`,
		`systemd-run --user --scope bash -c "kill -9 455274"`,
		`systemd-run --user podman stop x`,
		`systemd-run --user podman rm -f x`,
		`systemd-run --user podman kill x`,
		`systemd-run --user podman container stop x`,
		`systemd-run --user podman --log-level debug pod rm p`,
		`systemd-run --user --unit=x -p MemoryMax=1G killall sleep`,
		`systemd-run --user -- env timeout 5 /usr/bin/kill 455274`,
		`sudo systemd-run -q bash -c 'killall5 -15'`,
		`run0 systemd-run kill 455274`,
		`systemd-run --user --on-active=30 kill 455274`,
		`nice systemd-run --user pkill -f farm`,
	} {
		if validateKill(t, command).IsValid {
			t.Errorf("должно блокироваться: %s", command)
		}
	}
}

func TestKillGuardSystemdRunSignalAllows(t *testing.T) {
	for _, command := range []string{
		`systemd-run --user sleep 1`,
		`systemd-run --user --scope make test`,
		`systemd-run --user podman ps`,
		`systemd-run --user podman run --rm alpine true`,
		`systemd-run --user podman logs kill`,
		`systemd-run --user sh -c 'echo kill'`,
		`kill 455274`,
		`podman stop x`,
	} {
		if r := validateKill(t, command); !r.IsValid {
			t.Errorf("не должно блокироваться: %s: %v", command, r.Suggestions)
		}
	}
}

// Отказ называет, чей юнит; у бойца подсказка — worker stop
func TestKillGuardForeignUnitTexts(t *testing.T) {
	for _, tc := range []struct{ command, reason, hint string }{
		{`systemctl --user stop worker-x.service`, "systemctl --user stop worker-x.service: юнит бойца", "Бойца снимает его мастер: worker stop"},
		{userMgr + `KillUnit ssi worker-x.service all 15`, "worker-x.service: юнит бойца", "Бойца снимает его мастер: worker stop"},
		{`systemctl --user kill agent-y-2.scope`, "systemctl --user kill agent-y-2.scope: юнит другого агента", "Чужой юнит гасит его хозяин"},
		{`systemctl --user freeze graft-heavy-x.scope`, "тяжёлая команда graft другого агента", "Чужой юнит гасит его хозяин"},
		{`systemctl --user restart run-x.service`, "юнит systemd-run без имени", "Чужой юнит гасит его хозяин"},
		{`systemd-run --user kill 1`, "systemd-run запускает kill потомком systemd", "Шли сигнал прямо, без systemd-run"},
		{`systemd-run --user podman stop x`, "systemd-run запускает podman stop потомком systemd", "Шли сигнал прямо, без systemd-run"},
	} {
		r := validateKill(t, tc.command)
		if r.IsValid || !strings.Contains(strings.Join(r.Suggestions, "\n"), tc.reason) || !slices.ContainsFunc(r.Suggestions, func(h string) bool { return strings.HasPrefix(h, tc.hint) }) {
			t.Errorf("%s: ждали отказ с «%s» и подсказкой «%s», получили %v %q", tc.command, tc.reason, tc.hint, r.IsValid, r.Suggestions)
		}
	}
	// своя сессия — прежняя причина: в юните идёт claude
	r := validateKill(t, `systemctl --user stop agent-x-1.scope`)
	if r.IsValid || !strings.Contains(strings.Join(r.Suggestions, "\n"), "в юните идёт claude") {
		t.Errorf("свой agent-x-1.scope: ждали прежний отказ, получили %v %q", r.IsValid, r.Suggestions)
	}
}
