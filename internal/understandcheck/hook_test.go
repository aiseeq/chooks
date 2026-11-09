package understandcheck

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/chooks/internal/questioncheck"
)

// Строки транскрипта в формате Claude Code (записи 02.11, сокращены)
var clock = time.Date(2026, time.November, 2, 10, 0, 0, 0, time.UTC)

func stamp(n int) string { return clock.Add(time.Duration(n) * time.Second).Format(time.RFC3339Nano) }

func humanLine(n int, text string) string {
	content, _ := json.Marshal(text)
	return fmt.Sprintf(`{"parentUuid":null,"isSidechain":false,"type":"user","message":{"role":"user","content":%s},"uuid":"u%d","timestamp":"%s","origin":{"kind":"human"},"sessionId":"s"}`, content, n, stamp(n))
}

func peerLine(n int, from, text string) string {
	content, _ := json.Marshal("Another Claude session sent a message:\n<cross-session-message from-name=\"" + from + "\">\n" + text + "\n</cross-session-message>")
	return fmt.Sprintf(`{"parentUuid":null,"isSidechain":false,"type":"user","message":{"role":"user","content":%s},"uuid":"p%d","timestamp":"%s","origin":{"kind":"peer"},"sessionId":"s"}`, content, n, stamp(n))
}

func agentLine(n int, text, stop string) string {
	content, _ := json.Marshal(text)
	return fmt.Sprintf(`{"parentUuid":null,"isSidechain":false,"message":{"model":"claude-opus-5-5","id":"m%d","type":"message","role":"assistant","content":[{"type":"text","text":%s}],"stop_reason":"%s"},"type":"assistant","uuid":"a%d","timestamp":"%s","sessionId":"s"}`, n, content, stop, n, stamp(n))
}

