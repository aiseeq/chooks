package tools

import "testing"

// Тикет chooks-systemd-run-scope-kill-false-positive: systemd-run --scope
// делает exec в своём процессе — kill остаётся потомком агента, ядро
// (agent-signal-guard) его сигналы сверяет (журнал 18.11 09:28:35: deny,
// ppid — bash агента). Цель проверяется, как у прямого вызова
func TestKillGuardSystemdRunScopeSignal(t *testing.T) {
	for _, command := range []string{
		`systemd-run --user --scope --slice=agents.slice kill 12345`,
		`systemd-run --user --scope --slice=agents.slice --unit=probe-other-552851 /bin/kill -TERM 552870`,
		`systemd-run --scope kill 12345`,
		`systemd-run --user --sco -p CPUQuota=10% kill -TERM 12345`,
	} {
		if r := validateKill(t, command); !r.IsValid {
			t.Errorf("не должно блокироваться: %s: %v", command, r.Suggestions)
		}
	}
	for _, command := range []string{
		`systemd-run --user kill 12345`,
		`systemd-run --user --unit x kill 12345`,
		`systemd-run --user --wait kill 12345`,
		// --scope после команды — её аргумент, а не опция systemd-run
		`systemd-run --user kill --scope 12345`,
		// внешний --scope, внутренний — служба
		`systemd-run --user --scope systemd-run --user kill 12345`,
		// широкая цель — отказ, как у прямого kill
		//nolint:signal-implicit-target // строка — случай теста kill_guard для validateKill: файл её не запускает
		`systemd-run --user --scope kill -9 -1`,
		`systemd-run --user --scope pkill -u aisee`,
	} {
		if validateKill(t, command).IsValid {
			t.Errorf("должно блокироваться: %s", command)
		}
	}
}
