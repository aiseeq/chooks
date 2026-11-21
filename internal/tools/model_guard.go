package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/aiseeq/akit/shellcmd"
	"github.com/aiseeq/chooks/internal/approved"
	"github.com/aiseeq/chooks/internal/core"
	"golang.org/x/text/unicode/norm"
	"gopkg.in/yaml.v3"
	"mvdan.cc/sh/v3/syntax"
)

const defaultApprovedModelsPath = "~/.config/models/approved"

const modelGuardHint = `Правило ~/.claude/CLAUDE.md, раздел «Что разрешает «да»»: «Модели бери только из ~/.config/models/approved» (кому какая — в шапке файла); бойцы GPT — через worker; новая модель — только через бенчмарк llmark и одобрение пользователя`

type approvedModel struct{ name, id string }

// ModelGuardTool пропускает только модели, явно перечисленные пользователем.
type ModelGuardTool struct {
	*BaseTool
	path string
	// userOnly — имена одобренных моделей, которые запускает только
	// пользователь (core.ToolConfig.UserOnly)
	userOnly []string
}

// NewModelGuardTool создаёт проверку выбранной модели.
func NewModelGuardTool(config core.ToolConfig, logger core.Logger) (*ModelGuardTool, error) {
	return &ModelGuardTool{
		BaseTool: NewBaseTool("model_guard", config.Enabled, append([]string{"Agent"}, core.ShellTools...), logger),
		userOnly: config.UserOnly,
	}, nil
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
		return t.reject(reason, nil), nil
	}
	var reasons []string
	for _, c := range candidates {
		if c.unknownAgent {
			reasons = append(reasons, fmt.Sprintf("определение агента %s не найдено: модель не проверить", c.value))
			continue
		}
		if c.unknownWrite {
			reasons = append(reasons, "содержимое изменяемого файла настроек не проверить")
			continue
		}
		if c.unknownAgents {
			reasons = append(reasons, "файл --agents не прочитать: модель агента не проверить")
			continue
		}
		if c.userOnlyCheck {
			if c.dynamic {
				reasons = append(reasons, "модель задана переменной, назови её литералом")
				continue
			}
			// Переменная быстрой модели здесь проверяется только на ограничение
			// user_only, без проверки списка одобренных моделей.
			if name, ok := t.userOnlyModel(c.value, models); ok {
				reasons = append(reasons, userOnlyReason(c.value, name))
			}
			continue
		}
		if c.dynamic {
			reasons = append(reasons, "модель задана переменной, назови её литералом")
		} else if !isApproved(c.value, models) {
			reasons = append(reasons, fmt.Sprintf("модель %q не одобрена", c.value))
		} else if name, ok := t.userOnlyModel(c.value, models); ok {
			reasons = append(reasons, userOnlyReason(c.value, name))
		}
	}
	if len(reasons) == 0 {
		return &core.ValidationResult{IsValid: true}, nil
	}
	return t.reject(strings.Join(reasons, "; "), models), nil
}

func userOnlyReason(value, name string) string {
	return fmt.Sprintf("модель %q (%s) запускает только пользователь: агент её не задаёт; в сессии, которую пользователь поднял на ней, поле model не задавай — суб-агент унаследует модель", value, name)
}

type modelCandidate struct {
	value         string
	dynamic       bool
	unknownAgent  bool
	unknownWrite  bool
	unknownAgents bool
	// userOnlyCheck — переменные модели быстрых запросов проверяются только
	// на ограничение user_only.
	userOnlyCheck bool
}

