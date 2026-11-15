package main

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

const testHome = "/home/aisee"

type row struct {
	name  string
	tool  string
	cmd   string
	path  string
	cwd   string
	hits  []int // классы с попаданием
	cands []int // классы-кандидаты без попадания: отсеяны
}

var rows = []row{
	// Обязательные попадания из приёмки тикета (тексты команд из транскриптов).
	{name: "a558a391:129 lex build", tool: "Bash", cmd: `lex build --codex /home/aisee/.claude/codex /home/aisee/.claude/skills/dev-process/SKILL.md 2>&1 | cut -c1-300; grep -n -A3 'Находку проверяющего' /home/aisee/.claude/skills/dev-process/SKILL.md | cut -c1-200; grep -n '\.env' /home/aisee/.claude/skills/dev-process/SKILL.md | cut -c1-200`, hits: []int{1}},
	{name: "a38b7277:107 $G commit", tool: "Bash", cmd: "cd /home/aisee && G=\"git --git-dir=/home/aisee/.claude-config.git --work-tree=/home/aisee\"; $G commit -q -F - <<'EOF'\ninstructions: цитаты\n\nтекст\nEOF", hits: []int{2}},
	{name: "a38b7277:113 $G push", tool: "Bash", cmd: `cd /home/aisee && G="git --git-dir=/home/aisee/.claude-config.git --work-tree=/home/aisee"; $G push origin main 2>&1 | tail -3 | cut -c1-200; echo "push rc=${PIPESTATUS[0]}"; $G status -sb | head -1; date '+%H:%M %Z'`, hits: []int{2}},
	{name: "a734d87d:152 cfg commit", tool: "Bash", cmd: "cfg(){ git --git-dir=$HOME/.claude-config.git --work-tree=$HOME \"$@\"; }; cd ~ && cfg add -A -- .claude/decisions.md .claude/codex/manifest/.claude-decisions.md.yaml .claude/codex/frag/.claude-decisions.md && cfg commit -q -m \"decisions (15.11): Ж1\n\nCo-Authored-By: x\" -- .claude/decisions.md >/dev/null 2>&1; cfg log -1 --format='%h' ; cfg push -q 2>&1 | tail -1; cfg status -sb | head -1", hits: []int{2}, cands: []int{1}},
	{name: "1113-1209-step5-plan att3:75 sub+absorb", tool: "command_execution", cwd: "/var/tmp/step5-plan", cmd: "/bin/bash -lc \"sub -n 1 '(на Waibee без anthropic и openai: \"'`waibee-no-subscription-vendors`)'\"' '(на Waibee без anthropic и openai)' /home/aisee/.claude/skills/ai-research/SKILL.md && lex absorb --codex /home/aisee/.claude/codex /home/aisee/.claude/skills/ai-research/SKILL.md\"", hits: []int{1}},
	{name: "step5-plan graft exec lex build", tool: "command_execution", cmd: `/bin/bash -lc 'graft exec --heavy -- lex build --codex /home/aisee/.claude/codex /home/aisee/.claude/skills/ai-research/SKILL.md'`, hits: []int{1}},
	// Не попадают по приёмке: sweep без --apply.
	{name: "a2053ccd:55 sweep --help", tool: "Bash", cmd: `graft deploy sweep --help 2>&1 | head -20; grep -n -A25 'release_notes' .graft.yaml | head -60`, cands: []int{3}},
	{name: "a2053ccd:61 sweep без --apply", tool: "Bash", cmd: `graft deploy sweep prod 2>&1 | cut -c1-400 | head -60`, cands: []int{3}},

	// Класс 1: способы записи.
	{name: "Edit скилла", tool: "Edit", path: "/home/aisee/.claude/skills/x/SKILL.md", hits: []int{1}},
	{name: "Write settings.json", tool: "Write", path: "/home/aisee/.claude/settings.json", hits: []int{1}},
	{name: "Write settings.local.json", tool: "Write", path: "/home/aisee/.claude/settings.local.json", hits: []int{1}},
	{name: "codex file_change агента", tool: "file_change", path: "/home/aisee/.claude/agents/a.md", hits: []int{1}},
	{name: "перенаправление >> с $HOME", tool: "Bash", cmd: `echo '- правило' >> "$HOME/.claude/rules/go.md"`, hits: []int{1}},
	{name: "cat > ~ heredoc", tool: "Bash", cmd: "cat > ~/.claude/CLAUDE.md <<'E'\nтекст\nE", hits: []int{1}},
	{name: "tee -a decisions", tool: "Bash", cmd: `printf x | tee -a /home/aisee/.claude/decisions.md >/dev/null`, hits: []int{1}},
	{name: "sed -i фрагмента", tool: "Bash", cmd: `sed -i 's/a/b/' /home/aisee/.claude/codex/frag/x.md`, hits: []int{1}},
	{name: "sub spec heredoc с относительным путём после cd", tool: "Bash", cmd: "cd /home/aisee/.claude/codex && sub <<'EOF'\nfrag/.claude-skills-dev%2Dprocess-SKILL.md/env.md\n<<<<<<< SEARCH\na\n=======\nb\n>>>>>>> REPLACE\nEOF", hits: []int{1}},
	{name: "cp в манифест", tool: "Bash", cmd: `cp /tmp/m.yaml ~/.claude/codex/manifest/m.yaml`, hits: []int{1}},
	{name: "lex absorb без --check", tool: "Bash", cmd: `lex absorb --codex ~/.claude/codex ~/.claude/CLAUDE.md`, hits: []int{1}},
	{name: "относительный sub в cwd ~/.claude", tool: "Bash", cwd: "/home/aisee/.claude", cmd: `sub OLD NEW skills/a/SKILL.md`, hits: []int{1}},

	// Класс 1: чтение и копии вне ~/.claude — отсеяны.
	{name: "grep скилла", tool: "Bash", cmd: `grep -n 'x' /home/aisee/.claude/skills/a/SKILL.md`, cands: []int{1}},
	{name: "cat CLAUDE.md", tool: "Bash", cmd: `cat ~/.claude/CLAUDE.md | head`, cands: []int{1}},
	{name: "sed -n decisions", tool: "Bash", cmd: `sed -n '60,70p' /home/aisee/.claude/decisions.md`, cands: []int{1}},
	{name: "tl с путём", tool: "Bash", cmd: `tl --all --grep '.claude/skills/coordinator' | head`, cands: []int{1}},
	{name: "lex build --check", tool: "Bash", cmd: `lex build --check --codex /home/aisee/.claude/codex`, cands: []int{1}},
	{name: "lex build копии кодекса", tool: "Bash", cmd: `lex build --codex /tmp/codex-copy /tmp/codex-copy/out/.claude/skills/a/SKILL.md`, cands: []int{1}},
	{name: "sub в копии /tmp", tool: "Bash", cmd: `sub A B /tmp/clone/.claude/skills/a/SKILL.md`, cands: []int{1}},
	{name: "sub --dry-run", tool: "Bash", cmd: `sub --dry-run A B /home/aisee/.claude/skills/a/SKILL.md`, cands: []int{1}},
	{name: "Write в scratchpad", tool: "Write", path: "/home/aisee/.cache/claude-tmp/claude-1000/-home-aisee/s/scratchpad/skills/x/SKILL.md"},
	{name: "Write в копию с .claude", tool: "Write", path: "/tmp/x/.claude/skills/a/SKILL.md", cands: []int{1}},
	{name: "sub в клоне по cd в переменную", tool: "Bash", cmd: `cd "$TMPDIR/clone" && sub A B .claude/skills/a/SKILL.md`, cands: []int{1}},
	{name: "перенаправление stderr", tool: "Bash", cmd: `grep x ~/.claude/CLAUDE.md 2>&1 >/dev/null`, cands: []int{1}},
	{name: "a98172ba:72 временный HOME", tool: "Bash", cmd: `W=/home/aisee/.cache/lex-check.gHv3S5; export HOME=$W/h; mkdir -p $HOME/.claude/skills/demo && cd $HOME && printf '# x\n' > .claude/CLAUDE.md && printf 'y' > .claude/skills/demo/SKILL.md && $W/lex-new build --codex .claude/codex`, cands: []int{1}},
	{name: "запись в проекты ~/.claude не класс", tool: "Bash", cmd: `echo x > /home/aisee/.claude/projects/p/notes.md`},

	// Класс 2.
	{name: "git --git-dir commit", tool: "Bash", cmd: `git --git-dir=$HOME/.claude-config.git --work-tree=$HOME commit -m x`, hits: []int{2}},
	{name: "GIT_DIR= push", tool: "Bash", cmd: `GIT_DIR=/home/aisee/.claude-config.git git push origin main`, hits: []int{2}},
	{name: "cfg status — чтение", tool: "Bash", cmd: `cfg status -sb; cfg diff -- .claude/CLAUDE.md`, cands: []int{1, 2}},
	{name: "git diff конфиг-репо — чтение", tool: "Bash", cmd: `git --git-dir=/home/aisee/.claude-config.git --work-tree=/home/aisee diff -- .claude/skills`, cands: []int{1, 2}},
	{name: "commit в другом репо", tool: "Bash", cmd: `git -C /home/aisee/git/chooks commit -m 'про .claude-config.git'`, cands: []int{2}},
	// GIT_DIR окружения вызова: export, env, подоболочка, вложенный скрипт (тикет approval-report-export-decl).
	{name: "export GIT_DIR, затем commit", tool: "Bash", cmd: `export GIT_DIR=$HOME/.claude-config.git; git commit -m x`, hits: []int{2}},
	{name: "export GIT_DIR с GIT_WORK_TREE, push", tool: "Bash", cmd: `export GIT_DIR=~/.claude-config.git GIT_WORK_TREE=$HOME && git push origin main`, hits: []int{2}},
	{name: "env GIT_DIR=", tool: "Bash", cmd: `env GIT_DIR=/home/aisee/.claude-config.git git commit -m x`, hits: []int{2}},
	{name: "GIT_DIR из переменной", tool: "Bash", cmd: `R=/home/aisee/.claude-config.git; export GIT_DIR="$R"; git commit -m x`, hits: []int{2}},
	{name: "export GIT_DIR во вложенном скрипте и commit в нём", tool: "Bash", cmd: `bash -c 'export GIT_DIR=$HOME/.claude-config.git; git commit -m x'`, hits: []int{2}},
	{name: "export GIT_DIR в одной ветке — запас в сторону попадания", tool: "Bash", cmd: `if [ -d x ]; then export GIT_DIR=$HOME/.claude-config.git; fi; git commit -m x`, hits: []int{2}},
	{name: "GIT_DIR без export", tool: "Bash", cmd: `GIT_DIR=/home/aisee/.claude-config.git; git commit -m x`, cands: []int{2}},
	{name: "export GIT_DIR в подоболочке", tool: "Bash", cmd: `(export GIT_DIR=$HOME/.claude-config.git); git commit -m x`, cands: []int{2}},
	{name: "export GIT_DIR во вложенном скрипте", tool: "Bash", cmd: `bash -c 'export GIT_DIR=$HOME/.claude-config.git'; git commit -m x`, cands: []int{2}},
	{name: "env -u GIT_DIR", tool: "Bash", cmd: `export GIT_DIR=$HOME/.claude-config.git; env -u GIT_DIR git commit -m x`, cands: []int{2}},
	{name: "unset GIT_DIR", tool: "Bash", cmd: `export GIT_DIR=$HOME/.claude-config.git; unset GIT_DIR; git commit -m x`, cands: []int{2}},
	{name: "unset GIT_DIR в одной ветке — запас в сторону попадания", tool: "Bash", cmd: `export GIT_DIR=$HOME/.claude-config.git; [ -d x ] && unset GIT_DIR; git commit -m x`, hits: []int{2}},
	{name: "GIT_DIR в heredoc оболочки — по регулярке", tool: "Bash", cmd: "bash <<'E'\nGIT_DIR=/home/aisee/.claude-config.git git commit -m x\nE", hits: []int{2}},
	{name: "GIT_DIR у другого вызова", tool: "Bash", cmd: `GIT_DIR=$HOME/.claude-config.git git status; git commit -m x`, cands: []int{2}},

	// Класс 3.
	{name: "deploy prod", tool: "Bash", cmd: `graft deploy prod 2>&1 | tail`, hits: []int{3}},
	{name: "deploy test", tool: "Bash", cmd: `graft deploy --redeploy test`, hits: []int{3}},
	{name: "freeze с версией", tool: "Bash", cmd: `graft deploy freeze prod "App 1.2"`, hits: []int{3}},
	{name: "thaw", tool: "Bash", cmd: `graft deploy thaw prod --reason 'выпуск'`, hits: []int{3}},
	{name: "sweep --apply", tool: "Bash", cmd: `graft deploy sweep prod --apply`, hits: []int{3}},
	{name: "handover", tool: "Bash", cmd: `graft deploy handover prod --glint-ticket PROJ-1`, hits: []int{3}},
	{name: "freeze без версии", tool: "Bash", cmd: `graft deploy freeze prod`, cands: []int{3}},
	{name: "status/logs/check/versions", tool: "Bash", cmd: `graft deploy status prod; graft deploy logs prod --lines 50; graft deploy check uat; graft deploy versions; graft deploy cutoff prod`, cands: []int{3}},
	{name: "graft deploy в тексте echo", tool: "Bash", cmd: `echo "потом graft deploy prod"`, cands: []int{3}},

	// Класс 4.
	{name: "comment POST", tool: "Bash", cmd: `curl -s -u "$U:$T" -X POST -H 'Content-Type: application/json' "https://x.atlassian.net/rest/api/3/issue/PROJ-12/comment?expand=1" -d @/tmp/c.json`, hits: []int{4}},
	{name: "transition с -d без -X", tool: "Bash", cmd: `curl -s -u "$U:$T" -H 'Content-Type: application/json' https://x.atlassian.net/rest/api/3/issue/PROJ-12/transitions -d '{"transition":{"id":"31"}}'`, hits: []int{4}},
	{name: "bitbucket PUT", tool: "Bash", cmd: `curl -sS -X PUT -u "$B" https://api.bitbucket.org/2.0/repositories/w/r/pullrequests/5 --json @f`, hits: []int{4}},
	{name: "DELETE через переменную хоста", tool: "Bash", cmd: `J=https://x.atlassian.net; curl -s -X DELETE "$J/rest/api/3/issue/PROJ-1/comment/10"`, hits: []int{4}},
	{name: "GET задачи", tool: "Bash", cmd: `curl -s -u "$U:$T" "https://x.atlassian.net/rest/api/3/issue/PROJ-12?fields=summary"`, cands: []int{4}},
	{name: "поиск POST — чтение", tool: "Bash", cmd: `curl -s -u "$U:$T" -X POST https://x.atlassian.net/rest/api/3/search/jql -d '{"jql":"x"}'`, cands: []int{4}},
	{name: "POST не на atlassian", tool: "Bash", cmd: `curl -s -X POST https://example.com/hook -d x # atlassian в комментарии`, cands: []int{4}},
}

