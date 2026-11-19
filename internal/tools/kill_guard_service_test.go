package tools

import (
	"slices"
	"testing"
)

// Тикет kill-guard-service-exec-properties, заход 4: команда Exec-свойства —
// отдельный вызов под службой, --shell — systemd-run --pipe bash. Формы
// обходов из приёмки ac3fde8e: служба исполнит сигнал — отказ
func TestKillGuardServiceExecBypasses(t *testing.T) {
	for _, command := range []string{
		// *Ex
		`systemd-run --user -p 'ExecStartPreEx=/usr/bin/kill 12345' true`,
		`systemd-run --user -p 'ExecStopPostEx=/usr/bin/kill 12345' true`,
		`systemd-run --user -p 'ExecConditionEx=/usr/bin/kill 12345' true`,
		`run0 '--property=ExecStopPostEx=/usr/bin/kill 12345' true`,
		// @: второе слово — argv[0]
		`systemd-run --user -p 'ExecStartPre=@/usr/bin/env x kill 12345' true`,
		`systemd-run --user -p "ExecStartPre=@/bin/sh x -c 'kill 12345'" true`,
		`systemd-run --user -p 'ExecStartPre=@/usr/bin/env x pkill -u aisee' true`,
		`run0 '--property=ExecStartPre=@/usr/bin/env x kill 12345' true`,
		// | — оболочка -c со словами через пробел, | без команды — оболочка с ввода
		`systemd-run --user -p 'ExecStartPre=|echo a; kill 12345' true`,
		`systemd-run --user -p 'ExecStartPre=@|kill 12345' true`,
		`systemd-run --user --pipe -p 'ExecStartPre=|' true <<< 'kill 12345'`,
		// # — литерал systemd
		`systemd-run --user -p 'ExecStartPre=/usr/bin/env -u #x /usr/bin/kill 12345' true`,
		`run0 '--property=ExecStartPre=/usr/bin/env -u #x /usr/bin/kill 12345' true`,
		// окружение службы
		`systemd-run --user -E 'CMD=kill 12345' -p 'ExecStartPre=/bin/sh -c ${CMD}' true`,
		`systemd-run --user -E K=/usr/bin/kill -p 'ExecStartPre=/usr/bin/env ${K} 12345' true`,
		`systemd-run --user --setenv='CMD=pkill -u aisee' -p 'ExecStartPre=/bin/sh -c ${CMD}' true`,
		`systemd-run --user -E 'CMD=kill 1' -p 'ExecStartPre=/bin/sh -c $CMD' true`,
		// C-экранирование при kill в тексте
		`systemd-run --user -p 'ExecStartPre=/usr/bin/\x6bill 12345' true # kill`,
		`systemd-run --user -p 'ExecStartPre=/bin/sh -c "kill\s1"' true`,
		// свойство из ввода xargs
		`printf '%s\n' 'ExecStartPre=/usr/bin/kill 12345' | xargs -I{} systemd-run --user -p {} true`,
		`printf '%s\n' '/usr/bin/kill 12345' | xargs -I@ systemd-run --user -p ExecStartPre=@ true`,
		`echo -p ExecStartPre=/usr/bin/kill true | xargs systemd-run --user`,
		// --shell
		"systemd-run --user --shell <<'EOF'\nkill 12345\nEOF",
		"systemd-run --user -S <<EOF\npkill -u aisee\nEOF",
		`systemd-run --user --shell <<< $'\x6bill 12345'`,
		`systemd-run --user -S < <(echo kill 12345)`,
		`f() { systemd-run --user --shell; }; f <<< 'kill 12345'`,
		`bash -c "systemd-run --user -S <<< 'kill 12345'"`,
		`systemd-run --user -qS -- <<< 'kill 12345'`,
		// связку со значением в кавычках в тексте не переписать — текстом
		// systemdRunShellCommand
		`systemd-run --user '-Sp X=a b' <<< 'kill 12345'`,
		`systemd-run --user '-Sp ExecStartPre=/usr/bin/kill 12345'`,
		// systemd-run под systemd-run: свойства внутреннего — слова внешнего
		`systemd-run --user systemd-run --user -p 'ExecStartPre=/usr/bin/kill 12345' true`,
		`systemd-run --user --scope systemd-run --user -p 'ExecStartPre=/usr/bin/kill 12345' true`,
		`systemd-run --user env systemd-run --user -p 'ExecStartPre=/usr/bin/kill 12345' true`,
		`systemd-run --user timeout 60 systemd-run --user -p 'ExecStartPre=/usr/bin/kill 12345' true`,
		// заход 5: --shell у systemd-run, которого запускает служба с вводом
		`systemd-run --user --pipe -- systemd-run --user -S <<< 'kill 12345'`,
		`systemd-run --user -P systemd-run --user --shell <<< 'kill 12345'`,
		`systemd-run --user --pipe env systemd-run --user -S <<< 'kill 12345'`,
		`systemd-run --user --pipe timeout 60 systemd-run --user -S <<< 'kill 12345'`,
		`echo 'kill 12345' | systemd-run --user --pipe -- systemd-run --user -S`,
		`sudo systemd-run --pipe systemd-run -S <<< 'kill 12345'`,
		`run0 systemd-run --user -S <<< 'kill 12345'`,
		`run0 --pipe systemd-run -S <<< 'kill 12345'`,
		`systemd-run --user -qP systemd-run --user -P systemd-run --user -qS <<< 'kill 12345'`,
		// слова команды службы не разобрать (не UTF-8) — отказ при kill
		`systemd-run --user -P env A=$'\xff' systemd-run -S <<< 'kill 1'`,
		// $VAR словом целиком — значение переменной
		`systemd-run --user -E 'CMD=kill 1' -p "ExecStartPre=/bin/sh -c '\$CMD'" true`,
	} {
		if validateKill(t, command).IsValid {
			t.Errorf("должно блокироваться: %s", command)
		}
	}
}

