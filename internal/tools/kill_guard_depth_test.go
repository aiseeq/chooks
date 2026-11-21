package tools

import (
	"strings"
	"testing"
)

// Правило глубины: команда и одна обёртка над ней проверяются как прямые;
// сигнал или остановка под двумя и более обёртками, имя, вычисляемое там
// при запуске, с видимой командой сигнала, и сигнал из нелитерального
// источника parallel, sem, xargs — отказ с подсказкой запустить напрямую
func TestKillGuardDepthBlocks(t *testing.T) {
	for _, command := range []string{
		// сигнал на двух уровнях обёрток
		`timeout 5 sudo -- A=$a B=$b kill 455274`,
		`timeout 5 sudo -u root -- X=a/b kill -TERM 455274`,
		`sudo -u x env A=1 kill 455274`,
		`nohup setsid kill 455274`,
		`timeout 5 sudo bash -euo pipefail -c 'kill 455274'`,
		`timeout 5 sudo bash <<< 'sudo kill 455274'`,
		`sudo -u x timeout 5 systemctl stop nginx.service`,
		`setsid -w nohup ksh -c 'kill -0 -1'`,
		`timeout 5 sudo loginctl terminate-user aisee`,
		`sudo -u x env A=1 tmux kill-server`,
		`timeout 5 sudo fuser -k /tmp/app.sock`,
		`sudo find /proc -maxdepth 0 -exec sudo kill 455274 \;`,
		`systemd-run --user sem 'kill 455274'`,
		`sem timeout 5 systemctl --user stop pipewire.service`,
		`timeout 5 parallel ::: 'kill 455274'`,
		`systemd-run --user --pipe parallel :::: - <<< 'kill 455274'`,
		`sudo parallel kill ::: 455274`,
		// имя команды вычисляется при запуске, команда сигнала видна
		`timeout 5 sudo $(echo kill) 455274`,
		// сигнал с целями или командой из нелитерального источника
		`parallel -a units.txt systemctl --user stop`,
		`cat pids | parallel kill`,
		`parallel kill :::: pids.txt`,
		`xargs -a pids.txt kill`,
		`printf '%s\n' 'ExecStartPre=/usr/bin/kill 1' | parallel systemd-run --user -p {}`,
		`echo 'kill 1' | parallel 'sudo bash -c'`,
		`systemd-run --user parallel :::: +- <<< 'kill 12345'`,
		`systemd-run --user parallel --colsep , ::: 'kill,12345'`,
		`parallel -a - ::: -9 ::: -1 <<< kill`,
		`parallel -a - ::: 1 <<< kill`,
		// значения встают в команду подстановкой
		`systemd-run --user parallel -I XX XX ::: 'kill 12345'`,
		`systemd-run --user --pipe parallel {} <<< 'kill 12345'`,
		// parallel в задании parallel судится тем же правилом
		`parallel -I XX "parallel 'sudo systemctl stop {}' ::: XX" ::: nginx`,
		`echo 'kill 1' | parallel --cat 'parallel --cat pkexec bash {}'`,
		`echo user@1000.service | parallel --pipe -I XX "parallel 'sudo systemctl stop {}' ::: XX"`,
		`echo x | parallel --pipe "parallel 'sudo systemctl restart {}' ::: nginx"`,
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
		`parallel -I XX 'systemctl --user restart XX' ::: foo`,
		`echo make | parallel 'pkexec bash -c'; kill -l`,
		`systemd-run --user parallel ::: echo ::: a b`,
		`systemd-run --user parallel -a list.txt make`,
		`ls | parallel sudo make; kill 455274`,
		`parallel 'sudo bash -c "make {}"' ::: a b; kill 455274`,
		`cat files | parallel --pipe 'sudo parallel gzip'; kill 455274`,
		`sudo "$CMD" 455274; kill -l`,
		`find /proc -maxdepth 0 -exec kill 455274 \;`,
		// eval, как bash -c, уровнем не считается: разбор прочёл всё
		`eval eval eval eval eval kill 455274`,
		// решение зависит от команды сигнала, а не от слова в тексте
		`timeout 600 parallel -j4 :::: tasks.txt && grep -c killed build.log`,
		`nice parallel :::: jobs.txt; systemctl --user status foo`,
		`timeout 60 parallel < jobs.txt; systemctl --user is-active x`,
		`timeout 30 parallel -j2 'gzip {}' ::: a b; echo kill`,
		`timeout 60 sudo "$TOOL" status; pgrep -a kill`,
		`timeout 5 sudo "$CMD" 455274; kill -l`,
		// нелитеральный ввод без видимой команды сигнала
		`cat a.sh | parallel systemd-run --user --pipe bash`,
		`systemd-run --user parallel -a units.txt`,
		`systemd-run --user --pipe parallel ::: echo :::: - <<< 'done'`,
		`systemd-run --user parallel :::+ 'echo safe'`,
		`setsid parallel ::: 'echo kill'`,
		`grep -rl shellcmd . | xargs grep -ln 'pkill'`,
		`find . -print0 | xargs -0 grep -l kill`,
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
// рядом с -c, с текстом из <<< и heredoc, eval — тоже: команда в ней
// судится так же, как без оболочки
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
	for _, inner := range []string{`kill 455274`, `kill -9 1`} {
		for _, form := range [][2]string{
			{`sudo bash <<< '%s'`, `sudo bash -c '%s'`},
			{`sudo bash -s <<< '%s'`, `sudo bash -c '%s'`},
			{"sudo bash <<'E'\n%s\nE", `sudo bash -c '%s'`},
			{`timeout 5 eval '%s'`, `timeout 5 bash -c '%s'`},
		} {
			command, ref := strings.Replace(form[0], "%s", inner, 1), strings.Replace(form[1], "%s", inner, 1)
			if got, want := validateKill(t, command).IsValid, validateKill(t, ref).IsValid; got != want {
				t.Errorf("%s: пропуск %v, а у %s — %v", command, got, ref, want)
			}
		}
	}
}

// run0 запускает службу, как systemd-run: SuccessAction= и FailureAction=,
// которые гасят машину, — отказ
func TestKillGuardRun0EndAction(t *testing.T) {
	for _, command := range []string{
		`run0 --property=SuccessAction=poweroff true`,
		`run0 --property SuccessAction=poweroff true`,
		`run0 --property=FailureAction=reboot false`,
	} {
		if validateKill(t, command).IsValid {
			t.Errorf("должно блокироваться: %s", command)
		}
	}
	if r := validateKill(t, `run0 --property=SuccessAction=none true`); !r.IsValid {
		t.Errorf("не должно блокироваться: %v", r.Suggestions)
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
		{[]string{"pipe", "run0", "/bin/bash"}, 1},
		{[]string{"timeout", "eval"}, 1},
		{[]string{"sudo", "find", "-exec"}, 2},
	} {
		if got := wrapperLevels(tc.via); got != tc.want {
			t.Errorf("wrapperLevels(%q) = %d, ждали %d", tc.via, got, tc.want)
		}
	}
}

// Начало команды parallel ищется за опциями: слово сразу за опцией может
// быть её значением
func TestFeedSignals(t *testing.T) {
	for _, tc := range []struct {
		head, values []string
		want         string
	}{
		{[]string{"-j", "2", "pkill", "-f"}, []string{"firefox"}, "pkill"},
		{[]string{"--joblog", "log", "kill"}, []string{"455274"}, "kill"},
		{[]string{"-j4"}, []string{"echo a", "kill 455274"}, "kill"},
		{[]string{"grep", "-c", "kill"}, []string{"a"}, ""},
		{[]string{"kill", "-0"}, []string{"455274"}, ""},
	} {
		got := ""
		if sig, _ := feedSignals(tc.head, tc.values, -1); len(sig) > 0 {
			got = sig[0].Name
		}
		if got != tc.want {
			t.Errorf("feedSignals(%q, %q) = %q, ждали %q", tc.head, tc.values, got, tc.want)
		}
	}
}
