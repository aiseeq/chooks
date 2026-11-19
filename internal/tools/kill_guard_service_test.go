package tools

import (
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Значение Exec с C-экранированием или вычислениями может скрыть команду от killText.
func TestKillGuardServiceExecBypasses(t *testing.T) {
	for _, command := range []string{
		`systemd-run --user -p 'ExecStartPre=/usr/bin/kill 1' true`,
		`systemd-run --user -p 'ExecStartPre=/bin/sh -c "podman stop x"' true`,
		`systemd-run --user -p 'ExecStartPre=/usr/bin/\x6bill 1' true`,
		`systemd-run --user -p 'ExecStartPre=/usr/bin/\153ill 1' true`,
		`systemd-run --user -p 'ExecStartPre=/bin/sh -c "systemctl --user stop $U"' true`,
		`systemd-run --user -p 'ExecStartPre=/bin/sh -c "loginctl terminate-user $U"' true`,
		`systemd-run --user -p 'ExecStartPre=/bin/sh -c "fuser -k $F"' true`,
		`systemd-run --user -p 'ExecStartPre=/bin/sh -c "podman stop $C"' true`,
		`systemd-run --user -p 'ExecStartPre=/bin/sh -c "podman rm -f $C"' true`,
		`systemd-run --user -p 'ExecStartPre=/bin/sh -c "podman stop -a; echo $X"' true`,
		`P='ExecStartPre=/usr/bin/kill 1'; systemd-run --user -p "$P" true`,
		`systemd-run --user -p 'ExecStartPre=$Y' true`,
		`C=/usr/bin/kill; systemd-run --user -p "ExecStartPre=$C 1" true`,
		`systemd-run --user -p "$(printf '/usr/bin/kill') 1" true`,
		`systemd-run --user -p "$(cat f)" true`,
		`systemd-run --user -p "Exec${X}=1" true`,
		`printf '%s\n' 'ExecStartPre=/usr/bin/kill 1' | xargs -I{} systemd-run --user -p {} true`,
		`printf '%s\n' 'ExecStartPre=/usr/bin/kill 1' | parallel systemd-run --user -p {}`,
		`run0 --property="$P" true`,
		`systemd-run --user -E K=st podman '${K}op' -a`,
		`run0 --setenv=K=ki podman '${K}ll' -a`,
		`systemd-run --user systemctl '${K}ll' foo.service`,
		`systemd-run --user loginctl '${K}ll-user' aisee`,
		`systemd-run --user sh -c '${K}ll 1'`,
		`systemd-run --user --setenv=K=ki env '${K}ll' 1`,
		`setsid nice systemd-run --user -p 'ExecStartPre=/usr/bin/kill 1' true`,
		`setsid -f systemd-run --user -p 'ExecStartPre=/usr/bin/\x6bill 1' true`,
		`flock /tmp/l systemd-run --user -p 'ExecStartPre=/usr/bin/\x6bill 1' true`,
		`chrt -o 0 systemd-run --user -p 'ExecStartPre=/usr/bin/\x6bill 1' true`,
		`setsid nice systemd-run --user -p 'ExecStartPre=/usr/bin/\x6bill 1' true`,
		`run0 --property='ExecStartPre=/usr/bin/\x6bill 1' true`,
		`run0 --setenv=K=ki --property='ExecStartPre=/bin/sh -c "${K}ll 1"' true`,
		// подстановка systemd в команде вложенного systemd-run
		`systemd-run --user --scope systemd-run --user -E K=st podman '${K}op' -a`,
		`systemd-run --user --pipe -- systemd-run --user -E K=st podman '${K}op' -a`,
		// --expand-environment — boolean systemd: истинное значение раскрывает
		`systemd-run --user --expand-environment=yes -E K=st podman '${K}op' -a`,
		`systemd-run --user --expand-environment=On -E K=st podman '${K}op' -a`,
		// обёртки systemd-run
		`setsid systemd-run --user -p 'ExecStartPre=/usr/bin/kill 1' true`,
		`setsid -f systemd-run --user -p 'ExecStartPre=/usr/bin/kill 1' true`,
		`flock /tmp/l systemd-run --user -p 'ExecStartPre=/usr/bin/kill 1' true`,
		`chrt 1 systemd-run --user -p 'ExecStartPre=/usr/bin/kill 1' true`,
		`echo x; setsid systemd-run --user -p 'ExecStartPre=/usr/bin/\x6bill 1' true`,
		`flock /tmp/systemd-run systemd-run --user -p 'ExecStartPre=/usr/bin/\x6bill 1' true`,
	} {
		if r := validateKill(t, command); r.IsValid {
			t.Errorf("должно блокироваться: %s", command)
		}
	}
}

func TestKillGuardWrappedSystemdRunLists(t *testing.T) {
	for i, tc := range []struct {
		command string
		deny    bool
	}{
		{`setsid systemd-run --user -p 'ExecStartPre=/usr/bin/\x6bill 1' true && echo done`, true},
		{`setsid systemd-run --user -p 'ExecStartPre=/usr/bin/\x6bill 1' true || echo done`, true},
		{`setsid systemd-run --user -p 'ExecStartPre=/usr/bin/\x6bill 1' true | cat`, true},
		{`true && setsid systemd-run --user -p 'ExecStartPre=/usr/bin/\x6bill 1' true`, true},
		{`flock /tmp/l systemd-run --user -p 'ExecStartPre=/usr/bin/\x6bill 1' true && echo done`, true},
		{`nice systemd-run --user -p 'ExecStartPre=/usr/bin/\x6bill 1' true && echo done`, true},
		{`setsid systemd-run --user -E "X=$X" -p 'ExecStartPre=/usr/bin/\x6bill 1' true && echo done`, true},
		{`setsid systemd-run --user -E K=st podman '${K}op' -a && echo done`, true},
		{`setsid systemd-run --user -p 'ExecStartPre=/usr/bin/kill 1' true && echo done`, true},
		{`setsid systemd-run --user make && echo done`, false},
		// слово, ломающее склейку слов: проверка по исходным словам
		{`setsid systemd-run --user -p 'ExecStartPre=/usr/bin/\x6bill 1' make "a\"b" && echo done`, true},
		{`setsid systemd-run --user -p 'ExecStartPre=/usr/bin/\x6bill 1' make "$X" "a(b" | cat`, true},
		{`setsid systemd-run --user make "a\"b" && echo skills`, false},
		{"setsid systemd-run --user make \"a\\`b\" && echo skills", false},
		{`setsid systemd-run --user make $'a"b' && echo skills`, false},
		{`flock /tmp/l systemd-run --user make | cat`, false},
		{`setsid systemd-run --user -E "X=$X" make && echo done`, false},
	} {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			if r := validateKill(t, tc.command); r.IsValid == tc.deny {
				t.Errorf("deny=%v: %v", tc.deny, r)
			}
		})
	}
}