func writeTranscript(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadTurn_RepliesAfterFinishedAnswer(t *testing.T) {
	path := writeTranscript(t,
		agentLine(1, "Выкатить по таймеру или сразу?", "end_turn"),
		humanLine(2, "по таймеру"),
		humanLine(3, "и только в проекте A"),
		agentLine(4, "Передаю.", "tool_use"),
	)
	turn, err := ReadTurn(path)
	if err != nil {
		t.Fatal(err)
	}
	if !turn.InTurn || !turn.Anchored || !turn.Decided {
		t.Fatalf("turn state wrong: %+v", turn)
	}
	if turn.Prev != "Выкатить по таймеру или сразу?" {
		t.Errorf("prev = %q", turn.Prev)
	}
	if !strings.Contains(turn.Human, "[реплика 1 из 2]\nпо таймеру") || !strings.Contains(turn.Human, "и только в проекте A") {
		t.Errorf("human = %q", turn.Human)
	}
}

func TestReadTurn_PeerStartsTurn(t *testing.T) {
	path := writeTranscript(t,
		agentLine(1, "Вопрос?", "end_turn"),
		humanLine(2, "да"),
		agentLine(3, "Сделал.", "end_turn"),
		peerLine(4, "git-admin", "итог"),
	)
	turn, err := ReadTurn(path)
	if err != nil {
		t.Fatal(err)
	}
	if turn.InTurn || !turn.Decided {
		t.Fatalf("peer turn must be decided and not a user turn: %+v", turn)
	}
}

// Сценарий 1 противника: письмо другой сессии посреди хода пользователя не
// снимает проверку с действий, которые исполняют его реплику
func TestReadTurn_PeerMidTurnKeepsUserTurn(t *testing.T) {
	path := writeTranscript(t,
		agentLine(1, "Передать мастерам?", "end_turn"),
		humanLine(2, "а у них тикеты не копятся?"),
		peerLine(3, "glint-master", "готово"),
		agentLine(4, "Пишу.", "tool_use"),
	)
	turn, err := ReadTurn(path)
	if err != nil {
		t.Fatal(err)
	}
	if !turn.InTurn || turn.Human != "а у них тикеты не копятся?" || turn.Prev != "Передать мастерам?" {
		t.Fatalf("user turn lost after mid-turn peer: %+v", turn)
	}
}

// Окно растёт, пока реплика не привязана к завершённому ответу: между ними
// больше первого окна
func TestReadTurn_WindowGrowsToFinishedAnswer(t *testing.T) {
	lines := []string{agentLine(1, "Выкатить сразу?", "end_turn")}
	filler := strings.Repeat("ш", 2000)
	for i := range 400 {
		lines = append(lines, agentLine(10+i, filler, "tool_use"))
	}
	lines = append(lines, humanLine(1000, "нет"))
	path := writeTranscript(t, lines...)
	if info, _ := os.Stat(path); info.Size() < 1<<20 {
		t.Fatalf("fixture must exceed the first window, got %d bytes", info.Size())
	}
	// Реплика после 400 текстов без конца хода: агент работал, пользователь
	// написал посреди — prev всё равно конец прошлого хода
	turn, err := ReadTurn(path)
	if err != nil {
		t.Fatal(err)
	}
	if !turn.Anchored || turn.Prev != "Выкатить сразу?" || turn.Human != "нет" {
		t.Fatalf("window did not grow to the finished answer: anchored=%v prev=%.40q human=%q", turn.Anchored, turn.Prev, turn.Human)
	}
}

// fakeJudge отвечает заданным текстом или ошибкой; hang — ждать конца контекста
type fakeJudge struct {
	answer string
	err    error
	hang   bool
	calls  int
	inputs []string
}

func (f *fakeJudge) Model() string { return "fake" }

func (f *fakeJudge) Ask(ctx context.Context, input string) (string, questioncheck.Usage, error) {
	f.calls++
	f.inputs = append(f.inputs, input)
	if f.hang {
		<-ctx.Done()
		return "", questioncheck.Usage{}, ctx.Err()
	}
	return f.answer, questioncheck.Usage{InputTokens: 10, OutputTokens: 2}, f.err
}

const fixAnswer = `{"verdict":"fix","fixes":[{"quote":"а у них тикеты не копятся?","action":"По каждому: закрыть","why":"вопрос исполнен как указание","fix":"спросить, а не поручать"}]}`

type hookFixture struct {
	hook  Hook
	judge *fakeJudge
	path  string
}

func newHookFixture(t *testing.T, answer string, lines ...string) hookFixture {
	t.Helper()
	dir := t.TempDir()
	judge := &fakeJudge{answer: answer}
	if len(lines) == 0 {
		lines = []string{
			agentLine(1, "Передать мастерам?", "end_turn"),
			humanLine(2, "а у них тикеты не копятся?"),
		}
	}
	return hookFixture{
		hook: Hook{
			Judge:       judge,
			JournalPath: filepath.Join(dir, "journal.jsonl"),
			DeniedDir:   filepath.Join(dir, "denied"),
			Budget:      time.Second,
			Now:         func() time.Time { return time.Date(2026, time.November, 9, 14, 30, 0, 0, time.Local) },
			Warn:        func(string, ...any) {},
		},
		judge: judge,
		path:  writeTranscript(t, lines...),
	}
}

func (f hookFixture) send(to, message string) Call {
	raw, _ := json.Marshal(map[string]any{"to": to, "message": message})
	return Call{SessionID: "sess-1", TranscriptPath: f.path, Tool: "SendMessage", Input: raw}
}

func (f hookFixture) journal(t *testing.T) []Entry {
	t.Helper()
	file, err := os.Open(f.hook.JournalPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var entries []Entry
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		var e Entry
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, e)
	}
	return entries
}

func TestHook_FixDeniesOnceThenRepeatPasses(t *testing.T) {
	f := newHookFixture(t, fixAnswer)
	ctx := context.Background()
	first := f.hook.Run(ctx, f.send("glint-master", "По каждому: закрыть или вернуть"))
	if !first.Deny || first.Verdict != VerdictFix {
		t.Fatalf("fix must deny: %+v", first)
	}
	for _, want := range []string{"«а у них тикеты не копятся?»", "Расхождение: вопрос исполнен как указание", "повтори тот же вызов без изменений"} {
		if !strings.Contains(first.Reason, want) {
			t.Errorf("reason lacks %q:\n%s", want, first.Reason)
		}
	}
	if !strings.Contains(f.judge.inputs[0], "Передать мастерам?") || !strings.Contains(f.judge.inputs[0], "Кому: glint-master") {
		t.Errorf("judge input lacks prev or action:\n%s", f.judge.inputs[0])
	}
	repeat := f.hook.Run(ctx, f.send("glint-master", "По каждому: закрыть или вернуть"))
	if repeat.Deny || repeat.Verdict != VerdictOverride {
		t.Fatalf("identical repeat must pass as override: %+v", repeat)
	}
	// Сценарий 4: тот же текст другому адресату — то же решение, без судьи
	other := f.hook.Run(ctx, f.send("graft-master", "По каждому: закрыть или вернуть"))
	if other.Deny || other.Verdict != VerdictOverride {
		t.Fatalf("same text to another recipient must pass as override: %+v", other)
	}
	if f.judge.calls != 1 {
		t.Errorf("judge calls = %d, want 1", f.judge.calls)
	}
	entries := f.journal(t)
	if len(entries) != 3 || entries[0].Verdict != VerdictFix || entries[1].Verdict != VerdictOverride || entries[0].Key != entries[1].Key {
		t.Fatalf("journal wrong: %+v", entries)
	}
	if entries[0].Human != "а у них тикеты не копятся?" || len(entries[0].Fixes) != 1 || entries[0].Usage == nil || entries[0].DenyRev != DenyRev {
		t.Errorf("fix entry incomplete: %+v", entries[0])
	}
}

