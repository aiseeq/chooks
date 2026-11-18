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

// Тикет kill-guard-systemd-run-hidden-cmd: команда под systemd-run, которую
// до запуска не видно (имя или скрипт sh -c из переменной или подстановки),
// — отказ, как юнит из переменной; -M на эту машину (.host, пусто, user@,
// user@.host) — проверка как без -M
func TestKillGuardSystemdRunHiddenBlocks(t *testing.T) {
	for _, command := range []string{
		`systemd-run --user sh -c "$CMD"`,
		`systemd-run --user "$X" 455274`,
		`systemd-run --user $X 455274`,
		`systemd-run --user ${X} 455274`,
		`systemd-run --user --scope bash -c "$(cat script)"`,
		"systemd-run --user sh -c `cat script`",
		`systemd-run --user sh -c 'sh -c "$CMD"'`,
		`systemd-run --user sh -c '$X 455274'`,
		`systemd-run --user env "$X" 455274`,
		`sudo systemd-run "$X" 455274`,
		`run0 systemd-run sh -c "$CMD"`,
		// эта машина: -M .host, пусто, user@, user@.host
		`systemd-run -M .host --user kill 455274`,
		`systemd-run --machine=.host kill 455274`,
		`systemd-run -M '' kill 455274`,
		`systemd-run -M aisee@ --user kill 455274`,
		`systemd-run --machine aisee@.host --user kill 455274`,
		`systemd-run -M "$M" kill 455274`,
		// systemd-run на другую машину и рядом здешний — проверка как здесь
		`systemd-run -H h true; systemd-run --user sh -c 'kill 455274'`,
		// транспорт задаёт последняя из -H, -M, -C (src/run/run.c)
		`systemd-run -H host -M .host kill 455274`,
		`systemd-run -M c1 -M .host kill 455274`,
		`systemd-run -M .host -M c1 -M .host kill 455274`,
		`systemd-run -H host -M '' kill 455274`,
		`systemd-run -H host --machine=aisee@.host --user kill 455274`,
		`systemd-run -H host --machine .host kill 455274`,
		`systemd-run --host=h -qM.host kill 455274`,
		`systemd-run -M c1 --machine=.host sh -c "$CMD"`,
		`systemd-run -H host -C cap kill 455274`,
		`systemd-run -H host --capsule=cap kill 455274`,
		// сокращение длинной опции getopt_long дополняет: --mach — --machine
		`systemd-run -H host --mach=.host kill 455274`,
		`systemd-run --mach=.host kill 455274`,
		// скрипт — одна подстановка
		`systemd-run --user sh -c "${CMD:-kill 455274}"`,
		`systemd-run --user sh -c ${CMD}`,
		`systemd-run --user bash -lc "$CMD"`,
		`systemd-run --user eval "$CMD"`,
		// подстановка на месте имени команды в скрипте — правило имени
		`systemd-run --user sh -c "make && $X"`,
		// оболочка читает скрипт из ввода, которого не видно
		`printf '%s\n' "$CMD" | systemd-run --user --pipe bash`,
		`cat "$F" | systemd-run --user --scope sh`,
		`echo "$CMD" | systemd-run --user -P sh`,
		`systemd-run --user --scope sh < "$F"`,
		`systemd-run --user --scope sh -s < "$F"`,
		`systemd-run --user --scope sh <<< "$CMD"`,
		"systemd-run --user --scope sh <<'EOF'\nkill 455274\nEOF",
	} {
		if validateKill(t, command).IsValid {
			t.Errorf("должно блокироваться: %s", command)
		}
	}
}

