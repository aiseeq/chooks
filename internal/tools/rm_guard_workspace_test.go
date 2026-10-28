package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiseeq/chooks/internal/core"
)

func validateRmIn(t *testing.T, command, cwd string) *core.ValidationResult {
	t.Helper()
	result, err := newRmGuard(t).ValidateTool(context.Background(),
		&core.ToolInput{ToolName: "Bash", Command: command, CWD: cwd})
	if err != nil {
		t.Fatalf("ValidateTool: %v", err)
	}
	return result
}

// TestRmGuardWorkspaceMatchesClaudeCode третья форма: каждую команду прогнали
// через claude -p --model opus в bypassPermissions (Claude Code 2.1.312) из
// песочницы /…/lab/proj с --add-dir /…/lab/extra; двухшаговые — первым вызовом
// cd в cwd из таблицы. true — Claude Code остановил её запросом «This command
// would remove a workspace directory». Не повторяем: rm -rf lab/extra/* —
// дополнительную рабочую папку хук не видит. cd …/extra && echo a | tail -1;
// rm -rf …/proj Claude Code останавливает раньше другим запросом («multiple
// operations … requires approval»), хук — по рабочей папке
func TestRmGuardWorkspaceMatchesClaudeCode(t *testing.T) {
	const proj = "/home/u/lab/proj"
	for _, tc := range []struct {
		cwd, command string
		deny         bool
	}{
		{proj, `rm -rf /home/u/lab/proj`, true},
		{proj, `rm -rf /home/u/lab`, true},
		{proj, `rm -rf /home/u/lab/proj/sub`, false},
		{proj, `rm -rf /home/u/lab/proj/*`, true},
		{proj, `rm -rf *`, true},
		{proj, `rm -rf .`, true},
		{proj, `rm -rf /home/u/lab/extra`, false},
		{proj + "/sub", `rm -rf /home/u/lab/proj/sub`, true},
		{proj + "/sub", `rm -rf /home/u/lab/proj/sub/deep`, false},
		{"/home/u/lab/extra", `rm -rf /home/u/lab/proj`, false},
		{proj, `rm -rf ../proj`, true},
		{proj, `rm -rf /home/u/lab/proj/sub/..`, true},
		{proj, `rm -rf /home/u/lab/proj/`, true},
		{proj, `rm -rf /home/u/lab/link`, false},
		{proj, `cd /home/u/lab/extra && rm -rf /home/u/lab/proj`, false},
		{proj, `cd /home/u/lab/proj/sub && rm -rf /home/u/lab/proj/sub`, true},
		{proj, `cd /home/u/lab/proj/sub; rm -rf /home/u/lab/proj/sub`, false},
		{proj + "/sub", `cd /home/u/lab/proj && rm -rf /home/u/lab/proj/sub`, false},
		{proj, `true && cd /home/u/lab/extra && rm -rf /home/u/lab/proj`, true},
		{proj, `cd /home/u/lab/extra && true && rm -rf /home/u/lab/proj`, false},
		{proj, `cd /home/u/lab/extra; rm -rf /home/u/lab/proj`, true},
		{proj, `cd /home/u/lab/extra && rm -rf /home/u/lab/proj; ls`, true},
		{proj, `cd /home/u/lab/extra && true; rm -rf /home/u/lab/proj`, true},
		{proj, `true; rm -r /home/u/lab/proj`, true},
		{proj, `cd /home/u/lab/extra || exit 1; rm -rf /home/u/lab/proj`, true},
		{proj, `(cd /home/u/lab/extra && rm -rf /home/u/lab/proj)`, true},
		{proj, `cd ../extra && rm -rf /home/u/lab/proj`, true},
		{proj, `cd /home/u/lab/proj/sub && rm -rf ../sub`, true},
	} {
		if got := !validateRmIn(t, tc.command, tc.cwd).IsValid; got != tc.deny {
			t.Errorf("cwd %s: %q: отказ %v, у Claude Code %v", tc.cwd, tc.command, got, tc.deny)
		}
	}
}

