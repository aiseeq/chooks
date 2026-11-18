package tools

import "testing"

// Тикет kill-guard-run0-signal: run0 запускает команду временной службой
// менеджера, а не потомком агента — сигнал под run0 ядро не сверяет: отказ.
// sudo и pkexec делают exec в своём процессе — как было
func TestKillGuardRun0Signal(t *testing.T) {
	for _, command := range []string{
		`run0 kill 12345`,
		`run0 pkill opencode`,
		`run0 --user=aisee killall sleep`,
		`run0 systemd-run --scope kill 12345`,
		`run0 sh -c 'kill 12345'`,
		`nice run0 /bin/kill -TERM 12345`,
		// ввод оболочки за run0 — её скрипт
		"run0 bash <<EOF\nkill 12345\nEOF",
		"run0 bash <<EOF\nkill -9 -1\nEOF",
		`run0 bash <<< 'kill 12345'`,
		"run0 sh -s <<EOF\nkill 12345\nEOF",
		"run0 systemd-run --scope bash <<EOF\nkill 12345\nEOF",
		// тело с ошибкой разбора: bash исполнит kill до неё
		"run0 bash <<EOF\nkill 1234\nfi\nEOF",
		"run0 bash <<EOF\nkill -9 -1\n)\nEOF",
		// ввод из подстановки не виден
		`run0 bash < <(echo kill -9 -1)`,
		`echo kill 12345 | run0 bash`,
	} {
		if validateKill(t, command).IsValid {
			t.Errorf("должно блокироваться: %s", command)
		}
	}
	for _, command := range []string{
		`sudo kill 12345`,
		`pkexec kill 12345`,
		`run0 systemctl --user status x.service`,
		`systemd-run --user --scope kill 12345`,
	} {
		if r := validateKill(t, command); !r.IsValid {
			t.Errorf("не должно блокироваться: %s: %v", command, r.Suggestions)
		}
	}
}
