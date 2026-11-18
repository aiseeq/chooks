package tools

import (
	"slices"
	"strings"
	"testing"
)

// Префиксы вызовов к менеджеру systemd --user, системному менеджеру и logind
const (
	userMgr  = `busctl --user call org.freedesktop.systemd1 /org/freedesktop/systemd1 org.freedesktop.systemd1.Manager `
	sysMgr   = `busctl call org.freedesktop.systemd1 /org/freedesktop/systemd1 org.freedesktop.systemd1.Manager `
	userUnit = `busctl --user call org.freedesktop.systemd1 /org/freedesktop/systemd1/unit/`
	login    = `busctl call org.freedesktop.login1 /org/freedesktop/login1 org.freedesktop.login1.Manager `
	gdbusMgr = `gdbus call --session --dest org.freedesktop.systemd1 --object-path /org/freedesktop/systemd1 --method org.freedesktop.systemd1.Manager.`
	sendMgr  = `dbus-send --session --dest=org.freedesktop.systemd1 --print-reply /org/freedesktop/systemd1 org.freedesktop.systemd1.Manager.`
	qdbusMgr = `qdbus org.freedesktop.systemd1 /org/freedesktop/systemd1 org.freedesktop.systemd1.Manager.`
)

// Тикет kill-guard-dbus-bypass: те же действия, что systemctl stop/kill/
// restart/freeze и loginctl kill-*/terminate-*, напрямую по D-Bus. Юнит
// своей сессии — agent-x-1.scope (в нём claude), рабочего стола —
// plasma-kwin_wayland.service
func TestKillGuardDbusBlocks(t *testing.T) {
	for _, command := range []string{
		// busctl, методы менеджера с юнитом первым аргументом
		userMgr + `KillUnit ssi agent-x-1.scope all 15`,
		userMgr + `KillUnit ssi plasma-kwin_wayland.service all 9`,
		userMgr + `StopUnit ss plasma-kwin_wayland.service replace`,
		userMgr + `RestartUnit ss plasma-kwin_wayland.service replace`,
		userMgr + `TryRestartUnit ss agent-x-1.scope replace`,
		userMgr + `ReloadOrRestartUnit ss plasma-kwin_wayland.service replace`,
		userMgr + `ReloadOrTryRestartUnit ss plasma-kwin_wayland.service replace`,
		userMgr + `FreezeUnit s plasma-kwin_wayland.service`,
		userMgr + `KillUnitSubgroup sssi agent-x-1.scope all / 9`,
		userMgr + `QueueSignalUnit ssii agent-x-1.scope main 15 0`,
		userMgr + `EnqueueUnitJob sss plasma-kwin_wayland.service stop replace`,
		userMgr + `StopUnit ss agents.slice replace`,
		userMgr + `StopUnit ss dbus.socket replace`,
		`busctl -q --user --timeout=5 call org.freedesktop.systemd1 /org/freedesktop/systemd1 org.freedesktop.systemd1.Manager KillUnit ssi plasma-kwin_wayland.service all 15`,
		`sudo ` + sysMgr + `StopUnit ss sddm.service replace`,
		sysMgr + `RestartUnit ss display-manager.service replace`,
		// весь менеджер: смена цели, выход, выключение
		userMgr + `StartUnit ss default.target isolate`,
		userMgr + `EnqueueUnitJob sss default.target start isolate`,
		userMgr + `Exit`,
		sysMgr + `PowerOff`,
		sysMgr + `Reboot`,
		// методы объекта юнита: имя из пути, _2e — точка, _2d — дефис
		userUnit + `plasma_2dkwin_5fwayland_2eservice org.freedesktop.systemd1.Unit Stop s replace`,
		userUnit + `plasma_2dkwin_5fwayland_2eservice org.freedesktop.systemd1.Unit Kill si all 15`,
		userUnit + `agent_2dx_2d1_2escope org.freedesktop.systemd1.Unit Kill si all 9`,
		userUnit + `agent_2dx_2d1_2escope org.freedesktop.systemd1.Unit Freeze`,
		userUnit + `agent_2dx_2d1_2escope org.freedesktop.systemd1.Unit EnqueueJob ss restart replace`,
		// self — юнит самой команды; ID запуска — юнит не назван
		userUnit + `self org.freedesktop.systemd1.Unit Stop s replace`,
		userUnit + `0123456789abcdef0123456789abcdef org.freedesktop.systemd1.Unit Kill si all 15`,
		// юнит, метод, объект, получатель из переменной; юниты из ввода
		userMgr + `KillUnit ssi "$U" all 15`,
		userMgr + `StopUnit ss $(cat unit) replace`,
		userMgr + `$M ssi foo.service all 15`,
		userMgr + `EnqueueUnitJob sss foo.service $JOB replace`,
		`busctl --user call org.freedesktop.systemd1 "$P" org.freedesktop.systemd1.Manager KillUnit ssi foo.service all 15`,
		`busctl --user call "$D" "$P" "$I" KillUnit ssi foo.service all 15`,
		`busctl --user call "$D" "$P" "$I" "$M"`,
		`echo plasma-kwin_wayland.service | xargs ` + userMgr + `StopUnit ss`,
		// шина по адресу: менеджер не виден — проверка по обоим
		`busctl --address=unix:path=/run/user/1000/systemd/private call org.freedesktop.systemd1 /org/freedesktop/systemd1 org.freedesktop.systemd1.Manager StopUnit ss plasma-kwin_wayland.service replace`,
		// gdbus: длинные, короткие опции и опции с =, значения GVariant
		gdbusMgr + `KillUnit plasma-kwin_wayland.service all 15`,
		gdbusMgr + `KillUnit "'agent-x-1.scope'" "'all'" "int32 15"`,
		`gdbus call -e -d org.freedesktop.systemd1 -o /org/freedesktop/systemd1 -m org.freedesktop.systemd1.Manager.StopUnit "'plasma-kwin_wayland.service'" "'replace'"`,
		`gdbus call --session --dest=org.freedesktop.systemd1 --object-path=/org/freedesktop/systemd1 --method=org.freedesktop.systemd1.Manager.StopUnit agent-x-1.scope replace`,
		`gdbus call --session --dest org.freedesktop.systemd1 --object-path /org/freedesktop/systemd1/unit/plasma_2dkwin_5fwayland_2eservice --method org.freedesktop.systemd1.Unit.Stop replace`,
		// dbus-send: тип:значение
		sendMgr + `KillUnit string:plasma-kwin_wayland.service string:all int32:15`,
		sendMgr + `StopUnit string:agent-x-1.scope string:replace`,
		`dbus-send --dest=org.freedesktop.systemd1 --type=method_call /org/freedesktop/systemd1 org.freedesktop.systemd1.Manager.RestartUnit string:plasma-kwin_wayland.service string:replace`,
		// qdbus: метод с интерфейсом и без
		qdbusMgr + `StopUnit plasma-kwin_wayland.service replace`,
		`qdbus6 org.freedesktop.systemd1 /org/freedesktop/systemd1 KillUnit agent-x-1.scope all 15`,
		`qdbus-qt6 org.freedesktop.systemd1 /org/freedesktop/systemd1/unit/agent_2dx_2d1_2escope org.freedesktop.systemd1.Unit.Stop replace`,
		// logind Kill* и Terminate* — целиком, и сигналом 0 тоже (как loginctl)
		login + `KillSession ssi 3 all 15`,
		login + `KillSession ssi 3 all 0`,
		login + `KillUser usi 1000 all 9`,
		login + `TerminateSession s 3`,
		login + `TerminateUser u 1000`,
		login + `TerminateSeat s seat0`,
		`busctl call org.freedesktop.login1 /org/freedesktop/login1/session/_33 org.freedesktop.login1.Session Terminate`,
		`busctl call org.freedesktop.login1 /org/freedesktop/login1/session/_33 org.freedesktop.login1.Session Kill si all 15`,
		`gdbus call --system --dest org.freedesktop.login1 --object-path /org/freedesktop/login1/user/_1000 --method org.freedesktop.login1.User.Kill 15`,
		`dbus-send --system --dest=org.freedesktop.login1 --print-reply /org/freedesktop/login1 org.freedesktop.login1.Manager.KillSession string:3 string:all int32:15`,
		`qdbus --system org.freedesktop.login1 /org/freedesktop/login1 org.freedesktop.login1.Manager.TerminateUser 1000`,
		`busctl call org.freedesktop.login1 /org/freedesktop/login1 org.freedesktop.login1.Manager $M s 3`,
	} {
		if validateKill(t, command).IsValid {
			t.Errorf("должно блокироваться: %s", command)
		}
	}
}