// systemd-run -H/--host и -M на чужую машину или контейнер исполнит команду
// не здесь — как под ssh; видимые имя и скрипт — пропуск
func TestKillGuardSystemdRunRemoteAllows(t *testing.T) {
	for _, command := range []string{
		`systemd-run -H host kill 455274`,
		`systemd-run --host=user@host kill 455274`,
		`systemd-run --host user@host --scope sh -c 'pkill opencode'`,
		`systemd-run -qH host podman stop x`,
		`systemd-run -M c1 kill 455274`,
		`systemd-run --machine=user@c1 --user pkill opencode`,
		`systemd-run -Mc1 kill 455274`,
		`nice systemd-run -M c1 kill 455274`,
		`run0 systemd-run -H host kill 455274`,
		`systemd-run -H host sh -c "$CMD"`,
		`systemd-run --user sh -c 'make -C "$DIR"'`,
		`systemd-run --user "$(go env GOPATH)/bin/tool" 1`,
		`systemd-run --user "$HOME/bin/tool" 1`,
		`systemd-run --user --scope -p MemoryMax=4G go test ./...`,
		// последняя из -H, -M, -C — удалённая
		`systemd-run -M .host -H host kill 455274`,
		`systemd-run -C cap -M c1 kill 455274`,
		`systemd-run --mach=.host --ho=h kill 455274`,
		// подстановка только в аргументах скрипта — команды видны
		`systemd-run --user sh -c "cd $DIR && make"`,
		`systemd-run --user sh -c "make -C $DIR"`,
		`systemd-run --user bash -c "go test ./... > $LOG 2>&1"`,
		`systemd-run --user sh -c "echo $HOME"`,
		`systemd-run --user sh -c "make -j$(nproc)"`,
		`DIR=/x; systemd-run --user sh -c "cd $DIR && make"`,
		`systemd-run --user sh -c "$HOME/bin/tool"`,
		`systemd-run --user -E DIR="$DIR" sh -c 'make -C "$DIR"'`,
		`systemd-run --user --working-directory="$DIR" make`,
		// ввод оболочки виден или это файл скрипта (вне проверки)
		"systemd-run --user --scope sh <<'EOF'\nmake\nEOF",
		`echo 'make' | systemd-run --user -P sh`,
		`systemd-run --user --scope sh < build.sh`,
		`systemd-run --user bash script.sh`,
		`systemd-run --user -t bash`,
	} {
		if r := validateKill(t, command); !r.IsValid {
			t.Errorf("не должно блокироваться: %s: %v", command, r.Suggestions)
		}
	}
}

// Правка elevated (Via с обёрткой у всех вызовов за run0): присваивание
// после sudo -- за run0 узнаётся, как без run0 — программа за ним
// проверяется своим вызовом. Было отказом из-за вызова-присваивания, стало
// как у sudo -- PATH=$PATH kill 455274 без run0
func TestKillGuardElevatedSudoAssignment(t *testing.T) {
	for _, command := range []string{
		// под run0 kill — отказ по другой причине (kill-guard-run0-signal):
		// присваивание проверяется на программе без сигнала, но со словом
		// kill — иначе скан не идёт
		`run0 sudo -- PATH=$PATH grep kill /var/log/messages`,
		`sudo -- PATH=$PATH kill 455274`,
	} {
		if r := validateKill(t, command); !r.IsValid {
			t.Errorf("не должно блокироваться: %s: %v", command, r.Suggestions)
		}
	}
	for _, command := range []string{
		`run0 sudo -- PATH=$PATH kill 1`,
		`run0 sudo -- PATH=$PATH systemd-run kill 455274`,
	} {
		if validateKill(t, command).IsValid {
			t.Errorf("должно блокироваться: %s", command)
		}
	}
}

func TestKillGuardSystemdRunHiddenTexts(t *testing.T) {
	for _, tc := range []struct{ command, reason string }{
		{`systemd-run --user "$X" 455274`, "systemd-run запускает команду, имя которой вычисляется при запуске"},
		{`systemd-run --user sh -c "$CMD"`, "systemd-run запускает sh со скриптом из одной подстановки"},
		{`cat "$F" | systemd-run --user --scope sh`, "systemd-run запускает sh, который читает скрипт из ввода"},
		{`systemd-run --user --scope sh < "$F"`, "systemd-run запускает sh со скриптом на вводе из < \"$F\""},
	} {
		r := validateKill(t, tc.command)
		if r.IsValid || !strings.Contains(strings.Join(r.Suggestions, "\n"), tc.reason) || !slices.Contains(r.Suggestions, systemdRunHiddenHint) {
			t.Errorf("%s: ждали отказ с «%s» и подсказкой, получили %v %q", tc.command, tc.reason, r.IsValid, r.Suggestions)
		}
	}
}