// TestRmGuardWorkspaceTranscripts случаи из транскриптов 26–27.10, на которых
// Claude Code ждал окно разрешения 120 с и отказал
func TestRmGuardWorkspaceTranscripts(t *testing.T) {
	const tmp = "/home/u/.cache/claude-tmp"
	for _, tc := range []struct{ cwd, command string }{
		{tmp + "/tmp.Xlb6h1KaEC", `rm -rf /home/u/.cache/claude-tmp/tmp.Xlb6h1KaEC /home/u/.cache/claude-tmp/tmp.WpCzHsmq1J; cd /home/u/git/graft && git status --short; date '+%H:%M %Z'`},
		{tmp + "/s/scratchpad/app-copy", `rm -rf /home/u/.cache/claude-tmp/s/scratchpad/app-copy; cd /home/u/git/glint && echo "- $(date '+%Y-%m-%d %H:%M') ok"`},
		{tmp + "/s/scratchpad/crontest", `tmux kill-session -t crontest; sleep 2; pgrep -a -f '[e]424f001' ; rm -rf /home/u/.cache/claude-tmp/s/scratchpad/crontest; ls -d ~/.claude/projects/*crontest | sed "s|$HOME|~|"`},
		{tmp + "/s/scratchpad/app-clone", `docker compose -p app-check down -v 2>&1 | tail -3 && cd /home/u/work/app && rm -rf /home/u/.cache/claude-tmp/s/scratchpad/app-clone && docker volume ls -q | grep -c app-check`},
	} {
		r := validateRmIn(t, tc.command, tc.cwd)
		if r.IsValid {
			t.Errorf("должно отклоняться: %q", tc.command)
			continue
		}
		if !strings.Contains(strings.Join(r.Suggestions, "\n"), "рабочую папку Bash "+tc.cwd) {
			t.Errorf("%q: нет причины про рабочую папку: %q", tc.command, r.Suggestions)
		}
	}
}

// TestRmGuardWorkspaceReason подсказка уводит отдельным cd в папку проекта, и
// rm оттуда сам проходит проверку
func TestRmGuardWorkspaceReason(t *testing.T) {
	rmGuardEnv["CLAUDE_PROJECT_DIR"] = "/home/u/git/glint"
	t.Cleanup(func() { delete(rmGuardEnv, "CLAUDE_PROJECT_DIR") })
	const cwd = "/home/u/.cache/claude-tmp/s/copy"
	r := validateRmIn(t, `rm -rf /home/u/.cache/claude-tmp/s/copy; cd /home/u/git/glint`, cwd)
	if r.IsValid {
		t.Fatal("должно отклоняться")
	}
	if !strings.Contains(r.Violations[0].Message, "рабочей папки") {
		t.Errorf("заголовок: %q", r.Violations[0].Message)
	}
	want := "Сначала отдельным вызовом `cd /home/u/git/glint`, потом отдельным вызовом `rm -rf /home/u/.cache/claude-tmp/s/copy`"
	if got := strings.Join(r.Suggestions, "\n"); !strings.Contains(got, want) {
		t.Errorf("\nесть  %q\nждали %q", got, want)
	}
	if !validateRmIn(t, `rm -rf /home/u/.cache/claude-tmp/s/copy`, "/home/u/git/glint").IsValid {
		t.Error("rm из папки проекта отклоняется")
	}
}

// TestRmGuardWorkspaceCritical корень, верхний уровень и домашний Claude Code
// останавливает своей проверкой критических путей — подсказка cd им не поможет
func TestRmGuardWorkspaceCritical(t *testing.T) {
	for _, command := range []string{`rm -rf /`, `rm -rf /home`, `rm -rf ~`, `rm -rf /home/u`, `rm -rf $HOME/lab/proj`} {
		r := validateRmIn(t, command, "/home/u/lab/proj")
		if strings.Contains(strings.Join(r.Suggestions, "\n"), "рабочую папку") {
			t.Errorf("%q: причина про рабочую папку: %q", command, r.Suggestions)
		}
	}
}

// TestRmGuardWorkspaceSymlinkCwd рабочая папка через ссылку: Claude Code
// сверяет цель и с настоящим путём папки
func TestRmGuardWorkspaceSymlinkCwd(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	link := filepath.Join(dir, "link")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if validateRmIn(t, "rm -rf "+real, link).IsValid {
		t.Errorf("rm -rf %s из %s должно отклоняться", real, link)
	}
}

// TestRmGuardWorkspaceRealPathError сбой realpath — ошибка проверки, а не
// молчаливый пропуск
func TestRmGuardWorkspaceRealPathError(t *testing.T) {
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	_, err := newRmGuard(t).ValidateTool(context.Background(),
		&core.ToolInput{ToolName: "Bash", Command: "rm -rf /x", CWD: filepath.Join(locked, "cwd")})
	if err == nil && os.Geteuid() != 0 {
		t.Error("нет ошибки realpath в папке без прав")
	}
}
