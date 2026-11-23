package claimcheck

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aiseeq/chooks/internal/questioncheck"
)

// stubJudge — судья-заглушка: ответ по учётке, задержка, счёт вызовов и
// одновременных вызовов
type stubJudge struct {
	answer   func(account string) (string, error)
	delay    time.Duration
	calls    atomic.Int64
	running  atomic.Int64
	maxSeen  atomic.Int64
	mu       sync.Mutex
	accounts []string
}

func (s *stubJudge) Model() string { return "stub/high" }

func (s *stubJudge) Ask(ctx context.Context, account, _ string) (string, questioncheck.Usage, error) {
	s.calls.Add(1)
	s.mu.Lock()
	s.accounts = append(s.accounts, account)
	s.mu.Unlock()
	n := s.running.Add(1)
	defer s.running.Add(-1)
	for {
		seen := s.maxSeen.Load()
		if n <= seen || s.maxSeen.CompareAndSwap(seen, n) {
			break
		}
	}
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return "", questioncheck.Usage{}, ctx.Err()
		}
	}
	answer, err := s.answer(account)
	return answer, questioncheck.Usage{InputTokens: 10, OutputTokens: 2}, err
}

func answerOf(claims ...Claim) func(string) (string, error) {
	data, _ := json.Marshal(map[string]any{"claims": claims})
	return func(string) (string, error) { return string(data), nil }
}

var (
	flagClaim  = Claim{Text: "тест прошёл", Verdict: VerdictUnchecked, Basis: "нет", CouldCheck: "go test"}
	okClaim    = Claim{Text: "файл есть", Verdict: VerdictChecked, Basis: "Read 10:00"}
	bogusClaim = Claim{Text: "не факт", Verdict: "нет утверждения о факте", Basis: "нет"}
)

func testHook(t *testing.T, judge Asker) Hook {
	t.Helper()
	root := t.TempDir()
	return Hook{Judge: judge, StateDir: filepath.Join(root, "state"), SlotDir: filepath.Join(root, "run", "claimcheck"),
		Budget: 10 * time.Second, SlotPoll: 10 * time.Millisecond, Warn: func(string, ...any) {}}
}

const lastAnswer = "Готово: тест прошёл, файл есть."

func stopTranscript(t *testing.T) string {
	return fixture(t,
		ev("user", "2026-01-01T00:00:00Z", "сделай"),
		ev("assistant", "2026-01-01T00:00:01Z", []any{map[string]any{"type": "tool_use", "id": "c1", "name": "Bash", "input": map[string]any{"command": "go test"}}}),
		map[string]any{"type": "user", "timestamp": "2026-01-01T00:00:02Z", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "c1", "content": "ok"}}}},
		ev("assistant", "2026-01-01T00:00:03Z", []any{map[string]any{"type": "text", "text": lastAnswer}}),
	)
}

func stopEvent(t *testing.T) StopEvent {
	return StopEvent{SessionID: "s1", TranscriptPath: stopTranscript(t), LastMessage: lastAnswer}
}

