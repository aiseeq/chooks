package tools

import (
	"strings"
	"testing"
)

// Пропуски kill_guard из тикета kill-guard-gaps-3: pkill и killall
// (фильтры без шаблона, регистр, -e, шаблон, который Go не разбирает,
// повтор сигнала), средства кроме kill (loginctl, systemctl, fuser, skill,
// os.kill в python, tmux), имя kill из ${X:-kill} и позиционных
// параметров, сигнал из переменной
func TestKillGuardGaps3Blocks(t *testing.T) {
	for _, command := range []string{
		// фильтр без шаблона: --env и -F значения берут, шаблона нет
		`pkill --env HOME=/home/aisee`,
		`pkill --env=HOME=/home/aisee`,
		`pkill -F /tmp/pids`,
		`pkill --cgroup user.slice`,
		`pkill -fu aisee`,
		// -p называет PID: предок, systemd --user, init
		`pkill -p 2100`,
		`pkill --pid 1500`,
		`pkill --pid=1`,
		`pkill -p 455274,2000`,
		`pkill -p x`,
		`pkill -p 2100 -x sleep`,
		// без учёта регистра
		`pkill -i CLAUDE`,
		`pkill --ignore-case KONSOLE`,
		`pkill -fi 'CLAUDE --DANGER'`,
		`pkill -if CLAUDE`,
		`killall -I CLAUDE`,
		`killall --ignore-case Konsole`,
		`killall -Ir 'CLAU.*'`,
		`killall -s9 -I CLAUDE`,
		// -e у pkill — echo: шаблон сравнивается подстрокой
		`pkill -e clau`,
		`pkill -e konsol`,
		`pkill --echo konso`,
		// шаблон, который Go не разбирает, а glibc совпадает
		`pkill 'claud\e'`,
		`killall -r 'claud\e'`,
		`pkill -f '(a'`,
		// сигнал задан несколько раз: 0 только в одной опции
		`pkill --signal 0 --signal 9 claude`,
		`pkill --signal=0 --signal=9 claude`,
		`pkill -0 --signal 9 konsole`,
		`pkill --signal 0 -9 konsole`,
		`killall -s 0 -s 9 claude`,
		`killall -s0 -s9 claude`,
		`killall --signal 0 -s KILL konsole`,
		`killall -0 -9 claude`,
		// killall -g: сигнал группе найденного
		`killall -g opencode`,
		`killall --process-group sleep`,
		// loginctl: процессы сеанса или пользователя целиком
		`loginctl terminate-user aisee`,
		`loginctl terminate-session 3`,
		`loginctl kill-user aisee`,
		`loginctl kill-session c2`,
		`sudo loginctl --no-ask-password terminate-user aisee`,
		`loginctl -H host terminate-seat seat0`,
		// systemctl: юнит предка или графической оболочки, цель, шаблон,
		// переменная, выход менеджера
		`systemctl --user stop agents.slice`,
		`systemctl --user kill agent-x-1.scope`,
		`systemctl --user restart app.slice`,
		`systemctl --user stop app-org.kde.konsole-7438.scope`,
		`systemctl --user stop plasma-plasmashell`,
		`systemctl --user freeze agents.slice`,
		`systemctl --user kill -s USR1 app.slice`,
		`systemctl --user kill --signal=KILL agents.slice`,
		`systemctl --user disable --now app.slice`,
		`systemctl --user stop graphical-session.target`,
		`systemctl --user stop -- -.slice`,
		`systemctl --user isolate default.target`,
		`systemctl --user exit`,
		`systemctl reboot`,
		`sudo systemctl stop user@1000.service`,
		`sudo systemctl stop sddm`,
		`sudo systemctl stop user-1000.slice`,
		`sudo systemctl restart user.slice`,
		`systemctl --user stop "$u"`,
		`systemctl --user stop 'agent-*'`,
		`systemctl --user stop opencode.service agents.slice`,
		`echo agents.slice | xargs systemctl --user stop`,
		// fuser -k: файл, каталог, терминал, файловая система, переменная
		`fuser -k /dev/pts/1`,
		`fuser -k .`,
		`fuser -km /home`,
		`fuser -k -TERM app.log`,
		`fuser --kill /tmp/x`,
		`fuser -vk "$F"`,
		`fuser -k -n file 8080`,
		`echo 8080/tcp | xargs fuser -k`,
		// skill выбирает по пользователю, терминалу, команде
		`skill -9 -u aisee`,
		`skill -KILL -t pts/1`,
		`skill firefox`,
		// os.kill в python: родитель, группа, все, предок, вычисляемый PID
		`python3 -c 'import os; os.kill(os.getppid(), 9)'`,
		`python3 -c 'import os, signal; os.killpg(0, signal.SIGKILL)'`,
		`python3 -c 'import os, signal; os.killpg(os.getpgid(0), 15)'`,
		`python -c 'import os; os.kill(-1, 9)'`,
		`python3 -c 'import os; os.kill(0, 15)'`,
		`python3 -c 'import os; os.kill(2100, 9)'`,
		`python3 -c 'import os; os.kill(-2100, 9)'`,
		`python3.12 -u -c 'import os; os.kill(1500, 15)'`,
		`python3 -c 'import os; os.kill(*a)'`,
		"python3 - <<'E'\nimport os\nos.kill(2100, 9)\nE",
		"python3 <<'E'\nimport os, sys\nos.kill(int(sys.argv[1]),\n  9)\nE",
		`python3 <<< 'import os; os.kill(-1, 9)'`,
		`sudo python3 -c 'import os; os.kill(1, 9)'`,
		// tmux: весь сервер, текущая или все, кроме текущей, цель из переменной
		`tmux kill-server`,
		`tmux kill-session`,
		`tmux kill-session -a -t scratch`,
		`tmux kill-pane`,
		`tmux killp -a`,
		`tmux kill-window -t "$W"`,
		`tmux new -d -s x \; kill-server`,
		// имя kill из значения по умолчанию
		`${X:-kill} -9 -1`,
		`sudo ${X:-kill} -9 -1`,
		`${X-/bin/kill} -9 -1`,
		`${X-kill} -9 -1`,
		`nice ${K:=kill} -9 -1`,
		`timeout 5 ${X:-pkill} -u aisee`,
		// имя kill из позиционных параметров
		`set -- kill -9 -1; "$@"`,
		`set -- kill -9 -1; sudo "$@"`,
		`set -- pkill -u aisee; $@`,
		`set -- kill 1500; ${@}`,
		`f() { "$@"; }; f kill -9 -1`,
		`bash -c '"$@"' _ kill -9 -1`,
		`set -- kill -9 -1; exec "$@"`,
		`set -- kill -9 -1; $1 "$2" "$3"`,
		`set -- kill -9 -1; $*`,
		// слово kill только в значении по умолчанию: скрипт с подстановкой рядом
		`X=${Y:-kill}; eval "$X -9 -1"`,
		// имя kill из переменной за обёртками
		`K=kill; nice $K -9 -1`,
		`K=kill; exec $K -9 -1`,
		`K=kill; command $K -9 -1`,
		`K=kill; env $K -9 -1`,
		// сигнал из переменной: без кавычек S несёт и цели
		`kill -s $S 455274`,
		`kill -n $S 455274`,
		`kill --signal $S 455274`,
		`kill -0 -s $S 455274`,
	} {
		if validateKill(t, command).IsValid {
			t.Errorf("должно блокироваться: %s", command)
		}
	}
}

