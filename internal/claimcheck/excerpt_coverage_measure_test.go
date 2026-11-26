package claimcheck

import (
	"fmt"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMeasureExcerptWindowCoverage reconstructs a sparse-evidence turn: one
// line in five contains answer words, spread over all tool outputs.
func TestMeasureExcerptWindowCoverage(t *testing.T) {
	for _, tc := range []struct{ calls, lines, words int }{{30, 200, 300}, {60, 100, 400}} {
		t.Run(fmt.Sprintf("%dx%d-%d", tc.calls, tc.lines, tc.words), func(t *testing.T) {
			vocab := make([]string, tc.words)
			for i := range vocab {
				vocab[i] = fmt.Sprintf("evidence%04dword", i)
			}
			rows := []any{ev("user", "2026-01-01T00:00:00Z", "проверь результаты")}
			for c := 0; c < tc.calls; c++ {
				id := fmt.Sprintf("call-%d", c)
				at := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC).Add(time.Duration(c) * time.Second).Format(time.RFC3339)
				out := make([]string, tc.lines)
				for l := range out {
					out[l] = fmt.Sprintf("строка %d: обычный вывод команды без совпадений", l)
					if l%5 == 0 {
						out[l] += " " + vocab[(c*(tc.lines/5)+l/5)%tc.words]
					}
				}
				rows = append(rows, ev("assistant", at, []any{map[string]any{"type": "tool_use", "id": id, "name": "Bash", "input": map[string]any{"command": fmt.Sprintf("command-%d", c)}}}),
					map[string]any{"type": "user", "timestamp": at, "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": id, "content": strings.Join(out, "\n")}}}})
			}
			rows = append(rows, ev("assistant", "2026-01-02T00:00:00Z", []any{map[string]any{"type": "text", "text": strings.Join(vocab, " ")}}))
			path := fixture(t, rows...)
			var before, after syscall.Rusage
			if err := syscall.Getrusage(syscall.RUSAGE_SELF, &before); err != nil {
				t.Fatal(err)
			}
			input, err := BuildInput(path, len(rows), Options{})
			if err2 := syscall.Getrusage(syscall.RUSAGE_SELF, &after); err2 != nil {
				t.Fatal(err2)
			}
			if err != nil {
				t.Fatal(err)
			}
			var output strings.Builder
			for _, tool := range input.TurnTools {
				output.WriteString(tool.Output)
				output.WriteByte('\n')
			}
			visible := 0
			for _, word := range vocab {
				if strings.Contains(output.String(), word) {
					visible++
				}
			}
			cpu := time.Duration(after.Utime.Nano() + after.Stime.Nano() - before.Utime.Nano() - before.Stime.Nano())
			t.Logf("coverage %d/%d (%.1f%%), CPU %v", visible, tc.words, 100*float64(visible)/float64(tc.words), cpu)
		})
	}
}
