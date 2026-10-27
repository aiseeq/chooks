package questioncheck

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stubJudge — судья-заглушка: живую модель тесты не зовут
type stubJudge struct {
	calls   int
	verdict Verdict
	err     error
	// hang — ждать отмены контекста (таймаут судьи)
	hang bool
	// input — что получил судья последним
	input string
}

func (s *stubJudge) Model() string { return "stub/low" }

func (s *stubJudge) Judge(ctx context.Context, input string) (Verdict, Usage, error) {
	s.calls++
	s.input = input
	if s.hang {
		<-ctx.Done()
		return Verdict{}, Usage{}, ctx.Err()
	}
	return s.verdict, Usage{InputTokens: 100, OutputTokens: 10}, s.err
}

type fixture struct {
	checker    Checker
	judge      *stubJudge
	transcript string
	warnings   []string
}

const (
	testPID  = "4242"
	humanMsg = "давай посчитаем от 100-баксовой подписки"
)

// newFixture — домашний каталог с записью сессии name и транскриптом в
// каталоге проекта project
func newFixture(t *testing.T, name, project string) *fixture {
	t.Helper()
	home := t.TempDir()
	claudeDir := filepath.Join(home, ".claude")
	writeFile(t, filepath.Join(claudeDir, "sessions", testPID+".json"),
		`{"pid":4242,"sessionId":"s1","name":"`+name+`","cwd":"`+home+`"}`)
	writeFile(t, filepath.Join(claudeDir, "user-replies.md"),
		"# Реплики\n\n### 2026-10-20T14:01:16.007Z (20.10 16:01 CEST) · ~/work · claude x\nв ответ на: Копия артефакта Xq7pTz3mKd9LbWv2 удалить?\nДа, зачем мне копия?\n")
	writeFile(t, filepath.Join(claudeDir, "decisions.md"), "# Журнал решений\n\n## Записи\n")

	projectDir := strings.ReplaceAll(project, "~", ProjectDirName(home))
	transcript := filepath.Join(claudeDir, "projects", projectDir, "s1.jsonl")
	writeFile(t, transcript, strings.Join([]string{
		`{"type":"user","uuid":"u1","timestamp":"2026-10-23T10:00:00Z","origin":{"kind":"human"},"message":{"role":"user","content":"` + humanMsg + `"}}`,
		`{"type":"attachment","uuid":"a1","timestamp":"2026-10-23T10:00:00Z","attachment":{"type":"queued_command","prompt":"сообщение другой сессии","origin":{"kind":"peer"}}}`,
		`{"type":"assistant","uuid":"m1","timestamp":"2026-10-23T10:00:00Z","message":{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"Посчитал. Поменять на $200?"}]}}`,
		`{"type":"assistant","uuid":"m2","timestamp":"2026-10-23T10:00:00Z","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash"}]}}`,
		`{"type":"system","subtype":"turn_duration"}`,
	}, "\n")+"\n")

	judge := &stubJudge{verdict: Verdict{Verdict: VerdictPass}}
	f := &fixture{judge: judge, transcript: transcript}
	f.checker = Checker{
		Scope:       Scope{ClaudeDir: claudeDir, Targets: DefaultTargets(home)},
		Journals:    DefaultJournals(claudeDir),
		Judge:       judge,
		JournalPath: filepath.Join(home, "state", "question-check.jsonl"),
		Now:         func() time.Time { return time.Date(2026, 10, 23, 12, 0, 0, 0, time.Local) },
		Budget:      time.Second,
		Warn: func(msg string, args ...any) {
			f.warnings = append(f.warnings, msg)
		},
	}
	return f
}

func (f *fixture) event(message string) Event {
	return Event{SessionID: "s1", TranscriptPath: f.transcript, LastMessage: message, ClaudePID: testPID}
}

func (f *fixture) journal(t *testing.T) []Entry {
	t.Helper()
	file, err := os.Open(f.checker.JournalPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var entries []Entry
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var entry Entry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			t.Fatalf("journal line %q: %v", scanner.Text(), err)
		}
		entries = append(entries, entry)
	}
	return entries
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