func modelCandidates(input *core.ToolInput, logger core.Logger) []modelCandidate {
	if input.ToolName != "Agent" && !core.IsShellTool(input.ToolName) {
		return nil
	}
	if input.CWD == "" {
		if cwd, err := os.Getwd(); err == nil {
			input.CWD = cwd
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// ~ в пути --settings не раскроется, определения агентов в ~ не
		// читаются: остальное проверяется
		logger.Warn("model_guard: домашний каталог неизвестен", "error", err)
	}
	agentModels := func(name string) []modelCandidate {
		return agentDefinitionModels(name, input.CWD, home, nil, logger, false)
	}
	if input.ToolName == "Agent" {
		return agentToolCandidates(input, agentModels, logger)
	}
	unresolvedTildeSettings := false
	readSettings := func(path string, tilde shellcmd.Tilde, cwd string) []byte {
		commandHome, _, homeDynamic := commandHomeDir(input.Command, cwd, home)
		if homeDynamic {
			unresolvedTildeSettings = true
			return nil
		}
		if tilde&shellcmd.TildeHome != 0 && commandHome == "" {
			unresolvedTildeSettings = true
			return nil
		}
		abs := absPath(tildePath(path, tilde, commandHome), cwd)
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
	settingsAt := func(cwd string, includeProject bool) []modelCandidate {
		commandHome, _, dynamic := commandHomeDir(input.Command, cwd, home)
		if dynamic {
			return []modelCandidate{{value: "HOME", dynamic: true}}
		}
		return settingsForCWD(cwd, input.Command, commandHome, logger, includeProject)
	}
	candidates := bashModelCandidatesAt(input.Command, input.CWD, modelFiles{settings: readSettings, agent: agentModels}, settingsAt, func(name, cwd string, extra []string) []modelCandidate {
		commandHome, _, homeDynamic := commandHomeDir(input.Command, cwd, home)
		if homeDynamic {
			return []modelCandidate{{value: "HOME", dynamic: true}}
		}
		if configDir, found, dynamic := commandConfigDir(input.Command, cwd, commandHome); found {
			if dynamic {
				return []modelCandidate{{value: "CLAUDE_CONFIG_DIR", dynamic: true}}
			}
			extra = append(extra, filepath.Join(configDir, "agents"))
		} else if configDir := os.Getenv("CLAUDE_CONFIG_DIR"); configDir != "" {
			extra = append(extra, filepath.Join(configDir, "agents"))
		}
		extra = append(extra, filepath.Join(commandHome, ".claude", "agents"))
		return agentDefinitionModels(name, cwd, commandHome, extra, logger, true)
	}, logger)
	if unresolvedTildeSettings {
		candidates = append(candidates, modelCandidate{value: "settings path with unknown home", dynamic: true})
	}
	return candidates
}

func settingsForCWD(cwd, command, home string, logger core.Logger, includeProject bool) []modelCandidate {
	configDir := os.Getenv("CLAUDE_CONFIG_DIR")
	if value, found, dynamic := commandConfigDir(command, cwd, home); found {
		if dynamic {
			return []modelCandidate{{value: "CLAUDE_CONFIG_DIR", dynamic: true}}
		}
		configDir = value
	}
	var paths []string
	if includeProject {
		paths = append(paths, projectSettingsPaths(cwd)...)
	}
	paths = append(paths, filepath.Join(home, ".claude", "settings.json"), managedSettingsPath)
	var agentDirsForSettings []string
	if configDir != "" {
		paths = append(paths, filepath.Join(configDir, "settings.json"))
		agentDirsForSettings = append(agentDirsForSettings, filepath.Join(configDir, "agents"))
	}
	agentDirsForSettings = append(agentDirsForSettings, filepath.Join(home, ".claude", "agents"))
	lookupCWD := cwd
	if !includeProject {
		lookupCWD = ""
	}
	lookupAgent := func(name string) []modelCandidate {
		return agentDefinitionModels(name, lookupCWD, home, agentDirsForSettings, logger, true)
	}
	var candidates []modelCandidate
	for _, path := range paths {
		raw, err := readSettingsFile(path)
		if err == nil {
			candidates = append(candidates, settingsCandidates(raw, lookupAgent)...)
		} else if !errors.Is(err, os.ErrNotExist) {
			logger.Warn("model_guard: файл настроек не читается, модель не проверена", "path", path, "error", err)
			candidates = append(candidates, modelCandidate{value: path, dynamic: true})
		}
	}
	return candidates
}

func projectSettingsPaths(cwd string) []string {
	var paths []string
	cwd = filepath.Clean(cwd)
	for dir := cwd; dir != ""; {
		paths = append(paths, filepath.Join(dir, ".claude", "settings.json"), filepath.Join(dir, ".claude", "settings.local.json"))
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return paths
}

var commandEnvAssignment = regexp.MustCompile(`(?:^|[;|&\n\s])(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)=("[^"]*"|'[^']*'|[^\s;|&]+)`)

func commandEnvPath(command, name, cwd, home string) (string, bool, bool) {
	matches := commandEnvAssignment.FindAllStringSubmatch(command, -1)
	var value string
	found := false
	for _, match := range matches {
		if match[1] == name {
			value, found = strings.Trim(match[2], "'\""), true
		}
	}
	if !found {
		return "", false, false
	}
	if strings.ContainsAny(value, "$`*?[") {
		return "", true, true
	}
	if after, ok := strings.CutPrefix(value, "~/"); ok {
		value = filepath.Join(home, after)
	}
	return resolvePath(value, 0, cwd, home), true, false
}

func commandConfigDir(command, cwd, home string) (string, bool, bool) {
	return commandEnvPath(command, "CLAUDE_CONFIG_DIR", cwd, home)
}

func commandHomeDir(command, cwd, defaultHome string) (string, bool, bool) {
	value, found, dynamic := commandEnvPath(command, "HOME", cwd, defaultHome)
	if !found {
		return defaultHome, false, false
	}
	return value, true, dynamic
}

// maxSettingsSize — предел файла --settings: настройки — килобайты
const maxSettingsSize = 1 << 20

// agentToolCandidates — модель Agent: поле model, без него — модель
// определения агента subagent_type
func agentToolCandidates(input *core.ToolInput, agentModels func(string) []modelCandidate, logger core.Logger) []modelCandidate {
	var fields map[string]json.RawMessage
	if json.Unmarshal(input.ToolInput, &fields) != nil {
		return nil
	}
	if raw := fields["model"]; len(raw) > 0 && string(raw) != "null" {
		var model string
		if err := json.Unmarshal(raw, &model); err != nil {
			logger.Warn("поле модели Agent имеет неверный тип", "value", string(raw), "error", err)
			return []modelCandidate{{value: string(raw)}}
		}
		if strings.TrimSpace(model) != "" {
			return []modelCandidate{{value: model}}
		}
	}
	var agentType string
	if json.Unmarshal(fields["subagent_type"], &agentType) == nil && agentType != "" {
		return agentModels(agentType)
	}
	return nil
}

// agentFlagCandidates — модель агента claude --agent: имя из переменной не
// прочесть — модель переменной, как у --model
func agentFlagCandidates(agent word, files modelFiles) []modelCandidate {
	if !agent.literal {
		return []modelCandidate{{value: agent.text, dynamic: true}}
	}
	return files.agent(agent.text)
}

// modelFiles — файлы, откуда клиент берёт модель: --settings по пути и
// определение агента по имени
type modelFiles struct {
	settings func(path string, tilde shellcmd.Tilde, cwd string) []byte
	agent    func(name string) []modelCandidate
}

// agentDefinitionModels — модель из определения агента name: Agent без
// model и claude --agent берут её оттуда. Определения — *.md в
// agentDirs, по симлинкам тоже; имя — поле name frontmatter, сравнивается
// точно и нормализованным, как в Claude Code 2.1.330 (приёмка 21.11).
// Модель определения проверяется по обычному списку одобренных моделей.
func agentDefinitionModels(name, cwd, home string, extra []string, logger core.Logger, rejectMissing bool) []modelCandidate {
	var found []modelCandidate
	definitionFound := false
	walk := agentWalk{seen: map[string]bool{}, logger: logger}
	dirs := append(agentDirs(cwd, home), extra...)
	if builtinAgent(name) {
		pluginDir := filepath.Join(home, ".claude", "plugins")
		dirs = slices.DeleteFunc(dirs, func(dir string) bool { return filepath.Clean(dir) == pluginDir })
	}
	for _, dir := range dirs {
		// срок — каждому корню, как в Claude Code: большое дерево одного
		// каталога не снимает проверку остальных
		walk.deadline, walk.late = time.Now().Add(agentScanBudget), false
		walk.dir(dir)
	}
	for _, path := range walk.files {
		data, ok, err := readAgentDefinition(path)
		if err != nil {
			logger.Warn("model_guard: определение агента не читается, модель из него не проверена", "path", path, "error", err)
			continue
		}
		// больше maxAgentFile Claude Code определение не загружает; inherit
		// не задаёт отдельную модель и проверку проходит.
		if defName, model := agentFrontmatter(data, path); ok && agentNameMatches(defName, name) {
			definitionFound = true
			if model != "" && model != "inherit" {
				found = append(found, modelCandidate{value: model})
			}
		}
	}
	if rejectMissing && !definitionFound && !builtinAgent(name) {
		return []modelCandidate{{value: name, unknownAgent: true}}
	}
	return found
}

var managedSettingsPath = "/etc/claude-code/managed-settings.json"

// names confirmed in Claude Code 2.1.330 strings; Explore and Plan may be
// disabled by environment, but treating their names as built in is safe.
var builtinAgents = map[string]bool{"general-purpose": true, "Explore": true, "Plan": true, "claude-code-guide": true, "statusline-setup": true, "claude": true, "fork": true}

func builtinAgent(name string) bool { return builtinAgents[name] }

func agentNameMatches(defName, name string) bool {
	if sameAgentName(defName, name) {
		return true
	}
	_, pluginName, ok := strings.Cut(name, ":")
	return ok && sameAgentName(defName, pluginName)
}

// agentDirs — каталоги определений агентов: пользовательские первыми
// ($CLAUDE_CONFIG_DIR/agents и ~/.claude/agents), затем .claude/agents в
// cwd и всех его предках (Claude Code собирает их вверх до корня
// репозитория или ~, хук — до /, это шире)
func agentDirs(cwd, home string) []string {
	var dirs []string
	if cwd != "" {
		cwd = filepath.Clean(cwd)
	}
	if config := os.Getenv("CLAUDE_CONFIG_DIR"); config != "" {
		dirs = append(dirs, filepath.Join(config, "agents"))
	}
	if home != "" {
		dirs = append(dirs, filepath.Join(home, ".claude", "agents"))
		dirs = append(dirs, filepath.Join(home, ".claude", "plugins"))
	}
	if cwd != "" {
		for dir := cwd; dir != ""; {
			dirs = append(dirs, filepath.Join(dir, ".claude", "agents"))
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	return dirs
}

// agentScanBudget — время на обход одного корня каталогов агентов: Claude
// Code обрывает обход каждого каталога через 3 с (AbortSignal.timeout на
// вызов, 2.1.330), хук не ждёт дольше
const agentScanBudget = 3 * time.Second

// agentWalk — обход каталогов агентов: обычные файлы *.md во всех
// подкаталогах, по симлинкам на файлы и каталоги, как Claude Code (rg
// --follow); каждый каталог — один раз по реальному пути (петли симлинков,
// ~/.claude/agents и как предок cwd, и как пользовательский)
type agentWalk struct {
	seen     map[string]bool
	deadline time.Time
	logger   core.Logger
	files    []string
	late     bool
}

func (w *agentWalk) dir(dir string) {
	if time.Now().After(w.deadline) {
		if !w.late {
			w.late = true
			w.logger.Warn("model_guard: обход каталогов агентов не уложился в срок, остальные определения не проверены", "budget", agentScanBudget, "dir", dir)
		}
		return
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		// нет каталога — определений в нём нет
		if !errors.Is(err, fs.ErrNotExist) {
			w.logger.Warn("model_guard: каталог агентов не читается, модели определений не проверены", "path", dir, "error", err)
		}
		return
	}
	if w.seen[real] {
		return
	}
	w.seen[real] = true
	entries, err := os.ReadDir(dir)
	if err != nil {
		w.logger.Warn("model_guard: каталог агентов не читается, модели определений не проверены", "path", dir, "error", err)
		return
	}
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		info, err := os.Stat(path)
		if err != nil {
			// битый симлинк Claude Code тоже не прочтёт
			if !errors.Is(err, fs.ErrNotExist) {
				w.logger.Warn("model_guard: файл в каталоге агентов не читается, модель из него не проверена", "path", path, "error", err)
			}
			continue
		}
		if info.IsDir() {
			w.dir(path)
		} else if info.Mode().IsRegular() && strings.HasSuffix(e.Name(), ".md") {
			w.files = append(w.files, path)
		}
	}
}

// maxAgentFile — определение больше Claude Code не загружает (2.1.330)
const maxAgentFile = 1 << 20

// readAgentDefinition — определение агента целиком; ok=false — больше
// maxAgentFile, Claude Code его не загрузит
func readAgentDefinition(path string) (data []byte, ok bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, fmt.Errorf("определение агента %s: %w", path, err)
	}
	defer f.Close()
	data, err = io.ReadAll(io.LimitReader(f, maxAgentFile+1))
	if err != nil {
		return nil, false, fmt.Errorf("определение агента %s: %w", path, err)
	}
	return data, len(data) <= maxAgentFile, nil
}

// sameAgentName — имя subagent_type или --agent указывает на определение:
// точно или после нормализации Claude Code (NFKC, нижний регистр, без
// пробелов, тире \p{Pd} и _). Claude Code берёт нормализованное только при
// единственном совпадении — хук отказывает при любом, это шире
func sameAgentName(defName, name string) bool {
	return defName == name || normalizeAgentName(defName) == normalizeAgentName(name)
}

func normalizeAgentName(name string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || unicode.Is(unicode.Pd, r) || r == '_' {
			return -1
		}
		return unicode.ToLower(r)
	}, norm.NFKC.String(name))
}

// agentFrontmatterBlock — frontmatter в начале определения, как в Claude
// Code: ^---\s*\n … \n---
var agentFrontmatterBlock = regexp.MustCompile(`^---\s*\n([\s\S]*?)\n---\s*(?:\n|$)`)

// agentFrontmatter — name и model верхнего уровня frontmatter определения
// агента (YAML, как в Claude Code; BOM и CRLF снимаются); без name — имя
// файла. YAML не разобрался — ключи верхнего уровня построчно
func agentFrontmatter(data []byte, path string) (name, model string) {
	name = strings.TrimSuffix(filepath.Base(path), ".md")
	text := strings.ReplaceAll(strings.TrimPrefix(string(data), "\uFEFF"), "\r\n", "\n")
	m := agentFrontmatterBlock.FindStringSubmatch(text)
	if m == nil {
		return name, ""
	}
	fields := map[string]any{}
	if yaml.Unmarshal([]byte(m[1]), &fields) != nil {
		fields = topLevelFields(m[1])
	}
	if v, ok := fields["name"].(string); ok && strings.TrimSpace(v) != "" {
		name = strings.TrimSpace(v)
	}
	if v, ok := fields["model"].(string); ok {
		model = strings.TrimSpace(v)
	}
	return name, model
}

// topLevelFields — ключи верхнего уровня frontmatter, который YAML не
// разобрал: строки без отступа key: value
func topLevelFields(text string) map[string]any {
	fields := map[string]any{}
	for line := range strings.Lines(text) {
		if line == "" || line[0] == ' ' || line[0] == '\t' {
			continue
		}
		if key, value, ok := strings.Cut(line, ":"); ok {
			fields[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), `"'`)
		}
	}
	return fields
}

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