// Соседи пропусков: узкая цель — литеральный PID чужого процесса, точное
// имя не из сессии, юнит, порт, сессия tmux по имени; справка и сигнал 0
func TestKillGuardGaps3Allows(t *testing.T) {
	for _, command := range []string{
		`pkill -p 455274`,
		`pkill --pid=455274,600`,
		`pkill -p 455274 -x opencode`,
		`pkill -x -e sleep`,
		`pkill -e -x sleep`,
		`pkill -i SLEEP`,
		`pkill -ix SLEEP`,
		`pkill -A -x sleep`,
		`pkill -n -x sleep`,
		`pkill -9 -x sleep`,
		`pkill -KILL -x sleep`,
		`pkill -SIGTERM -x sleep`,
		`pkill -r S -x sleep`,
		`pkill -s 0 -x sleep`,
		`pkill --env FOO=1 -x sleep`,
		`pkill -- sleep`,
		`killall -I OPENCODE`,
		// -i у killall — вопрос перед каждым, не регистр
		`killall -i CLAUDE`,
		`killall -e opencode`,
		`killall -s 9 opencode`,
		`killall -s0 -u aisee`,
		`pkill --signal 0 --signal=0 konsole`,
		`pkill -0 -0 konsole`,
		`pkill -fi '[f]ARM-reflect'`,
		`killall -r 'open.*'`,
		`loginctl list-sessions`,
		`loginctl show-session 3`,
		`loginctl lock-session`,
		`systemctl --user stop foo.service`,
		`systemctl --user restart foo`,
		`systemctl --user stop opencode`,
		`systemctl --user stop run-sleep.scope`,
		`systemctl --user kill -s 0 app.slice`,
		`systemctl --user kill --signal=0 agents.slice`,
		`systemctl --user kill --kill-whom=main -s USR1 opencode.service`,
		`systemctl --user status agents.slice`,
		`systemctl --user start app.slice`,
		`systemctl --user disable app.slice`,
		`systemctl --user show -p MainPID agents.slice`,
		`systemctl --user -p MainPID show agents.slice`,
		`sudo systemctl stop app.slice`,
		`systemctl --user stop user.slice`,
		`sudo systemctl restart nginx`,
		`systemctl --user daemon-reload`,
		// юнит системного менеджера — не юнит менеджера пользователя
		`systemctl --user stop sddm`,
		// без слова kill ошибка разбора — не отказ
		`systemctl --user status x; echo a(b)`,
		`fuser -k 8080/tcp`,
		`fuser -k -n tcp 8080`,
		`fuser -k -9 3000/tcp`,
		`fuser -k 8080,,80/udp`,
		`fuser -v .`,
		`fuser -vm /home`,
		`fuser -l`,
		`skill -l`,
		`skill -n -u aisee`,
		`python3 -c 'import os; os.kill(455274, 9)'`,
		`python3 -c 'import os,sys; os.kill(int(sys.argv[1]), 0)' 1500`,
		`python3 -c 'print("skills")'`,
		`python3 x.py <<< 'import os; os.kill(1, 9)'`,
		`python3 -m json.tool <<< '{"a": "os.kill(1, 9)"}'`,
		`python3 -I -c "import os,signal;os.kill(os.getpid(),signal.SIGTERM)"`,
		// скрипт ssh исполнится на другой машине
		`ssh host 'sudo systemctl reboot'`,
		`ssh host systemctl --user stop app.slice`,
		`ssh host loginctl terminate-user aisee`,
		"python3 - <<'E'\nimport os\nos.kill(455274, 15)\nE",
		`tmux kill-session -t scratch`,
		`tmux kill-session -tscratch`,
		`tmux kill-pane -t %3`,
		`tmux kill-window -t foo:1`,
		`tmux -L test kill-server`,
		`tmux -S /tmp/s kill-session`,
		`tmux -2 -L test kill-server`,
		`tmux list-sessions`,
		`echo ${X:-kill}`,
		`$HOME/bin/kill-stale.sh 1`,
		`set -- kill 455274; "$@"`,
		`f() { "$@"; }; f kill 455274`,
		`set -- a b; "$@"`,
		`kill -s TERM 455274`,
		`kill -n 9 455274`,
	} {
		if r := validateKill(t, command); !r.IsValid {
			t.Errorf("не должно блокироваться: %s: %v", command, r.Suggestions)
		}
	}
}