// Третий круг приёмки: скрипт с подстановкой, где разбор не видит ни одной
// команды; пайп в оболочку за run0 (разбор за обёрткой ввода не знает);
// оболочка читает ввод, хотя в аргументах есть слово (-s -- a b, /dev/stdin)
func TestKillGuardSystemdRunHiddenRound3(t *testing.T) {
	// ввод run0 забирает xargs, а не sh -c: прежняя причина, не «читает из ввода»
	r := validateKill(t, `echo 1 | run0 xargs sh -c 'kill "$@"' _`)
	if r.IsValid || strings.Contains(strings.Join(r.Suggestions, "\n"), "читает скрипт из ввода") {
		t.Errorf("echo 1 | run0 xargs sh -c 'kill \"$@\"' _: ждали отказ без «читает скрипт из ввода», получили %v %q", r.IsValid, r.Suggestions)
	}
	for _, command := range []string{
		`systemd-run --user sh -c "$CMD"' 455274'`,
		`systemd-run --user sh -c "$CMD"x`,
		`systemd-run --user sh -c "$A$B"`,
		// имя make$X — тоже вычисляется
		`systemd-run --user sh -c 'make'"$X"`,
		`printf '%s\n' "$CMD" | systemd-run --user --pipe run0 bash`,
		`printf '%s\n' "$CMD" | run0 systemd-run --pipe bash`,
		`printf '%s\n' "$CMD" | run0 run0 systemd-run --pipe bash`,
		`printf '%s\n' "$CMD" | systemd-run --user --pipe bash -s -- a b`,
		`printf '%s\n' "$CMD" | systemd-run --user --pipe bash -s`,
		`printf '%s\n' "$CMD" | systemd-run --user --pipe bash -`,
		`printf '%s\n' "$CMD" | systemd-run --user --pipe bash /dev/stdin`,
		`printf '%s\n' "$CMD" | systemd-run --user --pipe bash /dev/fd/0`,
		`printf '%s\n' "$CMD" | systemd-run --user --pipe bash -- /proc/self/fd/0`,
	} {
		if validateKill(t, command).IsValid {
			t.Errorf("должно блокироваться: %s", command)
		}
	}
	for _, command := range []string{
		`systemd-run --user sh -c "cd $DIR && make"`,
		`systemd-run --user sh -c "make $TARGET"`,
		`systemd-run --user sh -c "make -C \"$DIR\" && echo $(date)"`,
		`systemd-run --user sh -c "go test ./... -run '$RE'"`,
		`systemd-run --user eval "make -C $DIR"`,
		`systemd-run --user sh -c "$HOME/bin/tool"`,
		`systemd-run --user bash -- script.sh a b`,
		`systemd-run --user bash -x script.sh`,
		`printf '%s\n' "$CMD" | systemd-run --user --pipe cat`,
		`printf '%s\n' "$CMD" | run0 cat`,
		`run0 systemd-run --user sleep 1`,
		`echo make | systemd-run --user --pipe sh`,
		`echo 1 | run0 xargs sh -c 'echo "$@"' _`,
	} {
		if r := validateKill(t, command); !r.IsValid {
			t.Errorf("не должно блокироваться: %s: %v", command, r.Suggestions)
		}
	}
}

