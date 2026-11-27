package claimcheck

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Мера тикета claim-check-hook (разбор 23.11): Stop git-admin упёрся в
// SIGTERM 120 с при бюджете 100 с — вход судьи на длинном ходе строился
// дольше бюджета, а бюджет его не прерывал; письмо не нашло свою строку
// tool_use за 3 с, хотя она была в транскрипте

// longTurnTranscript — ход из calls вызовов Bash, у каждого вывод в lines
// строк со словами ответа, и длинный ответ из answerWords слов
func longTurnTranscript(t *testing.T, calls, lines, words int) (string, int) {
	t.Helper()
	vocab := make([]string, words)
	for i := range vocab {
		vocab[i] = fmt.Sprintf("слово%dзнач", i)
	}
	answer := strings.Join(vocab, " ")
	rows := []any{ev("user", "2026-01-01T00:00:00Z", "сделай и проверь")}
	for c := range calls {
		id := fmt.Sprintf("c%d", c)
		at := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC).Add(time.Duration(c) * time.Second).Format(time.RFC3339)
		var out []string
		for l := range lines {
			out = append(out, fmt.Sprintf("строка %d вывода %s и ещё %s", l, vocab[(c*lines+l)%words], vocab[(c*7+l*13)%words]))
		}
		rows = append(rows,
			ev("assistant", at, []any{map[string]any{"type": "tool_use", "id": id, "name": "Bash", "input": map[string]any{"command": "echo " + vocab[c%words]}}}),
			map[string]any{"type": "user", "timestamp": at, "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": id, "content": strings.Join(out, "\n")}}}})
	}
	rows = append(rows, ev("assistant", "2026-01-02T00:00:00Z", []any{map[string]any{"type": "text", "text": answer}}))
	return fixture(t, rows...), len(rows)
}

// Вход судьи на длинном ходе строится за секунды, а не за минуты: на
// настоящем транскрипте 93 МБ сборка шла дольше 70 с. Предел — по
// процессорному времени процесса, а не по часам: под нагрузкой гейта те же
// 4,6 с по часам становились 15 с, а прежний кубический отбор тратил на
// этом ходе больше 300 с процессора
func TestMeasureBuildInputLongTurn(t *testing.T) {
	path, line := longTurnTranscript(t, 1500, 60, 5000)
	cpuBefore := processCPU(t)
	started := time.Now()
	input, err := BuildInput(context.Background(), path, line, Options{})
	took := time.Since(started)
	cpu := processCPU(t) - cpuBefore
	if err != nil {
		t.Fatal(err)
	}
	if input == nil || input.Answer == "" {
		t.Fatalf("empty input: %+v", input)
	}
	t.Logf("BuildInput %d calls: cpu %v, wall %v", 1500, cpu, took)
	if cpu > 20*time.Second {
		t.Fatalf("BuildInput on a long turn used %v of CPU, want under 20s", cpu)
	}
}

func TestMeasureExcerptManyLongOutputs(t *testing.T) {
	path, line := longTurnTranscript(t, 40, 2000, 3000)
	cpuBefore := processCPU(t)
	started := time.Now()
	input, err := BuildInput(context.Background(), path, line, Options{})
	cpu := processCPU(t) - cpuBefore
	took := time.Since(started)
	if err != nil {
		t.Fatal(err)
	}
	if input == nil || input.Answer == "" {
		t.Fatalf("empty input: %+v", input)
	}
	t.Logf("BuildInput 40 calls × 2000 output lines, 3000 answer words: cpu %v, wall %v", cpu, took)
	if cpu > 10*time.Second {
		t.Fatalf("BuildInput on many long outputs used %v of CPU, want under 10s", cpu)
	}
}

func longWriteTranscript(t *testing.T, calls, lines, words int) (string, int) {
	t.Helper()
	vocab := make([]string, words)
	for i := range vocab {
		vocab[i] = fmt.Sprintf("слово%dзнач", i)
	}
	answer := strings.Join(vocab, " ")
	rows := []any{ev("user", "2026-01-01T00:00:00Z", "запиши документ и проверь")}
	const wordsPerLine = 42
	for c := range calls {
		id := fmt.Sprintf("write-%d", c)
		at := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC).Add(time.Duration(c) * time.Second).Format(time.RFC3339)
		var content strings.Builder
		for l := range lines {
			fmt.Fprintf(&content, "Строка %d:", l)
			for w := range wordsPerLine {
				fmt.Fprintf(&content, " %s", vocab[(c*lines*wordsPerLine+l*wordsPerLine+w)%words])
			}
			content.WriteByte('\n')
		}
		rows = append(rows,
			ev("assistant", at, []any{map[string]any{"type": "tool_use", "id": id, "name": "Write", "input": map[string]any{"file_path": fmt.Sprintf("/work/report-%d.md", c), "content": content.String()}}}),
			map[string]any{"type": "user", "timestamp": at, "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": id, "content": "file written"}}}},
		)
	}
	rows = append(rows, ev("assistant", "2026-01-02T00:00:00Z", []any{map[string]any{"type": "text", "text": answer}}))
	return fixture(t, rows...), len(rows)
}

