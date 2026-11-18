package tools

import (
	"slices"
	"strings"
	"testing"
)

const sysUnit = `busctl call org.freedesktop.systemd1 /org/freedesktop/systemd1/unit/`

// Формы с приёмки: -C внутри значения чужой опции или после -- — не
// капсула; вычисляемое имя, связанное со средством; последний из --user и
// --system выбирает менеджер; алиасы целей выключения; kquitapp
// и closeSession KDE
func TestKillGuardSessionEndReviewBlocks(t *testing.T) {
	for _, command := range []string{
		`busctl --user --match -Cx call org.freedesktop.systemd1 /org/freedesktop/systemd1 org.freedesktop.systemd1.Manager StopUnit ss plasma-kwin_wayland.service replace`,
		`busctl --user --match=-Cx call org.freedesktop.systemd1 /org/freedesktop/systemd1 org.freedesktop.systemd1.Manager KillUnit ssi agent-x-1.scope all 15`,
		`busctl --match -C call org.freedesktop.login1 /org/freedesktop/login1 org.freedesktop.login1.Manager TerminateUser u 1000`,
		`busctl --user --match --capsule=x call org.freedesktop.systemd1 /org/freedesktop/systemd1 org.freedesktop.systemd1.Manager StartUnit ss exit.target replace`,
		`busctl --user --size -C call org.freedesktop.systemd1 /org/freedesktop/systemd1 org.freedesktop.systemd1.Manager StopUnit ss plasma-kwin_wayland.service replace`,
		`busctl --user -- call org.freedesktop.systemd1 /org/freedesktop/systemd1 org.freedesktop.systemd1.Manager SetUnitProperties sba"(sv)" agent-x-1.scope true 2 Description s -Cx MemoryMax t 1048576`,
		`busctl --user -- call org.freedesktop.systemd1 /org/freedesktop/systemd1 org.freedesktop.systemd1.Manager SetUnitProperties sba"(sv)" agent-x-1.scope true 2 Description s --capsule=x MemoryMax t 1048576`,
		sysMgr + `StopUnit ss plasma-kwin_wayland.service replace --user`,

		`export X=systemctl; $X --user stop plasma-kwin_wayland.service`,
		`X="sudo systemctl"; $X stop sddm.service`,
		`$(command -v systemctl) --user stop plasma-kwin_wayland.service`,
		"`which busctl` --user call org.freedesktop.systemd1 /org/freedesktop/systemd1 org.freedesktop.systemd1.Manager KillUnit ssi agent-x-1.scope all 15",

		`systemctl --user --system stop sddm.service`,
		`sudo systemctl --user --system stop user@1000.service`,
		`systemctl --global --user stop plasma-kwin_wayland.service`,
		`systemctl -M .host --user --system stop sddm.service`,

		`systemctl --user start systemd-exit.service`,
		`systemctl --user start systemd-exit`,
		`systemctl start systemd-exit.service`,
		`systemctl start ctrl-alt-del.target`,
		`systemctl start runlevel0.target`,
		`sudo systemctl start runlevel6.target`,
		`systemctl start systemd-poweroff.service`,
		`systemctl start systemd-reboot`,
		`systemctl start systemd-halt.service`,
		`systemctl start systemd-kexec.service`,
		`systemctl start systemd-soft-reboot.service`,
		userMgr + `StartUnit ss systemd-exit.service replace`,
		sysMgr + `StartUnit ss runlevel6.target replace`,
		sysMgr + `StartUnit ss systemd-poweroff.service replace`,
		userUnit + `systemd_2dexit_2eservice org.freedesktop.systemd1.Unit Start s replace`,
		sysUnit + `runlevel0_2etarget org.freedesktop.systemd1.Unit Start s replace`,

		`kquitapp6 plasmashell`,
		`kquitapp5 plasmashell`,
		`kquitapp6 kwin_wayland`,
		`kquitapp6 kwin`,
		`kquitapp6 ksmserver`,
		`kquitapp6 --service org.kde.plasmashell`,
		`kquitapp6 --service=org.kde.KWin`,
		`qdbus org.kde.ksmserver /KSMServer org.kde.KSMServerInterface.closeSession`,
		`qdbus org.kde.ksmserver /KSMServer closeSession`,
		`gdbus call --session --dest org.kde.ksmserver --object-path /KSMServer --method org.kde.KSMServerInterface.closeSession`,
	} {
		if validateKill(t, command).IsValid {
			t.Errorf("должно блокироваться: %s", command)
		}
	}
}