// Хвост: ввод наследуется через sh -c и run0 (пайп снаружи скрипта и
// внутри), пути к вводу в других написаниях, литеральный файл через cat —
// как sh < файл
func TestKillGuardSystemdRunStdinTail(t *testing.T) {
	for _, command := range []string{
		`printf '%s\n' "$CMD" | sh -c 'systemd-run --pipe bash'`,
		`printf '%s\n' "$CMD" | sh -c "systemd-run --pipe bash"`,
		`printf '%s\n' "$CMD" | run0 sh -c 'systemd-run --pipe bash'`,
		`printf '%s\n' "$CMD" | bash -ec 'cd /tmp; systemd-run --user --pipe sh'`,
		`printf '%s\n' "$CMD" | sh -c 'sh -c "systemd-run --pipe bash"'`,
		`sh -c 'printf "%s\n" "$CMD" | run0 systemd-run --pipe bash'`,
		`sh -c 'printf "%s\n" "$CMD" | systemd-run --pipe bash'`,
		`run0 sh -c 'printf "%s\n" "$CMD" | systemd-run --pipe bash'`,
		`printf '%s\n' "$CMD" | systemd-run --user --pipe bash /proc/$$/fd/0`,
		`printf '%s\n' "$CMD" | systemd-run --user --pipe bash /dev/fd/./0`,
		`printf '%s\n' "$CMD" | systemd-run --user --pipe bash //dev/stdin`,
		`printf '%s\n' "$CMD" | systemd-run --user --pipe bash "/proc/$BASHPID/fd/0"`,
		`printf '%s\n' "$CMD" | systemd-run --user --pipe bash /proc/thread-self/fd/0`,
		// cat не литерального файла, cat с вводом — текст не виден
		`cat "$F" | systemd-run --user --pipe bash`,
		`cat - | systemd-run --user --pipe bash`,
		`cat /dev/stdin | systemd-run --user --pipe bash`,
		`cat -A script.sh | systemd-run --user --pipe bash`,
		`cat < "$F" | systemd-run --user --pipe bash`,
		`printf '%s\n' "$CMD" | cat script.sh - | systemd-run --user --pipe bash`,
		// строки ввода станут аргументами bash (-c, kill 1)
		`printf '%s\n' "$CMD" | xargs systemd-run --user --pipe bash`,
		// копия дескриптора — не перенаправление из файла
		`exec 3< "$F"; systemd-run --user --scope sh <&3`,
		`systemd-run --user --scope sh 0<&3`,
		`sh -c 'systemd-run --user --scope sh <&3' 3< "$F"`,
		`printf x | sh -c 'systemd-run --user --scope sh' 0<&3`,
	} {
		if validateKill(t, command).IsValid {
			t.Errorf("должно блокироваться: %s", command)
		}
	}
	for _, command := range []string{
		`cat script.sh | systemd-run --user --pipe bash`,
		`cat a.sh b.sh | systemd-run --user --pipe sh`,
		`cat ./build.sh | run0 systemd-run --pipe bash`,
		`cat build.sh | sh -c 'systemd-run --user --pipe bash'`,
		`systemd-run --user --scope sh < script.sh`,
		// ввод sh внутри скрипта — свой, литеральный
		`printf '%s\n' "$CMD" | sh -c 'systemd-run --user --pipe bash < build.sh'`,
		`printf '%s\n' "$CMD" | sh -c 'echo make | systemd-run --user --pipe bash'`,
		// тело из пайпа в скрипте разбор читает (Via sh -c pipe systemd-run bash)
		`sh -c 'echo make | systemd-run --user --pipe bash'`,
		// без пайпа ввод — агента
		`sh -c 'systemd-run --user --pipe bash build.sh'`,
		`systemd-run --user --scope bash`,
		`printf '%s\n' "$CMD" | systemd-run --user --pipe bash /proc/1/fd/0x`,
	} {
		if r := validateKill(t, command); !r.IsValid {
			t.Errorf("не должно блокироваться: %s: %v", command, r.Suggestions)
		}
	}
}