// Свойства без вычисляемой Exec-команды и службы, которые сигнал не
// исполнят здесь, — пропуск, как на main
func TestKillGuardServiceExecAllowed(t *testing.T) {
	for _, command := range []string{
		`systemd-run --user -p "MemoryMax=$MEM" make # pkill later`,
		`systemd-run --user -p "Environment=A=$A" make # kill`,
		`systemd-run --user -H host -p 'ExecStartPre=/usr/bin/kill 12345' true`,
		`systemd-run -M box -p 'ExecStartPre=/usr/bin/kill 12345' true`,
		`systemd-run --user --scope -p 'ExecStartPre=/usr/bin/kill 12345' make`,
		`systemd-run --user -p 'ExecStopPost=/usr/bin/echo killed' make`,
		`systemd-run --user -p 'ExecStopPost=/usr/bin/echo killed ${X} 100%' make`,
		`systemd-run --user -p 'ExecStartPre=/usr/bin/env x kill 12345' true`,
		`systemd-run --user --shell <<< 'make test'`,
		`echo make | systemd-run --user --shell`,
		// имя команды — первый операнд после опций с сокращением (--sete = --setenv)
		`systemd-run --user --sete X=1 "$(go env GOPATH)/bin/tool"`,
		`printf '%s\n' 1G | xargs -I{} systemd-run --user -p MemoryMax={} make # kill`,
		`run0 --property=Nice=5 dnf update`,
		`systemd-run --user -p MemoryMax=1G make`,
		// заход 5: $VAR внутри слова systemd не раскрывает — оболочка службы
		// получит текст $VAR
		`systemd-run --user -p 'ExecStopPost=/bin/sh -c "echo $SERVICE_RESULT >> /tmp/r"' make`,
		`systemd-run --user -E 'CMD=kill 1' -p 'ExecStartPre=/usr/bin/echo x$CMD' make`,
		`systemd-run --user --pipe -- systemd-run --user -S <<< 'make test'`,
	} {
		if r := validateKill(t, command); !r.IsValid {
			t.Errorf("должно пропускаться: %s: %v", command, r)
		}
	}
}