func TestKillGuardServiceExecAllowed(t *testing.T) {
	for _, command := range []string{
		`systemd-run --user -p 'ExecStartPre=/usr/bin/true' make`,
		`systemd-run --user -p MemoryMax=1G make`,
		`systemd-run --user -p 'Environment=A=\x41' make`,
		`systemd-run --user -p 'Environment=A=$A' make`,
		`systemd-run --user --expand-environment=no podman '${K}op' -a`,
		`systemd-run --user --expand-environment=false -E K=st podman '${K}op' -a`,
		`systemd-run --user --expand-environment=0 -E K=st podman '${K}op' -a`,
		`systemd-run --user --expand-environment=OFF -E K=st podman '${K}op' -a`,
		// run0 без короткой -p (systemd 259: invalid option) ничего не запустит
		`run0 -p 'ExecStartPre=/usr/bin/\x6bill 1' true`,
		// systemd-run операндом echo — не обёрнутый systemd-run
		`echo systemd-run -p 'ExecStartPre=/usr/bin/\x74rue' true; systemd-run --user make`,
		`setsid echo systemd-run -p 'ExecStartPre=/usr/bin/\x74rue' true; setsid systemd-run --user make`,
		`systemd-run --user echo '${X}'`,
		`systemd-run --user make '${X}'`,
		`systemd-run --user sh -c "$HOME/bin/tool"`,
		`run0 sh -c "$HOME/bin/tool"`,
		`run0 --property=Nice=5 dnf update`,
		`systemd-run --user --scope -p 'ExecStartPre=/usr/bin/true' make`,
	} {
		if r := validateKill(t, command); !r.IsValid {
			t.Errorf("должно пропускаться: %s: %v", command, r)
		}
	}
}

