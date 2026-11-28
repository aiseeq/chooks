package claimcheck

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
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

// Пределы мерных тестов сборки входа (тикет claim-budget-test-flake).
// Работу меряют байты аллокаций (TotalAlloc): от нагрузки машины они не
// зависят, разброс базы между прогонами — 0,02 %. Предел — база × 1,15, так
// что удвоение любого этапа с долей байт больше 15 % его пробивает.
// Процессорное время процесса от нагрузки зависит: длинный ход тратит 5,9 с
// на тихой машине и 21 с в гейте при load 13. Поэтому CPU — только потолок
// по бюджету хука: Stop — 100 с, судья v7 отвечал до 36 с (27.11), на сборку
// остаётся около 60 с. Потолок 40 с ловит прежний кубический отбор (больше
// 300 с) и не срабатывает от нагрузки гейта. Замедление без роста байт ниже
// потолка тест не ловит: в бюджете хука оно не опасно. Сборка с -race
// меряет не боевой код (байты +21 %, CPU ×5,4), там числа идут только в
// журнал теста
const (
	allocMargin = 1.15
	cpuCeiling  = 40 * time.Second
)

// buildMeasured — BuildInput с замером байт аллокаций и процессорного
// времени процесса: тесты пакета идут без t.Parallel, соседей в замере нет
func buildMeasured(t *testing.T, path string, line int) (*Input, uint64, time.Duration) {
	t.Helper()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	cpuBefore := processCPU(t)
	input, err := BuildInput(context.Background(), path, line, Options{})
	cpu := processCPU(t) - cpuBefore
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if input == nil || input.Answer == "" {
		t.Fatalf("empty input: %+v", input)
	}
	return input, after.TotalAlloc - before.TotalAlloc, cpu
}

// checkBuildBudget сверяет замер с базой байт base (верх трёх прогонов) и
// потолком CPU
func checkBuildBudget(t *testing.T, name string, allocated, base uint64, cpu time.Duration) {
	t.Helper()
	t.Logf("%s: allocated %d bytes (base %d), cpu %v", name, allocated, base, cpu)
	if raceBuild {
		return
	}
	if limit := uint64(float64(base) * allocMargin); allocated > limit {
		t.Errorf("%s allocated %d bytes, want at most %d (base %d × %.2f)", name, allocated, limit, base, allocMargin)
	}
	if cpu > cpuCeiling {
		t.Errorf("%s used %v of CPU, want under %v", name, cpu, cpuCeiling)
	}
}

// Вход судьи на длинном ходе строится за секунды, а не за минуты: на
// настоящем транскрипте 93 МБ сборка шла дольше 70 с. База — 1 141 770 824–
// 1 142 698 928 байт (28.11)
func TestMeasureBuildInputLongTurn(t *testing.T) {
	path, line := longTurnTranscript(t, 1500, 60, 5000)
	_, allocated, cpu := buildMeasured(t, path, line)
	checkBuildBudget(t, "BuildInput on a long turn (1500 calls)", allocated, 1_142_700_000, cpu)
}

// База — 404 756 392–405 348 664 байт (28.11)
func TestMeasureExcerptManyLongOutputs(t *testing.T) {
	path, line := longTurnTranscript(t, 40, 2000, 3000)
	_, allocated, cpu := buildMeasured(t, path, line)
	checkBuildBudget(t, "BuildInput on 40 calls × 2000 output lines", allocated, 405_350_000, cpu)
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

// Длинные Write проверяют общий матчёр на строках больше лимита входа. База —
// 106 133 864–106 158 768 байт (28.11)
func TestMeasureBuildInputManyLongWrites(t *testing.T) {
	for _, tc := range []struct {
		calls, words int
		base         uint64
	}{
		{calls: 8, words: 1500, base: 106_160_000},
	} {
		path, line := longWriteTranscript(t, tc.calls, 100, tc.words)
		input, allocated, cpu := buildMeasured(t, path, line)
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
		checkBuildBudget(t, fmt.Sprintf("BuildInput on %d Write × 100 lines × ~600 chars, %d answer words (%d retained, %d cropped)", tc.calls, tc.words, writes, croppedWrites), allocated, tc.base, cpu)
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
