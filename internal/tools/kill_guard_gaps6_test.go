package tools

import (
	"slices"
	"strings"
	"testing"

	"github.com/aiseeq/akit/shellcmd"
)

// Остаток приёмки kill-guard-gaps-5 (тикет kill-guard-gaps-6): несколько
// присваиваний подряд за sudo -- в любом порядке $ — не имя программы,
// цель kill решается, как без них; вычисляемое имя программы и оболочка,
// читающая ввод, — отказ, как с одним присваиванием
func TestKillGuardGaps6Blocks(t *testing.T) {
	for _, command := range []string{
		`sudo -- PATH=$PATH LANG=C kill -9 -1`,
		`sudo -- LANG=C PATH=$PATH kill -9 -1`,
		`sudo -- A=$a B=$b kill 1500`,
		`sudo -- A=$a B=$b C=$(pwd) kill 1`,
		`sudo -- A=$a B=1 C=$c /bin/kill -9 -1`,
		`sudo -- A=$a B=$b $(echo kill) 455274`,
		`K=kill; sudo -- A=$a B=$b $K -9 -1`,
		`sudo -- A=$a B=$b bash -c 'kill 1500'`,
		`echo 'kill 455274' | sudo -- A=$a B=1 bash`,
		`sudo -- A=$a B=$b tmux kill-server`,
		`timeout 5 sudo -- A=$a B=$b kill 2100`,
	} {
		if validateKill(t, command).IsValid {
			t.Errorf("должно блокироваться: %s", command)
		}
	}
}

// Соседи: несколько присваиваний за sudo -- перед узкой целью; присваивания
// без программы за ними
func TestKillGuardGaps6Allows(t *testing.T) {
	for _, command := range []string{
		`sudo -- PATH=$PATH LANG=C kill 455274`,
		`sudo -- A=$a B=$b kill 455274`,
		`sudo -- LANG=C PATH=$PATH kill 455274`,
		`sudo -- A=$a B=$b C=$(pwd) D=1 kill 455274`,
		`sudo -- PATH=$PATH LANG=C /bin/kill 455274`,
		`sudo -- A=$a B=$b tmux kill-session -t x`,
		`sudo -E -- PATH=$PATH:/usr/sbin LANG=C fuser -k 8080/tcp`,
		`sudo -- A=$a B=$b; kill 455274`,
	} {
		if r := validateKill(t, command); !r.IsValid {
			t.Errorf("не должно блокироваться: %s: %v", command, r.Suggestions)
		}
	}
}

// sudoAssignment: первое из нескольких присваиваний узнаётся по программе
// за всеми ними; без программы или с другой программой — вызов как вызов
func TestSudoAssignmentSeveral(t *testing.T) {
	calls, err := shellcmd.Calls(`sudo -- A=$a B=$b kill 455274`)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(calls, func(c shellcmd.Call) bool { return c.Name == "A=$a" })
	if i < 0 {
		t.Fatalf("нет вызова с присваиванием: %+v", calls)
	}
	assign := calls[i]
	if !sudoAssignment(assign, calls) {
		t.Errorf("первое из присваиваний за sudo -- не узнано: %+v", calls)
	}
	program := shellcmd.Call{Name: "kill", Args: []string{"455274"}, Raw: assign.Raw, Via: assign.Via}
	only := assign
	only.Args = []string{"B=$b"}
	if sudoAssignment(only, []shellcmd.Call{only, program}) {
		t.Errorf("присваивания без программы приняты за присваивание перед программой")
	}
	other := program
	other.Name = "B=$b"
	if sudoAssignment(assign, []shellcmd.Call{assign, other}) {
		t.Errorf("программа — второе присваивание, а не слово за ним")
	}
}

// Подсказка к systemctl без юнита (выход менеджера, выключение, смена цели)
// — по смыслу команды, без подсказки про юнит. Текст каждой команды —
// TestKillGuardEndTexts
func TestKillGuardGaps6EndHint(t *testing.T) {
	for _, tc := range []struct{ command, hint string }{
		{`systemctl --user exit`, systemctlEndHint("systemctl --user", "exit")},
		{`systemctl --user isolate default.target`, systemctlEndHint("systemctl --user", "isolate")},
		{`sudo systemctl isolate rescue.target`, systemctlEndHint("systemctl", "isolate")},
		{`sudo systemctl rescue`, systemctlEndHint("systemctl", "rescue")},
		{`sudo systemctl poweroff`, systemctlEndHint("systemctl", "poweroff")},
		{`systemctl reboot`, systemctlEndHint("systemctl", "reboot")},
	} {
		got := validateKill(t, tc.command).Suggestions
		if !slices.Contains(got, tc.hint) || strings.Contains(strings.Join(got, "\n"), "status юнит") {
			t.Errorf("%s: ждали только подсказку %q, получили %q", tc.command, tc.hint, got)
		}
	}
}
