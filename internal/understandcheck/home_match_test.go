package understandcheck

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aiseeq/chooks/internal/hookctx"
)

// ~/ в команде раскрывается относительным $HOME как строка, как у CLI: файл
// правила в журнале — тот же у CLI и у демона с другим cwd процесса
func TestRuleFileRelativeHomeCLIAndRequest(t *testing.T) {
	env := []string{"HOME=home"}
	cli, err := hookctx.FromProcess()
	if err != nil {
		t.Fatal(err)
	}
	cli.Env = env
	daemon := hookctx.New(nil, nil, nil, env, "/w", 1, 2)
	raw, _ := json.Marshal(map[string]any{"command": "cp new.md ~/.claude/rules/a.md"})
	for name, request := range map[string]hookctx.Context{"CLI": cli, "request": daemon} {
		f := newHookFixture(t, fixAnswer)
		f.hook.Run(hookctx.WithRequest(context.Background(), request), Call{SessionID: "sess-1", TranscriptPath: f.path, Tool: "Bash", Input: raw})
		if e := f.journal(t); len(e) != 1 || e[0].File != "home/.claude/rules/a.md" {
			t.Errorf("%s: journal %+v, want file home/.claude/rules/a.md", name, e)
		}
	}
}
