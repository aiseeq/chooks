package tools

import (
	"slices"
	"strings"
	"testing"
)

// Тикет kill-guard-session-end-forms: формы, гасящие сессию или рабочий
// стол мимо хука, общие для systemctl и D-Bus. Юнит своей сессии —
// agent-x-1.scope (в нём claude), рабочего стола — plasma-kwin_wayland.service,
// системный — sddm.service и user@1000.service
func TestKillGuardSessionEndBlocks(t *testing.T) {
	const userBus = `unix:path=/run/user/1000/bus`
	for _, command := range []string{
		// запуск exit.target, shutdown.target у менеджера пользователя — как
		// systemctl --user exit; цели выключения у системного — как poweroff
		`systemctl --user start exit.target`,
		`systemctl --user start shutdown.target`,
		`systemctl --user restart exit.target`,
		`systemctl --user enable --now exit.target`,
		`sudo systemctl start poweroff.target`,
		`systemctl start reboot.target`,
		`systemctl start shutdown.target`,
		userMgr + `StartUnit ss exit.target replace`,
		userMgr + `StartUnit ss shutdown.target fail`,
		userMgr + `StartUnitWithFlags sst exit.target replace 0`,
		userMgr + `EnqueueUnitJob sss exit.target start replace`,
		sysMgr + `StartUnit ss poweroff.target replace-irreversibly`,
		gdbusMgr + `StartUnit exit.target replace`,
		userUnit + `exit_2etarget org.freedesktop.systemd1.Unit Start s replace`,
		// set-property на юнит своей сессии или рабочего стола
		`systemctl --user set-property agent-x-1.scope MemoryMax=1M`,
		`systemctl --user set-property --runtime agent-x-1.scope MemoryMax=1M`,
		`systemctl --user set-property plasma-kwin_wayland.service CPUQuota=1%`,
		`systemctl --user set-property "$U" MemoryMax=1M`,
		userMgr + `SetUnitProperties sba"(sv)" agent-x-1.scope true 1 MemoryMax t 1048576`,
		userMgr + `SetUnitProperties sba"(sv)" plasma-kwin_wayland.service true 1 CPUQuotaPerSecUSec t 1000`,
		userUnit + `agent_2dx_2d1_2escope org.freedesktop.systemd1.Unit SetProperties ba"(sv)" true 1 MemoryMax t 1048576`,
		// адрес шины подменён в окружении: менеджер — любой из двух
		`DBUS_SYSTEM_BUS_ADDRESS=` + userBus + ` busctl call org.freedesktop.systemd1 /org/freedesktop/systemd1 org.freedesktop.systemd1.Manager StopUnit ss plasma-kwin_wayland.service replace`,
		`env DBUS_SYSTEM_BUS_ADDRESS=` + userBus + ` gdbus call --system --dest org.freedesktop.systemd1 --object-path /org/freedesktop/systemd1 --method org.freedesktop.systemd1.Manager.StopUnit plasma-kwin_wayland.service replace`,
		`DBUS_SYSTEM_BUS_ADDRESS=` + userBus + ` systemctl stop plasma-kwin_wayland.service`,
		`DBUS_SYSTEM_BUS_ADDRESS=` + userBus + ` systemctl kill agent-x-1.scope`,
		`export DBUS_SYSTEM_BUS_ADDRESS=` + userBus + `; dbus-send --system --dest=org.freedesktop.systemd1 /org/freedesktop/systemd1 org.freedesktop.systemd1.Manager.StopUnit string:plasma-kwin_wayland.service string:replace`,
		`DBUS_SESSION_BUS_ADDRESS=unix:path=/run/dbus/system_bus_socket systemctl --user stop sddm.service`,
		// run0 и pkexec — обёртки, как sudo
		`run0 systemctl kill user@1000.service`,
		`run0 busctl call org.freedesktop.systemd1 /org/freedesktop/systemd1 org.freedesktop.systemd1.Manager KillUnit ssi user@1000.service all 15`,
		`pkexec busctl call org.freedesktop.systemd1 /org/freedesktop/systemd1 org.freedesktop.systemd1.Manager StopUnit ss sddm.service replace`,
		`run0 -u root --nice 5 systemctl stop sddm.service`,
		`run0 --user=root systemctl stop sddm`,
		`pkexec --user root systemctl stop display-manager`,
		`run0 -- loginctl terminate-user aisee`,
		`run0 bash -c 'systemctl stop sddm'`,
		`run0 kill 1`,
		`pkexec kill -9 -1`,
		`run0 systemctl poweroff`,
		// имя команды из подстановки: аргументы — разрушающий вызов
		`$(echo busctl) --user call org.freedesktop.systemd1 /org/freedesktop/systemd1 org.freedesktop.systemd1.Manager StopUnit ss plasma-kwin_wayland.service replace`,
		`eval "$(echo busctl) --user call org.freedesktop.systemd1 /org/freedesktop/systemd1 org.freedesktop.systemd1.Manager StopUnit ss plasma-kwin_wayland.service replace"`,
		"`echo systemctl` --user stop plasma-kwin_wayland.service",
		`$(echo loginctl) terminate-user aisee`,
		`$(echo systemctl) --user set-property agent-x-1.scope MemoryMax=1M`,
		`X=systemctl; $X --user start exit.target`,
		// KDE: выход из сеанса, остановка панели и композитора
		`qdbus org.kde.Shutdown /Shutdown org.kde.Shutdown.logout`,
		`qdbus org.kde.Shutdown /Shutdown logoutAndReboot`,
		`qdbus org.kde.ksmserver /KSMServer logout 0 0 0`,
		`qdbus org.kde.ksmserver /KSMServer org.kde.KSMServerInterface.logout 0 0 0`,
		`qdbus org.kde.plasmashell /MainApplication quit`,
		`qdbus6 org.kde.KWin /MainApplication quit`,
		`gdbus call --session --dest org.kde.Shutdown --object-path /Shutdown --method org.kde.Shutdown.logout`,
		`dbus-send --session --dest=org.kde.plasmashell /MainApplication org.qtproject.Qt.QCoreApplication.quit`,
		`busctl --user call org.kde.plasmashell /MainApplication org.qtproject.Qt.QCoreApplication quit`,
		// logind ReleaseSession — как Kill*, Terminate*
		login + `ReleaseSession s 3`,
		`gdbus call --system --dest org.freedesktop.login1 --object-path /org/freedesktop/login1 --method org.freedesktop.login1.Manager.ReleaseSession 3`,
	} {
		if validateKill(t, command).IsValid {
			t.Errorf("должно блокироваться: %s", command)
		}
	}
}

