package main

import (
	"bytes"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Возврат приёмки 15.11: переменная в аргументе и в цели перенаправления (команды — по образцу
// транскриптов, ссылка в имени).
var varRows = []row{
	{name: "a0f009ae:133 sub … $F", tool: "Bash", cmd: "F=/home/aisee/.claude/codex/frag/.claude-skills-dream-intervals.md/limit-1.md; sub 'предел — 20.' 'предел сессии минус запас 2\nснять_когда:' $F && cat $F", hits: []int{1}},
	{name: "a0f009ae:272 sub … \"$M\"", tool: "Bash", cmd: `M="/home/aisee/.claude/codex/manifest/.claude-skills-google%2Dworkspace-SKILL.md.yaml"; sub '  - ".claude-skills-google%2Dworkspace-SKILL.md/a-1"' '  - ".claude-skills-google%2Dworkspace-SKILL.md/b-1"' "$M"`, hits: []int{1}},
	{name: "a0f009ae:280 M рядом с $(…)", tool: "Bash", cmd: `R=/tmp/r/rest; M="/home/aisee/.claude/codex/manifest/.claude-skills-google%2Dworkspace-install.md.yaml"; H=$(python3 -I $R/body.py "$M" | awk '{print $1}'); sub 'source_sha256: PLACEHOLDER' "source_sha256: $H" "$M"`, hits: []int{1}},
	{name: "a0f009ae:340 sub по $F фрагмента", tool: "Bash", cmd: "F=/home/aisee/.claude/codex/frag/git-x-CLAUDE.md/glossary-1.md; sub 'a' 'b' \"$F\"; sub 'c' 'd' \"$F\"", hits: []int{1}},
	{name: "a5306add:301 M в одинарных кавычках", tool: "Bash", cmd: "M='/home/aisee/.claude/codex/manifest/.claude-skills-worker%2Dmodel-SKILL.md.yaml'; sub '  - \"a-1\"\n' '  - \"a-1\"\n  - \"b-1\"\n' \"$M\"", hits: []int{1}},
	{name: "adb1f095:53 lex absorb --codex $C в цикле", tool: "Bash", cmd: `X=/tmp/x; C=/home/aisee/.claude/codex; S=/home/aisee/.claude/skills; date '+%T %Z'; : > $X/absorb.log; for f in $S/dev-process/masters.md $S/dream/SKILL.md; do lex absorb --codex $C $f >> $X/absorb.log 2>&1; done`, hits: []int{1}},
	{name: "adb1f095:144 lex build --codex $C", tool: "Bash", cmd: `X=/tmp/x; C=/home/aisee/.claude/codex; lex build --codex $C > $X/build.log 2>&1; tail -3 $X/build.log`, hits: []int{1}},
	{name: "aae6443f:290 printf >> $C/frag", tool: "Bash", cmd: "set -e; C=/home/aisee/.claude/codex; T=/tmp/t/mvfrag.py\npython3 -I $T $C 'a-1' 'b-1'\nprintf '\\n' >> $C/frag/.claude-CLAUDE.md/tl-lead-1.md", hits: []int{1}},
	{name: "acd87f54:238 > \"$DST\" в bash -c", tool: "Bash", cmd: "bash -euo pipefail -c '\nSRC=\"/home/aisee/.claude/codex/frag/work-CLAUDE.md/main-1.md\"\nDST=\"/home/aisee/.claude/codex/frag/.claude-skills-work%2Dsupervisor-SKILL.md/main-1.md\"\ntest ! -e \"$DST\"\nsed -e \"s|^id: a$|id: b|\" \"$SRC\" > \"$DST\"\ncat \"$DST\"\n'", hits: []int{1}},
	{name: "a92cfdb0:200 cat > \"$F\" <<'EOF'", tool: "Bash", cmd: "F=\"/home/aisee/.claude/codex/frag/.claude-skills-coordinator-SKILL.md/profile-1.md\"; test -e \"$F\" && echo EXISTS || cat > \"$F\" <<'EOF'\n---\nid: x\n---\n- текст\nEOF\ncat \"$F\"", hits: []int{1}},
	{name: "codex: присваивание внутри bash -lc", tool: "command_execution", cmd: `/bin/bash -lc 'C=/home/aisee/.claude/codex; lex build --codex "$C"'`, hits: []int{1}},
	{name: "C от $HOME, F от $C", tool: "Bash", cmd: `C=$HOME/.claude/codex; F="$C/frag/a/b-1.md"; echo x >> "${F}"`, hits: []int{1}},
	{name: "export S=~/.claude/skills", tool: "Bash", cmd: `export S=~/.claude/skills; cp /tmp/a.md $S/x/SKILL.md`, hits: []int{1}},

	// Переменная неизвестна или не та: попадания нет.
	{name: "значение из $(…)", tool: "Bash", cmd: `F=$(mktemp -p /home/aisee/.claude/skills); sub a b "$F"`, cands: []int{1}},
	{name: "префикс вызова не подставляется в его аргументы", tool: "Bash", cmd: `F=/home/aisee/.claude/CLAUDE.md sub a b $F`, cands: []int{1}},
	{name: "переприсвоение до вызова", tool: "Bash", cmd: `F=/home/aisee/.claude/CLAUDE.md; F=/tmp/x.md; sub a b "$F"`, cands: []int{1}},
	{name: "присваивание после вызова", tool: "Bash", cmd: `sub a b "$F"; F=/home/aisee/.claude/CLAUDE.md`, cands: []int{1}},
	{name: "HOME переприсвоен", tool: "Bash", cmd: `export HOME=/tmp/h; C=$HOME/.claude/codex; lex build --codex $C`, cands: []int{1}},
	{name: "a9aad183:321 конфиг-репо пробы в переменной", tool: "Bash", cmd: `H=/var/tmp/review/probe/p15; git --git-dir=$H/.claude-config.git --work-tree=$H commit -qm x`, cands: []int{2}},
	{name: "конфиг-репо в переменной от $HOME", tool: "Bash", cmd: `R=$HOME/.claude-config.git; git --git-dir=$R --work-tree=$HOME push -q`, hits: []int{2}},
	{name: "копия кодекса в переменной", tool: "Bash", cmd: `C=/tmp/codex-copy; lex build --codex $C; echo .claude/skills`, cands: []int{1}},

	// Улучшения: старая форма sub на stdin, python-скрипт с путём, выкатка в пробе.
	{name: "aeaaae5d:75 sub <<EOF старой формы", tool: "Bash", cmd: "cd /home/aisee/.claude/skills/dev-process/ && sub <<'EOF'\nSKILL.md\n«готово» - `check.md` рядом: проверка.\n«готово» - `check.md` рядом.\n=\nбойца - `check.md` рядом (бриф).\nбойца - `check.md` рядом.\nEOF", hits: []int{1}},
	{name: "a01afbd7:136 python3 fx.py sub $S/…", tool: "Bash", cmd: "cd /tmp/s && S=/home/aisee/.claude/skills\npython3 fx.py sub $S/coordinator/SKILL.md '(старое' '(новое'", cands: []int{1}},
	{name: "1110-1204 att1:66 deploy test с временным XDG_CONFIG_HOME", tool: "command_execution", cmd: `/bin/bash -lc 'XDG_CONFIG_HOME="$TMPDIR/load-hunt/config" XDG_CACHE_HOME="$TMPDIR/load-hunt/cache" graft deploy test > "$TMPDIR/load-hunt/deploy.log" 2>&1; cat "$TMPDIR/load-hunt/deploy.log"'`, cwd: "/var/tmp/w/load-hunt/project", cands: []int{3}},
	{name: "deploy с export временного XDG_CONFIG_HOME", tool: "Bash", cwd: "/tmp/w", cmd: `P=$(mktemp -d); export XDG_CONFIG_HOME=$(mktemp -d) PATH=/tmp/bin:$PATH; graft deploy test`, cands: []int{3}},
}

func TestClassifyVars(t *testing.T) {
	for _, r := range varRows {
		t.Run(r.name, func(t *testing.T) { checkRow(t, r) })
	}
}

func targets(hs []Hit) []string {
	var out []string
	for _, h := range hs {
		out = append(out, h.Target)
	}
	return out
}

func TestVarTargets(t *testing.T) {
	cases := []struct {
		name, cmd string
		want      []string
	}{
		{"a0f009ae:133", "F=/home/aisee/.claude/codex/frag/a/b-1.md; sub 'x' 'y' $F && cat $F", []string{"/home/aisee/.claude/codex/frag/a/b-1.md"}},
		{"acd87f54:238 bash -c", "bash -euo pipefail -c '\nDST=\"/home/aisee/.claude/codex/frag/c/d-1.md\"\nsed -e s/a/b/ /tmp/src > \"$DST\"\n'", []string{"/home/aisee/.claude/codex/frag/c/d-1.md"}},
		{"aae6443f:290 heredoc sub -F - с $C", "C=/home/aisee/.claude/codex; sub -F - <<EOF\n$C/frag/.claude-skills-go%2Dperformance-SKILL.md/timeit-1.md\n<<<<<<< SEARCH\na\n=======\nb\n>>>>>>> REPLACE\n$C/frag/.claude-CLAUDE.md/remote-1.md\n<<<<<<< SEARCH\nc\n=======\nd $B\n>>>>>>> REPLACE\nEOF", []string{"/home/aisee/.claude/codex/frag/.claude-skills-go%2Dperformance-SKILL.md/timeit-1.md", "/home/aisee/.claude/codex/frag/.claude-CLAUDE.md/remote-1.md"}},
		{"heredoc в кавычках не подставляет", "C=/home/aisee/.claude/codex; sub -F - <<'EOF'\n$C/frag/a-1.md\n<<<<<<< SEARCH\na\n=======\nb\n>>>>>>> REPLACE\nEOF", nil},
		{"adb1f095:53 lex absorb", "C=/home/aisee/.claude/codex; lex absorb --codex $C /home/aisee/.claude/skills/dream/SKILL.md", []string{"/home/aisee/.claude/skills/dream/SKILL.md"}},
		{"aeaaae5d:75 старая форма: только путь", "cd /home/aisee/.claude/skills/dev-process/ && sub <<'EOF'\nSKILL.md\nстарый текст\nновый текст\n=\nещё\nдругое\nEOF", []string{"/home/aisee/.claude/skills/dev-process/SKILL.md"}},
	}
	for _, c := range cases {
		v := Classify(Event{Tool: "Bash", Command: c.cmd, Home: testHome})
		if got := targets(v.Hits); !slices.Equal(got, c.want) {
			t.Errorf("%s: цели %v, ждём %v", c.name, got, c.want)
		}
	}
}

func TestProbeAndScriptArg(t *testing.T) {
	v := Classify(Event{Tool: "command_execution", Command: `/bin/bash -lc 'XDG_CONFIG_HOME="$TMPDIR/h/config" graft deploy test'`, Cwd: "/var/tmp/h/p", CallCwd: true, Home: testHome})
	if len(v.Possible) != 1 || v.Possible[0].Class != 3 || v.Possible[0].Target != "test" || v.HitClass(3) {
		t.Errorf("выкатка в пробе: Possible=%+v Hits=%+v", v.Possible, v.Hits)
	}
	v = Classify(Event{Tool: "Bash", Command: `XDG_CONFIG_HOME=/home/aisee/.config graft deploy test`, Home: testHome})
	if len(v.Possible) != 0 || !v.HitClass(3) {
		t.Errorf("живой XDG_CONFIG_HOME — выкатка: Possible=%+v Hits=%+v", v.Possible, v.Hits)
	}
	v = Classify(Event{Tool: "Bash", Command: "S=/home/aisee/.claude/skills\npython3 fx.py sub $S/dream/SKILL.md 'a' 'b'", Home: testHome})
	if len(v.Possible) != 1 || v.Possible[0].Target != "/home/aisee/.claude/skills/dream/SKILL.md" || v.Possible[0].Verb != "python3 fx.py (аргумент)" {
		t.Errorf("python-скрипт с путём класса 1: %+v", v.Possible)
	}
	v = Classify(Event{Tool: "Bash", Command: "python3 check.py /home/aisee/.claude/skills/dream/SKILL.md", Home: testHome})
	if len(v.Possible) != 0 {
		t.Errorf("скрипт без слова правки — не запись: %+v", v.Possible)
	}
}

// a5306add:138, :166, adb1f095:41: вывод sub обрезан `cut`, цели — из файла спецификации.
func TestSpecTruncated(t *testing.T) {
	v := Classify(Event{Tool: "Bash", Command: `D=/tmp/d/decisions; sub -F $D/spec/I.spec | cut -c1-120 && cd /home/aisee/.claude/codex/frag`, Home: testHome})
	if !v.SpecFile || v.Spec == nil || v.Spec.Path != "/tmp/d/decisions/spec/I.spec" {
		t.Fatalf("ждём SpecFile с путём из переменной: %+v", v)
	}
	v.ResolveSpec("sub: 12 replacements in 11 files: /home/aisee/.claude/codex/frag/.claude-skills-instructions-craft.md/rewrite-user-words", testHome, "")
	if !v.SpecFile || len(v.Hits) != 0 {
		t.Fatalf("обрезанный вывод: цели не видны, SpecFile остаётся: %+v", v)
	}
	v.ResolveSpecFile(".claude-skills-a.md/b-1.md\n<<<<<<< SEARCH\nx\n=======\ny\n>>>>>>> REPLACE\n/home/aisee/.claude/codex/frag/c/d-1.md\n<<<<<<< SEARCH\nx\n=======\ny\n>>>>>>> REPLACE\n", testHome)
	want := []string{"/home/aisee/.claude/codex/frag/c/d-1.md"}
	if v.SpecFile || !slices.Equal(targets(v.Hits), want) || v.Hits[0].Verb != "sub -F (по файлу спецификации)" {
		t.Fatalf("цели из файла: %+v", v)
	}
	// Относительный путь спецификации — от каталога вызова после cd.
	v = Classify(Event{Tool: "Bash", Command: `cd /home/aisee/.claude/codex/frag && sub -F /tmp/s.spec 2>&1 | cut -c1-80`, Home: testHome})
	v.ResolveSpec("sub: 1 replacement in 1 file: /home/aisee/.claude/cod", testHome, "")
	v.ResolveSpecFile("a/b-1.md\n<<<<<<< SEARCH\nx\n=======\ny\n>>>>>>> REPLACE\n", testHome)
	if got := targets(v.Hits); !slices.Equal(got, []string{"/home/aisee/.claude/codex/frag/a/b-1.md"}) {
		t.Fatalf("относительный путь спецификации: %v", got)
	}
}

// Сканер читает файл спецификации, если вывод обрезан; файла нет — событие в «не видно разбором».
func TestScanSpecFile(t *testing.T) {
	root := t.TempDir()
	projects := filepath.Join(root, "projects")
	spec := filepath.Join(root, "spec", "I.spec")
	writeFile(t, spec, "/home/aisee/.claude/codex/frag/c/d-1.md\n<<<<<<< SEARCH\nx\n=======\ny\n>>>>>>> REPLACE\n")
	missing := filepath.Join(root, "spec", "gone.spec")
	writeFile(t, filepath.Join(projects, "-home-aisee", "8729f497-f8fa-4cce-96c9-9958d41a7c85.jsonl"), `{"type":"agent-name","agentName":"lead"}`+"\n")
	writeFile(t, filepath.Join(projects, "-home-aisee", "8729f497-f8fa-4cce-96c9-9958d41a7c85", "subagents", "agent-a5306add00000000.jsonl"), strings.Join([]string{
		`{"type":"user","message":{"role":"user","content":"задание"}}`,
		`{"type":"assistant","timestamp":"2026-11-13T18:00:07Z","cwd":"/home/aisee","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"D=` + filepath.Dir(spec) + `; sub -F $D/I.spec | cut -c1-60"}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"sub: 1 replacement in 1 file: /home/aisee/.claude/codex/fr"}]}}`,
		`{"type":"assistant","timestamp":"2026-11-13T18:01:07Z","cwd":"/home/aisee","message":{"content":[{"type":"tool_use","id":"toolu_2","name":"Bash","input":{"command":"sub -F ` + missing + ` | cut -c1-60 # .claude/codex/frag"}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_2","content":"sub: 1 replacement in 1 file: /home/aisee/.claude/codex/fr"}]}}`,
	}, "\n")+"\n")
	from, _ := time.Parse(time.RFC3339, "2026-11-08T00:00:00+01:00")
	to, _ := time.Parse(time.RFC3339, "2026-11-15T13:24:00+01:00")
	sc := scanner{home: "/home/aisee", from: from, to: to, jobs: 1}
	res, err := sc.run(projects, filepath.Join(root, "worker"))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	report(&out, res, from, to)
	reportChecks(&out, res, []string{"a5306add:2", "a5306add:4"})
	for _, s := range []string{
		"a5306add:2: попадание, класс 1",
		"sub -F (по файлу спецификации) /home/aisee/.claude/codex/frag/c/d-1.md",
		"a5306add:4: кандидат класса 1, отсеян",
		"ни в файле спецификации (файла уже нет или путь в переменной) — 1, из них с путём класса 1 в тексте и без попадания 1",
		"tl a5306add:4 — файл " + missing,
		"`sub -F файл` с целями из файла спецификации (вывод их не показал): 1 — цели по нынешнему содержимому файла",
	} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("в отчёте нет %q:\n%s", s, out.String())
		}
	}
}

