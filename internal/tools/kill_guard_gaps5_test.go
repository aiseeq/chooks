package tools

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/aiseeq/akit/shellcmd"
	"github.com/aiseeq/chooks/internal/core"
)

// Остаток приёмки kill-guard-gaps-4 (тикет kill-guard-gaps-5): начало
// длинной опции pkill и killall, tmux kill-server\; и сервер по умолчанию
// через -S и -L, пустая цель -t, рестарт звука
func TestKillGuardGaps5Blocks(t *testing.T) {
	for _, command := range []string{
		// однозначное начало длинной опции — как полная (getopt_long)
		`pkill --inv zzzz`,
		`pkill --in zzzz`,
		`pkill --invers -x zzzz`,
		`pkill --signal=KILL --inv zzzz`,
		`pkill --sig 9 --inv zzzz`,
		// --f — full: шаблон сверяется с командной строкой claude
		`pkill --f 'dangerously-skip'`,
		`killall --proc zzzz`,
		`killall --process-g zzzz`,
		`killall --regex 'clau.*'`,
		// --p у pkill неоднозначно, но -p со значением — PID 1
		`pkill -p1`,
		// tmux: ; на конце слова делит команды
		`tmux kill-server\; ls`,
		`tmux kill-ser\; ls`,
		`tmux new -d -s x\; kill-server`,
		`tmux kill-session -t x\; kill-session`,
		// сервер по умолчанию через -S и -L
		`tmux -S /tmp/tmux-1000/default kill-server`,
		`tmux -S/tmp/tmux-1000/default kill-ser`,
		`tmux -S /tmp//tmux-1000/default/ kill-server`,
		`tmux -S "$TMUX_TMPDIR/tmux-$UID/default" kill-server`,
		`tmux -L default kill-server`,
		`tmux -Ldefault kill-session`,
		`tmux -L foo -S /tmp/tmux-1000/default kill-server`,
		`tmux -2 -L foo -L default kill-server`,
		`tmux -f /dev/null kill-server`,
		`tmux -uf /dev/null kill-server`,
		// пустая цель — текущая
		`tmux kill-session -t ''`,
		`tmux kill-window -t ""`,
		// присваивание за sudo -- не прячет kill за ним
		`sudo -- PATH=$PATH kill -9 -1`,
		`sudo -- PATH=$PATH kill 1`,
		`sudo -- X=$(pwd) kill 1`,
		`sudo -- X=1 $(echo kill) 455274`,
		`K=kill; sudo -- PATH=$PATH $K -9 -1`,
		`sudo -- PATH=$PATH bash -c 'kill 1500'`,
		`echo 'kill 455274' | sudo -- X=1 bash`,
		// звук: стоп, kill, mask и disable --now — отказ
		`systemctl --user stop pipewire`,
		`systemctl --user stop pipewire.socket`,
		`systemctl --user kill pipewire`,
		`systemctl --user mask --now pipewire`,
		`systemctl --user restart pipewire plasma-kwin_wayland`,
		`systemctl --user restart session.slice`,
	} {
		if validateKill(t, command).IsValid {
			t.Errorf("должно блокироваться: %s", command)
		}
	}
}