func TestSystemdExecCommand(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`/usr/bin/kill 1`, `'/usr/bin/kill' '1'`},
		{`@/usr/bin/env x kill 1`, `'/usr/bin/env' 'kill' '1'`},
		{`|echo a; kill 1`, `sh -c 'echo a; kill 1'`},
		{`/bin/sh -c 'kill 1' "a b"`, `'/bin/sh' '-c' 'kill 1' 'a b'`},
		{`/usr/bin/echo a"b c"d`, `'/usr/bin/echo' 'ab cd'`},
	} {
		got, ok := systemdExecCommand(tc.in)
		if got != tc.want || !ok {
			t.Errorf("systemdExecCommand(%q) = %q, %v; ждали %q", tc.in, got, ok, tc.want)
		}
	}
}

func TestExecPropertyCommand(t *testing.T) {
	none := func(string) bool { return false }
	fed := func(w string) bool { return strings.Contains(w, "{}") }
	for _, tc := range []struct {
		arg, inner string
		fed        func(string) bool
		deny       bool
	}{
		{`ExecStartPre=/usr/bin/kill 1`, `'/usr/bin/kill' '1'`, none, false},
		{`MemoryMax=1G`, ``, none, false},
		{`ExecStartPre=/usr/bin/\x6bill 1`, ``, none, true},
		{`ExecStartPre=/bin/sh -c '$X'`, ``, none, true},
		{`"Exec${X}=1"`, ``, none, true},
		{`ExecStartPre={}`, ``, fed, true},
	} {
		inner, reason, deny := execPropertyCommand(tc.arg, tc.fed)
		if inner != tc.inner || (reason != "") != tc.deny || deny != tc.deny {
			t.Errorf("execPropertyCommand(%q) = %q, %q, %v", tc.arg, inner, reason, deny)
		}
	}
}

// --shell/-S в тексте команды — --pipe/-P и -- bash в конце
func TestRewriteSystemdRunShell(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`systemd-run --user --shell <<< 'kill 1'`, `systemd-run --user --pipe -- bash <<< 'kill 1'`},
		{`systemd-run --user -qS <<< x`, `systemd-run --user -qP -- bash <<< x`},
		{`systemd-run --user --sh --`, `systemd-run --user --pipe -- bash`},
		{`systemd-run --user "-S" -q`, `systemd-run --user -P -q -- bash`},
		{`echo a | systemd-run -S; systemd-run --scope -S`, `echo a | systemd-run -P -- bash; systemd-run --scope -P -- bash`},
		{`sh -c 'systemd-run -S'`, `sh -c 'systemd-run -P -- bash'`},
		// systemd-run в команде службы: переписывается --shell внутреннего
		{`systemd-run --user --pipe -- systemd-run --user -S <<< x`, `systemd-run --user --pipe -- systemd-run --user -P -- bash <<< x`},
		{`systemd-run -P env A=1 systemd-run -qS`, `systemd-run -P env A=1 systemd-run -qP -- bash`},
		{`systemd-run -P timeout 60 systemd-run --shell --`, `systemd-run -P timeout 60 systemd-run --pipe -- bash`},
		{`systemd-run -S systemd-run make`, `systemd-run -S systemd-run make`},
		// не переписывается: команда при --shell (systemd-run её не запустит),
		// -p с S в значении, нет --shell
		{`systemd-run -S kill 1`, `systemd-run -S kill 1`},
		{`systemd-run -pS true`, `systemd-run -pS true`},
		{`systemd-run --user make`, `systemd-run --user make`},
	} {
		if got, _ := rewriteSystemdRunShell(tc.in); got != tc.want {
			t.Errorf("rewriteSystemdRunShell(%q) = %q, ждали %q", tc.in, got, tc.want)
		}
	}
}

// systemd-run --shell текстом для разбора, когда его нельзя переписать в
// команде: -- не повторяется, иначе bash стал бы аргументом команды --
func TestSystemdRunShellCommand(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
		ok   bool
	}{
		{[]string{"--user", "-Sp X=a b"}, `systemd-run '--user' '-Pp X=a b' '--' 'bash'`, true},
		{[]string{"--shell", "--"}, `systemd-run '--pipe' '--' 'bash'`, true},
		{[]string{"-S", "kill", "1"}, ``, false},
	} {
		if got, ok := systemdRunShellCommand(tc.args); got != tc.want || ok != tc.ok {
			t.Errorf("systemdRunShellCommand(%q) = %q, %v; ждали %q, %v", tc.args, got, ok, tc.want, tc.ok)
		}
	}
}

