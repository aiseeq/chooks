package tools

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/aiseeq/chooks/internal/core"
)

// Пропуски kill_guard из тикета kill-guard-gaps-4: pkill --inverse, алиасы
// юнитов, сокет службы сессии, сеанс logind, {…} в юните, сокращения tmux
// kill-*, стоп plasmashell; тесты на выживших мутантов kill-guard-gaps-3
func TestKillGuardGaps4Blocks(t *testing.T) {
	for _, command := range []string{
		// pkill --inverse: всем, кроме совпавших
		`pkill --inverse zzzz`,
		`pkill --inverse -x my_worker`,
		`pkill -9 --inverse -u aisee zzzz`,
		`pkill --inverse -f '^/usr/bin/sleep'`,
		// алиас защищённого юнита
		`sudo systemctl restart display-manager`,
		`sudo systemctl stop display-manager.service`,
		`sudo systemctl restart dbus`,
		`systemctl --user restart dbus`,
		`systemctl --user kill dbus.service`,
		// сокет, который требует служба сессии: со стопом сокета гаснет она
		`systemctl --user stop dbus.socket`,
		`sudo systemctl stop dbus.socket`,
		`systemctl --user stop pipewire.socket`,
		// сеанс logind — как loginctl terminate-session
		`sudo systemctl stop session-3.scope`,
		`sudo systemctl kill session-c2.scope`,
		`systemctl stop session-3.scope`,
		// {…} в имени юнита оболочка раскроет в несколько юнитов
		`systemctl --user stop pipewire{,-pulse}.{service,socket}`,
		`systemctl --user stop ap{p,}.slice`,
		// сокращения tmux kill-*
		`tmux kill-ser`,
		`tmux kill-ses`,
		`tmux kill-serv`,
		`tmux kill-w`,
		`tmux kill-p -a -t x`,
		`tmux new -d -s x \; kill-ser`,
		// plasmashell: стоп, kill, заморозка и disable --now — отказ;
		// рестарт kwin и среза с plasmashell — тоже
		`systemctl --user stop plasma-plasmashell`,
		`systemctl --user kill plasma-plasmashell.service`,
		`systemctl --user freeze plasma-plasmashell`,
		`systemctl --user disable --now plasma-plasmashell`,
		`systemctl --user restart plasma-kwin_wayland`,
		`systemctl --user restart session.slice`,
		`systemctl --user restart plasma-plasmashell agents.slice`,
		// мутанты kill-guard-gaps-3: -s у loginctl берёт значение
		`loginctl -s 9 kill-user aisee`,
		`loginctl --signal 9 terminate-user aisee`,
		// слова за kill в позиционных параметрах: сигнал из переменной
		`set -- kill -s $S 455274; $1 $2 $3 $4`,
		// сигнал в кавычках, но цель опасна, или раскрытие вне кавычек
		`kill -s "$SIG" -1`,
		`kill -s "$SIG" 2100`,
		`kill -s "$S"$T 455274`,
		`kill -s "$@" 455274`,
		`kill -s \"$S\" 455274`,
		`kill -s '"'$S'"' 455274`,
		`tmux killw`,
		`kill -s "${a[@]}" 455274`,
		"kill -s `echo 9` 455274",
	} {
		if validateKill(t, command).IsValid {
			t.Errorf("должно блокироваться: %s", command)
		}
	}
}

// Соседи: узкая цель, рестарт plasmashell, справка, неоднозначное
// сокращение tmux (tmux его не исполнит), сигнал одним словом в кавычках
func TestKillGuardGaps4Allows(t *testing.T) {
	for _, command := range []string{
		`pkill --inverse --signal 0 zzzz`,
		`pkill -x sleep`,
		// -x — шаблон целиком: konsol|zzz не совпадает с konsole
		`pkill -x 'konsol|zzz'`,
		// -TERM у fuser — сигнал, не связка -T -E -R -M
		`fuser -k -TERM 8080/tcp`,
		// рестарт plasmashell сессию не гасит
		`systemctl --user restart plasma-plasmashell`,
		`systemctl --user try-restart plasma-plasmashell.service`,
		`systemctl --user reload plasma-plasmashell`,
		`systemctl --user reload-or-restart plasma-plasmashell`,
		`systemctl --user condrestart plasma-plasmashell`,
		// сокет и алиас без службы сессии за ними
		`systemctl --user stop pipewire-pulse.socket`,
		`systemctl --user stop foo.socket`,
		`systemctl --user stop display-manager`,
		`sudo systemctl stop dbus-org.example.service`,
		`systemctl --user status session-3.scope`,
		`sudo systemctl status display-manager`,
		`tmux kill-ses -t scratch`,
		`tmux kill-w -t foo:1`,
		`tmux kill-s`,
		`tmux kill -t x`,
		`kill -s "$SIG" 455274`,
		`sudo kill -s "$SIG" 455274`,
		`kill -n "${S}" 455274`,
		`kill --signal "$S" 455274`,
		`kill -s x"$S" 455274`,
		`set -- kill -s TERM 455274; $1 $2 $3 $4`,
	} {
		if r := validateKill(t, command); !r.IsValid {
			t.Errorf("не должно блокироваться: %s: %v", command, r.Suggestions)
		}
	}
}

