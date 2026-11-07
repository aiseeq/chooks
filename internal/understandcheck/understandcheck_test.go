package understandcheck

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/akit/transcript"
)

func cmd(line int, tool string, input map[string]any) transcript.Event {
	raw, _ := json.Marshal(input)
	return transcript.Event{Line: line, Role: transcript.Cmd, Who: tool, Raw: raw, Time: time.Unix(int64(line), 0)}
}

func text(line int, role transcript.Role, s string) transcript.Event {
	return transcript.Event{Line: line, Role: role, Text: s, Time: time.Unix(int64(line), 0)}
}

// final — текст агента, которым кончился ход
func final(line int, s string) transcript.Event {
	e := text(line, transcript.Agent, s)
	e.Reply = &transcript.Reply{StopReason: "end_turn"}
	return e
}

func TestCollector_TurnBoundaries(t *testing.T) {
	c := Collector{Session: "abcd1234"}
	events := []transcript.Event{
		final(1, "Выкатить по таймеру или сразу?"),
		text(2, transcript.User, "по таймеру"),
		cmd(3, "SendMessage", map[string]any{"to": "deploy-master", "message": "Поставь таймер или запусти сразу"}),
		cmd(4, "Bash", map[string]any{"command": "go test ./..."}),
		final(5, "Передал."),
		// письмо другой сессии начинает ход не по слову пользователя
		text(6, transcript.Peer, "готово"),
		cmd(7, "SendMessage", map[string]any{"to": "lead", "message": "итог"}),
		text(8, transcript.User, "И да, один сегодняшний деплой"),
		cmd(9, "Agent", map[string]any{"subagent_type": "general-purpose", "description": "d", "prompt": "бриф"}),
		cmd(10, "SendMessage", map[string]any{"to": "x", "notify_when_idle": true}),
	}
	for _, e := range events {
		c.Add(e)
	}
	got := c.Cases()
	if len(got) != 2 {
		t.Fatalf("want 2 cases (send in user turn, agent), got %d: %+v", len(got), got)
	}
	send := got[0]
	if send.ID != "abcd1234:3" || send.Kind != KindSend || send.Human != "по таймеру" || send.Prev != "Выкатить по таймеру или сразу?" {
		t.Errorf("send case wrong: %+v", send)
	}
	if send.Next != "И да, один сегодняшний деплой" {
		t.Errorf("next reply = %q", send.Next)
	}
	agent := got[1]
	if agent.Kind != KindAgent || agent.Prev != "Передал." || agent.Next != "" {
		t.Errorf("agent case wrong: %+v", agent)
	}
}

// Реплики, набранные подряд, пока агент работал, судья видит все: действие
// бывает ответом на раннюю (случай 8 лида)
func TestCollector_UnansweredRepliesAccumulate(t *testing.T) {
	c := Collector{Session: "s"}
	for _, e := range []transcript.Event{
		final(1, "Готово."),
		text(2, transcript.User, "снижать потребление luna. А ты как считаешь?"),
		text(3, transcript.Agent, "Смотрю замер."),
		text(4, transcript.User, "\"лид\" - все агенты"),
		cmd(5, "SendMessage", map[string]any{"to": "git-admin", "message": "Luna вдвое меньше"}),
		final(6, "Разослал."),
		text(7, transcript.User, "нет, я про планку"),
		cmd(8, "SendMessage", map[string]any{"to": "git-admin", "message": "отменяю"}),
	} {
		c.Add(e)
	}
	got := c.Cases()
	if len(got) != 2 {
		t.Fatalf("want 2 cases, got %d", len(got))
	}
	want := "[реплика 1 из 2]\nснижать потребление luna. А ты как считаешь?\n\n[реплика 2 из 2]\n\"лид\" - все агенты"
	if got[0].Human != want || got[0].Prev != "Готово." {
		t.Errorf("first case human=%q prev=%q", got[0].Human, got[0].Prev)
	}
	if got[1].Human != "нет, я про планку" || got[1].Prev != "Разослал." {
		t.Errorf("after end_turn the reply list restarts: human=%q prev=%q", got[1].Human, got[1].Prev)
	}
}

