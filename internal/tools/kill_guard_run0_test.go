package tools

import (
	"slices"
	"testing"

	"github.com/aiseeq/akit/shellcmd"
)

// Тикеты kill-guard-run0-signal и kill-guard-run0-as-systemd-run: run0
// запускает команду временной службой менеджера (man run0), как
// systemd-run --pipe без --scope: процесс — не потомок агента, ядро его
// сигналы не сверяет. run0 проверяется теми же правилами, что systemd-run в
// режиме службы; sudo и pkexec делают exec в своём процессе — как было
func TestKillGuardRun0AsSystemdRun(t *testing.T) {
	for _, command := range []string{
		`run0 kill 12345`,
		`run0 pkill opencode`,
		`run0 --user=aisee killall sleep`,
		`run0 systemd-run --scope kill 12345`,
		`run0 sh -c 'kill 12345'`,
		`nice run0 /bin/kill -TERM 12345`,
		`/usr/bin/run0 kill 12345`,
		`\run0 kill 12345`,
		`sh -c 'run0 kill 12345'`,
		"bash <<EOF\nrun0 kill 12345\nEOF",
		`find . -exec run0 kill 12345 \;`,
		// run0 не словом в тексте: проверка текста за ним
		`R=run0; $R kill 12345`,
		// как systemd-run --user kill -0 12345: сигнальная команда под службой
		`run0 kill -0 12345`,
		`systemd-run --user kill -0 12345`,
		// опции — как их разбирает getopt_long: сокращения и связки
		`run0 -iu root kill 12345`,
		`run0 -iD /tmp kill 12345`,
		`run0 -uroot kill 12345`,
		`run0 --us root kill 12345`,
		`run0 --uni x kill 12345`,
		`run0 --chd /tmp kill 12345`,
		`run0 --desc x kill 12345`,
		`run0 --machine=.host kill 12345`,
		`run0 --property=SuccessAction=poweroff true`,
		// run0 без команды и с --via-shell (-i) — оболочка
		`run0 <<< 'kill 12345'`,
		`echo 'kill 12345' | run0`,
		"run0 <<EOF\nkill 12345\nEOF",
		`run0 -i <<< 'kill 12345'`,
		`run0 --via-shell <<< 'kill 12345'`,
		`run0 -u root -- <<< 'kill 12345'`,
		`run0 --setenv=SHELL=/bin/sh <<< 'kill 12345'`,
		`run0 -i 'kill 12345'`,
		`run0 --via-shell 'pkill opencode'`,
		`sh -c 'run0 -i podman stop x'`,
		// текст вложенного скрипта иной, чем в команде: замена — по тексту за run0
		`sh -c "run0 -u \$U kill 12345"`,
		// программы, которые запускают оболочку на своём вводе
		`run0 sudo -s <<< 'kill 12345'`,
		`run0 sudo -i <<< 'kill 12345'`,
		`run0 su <<< 'kill 12345'`,
		`run0 su - <<< 'kill 12345'`,
		`run0 su -s /bin/bash <<< 'kill 12345'`,
		`run0 runuser - root <<< 'kill 12345'`,
		`run0 script -q /dev/null <<< 'kill 12345'`,
		`run0 machinectl shell <<< 'kill 12345'`,
		`run0 parallel <<< 'kill 12345'`,
		`run0 parallel ::: 'kill 12345'`,
		`systemd-run --pipe script -q /dev/null <<< 'kill 12345'`,
		"systemd-run --pipe script -q /dev/null <<EOF\nkill 12345\nEOF",
		`systemd-run --pipe machinectl shell <<< 'kill 12345'`,
		`systemd-run --pipe parallel <<< 'kill 12345'`,
		`systemd-run --pipe parallel ::: 'kill 12345'`,
		`script -q /dev/null <<< 'kill -9 -1'`,
		`parallel ::: 'kill -9 -1'`,
		`machinectl shell <<< 'kill -9 -1'`,
		// команда или скрипт вычисляются при запуске
		`r() { run0 "$@"; }; r kill 12345`,
		`r() { run0 "$@"; }; r bash <<< 'kill 12345'`,
		`run0 $(cat kill-cmd.txt)`,
		`run0 bash -c "$(cat kill-cmd.txt)"`,
		`run0 "$CMD"; echo kill-switch`,
		`run0 sh -c 'exec "$@"' _ kill 12345`,
		`run0 sh -c 'read x; $x' <<< 'kill 12345'`,
		`run0 bash -c 'read -a w; "${w[@]}"' <<< 'kill 12345'`,
		// podman stop, rm, restart без слова kill в тексте
		`run0 podman stop x`,
		`run0 podman rm -f x`,
		`run0 podman container stop x`,
		`run0 podman --log-level=debug stop x`,
		`run0 podman-remote stop x`,
		`run0 podman restart x`,
		`run0 podman stop -a`,
		`run0 podman pod rm -f p`,
		`run0 -u aisee podman stop x`,
		`systemd-run --user podman restart x`,
		// ввод оболочки за run0 — её скрипт, как за systemd-run
		"run0 bash <<EOF\nkill 12345\nEOF",
		"run0 bash <<EOF\nkill -9 -1\nEOF",
		`run0 bash <<< 'kill 12345'`,
		"run0 sh -s <<EOF\nkill 12345\nEOF",
		"run0 systemd-run --scope bash <<EOF\nkill 12345\nEOF",
		"run0 bash <<EOF\nCHOOKS_END\nkill 12345\nEOF",
		"run0 bash <<EOF\nkill 1234\nfi\nEOF",
		`run0 bash < <(echo kill -9 -1)`,
		`echo kill 12345 | run0 bash`,
		"{ read x; run0 bash; } <<EOF\nx\nkill 1234\nEOF",
		"while read x; do run0 bash; done <<EOF\nx\nkill 1234\nEOF",
		`printf 'x\nkill 1234\n' | { read x; run0 bash; }`,
		`cat kill-list | xargs run0 bash`,
		"f() { run0 bash; }; f <<EOF\nkill 1234\nEOF",
		`coproc run0 bash; echo 'kill 12345' >&${COPROC[1]}`,
		"bash <<EOF\n{ read x; run0 bash; } <<IN\nx\nkill 1234\nIN\nEOF",
		`run0 sh -c 'echo kill 1234 | bash'`,
		// ввод, которого разбор не прочёл (функция, exec <, цикл с < <(…),
		// xargs), при kill в тексте — отказ под службой, с run0 и без
		`while read x; do run0 bash; done < <(printf 'x\nkill 1234\n')`,
		`while read x; do systemd-run --pipe bash; done < <(printf 'x\nkill 1234\n')`,
		`f() { run0 bash; }; echo 'kill 1234' | f`,
		`f() { systemd-run --pipe bash; }; echo 'kill 1234' | f`,
		`exec < <(echo kill 1234); run0 bash`,
		`exec < <(echo kill 1234); systemd-run --pipe bash`,
		"exec <<EOF\nkill 1234\nEOF\nrun0 bash",
		"exec <<EOF\nkill 1234\nEOF\nsystemd-run --pipe bash",
		"xargs run0 bash -c <<EOF\n'kill 1234'\nEOF",
		"{ xargs systemd-run --pipe bash -c; } <<EOF\n'kill 1234'\nEOF",
		`run0 xargs -0 bash -c <<< 'kill 12345'`,
		`systemd-run --pipe xargs -0 bash -c <<< 'kill 12345'`,
		// run0 не словом: ввод разбирается вместе с текстом за ним
		"R=run0; $R bash <<EOF\nkill 12345\nEOF",
		"R=run0; $R bash <<EOF\nCHOOKS_END\nkill 12345\nEOF",
		`R=run0; $R <<< 'kill 12345'`,
		`R=run0; $R podman stop x`,
		// pkill -f по тексту, который прислал агент (с run0), убьёт эту оболочку
		`run0 true; pkill -f 'run0 true'`,
		`run0 true; pkill -f 'run0 [t]rue'`,
		// systemd-run под systemd-run: на другую машину идёт только -H
		`systemd-run --user systemd-run --user kill 12345`,
		// цена: оболочка под службой со своим вводом (агента) при слове
		// kill в тексте — отказ, ввод ей могли задать вне видимого текста
		`run0 bash; echo skill`,
		`systemd-run --user --pipe bash; echo skill`,
		// вызовы чужого скрипта той же цепочки до c — не ввод c
		`systemd-run --pipe bash -c true; systemd-run --pipe bash; echo skill`,
		// parallel без команды за обёрткой под службой; следующие списки
		// ::: дописывают аргументы к строке первого
		`systemd-run --pipe nice parallel <<< 'kill 12345'`,
		`run0 nice -n 5 parallel ::: 'kill 12345'`,
		`run0 sudo parallel <<< 'kill 12345'`,
		`systemd-run --pipe parallel ::: kill ::: 12345`,
		`run0 parallel ::: 'kill -s TERM' ::: 12345`,
		`parallel ::: nice ::: 'kill -9 -1'`,
		"run0 -i --property=SuccessAction=poweroff true",
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
		// run0 --machine — как systemd-run -M: команда идёт в контейнере
		`run0 --machine=foo kill 12345`,
		`run0 --mach=foo kill 12345`,
		`R=run0; $R --machine=foo kill 12345`,
		`R=run0; $R bash <<< 'echo skill'`,
		`run0 systemd-run -H host kill 12345`,
		`run0 run0 systemd-run -H host kill 12345`,
		`systemd-run --user systemd-run -M c1 kill 12345`,
		// run0 ничего не запустит: --help, --version, неизвестная или
		// неоднозначная опция (getopt_long), флаг со значением, нет значения
		`run0 --help kill 12345`,
		`run0 -V kill 12345`,
		`run0 --u root kill 12345`,
		`run0 -p x kill 12345`,
		`run0 -M foo kill 12345`,
		`run0 --pipe=x kill 12345`,
		`run0 --bogus kill 12345`,
		// литеральный скрипт ввода без сигнала
		`run0 bash <<< 'echo skill'`,
		"run0 bash <<EOF\ngrep -c kill /var/log/messages\nEOF",
		"run0 bash <<'EOF'\nsystemctl restart foo\nEOF\nls ~/.claude/skills",
		"run0 bash <<EOF\necho make\nEOF\necho kill",
		`echo make | run0 bash; echo skill`,
		"{ read x; run0 bash; } <<EOF\nx\nmake\nEOF",
		"{ read x; run0 bash; } <<EOF\nx\nmake\nEOF\necho skill",
		"f() { sudo bash; }; f <<EOF\nmake\nEOF\necho skill",
		// скрипт файлом — вне проверки, как у systemd-run
		`run0 bash < /dev/null; echo skill`,
		`run0 sh -s -- arg < ./build.sh; grep kill /var/log/messages`,
		`run0 bash -c 'make'; echo skill`,
		`run0 bash ./build.sh; echo skill`,
		`run0 sh -c 'echo make | bash'; echo skill`,
		`bash < ./build.sh; echo skill`,
		// без kill в тексте ошибка разбора и невидимый ввод — не отказ
		"run0 bash <<EOF\nmake\nfi\nEOF",
		`while read x; do run0 bash; done < <(printf 'x\nmake\n')`,
		`echo make | run0 bash; systemctl --user status x.service`,
		`f() { run0 bash; }; echo make | f`,
		`run0 bash`,
		`run0 dnf install x`,
		`run0 systemctl restart foo`,
		`run0 podman ps`,
		`run0 parallel ::: 'make a' 'make b'`,
		`script -q /dev/null <<< 'make'`,
		// script с командой (-c, --command) исполняет её, а не оболочку на вводе
		`systemd-run --pipe script -qc make /dev/null <<< 'kill 12345'`,
		`systemd-run --pipe script --command make /dev/null <<< 'kill 12345'`,
		`systemd-run --pipe script --command=make /dev/null <<< 'kill 12345'`,
		`systemd-run --pipe parallel ::: echo ::: 'kill 12345'`,
		// аргументы из файла (::::) не видны, как у -a
		`systemd-run --pipe parallel ::: nice :::: kill`,
		// parallel под sudo — потомок агента; под ssh за systemd-run — на другой машине
		`sudo parallel <<< 'kill 12345'`,
		`systemd-run --pipe ssh host parallel <<< 'kill 12345'`,
		// слова команды systemd-run — литералы exec: '$(' не подстановка
		`systemd-run --pipe echo '$(' kill`,
		// --scope: оболочка остаётся потомком агента, её сигналы сверяет ядро
		`systemd-run --scope bash; echo skill`,
		// свой heredoc и <<< без вызовов (комментарий) — ввод виден
		"run0 bash <<'EOF'\n# kill 12345 later\nEOF",
		`systemd-run --pipe bash <<< '# kill 12345'`,
	} {
		if r := validateKill(t, command); !r.IsValid {
			t.Errorf("не должно блокироваться: %s: %v", command, r.Suggestions)
		}
	}
}

