package tools

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/aiseeq/chooks/internal/core"
)

// fakeProc процесс поддельного /proc
type fakeProc struct {
	pid, ppid int
	comm      string
	args      []string
}

// Дерево как в сессии, где systemd --user убили kill $p: claude → bash (хук),
// отцепившийся opencode у systemd --user, посторонний sleep
var killGuardTree = []fakeProc{
	{1, 0, "systemd", []string{"/usr/lib/systemd/systemd", "--switched-root"}},
	{1500, 1, "systemd", []string{"/usr/lib/systemd/systemd", "--user"}},
	{2000, 1500, "konsole", []string{"/usr/bin/konsole"}},
	{2100, 2000, "claude", []string{"claude", "--dangerously-skip-permissions"}},
	{2200, 2100, "chooks", []string{"chooks", "pre-tool-use"}},
	{455274, 1500, "opencode", []string{"opencode", "run", "--dir", "/home/u/.cache/worktrees/sc2demu/farm-reflect-longcat"}},
	{600, 1500, "sleep", []string{"sleep", "1000"}},
	{3000, 1500, "plasmashell", []string{"/usr/bin/plasmashell"}},
	{3100, 1, "sddm", []string{"/usr/bin/sddm"}},
	{3200, 1500, "dbus-broker", []string{"/usr/bin/dbus-broker", "--scope", "user"}},
	{3300, 1500, "pipewire", []string{"/usr/bin/pipewire"}},
	{3400, 1, "dbus-broker", []string{"/usr/bin/dbus-broker", "--scope", "system"}},
	{3500, 1500, "kwin_wayland", []string{"/usr/bin/kwin_wayland"}},
}

// killGuardCgroups cgroup процессов дерева, как в сессии KDE: юниты предков
// и plasmashell systemctl трогать нельзя, юнит opencode и sleep — можно
var killGuardCgroups = map[int]string{
	1:      "0::/init.scope",
	1500:   "0::/user.slice/user-1000.slice/user@1000.service/init.scope",
	2000:   "0::/user.slice/user-1000.slice/user@1000.service/app.slice/app-org.kde.konsole-7438.scope/main.scope",
	2100:   "0::/user.slice/user-1000.slice/user@1000.service/agents.slice/agent-x-1.scope",
	2200:   "0::/user.slice/user-1000.slice/user@1000.service/agents.slice/agent-x-1.scope",
	455274: "0::/user.slice/user-1000.slice/user@1000.service/opencode.service",
	600:    "0::/user.slice/user-1000.slice/user@1000.service/background.slice/run-sleep.scope",
	3000:   "0::/user.slice/user-1000.slice/user@1000.service/session.slice/plasma-plasmashell.service",
	3100:   "0::/system.slice/sddm.service",
	3200:   "0::/user.slice/user-1000.slice/user@1000.service/session.slice/dbus-broker.service",
	3300:   "0::/user.slice/user-1000.slice/user@1000.service/session.slice/pipewire.service",
	3400:   "0::/system.slice/dbus-broker.service",
	3500:   "0::/user.slice/user-1000.slice/user@1000.service/session.slice/plasma-kwin_wayland.service",
}