// modelClients — программы, которым модель задают в командной строке: cl
// передаёт флаги claude, worker run|ask|continue берёт --model
var modelClients = []string{"claude", "cl", "codex", "opencode", "worker"}

// clientDialect — по чьим правилам разбирать флаги клиента: cl — обёртка claude
func clientDialect(name string) string {
	if name == "cl" {
		return "claude"
	}
	return name
}

// bashModelCandidatesAt — модели вызовов агентов в команде оболочки. Вызовы
// ищет akit shellcmd: он видит и вложенные скрипты (bash -c, ssh, here-doc на
// вход оболочке). Команду, которую shellcmd не разобрал, оболочка не
// исполнит, если ошибка настоящая: модели ищутся в её тексте (--model, -m,
// присваивание переменной модели), чтобы ошибка разбора akit не пропустила
// неодобренную. readSettings читает файл --settings по пути из команды, nil —
// не прочесть; файла нет — текст команды, которая может его написать
func bashModelCandidatesAt(command, initialCWD string, files modelFiles, settingsAt func(string, bool) []modelCandidate, agentAt func(string, string, []string) []modelCandidate, logger core.Logger) []modelCandidate {
	calls, err := shellcmd.Calls(command)
	if calls == nil && err != nil {
		logger.Warn("model_guard: shellcmd не разобрал команду, модели ищутся в тексте", "error", err)
		return textModelCandidates(command)
	}
	var found []modelCandidate
	claude := false
	cwd := initialCWD
	cwdKnown := cwd != ""
	for i := range calls {
		call := &calls[i]
		if call.Name == "cd" || call.Name == "pushd" {
			var known bool
			previousKnown := cwdKnown
			cwd, known = changeDirKnown(call, cwd, homeForPaths())
			cwdKnown = known && (previousKnown || absoluteChangePath(call))
			continue
		}
		if call.Name == "env" {
			if dir, found, known := envChdir(call.Args, cwd, homeForPaths()); found {
				previousKnown := cwdKnown
				if known {
					cwd = dir
				}
				cwdKnown = known && (previousKnown || envChdirAbsolute(call.Args))
			}
			continue
		}
		isNpxClaude := call.Name == "npx" && npxClaudeCode(call.Args)
		if !slices.Contains(modelClients, call.Name) && !isNpxClaude {
			continue
		}
		callArgs := call.Args
		if isNpxClaude {
			callArgs = npxClaudeArgs(call.Args)
		}
		literalArgs := shellCallArgumentLiterals(call.Raw, call.Name)
		literalOffset := 0
		if isNpxClaude {
			for i, arg := range call.Args {
				if arg == "@anthropic-ai/claude-code" {
					literalOffset = i + 1
					break
				}
			}
		}
		args := make([]word, len(callArgs))
		for i, a := range callArgs {
			shellLiteral := i+literalOffset < len(literalArgs) && literalArgs[i+literalOffset]
			args[i] = word{text: a, literal: !strings.ContainsAny(a, "$`"), shellLiteral: shellLiteral, shellLiteralKnown: true, tilde: call.ArgTilde(i + literalOffset)}
		}
		dialect := clientDialect(call.Name)
		if isNpxClaude {
			dialect = "claude"
		}
		callExtraDirs := []string{}
		localFiles := files
		lookupCWD := cwd
		if !cwdKnown {
			lookupCWD = initialCWD
		}
		if localFiles.settings != nil {
			read := localFiles.settings
			localFiles.settings = func(path string, tilde shellcmd.Tilde, _ string) []byte { return read(path, tilde, cwd) }
		}
		if agentAt != nil {
			localFiles.agent = func(name string) []modelCandidate { return agentAt(name, lookupCWD, callExtraDirs) }
		}
		if dialect == "claude" {
			callExtraDirs = claudeAddedAgentDirs(args, cwd, homeForPaths())
			if settingsAt != nil {
				found = append(found, settingsAt(cwd, cwdKnown)...)
			}
		}
		callCandidates := candidatesFromArgs(dialect, args, localFiles)
		found = append(found, callCandidates...)
		if dialect == "claude" {
			claude = true
		}
	}
	if claude {
		found = append(found, envModelCandidates(command, calls, logger)...)
		found = append(found, writtenProtectedFileCandidates(command)...)
	}
	if err != nil {
		// вызовы найдены не все: вложенный скрипт, который akit не разобрал
		// (fish вне простого подмножества: and, set -x, подстановка), отдаёт
		// только вызов оболочки — модели ищутся в тексте этого скрипта (set
		// -x в fish -c не задаёт модель claude за ним), без него — команды
		text := command
		if nested, ok := errors.AsType[*shellcmd.NestedError](err); ok {
			text = nested.Script
		}
		logger.Warn("model_guard: shellcmd разобрал команду не целиком, модели ищутся и в тексте", "error", err)
		found = append(found, textModelCandidates(text)...)
	}
	if whichClaudeSubstitution.MatchString(command) {
		replaced := whichClaudeSubstitution.ReplaceAllString(command, "claude")
		found = append(found, textModelCandidates(replaced)...)
	}
	return found
}