// Сценарий 3: отказ прошлого хода не пропускает тот же вызов после новой реплики
func TestHook_NewReplyJudgesRepeatAgain(t *testing.T) {
	f := newHookFixture(t, fixAnswer)
	ctx := context.Background()
	call := f.send("glint-master", "Запретить X")
	if !f.hook.Run(ctx, call).Deny {
		t.Fatal("first call must be denied")
	}
	f.path = writeTranscript(t,
		agentLine(1, "Передать мастерам?", "end_turn"),
		humanLine(2, "а у них тикеты не копятся?"),
		agentLine(3, "Не отправил.", "end_turn"),
		humanLine(4, "с X осторожнее"),
	)
	again := f.hook.Run(ctx, f.send("glint-master", "Запретить X"))
	if !again.Deny || f.judge.calls != 2 {
		t.Fatalf("same call after a new reply must be judged again: %+v, calls %d", again, f.judge.calls)
	}
}

func TestHook_OKPasses(t *testing.T) {
	f := newHookFixture(t, `{"verdict":"ok"}`)
	out := f.hook.Run(context.Background(), f.send("x", "Пользователь спрашивает: копятся ли тикеты?"))
	if out.Deny || out.Verdict != VerdictOK {
		t.Fatalf("ok must pass: %+v", out)
	}
	if e := f.journal(t); len(e) != 1 || e[0].Verdict != VerdictOK || !e[0].Anchored {
		t.Fatalf("journal wrong: %+v", e)
	}
}

func TestHook_JudgeFailureLetsThrough(t *testing.T) {
	for _, tc := range []struct {
		name  string
		judge fakeJudge
		want  string
	}{
		{"error", fakeJudge{err: errors.New("claude exited 1")}, VerdictError},
		{"unparsable", fakeJudge{answer: "не знаю"}, VerdictError},
		{"timeout", fakeJudge{hang: true}, VerdictTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHookFixture(t, "")
			judge := tc.judge
			f.hook.Judge = &judge
			f.hook.Budget = 20 * time.Millisecond
			out := f.hook.Run(context.Background(), f.send("x", "текст"))
			if out.Deny || out.Verdict != tc.want {
				t.Fatalf("failure must let through with %s: %+v", tc.want, out)
			}
			if e := f.journal(t); len(e) != 1 || e[0].Verdict != tc.want || e[0].Error == "" {
				t.Fatalf("journal wrong: %+v", e)
			}
			// После сбоя ключ не записан: повтор снова к судье, не override
			if again := f.hook.Run(context.Background(), f.send("x", "текст")); again.Verdict == VerdictOverride {
				t.Fatalf("failure must not store a key: %+v", again)
			}
		})
	}
}

