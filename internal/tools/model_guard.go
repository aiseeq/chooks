package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/aiseeq/akit/shellcmd"
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
	home, err := os.UserHomeDir()
	if err != nil {
		// ~ в пути --settings не раскроется: файл не читается, остальное проверяется
		logger.Warn("model_guard: домашний каталог неизвестен", "error", err)
	}
	readSettings := func(path string) []byte {
		abs := absPath(path, input.CWD, home)
		if abs == "" {
			return nil
		}
		data, err := os.ReadFile(abs)
		switch {
		case errors.Is(err, os.ErrNotExist):
			// Файл настроек пишет та же команда: до её запуска его нет
			return nil
		case err != nil:
			logger.Warn("model_guard: файл --settings не читается, модель из него не проверена", "path", abs, "error", err)
			return nil
		}
		return data
	}
	return bashModelCandidates(input.Command, readSettings, logger)
}

// modelClients — программы, которым модель задают в командной строке
var modelClients = []string{"claude", "codex", "opencode"}

// bashModelCandidates — модели вызовов агентов в команде оболочки. Вызовы
// ищет akit shellcmd: он видит и вложенные скрипты (bash -c, ssh, here-doc на
// вход оболочке). Команду, которую shellcmd не разобрал, проверяет прежний
// лексер: без вложенных скриптов, но и без пропуска простых вызовов
// readSettings читает файл --settings по пути из команды, nil — не прочесть
func bashModelCandidates(command string, readSettings func(string) []byte, logger core.Logger) []modelCandidate {
	calls, err := shellcmd.Find(command, modelClients...)
	if calls == nil && err != nil {
		logger.Warn("model_guard: shellcmd не разобрал команду, проверка простых вызовов", "error", err)
		return lexedModelCandidates(command, readSettings)
	}
	var found []modelCandidate
	claude := false
	for _, call := range calls {
		args := make([]word, len(call.Args))
		for i, a := range call.Args {
			args[i] = word{text: a, literal: !strings.ContainsAny(a, "$`")}
		}
		found = append(found, candidatesFromArgs(call.Name, args, readSettings)...)
		if call.Name == "claude" {
			claude = true
		}
	}
	if claude {
		found = append(found, envModelCandidates(command)...)
	}
	return found
}

// lexedModelCandidates — модели вызовов агентов по прежнему лексеру
func lexedModelCandidates(command string, readSettings func(string) []byte) []modelCandidate {
	var found []modelCandidate
	claude := false
	for _, words := range splitSimpleCommands(withoutHereDocBodies(command, false)) {
		name, args, _ := unwrapCommand(words)
		if !slices.Contains(modelClients, name) {
			continue
		}
		found = append(found, candidatesFromArgs(name, args, readSettings)...)
		if name == "claude" {
			claude = true
		}
	}
	if claude {
		found = append(found, envModelCandidates(command)...)
	}
	return found
}

// modelEnv — присваивание переменной, задающей модель Claude Code: префиксом
// вызова, export или в вложенном скрипте. Ищется по всему тексту команды с
// вызовом claude: присваивание действует на вызов и из другой простой команды
// (export X=…; claude), а shellcmd присваиваний не отдаёт (тикет akit
// shellcmd-assignments). Переменные быстрой модели (haiku) Claude Code
// задаёт сам, они не проверяются
var modelEnv = regexp.MustCompile(`\b(?:ANTHROPIC_MODEL|ANTHROPIC_DEFAULT_OPUS_MODEL|ANTHROPIC_DEFAULT_SONNET_MODEL|CLAUDE_CODE_SUBAGENT_MODEL)=("[^"]*"|'[^']*'|[^\s;&|)'"]*)`)

func envModelCandidates(command string) []modelCandidate {
	var found []modelCandidate
	for _, m := range modelEnv.FindAllStringSubmatch(command, -1) {
		value, single := m[1], strings.HasPrefix(m[1], "'")
		if single || strings.HasPrefix(value, `"`) {
			value = value[1 : len(value)-1]
		}
		if value == "" {
			continue
		}
		found = append(found, modelCandidate{value: value, dynamic: !single && strings.ContainsAny(value, "$`")})
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

func candidatesFromArgs(name string, args []word, readSettings func(string) []byte) []modelCandidate {
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
		case name == "claude" && a.text == "--fallback-model":
			if i+1 < len(args) {
				i++
				found = append(found, modelCandidate{value: args[i].text, dynamic: !args[i].literal})
			}
		case name == "claude" && (a.text == "--settings" || a.text == "--agents"):
			if i+1 < len(args) {
				i++
				found = append(found, jsonModelCandidates(a.text, args[i], readSettings)...)
			}
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

// jsonModelCandidates — модели в JSON флага claude: --settings (строка JSON
// или файл: model и env.ANTHROPIC_MODEL…) и --agents (model каждого агента).
// Файла нет или он не JSON — моделей нет: такой запуск claude сам не начнётся
// jsonModelKey — ключ модели в исходном тексте JSON, в том числе с
// экранированными кавычками внутри "…": "model": и \"ANTHROPIC_MODEL\":
var jsonModelKey = regexp.MustCompile(`\b(?:model|[A-Z_]+_MODEL)\\*"\s*:`)

func jsonModelCandidates(flag string, arg word, readSettings func(string) []byte) []modelCandidate {
	if !arg.literal {
		// Путь файла из переменной не прочесть, а JSON с подстановкой — модель
		// переменной, если в нём есть поле модели
		if jsonModelKey.MatchString(arg.text) {
			return []modelCandidate{{value: arg.text, dynamic: true}}
		}
		return nil
	}
	raw := []byte(arg.text)
	if !strings.HasPrefix(strings.TrimSpace(arg.text), "{") {
		if flag != "--settings" {
			return nil
		}
		if raw = readSettings(arg.text); raw == nil {
			return nil
		}
	}
	var found []modelCandidate
	add := func(model string) {
		if strings.TrimSpace(model) != "" {
			found = append(found, modelCandidate{value: model})
		}
	}
	if flag == "--agents" {
		var agents map[string]struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(raw, &agents) == nil {
			for _, a := range agents {
				// inherit — модель основного вызова, она проверена отдельно
				if a.Model != "inherit" {
					add(a.Model)
				}
			}
		}
		return found
	}
	var settings struct {
		Model string            `json:"model"`
		Env   map[string]string `json:"env"`
	}
	if json.Unmarshal(raw, &settings) != nil {
		return nil
	}
	add(settings.Model)
	for name, value := range settings.Env {
		if modelEnv.MatchString(name + "=x") {
			add(value)
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