// Выкатка с временным XDG_CONFIG_HOME — проба, только когда и каталог вызова временный: живой
// или неизвестный каталог с временным конфигом — выкатка класса 3. Если команда могла вернуться в
// прежний каталог (popd, `cd -`, подоболочка, cd во вложенном скрипте), временными должны быть все
// её каталоги. Формы — из повторной приёмки approval-report-polish 15.11.
func TestProbeCwd(t *testing.T) {
	const deploy = `XDG_CONFIG_HOME="$(mktemp -d)" graft deploy test`
	const live, tmp = "/home/aisee/git/proj", "/tmp/p"
	cases := []struct {
		name, tool, cwd string
		callCwd         bool
		cmd             string
		probe           string // "" — выкатка класса 3
	}{
		{"живой проект", "Bash", live, false, deploy, ""},
		{"временный проект", "Bash", "/home/aisee/.cache/claude-tmp/p/proj", false, deploy, "проба: каталог /home/aisee/.cache/claude-tmp/p/proj"},
		{"/tmp", "Bash", "/tmp", false, deploy, "проба: каталог /tmp"},
		{"scratchpad", "Bash", "/home/aisee/s/scratchpad", false, deploy, "проба: каталог /home/aisee/s/scratchpad"},
		{"/tmpx — не временный", "Bash", "/tmpx/proj", false, deploy, ""},
		{"каталог не записан", "Bash", "", false, deploy, ""},
		{"cd в живой проект", "Bash", tmp, false, "cd " + live + " && " + deploy, ""},
		{"cd во временный", "Bash", live, false, "cd /var/tmp/h/proj && " + deploy, "проба: каталог /var/tmp/h/proj"},
		{"cd в неизвестную переменную", "Bash", tmp, false, `cd "$W/proj" && ` + deploy, ""},
		{"cd в git rev-parse", "Bash", tmp, false, `cd "$(git -C ` + live + ` rev-parse --show-toplevel)" && ` + deploy, ""},
		{"cd в переменную из mktemp", "Bash", live, false, `W=$(mktemp -d); cd "$W" && ` + deploy, "проба: каталог из mktemp или $TMPDIR"},
		{"cd в $TMPDIR", "Bash", live, false, `cd "$TMPDIR/p" && ` + deploy, "проба: каталог из mktemp или $TMPDIR"},
		{"pushd и popd", "Bash", live, false, "pushd " + tmp + " && true; popd && " + deploy, ""},
		{"popd после cd в стеке", "Bash", live, false, "pushd /tmp/a && cd /tmp/b && popd && " + deploy, ""},
		{"popd во временных", "Bash", "/tmp/x", false, "pushd /tmp/a && cd /tmp/b && popd && " + deploy, "проба: каталог /tmp/a"},
		{"cd -", "Bash", live, false, "cd " + tmp + " && cd - && " + deploy, ""},
		{"выкатка после подоболочки", "Bash", live, false, "( cd " + tmp + "; true ); " + deploy, ""},
		{"подоболочка из временного", "Bash", "/tmp/x", false, "( cd " + tmp + " && " + deploy + " )", "проба: каталог /tmp/p"},
		{"cd в bash -c", "Bash", live, false, "bash -c 'cd " + tmp + "' && " + deploy, ""},
		{"боец без каталога из rollout", "command_execution", tmp, false, deploy, ""},
		{"боец, каталог из rollout временный", "command_execution", tmp, true, deploy, "проба: каталог /tmp/p"},
		{"боец, каталог из rollout живой", "command_execution", live, true, deploy, ""},
		{"боец с cd в живой проект", "command_execution", tmp, true, "cd " + live + " && " + deploy, ""},
		{"временный конфиг у другого вызова", "Bash", tmp, false, `XDG_CONFIG_HOME=/tmp/c graft status; graft deploy prod`, ""},
		{"export временного конфига", "Bash", tmp, false, `export XDG_CONFIG_HOME=/tmp/c; graft deploy test`, "проба: каталог /tmp/p"},
		{"env с временным конфигом", "Bash", tmp, false, `env XDG_CONFIG_HOME=/tmp/c graft deploy test`, "проба: каталог /tmp/p"},
		{"graft -C в живой проект", "Bash", tmp, false, `XDG_CONFIG_HOME=/tmp/c graft -C ` + live + ` deploy test`, ""},
		{"graft -C во временный", "Bash", live, false, `XDG_CONFIG_HOME=/tmp/c graft -C /tmp/q deploy test`, "проба: каталог /tmp/q"},
		{"graft -C в $TMPDIR", "Bash", live, false, `XDG_CONFIG_HOME=/tmp/c graft -C "$TMPDIR/p" deploy test`, "проба: каталог из mktemp или $TMPDIR"},
		{"export переопределён живым", "Bash", tmp, false, `export XDG_CONFIG_HOME=/tmp/c; export XDG_CONFIG_HOME=/home/aisee/.config; graft deploy prod`, ""},
		{"export снят unset", "Bash", tmp, false, `export XDG_CONFIG_HOME=/tmp/c; unset XDG_CONFIG_HOME; graft deploy prod`, ""},
		{"export в подоболочке", "Bash", tmp, false, `( export XDG_CONFIG_HOME=/tmp/c ); graft deploy prod`, ""},
		{"живой export, затем временный", "Bash", tmp, false, `export XDG_CONFIG_HOME=/home/aisee/.config; export XDG_CONFIG_HOME="/tmp/c"; graft deploy test`, "проба: каталог /tmp/p"},
		{"подстановка процесса <( — не подоболочка", "Bash", live, false, "diff <(echo a) /dev/null; cd " + tmp + " && " + deploy, "проба: каталог /tmp/p"},
		{"подстановка процесса >( — не подоболочка", "Bash", live, false, "echo a > >(cat); cd " + tmp + " && " + deploy, "проба: каталог /tmp/p"},
	}
	for _, c := range cases {
		v := Classify(Event{Tool: c.tool, Command: c.cmd, Cwd: c.cwd, CallCwd: c.callCwd, Home: testHome})
		if c.probe == "" {
			if len(v.Possible) != 0 || !v.HitClass(3) {
				t.Errorf("%s: ждём выкатку, Possible=%+v Hits=%+v", c.name, v.Possible, v.Hits)
			}
			continue
		}
		if len(v.Possible) != 1 || v.Possible[0].Prod != c.probe || v.HitClass(3) {
			t.Errorf("%s: ждём %q, Possible=%+v Hits=%+v", c.name, c.probe, v.Possible, v.Hits)
		}
	}
	// graft -C без временного конфига — выкатка (раньше не распознавалась вовсе)
	if v := Classify(Event{Tool: "Bash", Command: "graft -C " + live + " deploy prod", Cwd: tmp, Home: testHome}); !v.HitClass(3) {
		t.Errorf("graft -C … deploy prod: Hits=%+v", v.Hits)
	}
}

