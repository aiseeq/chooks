package tools

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/aiseeq/akit/shellcmd"
	"github.com/aiseeq/chooks/internal/core"
)

// launchFixture — главный репозиторий с settings.local.json без хуков, его
// worktree (файл .git с gitdir: и commondir), подкаталог и каталог с
// settings.json без хуков только у родителя
type launchFixture struct {
	repo, worktree, sub, child, parent string
}

func newLaunchFixture(t *testing.T) launchFixture {
	t.Helper()
	root := t.TempDir()
	f := launchFixture{
		repo:     filepath.Join(root, "repo"),
		worktree: filepath.Join(root, "wt"),
		sub:      filepath.Join(root, "repo", "sub"),
		parent:   filepath.Join(root, "parent"),
		child:    filepath.Join(root, "parent", "child"),
	}
	gitDir := filepath.Join(f.repo, ".git", "worktrees", "wt")
	for _, dir := range []string{gitDir, f.worktree, f.sub, filepath.Join(f.repo, ".claude"), filepath.Join(f.parent, ".claude"), f.child} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		filepath.Join(f.worktree, ".git"):                         "gitdir: " + gitDir + "\n",
		filepath.Join(gitDir, "commondir"):                        "../..\n",
		filepath.Join(f.repo, ".claude", "settings.local.json"):   `{"disableAllHooks":true}`,
		filepath.Join(f.parent, ".claude", "settings.json"):       `{"disableAllHooks":true}`,
		filepath.Join(f.parent, ".claude", "settings.local.json"): `{"disableAllHooks":true}`,
	}
	for path, contents := range files {
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func TestCanonicalGitRoot(t *testing.T) {
	f := newLaunchFixture(t)
	relative := filepath.Join(filepath.Dir(f.repo), "rel")
	relGit := filepath.Join(f.repo, ".git", "worktrees", "rel")
	if err := os.MkdirAll(relGit, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(relative, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(relative, ".git"), []byte("gitdir: ../repo/.git/worktrees/rel"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(relGit, "commondir"), []byte("../.."), 0o600); err != nil {
		t.Fatal(err)
	}
	submodule := filepath.Join(filepath.Dir(f.repo), "submodule")
	if err := os.MkdirAll(filepath.Join(f.repo, ".git", "modules", "s"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(submodule, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(submodule, ".git"), []byte("gitdir: "+filepath.Join(f.repo, ".git", "modules", "s")), 0o600); err != nil {
		t.Fatal(err)
	}
	bare := filepath.Join(filepath.Dir(f.repo), "bare")
	bareGit := filepath.Join(filepath.Dir(f.repo), "store", "worktrees", "b")
	for _, dir := range []string{bare, bareGit} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(bare, ".git"), []byte("gitdir: "+bareGit), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bareGit, "commondir"), []byte("../.."), 0o600); err != nil {
		t.Fatal(err)
	}
	// путь через файл: Lstat даёт ENOTDIR, не «нет файла» — ошибка, а не подъём
	if err := os.WriteFile(filepath.Join(f.repo, "file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(filepath.Dir(f.repo), "broken")
	if err := os.MkdirAll(broken, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(broken, ".git"), []byte("not a gitdir line"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		dir, want string
		fails     bool
	}{
		{f.repo, f.repo, false},
		{f.sub, f.repo, false},
		{f.worktree, f.repo, false},
		{relative, f.repo, false},
		{submodule, submodule, false},
		{bare, bare, false},
		{f.child, "", false},
		{broken, "", true},
		{filepath.Join(f.repo, "file"), "", true},
	}
	for _, tc := range cases {
		got, err := canonicalGitRoot(tc.dir)
		if got != tc.want || (err != nil) != tc.fails {
			t.Errorf("canonicalGitRoot(%s) = %q, %v; want %q, fails=%v", tc.dir, got, err, tc.want, tc.fails)
		}
	}
}

func TestLaunchSettingsPaths(t *testing.T) {
	f := newLaunchFixture(t)
	logger := discardLogger{}
	worktree := launchSettingsPaths(f.worktree, nil, logger)
	if !slices.Contains(worktree, filepath.Join(f.repo, ".claude", "settings.local.json")) || slices.Contains(worktree, filepath.Join(f.repo, ".claude", "settings.json")) {
		t.Fatalf("worktree: %v", worktree)
	}
	if child := launchSettingsPaths(f.child, nil, logger); len(child) != 2 {
		t.Fatalf("без репозитория — только файлы каталога запуска: %v", child)
	}
	if repo := launchSettingsPaths(f.repo, nil, logger); len(repo) != 2 {
		t.Fatalf("корень репозитория — файлы каталога запуска один раз: %v", repo)
	}
	if home := launchSettingsPaths(f.sub, []string{f.repo}, logger); len(home) != 2 {
		t.Fatalf("корень, равный дому, не берётся: %v", home)
	}
	if sub := launchSettingsPaths(f.sub, nil, logger); len(sub) != 3 {
		t.Fatalf("подкаталог репозитория — и settings.local.json корня: %v", sub)
	}
}

func TestModelGuardLaunchSettings(t *testing.T) {
	tool := modelGuardForTest(t, "opus claude-opus-5-5\n")
	f := newLaunchFixture(t)
	runClientOptionCases(t, tool, []clientOptionCase{
		{"worktree reads main repository local settings", "cd " + f.worktree + " && claude -p --model opus hi", false, "settings.local.json: disableAllHooks"},
		{"worktree through runner", "cd " + f.worktree + "/ && npx -y @anthropic-ai/claude-code -p --model opus hi", false, "disableAllHooks"},
		{"repository subdirectory", "cd " + f.sub + " && claude -p --model opus hi", false, "disableAllHooks"},
		{"launch directory itself", "cd " + f.parent + " && claude -p --model opus hi", false, "disableAllHooks"},
		{"parent settings are not read", "cd " + f.child + " && claude -p --model opus hi", true, ""},
		{"cd through assigned variable", "D=" + f.parent + "; cd \"$D\" && claude -p --model opus hi", false, "disableAllHooks"},
		{"cd through variable and list", "D=" + f.parent + " && cd $D && claude -p --model opus hi", false, "disableAllHooks"},
		{"cd through braces", "D=" + f.parent + "; cd ${D} && claude -p --model opus hi", false, "disableAllHooks"},
		{"cd through reassigned variable", "D=" + f.parent + "; D=" + f.child + "; cd $D && claude -p --model opus hi", true, ""},
		{"cd through appended variable", "D=" + f.parent + "; D+=/child; cd $D && claude -p --model opus hi", true, ""},
		{"cd through unknown variable", "cd \"$WORKDIR\" && claude -p --model opus hi", true, ""},
	})
}

func TestAssignedArguments(t *testing.T) {
	command := `D=/a; E=~/b; F=$(pwd); G=/c; G+=/d; cd "$D" $E ${D} $D/x $F $G $H; H=/h`
	calls, err := shellcmd.Calls(command)
	if err != nil {
		t.Fatal(err)
	}
	assigns, err := shellcmd.Assigns(command)
	if err != nil {
		t.Fatal(err)
	}
	var cd *shellcmd.Call
	for i := range calls {
		if calls[i].Name == "cd" {
			cd = &calls[i]
		}
	}
	got := assignedArguments(cd, assigns).Args
	if want := []string{"/a", "$E", "/a", "$D/x", "$F", "$G", "$H"}; !slices.Equal(got, want) {
		t.Fatalf("args = %q, want %q", got, want)
	}
	if !slices.Equal(cd.Args, []string{`"$D"`, "$E", "${D}", "$D/x", "$F", "$G", "$H"}) {
		t.Fatalf("исходный вызов изменён: %q", cd.Args)
	}
	evaluated := `D=/a; eval "$X"; cd $D`
	calls, err = shellcmd.Calls(evaluated)
	if err != nil {
		t.Fatal(err)
	}
	assigns, err = shellcmd.Assigns(evaluated)
	if err != nil {
		t.Fatal(err)
	}
	if got := assignedArguments(&calls[len(calls)-1], assigns).Args; !slices.Equal(got, []string{"$D"}) {
		t.Fatalf("после eval значение неизвестно: %q", got)
	}
	computed := &shellcmd.Call{Name: "cd", Args: []string{"$D"}, Offset: -1}
	if assignedArguments(computed, assigns) != computed {
		t.Fatal("вызов без смещения не меняется")
	}
}

func TestModelGuardRunnerEnvironment(t *testing.T) {
	tool := modelGuardForTest(t, "opus claude-opus-5-5\n")
	runClientOptionCases(t, tool, []clientOptionCase{
		{"backticks", "ANTHROPIC_MODEL=claude-haiku-4-5 `which claude` -p hi", false, "claude-haiku-4-5"},
		{"type -P", "ANTHROPIC_MODEL=claude-haiku-4-5 $(type -P claude) -p hi", false, "claude-haiku-4-5"},
		{"version binary", "ANTHROPIC_MODEL=claude-haiku-4-5 /home/x/.local/share/claude/versions/2.1.332 -p hi", false, "claude-haiku-4-5"},
		{"npx", "ANTHROPIC_MODEL=claude-haiku-4-5 npx -y @anthropic-ai/claude-code -p hi", false, "claude-haiku-4-5"},
		{"bunx", "ANTHROPIC_MODEL=claude-haiku-4-5 bunx @anthropic-ai/claude-code -p hi", false, "claude-haiku-4-5"},
		{"approved", "ANTHROPIC_MODEL=opus bunx @anthropic-ai/claude-code -p hi", true, ""},
		{"other program", "ANTHROPIC_MODEL=claude-haiku-4-5 npx -y prettier --check .", true, ""},
		{"other substitution", "ANTHROPIC_MODEL=claude-haiku-4-5 $(go env GOPATH)/bin/x", true, ""},
		{"other client substitution", "ANTHROPIC_MODEL=claude-haiku-4-5 $(which codex) exec -m opus hi", true, ""},
		{"wrapped substitution", "ANTHROPIC_MODEL=claude-haiku-4-5 timeout 5 $(type -P claude) -p hi", false, "claude-haiku-4-5"},
		{"wrapped backticks", "ANTHROPIC_MODEL=claude-haiku-4-5 nice `type -P claude` -p hi", false, "claude-haiku-4-5"},
		{"wrapped variable", "C=$(type -P claude); ANTHROPIC_MODEL=claude-haiku-4-5 timeout 5 $C -p hi", false, "claude-haiku-4-5"},
		{"wrapped other client substitution", "ANTHROPIC_MODEL=claude-haiku-4-5 timeout 5 $(type -P codex) exec -m opus hi", true, ""},
		{"wrapped other substitution then claude", "ANTHROPIC_MODEL=claude-haiku-4-5 timeout 5 $(type -P htop) && claude -p --model opus hi", true, ""},
		{"other client", "ANTHROPIC_MODEL=claude-haiku-4-5 codex exec -m opus hi", true, ""},
		{"other client before claude", "ANTHROPIC_MODEL=claude-haiku-4-5 codex exec -m opus hi; claude -p --model opus hi", true, ""},
		{"other client substitution before claude", "ANTHROPIC_MODEL=claude-haiku-4-5 $(type -P codex) exec -m opus hi; claude -p --model opus hi", true, ""},
		{"wrapped other client substitution before claude", "ANTHROPIC_MODEL=claude-haiku-4-5 timeout 5 $(type -P codex) exec -m opus hi; claude -p --model opus hi", true, ""},
		{"wrapped substitution before claude", "ANTHROPIC_MODEL=claude-haiku-4-5 timeout 5 $(type -P claude) -p hi; claude -p --model opus hi", false, "claude-haiku-4-5"},
	})
}

func TestModelGuardVariableWriters(t *testing.T) {
	tool := modelGuardForTest(t, "opus claude-opus-5-5\n")
	runClientOptionCases(t, tool, []clientOptionCase{
		{"printf -v", `printf -v C %s "$(command -v claude)"; "$C" --bare -p hi`, false, "--bare"},
		{"read process substitution", "read -r C < <(command -v claude); $C --bare -p hi", false, "--bare"},
		{"reference chain", "C=$(command -v claude); C2=$C; $C2 --bare -p hi", false, "--bare"},
		{"read here-string", `IFS= read -r C <<<"$(which claude)"; "$C" --bare -p hi`, false, "--bare"},
		{"mapfile", "mapfile -t C < <(which claude); $C --bare -p hi", false, "--bare"},
		{"read here-doc", "read -r C <<EOF\n$(which claude)\nEOF\n$C --bare -p hi", false, "--bare"},
		{"for loop", "for C in $(which -a claude); do $C --bare -p hi; done", false, "--bare"},
		{"export", `export C="$(which claude)"; "$C" --bare -p hi`, false, "--bare"},
		{"nested shell", `bash -c 'C=$(which claude); $C --bare -p hi'`, false, "--bare"},
		{"printf other variable", `printf -v D %s "$(command -v claude)"; "$C" --bare -p hi`, true, ""},
		{"read other variable", "read -r D < <(command -v claude); $C --bare -p hi", true, ""},
		{"printf without -v", `printf %s "$(command -v claude)"; "$C" --bare -p hi`, true, ""},
		{"printf name as format argument", `printf '%s %s' C "$(command -v claude)"; "$C" --bare -p hi`, true, ""},
		{"reference to other", "D=$(mktemp); C2=$D; $C2 --bare -p hi", true, ""},
	})
}

func TestVariableClientDepth(t *testing.T) {
	chain := "A0=$(which claude); A1=$A0; A2=$A1; A3=$A2; A4=$A3; A5=$A4"
	if got := variableClient(chain, "A4", 0, discardLogger{}); got != "claude" {
		t.Fatalf("цепочка из 4 ссылок: %q", got)
	}
	if got := variableClient(chain, "A5", 0, discardLogger{}); got != "" {
		t.Fatalf("цепочка длиннее variableDepth не разбирается: %q", got)
	}
	nested := "bash -c 'C=$(which claude)'"
	if got := variableWriterTexts(nested, "C", variableDepth, discardLogger{}); len(got) != 0 {
		t.Fatalf("на пределе вложенности скрипты не разбираются: %q", got)
	}
	if got := variableWriterTexts(nested, "C", variableDepth-1, discardLogger{}); len(got) != 1 {
		t.Fatalf("под пределом вложенный скрипт разбирается: %q", got)
	}
	if got := variableWriterTexts("x=(", "x", 0, discardLogger{}); got != nil {
		t.Fatalf("неразобранный скрипт: %q", got)
	}
}

func TestModelGuardAgentWriteClientNames(t *testing.T) {
	tool := modelGuardForTest(t, "opus claude-opus-5-5\n")
	runClientOptionCases(t, tool, []clientOptionCase{
		{"bunx", `bunx @anthropic-ai/claude-code -p --model opus "review /home/x/.claude/agents/x.md"`, true, ""},
		{"version binary", `/home/x/.local/share/claude/versions/2.1.332 -p --model opus "review /home/x/.claude/agents/x.md"`, true, ""},
		{"substitution", `"$(command -v claude)" -p --model opus "review /home/x/.claude/agents/x.md"`, true, ""},
		{"screen", `screen -dm claude -p --model opus "review /home/x/.claude/agents/x.md"`, true, ""},
		{"substitution debug file", `"$(command -v claude)" -p --model opus --debug-file /home/x/.claude/agents/x.md hi`, false, ""},
		{"runner debug file", `bunx @anthropic-ai/claude-code -p --model opus --debug-file /home/x/.claude/agents/x.md hi`, false, ""},
	})
}

func TestClaudeClientCallUnparsed(t *testing.T) {
	// текст вызова не разобран — вызов судится как есть, как в разборе вызова
	call := &shellcmd.Call{Name: "", Raw: "timeout 5 $(", Args: []string{"-p"}}
	if claudeClientCall(call, call.Raw, discardLogger{}) {
		t.Fatal("неразобранный текст вызова — не claude")
	}
	literal := &shellcmd.Call{Name: "cl", Raw: "cl -p", Args: []string{"-p"}}
	if !claudeClientCall(literal, literal.Raw, discardLogger{}) {
		t.Fatal("cl — claude")
	}
}

func TestAgentWriteScanClientNames(t *testing.T) {
	cases := []struct {
		name, command string
		unknown       bool
	}{
		{"substitution", `"$(command -v claude)" -p --model opus "review /home/x/.claude/agents/x.md"`, false},
		{"substitution debug file", `"$(command -v claude)" -p --model opus --debug-file /home/x/.claude/agents/x.md hi`, true},
		{"runner", `bunx @anthropic-ai/claude-code -p --model opus "review /home/x/.claude/agents/x.md"`, false},
		{"runner debug file", `bunx @anthropic-ai/claude-code -p --model opus --debug-file /home/x/.claude/agents/x.md hi`, true},
		{"screen debug file", `screen -dm claude -p --model opus --debug-file=/home/x/.claude/agents/x.md hi`, true},
	}
	for _, tc := range cases {
		if _, unknown := writtenAgentDefinitionCandidates(tc.command); unknown != tc.unknown {
			t.Errorf("%s: unknown = %v, want %v", tc.name, unknown, tc.unknown)
		}
	}
}

func TestModelGuardLaunchSettingsUnreadableRoot(t *testing.T) {
	tool := modelGuardForTest(t, "opus claude-opus-5-5\n")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := tool.ValidateTool(gitGuardTestContext(t), &core.ToolInput{ToolName: "Bash", Command: "cd " + dir + " && claude -p --model opus hi", CWD: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsValid {
		t.Fatalf("неразобранный .git — только файлы каталога запуска: %v", result.Violations)
	}
}