const questionMessage = "Посчитал от $100. Поменять на $200?"

func TestRun_BlocksRedundantQuestion(t *testing.T) {
	f := newFixture(t, "lead", "~")
	f.judge.verdict = Verdict{Verdict: VerdictBlock, Lishnie: []Finding{{
		Quote: "Поменять на $200?", Why: "ответ в прошлой реплике", Where: "прошлая реплика",
	}}}

	result := f.checker.Run(context.Background(), f.event(questionMessage))

	if !result.Block || !strings.Contains(result.Reason, "Поменять на $200?") || !strings.Contains(result.Reason, "Искал") {
		t.Fatalf("result = %+v", result)
	}
	// Снятый вопрос виден пользователю: где нашёл ответ или что сделал сам
	for _, want := range []string{"«Хотел спросить \"X\", но нашёл в Y, что ответ Z»", "«Хотел спросить \"X\", сделал сам: …»"} {
		if !strings.Contains(result.Reason, want) {
			t.Errorf("в тексте остановки нет %s:\n%s", want, result.Reason)
		}
	}
	if !strings.Contains(f.judge.input, humanMsg) || strings.Contains(f.judge.input, "сообщение другой сессии") {
		t.Fatalf("judge input lacks the human reply or carries a session message:\n%s", f.judge.input)
	}
	entries := f.journal(t)
	if len(entries) != 1 {
		t.Fatalf("journal = %+v", entries)
	}
	entry := entries[0]
	if entry.Verdict != VerdictBlock || entry.BlockRev != BlockRev || entry.Session != "lead" || entry.MessageID != "m1" || !entry.HumanFound ||
		entry.Model != "stub/low" || entry.Transcript != f.transcript || len(entry.Questions) != 1 || entry.Usage == nil {
		t.Fatalf("entry = %+v", entry)
	}
	if _, err := time.Parse(time.RFC3339, entry.Time); err != nil || !strings.ContainsAny(entry.Time[19:], "+-Z") {
		t.Fatalf("time %q has no zone: %v", entry.Time, err)
	}
}

func TestRun_PassesNeededQuestion(t *testing.T) {
	f := newFixture(t, "work-admin", "~-work")
	result := f.checker.Run(context.Background(), f.event("Выкатить admin на prod сейчас?"))
	if result.Block || f.judge.calls != 1 {
		t.Fatalf("result = %+v, calls = %d", result, f.judge.calls)
	}
	if entries := f.journal(t); len(entries) != 1 || entries[0].Verdict != VerdictPass || entries[0].Session != "work-admin" {
		t.Fatalf("journal = %+v", entries)
	}
}

// Выдержка из журнала реплик доходит до судьи
func TestRun_JudgeSeesJournalExcerpt(t *testing.T) {
	f := newFixture(t, "lead", "~")
	f.checker.Run(context.Background(), f.event("Артефакт Xq7pTz3mKd9LbWv2 остался. Удалить?"))
	if !strings.Contains(f.judge.input, "Да, зачем мне копия?") {
		t.Fatalf("judge input lacks the excerpt:\n%s", f.judge.input)
	}
}

func TestRun_SessionSelection(t *testing.T) {
	cases := []struct {
		name    string
		session string
		project string
	}{
		{"чужая сессия", "shop-master", "~-work-shop"},
		{"имя lead в чужом каталоге", "lead", "~-work"},
		{"имя work-admin в каталоге лида", "work-admin", "~"},
		{"сессия без имени", "", "~"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, tc.session, tc.project)
			result := f.checker.Run(context.Background(), f.event(questionMessage))
			if result.Block || result.Skip == "" || f.judge.calls != 0 || len(f.journal(t)) != 0 {
				t.Fatalf("result = %+v, calls = %d", result, f.judge.calls)
			}
		})
	}
}