// Длинные Write проверяют общий матчёр на строках больше лимита входа.
// Предел оставляет не менее пятикратный запас относительно 11 с CPU на машине.
func TestMeasureBuildInputManyLongWrites(t *testing.T) {
	for _, tc := range []struct {
		calls, words int
		cpuLimit     time.Duration
	}{
		{calls: 8, words: 1500, cpuLimit: 15 * time.Second},
	} {
		path, line := longWriteTranscript(t, tc.calls, 100, tc.words)
		cpuBefore := processCPU(t)
		started := time.Now()
		input, err := BuildInput(context.Background(), path, line, Options{})
		cpu := processCPU(t) - cpuBefore
		took := time.Since(started)
		if err != nil {
			t.Fatal(err)
		}
		if input == nil || input.Answer == "" {
			t.Fatalf("empty input: %+v", input)
		}
		writes := 0
		croppedWrites := 0
		for _, tool := range input.TurnTools {
			if tool.Name == "Write" {
				writes++
				if strings.Contains(tool.Input, "[обрезано:") {
					croppedWrites++
				}
			}
		}
		if writes == 0 {
			t.Fatal("BuildInput retained no Write calls")
		}
		if croppedWrites == 0 {
			t.Fatal("BuildInput retained no cropped long Write inputs")
		}
		t.Logf("BuildInput %d Write × 100 lines × ~600 chars, %d answer words (%d retained, %d cropped): cpu %v, wall %v", tc.calls, tc.words, writes, croppedWrites, cpu, took)
		if cpu > tc.cpuLimit {
			t.Fatalf("BuildInput on %d long Write inputs used %v of CPU, want under %v", tc.calls, cpu, tc.cpuLimit)
		}
	}
}

// processCPU — процессорное время процесса (user + system): тесты пакета
// идут без t.Parallel, так что разность — время одной сборки и её GC
func processCPU(t *testing.T) time.Duration {
	t.Helper()
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		t.Fatalf("getrusage: %v", err)
	}
	return time.Duration(usage.Utime.Nano() + usage.Stime.Nano())
}

// slowBuild — сборка входа, которая длится дольше бюджета и не смотрит на ctx
func slowBuild(delay time.Duration) func(context.Context, string, int, Options) (*Input, error) {
	return func(context.Context, string, int, Options) (*Input, error) {
		time.Sleep(delay)
		return &Input{Event: "stop", Answer: "ответ"}, nil
	}
}

// Stop укладывается в бюджет, даже когда сборка входа его не уважает: хук
// пропускает ход со сбоем входа, а не ждёт SIGTERM
func TestMeasureStopKeepsBudgetWhenInputIsSlow(t *testing.T) {
	path := fixture(t, ev("user", "2026-01-01T00:00:00Z", "вопрос"),
		ev("assistant", "2026-01-01T00:00:01Z", []any{map[string]any{"type": "text", "text": "ответ"}}))
	hook := testHook(t, &stubJudge{answer: answerOf(okClaim)})
	hook.Budget = 500 * time.Millisecond
	hook.Build = slowBuild(5 * time.Second)
	started := time.Now()
	outcome := hook.Stop(context.Background(), StopEvent{SessionID: "s", TranscriptPath: path, LastMessage: "ответ"})
	took := time.Since(started)
	if took > 1500*time.Millisecond {
		t.Fatalf("Stop took %v with budget 500ms", took)
	}
	if outcome.Block {
		t.Fatalf("slow input must let the stop through: %+v", outcome)
	}
	if got := lastEntry(t, hook); got.Outcome != OutcomeError || got.Error == "" {
		t.Fatalf("journal entry = %+v, want error outcome with reason", got)
	}
}

// Send — то же для письма
func TestMeasureSendKeepsBudgetWhenInputIsSlow(t *testing.T) {
	path := fixture(t, ev("user", "2026-01-01T00:00:00Z", "вопрос"),
		ev("assistant", "2026-01-01T00:00:01Z", []any{map[string]any{"type": "tool_use", "id": "t1", "name": "SendMessage", "input": map[string]any{"to": "x", "message": "письмо"}}}))
	hook := testHook(t, &stubJudge{answer: answerOf(okClaim)})
	hook.Budget = 500 * time.Millisecond
	hook.Build = slowBuild(5 * time.Second)
	started := time.Now()
	outcome := hook.Send(context.Background(), SendEvent{SessionID: "s", TranscriptPath: path, ToolUseID: "t1", Text: "письмо"})
	if took := time.Since(started); took > 1500*time.Millisecond {
		t.Fatalf("Send took %v with budget 500ms", took)
	}
	if outcome.Block {
		t.Fatalf("slow input must let the letter through: %+v", outcome)
	}
}

// Письмо находит свою строку tool_use, дописанную в транскрипт через 5 с
// после входа хука: 23.11 05:48 строка 48660 с меткой 04:48:10.143Z была в
// транскрипте, а хук сдался в 05:48:13
func TestMeasureSendFindsLateToolUseLine(t *testing.T) {
	path := fixture(t, ev("user", "2026-01-01T00:00:00Z", "вопрос"),
		ev("assistant", "2026-01-01T00:00:01Z", []any{map[string]any{"type": "text", "text": "думаю"}}))
	call := ev("assistant", "2026-01-01T00:00:02Z", []any{map[string]any{"type": "tool_use", "id": "late", "name": "SendMessage", "input": map[string]any{"to": "x", "message": "письмо"}}})
	done := make(chan error, 1)
	go func() {
		time.Sleep(5 * time.Second)
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			done <- err
			return
		}
		b, _ := json.Marshal(call)
		_, err = f.Write(append(b, '\n'))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		done <- err
	}()
	hook := testHook(t, &stubJudge{answer: answerOf(okClaim)})
	hook.Budget = 30 * time.Second
	outcome := hook.Send(context.Background(), SendEvent{SessionID: "s", TranscriptPath: path, ToolUseID: "late", Text: "письмо"})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if outcome.Result == OutcomeError {
		t.Fatalf("late tool_use line not found: %+v, journal %+v", outcome, lastEntry(t, hook))
	}
}

func lastEntry(t *testing.T, h Hook) Entry {
	t.Helper()
	entries := journal(t, h)
	if len(entries) == 0 {
		t.Fatal("journal is empty")
	}
	return entries[len(entries)-1]
}