// Отказ в команде с run0, заменённым на systemd-run, объясняет замену
func TestKillGuardRun0Hint(t *testing.T) {
	r := validateKill(t, `run0 kill 12345`)
	if r.IsValid || !slices.Contains(r.Suggestions, run0Hint) {
		t.Errorf("run0 kill 12345: ждали отказ с подсказкой про run0, получили %v %q", r.IsValid, r.Suggestions)
	}
	// подсказка — и у run0, который проверен текстом run0Command (--via-shell
	// с командой), и только там, где run0 есть
	for command, want := range map[string]bool{
		`run0 -i kill 12345`:            true,
		`run0 bash; echo skill`:         true,
		`systemd-run --user kill 12345`: false,
		`parallel ::: 'kill -9 -1'`:     false,
	} {
		r := validateKill(t, command)
		if r.IsValid || slices.Contains(r.Suggestions, run0Hint) != want {
			t.Errorf("%s: ждали отказ, подсказка про run0 %v; получили %v %q", command, want, r.IsValid, r.Suggestions)
		}
	}
}

// Места слов run0 в тексте: вызов, у которого слов в тексте меньше, чем
// аргументов, не заменяется (иначе выход за срез)
func TestRun0Words(t *testing.T) {
	c := shellcmd.Call{Name: "run0", Args: []string{"kill", "1"}, Raw: "run0 kill", Offset: 0}
	if _, ok, _ := replaceRun0("run0 kill", c); ok {
		t.Errorf("replaceRun0 с двумя аргументами и одним словом за run0: ждали ok false")
	}
	// текст вызова, который не разбирается на слова, — ошибка, не замена
	c = shellcmd.Call{Name: "run0", Args: []string{"kill"}, Raw: "run0 'kill", Offset: 0}
	if _, ok, err := replaceRun0("run0 'kill", c); ok || err == nil {
		t.Errorf("replaceRun0 с неразбираемым текстом вызова: ok=%v err=%v, ждали ok false и ошибку", ok, err)
	}
}

