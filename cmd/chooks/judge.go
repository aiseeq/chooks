package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/aiseeq/chooks/internal/core"
	"github.com/aiseeq/chooks/internal/hookctx"
	"github.com/aiseeq/chooks/internal/questioncheck"
)

// newJudge — судья из списка одобренных моделей, запускаемый в пустом
// каталоге времени выполнения: там нет CLAUDE.md и AGENTS.md
func newJudge(model, effort string) (questioncheck.CLIJudge, error) {
	dir := filepath.Join(core.RuntimeDir(), "question-judge")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return questioncheck.CLIJudge{}, fmt.Errorf("create judge dir: %w", err)
	}
	return questioncheck.NewCLIJudge(model, effort, dir)
}

func newJudgeForRequest(model, effort string, request hookctx.Context) (questioncheck.CLIJudge, error) {
	dir := filepath.Join(request.RuntimeDir(), "question-judge")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return questioncheck.CLIJudge{}, fmt.Errorf("create judge dir: %w", err)
	}
	path := request.Path(hookctx.EnvModelsApproved)
	if path == "" {
		home := request.Path(hookctx.EnvHome)
		if home == "" {
			return questioncheck.CLIJudge{}, fmt.Errorf("cannot resolve home dir for the approved models list: $HOME is not defined")
		}
		path = filepath.Join(home, ".config", "models", "approved")
	}
	return questioncheck.NewCLIJudgeFromPath(model, effort, dir, path)
}

// stopBlockOutput — ответ Stop-хука, который не даёт ходу закончиться:
// агент получает reason и продолжает
type stopBlockOutput struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}
