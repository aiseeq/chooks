package tools

import (
	"strings"
	"testing"
)

// Правило глубины: команда и одна обёртка над ней проверяются как прямые;
// сигнал или остановка под двумя и более обёртками, а также имя или
// скрипт, пришедшие под второй обёрткой не литералом, — отказ с подсказкой
// запустить напрямую
func TestKillGuardDepthBlocks(t *testing.T) {
	for _, command := range []string{
		// сигнал на двух уровнях обёрток
		`timeout 5 sudo -- A=$a B=$b kill 455274`,
		`timeout 5 sudo -u root -- X=a/b kill -TERM 455274`,
		`sudo -u x env A=1 kill 455274`,
		`nohup setsid kill 455274`,
		`eval eval eval eval kill 455274`,
		`timeout 5 sudo bash -euo pipefail -c 'kill 455274'`,
		`sudo -u x timeout 5 systemctl stop nginx.service`,
		`setsid -w nohup ksh -c 'kill -0 -1'`,
		`timeout 5 sudo loginctl terminate-user aisee`,
		`sudo -u x env A=1 tmux kill-server`,
		`systemd-run --user sem 'kill 455274'`,
		`systemd-run --user --pipe parallel :::: - <<< 'kill 455274'`,
		`sudo parallel kill ::: 455274`,
		// имя команды вычисляется при запуске под двумя обёртками
		`timeout 5 sudo "$CMD" 455274; kill -l`,
		// имя команды из источников parallel под второй обёрткой
		`systemd-run --user --pipe parallel ::: echo :::: - <<< 'done'`,
		`systemd-run --user parallel :::+ 'echo safe'`,
		`systemd-run --user parallel -a units.txt`,
		`printf '%s\n' 'ExecStartPre=/usr/bin/kill 1' | parallel systemd-run --user -p {}`,
		// сигнал в словах или вводе сложной формы parallel
		`parallel -a - ::: -9 ::: -1 <<< kill`,
		`parallel --colsep , -a - <<< 'kill,-9,-1'`,
		`parallel 'systemctl --user stop {}' ::: foo`,
		`echo 'kill 1' | parallel 'sudo bash -c'`,
	} {
		t.Run(command, func(t *testing.T) {
			r := validateKill(t, command)
			if r.IsValid {
				t.Fatalf("должно блокироваться: %s", command)
			}
			if !strings.Contains(strings.Join(r.Suggestions, "\n"), "Запусти напрямую") {
				t.Errorf("нет подсказки правила глубины: %s: %v", command, r.Suggestions)
			}
		})
	}
}

func TestKillGuardDepthAllows(t *testing.T) {
	for _, command := range []string{
		// безвредная команда на двух уровнях
		`timeout 60 bash -c 'go test ./...'; kill -l`,
		`sudo -u x env A=1 make; kill -l`,
		`timeout 5 bash -c 'while kill -0 455274; do sleep 1; done'`,
		`sudo -u x env kill -l`,
		`systemd-run --user nice make`,
		// один уровень: проверка как у прямой команды
		`systemd-run --user make`,
		`parallel echo ::: a b; kill 455274`,
		`sudo kill 455274`,
		`bash -c 'kill 455274'`,
		`timeout 5 kill 455274`,
		`parallel kill ::: 455274`,
		`parallel :::: - <<< 'kill 455274'`,
		`echo make | parallel 'pkexec bash -c'; kill -l`,
		`echo 'kill 1' | parallel 'sudo bash -c "true"'`,
		`systemd-run --user --pipe parallel :::: - <<< 'echo done'`,
		`systemd-run --user parallel ::: echo ::: a b`,
		`systemd-run --user parallel -a list.txt make`,
		`systemd-run --user sem --wait kill 455274`,
		`ls | parallel sudo make; kill 455274`,
		`parallel 'sudo bash -c "make {}"' ::: a b; kill 455274`,
		`cat files | parallel --pipe 'sudo parallel gzip'; kill 455274`,
		`sudo "$CMD" 455274; kill -l`,
		// ssh и systemd-run на другую машину исполнят команду не здесь
		`ssh host sudo bash -c 'kill 1'`,
		`nice systemd-run -M c1 kill 455274`,
	} {
		t.Run(command, func(t *testing.T) {
			if r := validateKill(t, command); !r.IsValid {
				t.Errorf("не должно блокироваться: %s: %v", command, r.Suggestions)
			}
		})
	}
}

// Оболочка с литеральным текстом уровнем не считается при любых флагах
// рядом с -c: команда в ней судится так же, как без обёртки
func TestKillGuardShellFlagsNotLevel(t *testing.T) {
	for _, inner := range []string{
		`sudo systemctl stop snapper-cleanup.timer`,
		`timeout 60 pkill -f build`,
		`sudo kill 455274`,
		`timeout 5 kill -0 -1`,
	} {
		want := validateKill(t, inner).IsValid
		for _, shell := range []string{`bash -euo pipefail -c '%s'`, `bash -c -e '%s'`, `sh -ec '%s'`} {
			command := strings.Replace(shell, "%s", inner, 1)
			if got := validateKill(t, command).IsValid; got != want {
				t.Errorf("%s: пропуск %v, а без оболочки %v", command, got, want)
			}
		}
	}
}

func TestWrapperLevels(t *testing.T) {
	for _, tc := range []struct {
		via  []string
		want int
	}{
		{nil, 0},
		{[]string{"sudo"}, 1},
		{[]string{"bash", "-c"}, 0},
		{[]string{"timeout", "bash", "-c"}, 1},
		{[]string{"timeout", "bash", "-c", "sudo"}, 2},
		{[]string{"command", "exec", "time", "sudo"}, 1},
		{[]string{"pipe", "run0", "bash"}, 2},
	} {
		if got := wrapperLevels(tc.via); got != tc.want {
			t.Errorf("wrapperLevels(%q) = %d, ждали %d", tc.via, got, tc.want)
		}
	}
}

func TestParseFeedSimple(t *testing.T) {
	for _, tc := range []struct {
		args   []string
		simple bool
	}{
		{[]string{":::", "echo a", "echo b"}, true},
		{[]string{"echo", ":::", "a", ":::", "b"}, true},
		{[]string{"::::", "-"}, true},
		{[]string{"-a", "-"}, true},
		{[]string{"--arg-file", "-", "make"}, true},
		{[]string{"-j4", "echo", ":::", "a"}, false},
		{[]string{":::", "echo", "::::", "-"}, false},
		{[]string{":::+", "echo"}, false},
		{[]string{"-a", "list.txt", "make"}, false},
		{[]string{"gzip", "{}", ":::", "a"}, false},
		{[]string{"-I", "XX", "echo", "XX", ":::", "a"}, false},
		{[]string{"kill", ":::", "$PPID"}, false},
	} {
		if got := parseFeed(tc.args, false).simple; got != tc.simple {
			t.Errorf("parseFeed(%q).simple = %v, ждали %v", tc.args, got, tc.simple)
		}
	}
	if f := parseFeed([]string{"--wait", "kill", "1"}, true); !f.wait {
		t.Error("sem --wait без --sql* команду не запускает")
	}
	if f := parseFeed([]string{"--wait", "--sqlandworker", "x", "kill", "1"}, true); f.wait {
		t.Error("sem --wait с --sqlandworker команду запускает")
	}
}