func TestHook_SkipsWithoutJudge(t *testing.T) {
	peerTurn := []string{
		agentLine(1, "Вопрос?", "end_turn"),
		humanLine(2, "да"),
		agentLine(3, "Сделал.", "end_turn"),
		peerLine(4, "git-admin", "итог"),
	}
	edit, _ := json.Marshal(map[string]any{"file_path": "/home/u/git/p/main.go", "old_string": "a", "new_string": "b"})
	bash, _ := json.Marshal(map[string]any{"command": "go test ./..."})
	for _, tc := range []struct {
		name  string
		lines []string
		call  func(f hookFixture) Call
	}{
		{"subagent", nil, func(f hookFixture) Call { c := f.send("x", "t"); c.AgentID = "a1"; return c }},
		{"edit outside instructions", nil, func(f hookFixture) Call {
			return Call{SessionID: "sess-1", TranscriptPath: f.path, Tool: "Edit", Input: edit}
		}},
		{"plain bash", nil, func(f hookFixture) Call {
			return Call{SessionID: "sess-1", TranscriptPath: f.path, Tool: "Bash", Input: bash}
		}},
		{"peer turn", peerTurn, func(f hookFixture) Call { return f.send("lead", "итог") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHookFixture(t, fixAnswer, tc.lines...)
			out := f.hook.Run(context.Background(), tc.call(f))
			if out.Deny || out.Skip == "" || f.judge.calls != 0 {
				t.Fatalf("must skip without judge: %+v, calls %d", out, f.judge.calls)
			}
			if e := f.journal(t); len(e) != 0 {
				t.Fatalf("skip must not be journaled: %+v", e)
			}
		})
	}
}

func TestHook_RuleEditJudged(t *testing.T) {
	f := newHookFixture(t, fixAnswer)
	edit, _ := json.Marshal(map[string]any{"file_path": "/home/u/.claude/CLAUDE.md", "old_string": "a", "new_string": "Запрещено X"})
	out := f.hook.Run(context.Background(), Call{SessionID: "sess-1", TranscriptPath: f.path, Tool: "Edit", Input: edit})
	if !out.Deny {
		t.Fatalf("instruction edit with fix must deny: %+v", out)
	}
	if e := f.journal(t); len(e) != 1 || e[0].Kind != KindRule || e[0].File != "/home/u/.claude/CLAUDE.md" {
		t.Fatalf("journal wrong: %+v", e)
	}
}

func TestHook_BadSessionIDLetsThrough(t *testing.T) {
	f := newHookFixture(t, fixAnswer)
	call := f.send("x", "t")
	call.SessionID = "../evil"
	if out := f.hook.Run(context.Background(), call); out.Deny || out.Verdict != VerdictError {
		t.Fatalf("bad session id must let through with error: %+v", out)
	}
}

// Значение из реплики, предыдущего ответа и действия не доходит ни до судьи,
// ни до журнала, в том числе начало ключа после среза
func TestHook_HidesSecrets(t *testing.T) {
	body := "Qz7Xw2Rt9Lm4Vb6Nc1Pk8Jh3Gf5Ds0Ay"
	key := "sk_live" + "_" + body
	f := newHookFixture(t, `{"verdict":"ok"}`,
		agentLine(1, key+" Передать мастерам?", "end_turn"),
		humanLine(2, strings.Repeat("я", humanRunes-len("sk_live_Qz"))+key),
	)
	out := f.hook.Run(context.Background(), f.send("x", "ключ "+key))
	if out.Deny || out.Verdict != VerdictOK {
		t.Fatalf("ok must pass: %+v", out)
	}
	entries := f.journal(t)
	if len(entries) != 1 || len(f.judge.inputs) != 1 {
		t.Fatalf("journal %+v, inputs %d", entries, len(f.judge.inputs))
	}
	for where, text := range map[string]string{
		"судья":            f.judge.inputs[0],
		"журнал: реплика":  entries[0].Human,
		"журнал: ответ":    entries[0].Prev,
		"журнал: действие": entries[0].Action,
	} {
		for _, part := range []string{body[:4], "sk_live" + "_"} {
			if strings.Contains(text, part) {
				t.Errorf("%s: %q passed", where, part)
			}
		}
	}
	if !strings.Contains(entries[0].Action, "[скрыто:sk_live]") {
		t.Errorf("action must show what was hidden: %q", entries[0].Action)
	}
}

// Срез конца сообщения агента посреди ключа не оставляет его хвост
func TestClipHeadHidesSecretTail(t *testing.T) {
	body := "Qz7Xw2Rt9Lm4Vb6Nc1Pk8Jh3Gf5Ds0Ay"
	text := "sk_live" + "_" + body + strings.Repeat("я", 20)
	if got := clipHead(text, 25); strings.Contains(got, body[len(body)-4:]) {
		t.Errorf("key tail passed: %q", got)
	}
}