// Ввод вызова за обёрткой текстом: тело heredoc — со строкой ограничителя,
// цель, которую в кавычки не поставить (нулевой байт), — ошибка; при kill в
// тексте ошибка — отказ, без него — нет
func TestInputRedirs(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{"", "E"},
		{"a\n", "a\nE"},
		{"a", "a\nE"},
	} {
		if got := heredocText(tc.body, "E"); got != tc.want {
			t.Errorf("heredocText(%q) = %q, ждали %q", tc.body, got, tc.want)
		}
	}
	for body, want := range map[string]string{"make": "CHOOKS_END", "x CHOOKS_END CHOOKS_END_ y": "CHOOKS_END__"} {
		if got := heredocEnd(body); got != want {
			t.Errorf("heredocEnd(%q) = %q, ждали %q", body, got, want)
		}
	}
	c := shellcmd.Call{Name: "pkexec", Args: []string{"true"}, Raw: "pkexec true", Redirs: []shellcmd.Redir{{Op: "<", Target: "a\x00b"}}}
	if _, err := inputRedirs(c); err == nil {
		t.Errorf("inputRedirs с нулевым байтом в цели: ждали ошибку")
	}
	for _, killText := range []bool{true, false} {
		s := &killScan{tool: newKillGuard(t), killText: killText}
		if err := s.elevated(c, "true"); err != nil {
			t.Fatal(err)
		}
		if got := len(s.reasons) > 0; got != killText {
			t.Errorf("elevated с непереносимым вводом, kill в тексте %v: отказ %v %q", killText, got, s.reasons)
		}
	}
}