func TestRun_NoSessionRecordIsNoop(t *testing.T) {
	f := newFixture(t, "lead", "~")
	event := f.event(questionMessage)
	event.ClaudePID = "999999"
	if result := f.checker.Run(context.Background(), event); result.Skip == "" || f.judge.calls != 0 {
		t.Fatalf("result = %+v, calls = %d", result, f.judge.calls)
	}
}

func TestRun_SubagentIsNoop(t *testing.T) {
	f := newFixture(t, "lead", "~")
	event := f.event(questionMessage)
	event.AgentID = "agent-1"
	if result := f.checker.Run(context.Background(), event); result.Skip == "" || f.judge.calls != 0 {
		t.Fatalf("result = %+v, calls = %d", result, f.judge.calls)
	}
}

func TestRun_AfterDeadlineIsNoop(t *testing.T) {
	f := newFixture(t, "lead", "~")
	f.checker.Now = func() time.Time { return Until }
	if result := f.checker.Run(context.Background(), f.event(questionMessage)); result.Skip == "" || f.judge.calls != 0 {
		t.Fatalf("result = %+v, calls = %d", result, f.judge.calls)
	}
	f.checker.Now = func() time.Time { return Until.Add(-time.Minute) }
	f.checker.Run(context.Background(), f.event(questionMessage))
	if f.judge.calls != 1 {
		t.Fatalf("30.10 23:59 still in the experiment, calls = %d", f.judge.calls)
	}
}

func TestRun_NoQuestionNoCall(t *testing.T) {
	f := newFixture(t, "lead", "~")
	result := f.checker.Run(context.Background(), f.event("Готово, коммит abc123. ```\nДелать?\n```"))
	if result.Block || f.judge.calls != 0 || len(f.journal(t)) != 0 {
		t.Fatalf("result = %+v, calls = %d", result, f.judge.calls)
	}
}

// Ход после остановки не блокируется повторно, а сообщение уходит в журнал:
// по нему итог недели видит, что агент сделал дальше
func TestRun_StopHookActiveNeverBlocksAgain(t *testing.T) {
	f := newFixture(t, "lead", "~")
	f.judge.verdict = Verdict{Verdict: VerdictBlock, Lishnie: []Finding{{Quote: "x", Why: "y"}}}
	event := f.event("Нашёл сам: в прошлой реплике $100. Поменять ещё что-то?")
	event.StopHookActive = true

	result := f.checker.Run(context.Background(), event)

	if result.Block || f.judge.calls != 0 {
		t.Fatalf("result = %+v, calls = %d", result, f.judge.calls)
	}
	entries := f.journal(t)
	if len(entries) != 1 || entries[0].Verdict != VerdictAfterBlock || !strings.Contains(entries[0].Message, "Нашёл сам") || entries[0].MessageID != "m1" {
		t.Fatalf("journal = %+v", entries)
	}
}

func TestRun_JudgeFailurePasses(t *testing.T) {
	f := newFixture(t, "lead", "~")
	f.judge.err = errors.New("codex failed: exit status 1")

	result := f.checker.Run(context.Background(), f.event(questionMessage))

	if result.Block {
		t.Fatalf("judge failure blocked: %+v", result)
	}
	entries := f.journal(t)
	if len(entries) != 1 || entries[0].Verdict != VerdictError || !strings.Contains(entries[0].Reason, "exit status 1") {
		t.Fatalf("journal = %+v", entries)
	}
	if len(f.warnings) == 0 {
		t.Fatal("judge failure not logged")
	}
}

func TestRun_JudgeTimeoutPasses(t *testing.T) {
	f := newFixture(t, "lead", "~")
	f.judge.hang = true
	f.checker.Budget = 20 * time.Millisecond

	result := f.checker.Run(context.Background(), f.event(questionMessage))

	if result.Block {
		t.Fatalf("judge timeout blocked: %+v", result)
	}
	if entries := f.journal(t); len(entries) != 1 || entries[0].Verdict != VerdictTimeout {
		t.Fatalf("journal = %+v", entries)
	}
}

