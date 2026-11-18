package tools

import "testing"

// Приёмка kill-guard-parallel-quoted-name, 2-й круг: -q, run0/pkexec/machinectl
// в команде parallel, строка в кавычках с $ — как голое имя
func TestKillGuardParallelQuotedName2Blocks(t *testing.T) {
	for _, command := range []string{
		`parallel -q "$HOME/bin/kill" ::: 455274`,
		`parallel -q "$HOME/bin/pkill" -u aisee ::: x`,
		`parallel -q nice "$HOME/bin/killall" node ::: x`,
		`sudo parallel -q "$HOME/bin/kill" ::: 455274`,
		`cat pids | parallel -q "$HOME/bin/kill"`,
		`parallel run0 "$HOME/bin/kill" ::: 455274`,
		`parallel -j1 run0 "$HOME/bin/kill" ::: 455274`,
		`nice parallel run0 "$HOME/bin/kill" ::: 455274`,
		`cat pids | parallel run0 "$HOME/bin/kill"`,
		`parallel pkexec "$HOME/bin/pkill" -u aisee ::: x`,
		`parallel machinectl shell .host "$HOME/bin/kill" ::: 455274`,
		`parallel "$HOME/bin/kill 455274" ::: x`,
		`parallel "$HOME/bin/kill -9" ::: 455274`,
		`parallel "echo start; $HOME/bin/pkill -f foo" ::: x`,
	} {
		if validateKill(t, command).IsValid {
			t.Errorf("должно блокироваться: %s", command)
		}
	}
}

func TestKillGuardParallelQuotedName2Allows(t *testing.T) {
	for _, command := range []string{
		`parallel gzip ::: *.log`,
		`parallel -j4 make -C {} ::: a b`,
		`parallel "$HOME/bin/killer" ::: a`,
		`parallel grep "$HOME/bin/kill" ::: a`,
		`parallel -q grep "$HOME/bin/kill" ::: a`,
		`parallel "$HOME/bin/kill-stale.sh" ::: 1`,
	} {
		if r := validateKill(t, command); !r.IsValid {
			t.Errorf("не должно блокироваться: %s: %v", command, r.Suggestions)
		}
	}
}
