package understandcheck

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiseeq/chooks/internal/hookctx"
)

// Относительный transcript_path запроса демона читается от cwd запроса: в
// cwd процесса лежит другой транскрипт с тем же именем
func TestReadTurnRelativeFromRequestCwd(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	write := func(dir, reply string) {
		t.Helper()
		body := strings.Join([]string{agentLine(1, "Вопрос?", "end_turn"), humanLine(2, reply)}, "\n") + "\n"
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "s.jsonl"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(root, "ответ из cwd процесса")
	requestDir := filepath.Join(root, "request")
	write(requestDir, "ответ из cwd запроса")
	request := hookctx.New(nil, nil, nil, []string{"HOME=" + requestDir}, requestDir, 1, 2)
	turn, err := ReadTurn("s.jsonl", request)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(turn.Human, "ответ из cwd запроса") {
		t.Errorf("human = %q, want the request cwd transcript", turn.Human)
	}
}
