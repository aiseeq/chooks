package main

import (
	"bytes"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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

// Каталог вызова бойца — из rollout codex по тексту команды и номеру её повтора: att*.jsonl его не
// пишет, dir задачи — не каталог вызова (1110-1204-load-wait-hunt выкатывал синтетический проект
// из $TMPDIR при dir в worktree graft). Вызова нет в rollout — каталог неизвестен, выкатка.
func TestScanWorkerCallCwd(t *testing.T) {
	root := t.TempDir()
	worker := filepath.Join(root, "worker")
	if err := os.MkdirAll(filepath.Join(root, "projects"), 0o700); err != nil {
		t.Fatal(err)
	}
	task := filepath.Join(worker, "1110-1204-y")
	writeFile(t, filepath.Join(task, "job.json"), `{"master":"g-master","dir":"/home/aisee/.cache/worktrees/graft/y","created":"2026-11-10T12:04:00+01:00"}`)
	deploy := `/bin/bash -lc 'XDG_CONFIG_HOME=\"$TMPDIR/c\" graft deploy test'`
	other := `/bin/bash -lc 'XDG_CONFIG_HOME=\"$TMPDIR/c\" graft deploy stage'`
	// кавычки codex: двойные с \" внутри — в rollout тот же текст без экранирования
	escaped := `/bin/bash -lc \"echo \\\"a\\\" && XDG_CONFIG_HOME=/tmp/c graft deploy dev\"`
	writeFile(t, filepath.Join(task, "att1.jsonl"), strings.Join([]string{
		`{"type":"thread.started","thread_id":"t2"}`,
		`{"type":"item.completed","item":{"id":"item_1","type":"command_execution","command":"` + deploy + `","exit_code":0}}`,
		`{"type":"item.completed","item":{"id":"item_2","type":"command_execution","command":"` + deploy + `","exit_code":0}}`,
		`{"type":"item.completed","item":{"id":"item_3","type":"command_execution","command":"` + other + `","exit_code":0}}`,
		`{"type":"item.completed","item":{"id":"item_4","type":"command_execution","command":"` + escaped + `","exit_code":0}}`,
	}, "\n")+"\n")
	rollout := func(cwd string) string {
		return `{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"CommandExecution","command":["/bin/bash","-lc","XDG_CONFIG_HOME=\"$TMPDIR/c\" graft deploy test"],"cwd":"file://` + cwd + `"}}}`
	}
	writeFile(t, filepath.Join(task, "codex", "sessions", "2026", "11", "10", "rollout-2026-11-10T12-09-35-t2.jsonl"), strings.Join([]string{
		`{"type":"session_meta","payload":{"id":"t2"}}`,
		rollout("/var/tmp/w/project"),
		rollout("/home/aisee/git/proj"),
		`{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"CommandExecution","command":["/bin/bash","-lc","echo \"a\" && XDG_CONFIG_HOME=/tmp/c graft deploy dev"],"cwd":"file:///tmp/e"}}}`,
	}, "\n")+"\n")

	from, _ := time.Parse(time.RFC3339, "2026-11-08T00:00:00+01:00")
	to, _ := time.Parse(time.RFC3339, "2026-11-15T13:24:00+01:00")
	sc := scanner{home: "/home/aisee", from: from, to: to, jobs: 1}
	res, err := sc.run(filepath.Join(root, "projects"), worker)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int]string{}
	for _, r := range res.records {
		switch {
		case r.V.HitClass(3):
			got[r.Line] = "выкатка"
		case len(r.V.Possible) > 0:
			got[r.Line] = r.V.Possible[0].Prod
		}
	}
	want := map[int]string{2: "проба: каталог /var/tmp/w/project", 3: "выкатка", 4: "выкатка", 5: "проба: каталог /tmp/e"}
	if !maps.Equal(got, want) {
		t.Errorf("записи %v, ждём %v", got, want)
	}
}