func TestClassify(t *testing.T) {
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) { checkRow(t, r) })
	}
}

// checkRow сверяет классы с попаданием и отсеянных кандидатов строки таблицы.
func checkRow(t *testing.T, r row) {
	t.Helper()
	ev := Event{Tool: r.tool, Command: r.cmd, Cwd: r.cwd, CallCwd: r.cwd != "", Home: testHome} // у бойца cwd строки — из rollout
	if r.path != "" {
		ev.Paths = []string{r.path}
	}
	v := Classify(ev)
	var hits, cands []int
	for c := 1; c <= 4; c++ {
		if v.HitClass(c) {
			hits = append(hits, c)
		} else if v.Mentioned[c] {
			cands = append(cands, c)
		}
	}
	if !slices.Equal(hits, r.hits) || !slices.Equal(cands, r.cands) {
		t.Errorf("попадания %v (ждём %v), отсеянные кандидаты %v (ждём %v); hits=%+v", hits, r.hits, cands, r.cands, v.Hits)
	}
}

func TestHitDetails(t *testing.T) {
	cases := []struct {
		cmd  string
		want string
	}{
		{`graft deploy prod`, "3 deploy prod prod"},
		{`graft deploy test`, "3 deploy test не-prod"},
		{`graft deploy sweep "$T" --apply`, "3 sweep --apply $T ?"},
		{`curl -s -X POST "https://u:p@x.atlassian.net/rest/api/3/issue/PROJ-12/comment?x=secret" -d @f`, "4 POST x.atlassian.net/rest/api/3/issue/PROJ-12/comment comment PROJ-12"},
		{`curl -s -X POST https://x.atlassian.net/rest/api/3/issue/PROJ-12/transitions -d @f`, "4 POST x.atlassian.net/rest/api/3/issue/PROJ-12/transitions transition PROJ-12"},
		{`curl -s -X POST https://x.atlassian.net/rest/api/3/issue -d @f`, "4 POST x.atlassian.net/rest/api/3/issue other "},
		{`cd ~/.claude && echo x > rules/a.md`, "1 > /home/aisee/.claude/rules/a.md  "},
	}
	for _, c := range cases {
		v := Classify(Event{Tool: "Bash", Command: c.cmd, Home: testHome})
		if len(v.Hits) != 1 {
			t.Errorf("%s: попаданий %d: %+v", c.cmd, len(v.Hits), v.Hits)
			continue
		}
		h := v.Hits[0]
		got := fmt.Sprintf("%d %s %s %s", h.Class, h.Verb, h.Target, h.Prod)
		if h.Class == 4 {
			got = fmt.Sprintf("%d %s %s %s %s", h.Class, h.Verb, h.Target, h.Kind, h.Key)
		}
		if h.Class == 1 {
			got = fmt.Sprintf("%d %s %s %s %s", h.Class, h.Verb, h.Target, h.Kind, h.Key)
		}
		if got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.cmd, got, c.want)
		}
	}
}

