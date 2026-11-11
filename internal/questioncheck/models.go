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
	case "low", "medium", "high":
	default:
		return CLIJudge{}, fmt.Errorf("unknown effort %q", effort)
	}
	return CLIJudge{Name: name, ID: id, Effort: effort, Dir: dir}, nil
}