// parallel под systemd-run: команда, которую разбор не прочёл (не UTF-8), —
// ошибка bareParallel и отказ при kill в тексте
func TestServiceParallel(t *testing.T) {
	c := shellcmd.Call{Name: "systemd-run", Args: []string{"--pipe", "parallel", "\xff"}, Raw: "systemd-run --pipe parallel"}
	if _, ok, err := bareParallel(c); ok || err == nil {
		t.Errorf("bareParallel с не-UTF-8 словом: ok=%v err=%v, ждали ошибку", ok, err)
	}
	for _, killText := range []bool{true, false} {
		s := &killScan{tool: newKillGuard(t), killText: killText}
		if err := s.serviceParallel(c, []shellcmd.Call{c}); err != nil {
			t.Fatal(err)
		}
		if got := len(s.reasons) > 0; got != killText {
			t.Errorf("serviceParallel с неразобранной командой, kill в тексте %v: отказ %v %q", killText, got, s.reasons)
		}
	}
}

// Начало команды systemd-run: за -- — следующее слово, даже с дефисом
func TestSystemdRunCommandStart(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want int
	}{
		{[]string{"--pipe", "--", "-x"}, 2},
		{[]string{"--pipe", "-p", "A=b", "kill"}, 3},
		{[]string{"--pipe"}, 1},
	} {
		if got := systemdRunCommandStart(tc.args); got != tc.want {
			t.Errorf("systemdRunCommandStart(%q) = %d, ждали %d", tc.args, got, tc.want)
		}
	}
}