var whichClaudeSubstitution = regexp.MustCompile(`\$\(\s*which\s+claude\s*\)`)

var protectedSettingsTarget = regexp.MustCompile(`(?i)(?:settings(?:\.local)?\.json|agents/[^\s"';&|<>]+\.md)`)

func protectedWriteTargets(command string) []string {
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		return nil
	}
	var targets []string
	addTarget := func(value string) {
		if protectedSettingsTarget.MatchString(value) || explicitSettingsTarget(command, value) {
			targets = append(targets, value)
		}
	}
	syntax.Walk(file, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.Stmt:
			for _, redirect := range n.Redirs {
				switch redirect.Op.String() {
				case ">", ">>", ">|", "&>", "&>>":
					if value, literal := literalValue(redirect.Word); literal {
						addTarget(value)
					}
				}
			}
		case *syntax.CallExpr:
			if len(n.Args) < 2 {
				return true
			}
			name, literal := literalValue(n.Args[0])
			if !literal || !slices.Contains([]string{"cp", "mv", "install", "tee"}, filepath.Base(name)) {
				return true
			}
			var positional []string
			for i := 1; i < len(n.Args); i++ {
				value, isLiteral := literalValue(n.Args[i])
				if !isLiteral {
					continue
				}
				if (value == "-t" || value == "--target-directory") && i+1 < len(n.Args) {
					target, ok := literalValue(n.Args[i+1])
					if ok {
						addTarget(target)
					}
					i++
					continue
				}
				if after, ok := strings.CutPrefix(value, "--target-directory="); ok {
					addTarget(after)
					continue
				}
				if strings.HasPrefix(value, "-") {
					continue
				}
				positional = append(positional, value)
			}
			if len(positional) == 0 {
				return true
			}
			if filepath.Base(name) == "tee" {
				for _, target := range positional {
					addTarget(target)
				}
			} else {
				target := positional[len(positional)-1]
				addTarget(target)
				if strings.HasSuffix(target, string(os.PathSeparator)) && len(positional) > 1 {
					addTarget(filepath.Join(target, filepath.Base(positional[len(positional)-2])))
				}
			}
		}
		return true
	})
	return targets
}

var settingsFileArgument = regexp.MustCompile(`(?:^|\s)--settings(?:=|\s+)("[^"]*"|'[^']*'|[^\s;&|]+)`)

func explicitSettingsTarget(command, target string) bool {
	target = filepath.Clean(strings.Trim(target, `"'`))
	for _, match := range settingsFileArgument.FindAllStringSubmatch(command, -1) {
		path := strings.Trim(match[1], `"'`)
		if strings.HasPrefix(strings.TrimSpace(path), "{") {
			continue
		}
		if filepath.Clean(path) == target {
			return true
		}
	}
	return false
}