func TestCollector_SidechainIgnored(t *testing.T) {
	c := Collector{Session: "s"}
	c.Add(text(1, transcript.User, "реплика"))
	e := cmd(2, "SendMessage", map[string]any{"to": "a", "message": "m"})
	e.Sidechain = true
	c.Add(e)
	if len(c.Cases()) != 0 {
		t.Fatalf("sidechain call must not be a case")
	}
}

func TestIsInstructionFile(t *testing.T) {
	yes := []string{
		"/home/u/.claude/CLAUDE.md",
		"/home/u/git/chooks/CLAUDE.md",
		"/home/u/.claude/skills/dev-process/SKILL.md",
		"/home/u/.claude/skills/dev-process/browser.md",
		"/home/u/.claude/rules/go.md",
		"/home/u/.claude/projects/-home-u-git-x/memory/MEMORY.md",
		"/home/u/.claude/decisions.md",
		"/home/u/.claude/settings.json",
		"/home/u/.claude/improve/prompts/judge.md",
	}
	no := []string{
		"/home/u/git/chooks/README.md",
		"/home/u/.claude/improve/state/backlog.md",
		"/home/u/git/chooks/docs/tasks/x.md",
		"/home/u/.claude/projects/-home-u/abc.jsonl",
	}
	for _, p := range yes {
		if !IsInstructionFile(p) {
			t.Errorf("%s: want instruction file", p)
		}
	}
	for _, p := range no {
		if IsInstructionFile(p) {
			t.Errorf("%s: not an instruction file", p)
		}
	}
}

func TestBashRuleFile(t *testing.T) {
	cases := map[string]string{
		"cd ~/.claude/skills/chat && sub 'a' 'b' SKILL.md":            "SKILL.md",
		"cat >> ~/.claude/decisions.md <<'EOF'\nx\nEOF":               "~/.claude/decisions.md",
		"sed -i 's/a/b/' /home/u/git/x/CLAUDE.md":                     "/home/u/git/x/CLAUDE.md",
		"grep -n foo ~/.claude/decisions.md > /tmp/out.txt":           "",
		"cat ~/.claude/skills/dev-process/SKILL.md":                   "",
		"grep -c x CLAUDE.md 2>&1 | head":                             "",
		"echo x | tee -a ~/.claude/projects/-home-u/memory/MEMORY.md": "~/.claude/projects/-home-u/memory/MEMORY.md",
	}
	for command, want := range cases {
		if got := bashRuleFile(command); got != want {
			t.Errorf("%q: got %q, want %q", command, got, want)
		}
	}
}

