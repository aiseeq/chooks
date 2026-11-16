package main

import "testing"

// Конструкции, которые обход не моделирует: переменные, которые они могут затронуть, неясны, и
// выкатка с ними — класс 3. Формы, которые разбор моделирует целиком, остаются пробой.
func TestUnmodeledUnclear(t *testing.T) {
	const tmp = "/tmp/p"
	const probe = "проба: каталог /tmp/p"
	const ex = `export XDG_CONFIG_HOME=/tmp/c`
	const exl = `export XDG_CONFIG_HOME=/home/aisee/.config`
	const un = `unset XDG_CONFIG_HOME`
	cases := []struct {
		name, cmd string
		probe     string // "" — выкатка класса 3
	}{
		// цикл: третий проход
		{"цепочка присваиваний в цикле", ex + `; T=/tmp/c; U=/tmp/c; for i in 1 2 3; do export XDG_CONFIG_HOME=$T; graft deploy prod; T=$U; U=/home/aisee/.config; done`, ""},
		{"цепочка через префикс", `export A=/tmp/c B=/tmp/c; for i in 1 2 3; do XDG_CONFIG_HOME=$A graft deploy prod; A=$B; B=/home/aisee/.config; done`, ""},
		{"цепочка в while", `T=/tmp/c; U=/tmp/c; while c; do export XDG_CONFIG_HOME=$T; graft deploy prod; T=$U; U=/home/aisee/.config; done`, ""},
		{"переменная for", `XDG_CONFIG_HOME=/tmp/c; export XDG_CONFIG_HOME; for XDG_CONFIG_HOME in /home/aisee/.config; do graft deploy prod; done`, ""},
		{"select", ex + `; select x in a; do ` + un + `; break; done; graft deploy prod`, ""},
		{"break 2", ex + `; for a in 1; do for b in 1; do c && break 2; done; ` + un + `; done; graft deploy prod`, ""},
		// return, exit в функции
		{"return до export", exl + `; f() { return 0; ` + ex + `; }; f; graft deploy prod`, ""},
		{"условный return до export", exl + `; f() { c && return; ` + ex + `; }; f; graft deploy prod`, ""},
		{"exit до export", exl + `; f() { c && exit; ` + ex + `; }; f; graft deploy prod`, ""},
		// конвейеры
		{"lastpipe", ex + `; shopt -s lastpipe; echo a | while read l; do ` + un + `; done; graft deploy prod`, ""},
		{"shopt из переменной", ex + `; shopt -s $o; echo a | while read l; do ` + un + `; done; graft deploy prod`, ""},
		{"set -o posix", ex + `; set -o posix; graft deploy prod`, ""},
		// функции не по литеральному имени
		{"вызов $g", ex + `; f() { ` + un + `; }; $g; graft deploy prod`, ""},
		{"вызов ${a[0]}", ex + `; f() { ` + un + `; }; a=(f); ${a[0]}; graft deploy prod`, ""},
		{"export -f во вложенный скрипт", `f() { ` + un + `; }; export -f f; ` + ex + `; bash -c 'f; graft deploy prod'`, ""},
		{"declare -fx во вложенный скрипт", `f() { ` + un + `; }; ` + ex + `; declare -fx f; bash -c 'f; graft deploy prod'`, ""},
		{"trap функции", ex + `; f() { ` + un + `; }; trap f DEBUG; graft deploy prod`, ""},
		{"trap действия", ex + `; trap '` + un + `' DEBUG; graft deploy prod`, ""},
		{"trap -- действия", ex + `; trap -- '` + un + `' DEBUG; graft deploy prod`, ""},
		{"trap из переменной", ex + `; trap "$a" DEBUG; graft deploy prod`, ""},
		{"command_not_found_handle", ex + `; command_not_found_handle() { ` + un + `; }; nosuch; graft deploy prod`, ""},
		{"coproc функции", ex + `; f() { ` + un + `; }; coproc f; graft deploy prod`, ""},
		{"функция с именем graft", ex + `; graft() { ` + un + `; command graft "$@"; }; graft deploy prod`, ""},
		{"функция с именем export", `function export { :; }; ` + ex + `; graft deploy prod`, ""},
		{"функция с eval, вызов $g", ex + `; f() { eval "$1"; }; $g; graft deploy prod`, ""},
		// встроенные команды и арифметика
		{"read", ex + `; read XDG_CONFIG_HOME < f; graft deploy prod`, ""},
		{"read из переменной", ex + `; read $n < f; graft deploy prod`, ""},
		{"mapfile", ex + `; mapfile XDG_CONFIG_HOME < f; graft deploy prod`, ""},
		{"printf -v", ex + `; printf -v XDG_CONFIG_HOME %s /home/aisee/.config; graft deploy prod`, ""},
		{"getopts", ex + `; getopts a XDG_CONFIG_HOME; graft deploy prod`, ""},
		{"wait -p", ex + `; sleep 1 & wait -p XDG_CONFIG_HOME; graft deploy prod`, ""},
		{"builtin read", ex + `; builtin read XDG_CONFIG_HOME < f; graft deploy prod`, ""},
		{"let", ex + `; let XDG_CONFIG_HOME=1; graft deploy prod`, ""},
		{"$(( ))", ex + `; : $((XDG_CONFIG_HOME=1)); graft deploy prod`, ""},
		{"(( ))", ex + `; (( XDG_CONFIG_HOME = 1 )); graft deploy prod`, ""},
		{"readonly, затем export", `readonly XDG_CONFIG_HOME=/home/aisee/.config; export XDG_CONFIG_HOME; ` + ex + `; graft deploy prod`, ""},
		{"declare -rx, затем export", `declare -rx XDG_CONFIG_HOME=/home/aisee/.config; ` + ex + `; graft deploy prod`, ""},
		{"readonly, затем префикс", `readonly XDG_CONFIG_HOME=/home/aisee/.config; export XDG_CONFIG_HOME; XDG_CONFIG_HOME=/tmp/c graft deploy prod`, ""},
		{"readonly, затем unset", `declare -rx XDG_CONFIG_HOME=/tmp/c; ` + un + `; graft deploy prod`, ""},
		{"declare -n", ex + `; declare -n r=XDG_CONFIG_HOME; r=/home/aisee/.config; graft deploy prod`, ""},
		{"case ;&", `T=/tmp/c; ` + ex + `; case x in a) T=/home/aisee/.config ;& b) export XDG_CONFIG_HOME=$T;; esac; graft deploy prod`, ""},
		{"case ;;&", `T=/tmp/c; ` + ex + `; case x in a) T=/home/aisee/.config ;;& b) export XDG_CONFIG_HOME=$T;; esac; graft deploy prod`, ""},
		{"case ;; не переходит", `T=/tmp/c; ` + ex + `; case x in a) T=/home/aisee/.config ;; b) export XDG_CONFIG_HOME=$T;; esac; graft deploy test`, probe},
		{"alias", ex + `; shopt -s expand_aliases; alias graft='unset XDG_CONFIG_HOME; graft'; graft deploy prod`, ""},
		{"BASH_ENV у вложенного скрипта", ex + `; BASH_ENV=/tmp/x bash -c 'graft deploy prod'`, ""},
		// разбор моделирует целиком: проба
		{"set -euo pipefail", `set -euo pipefail; ` + ex + `; graft deploy test`, probe},
		{"set -e -o pipefail", `set -e -o pipefail; ` + ex + `; graft deploy test`, probe},
		{"shopt -s nullglob и конвейер", `shopt -s nullglob; ` + ex + `; echo a | cat; graft deploy test`, probe},
		{"lastpipe без присваиваний в конвейере", `shopt -s lastpipe; ` + ex + `; echo a | while read l; do :; done; graft deploy test`, probe},
		{"trap rm EXIT", `d=$(mktemp -d); trap 'rm -rf "$d"' EXIT; ` + ex + `; graft deploy test`, probe},
		{"trap - сброс", ex + `; f() { ` + un + `; }; trap - DEBUG; graft deploy test`, probe},
		{"export в каждом проходе", `for e in a b; do export XDG_CONFIG_HOME=$(mktemp -d); graft deploy test; done`, probe},
		{"цикл не трогает конфиг", ex + `; for e in a b; do graft deploy test; done`, probe},
		{"функция без return", `f() { ` + ex + `; }; f; graft deploy test`, probe},
		{"read другой переменной", ex + `; read x < f; graft deploy test`, probe},
		{"арифметика другой переменной", ex + `; i=0; (( i++ )); graft deploy test`, probe},
		{"case без ;&", ex + `; case a in a) :;; b) ` + un + `;; esac; graft deploy test`, ""},
		{"case без присваиваний", ex + `; case a in a) :;; b) :;; esac; graft deploy test`, probe},
		{"вызов $g без функций: может быть unset", ex + `; g=$(echo unset); $g XDG_CONFIG_HOME; graft deploy prod`, ""},
		{"программа по пути из переменной", ex + `; "$D/bin/x"; graft deploy test`, probe},
		{"функции по trap не трогают конфиг", ex + `; f() { A=1; }; trap f DEBUG; graft deploy test`, probe},
		{"export -n другой переменной", ex + `; export -n A; graft deploy test`, probe},
		{"set -o печать", `set -o; ` + ex + `; graft deploy test`, probe},
		{"alias печать", ex + `; alias; graft deploy test`, probe},
		{"readonly другой переменной", ex + `; readonly A=1; A=2; graft deploy test`, probe},
		// что трогают тела функций (по тексту)
		{"тело: присваивание", ex + `; f() { XDG_CONFIG_HOME=/home/aisee/.config; }; trap f DEBUG; graft deploy prod`, ""},
		{"тело: declare -n", ex + `; f() { declare -n r=$1; }; trap f DEBUG; graft deploy prod`, ""},
		{"тело: declare из переменной", ex + `; f() { declare $o; }; trap f DEBUG; graft deploy prod`, ""},
		{"тело: for", ex + `; f() { for XDG_CONFIG_HOME in a; do :; done; }; trap f DEBUG; graft deploy prod`, ""},
		{"тело: builtin unset", ex + `; f() { builtin unset XDG_CONFIG_HOME; }; trap f DEBUG; graft deploy prod`, ""},
		{"тело: арифметика", ex + `; f() { (( XDG_CONFIG_HOME = 1 )); }; trap f DEBUG; graft deploy prod`, ""},
		{"тело: $(( ))", ex + `; f() { : $(( XDG_CONFIG_HOME = 1 )); }; trap f DEBUG; graft deploy prod`, ""},
		{"тело: let", ex + `; f() { let XDG_CONFIG_HOME=1; }; trap f DEBUG; graft deploy prod`, ""},
		{"тело: read", ex + `; f() { read XDG_CONFIG_HOME; }; trap f DEBUG; graft deploy prod`, ""},
		{"тело: read из переменной", ex + `; f() { read $n; }; trap f DEBUG; graft deploy prod`, ""},
		{"тело: printf -v", ex + `; f() { printf -v XDG_CONFIG_HOME x; }; trap f DEBUG; graft deploy prod`, ""},
		{"тело: wait -p", ex + `; f() { wait -p XDG_CONFIG_HOME; }; trap f DEBUG; graft deploy prod`, ""},
		{"тело: eval", ex + `; f() { eval "$1"; }; trap f DEBUG; graft deploy prod`, ""},
		{"тело: имя из подстановки", ex + `; f() { "$@"; }; trap f DEBUG; graft deploy prod`, ""},
		{"тело: export -n", ex + `; f() { export -n XDG_CONFIG_HOME; }; trap f DEBUG; graft deploy prod`, ""},
		{"set -eo posix", ex + `; set -eo posix; graft deploy prod`, ""},
		{"set -o из переменной", ex + `; set -o $o; graft deploy prod`, ""},
		{"set -o vi и конвейер", ex + `; set -o vi; echo a | while read l; do ` + un + `; done; graft deploy prod`, ""},
		{"break в функции", exl + `; f() { c && break; ` + ex + `; }; f; graft deploy prod`, ""},
		{"continue в функции", exl + `; f() { c && continue; ` + ex + `; }; f; graft deploy prod`, ""},
		{"ветка после ;; за ;&", `T=/tmp/c; ` + ex + `; case x in a) T=/home/aisee/.config ;& b) :;; c) export XDG_CONFIG_HOME=$T;; esac; graft deploy test`, probe},
		{"export -f без функций, затем вложенный скрипт", ex + `; export -f f; bash -c 'graft deploy test'`, probe},
		{"printf -v в конце", ex + `; printf -v; graft deploy test`, probe},
		{"printf -v из переменной", ex + `; printf -v "$n" x; graft deploy prod`, ""},
		{"wait -p", ex + `; wait -p A; graft deploy test`, probe},
		{"имя из ${:-/путь}", ex + `; ${c:-/bin/true} XDG_CONFIG_HOME; graft deploy prod`, ""},
		{"trap с одним словом — сброс сигнала", ex + `; f() { ` + un + `; }; trap f; graft deploy test`, probe},
		{"trap '' — сигнал игнорируется", ex + `; f() { ` + un + `; }; trap '' DEBUG; graft deploy test`, probe},
		{"trap не разобрано", ex + `; trap 'if' DEBUG; graft deploy prod`, ""},
		{"arith из переменной", ex + `; (( $n )); graft deploy prod`, ""},
		{"arith с внешней переменной", ex + `; (( i < n )); graft deploy prod`, ""},
		{"arith с переменной-выражением", ex + `; i='XDG_CONFIG_HOME=1'; (( i )); graft deploy prod`, ""},
		{"case: прямой путь в ветку после ;&", exl + `; case x in a) ` + ex + ` ;& b) graft deploy prod;; esac`, ""},
		{"coproc функции с eval", ex + `; f() { eval "$1"; }; coproc f; graft deploy prod`, ""},
		{"export -f функции с eval во вложенный скрипт", `f() { eval "$1"; }; export -f f; ` + ex + `; bash -c 'graft deploy prod'`, ""},
		{"builtin export -f во вложенный скрипт", `f() { ` + un + `; }; builtin export -f f; ` + ex + `; bash -c 'f; graft deploy prod'`, ""},
		{"readonly в подоболочке, затем живой конфиг", ex + `; (readonly XDG_CONFIG_HOME=/tmp/d); export XDG_CONFIG_HOME=/home/aisee/.config; graft deploy prod`, ""},
		{"export без значения в цикле", ex + `; for i in 1 2; do export XDG_CONFIG_HOME; graft deploy prod; done`, ""},
		{"функция из цикла меняет цепочку", `T=/tmp/c; U=/tmp/c; for i in 1 2 3 4; do export XDG_CONFIG_HOME=$T; graft deploy prod; f; f() { T=$U; U=/home/aisee/.config; }; done`, ""},
		{"trap родителя не во вложенном скрипте", ex + `; trap 'A=1' DEBUG; bash -c 'graft deploy test'`, probe},
		{"arith после += к неизвестному", ex + `; i=$(cat f); i+=1; (( i )); graft deploy prod`, ""},
		{"arith с неясным целым", `export i='XDG_CONFIG_HOME=1'; c && i=0; ` + ex + `; (( i )); graft deploy prod`, ""},
		{"arith с выражением после целого", ex + `; i='1,XDG_CONFIG_HOME=2'; (( i )); graft deploy prod`, ""},
		{"имя из подстановки с литералом", `xf() { ` + un + `; }; ` + ex + `; x$g; graft deploy prod`, ""},
		{"функция из первого прохода, конфиг не тронут", ex + `; while c; do graft deploy prod; f; f() { ` + un + `; }; done`, ""},
		{"local, затем снова readonly", ex + `; f() { local -r A=1; A=2; }; f; graft deploy test`, probe},
		{"export -f без функций", ex + `; export -f f; graft deploy test`, probe},
	}
	for _, c := range cases {
		checkProbe(t, c.name, Classify(Event{Tool: "Bash", Command: c.cmd, Cwd: tmp, Home: testHome}), c.probe)
	}
}

// readonly GIT_DIR: unset не удаётся, коммит уходит в конфиг-репо.
func TestUnmodeledGitDir(t *testing.T) {
	cmd := `readonly GIT_DIR=$HOME/.claude-config.git; export GIT_DIR; unset GIT_DIR; git commit -m x`
	if v := Classify(Event{Tool: "Bash", Command: cmd, Cwd: "/tmp", Home: testHome}); !v.HitClass(2) {
		t.Errorf("%s: ждём класс 2, Hits=%+v", cmd, v.Hits)
	}
}
