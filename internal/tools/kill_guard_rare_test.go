package tools

import (
	"slices"
	"strings"
	"testing"
)

// cgUser cgroup менеджера пользователя в дереве теста
const cgUser = `/sys/fs/cgroup/user.slice/user-1000.slice/user@1000.service`

// Редкие формы, гасящие сессию: запись в cgroupfs сессии и её срезов,
// systemd-run с SuccessAction/FailureAction, machinectl shell и run0
// --via-shell на этой машине, KDE-получатель из переменной или :1.N, quit
// у Konsole, rescue и emergency, systemctl -C с транспортом или poweroff
func TestKillGuardRareBlocks(t *testing.T) {
	for _, command := range []string{
		`echo 1 > ` + cgUser + `/agents.slice/agent-x-1.scope/cgroup.kill`,
		`echo 1 >> ` + cgUser + `/agents.slice/cgroup.freeze`,
		`echo 1M | tee ` + cgUser + `/session.slice/plasma-kwin_wayland.service/memory.max`,
		`echo 10 | sudo tee -a /sys/fs/cgroup/user.slice/user-1000.slice/pids.max`,
		`sh -c 'echo "50000 100000" > /sys/fs/cgroup/user.slice/cpu.max'`,
		`cp /tmp/one ` + cgUser + `/memory.high`,
		`dd if=/tmp/one of=` + cgUser + `/agents.slice/agent-x-1.scope/cgroup.kill`,
		`printf 1 > /sys/fs/cgroup/system.slice/sddm.service/cgroup.kill`,
		`echo 1 > ` + cgUser + `/agents.slice/agent-x-1.scope/../cgroup.kill`,

		`systemd-run --user -p SuccessAction=exit-force true`,
		`systemd-run --user --property=FailureAction=exit false`,
		`systemd-run --user --property FailureAction=exit-force false`,
		`systemd-run -p SuccessAction=poweroff-force true`,
		`sudo systemd-run -pSuccessAction=reboot true`,
		`systemd-run --user --on-active=30 -p SuccessAction=exit true`,
		`systemd-run --user -q --timer-property=SuccessAction=exit --on-active=1 true`,
		`systemd-run -p SuccessAction=soft-reboot-force true`,

		`machinectl shell root@ /usr/bin/systemctl stop sddm.service`,
		`machinectl shell .host /usr/bin/loginctl terminate-user aisee`,
		`machinectl shell aisee@.host /usr/bin/systemctl --user stop plasma-kwin_wayland.service`,
		`machinectl -q shell root@ /usr/bin/kill -9 -1`,
		`machinectl --setenv X=1 shell root@.host /usr/bin/systemctl poweroff`,

		`run0 --via-shell 'systemctl stop sddm.service'`,
		`run0 -i 'loginctl terminate-user aisee'`,
		`run0 --via-shell systemctl stop sddm.service`,

		`qdbus "$S" /Shutdown logout`,
		`qdbus :1.42 /KSMServer logout 0 0 0`,
		`qdbus $SVC /MainApplication quit`,
		`gdbus call --session --dest :1.42 --object-path /MainApplication --method org.qtproject.Qt.QCoreApplication.quit`,
		`qdbus org.kde.konsole /MainApplication quit`,
		`qdbus org.kde.konsole-7438 /MainApplication quit`,
		`dbus-send --session --dest=org.kde.konsole-7438 /MainApplication org.qtproject.Qt.QCoreApplication.quit`,
		`kquitapp6 konsole`,

		`systemctl start rescue.target`,
		`systemctl start rescue`,
		`sudo systemctl start emergency.service`,
		`systemctl start runlevel1.target`,
		`systemctl isolate emergency.target`,
		sysMgr + `StartUnit ss rescue.target replace`,

		`systemctl -C foo -M .host --user stop plasma-kwin_wayland.service`,
		`systemctl -C foo poweroff`,

		`$(npm bin)/kill -9 -1`,
	} {
		if validateKill(t, command).IsValid {
			t.Errorf("должно блокироваться: %s", command)
		}
	}
}

// Соседи: cgroup без процессов сессии, чтение cgroupfs, запись на другой
// машине; systemd-run без действия выключения или с ним в аргументах
// команды; machinectl к контейнеру и без разрушающей команды; KDE-вызовы
// без выхода; systemctl -C (капсула) с юнитом; путь с подстановкой каталога
// и литеральной программой ($(npm bin)/pm2 kill)
func TestKillGuardRareAllows(t *testing.T) {
	for _, command := range []string{
		`echo 1 > ` + cgUser + `/opencode.service/cgroup.kill`,
		`echo 1M > ` + cgUser + `/background.slice/run-sleep.scope/memory.max`,
		`cat ` + cgUser + `/agents.slice/agent-x-1.scope/memory.max`,
		`cat ` + cgUser + `/agents.slice/agent-x-1.scope/cgroup.procs > /tmp/procs`,
		`ssh host 'echo 1 > /sys/fs/cgroup/user.slice/cgroup.kill'`,

		`systemd-run --user -p MemoryMax=1G make`,
		`systemd-run --user -p SuccessAction=none true`,
		`systemd-run --user true -p SuccessAction=exit`,
		`systemd-run --user --unit=x sleep 10`,

		`machinectl shell mycontainer /usr/bin/systemctl stop sddm.service`,
		`machinectl shell root@ /usr/bin/systemctl status sddm.service`,
		`machinectl list`,

		`run0 --via-shell 'systemctl status sddm.service'`,

		`qdbus "$S" /MainApplication applicationName`,
		`qdbus org.kde.konsole-7438 /Sessions/1 title 0`,
		`qdbus :1.42 /org/foo ping`,

		`systemctl status rescue.target`,
		`systemctl --user start rescue.target`,

		`systemctl -C foo --user stop plasma-kwin_wayland.service`,
		`systemctl --capsule=foo stop plasma-kwin_wayland.service`,

		`cd /x && $(npm bin)/pm2 kill`,
		`"$(npm bin)/pm2" kill`,
		"`npm bin`/pm2 kill",
	} {
		if r := validateKill(t, command); !r.IsValid {
			t.Errorf("не должно блокироваться: %s: %v", command, r.Suggestions)
		}
	}
}