// Сбой журналов не срывает проверку: судья работает по тексту и реплике
func TestRun_MissingJournalsStillJudged(t *testing.T) {
	f := newFixture(t, "lead", "~")
	f.checker.Journals.RepliesPath = filepath.Join(t.TempDir(), "missing.md")
	f.checker.Run(context.Background(), f.event(questionMessage))
	if f.judge.calls != 1 || len(f.warnings) == 0 {
		t.Fatalf("calls = %d, warnings = %v", f.judge.calls, f.warnings)
	}
}

func TestParseVerdict(t *testing.T) {
	verdict, err := ParseVerdict("```json\n{\"verdict\":\"block\",\"lishnie\":[{\"quote\":\"Удалить?\",\"why\":\"ответил 20.10\",\"where\":\"user-replies.md\"}]}\n```")
	if err != nil || !verdict.Block() || verdict.Lishnie[0].Quote != "Удалить?" {
		t.Fatalf("verdict = %+v, err = %v", verdict, err)
	}
	if verdict, err := ParseVerdict(`{"verdict":"pass","lishnie":[]}`); err != nil || verdict.Block() {
		t.Fatalf("verdict = %+v, err = %v", verdict, err)
	}
	for _, bad := range []string{
		`{"verdict":"block","lishnie":[]}`,
		`{"verdict":"maybe"}`,
		`не знаю`,
		`{"verdict":`,
	} {
		if _, err := ParseVerdict(bad); err == nil {
			t.Fatalf("ParseVerdict(%q) accepted", bad)
		}
	}
}

func TestJournalsFindBefore(t *testing.T) {
	dir := t.TempDir()
	replies := filepath.Join(dir, "replies.md")
	writeFile(t, replies, strings.Join([]string{
		"# Реплики",
		"### 2026-10-19T09:50:00.000Z (19.10 11:50 CEST) · ~/work · claude a",
		"Доступы к бакету S3 выдаёт Сергей",
		"### 2026-10-21T09:50:00.000Z (21.10 11:50 CEST) · ~/work · claude b",
		"Бакет S3 для Atlas уже выдан",
	}, "\n"))
	decisions := filepath.Join(dir, "decisions.md")
	writeFile(t, decisions, "# Журнал\n\n## Записи\n\n## infra-owner\n- Решение: «Доступы - это Сергей» (21.10 10:00, ~/work)\n- Действует: бакет S3 и доступы\n")
	journals := Journals{RepliesPath: replies, DecisionsPath: decisions}
	questions := Questions("У кого просить бакет S3 для Atlas?")

	before := time.Date(2026, 10, 20, 8, 58, 0, 0, time.UTC)
	excerpts, err := journals.Find(questions, before)
	if err != nil {
		t.Fatal(err)
	}
	if len(excerpts.Replies) != 1 || !strings.Contains(excerpts.Replies[0], "выдаёт Сергей") || len(excerpts.Decisions) != 0 {
		t.Fatalf("excerpts before 20.10 = %+v", excerpts)
	}

	excerpts, err = journals.Find(questions, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(excerpts.Replies) != 2 || len(excerpts.Decisions) != 1 {
		t.Fatalf("excerpts = %+v", excerpts)
	}

	// Решение 21.10 10:00 не видно в 09:00 того же дня (стенд 27.10: «Вносим?»
	// в 18:05 снят по решению 18:54), видно после 10:00
	for _, tt := range []struct {
		hour, decisions int
	}{{9, 0}, {11, 1}} {
		at := time.Date(2026, 10, 21, tt.hour, 0, 0, 0, time.Local)
		excerpts, err := journals.Find(questions, at)
		if err != nil {
			t.Fatal(err)
		}
		if len(excerpts.Decisions) != tt.decisions {
			t.Errorf("21.10 %d:00: decisions = %d, want %d", tt.hour, len(excerpts.Decisions), tt.decisions)
		}
	}
	// Без времени решение того же дня не видно: когда оно принято, неизвестно
	writeFile(t, decisions, "# Журнал\n\n## Записи\n\n## infra-owner\n- Решение: «Доступы - это Сергей» (21.10, ~/work)\n- Действует: бакет S3 и доступы\n")
	excerpts, err = journals.Find(questions, time.Date(2026, 10, 21, 23, 0, 0, 0, time.Local))
	if err != nil {
		t.Fatal(err)
	}
	if len(excerpts.Decisions) != 0 {
		t.Errorf("решение без времени в тот же день видно: %+v", excerpts.Decisions)
	}
	// Запись дописана позже: «Да, давай 3» 11:12 в записи от 10:48 не видно в 10:49
	writeFile(t, decisions, "# Журнал\n\n## Записи\n\n## infra-owner\n- Решение: «Доступы - это Сергей» (21.10 10:48 CET, ~/work); на бакет S3: «Да, давай 3» (21.10 ~11:12 CET)\n- Действует: бакет S3 и доступы, 12.50 не дата\n")
	for _, tt := range []struct {
		minute, decisions int
	}{{49, 0}, {13 + 60, 1}} {
		at := time.Date(2026, 10, 21, 10, tt.minute, 0, 0, time.Local)
		excerpts, err := journals.Find(questions, at)
		if err != nil {
			t.Fatal(err)
		}
		if len(excerpts.Decisions) != tt.decisions {
			t.Errorf("%s: decisions = %d, want %d", at.Format("15:04"), len(excerpts.Decisions), tt.decisions)
		}
	}
}

func TestReadTailFindsHumanBeyondFirstWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	var b strings.Builder
	b.WriteString(`{"type":"user","uuid":"u1","timestamp":"2026-10-23T10:00:00Z","origin":{"kind":"human"},"message":{"content":[{"type":"text","text":"далёкая реплика"}]}}` + "\n")
	filler := `{"type":"api-request-blob","data":"` + strings.Repeat("x", 4096) + `"}` + "\n"
	for b.Len() < 3*tailWindowStart {
		b.WriteString(filler)
	}
	b.WriteString(`{"type":"assistant","uuid":"m9","timestamp":"2026-10-23T10:00:00Z","message":{"content":[{"type":"text","text":"Делать?"}]}}` + "\n")
	writeFile(t, path, b.String())

	tail, err := ReadTail(path, "Делать?")
	if err != nil {
		t.Fatal(err)
	}
	if !tail.HumanFound || tail.HumanReply != "далёкая реплика" || tail.AssistantUUID != "m9" {
		t.Fatalf("tail = %+v", tail)
	}
}