// Соседи: неоднозначное и незнакомое начало опции утилита не исполнит
// (как раньше — флаг без значения), сигнал 0 сокращением, свой сервер
// tmux, цель с литералом \;, присваивание с $ за sudo -- перед узкой целью,
// рестарт pipewire
func TestKillGuardGaps5Allows(t *testing.T) {
	for _, command := range []string{
		`pkill --inverse --sig 0 zzzz`,
		`pkill --inv --sig=0 zzzz`,
		`pkill --i zzzz`,
		`pkill --p 1 zzzz`,
		`pkill --inverse-x zzzz`,
		`pkill --ex konsol`,
		`killall --sig 0 konsole`,
		`killall --si=0 konsole`,
		`killall 'clau.*'`,
		`tmux -S /tmp/mysock kill-server`,
		`tmux -S /tmp/x/default kill-server`,
		// -S без значения: слов команды нет
		`tmux -S; echo kill-server`,
		`tmux -S /tmp/tmux-1000/mine kill-server`,
		`tmux -S /tmp/mysock -L default kill-server`,
		`tmux -L foo kill-server`,
		`tmux -L default -L foo kill-server`,
		`tmux -uL foo kill-server`,
		`tmux -2 -S /tmp/mysock kill-server`,
		`tmux kill-session -t x\;`,
		`tmux kill-session -t 'x\;'`,
		`tmux new -d -s x\; kill-session -t x`,
		// \; на конце слова — литерал: kill-server — слово команды new
		`tmux new -d -s x 'vim\;' kill-server`,
		`tmux -S /tmp/tmux-1000/default kill-session -t x`,
		`sudo -- PATH=$PATH kill 455274`,
		`sudo -- HOME=$HOME kill 455274`,
		`sudo -- X=$(pwd) kill 455274`,
		`sudo -- X=$HOME/a$Y kill 455274`,
		`sudo -- PATH=$PATH /bin/kill 455274`,
		`sudo -- PATH=$PATH tmux kill-session -t x`,
		`sudo -E -- PATH=$PATH:/usr/sbin fuser -k 8080/tcp`,
		`systemctl --user restart pipewire`,
		`systemctl --user restart pipewire.service`,
		`systemctl --user restart pipewire.socket`,
		`systemctl --user try-restart pipewire`,
		`systemctl --user restart pipewire-pulse.socket`,
		`systemctl --user restart pipewire pipewire-pulse`,
	} {
		if r := validateKill(t, command); !r.IsValid {
			t.Errorf("не должно блокироваться: %s: %v", command, r.Suggestions)
		}
	}
}

// wireplumber держит сессию, как pipewire: стоп — отказ, рестарт — можно
func TestKillGuardWireplumberRestart(t *testing.T) {
	for _, tc := range []struct {
		command string
		valid   bool
	}{
		{`systemctl --user restart wireplumber`, true},
		{`systemctl --user restart wireplumber.service`, true},
		{`systemctl --user stop wireplumber`, false},
		{`systemctl --user kill wireplumber.service`, false},
		{`systemctl --user disable --now wireplumber`, false},
	} {
		tool := newKillGuard(t)
		addFakeProc(t, tool, "3600", "1500", "wireplumber", "0::/user.slice/user-1000.slice/user@1000.service/session.slice/wireplumber.service")
		r, err := tool.ValidateTool(context.Background(), &core.ToolInput{ToolName: "Bash", Command: tc.command})
		if err != nil {
			t.Fatalf("ValidateTool: %v", err)
		}
		if r.IsValid != tc.valid {
			t.Errorf("%s: ждали %v, получили %v %q", tc.command, tc.valid, r.IsValid, r.Suggestions)
		}
	}
}

// Тексты: подсказка systemctl по менеджеру (системный — без --user) и по
// отказу (юнит назван — не «назови литералом»); у присваивания за sudo --
// нет своей строки «читает скрипт из ввода»
func TestKillGuardGaps5Texts(t *testing.T) {
	for _, tc := range []struct{ command, hint string }{
		{`sudo systemctl stop session-3.scope`, systemctlHint("systemctl", true)},
		{`sudo systemctl restart dbus`, systemctlHint("systemctl", true)},
		{`systemctl --user stop agents.slice`, systemctlHint("systemctl --user", true)},
		{`systemctl --user stop $U`, systemctlHint("systemctl --user", false)},
		{`systemctl --user stop 'app-*'`, systemctlHint("systemctl --user", false)},
		{`sudo systemctl stop 'session-{3,4}.scope'`, systemctlHint("systemctl", false)},
		{`ls | xargs systemctl --user stop`, systemctlHint("systemctl --user", false)},
	} {
		got := validateKill(t, tc.command).Suggestions
		if !slices.Contains(got, tc.hint) {
			t.Errorf("%s: ждали подсказку %q, получили %q", tc.command, tc.hint, got)
		}
	}
	if got := strings.Join(validateKill(t, `sudo systemctl stop session-3.scope`).Suggestions, "\n"); strings.Contains(got, "--user") || strings.Contains(got, "литералом и") {
		t.Errorf("системный systemctl и юнит литералом: подсказка про --user или «назови литералом» лишняя, получили %q", got)
	}
	if got := strings.Join(validateKill(t, `echo 'kill 455274' | sudo -- X=1 bash`).Suggestions, "\n"); strings.Contains(got, "X=1") || !strings.Contains(got, "bash читает скрипт из ввода") {
		t.Errorf("sudo -- X=1 bash: ждали отказ про bash без строки X=1, получили %q", got)
	}
	if got := strings.Join(validateKill(t, `tmux kill-server\; ls`).Suggestions, "\n"); !strings.Contains(got, "tmux kill-server гасит") {
		t.Errorf("tmux kill-server\\;: ждали отказ на kill-server, получили %q", got)
	}
	if got := strings.Join(validateKill(t, `pkill --inv zzzz`).Suggestions, "\n"); !strings.Contains(got, "--inverse") {
		t.Errorf("pkill --inv: ждали отказ на --inverse, получили %q", got)
	}
}