// Соседи: чужая пустышка, сигнал 0, чтение (get-property, introspect,
// list, tree, monitor, Get*/List*), рестарт plasmashell (как systemctl),
// запуск без isolate, вызовы другим службам
func TestKillGuardDbusAllows(t *testing.T) {
	for _, command := range []string{
		userMgr + `KillUnit ssi asg-dummy-kill.service all 15`,
		userMgr + `StopUnit ss asg-dummy-kill.service replace`,
		userMgr + `RestartUnit ss opencode.service replace`,
		userMgr + `RestartUnit ss plasma-plasmashell.service replace`,
		userMgr + `StartUnit ss asg-dummy-kill.service replace`,
		userMgr + `EnqueueUnitJob sss plasma-kwin_wayland.service reload replace`,
		userUnit + `asg_2ddummy_2dkill_2eservice org.freedesktop.systemd1.Unit Stop s replace`,
		// сигнал 0 лишь проверяет, жив ли процесс
		userMgr + `KillUnit ssi plasma-kwin_wayland.service all 0`,
		userMgr + `KillUnit ssi agent-x-1.scope all 0`,
		userUnit + `agent_2dx_2d1_2escope org.freedesktop.systemd1.Unit Kill si all 0`,
		gdbusMgr + `KillUnit agent-x-1.scope all 0`,
		gdbusMgr + `KillUnit "'agent-x-1.scope'" "'all'" "int32 0"`,
		sendMgr + `KillUnit string:agent-x-1.scope string:all int32:0`,
		qdbusMgr + `KillUnit agent-x-1.scope all 0`,
		// чтение
		`busctl --user get-property org.freedesktop.systemd1 /org/freedesktop/systemd1/unit/plasma_2dkwin_5fwayland_2eservice org.freedesktop.systemd1.Unit ActiveState`,
		`busctl --user introspect org.freedesktop.systemd1 /org/freedesktop/systemd1`,
		`busctl --user list`,
		`busctl --user tree org.freedesktop.systemd1`,
		`busctl monitor org.freedesktop.login1`,
		`busctl --user status`,
		userMgr + `GetUnit s plasma-kwin_wayland.service`,
		userMgr + `ListUnits`,
		login + `ListSessions`,
		login + `GetSession s 3`,
		`gdbus introspect --session --dest org.freedesktop.systemd1 --object-path /org/freedesktop/systemd1`,
		`gdbus call --session --dest org.freedesktop.systemd1 --object-path /org/freedesktop/systemd1 --method org.freedesktop.systemd1.Manager.ListUnits`,
		`dbus-send --system --dest=org.freedesktop.login1 --print-reply /org/freedesktop/login1 org.freedesktop.login1.Manager.ListSessions`,
		`qdbus org.freedesktop.systemd1 /org/freedesktop/systemd1`,
		`qdbus org.freedesktop.systemd1`,
		`qdbus`,
		// другие службы
		`busctl --user call org.kde.KWin /KWin org.kde.KWin reconfigure`,
		`qdbus org.kde.KWin /KWin reconfigure`,
		`qdbus org.kde.plasmashell /PlasmaShell org.kde.PlasmaShell.evaluateScript 'x'`,
		`dbus-send --session --dest=org.freedesktop.Notifications /org/freedesktop/Notifications org.freedesktop.Notifications.CloseNotification uint32:5`,
		`busctl --user call "$D" /org/kde/x org.kde.Foo Stop`,
		// ssh: вызов исполнится на другой машине
		`ssh host 'busctl call org.freedesktop.login1 /org/freedesktop/login1 org.freedesktop.login1.Manager KillUser usi 1000 all 9'`,
	} {
		if r := validateKill(t, command); !r.IsValid {
			t.Errorf("не должно блокироваться: %s: %v", command, r.Suggestions)
		}
	}
}