// aae6443f:171, :553: событие, засчитанное в класс, не стоит в «возможных» того же класса;
// возможная запись другого класса остаётся.
func TestPossibleNotDoubled(t *testing.T) {
	agent := &Agent{Pop: "sub", Short: "a1"}
	recs := []Record{
		{Agent: agent, Line: 1, V: Verdict{Hits: []Hit{{Class: 1, Verb: ">", Target: "/home/aisee/.claude/x.md"}}, Possible: []Hit{{Class: 1, Verb: "python3", Target: "/home/aisee/.claude/y.md"}}}},
		{Agent: agent, Line: 2, V: Verdict{Possible: []Hit{{Class: 1, Verb: "python3", Target: "/home/aisee/.claude/z.md"}}}},
		{Agent: agent, Line: 3, V: Verdict{Hits: []Hit{{Class: 2, Verb: "cfg commit"}}, Possible: []Hit{{Class: 1, Verb: "node", Target: "/home/aisee/.claude/w.md"}}}},
		{Agent: agent, Line: 4, V: Verdict{Hits: []Hit{{Class: 3, Verb: "deploy", Target: "prod"}}, Possible: []Hit{{Class: 3, Verb: "deploy", Target: "test", Prod: "проба: каталог /tmp"}}}},
	}
	var out bytes.Buffer
	popNotes(&out, recs)
	got := out.String()
	for _, s := range []string{"в числа классов не входят): 2\n", "tl a1:2 — класс 1?, python3, /home/aisee/.claude/z.md", "tl a1:3 — класс 1?, node, /home/aisee/.claude/w.md"} {
		if !strings.Contains(got, s) {
			t.Errorf("нет %q:\n%s", s, got)
		}
	}
	for _, s := range []string{"tl a1:1 ", "tl a1:4 ", "Выкатка в пробе"} {
		if strings.Contains(got, s) {
			t.Errorf("лишнее %q:\n%s", s, got)
		}
	}
}