func journal(t *testing.T, h Hook) []Entry {
	t.Helper()
	f, err := os.Open(JournalPath(h.StateDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []Entry
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var e Entry
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

func outcomes(entries []Entry) []string {
	var out []string
	for _, e := range entries {
		out = append(out, e.Outcome)
	}
	return out
}

func TestStopFlagBlocksWithReason(t *testing.T) {
	judge := &stubJudge{answer: answerOf(okClaim, flagClaim)}
	h := testHook(t, judge)
	got := h.Stop(context.Background(), stopEvent(t))
	if !got.Block || got.Result != OutcomeBlock {
		t.Fatalf("outcome %+v", got)
	}
	for _, want := range []string{"«тест прошёл» — unchecked: нет", "go test",
		"Проверь каждое утверждение или коротко ответь, чем оно уже проверено (вызов и его время). Чего проверить нечем — пометь как непроверенное."} {
		if !strings.Contains(got.Reason, want) {
			t.Errorf("reason lacks %q:\n%s", want, got.Reason)
		}
	}
	if strings.Contains(got.Reason, "файл есть") {
		t.Errorf("checked claim in reason:\n%s", got.Reason)
	}
	entries := journal(t, h)
	if len(entries) != 1 || entries[0].Outcome != OutcomeBlock || entries[0].Claims != 2 || entries[0].Flags != 1 || entries[0].Usage == nil {
		t.Fatalf("journal %+v", entries)
	}
}

func TestStopWithoutFlagsPasses(t *testing.T) {
	h := testHook(t, &stubJudge{answer: answerOf(okClaim, Claim{Text: "x", Verdict: VerdictUncheckedMarked})})
	if got := h.Stop(context.Background(), stopEvent(t)); got.Block || got.Result != OutcomePass {
		t.Fatalf("outcome %+v", got)
	}
	if got := outcomes(journal(t, h)); !slices.Equal(got, []string{OutcomePass}) {
		t.Fatalf("journal %v", got)
	}
}

// Второй ответ после блока судье не идёт; факт — в журнале
func TestStopHookActiveSkipsJudge(t *testing.T) {
	judge := &stubJudge{answer: answerOf(flagClaim)}
	h := testHook(t, judge)
	event := stopEvent(t)
	event.StopHookActive = true
	if got := h.Stop(context.Background(), event); got.Block || got.Result != OutcomeSecondAnswer {
		t.Fatalf("outcome %+v", got)
	}
	if judge.calls.Load() != 0 {
		t.Fatalf("judge called %d times on second answer", judge.calls.Load())
	}
	entries := journal(t, h)
	if len(entries) != 1 || entries[0].Outcome != OutcomeSecondAnswer || entries[0].Answer != lastAnswer {
		t.Fatalf("journal %+v", entries)
	}
}

func TestStopJudgeFailurePassesAndLogs(t *testing.T) {
	h := testHook(t, &stubJudge{answer: func(string) (string, error) { return "", errors.New("codex failed: boom") }})
	if got := h.Stop(context.Background(), stopEvent(t)); got.Block || got.Result != OutcomeError {
		t.Fatalf("outcome %+v", got)
	}
	entries := journal(t, h)
	if len(entries) != 1 || entries[0].Outcome != OutcomeError || entries[0].Reason != FailJudge || !strings.Contains(entries[0].Error, "boom") {
		t.Fatalf("journal %+v", entries)
	}
}

func TestStopJudgeTimeoutPasses(t *testing.T) {
	judge := &stubJudge{answer: answerOf(flagClaim), delay: time.Second}
	h := testHook(t, judge)
	h.Budget = 100 * time.Millisecond
	if got := h.Stop(context.Background(), stopEvent(t)); got.Block {
		t.Fatalf("outcome %+v", got)
	}
	if e := journal(t, h); len(e) != 1 || e[0].Reason != FailTimeout {
		t.Fatalf("journal %+v", e)
	}
}

// Неизвестный вердикт среди верных: верные флаги блокируют, неизвестный
// пункт — отдельной строкой unknown_verdict
func TestStopUnknownVerdictKeepsKnownFlags(t *testing.T) {
	h := testHook(t, &stubJudge{answer: answerOf(bogusClaim, flagClaim)})
	got := h.Stop(context.Background(), stopEvent(t))
	if !got.Block || !strings.Contains(got.Reason, "тест прошёл") {
		t.Fatalf("outcome %+v", got)
	}
	entries := journal(t, h)
	if got := outcomes(entries); !slices.Equal(got, []string{OutcomeUnknownVerdict, OutcomeBlock}) {
		t.Fatalf("journal %v", got)
	}
	if len(entries[0].UnknownClaims) != 1 || entries[0].UnknownClaims[0].Index != 0 || entries[1].Unknown != 1 || entries[1].Flags != 1 {
		t.Fatalf("journal %+v", entries)
	}
}

func TestStopOnlyUnknownVerdictPasses(t *testing.T) {
	h := testHook(t, &stubJudge{answer: answerOf(bogusClaim)})
	if got := h.Stop(context.Background(), stopEvent(t)); got.Block || got.Result != OutcomePass {
		t.Fatalf("outcome %+v", got)
	}
	if got := outcomes(journal(t, h)); !slices.Equal(got, []string{OutcomeUnknownVerdict, OutcomePass}) {
		t.Fatalf("journal %v", got)
	}
}

// Транскрипт ещё без последнего ответа — сбой входа, ход проходит
func TestStopTranscriptBehindPasses(t *testing.T) {
	judge := &stubJudge{answer: answerOf(flagClaim)}
	h := testHook(t, judge)
	event := stopEvent(t)
	event.LastMessage = "другой ответ"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if got := h.Stop(ctx, event); got.Block || got.Result != OutcomeError || judge.calls.Load() != 0 {
		t.Fatalf("outcome %+v calls %d", got, judge.calls.Load())
	}
}

// Отказ первой учётки по кредитам: вызов на второй и метка на час; по
// истечении метки — снова первая. 429 учётку не метит
func TestAccountsFailover(t *testing.T) {
	now := time.Date(2026, 11, 23, 1, 0, 0, 0, time.UTC)
	out := true
	judge := &stubJudge{}
	ok := answerOf(okClaim)
	judge.answer = func(account string) (string, error) {
		if account == "/acc/work" && out {
			return "", errors.New("codex turn failed: Your workspace is out of credits.")
		}
		return ok(account)
	}
	h := testHook(t, judge)
	h.Accounts = []string{"/acc/work", "/acc/personal"}
	h.Now = func() time.Time { return now }
	if got := h.Stop(context.Background(), stopEvent(t)); got.Result != OutcomePass {
		t.Fatalf("outcome %+v", got)
	}
	if !slices.Equal(judge.accounts, []string{"/acc/work", "/acc/personal"}) {
		t.Fatalf("accounts %v", judge.accounts)
	}
	if e := journal(t, h); e[len(e)-1].Account != "/acc/personal" {
		t.Fatalf("journal %+v", e)
	}
	judge.accounts = nil
	now = now.Add(59 * time.Minute)
	h.Stop(context.Background(), stopEvent(t))
	if !slices.Equal(judge.accounts, []string{"/acc/personal"}) {
		t.Fatalf("marked account called within the hour: %v", judge.accounts)
	}
	judge.accounts, out = nil, false
	now = now.Add(2 * time.Minute)
	h.Stop(context.Background(), stopEvent(t))
	if !slices.Equal(judge.accounts, []string{"/acc/work"}) {
		t.Fatalf("after the mark: %v", judge.accounts)
	}
	// Темп запросов — не кредиты: учётка не метится, ошибка — сбой судьи
	judge.accounts = nil
	judge.answer = func(string) (string, error) { return "", errors.New("429 rate limit: usage limit per minute") }
	if got := h.Stop(context.Background(), stopEvent(t)); got.Result != OutcomeError {
		t.Fatalf("outcome %+v", got)
	}
	if !slices.Equal(judge.accounts, []string{"/acc/work"}) {
		t.Fatalf("rate limit moved to next account: %v", judge.accounts)
	}
	if _, marked, _ := h.accountOutUntil("/acc/work", now); marked {
		t.Fatal("rate limit marked the account")
	}
}

// Метка worker с until в будущем — вызов сразу на вторую учётку; битая или
// прошедшая метка — не отказ учётке
func TestWorkerMarkSkipsAccount(t *testing.T) {
	now := time.Date(2026, 11, 23, 1, 0, 0, 0, time.UTC)
	judge := &stubJudge{answer: answerOf(okClaim)}
	h := testHook(t, judge)
	h.Accounts = []string{"/acc/work", "/acc/personal"}
	h.Now = func() time.Time { return now }
	mark := filepath.Join(t.TempDir(), "work.json")
	h.WorkerMarks = map[string]string{"/acc/work": mark}
	write := func(text string) {
		if err := os.WriteFile(mark, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"until":"2026-11-23T18:45:41+01:00","why":"Your workspace is out of credits."}`)
	h.Stop(context.Background(), stopEvent(t))
	if !slices.Equal(judge.accounts, []string{"/acc/personal"}) {
		t.Fatalf("worker mark ignored: %v", judge.accounts)
	}
	for _, text := range []string{`{"until":"2026-11-22T18:45:41+01:00"}`, `not json`} {
		judge.accounts = nil
		write(text)
		h.Stop(context.Background(), stopEvent(t))
		if !slices.Equal(judge.accounts, []string{"/acc/work"}) {
			t.Fatalf("mark %q: %v", text, judge.accounts)
		}
	}
	if _, err := os.Stat(mark); err != nil {
		t.Fatal(err)
	}
}

func TestAllAccountsOutIsLimitFailure(t *testing.T) {
	judge := &stubJudge{answer: func(string) (string, error) { return "", errors.New("You've hit your usage limit") }}
	h := testHook(t, judge)
	h.Accounts = []string{"/a", "/b"}
	h.Stop(context.Background(), stopEvent(t))
	if e := journal(t, h); len(e) != 1 || e[0].Reason != FailLimit {
		t.Fatalf("journal %+v", e)
	}
}

// 5 сбоев подряд — одно уведомление; 6-й в тот же час не шлёт; успех
// сбрасывает счётчик строкой «восстановлен»; через час снова 5 — снова одно
func TestConsecutiveFailuresNotifyOnce(t *testing.T) {
	now := time.Date(2026, 11, 23, 1, 0, 0, 0, time.UTC)
	fail := true
	judge := &stubJudge{}
	ok := answerOf(okClaim)
	judge.answer = func(a string) (string, error) {
		if fail {
			return "", errors.New("codex failed: boom")
		}
		return ok(a)
	}
	h := testHook(t, judge)
	h.Now = func() time.Time { return now }
	var notes []string
	h.Notify = func(title, message string) error { notes = append(notes, message); return nil }
	for i := 1; i <= 6; i++ {
		h.Stop(context.Background(), stopEvent(t))
		now = now.Add(time.Minute)
		want := 0
		if i >= 5 {
			want = 1
		}
		if len(notes) != want {
			t.Fatalf("after %d failures: %d alerts %v", i, len(notes), notes)
		}
	}
	if notes[0] != "claim-check: судья недоступен 5 раз подряд (judge)" {
		t.Fatalf("alert %q", notes[0])
	}
	fail = false
	h.Stop(context.Background(), stopEvent(t))
	e := journal(t, h)
	last := e[len(e)-2]
	if last.Outcome != OutcomeRecovered || last.Note != "восстановлен" || last.AfterFailures != 6 {
		t.Fatalf("recovery entry %+v", last)
	}
	var state health
	if err := readJSON(h.healthPath(), &state); err != nil || state.Failures != 0 {
		t.Fatalf("health %+v %v", state, err)
	}
	fail = true
	for range 5 {
		h.Stop(context.Background(), stopEvent(t))
	}
	if len(notes) != 1 {
		t.Fatalf("alert within the hour after reset: %v", notes)
	}
	now = now.Add(time.Hour)
	for range 5 {
		h.Stop(context.Background(), stopEvent(t))
	}
	if len(notes) != 2 {
		t.Fatalf("no alert an hour later: %v", notes)
	}
}

func percentileMS(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(p*float64(len(sorted)) + 0.5)
	return sorted[min(max(i-1, 0), len(sorted)-1)]
}

// 16 одновременных Stop с судьёй на 2 с: не больше 4 судей сразу; ожидание
// места — в журнале. Бюджет меньше очереди — queue_timeout
func TestStopSlotsLimitConcurrency(t *testing.T) {
	judge := &stubJudge{answer: answerOf(okClaim), delay: 2 * time.Second}
	h := testHook(t, judge)
	h.Budget = 30 * time.Second
	run := func(h Hook) {
		var wg sync.WaitGroup
		for range 16 {
			wg.Go(func() { h.Stop(context.Background(), stopEvent(t)) })
		}
		wg.Wait()
	}
	run(h)
	if got := judge.maxSeen.Load(); got > DefaultSlots || got < 2 {
		t.Fatalf("max concurrent judges %d, want 2..%d", got, DefaultSlots)
	}
	var waits []int64
	for _, e := range journal(t, h) {
		if e.Outcome != OutcomePass {
			t.Fatalf("entry %+v", e)
		}
		waits = append(waits, e.SlotWaitMS)
	}
	slices.Sort(waits)
	if len(waits) != 16 || waits[len(waits)-1] < 4000 {
		t.Fatalf("waits %v: 16 judges on 4 slots wait at least 3 rounds", waits)
	}
	t.Logf("slot wait ms: p50 %d, p95 %d, max %d (all %v)", percentileMS(waits, 0.5), percentileMS(waits, 0.95), waits[len(waits)-1], waits)

	judge2 := &stubJudge{answer: answerOf(okClaim), delay: 2 * time.Second}
	h2 := testHook(t, judge2)
	h2.Budget = 3 * time.Second
	run(h2)
	counts := map[string]int{}
	for _, e := range journal(t, h2) {
		counts[e.Outcome]++
	}
	if counts[OutcomeQueueTimeout] < 4 || counts[OutcomePass] < 4 || judge2.maxSeen.Load() > DefaultSlots {
		t.Fatalf("outcomes %v, max %d", counts, judge2.maxSeen.Load())
	}
	t.Logf("budget 3 s: outcomes %v", counts)
}

func sendTranscript(t *testing.T, letters ...string) string {
	rows := []any{ev("user", "2026-01-01T00:00:00Z", "напиши мастеру")}
	for i, text := range letters {
		rows = append(rows, ev("assistant", fmt.Sprintf("2026-01-01T00:00:%02dZ", i+1), []any{map[string]any{"type": "tool_use", "id": fmt.Sprintf("send%d", i),
			"name": "SendMessage", "input": map[string]any{"to": "git-admin", "message": text}}}))
	}
	return fixture(t, rows...)
}

// Тот же текст после отказа проходит без судьи; третий изменённый текст
// после двух отказов — retry_limit без судьи
func TestSendRetries(t *testing.T) {
	letters := []string{"тест прошёл", "тест прошёл, проверено", "тест прошёл в 10:00"}
	path := sendTranscript(t, letters...)
	judge := &stubJudge{answer: answerOf(flagClaim)}
	h := testHook(t, judge)
	send := func(i int, text string) Outcome {
		return h.Send(context.Background(), SendEvent{SessionID: "s1", TranscriptPath: path, ToolUseID: fmt.Sprintf("send%d", i), Text: text})
	}
	if got := send(0, letters[0]); !got.Block || !strings.Contains(got.Reason, "тот же текст без изменений пройдёт") {
		t.Fatalf("first %+v", got)
	}
	if got := send(0, letters[0]); got.Block || got.Result != OutcomeRetrySame {
		t.Fatalf("same %+v", got)
	}
	if got := send(1, letters[1]); !got.Block {
		t.Fatalf("second changed %+v", got)
	}
	if got := send(2, letters[2]); got.Block || got.Result != OutcomeRetryLimit {
		t.Fatalf("third changed %+v", got)
	}
	if judge.calls.Load() != 2 {
		t.Fatalf("judge calls %d, want 2", judge.calls.Load())
	}
	if got := outcomes(journal(t, h)); !slices.Equal(got, []string{OutcomeDeny, OutcomeRetrySame, OutcomeDeny, OutcomeRetryLimit}) {
		t.Fatalf("journal %v", got)
	}
	// Новый ход сессии — отказы прошлого не считаются
	next := fixture(t, ev("user", "2026-01-01T00:01:00Z", "ещё"),
		ev("assistant", "2026-01-01T00:01:01Z", []any{map[string]any{"type": "tool_use", "id": "n0", "name": "SendMessage", "input": map[string]any{"to": "x", "message": letters[2]}}}))
	if got := h.Send(context.Background(), SendEvent{SessionID: "s1", TranscriptPath: next, ToolUseID: "n0", Text: letters[2]}); !got.Block {
		t.Fatalf("new turn %+v", got)
	}
}

func TestSendInputIsLetterCall(t *testing.T) {
	path := sendTranscript(t, "первое письмо 1", "второе письмо 2")
	input, err := Hook{}.sendInput(context.Background(), SendEvent{TranscriptPath: path, ToolUseID: "send1", Text: "второе письмо 2"})
	if err != nil || input.Event != EventSend || input.Answer != "второе письмо 2" {
		t.Fatalf("input %+v err %v", input, err)
	}
	input, err = Hook{}.sendInput(context.Background(), SendEvent{TranscriptPath: path, Text: "первое письмо 1"})
	if err != nil || input.Answer != "первое письмо 1" {
		t.Fatalf("by text: %+v err %v", input, err)
	}
}

// panicJudge паникует первым вызовом, дальше отвечает
type panicJudge struct {
	stubJudge
	panicked atomic.Bool
}

func (p *panicJudge) Ask(ctx context.Context, account, input string) (string, questioncheck.Usage, error) {
	if p.panicked.CompareAndSwap(false, true) {
		panic("judge panic")
	}
	return p.stubJudge.Ask(ctx, account, input)
}

// Восстановленная паника судьи не держит место: следующий вызов в том же
// процессе (демон) берёт его без queue_timeout (ревью Terra 5ec62fa, п. 2)
func TestSlotReleasedAfterPanic(t *testing.T) {
	judge := &panicJudge{stubJudge: stubJudge{answer: answerOf(okClaim)}}
	h := testHook(t, judge)
	h.Slots, h.Budget = 1, 500*time.Millisecond
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("judge did not panic")
			}
		}()
		h.Stop(context.Background(), stopEvent(t))
	}()
	if got := h.Stop(context.Background(), stopEvent(t)); got.Result != OutcomePass {
		t.Fatalf("after recovered panic: %+v", got)
	}
}

// Два одновременных одинаковых письма: один вызов судьи, второе —
// retry_same; изменённое письмо после них ещё судится (ревью Terra 5ec62fa, п. 3)
func TestSendParallelSameLetter(t *testing.T) {
	letter := "тест прошёл"
	path := sendTranscript(t, letter, letter, "тест прошёл, проверено")
	judge := &stubJudge{answer: answerOf(flagClaim), delay: 300 * time.Millisecond}
	h := testHook(t, judge)
	results := make([]Outcome, 2)
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Go(func() {
			results[i] = h.Send(context.Background(), SendEvent{SessionID: "s1", TranscriptPath: path, ToolUseID: fmt.Sprintf("send%d", i), Text: letter})
		})
	}
	wg.Wait()
	got := []string{results[0].Result, results[1].Result}
	slices.Sort(got)
	if !slices.Equal(got, []string{OutcomeDeny, OutcomeRetrySame}) || judge.calls.Load() != 1 {
		t.Fatalf("results %v, judge calls %d", got, judge.calls.Load())
	}
	if out := h.Send(context.Background(), SendEvent{SessionID: "s1", TranscriptPath: path, ToolUseID: "send2", Text: "тест прошёл, проверено"}); out.Result != OutcomeDeny {
		t.Fatalf("changed letter after the pair: %+v", out)
	}
}
