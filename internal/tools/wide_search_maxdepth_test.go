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
		// круг 3, ревью cf6d958: обёртки внутри -exec
		{"-exec sh -c du", `find ~ -maxdepth 1 -exec sh -c 'du -s "$1"' _ {} \;`, "/tmp", true},
		{"-exec bash -c grep -r", `find ~ -maxdepth 1 -exec bash -c 'grep -r x "$0"' {} \;`, "/tmp", true},
		{"-exec sudo du", `find ~ -maxdepth 1 -exec sudo du -s {} \;`, "/tmp", true},
		{"-exec timeout du", `find ~ -maxdepth 1 -exec timeout 5 du -s {} \;`, "/tmp", true},
		{"-exec /usr/bin/du", `find ~ -maxdepth 1 -exec /usr/bin/du -s {} \;`, "/tmp", true},
		// пайп мелкого find в xargs рекурсивной команды
		{"| xargs du", "find ~ -maxdepth 1 | xargs du -sh", "/tmp", true},
		{"| xargs -0 du", "find ~ -maxdepth 1 -print0 | xargs -0 du -s", "/tmp", true},
		{"| xargs -I grep -r", "find ~ -maxdepth 1 -type d | xargs -I{} grep -r x {}", "/tmp", true},
		{"| xargs dirname | xargs du", "find ~/git -maxdepth 2 -name .git -type d | xargs -n1 dirname | xargs -I{} du -sh {}", "/tmp", true},
		{"| xargs basename", "find ~ -maxdepth 1 | xargs basename", "/tmp", false},
		{"| head", "find ~ -maxdepth 2 -name x | head", "/tmp", false},
		// внутренний мелкий find в -exec не рекурсивен
		{"-exec мелкий find", `find ~ -maxdepth 2 -name x -exec find {} -maxdepth 1 \;`, "/tmp", false},
		// + кончает -exec только после {}
		{"+ не после {}", `find ~ -exec echo + -maxdepth 1 \;`, "/tmp", true},
		// круг 4, ревью cfa2723: обёртка перед find (была паника), перенаправление, вывод find в рекурсивную команду без xargs
		{"timeout find | xargs du", "timeout 10 find ~ -maxdepth 1 | xargs du -s", "/tmp", true},
		{"nice ionice find | xargs du", "nice -n 19 ionice -c3 find ~ -maxdepth 1|xargs du", "/tmp", true},
		{"sudo find | xargs du", "sudo find ~ -maxdepth 1 | xargs du -s", "/tmp", true},
		{"find 2>&1 | xargs du", "find ~ -maxdepth 1 2>&1 | xargs du -s", "/tmp", true},
		{"find | grep ';' | xargs du", "find ~ -maxdepth 1 | grep -v ';' | xargs du -s", "/tmp", true},
		{"find | while read du", `find ~ -maxdepth 1 | while read d; do du -s "$d"; done`, "/tmp", true},
		{"for in $(find) du", `for d in $(find ~ -maxdepth 1 -type d); do du -s "$d"; done`, "/tmp", true},
		{"du $(find)", "du -sh $(find ~ -maxdepth 1 -type d)", "/tmp", true},
		{"| parallel du", "find ~ -maxdepth 1 | parallel du -s", "/tmp", true},
		// круг 5, ревью 7881d49: опции parallel, команда parallel строкой, метка xargs -I не {}
		{"| parallel -j4 du", "find ~ -maxdepth 1 | parallel -j4 du -s", "/tmp", true},
		{"| parallel --jobs 4 du {}", "find ~ -maxdepth 1 | parallel --jobs 4 du -s {}", "/tmp", true},
		{"| parallel 'du -s {}'", "find ~ -maxdepth 1 | parallel 'du -s {}'", "/tmp", true},
		{"| xargs -I% du %", "find ~ -maxdepth 1 | xargs -I% du -s %", "/tmp", true},
		{"| xargs -I X du X", "find ~ -maxdepth 1 | xargs -I X du -s X", "/tmp", true},
		{"| xargs -I{} du {}/", "find ~ -maxdepth 1 | xargs -I{} du -s {}/", "/tmp", true},
		{"| xargs -I{} sh -c du {}/x", "find ~ -maxdepth 1 | xargs -I{} sh -c 'du -s {}/x'", "/tmp", true},
		// рекурсивная команда со своим известным путём — вывод find она не обходит
		{"find | head; du /tmp/a", "find ~ -maxdepth 1 | head; du -s /tmp/a", "/tmp", false},
		// xargs дописывает вывод find к своему пути: широкий (круг 5)
		{"find | xargs -r du /tmp/a", "find ~ -maxdepth 1 -newer x | xargs -r du -s /tmp/a", "/tmp", true},
		// tree вне списка, на который пользователь дал «да» (тикет wide-search-hint): подсказки нет
		{"tree ~", "tree ~", "/tmp", false},
		{"tree -L 1 ~", "tree -L 1 ~", "/tmp", false},
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
