package tools

import "testing"

// Тикет kill-guard-wrapped-quoted-name: имя программы в кавычках за обёрткой
// (nice, sudo, systemd-run) — kill, а не kill" (akit shellcmd-wrapped-quoted-name):
// широкая цель и kill под systemd-run — отказ
func TestKillGuardWrappedQuotedNameBlocks(t *testing.T) {
	for _, command := range []string{
		`systemd-run --user "$HOME/bin/kill" 455274`,
		`systemd-run --user "$PREFIX/bin/pkill" opencode`,
		`nice "$HOME/bin/kill" -9 -1`,
		`sudo "$HOME/bin/kill" -9 -1`,
		`sudo nice $HOME/bin/"kill" -9 -1`,
		`sudo "${HOME}/bin/pkill" claude`,
		`nice "$HOME/bin/pkill" -u aisee`,
		`nice "$HOME/bin/k"illall claude`,
	} {
		if validateKill(t, command).IsValid {
			t.Errorf("должно блокироваться: %s", command)
		}
	}
}

// Имя в кавычках за обёрткой решается так же, как голое имя: узкая цель
// за nice и sudo проходит и с кавычками
func TestKillGuardWrappedQuotedNameSameAsBare(t *testing.T) {
	for _, pair := range [][2]string{
		{`nice "$HOME/bin/kill" 455274`, `nice kill 455274`},
		{`sudo "${HOME}/bin/pkill" opencode`, `sudo pkill opencode`},
		{`nice "$HOME/bin/kill" -9 -1`, `nice kill -9 -1`},
		{`systemd-run --user "$HOME/bin/kill" 455274`, `systemd-run --user kill 455274`},
	} {
		quoted, bare := validateKill(t, pair[0]).IsValid, validateKill(t, pair[1]).IsValid
		if quoted != bare {
			t.Errorf("%s: %v, а %s: %v", pair[0], quoted, pair[1], bare)
		}
	}
}
