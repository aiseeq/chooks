package questioncheck

import (
	"fmt"

	"github.com/aiseeq/chooks/internal/approved"
)

// NewCLIJudge собирает судью по имени модели, сверяя его со списком одобренных
// (approved.Path: MODELS_APPROVED или ~/.config/models/approved)
func NewCLIJudge(name, effort, dir string) (CLIJudge, error) {
	path, err := approved.Path()
	if err != nil {
		return CLIJudge{}, err
	}
	return NewCLIJudgeFromPath(name, effort, dir, path)
}

// NewCLIJudgeFromPath reads the approved models list belonging to a request.
func NewCLIJudgeFromPath(name, effort, dir, path string) (CLIJudge, error) {
	models, err := approved.Read(path)
	if err != nil {
		return CLIJudge{}, fmt.Errorf("approved models: %w", err)
	}
	id := ""
	for _, m := range models {
		if m.Name == name {
			id = m.ID
		}
	}
	if id == "" {
		return CLIJudge{}, fmt.Errorf("model %q is not in %s", name, path)
	}
	switch effort {
	// не ниже high: пользователь 22.11 (tl 27d21ddd:21210) «Ни одну из моделей
	// нельзя использовать менее чем на high»
	case "high", "xhigh":
	default:
		return CLIJudge{}, fmt.Errorf("effort %q: allowed high, xhigh", effort)
	}
	return CLIJudge{Name: name, ID: id, Effort: effort, Dir: dir}, nil
}
