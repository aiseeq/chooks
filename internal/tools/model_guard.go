package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/aiseeq/chooks/internal/approved"
	"github.com/aiseeq/chooks/internal/core"
)

const defaultApprovedModelsPath = "~/.config/models/approved"

const modelGuardHint = `Правило ~/.claude/CLAUDE.md "Модели"; Luna и Sol — через worker; новая модель — только через бенчмарк llmark и одобрение пользователя`

type approvedModel struct{ name, id string }

// ModelGuardTool пропускает только модели, явно перечисленные пользователем.
type ModelGuardTool struct {
	*BaseTool
	path string
}

// NewModelGuardTool создаёт проверку выбранной модели.
func NewModelGuardTool(config core.ToolConfig, logger core.Logger) (*ModelGuardTool, error) {
	return &ModelGuardTool{BaseTool: NewBaseTool("model_guard", config.Enabled, append([]string{"Agent"}, core.ShellTools...), logger)}, nil
}

// ValidateTool проверяет модель лишь у вызовов, которые её явно задают.
func (t *ModelGuardTool) ValidateTool(_ context.Context, input *core.ToolInput) (*core.ValidationResult, error) {
	if !t.IsEnabled() || input == nil {
		return &core.ValidationResult{IsValid: true}, nil
	}
	candidates := modelCandidates(input, t.Logger())
	if len(candidates) == 0 {
		return &core.ValidationResult{IsValid: true}, nil
	}
	path := t.approvedPath()
	models, reason := readApprovedModels(path)
	if reason != "" {
		return t.reject(reason), nil
	}
	var reasons []string
	for _, c := range candidates {
		if c.dynamic {
			reasons = append(reasons, "модель задана переменной, назови её литералом")
		} else if !isApproved(c.value, models) {
			reasons = append(reasons, fmt.Sprintf("модель %q не одобрена", c.value))
		}
	}
	if len(reasons) == 0 {
		return &core.ValidationResult{IsValid: true}, nil
	}
	return t.reject(strings.Join(reasons, "; "), models), nil
}

type modelCandidate struct {
	value   string
	dynamic bool
}

func modelCandidates(input *core.ToolInput, logger core.Logger) []modelCandidate {
	if input.ToolName == "Agent" {
		var found []modelCandidate
		var fields map[string]json.RawMessage
		if json.Unmarshal(input.ToolInput, &fields) == nil {
			if raw := fields["model"]; len(raw) > 0 && string(raw) != "null" {
				var model string
				if err := json.Unmarshal(raw, &model); err != nil {
					logger.Warn("поле модели Agent имеет неверный тип", "value", string(raw), "error", err)
					found = append(found, modelCandidate{value: string(raw)})
				} else if strings.TrimSpace(model) != "" {
					found = append(found, modelCandidate{value: model})
				}
			}
		}
		return found
	}
	if !core.IsShellTool(input.ToolName) {
		return nil
	}
	return bashModelCandidates(input.Command)
}

func bashModelCandidates(command string) []modelCandidate {
	var found []modelCandidate
	for _, words := range splitSimpleCommands(withoutHereDocBodies(command, false)) {
		name, args, _ := unwrapCommand(words)
		if name != "claude" && name != "codex" && name != "opencode" {
			continue
		}
		found = append(found, candidatesFromArgs(name, args)...)
	}
	return found
}

// withoutHereDocBodies вырезает тела here-doc: это текст для файла или
// утилиты, не команды. keepShellFed оставляет тело, если в строке с его
// началом есть оболочка (bash <<EOF, cat <<EOF | sh, ssh host <<EOF): его
// строки она исполнит
func withoutHereDocBodies(command string, keepShellFed bool) string {
	var output strings.Builder
	var delimiters []hereDoc
	keep := false
	for _, line := range strings.SplitAfter(command, "\n") {
		plain := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if len(delimiters) > 0 {
			bodyLine := plain
			if delimiters[0].stripTab {
				bodyLine = strings.TrimLeft(bodyLine, "\t")
			}
			switch {
			case bodyLine == delimiters[0].name:
				delimiters = delimiters[1:]
			case keep:
				output.WriteString(line)
			}
			continue
		}
		output.WriteString(line)
		lexed := lexShell(plain)
		delimiters = append(delimiters, lexed.hereDocs...)
		keep = keepShellFed && feedsShell(lexed.cmds)
	}
	return output.String()
}