func writtenProtectedFileCandidates(command string) []modelCandidate {
	if len(protectedWriteTargets(command)) == 0 {
		return nil
	}
	decoded := strings.ReplaceAll(command, "\\\"", "\"")
	decoded = strings.ReplaceAll(decoded, "\\\\n", "\n")
	var found []modelCandidate
	for _, match := range writtenModel.FindAllStringSubmatch(decoded, -1) {
		if match[1] == "model" {
			found = append(found, modelCandidate{value: match[2]})
		} else if c, ok := envCandidate(match[1], match[2], false); ok {
			found = append(found, c)
		}
	}
	for _, match := range regexp.MustCompile(`(?m)^\s*model:\s*([^\s#]+)`).FindAllStringSubmatch(decoded, -1) {
		found = append(found, modelCandidate{value: strings.Trim(match[1], `"'`), userOnlyCheck: true})
	}
	if len(found) == 0 {
		return []modelCandidate{{value: "written settings or agent contents", unknownWrite: true}}
	}
	return found
}

func envChdir(args []string, cwd, home string) (string, bool, bool) {
	for i := 0; i < len(args); i++ {
		var value string
		switch {
		case args[i] == "-C" || args[i] == "--chdir":
			if i+1 >= len(args) {
				return "", true, false
			}
			i++
			value = args[i]
		case strings.HasPrefix(args[i], "--chdir="):
			value = strings.TrimPrefix(args[i], "--chdir=")
		default:
			continue
		}
		path, known := knownPath(value, home)
		if !known {
			return "", true, false
		}
		return resolvePath(path, 0, cwd, home), true, true
	}
	return "", false, false
}

func envChdirAbsolute(args []string) bool {
	for i := 0; i < len(args); i++ {
		var value string
		if args[i] == "-C" || args[i] == "--chdir" {
			if i+1 < len(args) {
				i++
				value = args[i]
			}
		} else if after, ok := strings.CutPrefix(args[i], "--chdir="); ok {
			value = after
		}
		if value != "" {
			return filepath.IsAbs(value) || strings.HasPrefix(value, "~") || strings.HasPrefix(value, "$HOME")
		}
	}
	return false
}

func absoluteChangePath(call *shellcmd.Call) bool {
	i := 0
	for i < len(call.Args) && (call.Args[i] == "-P" || call.Args[i] == "-L") {
		i++
	}
	if i < len(call.Args) && call.Args[i] == "--" {
		i++
	}
	if i >= len(call.Args) {
		return call.Name == "cd"
	}
	path := call.Args[i]
	return filepath.IsAbs(path) || strings.HasPrefix(path, "~") || strings.HasPrefix(path, "$HOME") || strings.HasPrefix(path, "${HOME}")
}

func npxClaudeCode(args []string) bool {
	return slices.Contains(args, "@anthropic-ai/claude-code")
}

func npxClaudeArgs(args []string) []string {
	for i, arg := range args {
		if arg == "@anthropic-ai/claude-code" {
			return args[i+1:]
		}
	}
	return nil
}

func claudeAddedAgentDirs(args []word, cwd, home string) []string {
	var dirs []string
	for i := 0; i < len(args); i++ {
		var path string
		var tilde shellcmd.Tilde
		if (args[i].text == "--add-dir" || args[i].text == "--plugin-dir") && i+1 < len(args) {
			i++
			path, tilde = args[i].text, args[i].tilde
		} else if value, ok := strings.CutPrefix(args[i].text, "--add-dir="); ok {
			if strings.ContainsAny(value, "$`") {
				continue
			}
			path = value
		} else if value, ok := strings.CutPrefix(args[i].text, "--plugin-dir="); ok {
			if strings.ContainsAny(value, "$`") {
				continue
			}
			path = value
		} else {
			continue
		}
		text, known := knownPath(path, home)
		if !known {
			continue
		}
		resolved := resolvePath(text, tilde, cwd, home)
		dirs = append(dirs, resolved, filepath.Join(resolved, ".claude", "agents"))
	}
	return dirs
}

func homeForPaths() string { return os.Getenv("HOME") }

// modelClientWord — отдельное слово клиента из modelClients (не .claude/ и не claude-hooks)
var modelClientWord = regexp.MustCompile(`(^|[^[:alnum:]_.-])(claude|cl|codex|opencode|worker)([^[:alnum:]_.-]|$)`)

// envModelMention — присваивание переменной модели в тексте команды: A=x
// или set -x A x (fish)
var envModelMention = regexp.MustCompile(`\b(?:(` + modelEnvPattern() + `)=|set(?:\s+-\w+)*\s+(` + modelEnvPattern() + `)\s+)["']?([^\s"';&|]*)`)

// textModelCandidates — модели в тексте команды, которую не разобрала
// оболочка: --model, -m у агента и присваивания переменных модели, если в
// команде есть агент
func textModelCandidates(command string) []modelCandidate {
	if !modelClientWord.MatchString(command) {
		return nil
	}
	var found []modelCandidate
	for _, pattern := range []*regexp.Regexp{longModelMentionPattern, shortModelMentionPattern} {
		for _, m := range pattern.FindAllStringSubmatchIndex(command, -1) {
			if printedMention(command, m[0]) {
				continue
			}
			v := command[m[2]:m[3]]
			found = append(found, modelCandidate{value: v, dynamic: strings.ContainsAny(v, "$`")})
		}
	}
	return append(found, envTextCandidates(command)...)
}

// textKeywords слова перед командой в простой команде: and, or, not у fish,
// ключевые слова условий и циклов
var textKeywords = []string{"and", "or", "not", "begin", "if", "while", "then", "do", "else", "!", "{"}

// textPrinters команды, которые печатают свои слова, а не запускают их
var textPrinters = []string{"echo", "printf", "print"}

// printedMention — упоминание модели с позиции at стоит в простой команде,
// которая его печатает (echo claude --model x): текст, а не запуск. Простая
// команда — от последнего ; & | ( ` или перевода строки до at, без
// ключевых слов в начале
func printedMention(text string, at int) bool {
	segment := text[strings.LastIndexAny(text[:at], ";&|(`\n")+1 : at]
	words := strings.Fields(segment)
	for len(words) > 0 && slices.Contains(textKeywords, words[0]) {
		words = words[1:]
	}
	return len(words) > 0 && slices.Contains(textPrinters, words[0])
}

// envTextCandidates — присваивания переменных модели в тексте скрипта
func envTextCandidates(script string) []modelCandidate {
	var found []modelCandidate
	for _, m := range envModelMention.FindAllStringSubmatch(script, -1) {
		if c, ok := envCandidate(m[1]+m[2], m[3], strings.ContainsAny(m[3], "$`")); ok {
			found = append(found, c)
		}
	}
	return found
}

