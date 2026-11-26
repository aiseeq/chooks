package tools

import "testing"

func TestReplaceOption(t *testing.T) {
	cases := []struct {
		option string
		want   bool
	}{
		{"-I{}", true},
		{"-i", true},
		{"--replace=word", true},
		{"-0rI{}", true},
		{"-rI", true},
		{"-dI", false},
		{"-nI", false},
		{"--delimiter=I", false},
		{"list", false},
		{"--", false},
		{"-r", false},
		{"-ri", true},
		{"-aI", false},
		{"-eI", false},
		{"-EI", false},
		{"-LI", false},
		{"-lI", false},
		{"-PI", false},
		{"-sI", false},
		{"--interactive", false},
	}
	for _, tc := range cases {
		t.Run(tc.option, func(t *testing.T) {
			if got := replaceOption(tc.option); got != tc.want {
				t.Errorf("replaceOption(%q) = %v, want %v", tc.option, got, tc.want)
			}
		})
	}
}

// Подсказка на рекурсивный обход большого дерева (тикет wide-search-hint)
func TestWideSearch(t *testing.T) {
	const home = "/home/u"
	cases := []struct {
		name, command, cwd string
		wide               bool
	}{
		{"grep -r по ~/.cache", "grep -r x /home/u/.cache", "/tmp", true},
		{"rg --files claude-tmp", "rg --files ~/.cache/claude-tmp", "/tmp", true},
		{"du -sh claude-tmp", "du -sh ~/.cache/claude-tmp", "/tmp", true},
		{"du -d 1 без пути из ~", "du -d 1", home, true},
		{"find от ~", "find ~ -name x", "/tmp", true},
		{"find -L от ~", "find -L ~ -name x", "/tmp", true},
		{"grep -r . из ~", "grep -r x .", home, true},
		{"grep -rn связка", "grep -rn x ~/git", "/tmp", true},
		{"grep -R", "grep -R x ~/work", "/tmp", true},
		{"grep --recursive", "grep --recursive x ~/work", "/tmp", true},
		{"grep --directories=recurse", "grep --directories=recurse x ~/work", "/tmp", true},
		{"grep -r -e шаблон, путь первым", "grep -r -e x ~/git", "/tmp", true},
		{"grep -r без пути из ~/.local/state", "grep -r x", "/home/u/.local/state", true},
		{"rg из предка", "rg x /home", "/tmp", true},
		{"rg от корня", "rg x /", "/tmp", true},
		{"rg -t go шаблон из ~", "rg -t go x", home, true},
		{"rg -g шаблон путь", "rg -g '*.go' x ~/git", "/tmp", true},
		{"ls -R ~", "ls -lR ~", "/tmp", true},
		{"глоб в claude-tmp", "du -sh ~/.cache/claude-tmp/*", "/tmp", true},
		{"cd ~ && grep -r", "cd ~ && grep -r x .", "/tmp", true},
		{"внутри bash -c", "bash -c 'grep -r x ~/.cache'", "/tmp", true},
		{"путь с хвостовым /", "rg x ~/git/", "/tmp", true},
		{"grep -rl по ~/sc2/demu", "grep -rl --include=*.md x /home/u/sc2/demu", "/tmp", true},
		{"find по ~/sc2", "find ~/sc2 -name x", "/tmp", true},
		{"grep -r в узком подкаталоге demu", "grep -r x ~/sc2/demu/master/fighters/arch-tick", "/tmp", false},
		{"grep -r внутри репо", "grep -r x .", "/home/u/git/chooks", false},
		{"git grep", "git grep x", home, false},
		{"rg в worktree", "rg x", "/home/u/.cache/worktrees/chooks/a", false},
		{"rg по scratchpad", "rg x /home/u/.cache/claude-tmp/claude-1000/p/s/scratchpad", "/tmp", false},
		{"grep без -r", "grep x ~/.cache/f", "/tmp", false},
		{"grep без -r из ~", "grep x file", home, false},
		{"ls без -R", "ls ~", "/tmp", false},
		{"find в репо", "find . -name x", "/home/u/git/chooks", false},
		{"find в /tmp", "find /tmp -name x", home, false},
		{"rg шаблон-путь из репо", "rg /home/u", "/home/u/git/chooks", false},
		{"grep -r после --", "grep -- -r x", home, false},
		{"путь из переменной", `rg x "$D"`, "/tmp", false},
		{"cd в репо", "cd ~/git/chooks && rg x", home, false},
		{"echo", "echo grep -r x ~", "/tmp", false},
		{"длинная опция с r", "grep --color=never x ~/git", "/tmp", false},
		{"grep --color без значения", "grep -r --color x ~/git", "/tmp", true},
		{"-r после --", "grep -- -r ~", "/tmp", false},
		{"grep -r -f файл шаблонов", "grep -r -f pats ~/git", "/tmp", true},
		{"grep --regexp=", "grep -r --regexp=x ~/git", "/tmp", true},
		{"путь в кавычках", `rg x "/home/u/.cache"`, "/tmp", true},
		{"путь с подстановкой", "rg x $D/..", "/home/u/git", false},
		{"путь с подстановкой команды", "rg x `pwd`/..", "/home/u/git", false},
		{"cd с подстановкой", "cd $D/../.. && rg x", "/home/u/git/a", false},
		{"$HOME", "grep -r x $HOME", "/tmp", true},
		{"$HOME в кавычках", `du -sh "$HOME/.cache/claude-tmp"`, "/tmp", true},
		{"${HOME}", "rg --files ${HOME}/.cache", "/tmp", true},
		{"$HOMEDIR не дом", "rg x $HOMEDIR/git", "/tmp", false},
		{"$HOME и ещё подстановка", `rg x "$HOME/$D"`, "/tmp", false},
		{"worker в .local/state", "du -sh /home/u/.local/state/worker", "/tmp", true},
		{"все scratchpad", "du -sh /home/u/.cache/claude-tmp/claude-1000", "/tmp", true},
		{"транскрипты", "grep -r x ~/.claude/projects", "/tmp", true},
		{"прогоны Сна", "grep -rln x /home/u/.claude/improve/runs", "/tmp", true},
		{"предок прогонов Сна", "grep -r x ~/.claude/improve", "/tmp", true},
		{"несколько дней прогонов", "grep -rln x /home/u/.claude/improve/runs/2026-1[01]-*", "/tmp", true},
		{"файл дня прогонов", "grep -r x /home/u/.claude/improve/runs/2026-11-21/tasks-4b.md", "/tmp", false},
		{"папка дня прогонов", "grep -r x", "/home/u/.claude/improve/runs/2026-11-21", false},
		{"cd без пути", "cd && grep -r x .", "/home/u/git/chooks", true},
		{"pushd", "pushd ~ && rg x", "/home/u/git/chooks", true},
		{"pushd без пути", "pushd && rg x", "/home/u/git/chooks", false},
		{"cd -", "cd - && rg x", "/home/u/git/chooks", false},
		{"cd $HOME", "cd $HOME && rg x", "/home/u/git/chooks", true},
		{"cd с подстановкой в кавычках", `cd "$D" && rg x`, "/home/u/git/chooks", false},
		{"связка с -e последней", "grep -rne x", "/home/u", true},
		{"связка -re с путём-шаблоном", "grep -re /home/u .", "/home/u/git/chooks", false},
		{"rg связка -ie", "rg -ie /home/u/git src", "/home/u/git/chooks", false},
		{"значение в связке", "grep -rnex /home/u/git", "/tmp", true},
		{"grep -d recurse", "grep -d recurse x .", "/home/u", true},
		{"grep -drecurse", "grep -drecurse x .", "/home/u", true},
		{"grep --directories recurse", "grep --directories recurse x .", "/home/u", true},
		{"grep -d skip", "grep -d skip x .", "/home/u", false},
		{"grep -d recurse после --", "grep -- -d recurse .", "/home/u", false},
		{"cd $HOMEX", "cd $HOMEX/.. && rg x", "/home/u/git/chooks", false},
		{"длинная опция не связка", "rg --zzE x /home/u", "/home/u/git/chooks", true},
		{"rg --color never", "rg --color never /home/u", "/home/u/git/chooks", false},
		{"rg --max-filesize", "rg --max-filesize 1M x", "/home/u", true},
		{"du -hd 1", "du -hd 1 ~/git", "/tmp", true},
		{"du -hd1 без пути", "du -hd1", "/home/u/git/chooks", false},
		{"cd в подстановке присваивания", "x=$(cd ~) rg x", "/tmp", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			walk, err := WideSearch(c.command, c.cwd, home)
			if err != nil {
				t.Fatal(err)
			}
			if (walk != "") != c.wide {
				t.Fatalf("walk=%q, want wide=%v", walk, c.wide)
			}
		})
	}
}