func TestUnparsedFallback(t *testing.T) {
	// Незакрытая кавычка: оболочка не разбирает, регулярки ловят явные формы.
	v := Classify(Event{Tool: "Bash", Command: `cfg commit -m "x; echo 'a >> ~/.claude/CLAUDE.md`, Home: testHome})
	if !v.Unparsed || !v.HitClass(2) {
		t.Fatalf("ждём разбор регулярками и класс 2: %+v", v)
	}
	// graft -C перед deploy — та же выкатка
	v = Classify(Event{Tool: "Bash", Command: `echo "x; graft -C /home/aisee/git/proj deploy prod`, Home: testHome})
	if !v.Unparsed || !v.HitClass(3) {
		t.Fatalf("graft -C в запасном разборе: %+v", v)
	}
}

func TestResolveSpec(t *testing.T) {
	cases := []struct {
		out, cwd string
		spec     bool
		targets  []string
	}{
		{"sub: 9 replacements in 2 files: /home/aisee/.claude/codex/frag/a/b.md 1 (line 4), /var/tmp/p.tsv 8 (lines 1, 2)", "", false, []string{"/home/aisee/.claude/codex/frag/a/b.md"}},
		{"sub: 1 replacement in 1 file: skills/a/SKILL.md 1 (line 2)", "/home/aisee/.claude", false, []string{"/home/aisee/.claude/skills/a/SKILL.md"}},
		{"sub: nothing changed: 1 of 4 edits mismatched\n  block 1 (line 1): expected 1, found 0 in /home/aisee/.claude/CLAUDE.md", "", false, nil},
		{"Traceback (most recent call last):", "", true, nil},
	}
	for _, c := range cases {
		v := Classify(Event{Tool: "Bash", Command: `sub -F "$TMPDIR/x.spec"`, Home: testHome})
		if !v.SpecFile {
			t.Fatalf("sub -F файл: ждём SpecFile: %+v", v)
		}
		v.ResolveSpec(c.out, testHome, c.cwd)
		var got []string
		for _, h := range v.Hits {
			got = append(got, h.Target)
		}
		if v.SpecFile != c.spec || !slices.Equal(got, c.targets) || v.Mentioned[1] != (len(c.targets) > 0) {
			t.Errorf("%q: SpecFile=%v цели %v Mentioned=%v; ждём %v %v", c.out, v.SpecFile, got, v.Mentioned[1], c.spec, c.targets)
		}
	}
}