// Правка в любой форме вызова по-прежнему ловится
func TestBashRuleFile_RealEdit(t *testing.T) {
	cases := map[string]string{
		"cat > /tmp/fix.pl <<'EOF'\nsub once { 1 }\nEOF\nperl /tmp/fix.pl ~/.claude/skills/x/SKILL.md": "~/.claude/skills/x/SKILL.md",
		"bash <<'EOF'\nsed -i x CLAUDE.md\nEOF":                                                        "CLAUDE.md",
		"sub a b CLAUDE.md":                                                                            "CLAUDE.md",
		// Исполнение через аргумент, ввод или неразобранную строку: проверка aa76883
		"eval \"sed -i s/a/b/ CLAUDE.md\"":                               "CLAUDE.md",
		"echo 'sed -i s/a/b/ CLAUDE.md' | bash":                          "CLAUDE.md",
		"bash <<< 'sed -i s/a/b/ CLAUDE.md'":                             "CLAUDE.md",
		"source <(echo 'sed -i s/a/b/ CLAUDE.md')":                       "CLAUDE.md",
		"S='sed -i s/a/b/ CLAUDE.md'; echo go; bash -c \"$S\"":           "CLAUDE.md",
		"python3 -c \"import os; os.system('sed -i s/a/b/ CLAUDE.md')\"": "CLAUDE.md",
		"perl -e 'system(\"sed -i s/a/b/ CLAUDE.md\")'":                  "CLAUDE.md",
		"find . -name CLAUDE.md -exec env LC_ALL=C sed -i s/a/b/ {} \\;": "CLAUDE.md",
		"setsid --wait sub a b CLAUDE.md":                                "CLAUDE.md",
		"flock ./lock sed -i s/a/b/ CLAUDE.md":                           "CLAUDE.md",
		"run() { \"$@\"; }; run sed -i s/a/b/ CLAUDE.md":                 "CLAUDE.md",
		"sed -i s/a/b/ CLAUDE.md\necho \"oops":                           "CLAUDE.md",
		"bash -c 'sed -i s/a/b/ CLAUDE.md\n)'":                           "CLAUDE.md",
		"x=() true; sed -i s/a/b/ CLAUDE.md":                             "CLAUDE.md",
		"echo $(sed -i s/a/b/ CLAUDE.md)":                                "CLAUDE.md",
		"echo `sed -i s/a/b/ CLAUDE.md`":                                 "CLAUDE.md",
		"f() { sed -i s/a/b/ CLAUDE.md; }; echo go; f":                   "CLAUDE.md",
		"trap 'sed -i s/a/b/ CLAUDE.md' EXIT; echo go":                   "CLAUDE.md",
		"F='sed -i s/a/b/ CLAUDE.md'; $F":                                "CLAUDE.md",
		"echo 'sed -i s/a/b/ CLAUDE.md' > run.sh":                        "CLAUDE.md",
		"cat > run.sh <<'EOF'\nsed -i s/a/b/ CLAUDE.md\nEOF":             "CLAUDE.md",
		"git -c alias.x='!sed -i s/a/b/ CLAUDE.md' x":                    "CLAUDE.md",
		"graft exec -- sed -i s/a/b/ CLAUDE.md":                          "CLAUDE.md",
		"git commit -m 'x' && sub a b CLAUDE.md":                         "CLAUDE.md",
		"cd ~/.claude/skills/chat && sub a b notes.md":                   "~/.claude/skills/chat",
		"F=~/.claude/decisions.md; sub a b \"$F\"":                       "~/.claude/decisions.md",
		"echo x | tee CLAUDE.md > /dev/null":                             "CLAUDE.md",
		"echo x | sudo tee -a /etc/x CLAUDE.md":                          "CLAUDE.md",
		"sed -i.bak 's/a/b/' CLAUDE.md":                                  "CLAUDE.md",
		"perl -0pi -e 's/a/b/' SKILL.md":                                 "SKILL.md",
		"sed -E -i 's/a/b/' CLAUDE.md":                                   "CLAUDE.md",
		"perl -pi -e 's/a/b/' CLAUDE.md":                                 "CLAUDE.md",
		"perl -p -i -e 's/a/b/' CLAUDE.md":                               "CLAUDE.md",
		"grep -l x CLAUDE.md | xargs sed -i 's/a/b/' AGENTS.md":          "AGENTS.md",
		"find . -name CLAUDE.md -exec sed -i 's/a/b/' {} +":              "CLAUDE.md",
		"timeout 5 sub a b SKILL.md":                                     "SKILL.md",
		"echo x > CLAUDE.md":                                             "CLAUDE.md",
		"echo x &>> CLAUDE.md":                                           "CLAUDE.md",
		"{ echo a; echo b; } >> CLAUDE.md":                               "CLAUDE.md",
		"for f in a b; do echo $f; done > CLAUDE.md":                     "CLAUDE.md",
		"(echo a) > CLAUDE.md":                                           "CLAUDE.md",
		"> CLAUDE.md":                                                    "CLAUDE.md",
		"if true; then sub a b CLAUDE.md; fi":                            "CLAUDE.md",
		"echo x | tee >(sub a b CLAUDE.md) >/dev/null":                   "CLAUDE.md",
		"r=$(sub a b CLAUDE.md)":                                         "CLAUDE.md",
	}
	for command, want := range cases {
		if got := bashRuleFile(command); got != want {
			t.Errorf("%q: got %q, want %q", command, got, want)
		}
	}
}

