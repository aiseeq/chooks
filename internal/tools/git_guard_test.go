package tools

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/aiseeq/akit/shellcmd"
	"github.com/aiseeq/chooks/internal/core"
)

// gitGuardTree — общее дерево мастера (.git — каталог) с файлом main.go и
// подмодулем sub, linked worktree суб-агента (.git — файл) на ветке f
func gitGuardTree(t *testing.T) (shared, own string) {
	t.Helper()
	root := t.TempDir()
	shared, own = filepath.Join(root, "repo"), filepath.Join(root, "wt")
	wtGit := filepath.Join(shared, ".git", "worktrees", "wt")
	for _, dir := range []string{wtGit, filepath.Join(shared, ".git", "modules", "sub"), filepath.Join(shared, "pkg"), filepath.Join(shared, "sub"), own} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for path, text := range map[string]string{
		filepath.Join(shared, "main.go"):                        "package main\n",
		filepath.Join(shared, "sub", ".git"):                    "gitdir: ../.git/modules/sub\n",
		filepath.Join(own, ".git"):                              "gitdir: " + wtGit + "\n",
		filepath.Join(wtGit, "HEAD"):                            "ref: refs/heads/f\n",
		filepath.Join(shared, ".git", "modules", "sub", "HEAD"): "ref: refs/heads/main\n",
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
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(shared, filepath.Join(scratch, "link")); err != nil {
		t.Fatal(err)
	}
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
		{"checkout по шаблону", "git checkout '*.go'", true},
		{"checkout pathspec :/", "git checkout :/", true},
		{"checkout --pathspec-from-file", "git checkout --pathspec-from-file=list", true},
		{"reset --har", "git reset --har", true},
		{"checkout --forc", "git checkout --forc main", true},
		{"switch --discard", "git switch --discard main", true},
		{"switch -qf", "git switch -qf main", true},
		{"switch -C", "git switch -C main", true},
		{"checkout -qB", "git checkout -qB main", true},
		{"checkout -p", "git checkout -p", true},
		{"push --del", "git push --del origin x", true},
		{"push --mirr", "git push --mirr origin", true},
		{"push --force-w", "git push --force-w origin main", true},
		{"push --pru", "git push --pru origin", true},
		{"push --force-if-includes", "git push --force-if-includes origin main", true},
		{"push -- +ref", "git push -- origin +main", true},
		{"push -- :ref", "git push -- origin :main", true},
		{"clean -e слитно", "git clean -fdx -enode_modules", true},
		{"clean --dry-run", "git clean --dry-run -d", false},
		{"GIT_DIR из своего worktree", "cd " + own + " && GIT_DIR=" + shared + "/.git git reset --hard", true},
		{"export GIT_WORK_TREE", "cd " + own + " && export GIT_WORK_TREE=" + shared + " && git reset --hard", true},
		{"pushd в дерево мастера", "cd " + own + " && pushd " + shared + " && git reset --hard", true},
		{"после popd каталог неизвестен", "pushd " + own + " && popd && git reset --hard", true},
		{"симлинк из временного", "git -C " + filepath.Join(scratch, "link") + " reset --hard", true},
		{"алиас через -c", "git -c alias.w='reset --hard' w", true},
		{"алиас через --config-env", "W='reset --hard' git --config-env=alias.w=W w", true},
		{"алиас через GIT_CONFIG_*", "GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=alias.w GIT_CONFIG_VALUE_0='reset --hard' git w", true},
		{"-c remote.*.push", "git -c remote.origin.push=+HEAD:main push origin", true},
		{"--config-env remote.*.mirror", "git --config-env remote.origin.mirror=M push origin", true},
		{"clean -e -n", "git clean -fdx -e -n", true},
		{"clean --exclude --dry-run", "git clean -f --exclude --dry-run", true},
		{"clean -n -e x", "git clean -n -e x", false},
		{"rm --cached -f", "git rm --cached -f main.go", false},
		{"(cd свой) и git в дереве мастера", "(cd " + own + "); git reset --hard", true},
		{"cd свой в конвейере", "cd " + own + " | true; git reset --hard", true},
		{"cd свой в bash -c", "bash -c 'cd " + own + "'; git reset --hard", true},
		{"(cd свой && git) — свой", "(cd " + own + " && git reset --hard)", false},
		{"pushd -n", "pushd -n " + own + " && git reset --hard", true},
		{"ссылка в той же команде", "cd " + scratch + " && ln -s " + shared + " l2 && git -C l2 reset --hard", true},
		{"клон в той же команде", "git clone x " + scratch + "/c2 && cd " + scratch + "/c2 && git reset --hard", false},
		{"-c без алиаса", "git -c core.pager=cat reset --hard", true},
		{"-c без алиаса: status", "git -c core.pager=cat status", false},
		{"изоляция настроек", "GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null git status; env -u GIT_CONFIG_PARAMETERS git log", false},
		{"неизвестный каталог: checkout пути", "git --git-dir=x checkout pkg/a", true},
		{"неизвестный каталог: checkout ветки", "git --git-dir=x checkout main", false},

		{"rm -f", "git rm -f main.go", true},
		{"checkout-index -f", "git checkout-index -f -a", true},
		{"read-tree --reset -u", "git read-tree -u --reset HEAD", true},

		{"reflog expire", "git reflog expire --expire=now --all", true},
		{"подмодуль — дерево мастера", "git -C " + filepath.Join(shared, "sub") + " reset --hard", true},
		// Безобидные формы (решение 12.11 в тикете)
		{"stash list", "git stash list", false},
		{"stash show", "git stash show -p stash@{0}", false},
		{"restore --staged", "git restore --staged main.go", false},
		{"restore -S", "git restore -S main.go", false},
		{"checkout ветки", "git checkout main", false},
		{"checkout -b", "git checkout -b feature", false},
		{"checkout -b слитно", "git checkout -bfix", false},
		{"branch -D (уборка своих веток)", "git branch -D x", false},
		{"worktree remove --force (уборка своих worktree)", "git worktree remove --force ../wt2", false},
		{"rm", "git rm main.go", false},
		{"read-tree", "git read-tree HEAD", false},
		{"reflog", "git reflog show", false},
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
		{"свой worktree: lease без refspec", "cd " + own + " && git push --force-with-lease", false},
		{"свой worktree: lease ветки", "cd " + own + " && git push --force-with-lease origin f HEAD", false},
		{"свой worktree: lease в чужую ветку", "cd " + own + " && git push --force-with-lease origin HEAD:main", true},
		{"свой worktree: lease --all", "cd " + own + " && git push --force-with-lease --all origin", true},
		{"свой worktree: push --force", "cd " + own + " && git push --force origin HEAD:main", true},
		{"свой worktree: lease и +ref", "cd " + own + " && git push --force-with-lease origin +HEAD:main", true},
		{"свой worktree: push --delete", "git -C " + own + " push origin --delete main", true},
		{"свой worktree: reflog expire", "cd " + own + " && git reflog expire --all", true},
		{"свой worktree: switch и lease HEAD", "cd " + own + " && git switch develop && git push --force-with-lease origin HEAD", true},
		{"свой worktree: checkout -B", "cd " + own + " && git checkout -B develop origin/main", true},
		{"свой worktree: checkout -b", "cd " + own + " && git checkout -b feat2", false},
		{"свой worktree: stash", "cd " + own + " && git stash && git rm -f a", false},
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

// Основная сессия работает в linked worktree: её суб-агентам это дерево не
// своё
func TestGitGuardMasterInWorktree(t *testing.T) {
	_, own := gitGuardTree(t)
	logger, err := core.NewLogger(core.DefaultLoggerConfig())
	if err != nil {
		t.Fatal(err)
	}
	tool, _ := NewGitGuardTool(core.ToolConfig{Enabled: true}, logger)
	tool.tempRoots = nil
	for _, start := range []string{own, filepath.Join(own, "cmd")} {
		input := &core.ToolInput{ToolName: "Bash", Command: "git stash", CWD: own, AgentID: "a1",
			TranscriptPath: filepath.Join("/p", encodeProject(start), "s.jsonl")}
		if got, err := tool.ValidateTool(context.Background(), input); err != nil || got.IsValid {
			t.Errorf("мастер в %s: git stash суб-агенту: %v %+v", start, err, got)
		}
	}
	// Мастер в другом каталоге: worktree — суб-агента
	input := &core.ToolInput{ToolName: "Bash", Command: "git stash", CWD: own, AgentID: "a1", TranscriptPath: "/p/-home-x/s.jsonl"}
	if got, err := tool.ValidateTool(context.Background(), input); err != nil || !got.IsValid {
		t.Errorf("свой worktree: %v %+v", err, got)
	}
}

// Временные каталоги — системный и scratchpad Claude Code
func TestGitGuardTempRoots(t *testing.T) {
	t.Setenv("CLAUDE_CODE_TMPDIR", "/var/scratch-x")
	if roots := tempRoots(); !slices.Contains(roots, "/var/scratch-x") || !slices.Contains(roots, "/tmp") {
		t.Errorf("tempRoots: %v", roots)
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

// Каталог вызова: cd действует только в своей области
func TestPlanCommandScopes(t *testing.T) {
	cases := []struct{ command, want string }{
		{"cd /a && git x", "/a"},
		{"(cd /a); git x", "/w"},
		{"(cd /a && git x)", "/a"},
		{"cd /a | true; git x", "/w"},
		{"bash -c 'cd /a'; git x", "/w"},
		{"bash -c 'cd /a && git x'", "/a"},
		{"bash <<'E'\ncd /a\nE\ngit x", "/w"},
		{"bash <<'E'\ncd /a\ngit x\nE", "/a"},
		{"bash <<'E'\ncd /a\nE\nbash <<'F'\ngit x\nF", "/w"},
		{"G=git; (cd /a); $G x", ""}, // смещение вызова неизвестно: в области ли — не узнать
		{"G=git; cd /a; $G x", "/a"},
		{"cd /a & git x", "/w"},
		{"echo $(cd /a); git x", "/w"},
		{"timeout 5 cd /a; git x", "/w"},
		{"builtin cd /a; git x", "/a"},
		{"{ cd /a; }; git x", "/a"},
		{"cd /a; (cd /b); git x", "/a"},
		{"cd /a && pushd -n /b && git x", "/a"},
		{"pushd /a && popd && git x", ""},
		{"cd /a && cd - && git x", ""},
	}
	for _, tc := range cases {
		calls, err := shellcmd.Calls(tc.command)
		if err != nil {
			t.Fatal(err)
		}
		plan := planCommand(tc.command, "/w", "/h", calls)
		got := "нет git"
		for i, c := range calls {
			if c.Name == "git" {
				got = plan.cwds[i]
			}
		}
		if got != tc.want {
			t.Errorf("%q: каталог git %q, нужен %q", tc.command, got, tc.want)
		}
	}
}