func TestPossiblePython(t *testing.T) {
	v := Classify(Event{Tool: "Bash", Command: "python3 - <<'PY'\nfrom pathlib import Path\nPath('/home/aisee/.claude/skills/a/SKILL.md').write_text('x')\nPY", Home: testHome})
	if len(v.Hits) != 0 || len(v.Possible) != 1 || v.Possible[0].Class != 1 {
		t.Fatalf("ждём одно возможное класса 1 вне чисел: %+v", v)
	}
	v = Classify(Event{Tool: "Bash", Command: "python3 - <<'PY'\np='/home/aisee/.claude/s/skills/x.md'\nq = Path('/home/aisee/.claude/skills/a/SKILL.md')\nq.write_text(p)\nPY", Home: testHome})
	if len(v.Possible) != 1 || v.Possible[0].Target != "/home/aisee/.claude/skills/a/SKILL.md" {
		t.Fatalf("запись через переменную: %+v", v.Possible)
	}
	// Путь класса 1 только в данных, запись в другой файл (1108-0201-2026-11-08/att1.jsonl:68).
	v = Classify(Event{Tool: "command_execution", Command: "/bin/bash -lc \"python3 - <<'PY'\np='/home/aisee/.claude/improve/out/night.json'\na=[d('x','tl 2a7ed31d:1285; ~/.claude/decisions.md:784')]\nwith open(p,'w') as f: f.write('x')\nPY\"", Home: testHome})
	if len(v.Possible) != 0 {
		t.Fatalf("путь в данных — не запись: %+v", v.Possible)
	}
}

