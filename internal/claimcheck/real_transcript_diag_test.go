package claimcheck

import (
	"os"
	"testing"
	"time"
)

// TestRealTranscriptDiagnostic is opt-in because the fixture is a private,
// large transcript. Set CC_REAL_TRANSCRIPT to measure the last answer build.
func TestRealTranscriptDiagnostic(t *testing.T) {
	path := os.Getenv("CC_REAL_TRANSCRIPT")
	if path == "" {
		t.Skip("set CC_REAL_TRANSCRIPT to a transcript path")
	}
	started := time.Now()
	line, _, err := lastAnswerLine(path)
	lastAnswerTook := time.Since(started)
	if err != nil {
		t.Fatal(err)
	}
	started = time.Now()
	input, err := BuildInput(path, line, Options{})
	buildTook := time.Since(started)
	if err != nil {
		t.Fatal(err)
	}
	size, err := inputJSONSize(input)
	if err != nil {
		t.Fatal(err)
	}
	calls, keptOutput := 0, 0
	for _, tool := range input.TurnTools {
		if tool.Skipped > 0 {
			calls += tool.Skipped
			continue
		}
		calls++
		keptOutput += tool.OutputLength
	}
	t.Logf("lastAnswerLine=%v BuildInput=%v inputJSONSize=%d chars line=%d turnLine=%d tools=%d answerWords=%d keptOutputRunes=%d", lastAnswerTook, buildTook, size, line, input.TurnLine, calls, len(answerWords(input.Answer)), keptOutput)
}