// Соседи: капсула в опциях busctl до операндов (и в связке -qC), вычисляемое
// имя без связи со средством, менеджер по последнему флагу, чтение алиасов
// целей, kquitapp прочих приложений
func TestKillGuardSessionEndReviewAllows(t *testing.T) {
	for _, command := range []string{
		`busctl --user -qC foo call org.freedesktop.systemd1 /org/freedesktop/systemd1 org.freedesktop.systemd1.Manager StopUnit ss plasma-kwin_wayland.service replace`,
		`busctl --match x -C foo call org.freedesktop.login1 /org/freedesktop/login1 org.freedesktop.login1.Manager TerminateUser u 1000`,

		`ls ~/.claude/skills; "$ADB" reboot`,
		`ls ~/.claude/skills; $VAGRANT halt`,
		`ls ~/.claude/skills; "$CLI" exit`,
		`ls ~/.claude/skills; $(which grep) -r stop ''`,
		`systemctl --user status x; "$GAME" default`,

		`systemctl --user --system stop plasma-kwin_wayland.service`,
		`systemctl --system --user stop sddm.service`,

		`systemctl status runlevel0.target`,
		`systemctl --user cat systemd-exit.service`,
		`systemctl --user start systemd-tmpfiles-clean.service`,

		`kquitapp6 dolphin`,
		`kquitapp6 --help`,
		`qdbus org.kde.ksmserver /KSMServer isShuttingDown`,
	} {
		if r := validateKill(t, command); !r.IsValid {
			t.Errorf("не должно блокироваться: %s: %v", command, r.Suggestions)
		}
	}
}

// Капсула вместе с транспортом busctl (--address, -M, -H) — проверка по
// шине; менеджер systemctl — только из опций до -- и не из значений опций
// (-p --global, --message --user), --global менеджер не меняет
func TestKillGuardSessionEndOptionBlocks(t *testing.T) {
	const kwin = `org.freedesktop.systemd1 /org/freedesktop/systemd1 org.freedesktop.systemd1.Manager StopUnit ss plasma-kwin_wayland.service replace`
	const terminate = `org.freedesktop.login1 /org/freedesktop/login1 org.freedesktop.login1.Manager TerminateUser u 1000`
	for _, command := range []string{
		`busctl --address=unix:path=/run/user/1000/bus -C foo call ` + kwin,
		`busctl -C foo --address unix:path=/run/user/1000/bus call org.freedesktop.systemd1 /org/freedesktop/systemd1 org.freedesktop.systemd1.Manager StartUnit ss exit.target replace`,
		`busctl -C foo --address=unix:path=/run/dbus/system_bus_socket call ` + terminate,
		`busctl -C foo -M .host call ` + terminate,
		`busctl --machine=.host --capsule foo call ` + terminate,
		`busctl -C foo -H localhost call ` + terminate,
		`busctl --capsule=foo -M aisee@.host --user call ` + kwin,
		`busctl -M aisee@.host -C foo --user call ` + kwin,
		`busctl --user -J -C call ` + kwin,

		`systemctl --user stop plasma-kwin_wayland.service -- --global`,
		`systemctl --user stop plasma-kwin_wayland.service --global`,
		`systemctl --user stop -- plasma-kwin_wayland.service --global`,
		`systemctl --user kill agent-x-1.scope -- --global`,
		`systemctl --user start exit.target -- --global`,
		`systemctl --user disable --now plasma-kwin_wayland.service --global`,
		`systemctl --user --global disable --now plasma-kwin_wayland.service`,
		`systemctl --user -p --global stop plasma-kwin_wayland.service`,
		`systemctl --user stop plasma-kwin_wayland.service -p --global`,
		`systemctl --user --message --global stop plasma-kwin_wayland.service`,
		`systemctl --user -P --global kill agent-x-1.scope`,
		`systemctl --user --property=x stop plasma-kwin_wayland.service --property --global`,
		`systemctl --user -p --system stop plasma-kwin_wayland.service`,
		`systemctl --user stop -- plasma-kwin_wayland.service --system`,
		`systemctl -p --user stop sddm.service`,
		`systemctl --user --system stop sddm.service`,
	} {
		if validateKill(t, command).IsValid {
			t.Errorf("должно блокироваться: %s", command)
		}
	}
}

// Причина называет форму и что погаснет; подсказка — что делать вместо
func TestKillGuardSessionEndReviewTexts(t *testing.T) {
	for _, tc := range []struct{ command, reason, hint string }{
		{`systemctl start runlevel0.target`, "systemctl start runlevel0.target выключает машину", systemctlEndHint("systemctl", "poweroff")},
		{`systemctl --user start systemd-exit.service`, "systemctl --user start systemd-exit.service гасит менеджер systemctl --user", systemctlEndHint("systemctl --user", "exit")},
		{`kquitapp6 plasmashell`, "kquitapp quit к org.kde.plasmashell гасит plasmashell", "Выход из сеанса запускает пользователь сам"},
		{`qdbus org.kde.ksmserver /KSMServer closeSession`, "qdbus closeSession к org.kde.ksmserver завершает сеанс KDE", "Выход из сеанса запускает пользователь сам"},
		{`export X=systemctl; $X --user stop plasma-kwin_wayland.service`, "имя команды вычисляется при запуске ($(…), `…` или $X), а с именем systemctl это отказ: systemctl --user stop plasma-kwin_wayland.service", "Останавливай юнит"},
	} {
		r := validateKill(t, tc.command)
		if r.IsValid || !strings.Contains(strings.Join(r.Suggestions, "\n"), tc.reason) || !slices.ContainsFunc(r.Suggestions, func(h string) bool { return strings.HasPrefix(h, tc.hint) }) {
			t.Errorf("%s: ждали отказ с «%s» и подсказкой «%s», получили %v %q", tc.command, tc.reason, tc.hint, r.IsValid, r.Suggestions)
		}
	}
}