// Соседи: запуск и свойства чужого юнита, чтение, адрес шины не меняется
// (только читается) или вызов не разрушающий, обёртка над безопасной
// командой, вычисляемое имя с безопасными аргументами, прочие вызовы KDE,
// шина capsule (busctl -C: не эта сессия)
func TestKillGuardSessionEndAllows(t *testing.T) {
	for _, command := range []string{
		`systemctl --user start asg-dummy-kill.service`,
		`systemctl --user start default.target`,
		`systemctl --user status exit.target`,
		`systemctl --user cat shutdown.target`,
		userMgr + `StartUnit ss asg-dummy-kill.service replace`,
		userMgr + `StartUnit ss default.target replace`,
		userMgr + `EnqueueUnitJob sss asg-dummy-kill.service start replace`,
		`systemctl --user set-property asg-dummy-kill.service MemoryMax=1M`,
		`systemctl --user set-property opencode.service CPUWeight=50`,
		userMgr + `SetUnitProperties sba"(sv)" asg-dummy-kill.service true 1 MemoryMax t 1048576`,
		`systemctl --user show -p MemoryMax agent-x-1.scope`,
		`DBUS_SYSTEM_BUS_ADDRESS=unix:path=/run/user/1000/bus systemctl stop asg-dummy-kill.service`,
		`DBUS_SESSION_BUS_ADDRESS=x busctl --user call org.freedesktop.systemd1 /org/freedesktop/systemd1 org.freedesktop.systemd1.Manager KillUnit ssi agent-x-1.scope all 0`,
		`echo $DBUS_SESSION_BUS_ADDRESS; systemctl --user stop asg-dummy-kill.service`,
		`run0 systemctl status sddm`,
		`pkexec systemctl restart asg-dummy-kill.service`,
		`run0 kill 455274`,
		`run0 --help`,
		`pkexec --version`,
		`$(which systemctl) --user stop asg-dummy-kill.service`,
		`$(echo busctl) --user call org.freedesktop.systemd1 /org/freedesktop/systemd1 org.freedesktop.systemd1.Manager GetUnit s plasma-kwin_wayland.service`,
		`$X --user status plasma-kwin_wayland.service`,
		`qdbus org.kde.KWin /KWin reconfigure`,
		`qdbus org.kde.plasmashell /PlasmaShell org.kde.PlasmaShell.evaluateScript 'x'`,
		`qdbus org.kde.LogoutPrompt /LogoutPrompt promptLogout`,
		`qdbus org.kde.Solid.PowerManagement /org/kde/Solid/PowerManagement/Actions/SuspendSession suspendToRam`,
		login + `ListSessions`,
		login + `GetSession s 3`,
		`busctl -C foo call org.freedesktop.login1 /org/freedesktop/login1 org.freedesktop.login1.Manager KillSession ssi 3 all 15`,
		`busctl --capsule foo --user call org.freedesktop.systemd1 /org/freedesktop/systemd1 org.freedesktop.systemd1.Manager StopUnit ss plasma-kwin_wayland.service replace`,
		`busctl -C foo --user call org.freedesktop.systemd1 /org/freedesktop/systemd1 org.freedesktop.systemd1.Manager KillUnit ssi agent-x-1.scope all 15`,
		`busctl -C foo list`,
	} {
		if r := validateKill(t, command); !r.IsValid {
			t.Errorf("не должно блокироваться: %s: %v", command, r.Suggestions)
		}
	}
}