// Запись в «возможных» — первая вне засчитанных классов: Possible[0] того же класса, что попадание,
// не показывается вместо неё.
func TestPossibleFirstUnhit(t *testing.T) {
	recs := []Record{{Agent: &Agent{Pop: "sub", Short: "a1"}, Line: 1, V: Verdict{
		Hits:     []Hit{{Class: 4, Verb: "POST", Target: "x.atlassian.net"}},
		Possible: []Hit{{Class: 4, Verb: "python3", Target: "atlassian/bitbucket"}, {Class: 1, Verb: "node", Target: "/home/aisee/.claude/w.md"}},
	}}}
	var out bytes.Buffer
	popNotes(&out, recs)
	if want := "tl a1:1 — класс 1?, node, /home/aisee/.claude/w.md"; !strings.Contains(out.String(), want) {
		t.Errorf("нет %q:\n%s", want, out.String())
	}
}

// Строка пробы — из записей вне засчитанных классов, с пометкой каталога; строка про `sub -F` по
// файлу спецификации — только когда такие цели есть, и считает события по глаголу, не по классу.
func TestProbeNoteLine(t *testing.T) {
	agent := &Agent{Pop: "sub", Short: "a1", Master: "m"}
	recs := []Record{
		{Agent: agent, Line: 1, V: Verdict{Hits: []Hit{{Class: 1, Verb: ">", Target: "/home/aisee/.claude/x.md"}}, Possible: []Hit{{Class: 1, Verb: "python3", Target: "/home/aisee/.claude/y.md"}, {Class: 3, Verb: "deploy", Target: "test", Prod: "проба: каталог /tmp/p"}}}},
		{Agent: agent, Line: 2, V: Verdict{Hits: []Hit{{Class: 1, Verb: ">", Target: "/home/aisee/.claude/z.md"}}}},
	}
	var out bytes.Buffer
	popNotes(&out, recs)
	got := out.String()
	if want := "tl a1:1 — deploy test, проба: каталог /tmp/p, мастер m"; !strings.Contains(got, want) {
		t.Errorf("нет %q:\n%s", want, got)
	}
	if strings.Contains(got, "файла спецификации (вывод") || strings.Contains(got, "python3") {
		t.Errorf("лишняя строка:\n%s", got)
	}
	recs[1].V.Hits = append(recs[1].V.Hits, Hit{Class: 1, Verb: specBodyVerb, Target: "/home/aisee/.claude/w.md"})
	out.Reset()
	popNotes(&out, recs)
	if want := "(вывод их не показал): 1 — "; !strings.Contains(out.String(), want) {
		t.Errorf("нет %q:\n%s", want, out.String())
	}
}
