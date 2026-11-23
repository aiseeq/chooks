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