// Все попытки задачи пишут в один rollout: номер повтора команды считается по thread через
// попытки по номеру (att2 раньше att10), и попытка вне окна тоже считается. Каталог в rollout —
// в URL-кодировке. Каталог из rollout может сделать событие неинтересным: тогда записи нет.
func TestScanWorkerAttempts(t *testing.T) {
	root := t.TempDir()
	worker := filepath.Join(root, "worker")
	if err := os.MkdirAll(filepath.Join(root, "projects"), 0o700); err != nil {
		t.Fatal(err)
	}
	task := filepath.Join(worker, "1109-1933-z")
	writeFile(t, filepath.Join(task, "job.json"), `{"master":"m","dir":"/home/aisee/.claude/skills/x","created":"2026-11-09T19:33:00+01:00"}`)
	deploy := `/bin/bash -lc 'XDG_CONFIG_HOME=/tmp/c graft deploy test'`
	att := func(name, body string) {
		writeFile(t, filepath.Join(task, name), `{"type":"thread.started","thread_id":"t3"}`+"\n"+body+"\n")
	}
	item := func(id, cmd string) string {
		return `{"type":"item.completed","item":{"id":"` + id + `","type":"command_execution","command":"` + cmd + `","exit_code":0}}`
	}
	att("att1.jsonl", item("item_1", deploy))
	old := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(task, "att1.jsonl"), old, old); err != nil {
		t.Fatal(err)
	}
	att("att2.jsonl", item("item_1", deploy)+"\n"+item("item_2", `/bin/bash -lc 'echo a > SKILL.md'`))
	att("att10.jsonl", item("item_1", deploy))
	// попытка в другом thread: свой счёт повторов
	writeFile(t, filepath.Join(task, "att11.jsonl"), `{"type":"thread.started","thread_id":"t4"}`+"\n"+item("item_1", deploy)+"\n")
	writeFile(t, filepath.Join(task, "attX.jsonl"), `{"type":"thread.started","thread_id":"t5"}`+"\n")
	rec := func(cmd, cwd string) string {
		return `{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"CommandExecution","command":["/bin/bash","-lc","` + cmd + `"],"cwd":"file://` + cwd + `"}}}`
	}
	writeFile(t, filepath.Join(task, "codex", "sessions", "2026", "11", "09", "rollout-2026-11-09T19-33-00-t3.jsonl"), strings.Join([]string{
		rec("XDG_CONFIG_HOME=/tmp/c graft deploy test", "/home/aisee/git/proj"),
		rec("XDG_CONFIG_HOME=/tmp/c graft deploy test", "/var/tmp/%D1%8F"),
		rec("echo a > SKILL.md", "/tmp/q"),
		rec("XDG_CONFIG_HOME=/tmp/c graft deploy test", "/home/aisee/git/proj2"),
	}, "\n")+"\n")
	writeFile(t, filepath.Join(task, "codex", "sessions", "2026", "11", "09", "rollout-2026-11-09T20-00-00-t4.jsonl"), rec("XDG_CONFIG_HOME=/tmp/c graft deploy test", "/var/tmp/b")+"\n")

	from, _ := time.Parse(time.RFC3339, "2026-11-08T00:00:00+01:00")
	sc := scanner{home: "/home/aisee", from: from, to: time.Now().Add(time.Hour), jobs: 1}
	res, err := sc.run(filepath.Join(root, "projects"), worker)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range res.records {
		ref := r.File + ":" + strconv.Itoa(r.Line)
		switch {
		case r.V.HitClass(3):
			got[ref] = "выкатка"
		case len(r.V.Possible) > 0:
			got[ref] = r.V.Possible[0].Prod
		default:
			got[ref] = "запись без класса"
		}
	}
	want := map[string]string{"att2.jsonl:2": "проба: каталог /var/tmp/я", "att10.jsonl:2": "выкатка", "att11.jsonl:2": "проба: каталог /var/tmp/b"}
	if !maps.Equal(got, want) {
		t.Errorf("записи %v, ждём %v", got, want)
	}
	// файлов в окне — попытки в окне (att1 — вне), имя без номера — ошибка чтения
	if res.files["worker"] != 4 {
		t.Errorf("файлов %d, ждём 4", res.files["worker"])
	}
	if len(res.errs) != 1 || !strings.Contains(res.errs[0], "номер попытки attX.jsonl") {
		t.Errorf("ошибки %v", res.errs)
	}
}
