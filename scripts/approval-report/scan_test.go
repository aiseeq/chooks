package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Синтетическое дерево транскриптов: основная сессия не считается, ссылки — id:строка и задача/attN:строка.
func TestScanTree(t *testing.T) {
	root := t.TempDir()
	projects := filepath.Join(root, "projects")
	worker := filepath.Join(root, "worker")
	main := "8729f497-f8fa-4cce-96c9-9958d41a7c85"
	writeFile(t, filepath.Join(projects, "-home-aisee", main+".jsonl"), strings.Join([]string{
		`{"type":"agent-name","agentName":"old","sessionId":"x"}`,
		`{"type":"assistant","timestamp":"2026-11-10T10:00:00Z","cwd":"/home/aisee","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"cfg commit -m x"}}]}}`,
		`{"type":"agent-name","agentName":"lead","sessionId":"x"}`,
	}, "\n")+"\n")
	writeFile(t, filepath.Join(projects, "-home-aisee", main, "subagents", "agent-a558a3917a5bcc4db.jsonl"), strings.Join([]string{
		`{"type":"user","message":{"role":"user","content":"Сделай задачу PROJ-7"}}`,
		`{"type":"assistant","timestamp":"2026-11-10T10:00:00Z","cwd":"/home/aisee","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/home/aisee/.claude/skills/a/SKILL.md"}}]}}`,
		`{"type":"assistant","timestamp":"2026-11-10T10:01:00Z","cwd":"/home/aisee","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"curl -s -X POST https://x.atlassian.net/rest/api/3/issue/PROJ-7/comment -d @f"}}]}}`,
		`{"type":"assistant","timestamp":"2026-11-01T10:00:00Z","cwd":"/home/aisee","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"cfg push"}}]}}`,
		`{"type":"assistant","timestamp":"2026-11-10T10:02:00Z","cwd":"/home/aisee","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"grep x /home/aisee/.claude/CLAUDE.md"}}]}}`,
		`{"type":"assistant","timestamp":"2026-11-10T10:03:00Z","cwd":"/home/aisee","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"sub -F \"$TMPDIR/a.spec\""}}]}}`,
		`{"type":"user","timestamp":"2026-11-10T10:03:01Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"sub: 2 replacements in 2 files: /home/aisee/.claude/decisions.md 1 (line 4), /tmp/x.md 1 (line 2)"}]}}`,
	}, "\n")+"\n")
	writeFile(t, filepath.Join(worker, "1113-1209-x", "job.json"), `{"master":"x-master","dir":"/var/tmp/x","created":"2026-11-13T12:09:38+01:00","owner":{"name":"git-admin"}}`)
	writeFile(t, filepath.Join(worker, "1113-1209-x", "att1.jsonl"), strings.Join([]string{
		`{"type":"thread.started","thread_id":"t"}`,
		`{"type":"item.started","item":{"id":"item_1","type":"command_execution","command":"/bin/bash -lc 'lex build --codex /home/aisee/.claude/codex /home/aisee/.claude/skills/ai-research/SKILL.md'","status":"in_progress"}}`,
		`{"type":"item.completed","item":{"id":"item_1","type":"command_execution","command":"/bin/bash -lc 'lex build --codex /home/aisee/.claude/codex /home/aisee/.claude/skills/ai-research/SKILL.md'","exit_code":0}}`,
		`{"type":"item.completed","item":{"id":"item_2","type":"file_change","changes":[{"path":"/home/aisee/.claude/rules/go.md","kind":"update"}]}}`,
		`{"type":"item.started","item":{"id":"item_3","type":"command_execution","command":"/bin/bash -lc 'sub -F \"$TMPDIR/b.spec\"'","status":"in_progress"}}`,
		`{"type":"item.completed","item":{"id":"item_3","type":"command_execution","command":"/bin/bash -lc 'sub -F \"$TMPDIR/b.spec\"'","aggregated_output":"sub: 1 replacement in 1 file: /home/aisee/.claude/codex/frag/a/b.md 1 (line 4)\n","exit_code":0}}`,
		`{"type":"item.started","item":{"id":"item_4","type":"command_execution","command":"/bin/bash -lc 'sub -F \"$TMPDIR/c.spec\"'","status":"in_progress"}}`,
		`{"type":"item.completed","item":{"id":"item_4","type":"command_execution","command":"/bin/bash -lc 'sub -F \"$TMPDIR/c.spec\"'","aggregated_output":"sub: 1 replacement in 1 file: /var/tmp/x/plan.tsv 1 (line 4)\n","exit_code":0}}`,
	}, "\n")+"\n")

	from, _ := time.Parse(time.RFC3339, "2026-11-08T00:00:00+01:00")
	to, _ := time.Parse(time.RFC3339, "2026-11-15T13:24:00+01:00")
	sc := scanner{home: "/home/aisee", from: from, to: to, jobs: 2}
	res, err := sc.run(projects, worker)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range res.records {
		var cs []string
		for c := 1; c <= 4; c++ {
			if r.V.HitClass(c) {
				cs = append(cs, string(rune('0'+c)))
			}
		}
		got = append(got, r.Ref()+" "+r.Agent.Master+" "+strings.Join(cs, ","))
	}
	slices.Sort(got)
	want := []string{
		"1113-1209-x/att1.jsonl:2 x-master 1",
		"1113-1209-x/att1.jsonl:4 x-master 1",
		"1113-1209-x/att1.jsonl:5 x-master 1",
		"1113-1209-x/att1.jsonl:7 x-master ",
		"tl a558a391:2 lead 1",
		"tl a558a391:3 lead 4",
		"tl a558a391:5 lead ",
		"tl a558a391:6 lead 1",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("записи:\n%s\nждём:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	var out bytes.Buffer
	report(&out, res, from, to)
	reportChecks(&out, res, []string{"a558a391:3", "8729f497:2", "a558a391:5", "1113-1209-x"})
	for _, s := range []string{
		"Класс 4 (запись в Jira и Bitbucket): 1 событий у 1 агентов",
		"comment своей задачи 1",
		"Класс 1 (запись в инструкции и настройки): 2 событий у 1 агентов; кандидатов по тексту 3, отсеяно 1 (33.3%)",
		"Класс 1 (запись в инструкции и настройки): 3 событий у 1 агентов; кандидатов по тексту 3, отсеяно 0 (0.0%)",
		"чем: Edit 1, sub -F (по выводу) 1",
		"    1113-1209-x/att1.jsonl:4 — мастер x-master: file_change /home/aisee/.claude/rules/go.md",
		"a558a391:3: попадание, класс 4",
		"8729f497:2: не суб-агент и не боец в окне",
		"a558a391:5: кандидат класса 1, отсеян",
		"1113-1209-x: попадание, класс 1",
	} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("в отчёте нет %q:\n%s", s, out.String())
		}
	}
	if strings.Contains(out.String(), "secret") {
		t.Error("в отчёт попал query")
	}
}