func newKillGuard(t *testing.T) *KillGuardTool {
	t.Helper()
	root := t.TempDir()
	for _, p := range killGuardTree {
		dir := filepath.Join(root, strconv.Itoa(p.pid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		stat := strconv.Itoa(p.pid) + " (" + p.comm + ") S " + strconv.Itoa(p.ppid) + " 1 1 0"
		if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(strings.Join(p.args, "\x00")+"\x00"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "cgroup"), []byte(killGuardCgroups[p.pid]+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tool, err := NewKillGuardTool(core.ToolConfig{Enabled: true}, testLogger(t))
	if err != nil {
		t.Fatalf("NewKillGuardTool: %v", err)
	}
	tool.procRoot, tool.selfPID = root, 2200
	return tool
}

func validateKill(t *testing.T, command string) *core.ValidationResult {
	t.Helper()
	result, err := newKillGuard(t).ValidateTool(context.Background(), &core.ToolInput{ToolName: "Bash", Command: command})
	if err != nil {
		t.Fatalf("ValidateTool: %v", err)
	}
	return result
}

func TestKillGuardBlocks(t *testing.T) {
	for _, command := range []string{
		// PID родителя вычислен в той же команде и оказался systemd --user
		`p=$(ps -o ppid= -p 455273 | tr -d ' '); ps -o pid,args -p $p | tail -n 1; kill $p 455273 455274; sleep 1`,
		// pkill -f совпадает с текстом команды — убивает свою же оболочку
		`pkill -f 'run.sh reflect-longcat'; pkill -f 'farm-reflect-longcat'; sleep 2`,
		"kill `pgrep opencode`",
		`pgrep opencode | xargs kill -9`,
		`kill -9 -1`,
		`kill 1500`,
		`sudo kill -TERM 1`,
		`kill 2100`,
		`timeout 5 kill -s KILL 2000`,
		`killall systemd`,
		`pkill konsole`,
		`pkill -f "$NAME"`,
		// перенаправление не прячет опасную или вычисляемую цель
		`kill 1500 2>&1`,
		`kill -9 2100 2>/dev/null`,
		`kill $p 2>/dev/null`,
		`kill -0 455274 2>/dev/null && kill 1`,
		// подстановка процесса и команда в цели перенаправления — тоже команды
		`echo x | tee >(kill 1500)`,
		`echo x 2>$(kill 1500)`,
		// тело here-doc, которое исполнит оболочка, — команды
		"bash <<'EOF'\nkill 1500\nEOF",
		"cat <<EOF | sudo sh\nkill -9 2100\nEOF",
		"ssh host <<-EOF\n\tkill $p\n\tEOF",
		"cat <<'EOF' > /dev/null\ntext\nEOF\nkill 1500",
		// sudo -i/-s без команды читает скрипт из stdin, и за обёрткой тоже (akit v1.55.5)
		"nice sudo -i <<E\nkill 1500\nE",
		"sudo -u a sudo -s <<E\nkill $p\nE",
		`nice -n 5 sudo -s <<< 'kill 1500'`,
		"timeout 5 sudo -i <<'E'\nkill -9 2100\nE",
		// kill за ключевым словом оболочки (тикет kill-guard-gaps)
		`for p in $(pgrep -f foo); do kill $p; done`,
		`if kill $PID; then echo x; fi`,
		`if true; then echo; else kill 1500; fi`,
		`while true; do kill 1500; done`,
		`{ kill $PID; }`,
		`! kill $PID`,
		`time kill $PID`,
		`time -p kill 2100`,
		`builtin kill 1500`,
		// скрипт sh -c и eval — те же команды
		// в тикете — $PPID; та же ветка вычисляемого PID, $PPID в литерале
		// glint (signal-implicit-target, строгое правило) считает исполняемым сигналом родителю:
		// данные таблицы через поле структуры он не отличает от shell (тикет glint signal-table-through-struct-field отклонён)
		`bash -c 'kill -9 $PID'`,
		`sh -c "pkill -f claude"`,
		`bash -lc 'kill 1500'`,
		`sudo sh -c 'kill 2100'`,
		`eval "kill $PID"`,
		`bash -c "bash -c 'kill 1500'"`,
		`bash -o pipefail -c 'kill 1500'`,
		`bash -e -c -- 'kill 1500'`,
		// глубже предела вложенности разбор не идёт: отказ даже на безопасный PID
		`eval eval eval eval eval kill 455274`,
		// оболочка не разобрала команду со словом kill: отказ даже на
		// безопасный PID — kill за ошибкой разбора не виден
		`echo a(b); kill 455274`,
		`bash -c 'echo a(b); kill 455274'`,
		// фильтр без шаблона выбирает все процессы пользователя, родителя, терминала
		`pkill -u aisee`,
		`killall -u aisee`,
		`pkill -P 1`,
		`pkill -t pts/1`,
		`killall5`,
		`killall5 -9`,
		// -l у pkill не список сигналов (это флаг pgrep): справкой не считается
		`pkill -l -u aisee`,
		// обходы из проверки kill-guard-gaps (тикет kill-guard-gaps-2):
		// \ оболочка снимает
		`\kill -9 $PID`,
		`k\ill -9 $PID`,
		// скрипт оболочки из stdin
		`echo 'kill -9 $PID' | bash`,
		`bash <<< 'kill -9 $PID'`,
		`echo 'kill $PID' | bash -s`,
		`echo 'kill $PID' | bash -s arg1`,
		`echo a#b; kill 1500`,
		`pgrep foo | parallel kill`,
		`k'i'll $PID`,
		`bash -x <<< "kill 1500"`,
		// значение опции оболочки перед -c
		`bash -O extglob -c 'kill -9 $PID'`,
		`bash --rcfile /dev/null -c 'kill 1500'`,
		// значения опций обёрток
		`sudo -u aisee kill $PID`,
		`nice -n 10 kill $PID`,
		`timeout -s 9 5 kill $PID`,
		`FOO=1 kill $PID`,
		`xargs -I {} kill {} < pids`,
		// обёртки
		`stdbuf -oL kill $PID`,
		`flock /tmp/x kill $PID`,
		`flock -w 5 /tmp/x -c 'kill 1500'`,
		`busybox kill $PID`,
		`coproc kill $PID`,
		`parallel kill ::: 455274`,
		`find /proc -maxdepth 1 -exec kill {} +`,
		`find . -name x -exec echo {} \; -exec kill 1500 \;`,
		`find . -execdir kill -9 {} \;`,
		`find . -ok kill 1500 \;`,
		`find . -okdir kill 1500 \;`,
		`systemd-run --user --scope pkill -u aisee`,
		`ionice -c 3 kill 1500`,
		// строка-скрипт
		`watch -n1 'kill -9 $PID'`,
		`watch -n 1 pkill -u aisee`,
		`trap 'kill -9 $PID' EXIT`,
		`trap 'kill 1500' EXIT`,
		`su aisee -c 'kill 1500'`,
		`runuser -u aisee --command='kill 1500'`,
		`env -S 'pkill -u aisee'`,
		`env -i -S'kill 1500'`,
		`env --split-string='kill 1500'`,
		`env -u HOME -- kill 1500`,
		// полный разбор оболочки (тикет kill-guard-shellcmd): имя из
		// переменной, ANSI-C кавычки, функции, coproc, склеенные опции
		`K=kill; $K -9 $PID`,
		`$'kill' -9 $PID`,
		`$'pkill' -u aisee`,
		`function f { kill -9 $PID; }; f`,
		`f() { kill 1500; }; f`,
		`coproc K { kill -9 $PID; }`,
		`sudo -iu aisee kill -9 $PID`,
		`sudo -Eu aisee kill 1500`,
		`$(echo kill) -9 1500`,
		"`echo pkill` -u aisee",
		`K=xargs; pgrep foo | $K kill`,
		`K=pkill; $K -f "$NAME"`,
		// оболочка исполняет текст из ввода, который shellcmd не прочитал
		// (тикет kill-guard-script-input): kill в нём не виден как вызов
		`echo 'kill 1' | tee | bash`,
		`{ echo kill 1; } | bash`,
		`printf '%s' 'kill 1 #\c' | bash`,
		// process substitution и «bash -» — тоже ScriptInput
		`bash <(echo kill 1)`,
		`source <(echo kill 1)`,
		`. <(echo kill 1)`,
		`bash < <(echo kill 1)`,
		`echo kill 1 | bash -`,
		// скрипт eval и sh -c с подстановкой и ввод, который xargs -I и
		// parallel подставляют в скрипт (тикет kill-guard-stdin-gaps)
		`eval "$(echo kill 1)"`,
		`eval $(echo kill 1)`,
		`bash -c "$(echo kill 1)"`,
		`kill 600; eval "$X"`,
		`echo kill 1 | xargs -I{} bash -c {}`,
		`echo kill 1 | xargs -n 1 -I % sh -c '%'`,
		`echo kill 1 | xargs -i bash -c {}`,
		`echo kill 1 | parallel bash -c {}`,
		// те же признаки на shellcmd без своего лексера (тикет
		// kill-guard-shellcmd): скрипт с подстановкой у watch, trap, su -c и
		// у sh -c, который shellcmd разобрал ($X раскроется до запуска
		// оболочки), ввод в скрипт за обёрткой, parallel без :::, имя из
		// переменной перед kill
		`X='; kill -9 -1'; bash -c "echo $X"`,
		`watch "$X"; kill 455274`,
		`trap "$X" EXIT; kill 455274`,
		`su aisee -c "$X"; kill 455274`,
		`runuser -u aisee -c "$X"; kill 455274`,
		`eval "echo $X"; kill 455274`,
		`echo 'x; kill 1' | xargs -I{} sh -c 'echo {}'`,
		`echo 'x; kill 1' | xargs -I {} nice sh -c 'echo {}'`,
		`echo 'kill 1' | xargs --replace bash -c {}`,
		`seq 3 | parallel -j2 kill -9`,
		`$X kill 1`,
		"bash -c \"`echo kill 1`\"",
		`pgrep x | parallel /bin/kill`,
		// xargs за обёрткой (shellcmd отдаёт вызовами sudo и sh, xargs — в
		// Via), xargs с путём к оболочке; скрипт оболочки после опций со
		// значением и после --; env -S с подстановкой
		`echo 'kill 1' | sudo xargs -I{} sh -c {}`,
		`echo 'kill 1' | xargs -I{} /bin/sh -c {}`,
		`bash -eo pipefail -c "$X"; kill 455274`,
		`bash -O extglob -c "$X"; kill 455274`,
		`bash +O extglob -c "$X"; kill 455274`,
		`bash --rcfile /dev/null -c "$X"; kill 455274`,
		`bash -c -e "$X"; kill 455274`,
		`bash -c -- "$X"; kill 455274`,
		`env -S "$X"; kill 455274`,
		`env --split-string="$X"; kill 455274`,
		`env -iS"$X"; kill 455274`,
		// файл скрипта — дескриптор ввода, ввод из <&N и coproc-оболочка:
		// ScriptInput akit v1.72.2 (shellcmd-script-input-fd-coproc)
		`echo kill 1 | bash /dev/stdin`,
		`exec 3< <(echo kill 1); bash <&3`,
		`bash /dev/fd/3 3< <(echo kill 1)`,
		`bash 3< <(echo kill 1) /dev/fd/3`,
		`coproc bash; echo kill 1 >&${COPROC[1]}`,
		// heredoc и here-string, прочитанные оболочкой как скрипт через путь
		// дескриптора: вложенный скрипт akit v1.72.3 (shellcmd-heredoc-script-via-fd-path)
		"bash /dev/stdin <<'E'\nkill 1\nE",
		"bash /dev/fd/3 3<<'E'\nkill 1\nE",
		"exec 3<<'E'\nkill 1\nE\nbash <&3",
		`bash /dev/stdin <<< 'kill 1'`,
		// source и . с путём дескриптора и /dev/fd/$fd после exec {fd}<:
		// ScriptInput akit v1.72.3 (shellcmd-source-fd-path)
		`exec 3< <(echo kill 1); source /dev/fd/3`,
		`exec 3< <(echo kill 1); . /dev/fd/3`,
		`exec 3< <(echo kill 1); source /proc/self/fd/3`,
		`source /dev/fd/3 3< <(echo kill 1)`,
		`source /dev/stdin < <(echo kill 1)`,
		`echo kill 1 | source /dev/stdin`,
		`echo kill 1 | . /dev/stdin`,
		`echo kill 1 | source /proc/self/fd/0`,
		`exec {fd}< <(echo kill 1); bash /dev/fd/$fd`,
		`exec {fd}< <(echo kill 1); bash /proc/self/fd/$fd`,
		`exec {fd}< <(echo kill 1); source /dev/fd/$fd`,
		// комментарий с \ в конце: mvdan/sh продолжает его на следующую
		// строку, bash — нет; kill после него видит shellcmd (akit
		// shellcmd-comment-continuation)
		"echo x #\\\nkill 1",
		"true # a\\\nkill -9 1",
		// продолжение комментария съедает fi/done/}: разбор akit падает
		// (shellcmd-comment-parse-error), ошибка разбора — отказ
		"if true; then zz #\\\nfi; kill 1",
		"for i in 1; do zz #\\\ndone; kill 1",
		// строка тела heredoc без кавычек кончается \ + CR LF: mvdan/sh
		// склеивает её с ограничителем, а shellcmd находит вызовы после
		// heredoc или даёт ошибку разбора (akit shellcmd-carriage-return-words)
		"cat <<E\nq \\\r\nE\nkill 1",
		"cat <<E\r\nq \\\r\nE\r\nkill 1",
		"cat <<E\nq \\\r\nE\r\nkill $P\r\n",
		"cat <<E\nq \\\r\nE\npkill -f foo",
		// ограничитель heredoc в кавычках не целиком (<<"E"x): mvdan/sh
		// разбирает тело как некавыченное, \ + LF склеивает строку с
		// ограничителем; shellcmd находит вызовы после heredoc или даёт
		// ошибку разбора — отказ (akit v1.75.1, shellcmd-heredoc-partly-quoted-delimiter)
		"cat <<\"E\"x\nfoo \\\nEx\nkill -9 -1",
		"cat <<'E'x\n$(\nEx\nkill 1500",
		"cat <<E\"x\"\nfoo `\nEx\nkill 1500",
		"cat <<-\"E\"x\n\tfoo \\\n\tEx\nkill 1500",
		"cat <<\"\"\nfoo \\\n\nkill 1500",
		// setsid — обёртка, и с сокращёнными длинными ключами (akit v1.76.1,
		// shellcmd-setsid-ksh-fish); имя за setsid из подстановки — ошибка разбора
		`setsid kill -9 1500`,
		`setsid -f kill 1`,
		`setsid --fo kill -9 1500`,
		`setsid --wai kill 1`,
		`setsid -f -- kill 1`,
		`setsid -f bash -c 'kill -9 1'`,
		`nohup setsid fish -c 'kill 1500'`,
		`setsid $(echo kill) 1`,
		`K=kill; setsid $K 455274`,
		// ksh, mksh, fish — оболочки: скрипт из -c, here-string, heredoc и
		// конвейера с литеральным echo разбирается
		`ksh -c 'kill -9 1500'`,
		`mksh -c 'kill -9 1500'`,
		`fish -c 'kill -9 1500'`,
		`fish --command='kill 1'`,
		`ksh -ic 'kill 1500'`,
		`echo 'kill -9 1500' | fish`,
		`fish <<< 'kill -9 1500'`,
		`mksh <<< 'kill 1500'`,
		"ksh -s <<'E'\nkill 1\nE",
		// у fish разбирается простое подмножество: подстановка, переменная,
		// and/or, builtin первым словом — ошибка разбора; alias в ksh — тоже
		`fish -c 'kill (pgrep opencode)'`,
		`fish -c 'set p 1500; kill $p'`,
		`fish -c 'kill 455274; and kill 1'`,
		`fish -c "$X"; kill 455274`,
		`ksh -c 'alias k=kill; k 1'`,
		`cat f | fish; kill 455274`,
		// присваивание после опций sudo снимается (akit v1.76.2,
		// shellcmd-sudo-assign), и за ним — оболочки, ввод скрипта, ssh
		`sudo X=1 kill -9 1500`,
		`sudo -u root X=1 kill -9 1500`,
		// за sudo -- присваивание снимается (akit v1.77.1): kill — вызов
		// за sudo, и за обёртками перед sudo тоже
		`sudo -- X=1 kill -9 -1`,
		`sudo -u root -- X=1 kill -9 -1`,
		`sudo -- X=1 timeout 5 kill 2100`,
		`sudo -- X=a/b kill 1`,
		`sudo -u root -- X=a/b kill 1`,
		`sudo -- X=1 Y=2 kill 1500`,
		`timeout 5 sudo -- X=1 kill 2100`,
		`nice sudo -u aisee -- X=1 Y=2 kill 1500`,
		//nolint:signal-implicit-target // строка — случай теста kill_guard для ValidateTool: файл её не запускает, а kill во вложенной оболочке glint не прослеживает до помощника
		`sudo -- X=1 bash <<< 'kill -9 -1'`,
		`sudo -- X=1 bash -c 'kill -9 2100'`,
		`sudo -E X=1 kill 1`,
		`sudo X=1 Y=2 kill 1500`,
		`sudo X=1 timeout 5 kill 2100`,
		`sudo X=1 kill $PID`,
		`sudo X=1 bash -c 'kill 1500'`,
		`sudo X=1 sudo Y=2 kill 1`,
		`pgrep foo | xargs sudo X=1 kill`,
		`sudo 1X=2 kill 1`,
		`sudo X=1 nice kill 1`,
		`sudo X=1 bash <<< 'kill -9 1500'`,
		`echo 'kill -9 1500' | sudo X=1 bash`,
		"sudo X=1 sh -s <<'E'\nkill -9 -1\nE",
		"cat <<'E' | sudo X=1 bash\nkill -9 -1\nE",
		`sudo X=1 bash 0<<<'kill -9 1500'`,
		`sudo X=1 env bash <<< 'kill -9 1500'`,
		`sudo X=1 timeout 5 bash <<< 'kill -9 1500'`,
		`sudo X=1 ssh localhost 'kill -9 1500'`,
		`bash -c 'sudo X=1 bash <<< "kill -9 1500"'`,
		`eval 'sudo X=1 bash <<< "kill -9 1500"'`,
		// имя — раскрытие массива, которому присвоено слово kill
		`a=(kill -9 -1); "${a[@]}"`,
		`a=(kill -9 -1); ${a[@]}`,
		`declare -a a=(kill -9 -1); "${a[@]}"`,
		"a=(kill -9 -1)\n\"${a[@]}\"",
		`a=( kill -9 -1 ); "${a[@]}"`,
		"bash <<'E'\na=(kill -9 -1); \"${a[@]}\"\nE",
		`a[0]=kill; "${a[@]}" -9 -1`,
		`a+=(kill); "${a[*]}" 1`,
		`a=("kill" -9 -1); "${a[@]}"`,
		`a=(k'i'll -9 -1); "${a[@]}"`,
		`a=(kill); $a -9 -1`,
		`a=(kill); "${a}" -9 -1`,
		// <(…) в операнде ${…} (akit shellcmd-param-operand-procsubst)
		`echo ${X:-<(kill -9 -1)}`,
		`: ${X:+<(kill -9 -1)}`,
		`echo ${X:=<(kill -9 -1)}`,
		`echo ${X#<(kill -9 -1)}`,
		`cat ${X:-<(kill -9 -1)}`,
		`echo ${X:->(kill 1)}`,
		`echo ${X:-a <(kill 1)}`,
		`echo ${X/a/<(kill 1)}`,
		`echo ${X/<(kill 1)/a}`,
		`bash -c 'echo ${X:-<(kill 1)}'`,
		"bash <<'E'\necho ${X:-<(kill 1)}\nE",
		// <(…) в операнде ${…} вне аргументов вызова: присваивание,
		// перенаправление, [[ ]], for, select, case, вложенный скрипт
		`X=${Y:-<(kill -9 -1)}`,
		`local X=${Y:-<(kill -9 -1)}`,
		`export X=${Y:-<(kill -9 -1)}`,
		`X=${Y:-<(kill -9 -1)} true`,
		`echo hi > ${X:-<(kill -9 -1)}`,
		`cat < ${X:-<(kill -9 -1)}`,
		`[[ ${X:-<(kill -9 -1)} ]]`,
		`if [[ -n ${X:-<(kill -9 -1)} ]]; then :; fi`,
		`while [[ ${X:-<(kill -9 -1)} ]]; do break; done`,
		`for i in ${X:-<(kill -9 -1)}; do :; done`,
		`select i in ${X:-<(kill -9 -1)}; do break; done`,
		`case ${X:-<(kill -9 -1)} in *) ;; esac`,
		`bash -c '[[ ${X:-<(kill -9 -1)} ]]'`,
		`bash -c '[[ ${X:->(kill 1)} ]]'`,
		`X=${Y:-${Z}<(kill 1)}`,
		`X=${Y:-"}"<(kill 1)}`,
		`bash -c 'echo ${X:-"}"<(kill 1)}'`,
		// } в операнде перед <(…) (${Z}, "}", '}', \}) во вложенном скрипте
		// вне аргументов вызова и на верхнем уровне при незакрытом heredoc
		"bash -c '[[ ${X:-${Z}<(kill -9 -1)} ]]'",
		"bash -c 'for i in ${X:-${Z}<(kill -9 -1)}; do :; done'",
		"bash -c 'case ${X:-${Z}<(kill -9 -1)} in *) ;; esac'",
		"bash -c 'echo hi > ${X:-${Z}<(kill -9 -1)}'",
		"bash -c 'local X=${Y:-${Z}<(kill -9 -1)}'",
		"bash <<'E'\nX=${Y:-${Z}<(kill -9 -1)}\nE",
		"bash <<'E'\n[[ ${X:-${Z}<(kill -9 -1)} ]]\nE",
		"bash <<<'X=${Y:-${Z}<(kill -9 -1)}'",
		"X=${Y:-${Z}<(kill -9 -1)}; cat <<E\nfoo",
		"X=${Y:-\"}\"<(kill -9 -1)}; cat <<E\nfoo",
		"[[ ${X:-${Z}<(kill -9 -1)} ]]; cat <<E\nfoo",
		"for i in ${X:-${Z}<(kill -9 -1)}; do :; done; cat <<E\nfoo",
		"bash -c '[[ ${X:-\"}\"<(kill -9 -1)} ]]'",
		"bash -c '[[ ${X:-\\}<(kill -9 -1)} ]]'",
		"bash -c \"[[ \\${X:-'}'<(kill -9 -1)} ]]\"",
		"bash -c 'Y=1 X=${Y:-${Z}<(kill -9 -1)} true'",
		"bash -c 'export X=${Y:-${Z}<(kill -9 -1)}'",
		"bash -c 'select i in ${X:-${Z}<(kill -9 -1)}; do break; done'",
		"bash -c 'while [[ ${X:-${Z}<(kill -9 -1)} ]]; do break; done'",
		"bash -c 'cat < ${X:-${Z}<(kill -9 -1)}'",
		"bash -c 'X=${Y:-${Z}<(kill -9 -1)}; true'",
		"bash -c 'true; X=${Y:-${Z}<(kill -9 -1)}'",
		"eval '[[ ${X:-${Z}<(kill -9 -1)} ]]'",
		"eval 'local X=${Y:-${Z}<(kill -9 -1)}'",
		"sh -c '[[ ${X:-${Z}<(kill -9 -1)} ]]'",
		"bash <<'E'\nlocal X=${Y:-\"}\"<(kill -9 -1)}\nE",
		"bash <<<'[[ ${X:-${Z}<(kill -9 -1)} ]]'",
		"X=${Y:-'}'<(kill -9 -1)}; cat <<E\nfoo",
		"echo hi > ${X:-${Z}<(kill -9 -1)}; cat <<E\nfoo",
		"cat <<E; X=${Y:-${Z}<(kill -9 -1)}\nfoo",
		"[[ ${X:-${Z}<(kill -9 -1)} ]] <<E\nfoo",
		"bash -c \"[[ \\${X:-\\${Z}<(kill -9 -1)} ]]\"",
		"bash -lc '[[ ${X:-${Z}<(kill -9 -1)} ]]'",
		"nohup bash -c '[[ ${X:-${Z}<(kill -9 -1)} ]]'",
		"su -c '[[ ${X:-${Z}<(kill -9 -1)} ]]'",
		"watch '[[ ${X:-${Z}<(kill -9 -1)} ]]'",
		"trap '[[ ${X:-${Z}<(kill -9 -1)} ]]' EXIT",
		// ${ или <( собраны кавычками или \ во вложенном скрипте, <( из $'\x3c('
		"eval 'echo $''{X:-<(kill -9 -1)}'",
		"bash -c 'echo $''{X:-<(kill -9 -1)}'",
		"bash -c $'echo \\x24{X:-<(kill -9 -1)}'",
		"bash -c $'echo ${X:-\\x3c(kill -9 -1)}'",
		"bash -c $'echo ${X:-\\x3e(kill -9 -1)}'",
		"eval 'echo ${X:-<''(kill -9 -1)}'",
		"eval echo \\${X:-\\<\\(kill -9 -1\\)}",
		"bash -c \"echo \\$\"\"{X:-<(kill -9 -1)}\"",
		"eval 'echo $'{X:-'<(kill -9 -1)}'",
		"sh -c 'echo $''{X:-<(kill -9 -1)}'",
		"su -c 'echo $''{X:-<(kill -9 -1)}'",
		"watch 'echo $''{X:-<(kill -9 -1)}'",
		"trap 'echo $''{X:-<(kill -9 -1)}' EXIT",
		"bash -c 'X=$''{Y:-<(kill -9 -1)}; true'",
		"bash -c '[[ $''{X:-<(kill -9 -1)} ]]'",
		"bash <<< 'echo $''{X:-<(kill -9 -1)}'",
		"bash -c 'echo $''{X:->(kill -9 -1)}'",
		"bash <<< $'echo \\x24{X:-<(kill -9 -1)}'",
		"bash <<< $'echo ${X:-\\x3c(kill -9 -1)}'",
		"bash <<< 'X=$''{Y:-<(kill -9 -1)}'",
		// у zsh подстановка процесса и =(…)
		`zsh -c 'echo ${X:-=(kill -9 -1)}'`,
		`zsh -c 'echo ${X:-=(pkill -f claude)}'`,
		`zsh <<< 'echo ${X:-=(kill -9 -1)}'`,
		"zsh <<'E'\necho ${X:-=(kill -9 -1)}\nE",
		`z'sh' -c 'echo ${X:-=(kill -9 -1)}'`,
		`zsh -c 'true; X=${Y:-<(kill -9 -1)}'`,
		`zsh -c 'echo ${X:->(kill -9 -1)}'`,
		"zsh -c 'echo ${X:-\n=(kill -9 -1)}'",
		`zsh -c 'echo ${X:-${Y}=(kill -9 -1)}'`,
		`/bin/zsh -c 'echo ${X:-=(kill -9 -1)}'`,
		"zsh<<'E'\necho ${X:-=(kill -9 -1)}\nE",
		// временное правило: <(…) где-либо после ${ в команде со словом kill —
		// отказ, и ложный (в кавычках, в теле heredoc для файла, <(…) после
		// ${…}, а не внутри)
		`printf '%s' '${X:-<(kill 1)}'`,
		"cat > f <<'X'\necho ${X:-<(kill 1)}\nX",
		`echo ${X:-'<(kill 1)'}`,
		`echo ${X:-a}<(kill 455274)`,
		"X=${Y:-a\n<(kill -9 -1)}",
		`echo ${X}; diff <(a) <(b); kill 455274`,
		// k\ill за sudo -- — тот же kill
		`sudo -- X=1 k\ill -9 -1`,
		// второй -s или -n перекрывает сигнал 0
		`kill -0 -s 9 -- -1`,
		`kill -s 0 -s 9 -- -1`,
		`kill -n 0 -n 9 -- -1`,
		`kill --signal 0 --signal 9 -- -1`,
		`kill -s 0 --signal=9 -- -1`,
		`sudo kill -0 -s 9 1500`,
		`kill -s 0 -9 -1`,
		// bash исполняет скрипт и с +c
		`bash +c "$X"; kill 455274`,
		`kill -- 1500`,
		`K=kill; sudo $K -9 -1`,
		`K=kill; timeout 5 $K 1500`,
		`a=(kill); /bin/$a -9 -1`,
		`K=kill; env /usr/bin/$K 1500`,
		`kill --signal=9 1`,
		`kill -s 0 -s 9 1`,
		`kill -0 -s 9 1`,
		`K=kill; $P$K 1500`,
		// сигнал вычисляется при запуске: -$S бывает и «9 -1»
		`kill -$S 455274`,
	} {
		if validateKill(t, command).IsValid {
			t.Errorf("должно блокироваться: %s", command)
		}
	}
}

// Скрипт fish из нелитерального ввода и fish вне разбираемого подмножества
// akit не разбирает: отказ называет ошибку разбора, а не цель kill
func TestKillGuardParseErrorReason(t *testing.T) {
	for _, command := range []string{
		`cat f | fish; kill 455274`,
		`cat kill-stale.sh | fish -n`,
		`fish -c 'kill 455274; and true'`,
		`K=kill; setsid $K 455274`,
		`sudo X=1 fish -c 'kill 455274; and true'`,
		`K=kill; sudo X=1 nohup setsid $K 455274`,
	} {
		r := validateKill(t, command)
		if r.IsValid || !strings.Contains(strings.Join(r.Suggestions, "\n"), "разбор оболочки не прошёл") {
			t.Errorf("%s: ждали отказ «разбор оболочки не прошёл», получили %v %v", command, r.IsValid, r.Suggestions)
		}
	}
}

// killall5 бьёт по всей системе: причина называет это, а не «нет шаблона»
func TestKillGuardKillall5Reason(t *testing.T) {
	r := validateKill(t, `killall5 -15`)
	if r.IsValid || !strings.Contains(strings.Join(r.Suggestions, "\n"), "всем процессам системы") {
		t.Errorf("killall5: ждали отказ с причиной «всем процессам системы», получили %v %v", r.IsValid, r.Suggestions)
	}
}

// Один сигнал — одна причина: kill за parallel виден и снятым вызовом, и
// словом в аргументах parallel
func TestKillGuardReasonOnce(t *testing.T) {
	for _, command := range []string{`kill 1500`, `parallel kill ::: 455274`} {
		if r := validateKill(t, command); len(r.Violations) != 1 {
			t.Errorf("%s: ждали одну причину, получили %d: %v", command, len(r.Violations), r.Suggestions)
		}
	}
}

// Monitor исполняет command оболочкой, как Bash, — те же правила
func TestKillGuardMonitor(t *testing.T) {
	tool := newKillGuard(t)
	if !slices.Contains(tool.SupportedTools(), "Monitor") {
		t.Errorf("kill_guard не подписан на Monitor: %v", tool.SupportedTools())
	}
	result, err := tool.ValidateTool(context.Background(), &core.ToolInput{ToolName: "Monitor", Command: "kill $PID"})
	if err != nil {
		t.Fatalf("ValidateTool: %v", err)
	}
	if result.IsValid {
		t.Error("Monitor с kill $PID должен блокироваться")
	}
}

func TestKillGuardAllows(t *testing.T) {
	for _, command := range []string{
		`kill 455274`,
		`kill -9 455274 600`,
		`kill %1`,
		`kill -l`,
		`pkill -f '[f]arm-reflect-longcat'`,
		`pkill -x sleep`,
		`killall opencode`,
		`ps -eo pid,args | grep '[o]pencode'`,
		`echo "kill $p"`,
		// cat — не оболочка: kill в тексте не исполняется
		`echo 'kill 1' | cat`,
		// без слова kill сторож не вызывается
		`echo hi | bash`,
		`cat script.sh | bash`,
		// подстрока kill в пути (skills) — не слово kill: скрипт с подстановкой
		// и <(…) рядом с ней не отказ (ложные отказы на корпусе 13.11)
		`bash -c "F=/home/u/.claude/skills/x.tsv; head -1 \$F"`,
		`B=/tmp/s bash -c 'source <(sed -n "/^desc()/p" all.sh); f=/home/u/.claude/skills/a'`,
		`ls -d /home/u/git/*/ | xargs -I{} sh -c 'git -C {} remote get-url origin; ls {}/.claude/skills'`,
		// скрипт без подстановок проверяется сам
		`eval 'kill 455274'`,
		`bash -c 'echo $HOME; kill 455274'`,
		// перенаправление — не цель kill (ложные отказы 19–26.10)
		`kill 455274 2>&1`,
		`kill -0 455274 2>/dev/null`,
		`kill -0 455274 2>/dev/null && echo alive || echo gone`,
		`kill 455274 >/dev/null 2>&1; sleep 1`,
		`kill -TERM 455274 &>/dev/null`,
		`kill 455274 2> /tmp/kill.err`,
		// тело here-doc для файла или утилиты — текст, не команды (ложный
		// отказ aguard-master 27.10: комментарий «(kill -TERM, --resume)» в sub)
		"sed -n 1,2p decide.go && sub <<'EOF'\ndecide.go\n// замер 27.10 (kill -TERM, --resume) и (kill -TERM, -p --resume /compact)\nEOF",
		"cat > note.md <<-EOF\n\tkill $p\n\tEOF\nkill 455274",
		"graft commit -m \"fix: сброс\n(kill -TERM, --resume) и kill $p\"",
		// сигнал 0 никого не убивает (5 ложных отказов в живом логе до 11.11)
		`kill -0 $pid && echo alive`,
		`kill -s 0 $PID 2>/dev/null || echo gone`,
		`if kill -0 $PID; then kill 455274; fi`,
		`time kill 455274`,
		`bash -c 'kill 455274'`,
		`eval "kill 455274"`,
		`pkill -u aisee -x sleep`,
		`killall -l`,
		// ложные отказы из проверки kill-guard-gaps: комментарий и сигнал 0
		`echo hi # later; kill it`,
		"echo hi\n# kill $p\nls",
		`echo a#b; kill 455274`,
		`pkill -0 -u aisee`,
		`pkill --signal 0 -u aisee`,
		`killall -s 0 -u aisee`,
		`kill \
  455274`,
		`bash <<< 'kill 455274'`,
		// скрипт из heredoc через /dev/stdin разбирается: kill с PID в нём —
		// пропуск; source обычного файла — не ввод скрипта
		"bash /dev/stdin <<'E'\nkill 455274\nE",
		"bash /dev/stdin <<'E'\necho ok\nE\nkill 455274",
		`source ./env.sh; kill 455274`,
		`. ./env.sh && kill 455274`,
		// || — не конвейер: тело here-doc проверяется само
		"pgrep -x foo || bash <<'EOF'\nkill 455274\nEOF",
		`pkill -0 konsole`,
		`bash script.sh`,
		`cat kill-stale.sh | bash -n`,
		`echo ok | bash -n; grep kill log`,
		`kill 455274 || echo gone`,
		`trap - EXIT`,
		`trap -p`,
		`watch -n1 'kill -0 $PID'`,
		`find . -name '*.go' -exec grep -l kill {} +`,
		`timeout --kill-after=5 10 go version`,
		`$(git rev-parse --show-toplevel)/scripts/x.sh; echo kill`,
		`f() { kill 455274; }; f`,
		`K=kill; $K 455274`,
		`sudo -iu aisee kill -0 455274`,
		`pkill --help`,
		`pkill -V`,
		`killall --version`,
		`eval eval eval eval kill 455274`,
		`bash --norc 'kill 1500'`,
		// ввод xargs без -I — позиционные параметры скрипта, не его текст;
		// literal-скрипт с $ и подстановка у команды, не исполняющей скрипт
		`echo 1 | xargs bash -c 'echo $0'; kill 455274`,
		`echo 1 | xargs bash -c 'echo $0' -i; kill 455274`,
		`echo 1 | xargs sudo -i sh -c 'echo $0'; kill 455274`,
		`bash "$HOME/x.sh"; kill 455274`,
		`bash --norc "$HOME/x.sh"; kill 455274`,
		`bash -c 'A=$HOME true'; kill 455274`,
		`$HOME/bin/x; kill 455274`,
		`echo "$HOME"; kill 455274`,
		`bash -c 'echo "$1"' _ x; kill 455274`,
		// слова после скрипта — его позиционные параметры, у файла скрипта —
		// его аргументы, не опции оболочки; -i до xargs — опция sudo;
		// xargs -I в другой команде; kill внутри чужого слова у вычисляемого
		// имени; переменная в имени за обёрткой (G="git …"; timeout 60 $G)
		`bash -c 'echo "$0"' "$P"; kill 455274`,
		`bash x.sh -count=1 "$X"; kill 455274`,
		`bash -o pipefail x.sh -c "$X"; kill 455274`,
		`bash -- x.sh -c "$X"; kill 455274`,
		`env -u S "$X"; kill 455274`,
		`env -uS "$X"; kill 455274`,
		`echo 1 | sudo -i xargs sh -c 'echo $0'; kill 455274`,
		`echo a | xargs -I{} echo sh {}; echo 1 | xargs sh -c 'echo $0'; kill 455274`,
		`for b in /bin/true; do $b check -r raw-process-kill .; done`,
		`G="git -C /tmp"; timeout 60 $G log -1; kill 455274`,
		// setsid, ksh, mksh и fish с безопасной целью: скрипт разобран,
		// kill в нём проверен
		`setsid kill 455274`,
		`setsid -f kill 455274`,
		`setsid --fo kill 455274`,
		`setsid --wait kill -0 455274`,
		`setsid sleep 1; kill 455274`,
		`setsid -w ksh -c 'kill 455274'`,
		`setsid -f fish -c 'kill 455274'`,
		`ksh -c 'kill 455274'`,
		`mksh -c 'kill 455274'`,
		`fish -c 'kill 455274'`,
		`fish <<< 'kill 455274'`,
		`mksh <<< 'kill 455274'`,
		"ksh -s <<'E'\nkill 455274\nE",
		`fish -c 'echo hi'; kill 455274`,
		`fish -c 'string match kill x'`,
		`cat kill-stale.sh | ksh -n`,
		// за присваиванием после sudo — безопасная цель или не сигнал
		`sudo X=1 kill 455274`,
		`sudo -u root X=1 kill -0 455274`,
		`sudo X=1 echo kill`,
		`sudo X=1; kill 455274`,
		`sudo X=1 echo 'x; kill 1'`,
		// массив без слова kill, другой массив, kill — не имя
		`a=(echo hi); "${a[@]}"; kill 455274`,
		`b=(kill 1); "${a[@]}"`,
		`a=(kill 1); echo "${a[@]}"`,
		`ab=(kill 1); "${b[@]}"`,
		// <(…) вне ${…} и ${…} без <(…)
		`cat <(echo hi) ${X}; kill 455274`,
		`echo ${X:-default}; kill 455274`,
		// сигнал 0 во всех ключах
		`kill -0 -s 0 455274`,
		`kill -s 0 -n 0 455274`,
		`kill --signal=0 455274`,
		`kill -L`,
		`bash +c 'echo hi'; kill 455274`,
		`echo ${X:-<(ls skills)}`,
		`echo ${X/a}; kill 455274`,
		// ${ без <( и >( после него; <(…) до ${
		`echo ${X:-(a)}; kill 455274`,
		`echo "${NAME:-<unset>}"; kill 455274`,
		`kill 455274; echo ${X}`,
		`diff <(a) <(b); kill 455274; echo ${X}`,
		// $ без { и { без $ перед <(…)
		`echo $X; diff <(a) <(b); kill 455274`,
		`{ diff <(a) <(b); }; kill 455274`,
		// =( без zsh — массив, а не подстановка процесса
		`arr=(a b); kill 455274; echo ${X}`,
		`echo ${X}; arr=(a b); kill 455274`,
		`cat ~/.zshrc; echo ${X}; arr=(a b); kill 455274`,
		`zsh -c 'arr=(a b)'; kill 455274`,
		`zsh -c 'f() { a=(1); }'; kill 455274`,
		`ls oh-my-zsh; echo ${X}; arr=(1); kill 455274`,
		`cat zshenv; echo ${X}; arr=(1); kill 455274`,
		"cat <<E\nkill -l",
		// = в имени команды за sudo -- — не присваивание
		`sudo -- ./job=1.sh --mode kill`,
		// -L и --table печатают сигналы, -s без значения — ошибка kill
		`kill -L 1`,
		`kill --table 1`,
		`kill -s`,
		`kill -9 -s`,
		`kill -l 1`,
		`kill -9 -- 455274`,
		`kill --signal=0 1`,
		// сигнал задаёт последняя опция: шлётся 0
		`kill -s 9 -s 0 1`,
		`kill -9 -s 0 1`,
		`kill -s 0 -- 1`,
		// без -c первый операнд — файл скрипта, не текст
		`bash -x 'kill 1'`,
		`bash -x "$X"; kill 455274`,
		// присваивание за sudo -- без слова kill и kill за sudo без него;
		// за sudo -- X=1 — kill чужого PID литералом
		`sudo -- X=1 make test; kill 455274`,
		`sudo -- X=1 kill 455274`,
		`timeout 5 sudo -u root -- X=a/b kill -TERM 455274`,
		`sudo -- kill 455274`,
		`echo -- X=1 kill`,
		`sudo --; kill 455274`,
		`kill -s 0 1`,
	} {
		if r := validateKill(t, command); !r.IsValid {
			t.Errorf("не должно блокироваться: %s: %v", command, r.Suggestions)
		}
	}
}

// sh -c в sh -c: глубже maxScriptNesting — отказ даже на безопасный PID, как у eval
func TestKillGuardShellNesting(t *testing.T) {
	nested := func(n int) string {
		script := "kill 455274"
		for range n {
			script = "bash -c '" + strings.ReplaceAll(script, "'", `'"'"'`) + "'"
		}
		return script
	}
	if r := validateKill(t, nested(maxScriptNesting)); !r.IsValid {
		t.Errorf("вложенность %d: ждали пропуск, получили %v", maxScriptNesting, r.Suggestions)
	}
	if validateKill(t, nested(maxScriptNesting+1)).IsValid {
		t.Errorf("вложенность %d: ждали отказ", maxScriptNesting+1)
	}
}

// Отрицательный PID меньше -1 — одна группа процессов, -1 — все процессы
// пользователя, 0 — своя группа, 1 — init: причины различимы, отказ во всех
func TestKillGuardGroupReasons(t *testing.T) {
	for _, tc := range []struct{ command, want, not string }{
		{`kill -TERM -- -1447549`, "группе процессов 1447549", "все процессы"},
		{`kill -- -2`, "группе процессов 2", "init"},
		{`kill -9 -1`, "всем процессам пользователя", "группе процессов"},
		{`kill -TERM 0`, "своей группе процессов", "всем процессам"},
		{`kill 1`, "init", "группе процессов"},
	} {
		r := validateKill(t, tc.command)
		reasons := strings.Join(r.Suggestions, "\n")
		if r.IsValid || !strings.Contains(reasons, tc.want) || strings.Contains(reasons, tc.not) {
			t.Errorf("%s: ждали отказ с «%s» и без «%s», получили %v %q", tc.command, tc.want, tc.not, r.IsValid, reasons)
		}
	}
}

// Подсказка к отказу на группу не выдаёт завершение отдельных PID за то же
// самое; к прочим отказам — обычная подсказка про литеральный PID
func TestKillGuardGroupHint(t *testing.T) {
	group := validateKill(t, `kill -TERM -- -1447549`).Suggestions
	if slices.Contains(group, killGuardHint) || !slices.Contains(group, killGroupHint) {
		t.Errorf("группа: ждали только подсказку группы, получили %q", group)
	}
	mixed := validateKill(t, `kill -TERM -- -1447549; kill 1`).Suggestions
	if !slices.Contains(mixed, killGuardHint) || !slices.Contains(mixed, killGroupHint) {
		t.Errorf("группа и init: ждали обе подсказки, получили %q", mixed)
	}
	if plain := validateKill(t, `kill 1`).Suggestions; slices.Contains(plain, killGroupHint) {
		t.Errorf("init: подсказка группы лишняя: %q", plain)
	}
}