// Причина называет форму и что погаснет; подсказка — что делать вместо
func TestKillGuardSessionEndTexts(t *testing.T) {
	for _, tc := range []struct{ command, reason, hint string }{
		{`systemctl --user start exit.target`, "systemctl --user start exit.target гасит менеджер systemctl --user", systemctlEndHint("systemctl --user", "exit")},
		{userMgr + `StartUnit ss shutdown.target replace`, "(как systemctl --user start) shutdown.target останавливает все службы менеджера", "Команда останавливает все службы менеджера"},
		{`systemctl --user set-property agent-x-1.scope MemoryMax=1M`, "systemctl --user set-property agent-x-1.scope: в юните идёт claude (PID 2100)", "Свойства (лимит памяти, CPU) меняй у юнита без процессов сессии"},
		{`DBUS_SYSTEM_BUS_ADDRESS=unix:path=/run/user/1000/bus systemctl stop plasma-kwin_wayland.service`, "systemctl stop (адрес шины подменён в окружении — как systemctl --user stop) plasma-kwin_wayland.service: в юните идёт kwin_wayland", "Останавливай юнит"},
		{`run0 systemctl kill user@1000.service`, "systemctl kill user@1000.service: в юните идёт", "Останавливай юнит"},
		{`$(echo busctl) --user call org.freedesktop.systemd1 /org/freedesktop/systemd1 org.freedesktop.systemd1.Manager StopUnit ss plasma-kwin_wayland.service replace`, "имя команды вычисляется при запуске ($(…), `…` или $X), а с именем busctl это отказ: busctl call", "Останавливай юнит"},
		{`qdbus org.kde.Shutdown /Shutdown org.kde.Shutdown.logout`, "qdbus org.kde.Shutdown.logout к org.kde.Shutdown завершает сеанс KDE", "Выход из сеанса запускает пользователь сам"},
		{`qdbus org.kde.plasmashell /MainApplication quit`, "qdbus quit к org.kde.plasmashell гасит plasmashell", "Выход из сеанса запускает пользователь сам"},
		{login + `ReleaseSession s 3`, "busctl call org.freedesktop.login1.Manager.ReleaseSession (как loginctl", killGuardHint},
		{`systemctl --user stop ''`, `systemctl --user stop "": пустое имя юнита systemctl отвергает`, "Останавливай юнит"},
	} {
		r := validateKill(t, tc.command)
		if r.IsValid || !strings.Contains(strings.Join(r.Suggestions, "\n"), tc.reason) || !slices.ContainsFunc(r.Suggestions, func(h string) bool { return strings.HasPrefix(h, tc.hint) }) {
			t.Errorf("%s: ждали отказ с «%s» и подсказкой «%s», получили %v %q", tc.command, tc.reason, tc.hint, r.IsValid, r.Suggestions)
		}
	}
}
