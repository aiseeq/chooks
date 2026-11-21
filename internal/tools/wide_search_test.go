package tools

import "testing"

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
