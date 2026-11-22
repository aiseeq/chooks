package notifier

import (
	"testing"

	"github.com/aiseeq/chooks/internal/core"
	"github.com/aiseeq/chooks/internal/hookctx"
)

// Относительный $HOME сравнивается с каталогом из ввода как строка, как у
// CLI: с абсолютным каталогом под cwd запроса он не совпадает ни у CLI, ни у
// демона, и имя проекта — последний элемент каталога
func TestProjectNameRelativeHomeCLIAndRequest(t *testing.T) {
	env := []string{"HOME=home"}
	cli, err := hookctx.FromProcess()
	if err != nil {
		t.Fatal(err)
	}
	cli.Env = env
	daemon := hookctx.New(nil, nil, nil, env, "/w", 1, 2)
	input := core.ToolInput{CWD: "/w/home/work/projecta/frontend"}
	for name, request := range map[string]hookctx.Context{"CLI": cli, "request": daemon} {
		n := NewForRequest(core.ToolConfig{Enabled: true}, testLogger(t), request)
		if got := n.ProjectName(&input); got != "frontend" {
			t.Errorf("%s: project %q, want frontend as for a relative HOME that matches no absolute dir", name, got)
		}
	}
}