func TestWideSearchXargsFileArguments(t *testing.T) {
	const home = "/home/aisee"
	cases := []struct {
		name, command string
		wide          bool
	}{
		{
			name:    "xargs rg reads supplied file list",
			command: "cut -d: -f1 hits | xargs rg -c -F -f pat",
		},
		{
			name:    "xargs -r with supplied arguments",
			command: "printf '%s\\n' a b | xargs -r rg foo",
		},
		{
			name:    "xargs -r without input",
			command: "xargs -r rg foo",
		},
		{
			name:    "xargs -r with empty list",
			command: "printf '' | xargs -r rg foo",
		},
		{
			name:    "rg without path reads cwd",
			command: "rg foo",
			wide:    true,
		},
		{
			name:    "rg with explicit wide path",
			command: "rg foo /home/aisee",
			wide:    true,
		},
		{
			name:    "xargs rg with explicit wide path",
			command: "xargs rg foo /home/aisee",
			wide:    true,
		},
		{
			name:    "xargs replacement in rg pattern is not a path",
			command: "printf 'needle\\n' | xargs -I{} rg -e '{}' --",
			wide:    true,
		},
		{
			name:    "xargs replace in rg pattern is not a path",
			command: "printf 'needle\\n' | xargs --replace rg -e '{}' --",
			wide:    true,
		},
		{
			name:    "xargs replacement in grep pattern is not a path",
			command: "printf x | xargs -I{} grep -r -e '{}'",
			wide:    true,
		},
		{
			name:    "xargs shell script positional argument in rg pattern",
			command: "ls | xargs -n1 sh -c 'rg \"$1\"' _",
			wide:    true,
		},
		{
			name:    "xargs shell script all arguments in rg option",
			command: "ls | xargs sh -c 'rg -e \"$@\"' _",
			wide:    true,
		},
		{
			name:    "xargs shell script positional argument as rg path",
			command: "ls | xargs -n1 sh -c 'rg needle \"$1\"' _",
		},
		{
			name:    "xargs shell command has no explicit path",
			command: "xargs -r sh -c 'rg needle' _",
			wide:    true,
		},
		{
			name:    "xargs short replacement",
			command: "ls | xargs -i rg -e {} --",
			wide:    true,
		},
		{
			name:    "xargs attached short replacement",
			command: "ls | xargs -i{} rg -e {} --",
			wide:    true,
		},
		{
			name:    "xargs delimiter value before replacement",
			command: "ls | xargs -d ' ' -I{} rg -e {} --",
			wide:    true,
		},
		{
			name:    "xargs long delimiter value before replacement",
			command: "ls | xargs --delimiter=' ' -I{} rg -e {} --",
			wide:    true,
		},
		{
			name:    "xargs process slot value before replacement",
			command: "ls | xargs --process-slot-var V -I{} rg -e {} --",
			wide:    true,
		},
		{
			name:    "xargs replacement in short option cluster",
			command: "ls | xargs -0rI{} rg -e {} --",
			wide:    true,
		},
		{
			name:    "xargs separate replacement in short option cluster",
			command: "ls | xargs -rI {} rg -e {} --",
			wide:    true,
		},
		{
			name:    "xargs zsh script has no explicit path",
			command: "ls | xargs zsh -c 'rg needle' _",
			wide:    true,
		},
		{
			name:    "xargs dash script has no explicit path",
			command: "ls | xargs dash -c 'rg needle' _",
			wide:    true,
		},
		{
			name:    "xargs shell passes input as rg paths",
			command: `ls | xargs sh -c 'rg x "$@"' _`,
		},
		{
			name:    "xargs replacement in shell script pattern is not a path",
			command: "ls | xargs -I{} sh -c 'rg -e {} --'",
			wide:    true,
		},
		{
			name:    "xargs rg pattern option is not a path",
			command: "ls | xargs rg -I needle",
		},
		{
			name:    "xargs one argument rg pattern option is not a path",
			command: "ls | xargs -n1 rg -I needle",
		},
		{
			name:    "shell command with inner xargs appends paths",
			command: "sh -c 'ls | xargs rg needle'",
		},
		{
			name:    "bash command with inner xargs appends paths",
			command: "bash -c 'ls | xargs rg needle'",
		},
		{
			name:    "bash command with inner grep xargs appends paths",
			command: "bash -c 'ls | xargs -r grep -rn needle'",
		},
		{
			name:    "shell command with max one inner xargs appends paths",
			command: "sh -c 'ls | xargs -n1 rg needle'",
		},
		{
			name:    "replacement shell command with inner xargs appends paths",
			command: "ls | xargs -I{} bash -c 'ls {} | xargs rg needle'",
		},
		{
			name:    "xargs replacement is an rg positional path",
			command: "ls | xargs -I{} rg x {}",
		},
		{
			name:    "xargs separate replacement is an rg positional path",
			command: "ls | xargs -I X rg x X",
		},
		{
			name:    "xargs replace is an rg positional path",
			command: "ls | xargs --replace rg x {}",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := WideSearch(tc.command, home, home)
			if err != nil {
				t.Fatal(err)
			}
			if (got != "") != tc.wide {
				t.Errorf("WideSearch() = %q; want wide=%v", got, tc.wide)
			}
		})
	}
}
