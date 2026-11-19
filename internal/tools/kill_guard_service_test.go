package tools

import (
	"slices"
	"strings"
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
		// Exec-значение само по себе может скрывать сигнал от killText
		`systemd-run --user -p 'ExecStartPre=/usr/bin/\x6bill 12345' true`,
		`systemd-run --user -p 'ExecStartPre=/usr/bin/\153ill 12345' true`,
		`systemd-run --user -p $'ExecStartPre=/usr/bin/\\x6bill 1' true`,
		`systemd-run --user -p 'ExecStartPre=/usr/bin/p\x6bill -u aisee' true`,
		`systemd-run --user -E K=ki -p 'ExecStartPre=/usr/bin/${K}ll 1' true`,
		// Вычисленные shell-значения и имена Exec-свойств отклоняются без killText
		`P='ExecStartPre=/usr/bin/\x6bill 1'; systemd-run --user -p "$P" true`,
		`C=/usr/bin/\x6bill; systemd-run --user -p "ExecStartPre=$C 1" true`,
		`systemd-run --user -p "$(printf '/usr/bin/\x6bill') 1" true`,
		`printf '%s\n' 'ExecStartPre=/usr/bin/\x6bill 1' | xargs -I{} systemd-run --user -p {} true`,
		`systemd-run --user -p "$(cat f)" true`,
		`run0 --property="$P" true`,
		`systemd-run --user -p "${P}=x" true`,
		// Подстановка может выбрать команду в аргументах launcher, главной команде или shell script
		`systemd-run --user -E K=ki -p 'ExecStartPre=/usr/bin/podman ${K}ll -a' true`,
		`systemd-run --user -E K=stop -p 'ExecStartPre=/usr/bin/podman $K -a' true`,
		`systemd-run --user -E K=r -p 'ExecStartPre=/usr/bin/podman ${K}m -fa' true`,
		`systemd-run --user -E K=ki -p 'ExecStartPre=/usr/bin/loginctl ${K}ll-user aisee' true`,
		`systemd-run --user -E K=ki -p 'ExecStartPre=/usr/bin/systemctl --user ${K}ll foo.service' true`,
		`systemd-run --user -E K=ki -p 'ExecStartPre=/bin/sh -c "${K}ll 1"' true`,
		`systemd-run --user -E K=ki -p 'ExecStartPre=/usr/bin/env ${K}ll 1' true`,
		`systemd-run --user -E K=ki -p 'ExecStartPre=|${K}ll 1' true`,
		`systemd-run --user -E K=ki -p 'ExecStartPre=@/bin/sh x -c "${K}ll 1"' true`,
		`systemd-run --user -E K=ki -p 'ExecStartPre=/usr/bin/env -S "${K}ll 1"' true`,
		`systemd-run --user -E A=ki -E B=ll -p 'ExecStartPre=/bin/sh -c "$A$B 1"' true`,
		`systemd-run --user -p Environment=K=ki -p 'ExecStartPre=/bin/sh -c "${K}ll 1"' true`,
		`systemd-run --user -p EnvironmentFile=/tmp/kgep-rv/envfile -p 'ExecStartPre=/bin/sh -c "${K} 1"' true`,
		`systemd-run --user -E K=st podman '${K}op' -a`,
		`systemd-run --user -E K=r podman '${K}m' -fa`,
		`systemd-run --user -E K=ki /bin/sh -c '${K}ll 1'`,
		`systemd-run --user sh -c '$HOME/bin/tool'`,
		`run0 --setenv=K=ki podman '${K}ll' -a`,
		`run0 --setenv=K=ki --property='ExecStartPre=/usr/bin/podman ${K}ll -a' true`,
		`run0 --property='ExecStartPre=/usr/bin/\x6bill 1' true`,
		`run0 -p 'ExecStartPre=/usr/bin/\x6bill 1' true`,
		`run0 --setenv=K=ki -p 'ExecStartPre=/bin/sh -c "${K}ll 1"' true`,
		`run0 --setenv=K=ki sh -c '${K}ll 1'`,
		`systemd-run --user -p 'ExecStartPre=/usr/bin/podman stop -a' true`,
		`systemd-run --user podman stop -a`,
		`setsid systemd-run --user -p 'ExecStartPre=/usr/bin/kill 1' true`,
		`setsid -f systemd-run --user -p 'ExecStartPre=/usr/bin/kill 1' true`,
		`flock /tmp/l systemd-run --user -p 'ExecStartPre=/usr/bin/kill 1' true`,
		`chrt -o 0 systemd-run --user -p 'ExecStartPre=/usr/bin/kill 1' true`,
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
		// заход 6: обёртки, которые снимает разбор, — те же, что у прямого kill
		`systemd-run --user --pipe setsid -w systemd-run --user -S <<< 'kill 12345'`,
		`systemd-run --user --pipe flock /run/user/1000/x systemd-run --user -S <<< 'kill 12345'`,
		`systemd-run --user --pipe chrt -o 0 systemd-run --user -S <<< 'kill 12345'`,
		`systemd-run --user --pipe taskset -c 0 systemd-run --user -S <<< 'kill 12345'`,
		`systemd-run --user --pipe unshare -U systemd-run --user -S <<< 'kill 12345'`,
		`systemd-run --user --pipe busybox env systemd-run --user -S <<< 'kill 12345'`,
		`systemd-run --user --pipe doas systemd-run -S <<< 'kill 12345'`,
		`systemd-run --user --pipe strace -f -o /dev/null systemd-run --user -S <<< 'kill 12345'`,
		`systemd-run --user setsid systemd-run --user -p 'ExecStartPre=/usr/bin/kill 12345' true`,
		`systemd-run --user flock /tmp/x systemd-run --user -p 'ExecStartPre=/usr/bin/kill 12345' true`,
		// вложенность глубже предела
		strings.Repeat("systemd-run --user --pipe ", maxRun0Rewrites+1) + `systemd-run --user -S <<< 'kill 12345'`,
		// слова команды службы не разобрать (не UTF-8) — отказ при kill
		`systemd-run --user -P env A=$'\xff' systemd-run -S <<< 'kill 1'`,
		// $VAR словом целиком — значение переменной
		`systemd-run --user -E 'CMD=kill 1' -p "ExecStartPre=/bin/sh -c '\$CMD'" true`,
	} {
		if r := validateKill(t, command); r.IsValid {
			t.Errorf("должно блокироваться: %s: %+v", command, r)
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
		`systemd-run --user -p 'ExecStartPre=/usr/bin/true' make`,
		`systemd-run --user -p 'Environment=A=\x41' make`,
		`systemd-run --user -E T=x -p 'ExecStartPre=/usr/bin/podman start ${T}' true`,
		`systemd-run --user -p 'ExecStartPre=/usr/bin/echo ${X}' true`,
		`systemd-run --user -p 'ExecStartPre=/usr/bin/make ${X}' true`,
		`systemd-run --user -p 'ExecStopPost=/bin/sh -c "echo ${SERVICE_RESULT} > /tmp/r"' make`,
		`systemd-run --user -p 'ExecStopPost=/bin/sh -c "notify-send done ${EXIT_STATUS}"' make`,
		`systemd-run --user -p 'ExecStopPost=|echo ${SERVICE_RESULT} > /tmp/r' make`,
		`systemd-run --user make ${X}`,
		`systemd-run --user -p 'ExecStopPost=/usr/bin/echo a\tb' make`,
		`systemd-run --user -p 'ExecStartPre=/bin/sh -c "printf a\\nb > /tmp/x"' make`,
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
func TestSystemdExecCommandInfo(t *testing.T) {
	for _, tc := range []struct {
		in, want     string
		computed, ok bool
	}{
		{`/usr/bin/kill 12345`, `'/usr/bin/kill' '12345'`, false, true},
		{`-+!:/usr/bin/kill 1`, `'/usr/bin/kill' '1'`, false, true},
		{`@/usr/bin/env x kill 1`, `'/usr/bin/env' 'kill' '1'`, false, true},
		{`@/usr/bin/env`, ``, false, false},
		{`|echo a; kill 1`, `sh -c 'echo a; kill 1'`, false, true},
		{`@|kill 1`, `sh -c 'kill 1'`, false, true},
		{`|`, `sh`, false, true},
		{`/bin/sh -c 'kill 1' "a b"`, `'/bin/sh' '-c' 'kill 1' 'a b'`, false, true},
		{`/usr/bin/echo a"b c"d`, `'/usr/bin/echo' 'ab cd'`, false, true},
		{`/usr/bin/env -u #x kill`, `'/usr/bin/env' '-u' '#x' 'kill'`, false, true},
		{`/bin/sh -c ${CMD}`, `'/bin/sh' '-c' "${CMD}"`, true, true},
		{`/usr/bin/env ${K}ll $X $$Y 1%`, `'/usr/bin/env' "${K}"'ll' "${X}" '$Y' '1%'`, true, true},
		{`:/usr/bin/echo ${A}`, `'/usr/bin/echo' '${A}'`, false, true},
		{`/usr/bin/echo $$$A ${A}$B`, `'/usr/bin/echo' '$$A' "${A}"'$B'`, false, true},
		{`/bin/sh -c "echo $R >> /tmp/r"`, `'/bin/sh' '-c' 'echo $R >> /tmp/r'`, false, true},
		{`/usr/bin/echo x$A "$A" '$B' $A.y $ $$A e${A}f`, `'/usr/bin/echo' 'x$A' "${A}" "${B}" "${SYSTEMD_ENV}" "${SYSTEMD_ENV}" '$A' 'e'"${A}"'f'`, false, true},
		{`/usr/bin/echo ${a-b}`, `'/usr/bin/echo' "${SYSTEMD_ENV}"`, false, true},
		{`/usr/bin/echo ''`, `'/usr/bin/echo' ''`, false, true},
		{`/usr/bin/echo 'a`, ``, false, false},
		{`/usr/bin/echo ${X}`, `'/usr/bin/echo' "${X}"`, false, true},
		{`/usr/bin/podman ${K}op -a`, `'/usr/bin/podman' "${K}"'op' '-a'`, true, true},
		{`/bin/sh -c 'echo ${X}'`, `'/bin/sh' '-c' 'echo '"${X}"`, false, true},
		{`/bin/sh -c '${K}ll 1'`, `'/bin/sh' '-c' "${K}"'ll 1'`, true, true},
		{`/usr/bin/kill /tmp/\x31`, `'/usr/bin/kill' '/tmp/1'`, false, true},
		{`/usr/bin/${K}ll 1`, `'/usr/bin/'"${K}"'ll' '1'`, true, true},
		{``, ``, false, true},
		{`-`, ``, false, true},
	} {
		got, executableComputed, commandComputed, ok, parseErr := systemdExecCommandInfo(tc.in)
		if got != tc.want || (executableComputed || commandComputed) != tc.computed || ok != tc.ok || parseErr != nil {
			t.Errorf("systemdExecCommandInfo(%q) = %q, executable=%v command=%v ok=%v err=%v; ждали %q, computed=%v ok=%v err=nil", tc.in, got, executableComputed, commandComputed, ok, parseErr, tc.want, tc.computed, tc.ok)
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
		{`ExecStartPre=/usr/bin/\x6bill 1`, none, `'/usr/bin/kill' '1'`, false},
		{`ExecStartPre=/usr/bin/echo 'a`, none, ``, true},
		{`{}`, braces, ``, true},
		{`ExecStartPre={}`, braces, ``, true},
		{`ExecStartPre=`, none, ``, false},
		{`ExecStopPre=/usr/bin/kill 1`, none, ``, false},
	} {
		inner, reason, _, parseErr := execPropertyCommand(tc.arg, tc.fed)
		if inner != tc.inner || (reason != "") != tc.hasReason || parseErr != nil {
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