// Приёмка хвоста: дескриптор, маска и |& у cat — не файл; за xargs ввод из
// пайпа — отказ при любом источнике; литеральный ввод проходит сквозь sh -c
// и run0 так же, как напрямую, а kill в нём виден
func TestKillGuardSystemdRunStdinTailReview(t *testing.T) {
	for _, command := range []string{
		`exec 3< "$F"; cat /dev/fd/3 | systemd-run --user --pipe bash`,
		`cat /proc/self/fd/3 | systemd-run --user --pipe bash`,
		`cat /proc/$$/fd/7 | systemd-run --user --pipe bash`,
		`cat /dev/fd/./3 | systemd-run --user --pipe bash`,
		`cat a.sh /dev/fd/3 | run0 systemd-run --pipe bash`,
		`cat *.sh | systemd-run --user --pipe bash`,
		`cat a.sh |& systemd-run --user --pipe bash`,
		`cat a.sh | xargs systemd-run --pipe bash`,
		`echo make | xargs systemd-run --pipe bash`,
		`cat a.sh | sh -c 'xargs systemd-run --pipe bash'`,
		`echo 'kill 455274' | sh -c 'systemd-run --pipe bash'`,
		`sh -c 'echo "kill 455274" | run0 systemd-run --pipe bash'`,
		`echo 'make; pkill opencode' | run0 sh -c 'systemd-run --pipe bash'`,
		`echo "$CMD" | sh -c 'systemd-run --pipe bash'`,
		`echo '' | sh -c 'systemd-run --pipe bash'`,
	} {
		if validateKill(t, command).IsValid {
			t.Errorf("должно блокироваться: %s", command)
		}
	}
	for _, command := range []string{
		`echo make | sh -c 'systemd-run --pipe bash'`,
		`sh -c 'echo make | run0 systemd-run --pipe bash'`,
		`echo make | systemd-run --pipe bash`,
		`echo make | run0 systemd-run --pipe bash`,
		`echo make | run0 sh -c 'systemd-run --pipe bash'`,
		`printf '%s\n' 'make test' | sh -c 'cd /tmp && systemd-run --user --pipe sh'`,
		`cat 'my*.sh' | systemd-run --user --pipe bash`,
		`cat a.sh | systemd-run --user --pipe bash`,
	} {
		if r := validateKill(t, command); !r.IsValid {
			t.Errorf("не должно блокироваться: %s: %v", command, r.Suggestions)
		}
	}
}

// Остаток хвоста: parallel с вводом из пайпа — как xargs; ввод sh -c (и run0
// sh -c) из перенаправления — как у оболочки прямо под systemd-run:
// подстановка — отказ, литерал — пропуск, а текст <<< проверяется
func TestKillGuardSystemdRunStdinTail2(t *testing.T) {
	for _, command := range []string{
		`cat a.sh | parallel systemd-run --pipe bash`,
		`echo make | parallel systemd-run --user --pipe sh`,
		`cat a.sh | parallel -j2 systemd-run --user --pipe bash`,
		`sh -c 'systemd-run --pipe bash' < "$F"`,
		`sh -c 'systemd-run --pipe bash' < "$DIR/a.sh"`,
		`sh -c 'systemd-run --pipe bash' <<< "$CMD"`,
		`sh -c 'systemd-run --pipe bash' <<< $(cat f)`,
		`run0 sh -c 'systemd-run --pipe bash' < "$F"`,
		`run0 sh -c 'systemd-run --pipe bash' <<< "$CMD"`,
		`echo make | sh -c 'systemd-run --pipe bash' < "$F"`,
		`echo make | run0 sh -c 'systemd-run --pipe bash' < "$F"`,
		`sh -c 'true; systemd-run --user --pipe bash' 0< "$F"`,
		`sh -c 'systemd-run --pipe bash' <<< 'kill 455274'`,
		`run0 sh -c 'systemd-run --pipe bash' <<< 'pkill opencode'`,
		`sh -c 'systemd-run --pipe bash' <<< "kill $PID"`,
	} {
		if validateKill(t, command).IsValid {
			t.Errorf("должно блокироваться: %s", command)
		}
	}
	for _, command := range []string{
		`sh -c 'systemd-run --pipe bash' < script.sh`,
		`sh -c 'systemd-run --pipe bash' <<< make`,
		`sh -c 'systemd-run --pipe bash' <<< 'make test'`,
		`sh -c 'systemd-run --pipe bash' <<< "make $TARGET"`,
		`run0 sh -c 'systemd-run --pipe bash' < script.sh`,
		`run0 sh -c 'systemd-run --pipe bash' <<< make`,
		`printf '%s\n' "$CMD" | sh -c 'systemd-run --pipe bash' < script.sh`,
		`sh -c 'systemd-run --pipe bash' 2< "$F"`,
		`seq 3 | parallel systemd-run --user sleep`,
		`parallel systemd-run --user --pipe bash ::: a.sh b.sh`,
		`cat a.sh | parallel systemd-run -H host --pipe bash`,
		`cat list | parallel systemd-run --user bash build.sh`,
	} {
		if r := validateKill(t, command); !r.IsValid {
			t.Errorf("не должно блокироваться: %s: %v", command, r.Suggestions)
		}
	}
}