// Причина называет форму и что погаснет; подсказка — что делать вместо
func TestKillGuardRareTexts(t *testing.T) {
	for _, tc := range []struct{ command, reason, hint string }{
		{`echo 1 > ` + cgUser + `/agents.slice/agent-x-1.scope/cgroup.kill`, "cgroup.kill убивает все процессы cgroup и вложенных: в ней идёт claude (PID 2100)", "Cgroup сессии"},
		{`systemd-run --user -p SuccessAction=exit-force true`, "systemd-run --user -p SuccessAction=exit-force: когда служба завершится, менеджер выполнит exit-force — гасит менеджер systemctl --user", systemctlEndHint("systemctl --user", "exit")},
		{`machinectl shell root@ /usr/bin/systemctl stop sddm.service`, "systemctl stop sddm.service: в юните идёт sddm", "Останавливай юнит"},
		{`qdbus "$S" /Shutdown logout`, `qdbus logout: получатель "$S" не виден до запуска`, kdeHint},
		{`qdbus org.kde.konsole /MainApplication quit`, "qdbus quit к org.kde.konsole закрывает Konsole", kdeHint},
		{`systemctl start rescue.target`, "systemctl start rescue.target переключает цель системы", systemctlEndHint("systemctl", "rescue")},
	} {
		r := validateKill(t, tc.command)
		if r.IsValid || !strings.Contains(strings.Join(r.Suggestions, "\n"), tc.reason) || !slices.ContainsFunc(r.Suggestions, func(h string) bool { return strings.HasPrefix(h, tc.hint) }) {
			t.Errorf("%s: ждали отказ с «%s» и подсказкой «%s», получили %v %q", tc.command, tc.reason, tc.hint, r.IsValid, r.Suggestions)
		}
	}
}

// Запись, снимающая ограничение cgroup, — пропуск, если значение видно: max
// у лимитов, 0 у cgroup.freeze; cgroup.kill и невидимое значение — отказ.
// Quit и Exit у получателя из переменной или :1.N — отказ только у пути
// или интерфейса KDE и Qt (или пути, которого не видно)
func TestKillGuardRareRelease(t *testing.T) {
	const scope = cgUser + `/agents.slice/agent-x-1.scope`
	for _, tc := range []struct {
		command string
		allow   bool
	}{
		{`echo max > ` + scope + `/memory.max`, true},
		{`echo -n max > ` + scope + `/memory.high`, true},
		{`printf 'max\n' > ` + scope + `/pids.max`, true},
		{`echo "max 100000" > ` + scope + `/cpu.max`, true},
		{`echo max > ` + scope + `/cpu.max`, true},
		{`echo 0 > ` + scope + `/cgroup.freeze`, true},
		{`echo 0 > ` + scope + `/cgroup.kill`, false},
		{`echo max > ` + scope + `/cgroup.kill`, false},
		{`echo 1 > ` + scope + `/cgroup.freeze`, false},
		{`echo 0 > ` + scope + `/memory.max`, false},
		{`echo "$V" > ` + scope + `/memory.max`, false},
		{`echo max | tee ` + scope + `/memory.max`, false},
		{`cp /tmp/max ` + scope + `/memory.max`, false},
		{`echo "max 1 2" > ` + scope + `/cpu.max`, false},

		{`busctl --user call "$BUS_NAME" /org/example org.example.App Quit`, true},
		{`gdbus call --session --dest "$NAME" --object-path /org/example/App --method org.example.App.Exit`, true},
		{`busctl --user call :1.42 /org/example org.example.App Quit`, true},
		{`qdbus "$SVC" /MainApplication quit`, false},
		{`qdbus :1.42 /MainApplication quit`, false},
		{`busctl --user call "$B" /org/example org.qtproject.Qt.QCoreApplication quit`, false},
		{`busctl --user call "$B" /org/example org.kde.Foo Exit`, false},
		{`qdbus :1.42 /org/kde/app quit`, false},
		{`gdbus call --session --dest :1.42 --object-path "$P" --method org.example.App.Exit`, false},
		{`qdbus "$S" /org/example logout`, false},
	} {
		if r := validateKill(t, tc.command); r.IsValid != tc.allow {
			t.Errorf("%s: ждали пропуск %v, получили %v %v", tc.command, tc.allow, r.IsValid, r.Suggestions)
		}
	}
}