// Причина называет вызов, действие systemctl, юнит и что в нём идёт;
// подсказка — что делать вместо: та же, что у systemctl, у неразобранного
// вызова — назвать литералами
func TestKillGuardDbusTexts(t *testing.T) {
	for _, tc := range []struct{ command, reason, hint string }{
		{userMgr + `KillUnit ssi agent-x-1.scope all 15`,
			"busctl call org.freedesktop.systemd1.Manager.KillUnit (как systemctl --user kill) agent-x-1.scope: в юните идёт claude (PID 2100)",
			systemctlHint("systemctl --user", true)},
		{userUnit + `plasma_2dkwin_5fwayland_2eservice org.freedesktop.systemd1.Unit Stop s replace`,
			"(как systemctl --user stop) plasma-kwin_wayland.service: в юните идёт kwin_wayland (PID 3500)",
			systemctlHint("systemctl --user", true)},
		{`sudo ` + sysMgr + `StopUnit ss sddm.service replace`,
			"(как systemctl stop) sddm.service: в юните идёт sddm (PID 3100)",
			systemctlHint("systemctl", true)},
		{userMgr + `KillUnit ssi "$U" all 15`,
			"юнит вычисляется при запуске", systemctlHint("systemctl --user", false)},
		{userMgr + `Exit`, "(как systemctl --user exit) гасит менеджер systemctl --user", systemctlEndHint("systemctl --user", "exit")},
		{login + `KillSession ssi 3 all 15`,
			"busctl call org.freedesktop.login1.Manager.KillSession (как loginctl kill-* и terminate-*) завершает процессы сеанса", killGuardHint},
		{userMgr + `$M ssi foo.service all 15`, "метод не виден до запуска", "Назови получателя, объект, метод и юнит литералами"},
		{userUnit + `self org.freedesktop.systemd1.Unit Stop s replace`, "self — юнит самой команды", "Назови получателя, объект, метод и юнит литералами"},
	} {
		r := validateKill(t, tc.command)
		reasons, hints := strings.Join(r.Suggestions, "\n"), r.Suggestions
		if r.IsValid || !strings.Contains(reasons, tc.reason) || !slices.ContainsFunc(hints, func(h string) bool { return strings.HasPrefix(h, tc.hint) }) {
			t.Errorf("%s: ждали отказ с «%s» и подсказкой «%s», получили %v %q", tc.command, tc.reason, tc.hint, r.IsValid, r.Suggestions)
		}
	}
}