// parallel по приёмке: команда — слова, склеенные в строку для оболочки,
// оболочка ищется на любой глубине под systemd-run; --pipe — как прямой
// пайп; -S host — другая машина; имя из ввода ({}, {1}) — отказ
func TestKillGuardSystemdRunParallel(t *testing.T) {
	for _, command := range []string{
		`cat a.sh | parallel 'systemd-run --pipe bash'`,
		`cat a.sh | parallel "systemd-run --user --pipe bash"`,
		`cat a.sh | parallel systemd-run --pipe nice bash`,
		`cat a.sh | parallel systemd-run --pipe run0 bash`,
		`cat a.sh | parallel /usr/bin/systemd-run --pipe bash`,
		`cat a.sh | parallel nice systemd-run --pipe bash`,
		`cat a.sh | parallel systemd-run --user bash -c`,
		`cat a.sh | parallel -q systemd-run --pipe bash`,
		`cat a.sh | parallel --sshlogin : systemd-run --pipe bash`,
		`cat a.sh | parallel -S :,host systemd-run --pipe bash`,
		`printf '%s\n' "$CMD" | parallel --pipe systemd-run --pipe bash`,
		`echo 'kill 455274' | parallel --pipe systemd-run --pipe bash`,
		`cat "$F" | parallel --pipe systemd-run --user --pipe sh`,
		`cat a.sh | parallel systemd-run --user {}`,
		`cat a.sh | parallel systemd-run --user "{}"`,
		`cat a.sh | parallel --colsep ' ' systemd-run --user {1} {2}`,
		`cat a.sh | parallel -I XX systemd-run --user XX`,
		`cat a.sh | parallel systemd-run --user {.} arg`,
		// и на main: shellcmd снимает parallel с ::: и отдаёт имя {}
		`parallel systemd-run --user {} ::: make`,
	} {
		if validateKill(t, command).IsValid {
			t.Errorf("должно блокироваться: %s", command)
		}
	}
	for _, command := range []string{
		`echo make | parallel --pipe systemd-run --pipe bash`,
		`cat a.sh | parallel --pipe systemd-run --pipe bash`,
		`cat a.sh | parallel -S host systemd-run --pipe bash`,
		`cat a.sh | parallel -H host systemd-run --pipe bash`,
		`cat a.sh | parallel --sshlogin host1,host2 systemd-run --pipe bash`,
		`cat a.sh | parallel --sshlogin=host systemd-run --pipe bash`,
		`cat a.sh | parallel systemd-run -M c1 --pipe bash`,
		`parallel systemd-run --user make ::: a b`,
		`parallel -a list.txt systemd-run --user bash`,
		`cat a.sh | parallel systemd-run --user make`,
		`cat a.sh | parallel systemd-run --user bash build.sh`,
		`cat a.sh | parallel systemd-run --user bash -c 'make {}'`,
		`cat a.sh | parallel echo systemd-run`,
		`cat hosts | parallel ssh {} uptime`,
		`seq 3 | parallel -j2 go test ./pkg/{}`,
	} {
		if r := validateKill(t, command); !r.IsValid {
			t.Errorf("не должно блокироваться: %s: %v", command, r.Suggestions)
		}
	}
}