// Случай lead 25.10 19:01: рекомендация («Я бы взял dream») — в прошлом ответе,
// после него уведомление фоновой задачи, а проверяемого ответа в транскрипте
// к Stop-хуку ещё нет. Промежуточный текст хода (tool_use) — не ответ
func TestScanTailPrevAnswer(t *testing.T) {
	const (
		human   = `{"type":"user","uuid":"u1","timestamp":"2026-10-23T10:00:00Z","origin":{"kind":"human"},"message":{"content":"Кстати sleep vs dream - как таки лучше?"}}`
		interim = `{"type":"assistant","uuid":"m0","timestamp":"2026-10-23T10:00:00Z","message":{"stop_reason":"tool_use","content":[{"type":"text","text":"Сейчас проверю пересечения"}]}}`
		think   = `{"type":"assistant","uuid":"m1","timestamp":"2026-10-23T10:00:00Z","message":{"stop_reason":"end_turn","content":[{"type":"thinking","thinking":"…"}]}}`
		prev    = `{"type":"assistant","uuid":"m2","timestamp":"2026-10-23T10:00:00Z","message":{"stop_reason":"end_turn","content":[{"type":"text","text":"Я бы взял **dream**. Переименовываю в dream?"}]}}`
		notice  = `{"type":"user","uuid":"n1","timestamp":"2026-10-23T10:00:00Z","origin":{"kind":"task-notification"},"message":{"content":"<task-notification>…"}}`
		current = "Про переименование в dream жду твоего ответа."
	)
	currentRecord := `{"type":"assistant","uuid":"m3","timestamp":"2026-10-23T10:00:00Z","message":{"stop_reason":"end_turn","content":[{"type":"text","text":"` + current + `"}]}}`

	tests := []struct {
		name               string
		lines              []string
		prevAnswer, uuid   string
		humanFound, prevOK bool
		prevAfterHuman     bool
	}{
		{name: "ответа ещё нет в транскрипте", lines: []string{human, interim, think, prev, notice},
			prevAnswer: "Я бы взял **dream**. Переименовываю в dream?", uuid: "m2", humanFound: true, prevOK: true, prevAfterHuman: true},
		{name: "ответ успел попасть в транскрипт", lines: []string{human, prev, notice, currentRecord},
			prevAnswer: "Я бы взял **dream**. Переименовываю в dream?", uuid: "m3", humanFound: true, prevOK: true, prevAfterHuman: true},
		{name: "предыдущий ответ до реплики человека", lines: []string{prev, human, interim},
			prevAnswer: "Я бы взял **dream**. Переименовываю в dream?", uuid: "m0", humanFound: true, prevOK: true},
		{name: "кончившегося хода нет", lines: []string{human, interim}, uuid: "m0", humanFound: true},
		{name: "реплики нет в окне", lines: []string{interim, prev},
			prevAnswer: "Я бы взял **dream**. Переименовываю в dream?", uuid: "m2", prevOK: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tail := scanTail([]byte(strings.Join(tt.lines, "\n")), current)
			if tail.PrevAnswer != tt.prevAnswer || tail.PrevFound != tt.prevOK || tail.AssistantUUID != tt.uuid || tail.HumanFound != tt.humanFound || tail.PrevAfterHuman != tt.prevAfterHuman {
				t.Fatalf("tail = %+v", tail)
			}
		})
	}
}

