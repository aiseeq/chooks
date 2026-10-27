package tools

import (
	"context"
	"os"
	"path/filepath"
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
	} {
		if validateKill(t, command).IsValid {
			t.Errorf("должно блокироваться: %s", command)
		}
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
	} {
		if r := validateKill(t, command); !r.IsValid {
			t.Errorf("не должно блокироваться: %s: %v", command, r.Suggestions)
		}
	}
}