// parallel, последний круг приёмки: run0/pkexec перед systemd-run, машина
// «:» в записи -S с префиксом и списки машин, которых не видно; строки
// замены в длинной форме; опции со значением — по таблице parallel
// 20260922 (Getopt::Long с bundling и require_order)
func TestKillGuardSystemdRunParallelOptions(t *testing.T) {
	for _, command := range []string{
		`cat a.sh | parallel 'run0 systemd-run --pipe bash'`,
		`cat a.sh | parallel run0 systemd-run --pipe bash`,
		`cat a.sh | parallel pkexec systemd-run --pipe bash`,
		`cat a.sh | parallel run0 systemd-run --user {}`,
		`cat a.sh | parallel 'run0 systemd-run --user kill 455274'`,
		`cat a.sh | parallel -S 4/: systemd-run --pipe bash`,
		`cat a.sh | parallel --sshlogin=4/: systemd-run --pipe bash`,
		`cat a.sh | parallel -S host,4/: systemd-run --pipe bash`,
		`cat a.sh | parallel -S @grp/: systemd-run --pipe bash`,
		`cat a.sh | parallel -S @grp/4/: systemd-run --pipe bash`,
		"cat a.sh | parallel -S 'host\n:' systemd-run --pipe bash",
		`cat a.sh | parallel -S 'ssh -p 2222 :' systemd-run --pipe bash`,
		`cat a.sh | parallel -S 4/: systemd-run --user {}`,
		`cat a.sh | parallel -S "$H" systemd-run --pipe bash`,
		`cat a.sh | parallel -S - systemd-run --pipe bash`,
		`cat a.sh | parallel -S .. systemd-run --pipe bash`,
		`cat a.sh | parallel --replace=XX systemd-run --user XX`,
		`cat a.sh | parallel --er XX systemd-run --user XX`,
		`cat a.sh | parallel --extensionreplace XX systemd-run --user XX`,
		`cat a.sh | parallel --bnr XX systemd-run --user XX`,
		`cat a.sh | parallel --rpl 'XX s/x//' systemd-run --user XX`,
		`cat a.sh | parallel --id x systemd-run --pipe bash`,
		`cat a.sh | parallel --semaphorename x systemd-run --pipe bash`,
		`cat a.sh | parallel --match x systemd-run --pipe bash`,
		`cat a.sh | parallel --filter x systemd-run --pipe bash`,
		`cat a.sh | parallel --trc x systemd-run --pipe bash`,
		`cat a.sh | parallel --sqlmaster x systemd-run --pipe bash`,
		`cat a.sh | parallel --ctagstring x systemd-run --pipe bash`,
		`cat a.sh | parallel -e x systemd-run --pipe bash`,
		`cat a.sh | parallel --max-replace-args 1 systemd-run --pipe bash`,
		`cat a.sh | parallel --id x systemd-run --user {}`,
		`cat a.sh | parallel --max-replace-args 1 systemd-run --user {}`,
		`cat a.sh | parallel -kj4 systemd-run --pipe bash`,
		`cat a.sh | parallel --sshd 0.5 systemd-run --pipe bash`,
	} {
		if validateKill(t, command).IsValid {
			t.Errorf("должно блокироваться: %s", command)
		}
	}
	for _, command := range []string{
		`cat a.sh | parallel -S host -S h2 systemd-run --user {}`,
		`cat a.sh | parallel -S 'ssh -p 2222 host' systemd-run --pipe bash`,
		`cat a.sh | parallel -S u@host:22 systemd-run --pipe bash`,
		`cat a.sh | parallel --slf hosts systemd-run --pipe bash`,
		`ls | parallel --id x make -C {}`,
		`ls | parallel --replace=XX cp XX XX.bak`,
		`echo 1 | parallel 'run0 kill 455274'`,
		// -H у parallel — целое (H=i, снята): -H host — ошибка разбора, parallel
		// выходит; --sshl неоднозначно (sshlogin, sshloginfile) — тоже выход
		`cat a.sh | parallel -H host systemd-run --pipe bash`,
		`cat a.sh | parallel --sshl host systemd-run --pipe bash`,
		// --hgrp — флаг (hgrp|hostgroups): команда — x, systemd-run её аргумент
		`cat a.sh | parallel --hgrp x systemd-run --pipe bash`,
	} {
		if r := validateKill(t, command); !r.IsValid {
			t.Errorf("не должно блокироваться: %s: %v", command, r.Suggestions)
		}
	}
}