// shellNames утилиты, которые исполняют свой вход как команды оболочки
var shellNames = map[string]bool{"bash": true, "sh": true, "zsh": true, "dash": true, "ksh": true, "fish": true, "ssh": true}

func feedsShell(cmds [][]word) bool {
	for _, cmd := range cmds {
		if name, _, _ := unwrapCommand(cmd); shellNames[name] {
			return true
		}
	}
	return false
}

func candidatesFromArgs(name string, args []word) []modelCandidate {
	var found []modelCandidate
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a.text == "--model" || (name != "claude" && a.text == "-m"):
			if i+1 < len(args) {
				i++
				found = append(found, modelCandidate{value: args[i].text, dynamic: !args[i].literal})
			}
		case strings.HasPrefix(a.text, "--model="):
			found = append(found, modelCandidate{value: strings.TrimPrefix(a.text, "--model="), dynamic: !a.literal})
		case name == "codex" && (a.text == "-c" || a.text == "--config"):
			if i+1 < len(args) {
				i++
				if candidate, ok := codexConfigCandidate(args[i]); ok {
					found = append(found, candidate)
				}
			}
		}
	}
	return found
}

func codexConfigCandidate(config word) (modelCandidate, bool) {
	if !strings.HasPrefix(config.text, "model=") {
		return modelCandidate{}, false
	}
	return modelCandidate{value: strings.TrimPrefix(config.text, "model="), dynamic: !config.literal}, true
}

func (t *ModelGuardTool) approvedPath() string {
	if os.Getenv("MODELS_APPROVED") == "" && t.path != "" {
		return t.path
	}
	path, err := approved.Path()
	if err != nil {
		t.Logger().Warn("не удалось определить домашний каталог для списка моделей", "error", err)
		return defaultApprovedModelsPath
	}
	return path
}

func readApprovedModels(path string) ([]approvedModel, string) {
	list, err := approved.Read(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, fmt.Sprintf("список одобренных моделей %s не найден", path)
	case errors.Is(err, approved.ErrEmpty):
		return nil, fmt.Sprintf("список одобренных моделей %s пуст", path)
	case err != nil:
		return nil, fmt.Sprintf("список одобренных моделей не читается: %v", err)
	}
	models := make([]approvedModel, 0, len(list))
	for _, m := range list {
		if normalizeModel(m.ID) == "" {
			return nil, fmt.Sprintf("список одобренных моделей %s, строка %d: id без имени модели", path, m.Line)
		}
		models = append(models, approvedModel{m.Name, m.ID})
	}
	return models, ""
}

func isApproved(candidate string, models []approvedModel) bool {
	for _, m := range models {
		if strings.EqualFold(normalizeModel(candidate), m.name) || matchesModelID(candidate, m.id) {
			return true
		}
	}
	return false
}

// matchesModelID сравнивает кандидата с id из списка. id без «/» не называет
// маршрут: префикс провайдера у кандидата не важен. id с «/» называет его:
// кандидат с префиксом должен совпасть целиком, тот же конец под другим
// префиксом - другой маршрут; голое имя сравнивается с концом id
func matchesModelID(candidate, id string) bool {
	if strings.Contains(candidate, "/") && strings.Contains(id, "/") {
		return strings.EqualFold(withoutModelVariant(candidate), withoutModelVariant(id))
	}
	return strings.EqualFold(normalizeModel(candidate), normalizeModel(id))
}

func normalizeModel(model string) string {
	if i := strings.LastIndex(model, "/"); i >= 0 {
		model = model[i+1:]
	}
	return withoutModelVariant(model)
}

func withoutModelVariant(model string) string {
	if i := strings.Index(model, "["); i >= 0 && strings.HasSuffix(model, "]") {
		return model[:i]
	}
	return model
}

func (t *ModelGuardTool) reject(reason string, models ...[]approvedModel) *core.ValidationResult {
	var list []string
	if len(models) > 0 {
		for _, m := range models[0] {
			list = append(list, fmt.Sprintf("%s (%s)", m.name, m.id))
		}
	}
	message := reason
	if len(list) > 0 {
		message += "; одобрены: " + strings.Join(list, ", ")
	}
	message += "; " + modelGuardHint
	v := core.NewViolation("model_guard", message, "", core.LevelCritical, 0, 0)
	return core.NewValidationResult(false, []core.Violation{v}, []string{message})
}