// Временный XDG_CONFIG_HOME по окружению вызова (vars.go, callEnv): export действует в той же
// оболочке и во вложенных в неё после него; подоболочка, `$( … )`, элемент конвейера и вложенный
// `bash -c` — свои процессы; env -i, env -u, sudo без -E, ssh снимают конфиг; неясное окружение
// (ветка, функция, eval) — выкатка. Формы — из приёмки approval-report-polish, раунд 4.
func TestTempConfigEnv(t *testing.T) {
	const tmp = "/tmp/p"
	const probe = "проба: каталог /tmp/p"
	const exp = `export XDG_CONFIG_HOME=/tmp/c; `
	cases := []struct {
		name, tool, cmd string
		probe           string // "" — выкатка класса 3
	}{
		// живая выкатка
		{"export во вложенном скрипте обёртки бойца", "command_execution", `/bin/bash -lc 'bash -c "export XDG_CONFIG_HOME=/tmp/c"; graft deploy prod'`, ""},
		{"export в тексте echo у бойца", "command_execution", `/bin/bash -lc 'echo "export XDG_CONFIG_HOME=/tmp/c"; graft deploy prod'`, ""},
		{"export и выкатка в разных вложенных скриптах", "Bash", `bash -c 'export XDG_CONFIG_HOME=/tmp/c' ; bash -c 'graft deploy prod'`, ""},
		{"export в подстановке команды", "Bash", `X=$(export XDG_CONFIG_HOME=/tmp/c; echo); graft deploy prod`, ""},
		{"export в конвейере", "Bash", `export XDG_CONFIG_HOME=/tmp/c | true; graft deploy prod`, ""},
		{"export в задании &", "Bash", `export XDG_CONFIG_HOME=/tmp/c & graft deploy prod`, ""},
		{"env --unset=", "Bash", exp + `env --unset=XDG_CONFIG_HOME graft deploy prod`, ""},
		{"env -uИМЯ", "Bash", exp + `env -uXDG_CONFIG_HOME graft deploy prod`, ""},
		{"env -i", "Bash", exp + `env -i graft deploy prod`, ""},
		{"env -i с присваиванием", "Bash", exp + `env -i A=1 graft deploy prod`, ""},
		{"env -", "Bash", exp + `env - graft deploy prod`, ""},
		{"env --ignore-environment", "Bash", exp + `env --ignore-environment graft deploy prod`, ""},
		{"env -S — окружение не разобрано", "Bash", exp + `env -S 'A=1' graft deploy prod`, ""},
		{"sudo без -E", "Bash", exp + `sudo graft deploy prod`, ""},
		{"sudo -u без -E", "Bash", exp + `sudo -u root graft deploy prod`, ""},
		{"ssh", "Bash", exp + `ssh host graft deploy prod`, ""},
		{"export в ветке if", "Bash", `if [ -d x ]; then export XDG_CONFIG_HOME=/tmp/c; fi; graft deploy prod`, ""},
		{"export после && — мог не исполниться", "Bash", `cd /tmp/p && export XDG_CONFIG_HOME=/tmp/c; graft deploy prod`, ""},
		{"export после || ", "Bash", `false || export XDG_CONFIG_HOME=/tmp/c; graft deploy prod`, ""},
		{"export в цикле", "Bash", `for i in 1; do export XDG_CONFIG_HOME=/tmp/c; done; graft deploy prod`, ""},
		{"присваивание, затем export без значения во вложенном скрипте", "Bash", `XDG_CONFIG_HOME=/tmp/c; bash -c 'export XDG_CONFIG_HOME; graft deploy prod'`, ""},
		{"env -i перед вложенным скриптом", "Bash", exp + `env -i bash -c 'graft deploy prod'`, ""},
		{"живой export в одной ветке после временного", "Bash", exp + `if [ -d x ]; then export XDG_CONFIG_HOME=/home/aisee/.config; fi; graft deploy prod`, ""},
		{"TMPDIR переприсвоен живым", "Bash", `TMPDIR=/home/aisee/x; export XDG_CONFIG_HOME=$TMPDIR/c; graft deploy prod`, ""},
		{"declare +x с присваиванием", "Bash", exp + `declare +x XDG_CONFIG_HOME=/tmp/d; graft deploy prod`, ""},
		{"присваивание после export в одной ветке", "Bash", `if [ -d x ]; then export XDG_CONFIG_HOME=/tmp/c; fi; XDG_CONFIG_HOME=/tmp/c; graft deploy prod`, ""},
		{"export в цикле while", "Bash", `while false; do export XDG_CONFIG_HOME=/tmp/c; done; graft deploy prod`, ""},
		{"export в case", "Bash", `case x in y) export XDG_CONFIG_HOME=/tmp/c;; esac; graft deploy prod`, ""},
		{"export в одной ветке if/else", "Bash", `if [ -d x ]; then export XDG_CONFIG_HOME=/tmp/c; else true; fi; graft deploy prod`, ""},
		{"export после || и && выкатка", "Bash", `false || export XDG_CONFIG_HOME=/tmp/c && graft deploy prod`, ""},
		{"export в задании внутри &&", "Bash", `true && export XDG_CONFIG_HOME=/tmp/c & graft deploy prod`, ""},
		{"env --unset ИМЯ", "Bash", exp + `env --unset XDG_CONFIG_HOME graft deploy prod`, ""},
		{"env с неизвестной опцией", "Bash", exp + `env --frobnicate graft deploy prod`, ""},
		{"env с живым конфигом", "Bash", exp + `env XDG_CONFIG_HOME=/home/aisee/.config graft deploy prod`, ""},
		{"sudo --", "Bash", exp + `sudo -- graft deploy prod`, ""},
		{"sudo -u root без -E", "Bash", exp + `sudo -u root -- graft deploy prod`, ""},
		{"export -f не снимает и не ставит", "Bash", `export -f XDG_CONFIG_HOME=/tmp/c; graft deploy prod`, ""},
		{"declare без -x", "Bash", `declare XDG_CONFIG_HOME=/tmp/c; graft deploy prod`, ""},
		{"export без значения после ветки", "Bash", `if [ -d x ]; then XDG_CONFIG_HOME=/tmp/c; fi; export XDG_CONFIG_HOME; graft deploy prod`, ""},
		{"export в функции", "Bash", `f() { export XDG_CONFIG_HOME=/tmp/c; }; f; graft deploy prod`, ""},
		{"живой конфиг в функции после временного", "Bash", exp + `f() { export XDG_CONFIG_HOME=/home/aisee/.config; }; f; graft deploy prod`, ""},
		{"eval после export", "Bash", exp + `eval "$S"; graft deploy prod`, ""},
		{"source после export", "Bash", exp + `. ./env.sh; graft deploy prod`, ""},
		{"присваивание без export", "Bash", `XDG_CONFIG_HOME=/tmp/c; graft deploy prod`, ""},
		{"export -n", "Bash", exp + `export -n XDG_CONFIG_HOME; graft deploy prod`, ""},
		{"declare +x", "Bash", exp + `declare +x XDG_CONFIG_HOME; graft deploy prod`, ""},
		{"переменная из mktemp переприсвоена", "Bash", `D=$(mktemp -d); D=/home/aisee/.config; export XDG_CONFIG_HOME=$D; graft deploy prod`, ""},
		{"живой export вплотную к ;", "Bash", `export XDG_CONFIG_HOME=/home/aisee/.config;graft deploy prod`, ""},
		{"export в строке после \\ в одинарных кавычках", "Bash", `echo 'x\'"; export XDG_CONFIG_HOME=/tmp/c"; graft deploy prod`, ""},
		// проба
		{"export, затем вложенный скрипт", "Bash", exp + `bash -c 'graft deploy test'`, probe},
		{"export в обеих ветках if", "Bash", `if [ -d x ]; then export XDG_CONFIG_HOME=/tmp/c; elif [ -d y ]; then export XDG_CONFIG_HOME=/tmp/c; else export XDG_CONFIG_HOME=/tmp/c; fi; graft deploy test`, probe},
		{"unset -f не снимает переменную", "Bash", exp + `unset -f XDG_CONFIG_HOME; graft deploy test`, probe},
		{"export внешней оболочки во вложенный скрипт в двойных кавычках", "Bash", `bash -c "export XDG_CONFIG_HOME=$TMPDIR/c; graft deploy test"`, probe},
		{"вложенный скрипт без кавычек", "Bash", `bash -c export\ XDG_CONFIG_HOME=/tmp/c\;"graft deploy test"`, probe},
		{"префикс у вложенного скрипта", "Bash", `XDG_CONFIG_HOME=/tmp/c bash -c 'graft deploy test'`, probe},
		{"env -- с временным конфигом", "Bash", `env -- XDG_CONFIG_HOME=/tmp/c graft deploy test`, probe},
		{"env с временным конфигом из $TMPDIR", "Bash", `env XDG_CONFIG_HOME="$TMPDIR/c" graft deploy test`, probe},
		{"env -v --chdir=", "Bash", exp + `env -v --chdir=/tmp/p graft deploy test`, probe},
		{"env --chdir каталог -0", "Bash", exp + `env --chdir /tmp/p -0 graft deploy test`, probe},
		{"env -C каталог", "Bash", exp + `env -C/tmp/p graft deploy test`, probe},
		{"env --default-signal", "Bash", exp + `env --default-signal graft deploy test`, probe},
		{"sudo --preserve-env", "Bash", exp + `sudo --preserve-env graft deploy test`, probe},
		{"sudo -u root -E", "Bash", exp + `sudo -u root -E graft deploy test`, probe},
		{"sudo --user root -E", "Bash", exp + `sudo --user root -E graft deploy test`, probe},
		{"sudo -Eu root", "Bash", exp + `sudo -Eu root graft deploy test`, probe},
		{"sudo -uroot -E", "Bash", exp + `sudo -uroot -E graft deploy test`, probe},
		{"timeout перед выкаткой", "Bash", exp + `timeout 60 graft deploy test`, probe},
		{"export, затем timeout и вложенный скрипт", "Bash", exp + `timeout 60 bash -c 'graft deploy test'`, probe},
		{"апостроф в комментарии до export", "Bash", "# it's\n" + exp + `graft deploy test`, probe},
		{"апостроф в heredoc до export", "Bash", "cat <<'E'\nit's\nE\n" + exp + `graft deploy test`, probe},
		{"\\ в одинарных кавычках до export", "Bash", `echo 'a\'; ` + exp + `graft deploy test`, probe},
		{"апостроф в двойных кавычках до export", "Bash", `echo "it's"; ` + exp + `graft deploy test`, probe},
		{"временное присваивание после export", "Bash", exp + `XDG_CONFIG_HOME=/tmp/d; graft deploy test`, probe},
		{"export вплотную к ;", "Bash", `export XDG_CONFIG_HOME=/tmp/c;graft deploy test`, probe},
		{"живой конфиг у префикса другого вызова", "Bash", exp + `XDG_CONFIG_HOME=/home/aisee/.config graft status; graft deploy test`, probe},
		{"export && выкатка", "Bash", `cd /tmp/p && export XDG_CONFIG_HOME=/tmp/c && graft deploy test`, probe},
		{"export без значения, затем присваивание", "Bash", `export XDG_CONFIG_HOME; XDG_CONFIG_HOME=/tmp/c; graft deploy test`, probe},
		{"присваивание, затем export без значения", "Bash", `XDG_CONFIG_HOME=/tmp/c; export XDG_CONFIG_HOME; graft deploy test`, probe},
		{"declare -x", "Bash", `declare -x XDG_CONFIG_HOME=/tmp/c; graft deploy test`, probe},
		{"export из переменной mktemp", "Bash", `D=$(mktemp -d); export XDG_CONFIG_HOME="$D/c"; graft deploy test`, probe},
		{"env -i с временным конфигом", "Bash", `env -i XDG_CONFIG_HOME=/tmp/c graft deploy test`, probe},
		{"env -u другой переменной", "Bash", exp + `env -u A -C /tmp/p graft deploy test`, probe},
		{"sudo -E", "Bash", exp + `sudo -E graft deploy test`, probe},
		{"sudo --preserve-env=", "Bash", exp + `sudo --preserve-env=XDG_CONFIG_HOME graft deploy test`, probe},
		{"выкатка в конвейере", "Bash", exp + `graft deploy test | tee /tmp/p/log`, probe},
		{"export в ветке и после неё", "Bash", `if [ -d x ]; then export XDG_CONFIG_HOME=/tmp/c; fi; ` + exp + `graft deploy test`, probe},
		{"боец: двойные кавычки codex с \\$TMPDIR", "command_execution", `/bin/bash -lc "export XDG_CONFIG_HOME=\"\$TMPDIR/c\"; echo \"it's\"; graft deploy test"`, probe},
		{"боец: префикс в двойных кавычках codex", "command_execution", `/bin/bash -lc "XDG_CONFIG_HOME=\"\$TMPDIR/c\" graft deploy test"`, probe},
		{"боец: export во вложенном скрипте с выкаткой", "command_execution", `/bin/bash -lc 'bash -c "export XDG_CONFIG_HOME=/tmp/c; graft deploy test"'`, probe},
	}
	for _, c := range cases {
		checkProbe(t, c.name, Classify(Event{Tool: c.tool, Command: c.cmd, Cwd: tmp, CallCwd: c.tool == codexCommand, Home: testHome}), c.probe)
	}
	// GNU env кончает опции на первом присваивании: `env A=1 -u X graft …` запускает программу «-u»,
	// выкатки нет, и пробой она не становится
	if v := Classify(Event{Tool: "Bash", Command: exp + `env A=1 -u XDG_CONFIG_HOME graft deploy test`, Cwd: tmp, Home: testHome}); len(v.Possible) != 0 {
		t.Errorf("env A=1 -u: Possible=%+v", v.Possible)
	}
}