// Реплика с картинкой, набранная посреди хода, приходит в очередь с prompt
// списком частей, а не строкой. Прежний разбор ждал строку, терял такую
// реплику и брал более раннюю (сессия chooks 26.10, «[Image #1] - не забудьте
// везде заменить в проекте»)
func TestScanTailQueuedReplyWithImage(t *testing.T) {
	lines := []string{
		`{"type":"user","uuid":"u1","timestamp":"2026-10-23T10:00:00Z","origin":{"kind":"human"},"message":{"content":"ранняя реплика"}}`,
		`{"type":"attachment","uuid":"a1","timestamp":"2026-10-23T10:01:00Z","attachment":{"type":"queued_command","prompt":[{"type":"text","text":"[Image #1] - визуальный баг?"},{"type":"image","source":{"type":"base64","data":"AA=="}}],"origin":{"kind":"human"}}}`,
		`{"type":"assistant","uuid":"m1","timestamp":"2026-10-23T10:02:00Z","message":{"stop_reason":"tool_use","content":[{"type":"text","text":"Смотрю"}]}}`,
	}
	tail := scanTail([]byte(strings.Join(lines, "\n")), "ответ")
	if !tail.HumanFound || tail.HumanReply != "[Image #1] - визуальный баг?" || tail.AssistantUUID != "m1" {
		t.Fatalf("tail = %+v", tail)
	}
}

func TestCaseInputShowsPrevAnswer(t *testing.T) {
	input := Case{HumanReply: "sleep vs dream?", PrevAnswer: "Я бы взял dream. Переименовываю?", Answer: "Жду твоего ответа?"}.Input()
	prev := strings.Index(input, "[Предыдущий ответ агента]\n(написан до прошлой реплики пользователя)\nЯ бы взял dream.")
	final := strings.Index(input, "[Финальный ответ агента]")
	if prev < 0 || final < prev {
		t.Fatalf("предыдущий ответ должен идти перед финальным:\n%s", input)
	}
	if empty := (Case{Answer: "Делать?"}).Input(); !strings.Contains(empty, "[Предыдущий ответ агента]\n(не найден)") {
		t.Errorf("без предыдущего ответа — пометка «не найден»:\n%s", empty)
	}
	if !strings.Contains(JudgePrompt, "предыдущего ответа агента") {
		t.Error("промпт судьи должен говорить о вопросах со ссылкой на предыдущий ответ")
	}
}

