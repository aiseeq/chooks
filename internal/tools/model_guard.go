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
		data, err := readSettingsFile(abs)
		switch {
		case errors.Is(err, os.ErrNotExist):
			// Файл настроек пишет та же команда: до её запуска его нет, и
			// модель ищется в тексте самой команды
			return []byte(input.Command)
		case err != nil:
			logger.Warn("model_guard: файл --settings не читается, модель из него не проверена", "path", abs, "error", err)
			return nil
		}
		return data
	}
	return bashModelCandidates(input.Command, readSettings, logger)
}

// maxSettingsSize — предел файла --settings: настройки — килобайты
const maxSettingsSize = 1 << 20

// readSettingsFile читает файл настроек, только обычный и не больше
// maxSettingsSize: чтение FIFO или /dev/zero повесило бы хук (ревью 12.11)
func readSettingsFile(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("файл настроек %s: %w", path, err)
	}
	if !info.Mode().IsRegular() || info.Size() > maxSettingsSize {
		return nil, fmt.Errorf("не обычный файл или больше %d байт: %s", maxSettingsSize, info.Mode())
	}
	return os.ReadFile(path)
}

// modelClients — программы, которым модель задают в командной строке
var modelClients = []string{"claude", "codex", "opencode"}

// bashModelCandidates — модели вызовов агентов в команде оболочки. Вызовы
// ищет akit shellcmd: он видит и вложенные скрипты (bash -c, ssh, here-doc на
// вход оболочке). Команду, которую shellcmd не разобрал, проверяет прежний
// лексер: без вложенных скриптов, но и без пропуска простых вызовов
// readSettings читает файл --settings по пути из команды, nil — не прочесть;
// файла нет — текст команды, которая может его написать
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
		found = append(found, envModelCandidates(command, 0)...)
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
		found = append(found, envModelCandidates(command, 0)...)
	}
	return found
}

// modelEnvNames — переменные, задающие модель Claude Code. Переменные быстрой
// модели (haiku) Claude Code задаёт сам, они не проверяются
var modelEnvNames = map[string]bool{
	"ANTHROPIC_MODEL": true, "ANTHROPIC_DEFAULT_OPUS_MODEL": true,
	"ANTHROPIC_DEFAULT_SONNET_MODEL": true, "CLAUDE_CODE_SUBAGENT_MODEL": true,
}

// maxEnvDepth — предел вложенности скриптов (bash -c 'bash -c …') при поиске
// присваиваний
const maxEnvDepth = 4

// envModelCandidates — модели из присваиваний переменных modelEnvNames, что
// действуют на claude: префиксом вызова (и env), export/declare, префиксом
// оболочки и внутри её скрипта (bash -c, ssh). Слово с именем переменной в
// промпте, в теле here-doc для файла или у другой команды — не присваивание
// для claude (ревью 12.11). shellcmd присваиваний не отдаёт (тикет akit
// shellcmd-assignments), поэтому — прежний лексер
func envModelCandidates(script string, depth int) []modelCandidate {
	if depth > maxEnvDepth {
		return nil
	}
	var found []modelCandidate
	for _, cmd := range splitSimpleCommands(withoutHereDocBodies(script, true)) {
		name, args, _ := unwrapCommand(cmd)
		prefix := cmd[:len(cmd)-len(args)]
		switch {
		case name == "claude":
			found = append(found, assignedModels(prefix)...)
		case declarers[name]:
			// export, declare и родня: присваивания уходят в окружение следующих команд
			found = append(found, assignedModels(args)...)
		case shellNames[name]:
			found = append(found, assignedModels(prefix)...)
			for _, a := range args {
				found = append(found, envModelCandidates(a.text, depth+1)...)
			}
		}
	}
	return found
}

// assignedModels — значения присваиваний modelEnvNames среди слов
func assignedModels(words []word) []modelCandidate {
	var found []modelCandidate
	for _, w := range words {
		name, value, ok := strings.Cut(w.text, "=")
		if !ok || !modelEnvNames[name] || value == "" {
			continue
		}
		found = append(found, modelCandidate{value: value, dynamic: !w.literal})
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

// valueFlags — флаги claude со значением, которые пишутся и через «=»
var valueFlags = []string{"--settings", "--agents", "--fallback-model"}

// splitValueFlags разбивает --flag=value флагов valueFlags на два слова
func splitValueFlags(args []word) []word {
	out := make([]word, 0, len(args))
	for _, a := range args {
		if flag, value, ok := strings.Cut(a.text, "="); ok && slices.Contains(valueFlags, flag) {
			out = append(out, word{text: flag, literal: true}, word{text: value, literal: a.literal})
			continue
		}
		out = append(out, a)
	}
	return out
}

func candidatesFromArgs(name string, args []word, readSettings func(string) []byte) []modelCandidate {
	var found []modelCandidate
	if name == "claude" {
		args = splitValueFlags(args)
	}
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

// jsonModelKey — ключ модели в исходном тексте JSON, в том числе с
// экранированными кавычками внутри "…": "model": и \"ANTHROPIC_MODEL\":
var jsonModelKey = regexp.MustCompile(`\b(?:model|[A-Z_]+_MODEL)\\*"\s*:`)

// writtenModel — модель в тексте команды, которая пишет файл настроек:
// "model":"x" и "ANTHROPIC_MODEL":"x"
var writtenModel = regexp.MustCompile(`"(?:model|ANTHROPIC_MODEL|ANTHROPIC_DEFAULT_OPUS_MODEL|ANTHROPIC_DEFAULT_SONNET_MODEL|CLAUDE_CODE_SUBAGENT_MODEL)"\s*:\s*"([^"]*)"`)

// jsonModelCandidates — модели в JSON флага claude: --settings (строка JSON
// или файл: model и env.ANTHROPIC_MODEL…) и --agents (model каждого агента).
// Файла --settings ещё нет — модели в тексте команды, которая его пишет.
// Не JSON — моделей нет: такой запуск claude сам не начнётся
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
		// inherit — модель агента из основного вызова, она проверена отдельно
		if strings.TrimSpace(model) != "" && model != "inherit" {
			found = append(found, modelCandidate{value: model})
		}
	}
	if flag == "--agents" {
		var agents map[string]struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(raw, &agents) == nil {
			for _, a := range agents {
				add(a.Model)
			}
		}
		return found
	}
	// Поля разбираются по одному: поле не того типа рядом не прячет модель
	var settings map[string]json.RawMessage
	if json.Unmarshal(raw, &settings) != nil {
		if !json.Valid(raw) {
			// Текст команды вместо файла, которого ещё нет
			for _, m := range writtenModel.FindAllSubmatch(raw, -1) {
				add(string(m[1]))
			}
		}
		return found
	}
	var model string
	if json.Unmarshal(settings["model"], &model) == nil {
		add(model)
	}
	var env map[string]json.RawMessage
	if json.Unmarshal(settings["env"], &env) == nil {
		for name, raw := range env {
			var value string
			if modelEnvNames[name] && json.Unmarshal(raw, &value) == nil {
				add(value)
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
	v := core.NewViolation("model_guard", message, "", core.LevelCritical, 0, 0)
	return core.NewValidationResult(false, []core.Violation{v}, []string{modelGuardHint})
}