// Замена run0 в тексте команды: имя и опции — systemd-run --pipe с машиной
// и свойствами, слова команды и ввод остаются
func TestRewriteRun0(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`run0 kill 1`, `systemd-run --pipe -- kill 1`},
		{`run0 -u root --nice 5 -- kill 1`, `systemd-run --pipe -- kill 1`},
		// --via-shell с командой — текстом за run0 (run0Command)
		{`run0 -iu root kill 1`, `run0 -iu root kill 1`},
		{`run0 --mach=foo --property=A=b kill 1`, `systemd-run --pipe --machine=foo --property=A=b -- kill 1`},
		{`run0 <<< x`, `systemd-run --pipe -- sh <<< x`},
		{`echo a | run0 -u root bash; run0 kill 2`, `echo a | systemd-run --pipe -- bash; systemd-run --pipe -- kill 2`},
		{`sh -c 'run0 kill 1'`, `sh -c 'systemd-run --pipe -- kill 1'`},
		{`run0 run0 kill 1`, `systemd-run --pipe -- systemd-run --pipe -- kill 1`},
		{`nice -n 5 run0 -u root kill 1`, `nice -n 5 systemd-run --pipe -- kill 1`},
		{`run0 -i`, `systemd-run --pipe -- sh`},
		{`find . -exec run0 kill 1 \;`, `find . -exec systemd-run --pipe -- kill 1 \;`},
		{"run0 bash <<EOF\nkill 1\nEOF", "systemd-run --pipe -- bash <<EOF\nkill 1\nEOF"},
		// не заменяется: run0 ничего не запустит, имя из переменной
		{`run0 --help kill 1`, `run0 --help kill 1`},
		{`R=run0; $R kill 1`, `R=run0; $R kill 1`},
		// значение, которое нельзя перенести без кавычек
		{`run0 --machine='a b' kill 1`, `run0 --machine='a b' kill 1`},
	} {
		if got, _, _ := rewriteRun0(tc.in); got != tc.want {
			t.Errorf("rewriteRun0(%q) = %q, ждали %q", tc.in, got, tc.want)
		}
	}
}

// Разбор опций run0 по getopt_long ("+hVu:g:D:i" и длинные из run0 --help)
func TestParseRun0(t *testing.T) {
	for _, tc := range []struct {
		args    []string
		ok      bool
		start   int
		via     bool
		machine []string
	}{
		{[]string{"kill", "1"}, true, 0, false, nil},
		{[]string{"-u", "root", "kill"}, true, 2, false, nil},
		{[]string{"-uroot", "kill"}, true, 1, false, nil},
		{[]string{"-iu", "root", "kill"}, true, 2, true, nil},
		{[]string{"-iuroot", "kill"}, true, 1, true, nil},
		{[]string{"--us", "root", "kill"}, true, 2, false, nil},
		{[]string{"--user=root", "kill"}, true, 1, false, nil},
		{[]string{"--machine", "c1", "kill"}, true, 2, false, []string{"c1"}},
		{[]string{"--mach=c1", "kill"}, true, 1, false, []string{"c1"}},
		{[]string{"--via", "kill"}, true, 1, true, nil},
		{[]string{"--pty", "kill"}, true, 1, false, nil},
		{[]string{"--", "-x"}, true, 1, false, nil},
		{[]string{"-", "x"}, true, 0, false, nil},
		{[]string{}, true, 0, false, nil},
		{[]string{"--help", "kill"}, false, 0, false, nil},
		{[]string{"-ih", "kill"}, false, 0, false, nil},
		{[]string{"-V"}, false, 0, false, nil},
		{[]string{"--u", "root", "kill"}, false, 0, false, nil},
		{[]string{"--p", "kill"}, false, 0, false, nil},
		{[]string{"-p", "x", "kill"}, false, 0, false, nil},
		{[]string{"--pipe=x", "kill"}, false, 0, false, nil},
		{[]string{"-u"}, false, 0, false, nil},
		{[]string{"--user"}, false, 0, false, nil},
	} {
		inv, ok := parseRun0(tc.args)
		if ok != tc.ok || ok && (inv.start != tc.start || inv.viaShell != tc.via || !slices.Equal(inv.machines, tc.machine)) {
			t.Errorf("parseRun0(%q) = %+v, %v; ждали start=%d via=%v machine=%q ok=%v", tc.args, inv, ok, tc.start, tc.via, tc.machine, tc.ok)
		}
	}
}