// Формы вызова редактора, которые регулярка пропускала (тикет
// understand-bash-editor-missed-forms, проверка 05.11)
func TestBashRuleFile_MissedForms(t *testing.T) {
	commands := []string{
		"'sub' a b CLAUDE.md",
		"\"sub\" a b CLAUDE.md",
		"\\sub a b CLAUDE.md",
		"~/bin/sub a b CLAUDE.md",
		"$HOME/bin/sub a b CLAUDE.md",
		"\"$HOME/bin/tee\" CLAUDE.md",
		"bash -c \"sub a b CLAUDE.md\"",
		"bash --norc -c 'tee CLAUDE.md </dev/null'",
		"zsh -c 'sub a b CLAUDE.md'",
		"fish -c 'sub a b CLAUDE.md'",
		"timeout 5 bash -c 'sub a b CLAUDE.md'",
		"bash -c $'sub a b CLAUDE.md'",
		"bash -c \"bash -c 'sub a b CLAUDE.md'\"",
		"E=sub; $E a b CLAUDE.md",
		"r=`sub a b CLAUDE.md`",
		"sed --in-place=.bak s/a/b/ CLAUDE.md",
		"sed -nEi s/a/b/p CLAUDE.md",
		"sed -e s/a/b/ -i CLAUDE.md",
		"sed s/a/b/ CLAUDE.md -i",
		"perl -pe s/a/b/ -i CLAUDE.md",
		"perl -I/lib -pi -e s/a/b/ CLAUDE.md",
		"awk 'BEGIN{system(\"sub a b CLAUDE.md\")}'",
		// ; | & в кавычках скрипта, перенос строки, \n перед sed: проверка b3f4859
		"sed -e 's/a/b/; s/c/d/' -i CLAUDE.md",
		"sed -E 's/(a|b)/c/' CLAUDE.md -i",
		"perl -pe 's/a/b/; s/c/d/' -i CLAUDE.md",
		"sed 's/&/x/' -i CLAUDE.md",
		"sed -e 's/a/b/' \\\n-i CLAUDE.md",
		"printf 'cd /x\\nsed -i s/a/b/ CLAUDE.md\\n' | sh",
	}
	for _, command := range commands {
		if got := bashRuleFile(command); got != "CLAUDE.md" {
			t.Errorf("%q: got %q, want CLAUDE.md", command, got)
		}
	}
	// Цель перенаправления с переменной, без слова редактора в команде
	redirects := map[string]string{
		"{ echo x; } >> \"$HOME/.claude/CLAUDE.md\"": "$HOME/.claude/CLAUDE.md",
		"echo x > \"$D/CLAUDE.md\"":                  "$D/CLAUDE.md",
		"echo x > ${D}/CLAUDE.md":                    "${D}/CLAUDE.md",
	}
	for command, want := range redirects {
		if got := bashRuleFile(command); got != want {
			t.Errorf("%q: got %q, want %q", command, got, want)
		}
	}
}

func TestClassify_EditOutsideInstructions(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"file_path": "/home/u/git/x/main.go", "old_string": "a", "new_string": "b"})
	if _, _, _, ok := classify("Edit", raw); ok {
		t.Fatal("edit of code is not a case")
	}
	raw, _ = json.Marshal(map[string]any{"file_path": "/home/u/.claude/CLAUDE.md", "old_string": "a", "new_string": "b"})
	kind, file, action, ok := classify("Edit", raw)
	if !ok || kind != KindRule || file != "/home/u/.claude/CLAUDE.md" || !strings.Contains(action, "Было:\na\nСтало:\nb") {
		t.Fatalf("edit of CLAUDE.md: %v %q %q", ok, file, action)
	}
}

func TestParseVerdict(t *testing.T) {
	v, err := ParseVerdict("```json\n{\"verdict\":\"ok\"}\n```")
	if err != nil || v.Verdict != "ok" {
		t.Fatalf("ok: %v %+v", err, v)
	}
	v, err = ParseVerdict(`{"verdict":"fix","fixes":[{"quote":"по таймеру","action":"или сразу","why":"добавлен вариант","fix":"убрать"}]}`)
	if err != nil || len(v.Fixes) != 1 || v.Fixes[0].Quote != "по таймеру" {
		t.Fatalf("fix: %v %+v", err, v)
	}
	for _, bad := range []string{`{"verdict":"fix"}`, `{"verdict":"maybe"}`, "нет JSON"} {
		if _, err := ParseVerdict(bad); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}

func TestInput_ClipsPrevFromStart(t *testing.T) {
	c := Case{Prev: strings.Repeat("а", prevRunes) + "ВОПРОС?", Human: "да", Kind: KindSend, Action: "Кому: x\nm"}
	in := c.Input()
	if !strings.Contains(in, "ВОПРОС?") || !strings.Contains(in, "[обрезано]…") {
		t.Fatalf("prev must keep its end: %s", in[:200])
	}
}
