package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiseeq/chooks/internal/core"
)

// gitGuardTree — общее дерево мастера (.git — каталог) с файлом main.go и
// linked worktree суб-агента (.git — файл)
func gitGuardTree(t *testing.T) (shared, own string) {
	t.Helper()
	root := t.TempDir()
	shared, own = filepath.Join(root, "repo"), filepath.Join(root, "wt")
	for _, dir := range []string{filepath.Join(shared, ".git"), filepath.Join(shared, "pkg"), own} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for path, text := range map[string]string{
		filepath.Join(shared, "main.go"): "package main\n",
		filepath.Join(own, ".git"):       "gitdir: " + filepath.Join(shared, ".git", "worktrees", "wt") + "\n",
	} {
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return shared, own
}

func TestGitGuard(t *testing.T) {
	shared, own := gitGuardTree(t)
	logger, err := core.NewLogger(core.DefaultLoggerConfig())
	if err != nil {
		t.Fatal(err)
	}
	tool, _ := NewGitGuardTool(core.ToolConfig{Enabled: true}, logger)
	// Дерево теста само лежит во временном каталоге: временные — свои
	scratch := filepath.Join(t.TempDir(), "claude-tmp")
	t.Setenv("SCRATCH", scratch)
	tool.tempRoots = []string{scratch, "/var/tmp/gg-test"}
	cases := []struct {
		name, command string
		reject        bool
	}{
		{"reset --hard", "git reset --hard HEAD", true},
		{"reset --merge", "git reset --merge", true},
		{"restore", "git restore main.go", true},
		{"restore -W --staged", "git restore --staged --worktree main.go", true},
		{"checkout --", "git checkout -- main.go", true},
		{"checkout файла без --", "git checkout main.go", true},
		{"checkout .", "git checkout .", true},
		{"checkout коммит путь", "git checkout HEAD~1 pkg", true},
		{"checkout -f", "git checkout -f main", true},
		{"checkout -B", "git checkout -B main origin/main", true},
		{"clean", "git clean -fd", true},
		{"stash", "git stash", true},
		{"stash push", "git stash push -m x", true},
		{"stash drop", "git stash drop", true},
		{"switch --discard-changes", "git switch --discard-changes main", true},
		{"push --force", "git push --force origin main", true},
		{"push -f", "git push -fu origin main", true},
		{"push --force-with-lease", "git push --force-with-lease=main origin main", true},
		{"push +refspec", "git push origin +main", true},
		{"push --delete", "git push origin --delete feature", true},
		{"push :ветка", "git push origin :feature", true},
		{"за cd и -C", "cd /tmp && git -C " + shared + " reset --hard", true},
		{"в bash -c", `bash -c "git stash"`, true},
		{"за обёрткой", "timeout 10 git clean -fdx", true},
		{"--git-dir: дерево неизвестно", "git --git-dir=" + own + "/.git reset --hard", true},
		{"--work-tree словом: дерево неизвестно", "cd " + own + " && git --work-tree " + shared + " reset --hard", true},
		// Безобидные формы (решение 12.11 в тикете)
		{"stash list", "git stash list", false},
		{"stash show", "git stash show -p stash@{0}", false},
		{"restore --staged", "git restore --staged main.go", false},
		{"restore -S", "git restore -S main.go", false},
		{"checkout ветки", "git checkout main", false},
		{"checkout -b", "git checkout -b feature", false},
		{"switch", "git switch main", false},
		{"clean -n", "git clean -nd", false},
		{"reset без --hard", "git reset main.go", false},
		{"push", "git push origin main", false},
		{"status", "git status && git diff", false},
		{"не git", "echo 'git reset --hard'", false},
		{"grep", "grep -rn 'git stash' docs", false},
		// Свой linked worktree
		{"свой worktree", "cd " + own + " && git reset --hard && git stash", false},
		{"свой worktree через -C", "git -C " + own + " clean -fdx", false},
		{"свой worktree: push --force-with-lease", "cd " + own + " && git push --force-with-lease=f:abc --force-if-includes origin HEAD:refs/heads/f", false},
		{"свой worktree: push --force", "cd " + own + " && git push --force origin HEAD:main", true},
		{"свой worktree: lease и +ref", "cd " + own + " && git push --force-with-lease origin +HEAD:main", true},
		{"свой worktree: push --delete", "git -C " + own + " push origin --delete main", true},
		{"клон в scratchpad: push --force", "cd " + filepath.Join(scratch, "s", "repo") + " && git push --force origin main", false},
		{"клон в scratchpad через переменную окружения", "cd $SCRATCH/s/repo && git reset --hard", true},
		{"клон в scratchpad", "cd " + filepath.Join(scratch, "s", "repo") + " && git reset --hard && git clean -fdx", false},
		{"клон во втором временном", "git -C /var/tmp/gg-test/x/repo stash", false},
		{"соседний с временным", "git -C /var/tmp/gg-test2/repo stash", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := &core.ToolInput{ToolName: "Bash", Command: tc.command, CWD: shared, AgentID: "a1", AgentType: "fighter"}
			got, err := tool.ValidateTool(context.Background(), input)
			if err != nil {
				t.Fatal(err)
			}
			if got.IsValid == tc.reject {
				t.Fatalf("valid=%v violations=%v", got.IsValid, got.Violations)
			}
			if tc.reject && !strings.Contains(got.Violations[0].Message, "напиши об этом в итоге мастеру") {
				t.Errorf("нет подсказки: %s", got.Violations[0].Message)
			}
			// Основная сессия делает их по «да» пользователя — без отказа
			input.AgentID, input.AgentType = "", ""
			if main, err := tool.ValidateTool(context.Background(), input); err != nil || !main.IsValid {
				t.Errorf("основной сессии отказ: %v %v", err, main)
			}
		})
	}
}

// Форк — тоже суб-агент: работает в дереве мастера
func TestGitGuardFork(t *testing.T) {
	shared, _ := gitGuardTree(t)
	logger, err := core.NewLogger(core.DefaultLoggerConfig())
	if err != nil {
		t.Fatal(err)
	}
	tool, _ := NewGitGuardTool(core.ToolConfig{Enabled: true}, logger)
	tool.tempRoots = nil // дерево теста лежит во временном каталоге
	got, err := tool.ValidateTool(context.Background(), &core.ToolInput{ToolName: "Bash", Command: "git stash", CWD: shared, AgentID: "f1", AgentType: "fork"})
	if err != nil || got.IsValid {
		t.Errorf("форку git stash: %v %+v", err, got)
	}
}