// modelEnvNames — переменные, задающие модель Claude Code (имена — из
// бинаря claude 2.1.330, приёмка 20.11). true — модель проверяется целиком;
// false — модель быстрых запросов (haiku): её Claude Code задаёт сам, она не
// одобряется списком и проверяется только на модель «только пользователь»
var modelEnvNames = map[string]bool{
	"ANTHROPIC_MODEL": true, "ANTHROPIC_DEFAULT_MODEL": true,
	"ANTHROPIC_DEFAULT_OPUS_MODEL": true, "ANTHROPIC_DEFAULT_SONNET_MODEL": true,
	"ANTHROPIC_DEFAULT_FABLE_MODEL": true, "CLAUDE_CODE_SUBAGENT_MODEL": true,
	"CLAUDE_CODE_WORKFLOW_SUBAGENT_MODEL": true,
	"ANTHROPIC_DEFAULT_HAIKU_MODEL":       false, "ANTHROPIC_SMALL_FAST_MODEL": false,
	"CLAUDE_CODE_AUTO_MODE_MODEL": false, "CLAUDE_CODE_BG_CLASSIFIER_MODEL": false,
}

// modelEnvPattern — имена modelEnvNames альтернативой регулярного выражения
func modelEnvPattern() string {
	names := slices.Sorted(maps.Keys(modelEnvNames))
	return strings.Join(names, "|")
}

// envCandidate — кандидат из присваивания value переменной name; не
// переменная модели или пустое значение — нет кандидата
func envCandidate(name, value string, dynamic bool) (modelCandidate, bool) {
	full, ok := modelEnvNames[name]
	if !ok || value == "" {
		return modelCandidate{}, false
	}
	return modelCandidate{value: value, dynamic: dynamic, userOnlyCheck: !full}, true
}

// envModelCandidates — модели из присваиваний переменных modelEnvNames, что
// действуют на claude: префиксом вызова, за которым (и за обёртками вроде
// timeout) claude или оболочка, присваиванием env перед claude,
// export/declare и внутри скриптов оболочки (bash -c, ssh, here-doc,
// исполняемый оболочкой). Слово с именем переменной в промпте, в теле
// here-doc для файла или у другой команды — не присваивание для claude
// (ревью 12.11). shellcmd присваиваний не отдаёт (тикет akit
// shellcmd-assignments): их читает разбор mvdan/sh, а что исполнится за
// присваиванием — вызовы shellcmd calls того же скрипта. Вложенный скрипт —
// часть внешнего, рекурсия кончается
func envModelCandidates(script string, calls []shellcmd.Call, logger core.Logger) []modelCandidate {
	file, err := syntax.NewParser().Parse(strings.NewReader(script), "")
	if err != nil {
		// shellcmd скрипт разобрал, mvdan/sh — нет: присваиваний по разбору
		// не видно, они ищутся в тексте
		logger.Warn("model_guard: mvdan/sh не разобрал скрипт, присваивания модели ищутся в тексте", "error", err)
		return envTextCandidates(script)
	}
	var found []modelCandidate
	syntax.Walk(file, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.DeclClause:
			// export, declare и родня: присваивания уходят в окружение следующих команд
			found = append(found, assignedModels(script, n.Args)...)
		case *syntax.CallExpr:
			if len(n.Args) > 0 {
				found = append(found, callEnvModels(script, n, calls, logger)...)
			} else {
				// A=x; export A — присваивание без команды уходит в окружение
				// после export
				found = append(found, assignedModels(script, n.Assigns)...)
			}
		}
		return true
	})
	// тело here-doc, в котором shellcmd нашёл claude, исполняет оболочка;
	// снятая обёртка несёт те же перенаправления — тело читается один раз
	bodies := map[int]bool{}
	claudeOffsets := make([]int, 0, len(calls))
	for i := range calls {
		if clientDialect(calls[i].Name) == "claude" {
			claudeOffsets = append(claudeOffsets, calls[i].Offset)
		}
	}
	slices.Sort(claudeOffsets)
	for i := range calls {
		c := &calls[i]
		for _, r := range c.Redirs {
			j, _ := slices.BinarySearch(claudeOffsets, r.BodyStart)
			foundBody := j < len(claudeOffsets) && claudeOffsets[j] < r.BodyEnd
			if r.BodyStart < 0 || bodies[r.BodyStart] || !foundBody {
				continue
			}
			bodies[r.BodyStart] = true
			found = append(found, nestedEnvModels(r.Body, logger)...)
		}
	}
	return found
}

// callEnvModels — модели из присваиваний простого вызова n: префикс и
// присваивания env, если за ними исполнится claude или оболочка, и
// присваивания в скриптах оболочек этого вызова. Что исполнится — вызовы
// shellcmd с тем же исходным текстом в границах n: сам вызов и снятые с него
// обёртки
func callEnvModels(script string, n *syntax.CallExpr, calls []shellcmd.Call, logger core.Logger) []modelCandidate {
	start, end := int(n.Args[0].Pos().Offset()), int(n.End().Offset())
	head := -1
	for i := range calls {
		if calls[i].Offset == start {
			head = i
			break
		}
	}
	if head < 0 {
		return nil
	}
	var chain []shellcmd.Call
	for i := range calls {
		c := &calls[i]
		if c.Raw == calls[head].Raw && start <= c.Offset && c.Offset < end {
			chain = append(chain, *c)
		}
	}
	var found []modelCandidate
	runs := false
	for i := range chain {
		c := &chain[i]
		if clientDialect(c.Name) == "claude" || isShellName(c.Name) {
			runs = true
			break
		}
	}
	if runs {
		found = append(found, assignedModels(script, n.Assigns)...)
	}
	for i := range chain {
		c := &chain[i]
		switch {
		case isShellName(c.Name):
			for _, a := range c.Args {
				found = append(found, nestedEnvModels(a, logger)...)
			}
		case runs && i+1 < len(chain):
			// env A=x claude, sudo A=x claude, timeout 5 sudo A=x claude:
			// слова снятых обёрток до следующей команды цепочки и её имя —
			// опции и присваивания (shellcmd отдаёт голову цепочки и снятую
			// команду, промежуточные обёртки — словами головы). За sudo --
			// присваивание shellcmd отдаёт вызовом с ним вместо имени
			// (sudo -- A=x claude, A=a/x — именем x): оно — имя следующей
			opts := c.Args[:max(0, len(c.Args)-len(chain[i+1].Args))]
			for _, a := range opts {
				found = append(found, assignedModelWord(a)...)
			}
		}
	}
	return found
}

// nestedEnvModels — модели из присваиваний во вложенном скрипте
func nestedEnvModels(script string, logger core.Logger) []modelCandidate {
	calls, err := shellcmd.Calls(script)
	if calls == nil && err != nil {
		return nil
	}
	return envModelCandidates(script, calls, logger)
}