// Короткие опции systemd-run по getopt v259 "+hrC:H:M:E:p:tTPqvGdSu:"
func TestParseSystemdRunShortOptions(t *testing.T) {
	for _, tc := range []struct {
		args          []string
		shell, scope  bool
		start, letter int
	}{
		{[]string{"-qS"}, true, false, 1, 2},
		{[]string{"-GS"}, true, false, 1, 2},
		{[]string{"-P", "bash"}, false, false, 1, 0},
		{[]string{"-tT", "--scope", "make"}, false, true, 2, 0},
		{[]string{"-pS", "true"}, false, false, 1, 0},
		{[]string{"--shel"}, true, false, 1, -1},
	} {
		o := parseSystemdRunOptions(plainWords(tc.args))
		if o.shell != tc.shell || o.scope != tc.scope || o.start != tc.start || o.shell && o.shellLetter != tc.letter {
			t.Errorf("parseSystemdRunOptions(%q) = %+v", tc.args, o)
		}
	}
}

func TestXargsReplaceString(t *testing.T) {
	for _, tc := range []struct {
		opts []string
		want string
	}{
		{[]string{"-I{}"}, "{}"},
		{[]string{"-I", "@"}, "@"},
		{[]string{"-0", "-i"}, "{}"},
		{[]string{"-iXX"}, "XX"},
		{[]string{"--replace"}, "{}"},
		{[]string{"--replace=%"}, "%"},
		{[]string{"-0"}, ""},
	} {
		if got := xargsReplaceString(tc.opts); got != tc.want {
			t.Errorf("xargsReplaceString(%q) = %q, ждали %q", tc.opts, got, tc.want)
		}
	}
}

// systemd-run в команде службы: прямо, за обёртками; в скрипте и за ssh —
// нет; слова, которые не разобрать, — ошибка уровня
func TestSystemdRunLevels(t *testing.T) {
	var deepest []int // места уровней -P systemd-run … на пределе
	for i := range maxRun0Rewrites + 1 {
		deepest = append(deepest, 2*i)
	}
	for _, tc := range []struct {
		args []string
		at   []int
		err  bool
	}{
		{[]string{"--user", "make"}, []int{0}, false},
		{[]string{"--pipe", "--", "systemd-run", "-S"}, []int{0, 3}, false},
		{[]string{"-P", "env", "A=1", "systemd-run", "-q", "systemd-run", "-S"}, []int{0, 4, 6}, false},
		{[]string{"-P", "timeout", "60", "systemd-run", "-S"}, []int{0, 4}, false},
		{[]string{"-P", "sh", "-c", "systemd-run -S"}, []int{0}, false},
		{[]string{"-P", "ssh", "h", "systemd-run", "-S"}, []int{0}, false},
		{[]string{"-P", "env", "A=\xff", "systemd-run", "-S"}, []int{0}, true},
		{[]string{"-P", "setsid", "-w", "systemd-run", "-S"}, []int{0, 4}, false},
		{[]string{"-P", "flock", "/tmp/x", "systemd-run", "-S"}, []int{0, 4}, false},
		{[]string{"-P", "echo", "systemd-run", "-S"}, []int{0}, false},
		{[]string{"-P", "systemd-cat", "systemd-run", "-S"}, []int{0}, false},
		{[]string{"-p", "Description=systemd-run", "make"}, []int{0}, false},
		{[]string{"-P", "flock", "systemd-run", "systemd-run", "-S"}, []int{0, 4}, false},
		{strings.Fields(strings.Repeat("-P systemd-run ", maxRun0Rewrites) + "-S"), deepest, false},
		{strings.Fields(strings.Repeat("-P systemd-run ", maxRun0Rewrites+1) + "-S"), nil, true},
	} {
		levels := systemdRunLevels(tc.args)
		var at []int
		for i := range levels {
			at = append(at, levels[i].at)
		}
		last := &levels[len(levels)-1]
		if tc.at != nil && !slices.Equal(at, tc.at) || (last.err != nil) != tc.err {
			t.Errorf("systemdRunLevels(%q): места %v, ошибка %v; ждали %v, %v", tc.args, at, last.err, tc.at, tc.err)
		}
	}
}