// Командная строка Exec-свойства текстом оболочки (systemdExecCommand)
func TestSystemdExecCommand(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		ok       bool
	}{
		{`/usr/bin/kill 12345`, `'/usr/bin/kill' '12345'`, true},
		{`-+!:/usr/bin/kill 1`, `'/usr/bin/kill' '1'`, true},
		{`@/usr/bin/env x kill 1`, `'/usr/bin/env' 'kill' '1'`, true},
		{`@/usr/bin/env`, ``, false},
		{`|echo a; kill 1`, `sh -c 'echo a; kill 1'`, true},
		{`@|kill 1`, `sh -c 'kill 1'`, true},
		{`|`, `sh`, true},
		{`/bin/sh -c 'kill 1' "a b"`, `'/bin/sh' '-c' 'kill 1' 'a b'`, true},
		{`/usr/bin/echo a"b c"d`, `'/usr/bin/echo' 'ab cd'`, true},
		{`/usr/bin/env -u #x kill`, `'/usr/bin/env' '-u' '#x' 'kill'`, true},
		{`/bin/sh -c ${CMD}`, `'/bin/sh' '-c' "${CMD}"`, true},
		{`/usr/bin/env ${K}ll $X $$Y 1%`, `'/usr/bin/env' "${K}"'ll' "${X}" '$Y' '1%'`, true},
		{`:/usr/bin/echo ${A}`, `'/usr/bin/echo' '${A}'`, true},
		{`/usr/bin/echo $$$A ${A}$B`, `'/usr/bin/echo' '$$A' "${A}"'$B'`, true},
		{`/bin/sh -c "echo $R >> /tmp/r"`, `'/bin/sh' '-c' 'echo $R >> /tmp/r'`, true},
		{`/usr/bin/echo x$A "$A" '$B' $A.y $ $$A e${A}f`, `'/usr/bin/echo' 'x$A' "${A}" "${B}" "${SYSTEMD_ENV}" "${SYSTEMD_ENV}" '$A' 'e'"${A}"'f'`, true},
		{`/usr/bin/echo ${a-b}`, `'/usr/bin/echo' "${SYSTEMD_ENV}"`, true},
		{`/usr/bin/echo ''`, `'/usr/bin/echo' ''`, true},
		{`/usr/bin/echo 'a`, ``, false},
		{``, ``, true},
		{`-`, ``, true},
	} {
		got, ok := systemdExecCommand(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("systemdExecCommand(%q) = %q, %v; ждали %q, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// Ключ и значение свойства: вычисляемые — отказ только у ключа и Exec-значения
func TestExecPropertyCommand(t *testing.T) {
	none := func(string) bool { return false }
	braces := func(w string) bool { return slices.Contains([]string{"{}", "ExecStartPre={}"}, w) }
	for _, tc := range []struct {
		arg       string
		fed       func(string) bool
		inner     string
		hasReason bool
	}{
		{`ExecStartPre=/usr/bin/kill 1`, none, `'/usr/bin/kill' '1'`, false},
		{`ExecStopPostEx=/usr/bin/kill 1`, none, `'/usr/bin/kill' '1'`, false},
		{`MemoryMax=1G`, none, ``, false},
		{`"MemoryMax=$MEM"`, none, ``, false},
		{`"$P"`, none, ``, true},
		{`"Exec${X}=1"`, none, ``, true},
		{`"ExecStartPre=$X"`, none, ``, true},
		{`ExecStartPre=/usr/bin/\x6bill 1`, none, ``, true},
		{`ExecStartPre=/usr/bin/echo 'a`, none, ``, true},
		{`{}`, braces, ``, true},
		{`ExecStartPre={}`, braces, ``, true},
		{`ExecStartPre=`, none, ``, false},
		{`ExecStopPre=/usr/bin/kill 1`, none, ``, false},
	} {
		inner, reason := execPropertyCommand(tc.arg, tc.fed)
		if inner != tc.inner || (reason != "") != tc.hasReason {
			t.Errorf("execPropertyCommand(%q) = %q, %q; ждали %q, причина %v", tc.arg, inner, reason, tc.inner, tc.hasReason)
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
	} {
		levels := systemdRunLevels(tc.args)
		var at []int
		for i := range levels {
			at = append(at, levels[i].at)
		}
		last := &levels[len(levels)-1]
		if !slices.Equal(at, tc.at) || (last.err != nil) != tc.err {
			t.Errorf("systemdRunLevels(%q): места %v, ошибка %v; ждали %v, %v", tc.args, at, last.err, tc.at, tc.err)
		}
	}
}
