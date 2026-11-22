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
		`timeout 5 sudo fuser -km /tmp/app.sock`,
		`sudo -u x env A=1 systemctl reboot`,
		`sudo find /proc -maxdepth 0 -exec sudo kill 455274 \;`,
		`systemd-run --user sem 'kill 455274'`,
		`sem timeout 5 systemctl --user stop pipewire.service`,
		`sudo parallel kill ::: 455274`,
		// имя команды вычисляется при запуске, команда сигнала видна
		`timeout 5 sudo $(echo kill) 455274`,
		`R=run0; $R bash <<< 'kill 1'`,
		"R=run0; $R bash <<'E'\nkill 1\nE",
		`R=run0; $R bash -c 'kill 1'`,
		// сигнал с целями или командой из нелитерального источника
		`parallel -a units.txt systemctl --user stop`,
		`cat pids | parallel kill`,
		`echo 'kill 1' | parallel 'sudo bash -c'`,
		`echo 'kill 1' | parallel 'sudo ksh -c'`,
		`systemd-run --user --pipe parallel :::: - <<< 'kill 455274'`,
		`systemd-run --user parallel :::: +- <<< 'kill 12345'`,
		`parallel -a - ::: -9 ::: -1 <<< kill`,
		`parallel -a - ::: 1 <<< kill`,
		`parallel :::: - <<< 'kill 455274'`,
		`xargs kill <<< 1`,
		`printf 'kill -9 1\n' | parallel`,
		`parallel <<< 'kill -9 1'`,
		`printf 'pkill -f firefox\n' | parallel`,
		"parallel <<'E'\nkill -9 1\nE",
		`systemd-run --user parallel --colsep , ::: 'kill,12345'`,
		`systemd-run --user --pipe parallel {} <<< 'kill 12345'`,
		// значения встают в команду подстановкой
		// parallel в задании parallel судится тем же правилом
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

func TestKillGuardRun0UnreadInput(t *testing.T) {
	for _, command := range []string{
		`exec < <(echo kill 6159); run0 bash`,
		`f() { run0 bash; }; echo 'kill 6159' | f`,
		`while read x; do run0 bash; done < <(printf 'x\nkill 6159\n')`,
	} {
		if r := validateKill(t, command); r.IsValid {
			t.Errorf("нечитаемый сигнал во вводе run0 должен блокироваться: %s", command)
		}
	}
}

func TestKillGuardDepthAllows(t *testing.T) {
	for _, command := range []string{
		// безвредная команда на двух уровнях
		`timeout 60 bash -c 'go test ./...'; kill -l`,
		`sudo -u x env A=1 make; kill -l`,
		`sudo fish --command 'kill 455274'`,
		`sudo busybox sh -c 'kill 455274'`,
		`timeout 5 sudo killall -l`,
		`timeout 5 sudo killall --help`,
		`timeout 5 sudo pkill -V`,
		`timeout 5 sudo pkill --help`,
		`timeout 5 sudo systemctl stop --help`,
		`sudo timeout 5 sem --wait kill 455274`,
		`timeout 5 bash -c 'while kill -0 455274; do sleep 1; done'`,
		`sudo -u x env kill -l`,
		`systemd-run --user nice make`,
		// один уровень: проверка как у прямой команды
		`systemd-run --user make`,
		`parallel echo ::: a b; kill 455274`,
		`printf '%s\n' 'ExecStartPre=/usr/bin/kill 1' | parallel systemd-run --user -p {}`,
		`sudo kill 455274`,
		`bash -c 'kill 455274'`,
		`timeout 5 kill 455274`,
		`parallel kill ::: 455274`,
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
	for _, command := range []string{
		`run0 --prop=SuccessAction=poweroff true`,
		`run0 --prop=ExecStartPre=/usr/bin/kill\ 6159 true`,
	} {
		if validateKill(t, command).IsValid {
			t.Errorf("сокращённая --property должна разбираться: %s", command)
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
func TestWrappedFeedsParseErrorHint(t *testing.T) {
	r := validateKill(t, "# kill\nbash -c 'echo \"oops' parallel true")
	if r.IsValid {
		t.Fatal("unparseable nested wrapper should be rejected")
	}
	if !strings.Contains(strings.Join(r.Suggestions, "\n"), "Запусти напрямую") {
		t.Fatalf("pre-tool-use parse refusal lacks direct-run hint: %v", r.Suggestions)
	}
}