// addFakeProc добавляет процесс в поддельный /proc сторожа
func addFakeProc(t *testing.T, tool *KillGuardTool, pid, ppid, comm, cgroup string) {
	t.Helper()
	dir := filepath.Join(tool.procRoot, pid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"stat":    pid + " (" + comm + ") S " + ppid + " 1 1 0",
		"cmdline": comm + "\x00",
		"cgroup":  cgroup + "\n",
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// Рестарт сберегает plasmashell только в его юните и только не предка
// сессии: юнит, где идёт предок, и срез с plasmashell — отказ
func TestKillGuardPlasmashellRestart(t *testing.T) {
	for _, tc := range []struct{ name, command, want string }{
		{"предок в юните plasmashell", `systemctl --user restart plasma-plasmashell`, "предок этой сессии"},
		{"plasmashell в срезе", `systemctl --user restart other.slice`, "plasmashell (PID 3060)"},
	} {
		tool := newKillGuard(t)
		addFakeProc(t, tool, "3050", "3000", "bash", killGuardCgroups[2100])
		addFakeProc(t, tool, "3060", "1500", "plasmashell", "0::/user.slice/user-1000.slice/user@1000.service/other.slice/x.service")
		if tc.want == "предок этой сессии" {
			tool.selfPID = 3050
		}
		r, err := tool.ValidateTool(context.Background(), &core.ToolInput{ToolName: "Bash", Command: tc.command})
		if err != nil {
			t.Fatalf("ValidateTool: %v", err)
		}
		if r.IsValid || !strings.Contains(strings.Join(r.Suggestions, "\n"), tc.want) {
			t.Errorf("%s: ждали отказ с «%s», получили %v %q", tc.name, tc.want, r.IsValid, r.Suggestions)
		}
	}
}

// Причины называют настоящий юнит за алиасом и сокетом, сеанс logind,
// --inverse; подсказка своя у systemctl, tmux и fuser, обычная — у прочих
func TestKillGuardGaps4Reasons(t *testing.T) {
	for _, tc := range []struct{ command, want string }{
		{`pkill --inverse zzzz`, "--inverse"},
		{`systemctl --user stop dbus.socket`, "за ним dbus-broker.service: в юните идёт dbus-broker (PID 3200)"},
		{`sudo systemctl restart dbus`, "за ним dbus-broker.service: в юните идёт dbus-broker (PID 3400)"},
		{`sudo systemctl restart display-manager`, "за ним sddm.service"},
		{`systemctl --user stop pipewire.socket`, "за ним pipewire.service"},
		{`sudo systemctl stop session-3.scope`, "сеанс logind"},
		{`systemctl --user stop ap{p,}.slice`, "{…}"},
		{`tmux kill-ser`, "tmux kill-server"},
		{`tmux kill-ses`, "tmux kill-session без -t"},
		{`systemctl --user restart plasma-kwin_wayland`, "kwin_wayland (PID 3500)"},
	} {
		r := validateKill(t, tc.command)
		if r.IsValid || !strings.Contains(strings.Join(r.Suggestions, "\n"), tc.want) {
			t.Errorf("%s: ждали отказ с «%s», получили %v %q", tc.command, tc.want, r.IsValid, r.Suggestions)
		}
	}
	for _, tc := range []struct{ command, hint string }{
		{`systemctl --user stop agents.slice`, systemctlHint("systemctl --user", true)},
		{`sudo systemctl stop session-3.scope`, systemctlHint("systemctl", true)},
		{`tmux kill-server`, toolHints["tmux"]},
		{`fuser -k .`, toolHints["fuser"]},
	} {
		got := validateKill(t, tc.command).Suggestions
		if !slices.Contains(got, tc.hint) || slices.Contains(got, killGuardHint) {
			t.Errorf("%s: ждали только подсказку %q, получили %q", tc.command, tc.hint, got)
		}
	}
	if direct := validateKill(t, `systemctl --user stop agents.slice`).Suggestions; strings.Contains(strings.Join(direct, "\n"), "за ним") {
		t.Errorf("юнит сессии назван прямо: «за ним» лишнее, получили %q", direct)
	}
	mixed := validateKill(t, `kill 1; tmux kill-server; tmux kill-session`).Suggestions
	if want := []string{killGuardHint, toolHints["tmux"]}; !slices.Equal(mixed[len(mixed)-2:], want) {
		t.Errorf("kill и tmux: ждали подсказки %q по разу, получили %q", want, mixed)
	}
	if plain := validateKill(t, `loginctl terminate-user aisee`).Suggestions; !slices.Contains(plain, killGuardHint) {
		t.Errorf("loginctl: ждали обычную подсказку, получили %q", plain)
	}
}