// Обходы из приёмки 701a45d: пустое имя юнита (systemd берёт юнит
// вызывающего), escape в строке GVariant, аргументы gdbus после --, ID
// запуска в пути в любом регистре и форме GUID, выключение через logind,
// busctl -C со значением (шина capsule — TestKillGuardSessionEndAllows)
func TestKillGuardDbusReviewBlocks(t *testing.T) {
	for _, command := range []string{
		// 1. пустое имя юнита
		userMgr + `StopUnit ss "" replace`,
		userMgr + `KillUnit ssi '' all 9`,
		gdbusMgr + `KillUnit "''" all 9`,
		sendMgr + `KillUnit string: string:all int32:9`,
		qdbusMgr + `StopUnit "" replace`,
		userUnit + `_ org.freedesktop.systemd1.Unit Stop s replace`,
		// 2. escape в строке GVariant: -, \U0000002d, \- — дефис
		gdbusMgr + `StopUnit "'plasma-kwin_wayland.service'" replace`,
		gdbusMgr + `KillUnit "'agent-x-1.scope'" all 15`,
		gdbusMgr + `KillUnit "'agent\U0000002dx-1.scope'" all 15`,
		gdbusMgr + `KillUnit "'agent\-x\-1.scope'" all 15`,
		gdbusMgr + `KillUnit "\"agent-x-1.scope\"" all 15`,
		gdbusMgr + `KillUnit "string 'agent-x-1.scope'" all 15`,
		gdbusMgr + `KillUnit "@s 'agent-x-1.scope'" all 15`,
		// битая последовательность: имя не проверить
		gdbusMgr + `KillUnit "'agent\u002'" all 15`,
		// 3. аргументы gdbus после --
		gdbusMgr + `StopUnit -- "'plasma-kwin_wayland.service'" "'replace'"`,
		gdbusMgr + `KillUnit -- agent-x-1.scope all 15`,
		gdbusMgr + `KillUnit -- "'agent-x-1.scope'" "'all'" "int32 9"`,
		// 4. ID запуска: верхний регистр, GUID с _2d и _2D
		userUnit + `0123456789ABCDEF0123456789ABCDEF org.freedesktop.systemd1.Unit Kill si all 15`,
		userUnit + `01234567_2d89ab_2dcdef_2d0123_2d456789abcdef org.freedesktop.systemd1.Unit Kill si all 15`,
		userUnit + `01234567_2D89AB_2DCDEF_2D0123_2D456789ABCDEF org.freedesktop.systemd1.Unit Stop s replace`,
		userUnit + `_61gent_2Dx_2D1_2Escope org.freedesktop.systemd1.Unit Kill si all 15`,
		// 5. выключение и перезагрузка через logind — как systemctl poweroff
		login + `PowerOff b false`,
		login + `Reboot b false`,
		login + `Halt b false`,
		login + `PowerOffWithFlags t 0`,
		login + `RebootWithFlags t 0`,
		login + `HaltWithFlags t 0`,
		login + `ScheduleShutdown st poweroff 0`,
		login + `ScheduleShutdown st reboot 0`,
		login + `ScheduleShutdown st kexec 0`,
		login + `ScheduleShutdown st "$K" 0`,
		`gdbus call --system --dest org.freedesktop.login1 --object-path /org/freedesktop/login1 --method org.freedesktop.login1.Manager.Reboot false`,
		`dbus-send --system --dest=org.freedesktop.login1 --print-reply /org/freedesktop/login1 org.freedesktop.login1.Manager.PowerOff boolean:false`,
		`qdbus --system org.freedesktop.login1 /org/freedesktop/login1 org.freedesktop.login1.Manager.PowerOff false`,
	} {
		if validateKill(t, command).IsValid {
			t.Errorf("должно блокироваться: %s", command)
		}
	}
}

