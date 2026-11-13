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
		// glint ложно считает исполняемым сигналом родителю
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
		// их лексер уже ловил как «… | bash»; process substitution и
		// «bash -» видит только ScriptInput полного разбора
		`bash <(echo kill 1)`,
		`source <(echo kill 1)`,
		`. <(echo kill 1)`,
		`bash < <(echo kill 1)`,
		`echo kill 1 | bash -`,
	} {
		if validateKill(t, command).IsValid {
			t.Errorf("должно блокироваться: %s", command)
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

// лексер и полный разбор находят один и тот же kill: причина — одна
func TestKillGuardReasonOnce(t *testing.T) {
	r := validateKill(t, `kill 1500`)
	if len(r.Violations) != 1 {
		t.Errorf("ждали одну причину, получили %d: %v", len(r.Violations), r.Suggestions)
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
	} {
		if r := validateKill(t, command); !r.IsValid {
			t.Errorf("не должно блокироваться: %s: %v", command, r.Suggestions)
		}
	}
}
