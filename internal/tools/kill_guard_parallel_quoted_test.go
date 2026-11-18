package tools

import "testing"

// Тикет kill-guard-parallel-quoted-name: имя программы с $ в кавычках под
// parallel решается как голое (akit v1.81.1 отдаёт голое имя и под parallel)
func TestKillGuardParallelQuotedNameBlocks(t *testing.T) {
	for _, command := range []string{
		`parallel "$HOME/bin/pkill" -9 -u aisee ::: x`,
		`parallel nice "$HOME/bin/kill" ::: 455274`,
		`parallel -j1 sudo "$HOME/bin/kill" ::: 455274`,
		`parallel $HOME/bin/"pkill" -u aisee ::: x`,
		//nolint:signal-implicit-target // строка — случай теста kill_guard для validateKill: файл её не запускает
		`nice parallel "$HOME/bin/kill" -9 -1 ::: x`,
	} {
		if validateKill(t, command).IsValid {
			t.Errorf("должно блокироваться: %s", command)
		}
	}
}

// Пары: имя в кавычках под parallel — то же решение, что голое
func TestKillGuardParallelQuotedNameSameAsBare(t *testing.T) {
	for _, pair := range [][2]string{
		{`parallel "$HOME/bin/pkill" -u aisee ::: x`, `parallel pkill -u aisee ::: x`},
		{`parallel nice "$HOME/bin/kill" ::: 455274`, `parallel nice kill ::: 455274`},
		{`parallel -j1 sudo "$HOME/bin/kill" ::: 455274`, `parallel -j1 sudo kill ::: 455274`},
		{`parallel "$HOME/bin/kill" ::: 455274`, `parallel kill ::: 455274`},
		{`parallel "$HOME/bin/kill" 455274 ::: x`, `parallel kill 455274 ::: x`},
		{`parallel "$HOME/bin/kill-stale.sh" ::: 1`, `parallel kill-stale.sh ::: 1`},
	} {
		quoted, bare := validateKill(t, pair[0]).IsValid, validateKill(t, pair[1]).IsValid
		if quoted != bare {
			t.Errorf("%s: %v, а %s: %v", pair[0], quoted, pair[1], bare)
		}
	}
}
