package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/aiseeq/chooks/internal/core"
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

// stopBlockOutput — ответ Stop-хука, который не даёт ходу закончиться:
// агент получает reason и продолжает
type stopBlockOutput struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}