// isShellName — программа исполняет свои аргументы или вход как команды
// оболочки: оболочка по shellcmd или ssh
func isShellName(name string) bool {
	return shellcmd.IsShell(name) || name == "ssh"
}

// assignedModels — значения присваиваний modelEnvNames
func assignedModels(script string, assigns []*syntax.Assign) []modelCandidate {
	var found []modelCandidate
	for _, a := range assigns {
		if a.Name == nil || a.Value == nil {
			continue
		}
		value, literal := literalValue(a.Value)
		if !literal {
			value = script[a.Value.Pos().Offset():a.Value.End().Offset()]
		}
		if c, ok := envCandidate(a.Name.Value, value, !literal); ok {
			found = append(found, c)
		}
	}
	return found
}

// assignedModelWord — значение присваивания modelEnvNames в слове из
// аргументов shellcmd (env A=x)
func assignedModelWord(w string) []modelCandidate {
	name, value, ok := strings.Cut(w, "=")
	if !ok {
		return nil
	}
	if c, ok := envCandidate(name, value, strings.ContainsAny(value, "$`")); ok {
		return []modelCandidate{c}
	}
	return nil
}

// valueFlags — флаги Claude, которые model_guard анализирует как пару.
var valueFlags = []string{"--settings", "--agents", "--agent", "--fallback-model"}

// splitValueFlags разбивает --flag=value для флагов Claude со значением.
func splitValueFlags(args []word) []word {
	out := make([]word, 0, len(args))
	for _, a := range args {
		if flag, value, ok := strings.Cut(a.text, "="); ok && slices.Contains(valueFlags, flag) {
			out = append(out, word{text: flag, literal: true, shellLiteral: true, shellLiteralKnown: true}, word{text: value, literal: a.literal, shellLiteral: a.shellLiteral, shellLiteralKnown: a.shellLiteralKnown, tilde: a.tilde})
			continue
		}
		out = append(out, a)
	}
	return out
}

func claudeArgIsLiteral(arg word) bool {
	if arg.shellLiteralKnown {
		return arg.shellLiteral
	}
	return arg.literal
}

func shellCallArgumentLiterals(raw, commandName string) []bool {
	file, err := syntax.NewParser().Parse(strings.NewReader(raw), "")
	if err != nil {
		return nil
	}
	var flags []bool
	syntax.Walk(file, func(node syntax.Node) bool {
		if len(flags) != 0 {
			return false
		}
		call, ok := node.(*syntax.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		commandWord := call.Args[0]
		start, end := int(commandWord.Pos().Offset()), int(commandWord.End().Offset())
		if start < 0 || end < start || end > len(raw) || filepath.Base(strings.Trim(raw[start:end], "'\"")) != commandName {
			return true
		}
		flags = make([]bool, len(call.Args)-1)
		for i, arg := range call.Args[1:] {
			start, end := int(arg.Pos().Offset()), int(arg.End().Offset())
			if start >= 0 && end >= start && end <= len(raw) {
				flags[i] = shellWordSourceLiteral(raw[start:end])
			}
		}
		return false
	})
	return flags
}

func shellWordSourceLiteral(source string) bool {
	var quote byte
	for i := 0; i < len(source); i++ {
		ch := source[i]
		if quote == '\'' {
			if ch == '\'' {
				quote = 0
			}
			continue
		}
		if quote == '"' {
			switch ch {
			case '"':
				quote = 0
			case '$', '`':
				return false
			case '\\':
				i++
			}
			continue
		}
		switch ch {
		case '\\':
			i++
		case '\'', '"':
			quote = ch
		case '$', '`', '*', '?', '[', '{', '}', '(':
			return false
		case '<', '>':
			if i+1 < len(source) && source[i+1] == '(' {
				return false
			}
		}
	}
	return quote == 0
}

// modelValueFlag — флаг клиента name, значение которого в следующем слове —
// модель
func modelValueFlag(name, flag string) bool {
	switch flag {
	case "--model":
		return true
	case "-m":
		return name == "codex" || name == "opencode"
	case "-model":
		return name == "worker"
	case "--fallback-model":
		return name == "claude"
	}
	return false
}

func candidatesFromArgs(name string, args []word, files modelFiles) []modelCandidate {
	var found []modelCandidate
	inlineAgents := inlineAgentModels(args)
	if name == "claude" {
		args = splitValueFlags(args)
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if name == "opencode" || name == "worker" {
			if value, ok := strings.CutPrefix(a.text, "-m="); ok && name == "opencode" {
				found = append(found, modelCandidate{value: value, dynamic: !a.literal})
				continue
			}
			if value, ok := strings.CutPrefix(a.text, "-model="); ok && name == "worker" {
				found = append(found, modelCandidate{value: value, dynamic: !a.literal})
				continue
			}
		}
		if name == "codex" {
			if value, ok := strings.CutPrefix(a.text, "-m="); ok {
				found = append(found, modelCandidate{value: value, dynamic: !a.literal})
				continue
			}
			if value, ok := strings.CutPrefix(a.text, "--config="); ok {
				if candidate, ok := codexConfigCandidate(word{text: value, literal: a.literal}); ok {
					found = append(found, candidate)
				}
				continue
			}
		}
		if after, ok := strings.CutPrefix(a.text, "--model="); ok {
			found = append(found, modelCandidate{value: after, dynamic: !a.literal})
			continue
		}
		if i+1 >= len(args) {
			continue
		}
		next := args[i+1]
		switch {
		case modelValueFlag(name, a.text):
			found = append(found, modelCandidate{value: next.text, dynamic: !next.literal})
		case name == "claude" && (a.text == "--settings" || a.text == "--agents"):
			found = append(found, jsonModelCandidates(a.text, next, files.settings, files.agent)...)
		case name == "claude" && a.text == "--agent":
			found = append(found, inlineAgents[next.text]...)
			definitions := agentFlagCandidates(next, files)
			if len(inlineAgents[next.text]) > 0 {
				definitions = slices.DeleteFunc(definitions, func(candidate modelCandidate) bool { return candidate.unknownAgent })
			}
			found = append(found, definitions...)
		case name == "codex" && (a.text == "-c" || a.text == "--config"):
			if candidate, ok := codexConfigCandidate(next); ok {
				found = append(found, candidate)
			}
		default:
			continue
		}
		i++
	}
	return found
}

func inlineAgentModels(args []word) map[string][]modelCandidate {
	models := map[string][]modelCandidate{}
	args = splitValueFlags(args)
	for i := 0; i+1 < len(args); i++ {
		if args[i].text != "--agents" || !args[i+1].literal {
			continue
		}
		if parsed, ok := parseAgentsJSON([]byte(args[i+1].text)); ok {
			for name, candidates := range parsed {
				models[name] = append(models[name], candidates...)
			}
		}
	}
	return models
}

func parseAgentsJSON(raw []byte) (map[string][]modelCandidate, bool) {
	var agents map[string]json.RawMessage
	if json.Unmarshal(raw, &agents) != nil || agents == nil {
		return nil, false
	}
	models := make(map[string][]modelCandidate, len(agents))
	for name, rawAgent := range agents {
		var agent struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(rawAgent, &agent) == nil && strings.TrimSpace(agent.Model) != "" && agent.Model != "inherit" {
			models[name] = append(models[name], modelCandidate{value: agent.Model})
		}
	}
	return models, true
}

func flattenAgentModels(models map[string][]modelCandidate) []modelCandidate {
	var found []modelCandidate
	for _, name := range slices.Sorted(maps.Keys(models)) {
		found = append(found, models[name]...)
	}
	return found
}

// jsonModelKey — ключ модели в исходном тексте JSON, в том числе с
// экранированными кавычками внутри "…": "model": и \"ANTHROPIC_MODEL\":
var jsonModelKey = regexp.MustCompile(`\b(?:model|[A-Z_]+_MODEL)\\*"\s*:`)

// writtenModel — модель в тексте команды, которая пишет файл настроек:
// "model":"x" и "ANTHROPIC_MODEL":"x"
var writtenModel = regexp.MustCompile(`"(model|` + modelEnvPattern() + `)"\s*:\s*"([^"]*)"`)

// jsonModelCandidates — модели в JSON флага claude: --settings (строка JSON
// или файл: model и env.ANTHROPIC_MODEL…) и --agents (model каждого агента).
// Файла --settings ещё нет — модели в тексте команды, которая его пишет.
// Не JSON — моделей нет: такой запуск claude сам не начнётся
func jsonModelCandidates(flag string, arg word, readSettings func(string, shellcmd.Tilde, string) []byte, agent func(string) []modelCandidate) []modelCandidate {
	if flag == "--agents" {
		inlineJSON := strings.HasPrefix(strings.TrimSpace(arg.text), "{")
		if !arg.literal || (!inlineJSON && !claudeArgIsLiteral(arg)) {
			return []modelCandidate{{value: arg.text, unknownAgents: true}}
		}
		raw := []byte(arg.text)
		if !inlineJSON {
			if arg.tilde&(shellcmd.TildeOther|shellcmd.TildeModeUnknown) != 0 {
				return []modelCandidate{{value: arg.text, unknownAgents: true}}
			}
			raw = readSettings(arg.text, arg.tilde, "")
			if raw == nil {
				return []modelCandidate{{value: arg.text, unknownAgents: true}}
			}
		}
		models, ok := parseAgentsJSON(raw)
		if !ok && !inlineJSON {
			return []modelCandidate{{value: arg.text, unknownAgents: true}}
		}
		return flattenAgentModels(models)
	}
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
		if arg.tilde&(shellcmd.TildeOther|shellcmd.TildeModeUnknown) != 0 {
			return []modelCandidate{{value: arg.text, dynamic: true}}
		}
		if raw = readSettings(arg.text, arg.tilde, ""); raw == nil {
			return nil
		}
	}
	return settingsCandidates(raw, agent)
}