// sudo с присваиванием в словах: проходит ли оно, решает политика sudoers — переменная неясна.
// GNU env кончает опции на первом не-опционном слове.
// Через Classify не проверить: shellcmd (akit v1.76.0) берёт за программу `sudo X=/tmp/c graft`
// последний компонент значения присваивания.
func TestSudoAssignEnv(t *testing.T) {
	const exp = `export XDG_CONFIG_HOME=/tmp/c; `
	for _, c := range []struct {
		cmd     string
		unclear bool
	}{
		{`sudo XDG_CONFIG_HOME=/tmp/c graft deploy test`, true},
		{`sudo -E XDG_CONFIG_HOME=/tmp/c graft deploy test`, true},
		{`sudo --user=root -E -- graft deploy test`, false},
		// GNU env кончает опции на первом присваивании: «-u» — программа, окружение не тронуто
		{`env A=1 -u XDG_CONFIG_HOME graft deploy test`, false},
	} {
		sv, err := collectVars(exp+c.cmd, testHome, nil)
		if err != nil {
			t.Fatal(err)
		}
		env, ok := sv.callEnv(len(exp) + strings.Index(c.cmd, "graft"))
		v, _ := env.get("XDG_CONFIG_HOME")
		if !ok || v.unclear != c.unclear || !c.unclear && !(v.set && v.exported && v.temp) {
			t.Errorf("%s: найден %v, XDG_CONFIG_HOME %+v, ждём неясность %v", c.cmd, ok, v, c.unclear)
		}
	}
}
