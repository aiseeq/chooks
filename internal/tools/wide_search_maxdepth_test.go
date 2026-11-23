package tools

import "testing"

// Мера мастера (тикет wide-search-shallow-find): find с -maxdepth ≤ 3 без другого широкого обхода подсказку не даёт
func TestWideSearchMaxdepth(t *testing.T) {
	const home = "/home/u"
	cases := []struct {
		name, command, cwd string
		wide               bool
	}{
		{"find ~ -maxdepth 1", "find ~ -maxdepth 1 -name x", "/tmp", false},
		{"find ~ -maxdepth 3", "find ~ -maxdepth 3 -type d", "/tmp", false},
		{"find ~/.cache -maxdepth 2", "find ~/.cache -maxdepth 2 -name '*.log'", "/tmp", false},
		{"find -maxdepth после выражения", "find ~ -name x -maxdepth 2", "/tmp", false},
		{"find -L ~ -maxdepth 2", "find -L ~ -maxdepth 2 -name x", "/tmp", false},
		{"find . -maxdepth 1 из ~", "find . -maxdepth 1", home, false},
		{"find ~ -maxdepth 4", "find ~ -maxdepth 4 -name x", "/tmp", true},
		{"find ~ -maxdepth 10", "find ~ -maxdepth 10 -name x", "/tmp", true},
		{"find ~ без -maxdepth", "find ~ -name x", "/tmp", true},
		{"find ~ -maxdepth с переменной", "find ~ -maxdepth $D -name x", "/tmp", true},
		{"мелкий find и grep -r в одной команде", "find ~ -maxdepth 1 -name x; grep -r y ~/.cache", "/tmp", true},
		{"мелкий find по ~ и широкий find по ~/.cache", "find ~ -maxdepth 1 -name x && find ~/.cache -name y", "/tmp", true},
		{"find -mindepth 2 без -maxdepth", "find ~ -mindepth 2 -name x", "/tmp", true},
		// круг 2, ревью 1cd2fe2: -maxdepth внутри -exec — не глубина внешнего find
		{"-maxdepth только внутри -exec", `find ~ -name '*.go' -exec find {} -maxdepth 1 \;`, "/tmp", true},
		{"-maxdepth только внутри -execdir", "find ~ -name x -execdir ls -maxdepth 1 {} +", "/tmp", true},
		// мелкий find с -exec рекурсивной команды — широкий обход
		{"мелкий find -exec du", `find ~ -maxdepth 1 -exec du -sh {} \;`, "/tmp", true},
		{"мелкий find -exec grep -r", "find ~ -maxdepth 2 -type d -exec grep -rl x {} +", "/tmp", true},
		{"мелкий find -exec find", `find ~ -maxdepth 1 -type d -exec find {} -name x \;`, "/tmp", true},
		{"мелкий find -ok du", `find ~ -maxdepth 1 -ok du -s {} \;`, "/tmp", true},
		// GNU find берёт последний -maxdepth
		{"последний -maxdepth 10", "find ~ -maxdepth 1 -maxdepth 10 -name x", "/tmp", true},
		{"последний -maxdepth 2", "find ~ -maxdepth 10 -maxdepth 2 -name x", "/tmp", false},
		// неглубокий -exec неглубокой команды остаётся неглубоким
		{"мелкий find -exec basename", `find ~ -maxdepth 1 -exec basename {} \;`, "/tmp", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hint, err := WideSearch(tc.command, tc.cwd, home)
			if err != nil {
				t.Fatal(err)
			}
			if (hint != "") != tc.wide {
				t.Fatalf("hint=%q, want wide=%v", hint, tc.wide)
			}
		})
	}
}