// Соседи: сигнал 0 и чужой юнит в тех же формах, сон и пробное выключение
// (systemctl suspend хук тоже пускает), чтение logind
func TestKillGuardDbusReviewAllows(t *testing.T) {
	for _, command := range []string{
		userMgr + `KillUnit ssi '' all 0`,
		gdbusMgr + `KillUnit "'agent-x-1.scope'" all 0`,
		gdbusMgr + `KillUnit "'asg-dummy-kill.service'" all 15`,
		gdbusMgr + `KillUnit -- agent-x-1.scope all 0`,
		gdbusMgr + `KillUnit -- "'asg-dummy-kill.service'" "'all'" "int32 15"`,
		userUnit + `asg_2Ddummy_2Dkill_2Eservice org.freedesktop.systemd1.Unit Stop s replace`,
		login + `ScheduleShutdown st dry-poweroff 0`,
		login + `Suspend b false`,
		login + `Hibernate b false`,
		login + `CanPowerOff`,
		login + `CancelScheduledShutdown`,
	} {
		if r := validateKill(t, command); !r.IsValid {
			t.Errorf("не должно блокироваться: %s: %v", command, r.Suggestions)
		}
	}
}

// Причина на пустое имя и на выключение через logind; подсказка к
// выключению — та же, что у systemctl poweroff
func TestKillGuardDbusReviewTexts(t *testing.T) {
	for _, tc := range []struct{ command, reason, hint string }{
		{userMgr + `KillUnit ssi '' all 9`, "(как systemctl --user kill) \"\": пустое имя — юнит самой команды", "Останавливай юнит"},
		{login + `PowerOff b false`, "busctl call org.freedesktop.login1.Manager.PowerOff (как systemctl poweroff) выключает машину", "Команда выключает машину"},
		{login + `ScheduleShutdown st reboot 0`, "(как systemctl reboot) перезагружает машину", "Команда перезагружает машину"},
	} {
		r := validateKill(t, tc.command)
		if r.IsValid || !strings.Contains(strings.Join(r.Suggestions, "\n"), tc.reason) || !slices.ContainsFunc(r.Suggestions, func(h string) bool { return strings.HasPrefix(h, tc.hint) }) {
			t.Errorf("%s: ждали отказ с «%s» и подсказкой «%s», получили %v %q", tc.command, tc.reason, tc.hint, r.IsValid, r.Suggestions)
		}
	}
}