// Причины отказов называют, что именно не так: группа у killall -g и
// os.killpg — с подсказкой про группу, сигнал из переменной — не «PID»
func TestKillGuardGaps3Reasons(t *testing.T) {
	for _, tc := range []struct{ command, want string }{
		{`killall -g opencode`, "группе процессов"},
		{`python3 -c 'import os; os.killpg(0, 9)'`, "os.killpg"},
		{`fuser -km /home`, "файловой системе"},
		{`fuser -vk "$F"`, "вычисляется"},
		{`python3 -c 'import os; os.kill(os.getppid(), 9)'`, "вычисляется"},
		{`python3 -c 'import os; os.killpg(os.getpid(), 9)'`, "os.killpg"},
		{`loginctl terminate-user aisee`, "loginctl terminate-user"},
		{`kill -s $S 455274`, "сигнал вычисляется"},
		{`pkill 'claud\e'`, "шаблон не разбирается"},
		{`systemctl --user stop agents.slice`, "claude (PID 2100)"},
		{`systemctl --user stop plasma-plasmashell`, "plasmashell (PID 3000)"},
		{`pkill -p 2100`, "предок этой сессии"},
	} {
		r := validateKill(t, tc.command)
		if r.IsValid || !strings.Contains(strings.Join(r.Suggestions, "\n"), tc.want) {
			t.Errorf("%s: ждали отказ с «%s», получили %v %q", tc.command, tc.want, r.IsValid, r.Suggestions)
		}
	}
	for _, command := range []string{`killall -g opencode`, `python3 -c 'import os; os.killpg(0, 9)'`} {
		if r := validateKill(t, command); !strings.Contains(strings.Join(r.Suggestions, "\n"), killGroupHint) {
			t.Errorf("%s: ждали подсказку про группу, получили %q", command, r.Suggestions)
		}
	}
}