func settingsCandidates(raw []byte, agent func(string) []modelCandidate) []modelCandidate {
	found := (jsonModels{}).settings(raw)
	if agent != nil {
		var fields map[string]json.RawMessage
		var name string
		if json.Unmarshal(raw, &fields) == nil && json.Unmarshal(fields["agent"], &name) == nil && name != "" {
			found = append(found, agent(name)...)
		}
	}
	return found
}

// jsonModels — модели из JSON флага claude
type jsonModels []modelCandidate

func (found *jsonModels) add(model string) {
	// inherit — модель агента из основного вызова, она проверена отдельно
	if strings.TrimSpace(model) != "" && model != "inherit" {
		*found = append(*found, modelCandidate{value: model})
	}
}

// addField — поле настроек: model или переменная модели из env
func (found *jsonModels) addField(field, value string) {
	if field == "model" {
		found.add(value)
	} else if c, ok := envCandidate(field, value, false); ok {
		*found = append(*found, c)
	}
}

// settings — модели настроек --settings: model и переменные модели в env
func (found jsonModels) settings(raw []byte) []modelCandidate {
	// Поля разбираются по одному: поле не того типа рядом не прячет модель
	var settings map[string]json.RawMessage
	if json.Unmarshal(raw, &settings) != nil {
		if !json.Valid(raw) {
			// Текст команды вместо файла, которого ещё нет
			for _, m := range writtenModel.FindAllSubmatch(raw, -1) {
				found.addField(string(m[1]), string(m[2]))
			}
		}
		return found
	}
	var model string
	if json.Unmarshal(settings["model"], &model) == nil {
		found.add(model)
	}
	var agent struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(settings["agent"], &agent) == nil {
		found.add(agent.Model)
	}
	var env map[string]json.RawMessage
	if json.Unmarshal(settings["env"], &env) == nil {
		for name, raw := range env {
			var value string
			if json.Unmarshal(raw, &value) == nil {
				found.addField(name, value)
			}
		}
	}
	return found
}

func codexConfigCandidate(config word) (modelCandidate, bool) {
	if !strings.HasPrefix(config.text, "model=") {
		return modelCandidate{}, false
	}
	value, ok := strings.CutPrefix(config.text, "model=")
	if !ok {
		return modelCandidate{}, false
	}
	return modelCandidate{value: value, dynamic: !config.literal}, true
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

// userOnlyModel — имя модели из userOnly, которой отвечает кандидат: по имени
// или id её строки в списке одобренных
func (t *ModelGuardTool) userOnlyModel(candidate string, models []approvedModel) (string, bool) {
	for _, m := range models {
		if slices.ContainsFunc(t.userOnly, func(name string) bool { return strings.EqualFold(name, m.name) }) &&
			isApproved(candidate, []approvedModel{m}) {
			return m.name, true
		}
	}
	name := normalizeModel(candidate)
	for _, restricted := range t.userOnly {
		if strings.EqualFold(name, restricted) {
			return restricted, true
		}
		for _, token := range strings.FieldsFunc(name, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
			if strings.EqualFold(token, restricted) {
				return restricted, true
			}
		}
	}
	return "", false
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

func (t *ModelGuardTool) reject(reason string, models []approvedModel) *core.ValidationResult {
	var list []string
	for _, m := range models {
		list = append(list, fmt.Sprintf("%s (%s)", m.name, m.id))
	}
	message := reason
	if len(list) > 0 {
		message += "; одобрены: " + strings.Join(list, ", ")
	}
	v := core.NewViolation("model_guard", message, "", core.LevelCritical, 0, 0)
	return core.NewValidationResult(false, []core.Violation{v}, []string{modelGuardHint})
}