// «"Делаем так?" - да» в 23:17 отвечало на другое предложение, а судья счёл
// его ответом на вопрос предыдущего ответа 23:30 (lead 27.10 23:31): судья
// должен видеть, что предыдущий ответ написан после реплики
func TestCaseInputShowsPrevAnswerAfterReply(t *testing.T) {
	input := Case{HumanReply: `"Делаем так?" - да`, PrevAnswer: "Сабтаском сборной задачи. Делаем так?", PrevAfterReply: true, Answer: "Ждёт вашего ответа: делаем так?"}.Input()
	if !strings.Contains(input, "[Предыдущий ответ агента]\n(написан после прошлой реплики пользователя: её «да» не про вопросы этого ответа)\nСабтаском") {
		t.Errorf("судья не видит порядок:\n%s", input)
	}
	noReply := Case{PrevAnswer: "Делаем так?", PrevAfterReply: true, Answer: "Делаем так?"}.Input()
	if strings.Contains(noReply, "(написан") {
		t.Errorf("без реплики порядок не пишется:\n%s", noReply)
	}
	if !strings.Contains(JudgePrompt, "ответ только на то, что было написано до неё") {
		t.Error("промпт судьи должен говорить о порядке реплики и ответа")
	}

}

// Судья видит предыдущий ответ, журнал помечает версию судьи
func TestRun_JudgeSeesPrevAnswer(t *testing.T) {
	f := newFixture(t, "lead", "~")
	f.checker.Run(context.Background(), f.event("Жду твоего ответа: делать?"))

	if !strings.Contains(f.judge.input, "Посчитал. Поменять на $200?") {
		t.Errorf("судья не видел предыдущий ответ:\n%s", f.judge.input)
	}
	entries := f.journal(t)
	if len(entries) != 1 || entries[0].JudgeRev != JudgeRev || !entries[0].PrevFound {
		t.Fatalf("журнал = %+v", entries)
	}
}

// Стенд восстанавливает по транскрипту ответ, реплику и предыдущий ответ на
// момент записи журнала; ход, кончившийся позже, не берётся
func TestReplayTail(t *testing.T) {
	lines := []string{
		`{"type":"user","uuid":"u1","timestamp":"2026-10-27T22:17:10Z","origin":{"kind":"human"},"message":{"content":"\"Делаем так?\" - да"}}`,
		`{"type":"assistant","uuid":"m1","timestamp":"2026-10-27T22:30:24Z","message":{"stop_reason":"end_turn","content":[{"type":"text","text":"Сабтаском сборной задачи. Делаем так?"}]}}`,
		`{"type":"assistant","uuid":"m2","timestamp":"2026-10-27T22:31:50Z","message":{"stop_reason":"end_turn","content":[{"type":"thinking","thinking":"…"}]}}`,
		`{"type":"assistant","uuid":"m3","timestamp":"2026-10-27T22:31:55Z","message":{"stop_reason":"end_turn","content":[{"type":"text","text":"Ждёт вашего ответа: делаем так?"}]}}`,
		`{"type":"assistant","uuid":"m4","timestamp":"2026-10-27T22:40:00Z","message":{"stop_reason":"end_turn","content":[{"type":"text","text":"Позже"}]}}`,
	}
	path := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tail, answer, err := ReplayTail(path, time.Date(2026, 10, 27, 22, 32, 4, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if answer != "Ждёт вашего ответа: делаем так?" || tail.PrevAnswer != "Сабтаском сборной задачи. Делаем так?" || !tail.PrevAfterHuman || tail.HumanReply != `"Делаем так?" - да` {
		t.Fatalf("answer = %q, tail = %+v", answer, tail)
	}
	if _, _, err := ReplayTail(path, time.Date(2026, 10, 27, 22, 0, 0, 0, time.UTC)); err == nil {
		t.Error("до первого хода — ошибка, а не пустой ответ")
	}
}