// longOption: точное имя, единственное начало, неоднозначное и незнакомое
func TestLongOption(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		{"inv", "inverse"},
		{"f", "full"},
		{"ns", "ns"},
		{"pid", "pid"},
		{"pi", ""},
		{"p", ""},
		{"i", ""},
		// опции pgrep в общей таблице: --l и --q неоднозначны, --d — delimiter
		{"l", ""},
		{"q", ""},
		{"d", "delimiter"},
		{"zz", ""},
		{"", ""},
	} {
		if got := longOption(pkillOptions, tc.name); got != tc.want {
			t.Errorf("pkill --%s: ждали %q, получили %q", tc.name, tc.want, got)
		}
	}
	for _, tc := range []struct{ name, want string }{
		{"proc", "process-group"},
		{"ver", ""},
		{"si", "signal"},
	} {
		if got := longOption(killallOptions, tc.name); got != tc.want {
			t.Errorf("killall --%s: ждали %q, получили %q", tc.name, tc.want, got)
		}
	}
}

// sudoAssignment узнаёт присваивание за sudo -- по паре вызовов shellcmd:
// оно само и программа за ним с тем же текстом и Via. Без пары, без sudo в
// Via или с другой программой — вызов как вызов
func TestSudoAssignment(t *testing.T) {
	calls, err := shellcmd.Calls(`sudo -- PATH=$PATH /bin/kill 455274`)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(calls, func(c shellcmd.Call) bool { return c.Name == "PATH=$PATH" })
	if i < 0 {
		t.Fatalf("нет вызова с присваиванием: %+v", calls)
	}
	assign := calls[i]
	program := shellcmd.Call{Name: "kill", Args: []string{"455274"}, Raw: assign.Raw, Via: assign.Via}
	if !sudoAssignment(assign, calls) || !sudoAssignment(assign, []shellcmd.Call{assign, program}) {
		t.Errorf("присваивание за sudo -- не узнано: %+v", calls)
	}
	for name, change := range map[string]func(a, p *shellcmd.Call){
		"без sudo":         func(a, p *shellcmd.Call) { a.Via, p.Via = []string{"env"}, []string{"env"} },
		"другой текст":     func(_, p *shellcmd.Call) { p.Raw = "kill 455274" },
		"другие обёртки":   func(_, p *shellcmd.Call) { p.Via = []string{"sudo", "nice"} },
		"другая программа": func(_, p *shellcmd.Call) { p.Name = "pkill" },
		"другие аргументы": func(_, p *shellcmd.Call) { p.Args = []string{"1"} },
		"без аргументов":   func(a, _ *shellcmd.Call) { a.Args = nil },
	} {
		a, p := assign, program
		change(&a, &p)
		if sudoAssignment(a, []shellcmd.Call{a, p}) {
			t.Errorf("%s: вызов принят за присваивание за sudo --", name)
		}
	}
}
