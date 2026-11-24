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
	"github.com/aiseeq/chooks/internal/hookctx"
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
}

// NewModelGuardTool создаёт проверку выбранной модели.
func NewModelGuardTool(config core.ToolConfig, logger core.Logger) (*ModelGuardTool, error) {
	return &ModelGuardTool{
		BaseTool: NewBaseTool("model_guard", config.Enabled, append([]string{"Agent"}, core.ShellTools...), logger),
	}, nil
}

// ValidateTool проверяет модель лишь у вызовов, которые её явно задают.
func (t *ModelGuardTool) ValidateTool(ctx context.Context, input *core.ToolInput) (*core.ValidationResult, error) {
	if _, ok := hookctx.Request(ctx); !ok {
		return nil, fmt.Errorf("hook request is missing")
	}
	if !t.IsEnabled() || input == nil {
		return &core.ValidationResult{IsValid: true}, nil
	}
	if result := t.validateShellPrefix(input); result != nil {
		return result, nil
	}
	candidates := modelCandidates(ctx, input, t.Logger())
	if len(candidates) == 0 {
		return &core.ValidationResult{IsValid: true}, nil
	}
	path := t.approvedPath(ctx)
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
		if c.unknownSettings {
			reasons = append(reasons, "файл --settings не прочитать: модель не проверить")
			continue
		}
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

func (t *ModelGuardTool) validateShellPrefix(input *core.ToolInput) *core.ValidationResult {
	if !core.IsShellTool(input.ToolName) {
		return nil
	}
	reason, err := shellPrefixViolation(input.Command, t.Logger())
	if err != nil {
		t.Logger().Warn("model_guard: значение префикса хуков не удалось проверить", "error", err)
		return t.rejectPrefix(shellPrefixFailure)
	}
	if reason != "" {
		return t.rejectPrefix(reason)
	}
	return nil
}

const shellPrefixFailure = "Claude Code запускает хуки через этот префикс одним словом; не путь к исполняемому файлу — все хуки молча падают со статусом 127"

const executableMode = 0111

func shellPrefixViolation(command string, logger core.Logger) (string, error) {
	values, dynamic := shellEnvValues(command, "CLAUDE_CODE_SHELL_PREFIX", logger)
	if dynamic {
		return shellPrefixFailure + "; назови литералом", nil
	}
	for _, value := range values {
		if value == "" {
			continue
		}
		if strings.HasPrefix(value, "~/") {
			value = filepath.Join(os.Getenv("HOME"), value[2:])
		}
		if !filepath.IsAbs(value) {
			return shellPrefixFailure, nil
		}
		info, err := os.Stat(value)
		if err != nil {
			return "", fmt.Errorf("stat hook shell prefix %q: %w", value, err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&executableMode == 0 {
			return shellPrefixFailure, nil
		}
	}
	return "", nil
}

func (t *ModelGuardTool) rejectPrefix(reason string) *core.ValidationResult {
	v := core.NewViolation("model_guard", reason, "", core.LevelCritical, 0, 0)
	return core.NewValidationResult(false, []core.Violation{v}, nil)
}

// CandidateModels exposes the model values found by the same parser used for
// validation, for decision logging by runtime guards.
func (t *ModelGuardTool) CandidateModels(ctx context.Context, input *core.ToolInput) []string {
	candidates := modelCandidates(ctx, input, t.Logger())
	values := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		values = append(values, candidate.value)
	}
	return values
}

type modelCandidate struct {
	value           string
	dynamic         bool
	unknownAgent    bool
	unknownWrite    bool
	unknownAgents   bool
	unknownSettings bool
}

func modelCandidates(ctx context.Context, input *core.ToolInput, logger core.Logger) []modelCandidate {
	if input.ToolName != "Agent" && !core.IsShellTool(input.ToolName) {
		return nil
	}
	request, _ := hookctx.Request(ctx)
	if input.CWD == "" {
		input.CWD = request.Cwd
	}
	home, err := hookctx.UserHomeDir(ctx)
	if err != nil {
		logger.Warn("model_guard: домашний каталог неизвестен", "error", err)
		return []modelCandidate{{value: "HOME", dynamic: true}}
	}
	agentModels := func(name string) []modelCandidate {
		return agentDefinitionModels(ctx, name, input.CWD, home, nil, logger, false)
	}
	if input.ToolName == "Agent" {
		return agentToolCandidates(input, agentModels, logger)
	}
	cwdCandidates := commandCWDs(input.Command, input.CWD, []string{home}, logger)
	homes := commandPathCandidates(input.Command, "HOME", cwdCandidates, []string{home}, home, logger)
	cwdCandidates = commandCWDs(input.Command, input.CWD, homes.paths, logger)
	homes = commandPathCandidates(input.Command, "HOME", cwdCandidates, []string{home}, home, logger)
	configs := commandPathCandidates(input.Command, "CLAUDE_CONFIG_DIR", cwdCandidates, nonEmpty(claudeConfigDir(ctx)), home, logger)
	unresolvedTildeSettings := false
	readSettings := func(path string, tilde shellcmd.Tilde, _ string) [][]byte {
		var contents [][]byte
		for _, dir := range cwdCandidates {
			for _, commandHome := range homes.paths {
				abs := absPath(tildePath(path, tilde, commandHome), dir)
				if abs == "" {
					continue
				}
				data, err := readSettingsFile(abs)
				if err == nil {
					contents = append(contents, data)
				} else if !errors.Is(err, os.ErrNotExist) {
					logger.Warn("model_guard: файл --settings не читается, модель из него не проверена", "path", abs, "error", err)
				}
			}
		}
		// Проверяем текст команды вместе с файлами: запись может обновлять
		// уже существующий settings.json через dd, sponge и другие команды.
		if writtenModel.Match([]byte(input.Command)) || writtenOverrides.Match([]byte(input.Command)) {
			contents = append(contents, []byte(input.Command))
		}
		if len(contents) == 0 {
			unresolvedTildeSettings = true
			return nil
		}
		return contents
	}
	settingsAt := func(cwd string, includeProject bool) []modelCandidate {
		return settingsForCWD(ctx, cwd, homes, configs, logger, includeProject)
	}
	candidates := bashModelCandidatesAt(input.Command, input.CWD, home, cwdCandidates, homes, configs, modelFiles{settings: readSettings, agent: agentModels}, settingsAt, func(name, cwd string, extra []string) []modelCandidate {
		var found []modelCandidate
		for _, commandHome := range homes.paths {
			agentExtra := append([]string(nil), extra...)
			for _, configDir := range configs.paths {
				agentExtra = append(agentExtra, filepath.Join(configDir, "agents"))
				agentExtra = append(agentExtra, installedPluginAgentDirs(filepath.Join(configDir, "plugins", "installed_plugins.json"), logger)...)
			}
			agentExtra = append(agentExtra, filepath.Join(commandHome, ".claude", "agents"))
			found = append(found, agentDefinitionModels(ctx, name, cwd, commandHome, agentExtra, logger, true)...)
		}
		if homes.dynamic || configs.dynamic {
			found = append(found, modelCandidate{value: "HOME or CLAUDE_CONFIG_DIR", dynamic: true})
		}
		return found
	}, logger)
	if unresolvedTildeSettings {
		candidates = append(candidates, modelCandidate{value: "settings path with unknown home", dynamic: true})
	}
	return candidates
}

func settingsForCWD(ctx context.Context, cwd string, homes, configs pathSet, logger core.Logger, includeProject bool) []modelCandidate {
	var paths []string
	if includeProject {
		paths = append(paths, projectSettingsPaths(cwd)...)
	}
	for _, home := range homes.paths {
		paths = append(paths, filepath.Join(home, ".claude", "settings.json"))
	}
	paths = append(paths, managedSettingsPath)
	var agentDirsForSettings []string
	for _, configDir := range configs.paths {
		paths = append(paths, filepath.Join(configDir, "settings.json"))
		agentDirsForSettings = append(agentDirsForSettings, filepath.Join(configDir, "agents"))
		agentDirsForSettings = append(agentDirsForSettings, installedPluginAgentDirs(filepath.Join(configDir, "plugins", "installed_plugins.json"), logger)...)
	}
	for _, home := range homes.paths {
		agentDirsForSettings = append(agentDirsForSettings, filepath.Join(home, ".claude", "agents"))
	}
	lookupAgent := func(name string) []modelCandidate {
		var found []modelCandidate
		for _, home := range homes.paths {
			lookupCWD := cwd
			if !includeProject {
				lookupCWD = ""
			}
			found = append(found, agentDefinitionModels(ctx, name, lookupCWD, home, agentDirsForSettings, logger, true)...)
		}
		return found
	}
	var candidates []modelCandidate
	if homes.dynamic || configs.dynamic {
		candidates = append(candidates, modelCandidate{value: "HOME or CLAUDE_CONFIG_DIR", dynamic: true})
	}
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

type pathSet struct {
	paths   []string
	dynamic bool
}

func nonEmpty(value string) []string {
	if value == "" {
		return nil
	}
	return []string{value}
}

func commandPathCandidates(command, name string, cwds, defaults []string, defaultHome string, logger core.Logger) pathSet {
	set := pathSet{}
	add := func(path string) {
		if path != "" && !slices.Contains(set.paths, path) {
			set.paths = append(set.paths, path)
		}
	}
	for _, value := range defaults {
		add(filepath.Clean(value))
	}
	values, dynamic := shellEnvValues(command, name, logger)
	set.dynamic = dynamic
	for _, value := range values {
		if strings.ContainsAny(value, "$`*?[") {
			set.dynamic = true
			continue
		}
		for _, cwd := range cwds {
			if after, ok := strings.CutPrefix(value, "~/"); ok {
				value = filepath.Join(defaultHome, after)
			}
			add(resolvePath(value, 0, cwd, defaultHome))
		}
	}
	return set
}

func shellEnvValues(command, name string, logger core.Logger) ([]string, bool) {
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		logger.Warn("model_guard: присваивания пути не разобраны", "name", name, "error", err)
		// bash исполнит разобранное начало (незакрытый here-doc): имя в
		// неразобранной команде — «не проверить», без имени присваивания нет
		return nil, strings.Contains(command, name)
	}
	var values []string
	dynamic := false
	callValues, callDynamic, callErr := shellEnvCallValues(command, name)
	values = append(values, callValues...)
	dynamic = dynamic || callDynamic
	if callErr != nil {
		logger.Warn("model_guard: обёртки shell-присваиваний не разобраны", "name", name, "error", callErr)
		dynamic = true
	}
	nestedValues, nestedDynamic := shellNestedEnvValues(file, name, logger)
	values = append(values, nestedValues...)
	dynamic = dynamic || nestedDynamic
	calls, callsErr := shellcmd.Calls(command)
	if callsErr != nil {
		logger.Warn("model_guard: вложенные вызовы оболочки не полностью разобраны", "error", callsErr)
		if nested, ok := errors.AsType[*shellcmd.NestedError](callsErr); ok && strings.Contains(nested.Script, name) {
			dynamic = true
		}
	}
	for i := range calls {
		c := &calls[i]
		var scripts []string
		if shellcmd.IsShell(c.Name) {
			scripts = shellCommandStrings(c.Args)
		} else if c.Name == "script" {
			scripts = scriptCommandStrings(c.Args)
		}
		for _, nested := range scripts {
			found, unknown := shellEnvValues(nested, name, logger)
			values = append(values, found...)
			dynamic = dynamic || unknown
		}
	}
	declaredValues, declaredDynamic := shellEnvDeclarations(file, command, name)
	values = append(values, declaredValues...)
	dynamic = dynamic || declaredDynamic
	return values, dynamic
}

func shellNestedEnvValues(file *syntax.File, name string, logger core.Logger) ([]string, bool) {
	var values []string
	dynamic := false
	syntax.Walk(file, func(node syntax.Node) bool {
		call, ok := node.(*syntax.CallExpr)
		if !ok || len(call.Args) < 2 {
			return true
		}
		cmd, literal := literalValue(call.Args[0])
		if !literal || !shellcmd.IsShell(filepath.Base(cmd)) {
			return true
		}
		for _, arg := range call.Args[1:] {
			value, literal := literalValue(arg)
			if !literal || !strings.Contains(value, name) {
				continue
			}
			nested, nestedDynamic := shellEnvValues(value, name, logger)
			values = append(values, nested...)
			dynamic = dynamic || nestedDynamic
		}
		return true
	})
	return values, dynamic
}

func shellEnvDeclarations(file *syntax.File, command, name string) ([]string, bool) {
	var values []string
	dynamic := false
	add := func(key, value string, literal, selfReference bool) {
		if key != name {
			return
		}
		if !literal {
			if !selfReference {
				dynamic = true
			}
			return
		}
		values = append(values, value)
	}
	addAssign := func(assign *syntax.Assign) {
		if assign == nil || assign.Name == nil || assign.Value == nil {
			return
		}
		value, literal := literalValue(assign.Value)
		add(assign.Name.Value, value, literal, name == "HOME" && homeSelfReference(command, assign.Value))
	}
	syntax.Walk(file, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.CallExpr:
			dynamic = shellEnvCallDeclaration(n, command, name, add, addAssign) || dynamic
		case *syntax.DeclClause:
			if !declarers[n.Variant.Value] {
				return true
			}
			for _, assign := range n.Args {
				addAssign(assign)
			}
		}
		return true
	})
	return values, dynamic
}

func shellEnvCallDeclaration(call *syntax.CallExpr, command, name string, add func(string, string, bool, bool), addAssign func(*syntax.Assign)) bool {
	for _, assign := range call.Assigns {
		addAssign(assign)
	}
	if len(call.Args) == 0 {
		return false
	}
	cmd, ok := literalValue(call.Args[0])
	if !ok || !declarers[cmd] || cmd == "local" {
		return false
	}
	dynamic := false
	for _, arg := range call.Args[1:] {
		value, literal := literalValue(arg)
		if literal {
			key, val, assigned := strings.Cut(value, "=")
			if assigned {
				add(key, val, true, false)
			}
			continue
		}
		start, end := int(arg.Pos().Offset()), int(arg.End().Offset())
		if start >= 0 && end >= start && end <= len(command) && strings.HasPrefix(command[start:end], name+"=") && !(name == "HOME" && homeSelfReference(command, arg)) {
			dynamic = true
		}
	}
	return dynamic
}

func shellEnvCallValues(command, name string) ([]string, bool, error) {
	calls, err := shellcmd.Calls(command)
	via := shellEnvWrapperVia(calls)
	if !via["env"] && !via["sudo"] && !via["systemd-run"] {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("shellcmd calls: %w", err)
	}
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		return nil, false, fmt.Errorf("parse shell command: %w", err)
	}
	var values []string
	dynamic := false
	syntax.Walk(file, func(node syntax.Node) bool {
		call, ok := node.(*syntax.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		found, unknown := shellEnvAssignmentsForCall(call, command, name, via)
		values = append(values, found...)
		dynamic = dynamic || unknown
		return true
	})
	return values, dynamic, nil
}

func shellEnvWrapperVia(calls []shellcmd.Call) map[string]bool {
	via := map[string]bool{}
	for i := range calls {
		call := &calls[i]
		base := filepath.Base(call.Name)
		if base == "env" || base == "sudo" || base == "systemd-run" {
			via[base] = true
		}
		for _, wrapper := range call.Via {
			via[wrapper] = true
		}
	}
	return via
}

func shellEnvAssignmentsForCall(call *syntax.CallExpr, source, name string, via map[string]bool) ([]string, bool) {
	cmd, literal := literalValue(call.Args[0])
	if !literal {
		return nil, false
	}
	base, args := filepath.Base(cmd), call.Args[1:]
	var values []string
	dynamic := false
	if base != "env" && via[base] && via["env"] {
		if found, unknown := nestedEnvAssignments(args, source, name); len(found) > 0 || unknown {
			values = append(values, found...)
			dynamic = dynamic || unknown
		}
	}
	switch base {
	case "env":
		words := shellWordsFromAST(args, source)
		found, unknown := envCallAssignments(words, name)
		values = append(values, found...)
		dynamic = dynamic || unknown
	case "sudo":
		if via["sudo"] {
			for _, assignment := range sudoAssignments(args) {
				if value, ok := selectedEnvValue(assignment, name); ok {
					values = append(values, value)
				}
			}
		}
	case "systemd-run":
		if via["systemd-run"] {
			for _, assignment := range systemdRunAssignments(args) {
				if value, ok := selectedEnvValue(assignment, name); ok {
					values = append(values, value)
				}
			}
		}
	}
	return values, dynamic
}

func nestedEnvAssignments(args []*syntax.Word, source, name string) ([]string, bool) {
	for i, arg := range args {
		word, ok := literalValue(arg)
		if !ok || filepath.Base(word) != "env" {
			continue
		}
		return envCallAssignments(shellWordsFromAST(args[i+1:], source), name)
	}
	return nil, false
}

func shellWordsFromAST(args []*syntax.Word, source string) []string {
	words := make([]string, len(args))
	for i, arg := range args {
		word, ok := literalValue(arg)
		if !ok {
			word = shellWordText(source, arg)
		}
		words[i] = word
	}
	return words
}

func selectedEnvValue(assignment, name string) (string, bool) {
	key, value, ok := strings.Cut(assignment, "=")
	return value, ok && key == name
}

func shellWordText(source string, word *syntax.Word) string {
	start, end := int(word.Pos().Offset()), int(word.End().Offset())
	if start < 0 || end < start || end > len(source) {
		return ""
	}
	return source[start:end]
}

func sudoAssignments(args []*syntax.Word) []string {
	var assignments []string
	for i := 0; i < len(args); i++ {
		arg, literal := literalValue(args[i])
		if !literal {
			continue
		}
		if arg == "--" {
			return append(assignments, shellWordsFromAST(args[i+1:], "")...)
		}
		if strings.HasPrefix(arg, "-") {
			if slices.Contains([]string{"-u", "--user", "-g", "--group", "-h", "--host", "-p", "--prompt", "-C", "--close-from", "-D", "--chdir", "-R", "--chroot"}, arg) {
				i++
			}
			continue
		}
		if strings.Contains(arg, "=") {
			assignments = append(assignments, arg)
			continue
		}
		break
	}
	return assignments
}

func systemdRunAssignments(args []*syntax.Word) []string {
	var assignments []string
	for i := 0; i < len(args); i++ {
		arg, literal := literalValue(args[i])
		if !literal {
			if strings.HasPrefix(shellWordTextFromWord(args[i]), "-") {
				continue
			}
			break
		}
		if arg == "-E" || arg == "--setenv" {
			i++
			if i < len(args) {
				value, ok := literalValue(args[i])
				if ok {
					assignments = append(assignments, value)
				}
			}
			continue
		}
		if !strings.HasPrefix(arg, "-") {
			break
		}
		if after, ok := strings.CutPrefix(arg, "--setenv="); ok {
			assignments = append(assignments, after)
			continue
		}
		if strings.HasPrefix(arg, "--") {
			if systemdRunOptionTakesValue(arg) {
				i++
			}
			continue
		}
		value, isSetenv, consumesNext := systemdRunShortValue(arg, args, i)
		if isSetenv {
			assignments = append(assignments, value)
		}
		if consumesNext {
			i++
		}
	}
	return assignments
}

func systemdRunShortValue(arg string, args []*syntax.Word, index int) (value string, isSetenv, consumesNext bool) {
	for j := 1; j < len(arg); j++ {
		option := "-" + string(arg[j])
		if !systemdRunOptionTakesValue(option) {
			continue
		}
		isSetenv = option == "-E"
		if attached := arg[j+1:]; attached != "" {
			return attached, isSetenv, false
		}
		if index+1 < len(args) {
			if isSetenv {
				value, _ = literalValue(args[index+1])
			}
			return value, isSetenv, true
		}
		return "", isSetenv, false
	}
	return "", false, false
}

func systemdRunOptionTakesValue(arg string) bool {
	// An explicit value belongs to this word, even when the option normally
	// takes a separate argument.
	if strings.Contains(arg, "=") {
		return false
	}
	if slices.Contains([]string{
		"-u", "--unit", "--description", "--slice", "-p", "--property", "-E", "--setenv", "--service-type",
		"--uid", "--gid", "--nice", "--working-directory", "--root-directory", "--expand-environment", "-M", "--machine",
		"-H", "--host", "--json", "--job-mode", "--background", "--path-property",
		"--socket-property", "--timer-property", "--on-active", "--on-boot", "--on-startup",
		"--on-unit-active", "--on-unit-inactive", "--on-calendar", "-C", "--capsule",
	}, arg) {
		return true
	}
	return false
}

func shellWordTextFromWord(word *syntax.Word) string {
	if len(word.Parts) == 0 {
		return ""
	}
	if lit, ok := word.Parts[0].(*syntax.Lit); ok {
		return lit.Value
	}
	return ""
}

func envCallAssignments(args []string, name string) ([]string, bool) {
	var values []string
	dynamic := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			i++
			if i < len(args) {
				key, val, assigned := strings.Cut(args[i], "=")
				if assigned && key == name {
					values = append(values, val)
				}
			}
			break
		}
		if arg == "-i" || arg == "--ignore-environment" {
			continue
		}
		if slices.Contains([]string{"-u", "--unset", "-C", "--chdir", "-S", "--split-string"}, arg) {
			i++
			continue
		}
		if strings.HasPrefix(arg, "-") {
			continue
		}
		key, val, assigned := strings.Cut(arg, "=")
		if !assigned {
			break
		}
		if key != name {
			continue
		}
		if strings.ContainsAny(val, "$`") {
			dynamic = true
		} else {
			values = append(values, val)
		}
	}
	return values, dynamic
}

func homeSelfReference(source string, node syntax.Node) bool {
	start, end := int(node.Pos().Offset()), int(node.End().Offset())
	if start < 0 || end < start || end > len(source) {
		return false
	}
	value := strings.TrimSpace(source[start:end])
	value = strings.Trim(value, `"'`)
	if key, rhs, ok := strings.Cut(value, "="); ok && key == "HOME" {
		value = strings.Trim(rhs, `"'`)
	}
	return value == "$HOME" || value == "${HOME}"
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
	settings func(path string, tilde shellcmd.Tilde, cwd string) [][]byte
	agent    func(name string) []modelCandidate
}

// agentDefinitionModels — модель из определения агента name: Agent без
// model и claude --agent берут её оттуда. Определения — *.md в
// agentDirs, по симлинкам тоже; имя — поле name frontmatter, сравнивается
// точно и нормализованным, как в Claude Code 2.1.330 (приёмка 21.11).
// Модель определения проверяется по обычному списку одобренных моделей.
func agentDefinitionModels(ctx context.Context, name, cwd, home string, extra []string, logger core.Logger, rejectMissing bool) []modelCandidate {
	var found []modelCandidate
	definitionFound := false
	walk := agentWalk{seen: map[string]bool{}, logger: logger}
	dirs := append(agentDirs(ctx, cwd, home, logger), extra...)
	if builtinAgent(name) {
		pluginDirs := installedPluginAgentDirs(filepath.Join(home, ".claude", "plugins", "installed_plugins.json"), logger)
		dirs = slices.DeleteFunc(dirs, func(dir string) bool { return slices.Contains(pluginDirs, filepath.Clean(dir)) })
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
			if model != "" && !strings.EqualFold(model, "inherit") {
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
var builtinAgents = map[string]bool{"general-purpose": true, "Explore": true, "Plan": true, "claude-code-guide": true, "statusline-setup": true, "web-fetch": true, "claude": true, "fork": true}

func builtinAgent(name string) bool {
	if builtinAgents[name] {
		return true
	}
	for builtin := range builtinAgents {
		if strings.EqualFold(name, builtin) {
			return true
		}
	}
	return false
}

func agentNameMatches(defName, name string) bool {
	if sameAgentName(defName, name) {
		return true
	}
	_, pluginName, ok := strings.Cut(name, ":")
	return ok && sameAgentName(defName, pluginName)
}

// agentDirs — каталоги пользовательских и проектных определений, а также
// установленных плагинов из их реестра.
func agentDirs(ctx context.Context, cwd, home string, logger core.Logger) []string {
	var dirs []string
	if cwd != "" {
		cwd = filepath.Clean(cwd)
	}
	if config := claudeConfigDir(ctx); config != "" {
		dirs = append(dirs, filepath.Join(config, "agents"))
	}
	if home != "" {
		dirs = append(dirs, filepath.Join(home, ".claude", "agents"))
		dirs = append(dirs, installedPluginAgentDirs(filepath.Join(home, ".claude", "plugins", "installed_plugins.json"), logger)...)
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

func installedPluginAgentDirs(registry string, logger core.Logger) []string {
	raw, err := os.ReadFile(registry)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		logger.Warn("model_guard: реестр установленных плагинов не читается", "path", registry, "error", err)
		return nil
	}
	if len(raw) > maxSettingsSize {
		logger.Warn("model_guard: реестр установленных плагинов превышает лимит", "path", registry, "size", len(raw))
		return nil
	}
	var installed struct {
		Plugins map[string][]struct {
			InstallPath string `json:"installPath"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(raw, &installed); err != nil {
		logger.Warn("model_guard: реестр установленных плагинов имеет неверный JSON", "path", registry, "error", err)
		return nil
	}
	var dirs []string
	for _, entries := range installed.Plugins {
		for _, entry := range entries {
			if entry.InstallPath != "" && filepath.IsAbs(entry.InstallPath) {
				dirs = append(dirs, filepath.Join(entry.InstallPath, "agents"))
			}
		}
	}
	slices.Sort(dirs)
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

// commandCWDs возвращает cwd сессии и все известные цели cd, pushd и env -C.
func commandCWDs(command, initial string, homes []string, logger core.Logger) []string {
	paths := []string{}
	add := func(path string) {
		if path != "" && !slices.Contains(paths, path) {
			paths = append(paths, path)
		}
	}
	add(initial)
	calls, err := shellcmd.Calls(command)
	if calls == nil && err != nil {
		return paths
	}
	cwd := initial
	for i := range calls {
		call := &calls[i]
		if call.Name == "cd" || call.Name == "pushd" {
			for _, home := range homes {
				dir, known := changeDirKnown(call, cwd, home)
				if known && usableDirectory(dir, logger) {
					add(dir)
					cwd = dir
					break
				}
			}
		} else if call.Name == "env" {
			for _, home := range homes {
				if dir, found, known := envChdir(call.Args, cwd, home); found && known && usableDirectory(dir, logger) {
					add(dir)
					cwd = dir
					break
				}
			}
		}
	}
	return paths
}

// usableDirectory — каталог, в который cd выполнится. Stat идёт первым:
// открытие FIFO на чтение ждёт писателя и вешает хук
func usableDirectory(path string, logger core.Logger) bool {
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		logger.Warn("model_guard: каталог запуска недоступен или не каталог и не рассматривается", "path", path, "error", err)
		return false
	}
	file, err := os.Open(path)
	if err != nil {
		logger.Warn("model_guard: каталог запуска недоступен и не рассматривается", "path", path, "error", err)
		return false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		logger.Warn("model_guard: не удалось проверить каталог запуска", "path", path, "error", err)
		return false
	}
	return info.IsDir()
}

// bashModelCandidatesAt — модели вызовов агентов в команде оболочки. Вызовы
// ищет akit shellcmd: он видит и вложенные скрипты (bash -c, ssh, here-doc на
// вход оболочке). Если shellcmd не разобрал команду, модели ищутся в её тексте
// (--model, -m, присваивание переменной модели). readSettings возвращает все
// прочитанные варианты файла --settings.
func bashModelCandidatesAt(command, initialCWD, home string, cwdCandidates []string, homes, configs pathSet, files modelFiles, settingsAt func(string, bool) []modelCandidate, agentAt func(string, string, []string) []modelCandidate, logger core.Logger) []modelCandidate {
	calls, err := shellcmd.Calls(command)
	if calls == nil && err != nil {
		logger.Warn("model_guard: shellcmd не разобрал команду, модели ищутся в тексте", "error", err)
		return textModelCandidates(command)
	}
	var found []modelCandidate
	claude := false
	cwd := initialCWD
	for i := range calls {
		call := &calls[i]
		if call.Name == "cd" || call.Name == "pushd" {
			if next, known := changeDirKnown(call, cwd, home); known && usableDirectory(next, logger) {
				cwd = next
			}
			continue
		}
		if call.Name == "env" {
			if dir, found, known := envChdir(call.Args, cwd, home); found {
				if known && usableDirectory(dir, logger) {
					cwd = dir
				}
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
		if localFiles.settings != nil {
			read := localFiles.settings
			localFiles.settings = func(path string, tilde shellcmd.Tilde, cwd string) [][]byte { return read(path, tilde, cwd) }
		}
		if agentAt != nil {
			localFiles.agent = func(name string) []modelCandidate {
				var candidates []modelCandidate
				for _, candidateCWD := range cwdCandidates {
					candidates = append(candidates, agentAt(name, candidateCWD, callExtraDirs)...)
				}
				return candidates
			}
		}
		if dialect == "claude" {
			callExtraDirs = claudeAddedAgentDirs(args, cwd, home)
			if settingsAt != nil {
				for _, candidateCWD := range cwdCandidates {
					found = append(found, settingsAt(candidateCWD, true)...)
				}
				if homes.dynamic || configs.dynamic {
					found = append(found, modelCandidate{value: "HOME or CLAUDE_CONFIG_DIR", dynamic: true})
				}
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
var writtenAgentTarget = regexp.MustCompile(`(?:^|/)agents/.+\.md$`)

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
			if !literal {
				return true
			}
			base := filepath.Base(name)
			if base == "sed" {
				inPlace := false
				for _, arg := range n.Args[1:] {
					value, ok := literalValue(arg)
					if ok && (strings.HasPrefix(value, "-") && strings.Contains(value, "i") && !strings.HasPrefix(value, "--") || value == "--in-place" || strings.HasPrefix(value, "--in-place=")) {
						inPlace = true
					}
				}
				for _, arg := range n.Args[1:] {
					value, ok := literalValue(arg)
					if inPlace && ok && !strings.HasPrefix(value, "-") && (protectedSettingsTarget.MatchString(value) || explicitSettingsTarget(command, value)) {
						addTarget(value)
					} else if inPlace && strings.Contains(shellWordText(command, arg), ".claude/agents/") {
						addTarget(shellWordText(command, arg))
					}
				}
				return true
			}
			if !slices.Contains([]string{"cp", "mv", "install", "tee"}, base) {
				return true
			}
			var positional []string
			targetDirectory := ""
			for i := 1; i < len(n.Args); i++ {
				value, isLiteral := literalValue(n.Args[i])
				if !isLiteral {
					value = shellWordText(command, n.Args[i])
					if strings.Contains(value, ".claude/agents/") || protectedSettingsTarget.MatchString(value) {
						positional = append(positional, value)
					}
					continue
				}
				if (value == "-t" || value == "--target-directory") && i+1 < len(n.Args) {
					target, ok := literalValue(n.Args[i+1])
					if ok {
						targetDirectory = target
					}
					i++
					continue
				}
				if after, ok := strings.CutPrefix(value, "--target-directory="); ok {
					targetDirectory = after
					continue
				}
				if strings.HasPrefix(value, "-") {
					continue
				}
				positional = append(positional, value)
			}
			if targetDirectory != "" {
				for _, arg := range n.Args[1:] {
					value, ok := literalValue(arg)
					if !ok || strings.HasPrefix(value, "-") || value == targetDirectory {
						continue
					}
					addTarget(filepath.Join(targetDirectory, filepath.Base(value)))
				}
				if len(positional) == 0 && (protectedSettingsTarget.MatchString(targetDirectory) || strings.Contains(targetDirectory, ".claude/agents")) {
					addTarget(filepath.Join(targetDirectory, "unknown.md"))
				}
			}
			if len(positional) == 0 {
				return true
			}
			if base == "tee" {
				for _, target := range positional {
					addTarget(target)
				}
			} else {
				target := positional[len(positional)-1]
				if base == "cp" && len(positional) > 1 && strings.Contains(target, ".claude/agents") && !strings.HasSuffix(target, ".md") {
					for _, source := range positional[:len(positional)-1] {
						addTarget(filepath.Join(target, filepath.Base(source)))
					}
					return true
				}
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
	recognizedAgentDefinitions := map[string]bool{}
	unknownAgentDefinitions := map[string]bool{}
	for _, match := range writtenModel.FindAllStringSubmatch(decoded, -1) {
		if match[1] == "model" {
			found = append(found, modelCandidate{value: match[2]})
		} else if c, ok := envCandidate(match[1], match[2], false); ok {
			found = append(found, c)
		}
	}
	objects := jsonObjects(decoded)
	parsedSettingsContents := false
	parsedSettingsModels := false
	for _, raw := range objects {
		if json.Valid(raw) {
			parsedSettingsContents = true
			settings := settingsCandidates(raw, nil)
			parsedSettingsModels = parsedSettingsModels || len(settings) > 0
			found = append(found, settings...)
		} else {
			found = append(found, writtenOverrideCandidates(raw)...)
		}
	}
	if len(objects) == 0 {
		found = append(found, writtenOverrideCandidates([]byte(decoded))...)
	}
	if file, err := syntax.NewParser().Parse(strings.NewReader(decoded), ""); err == nil {
		syntax.Walk(file, func(node syntax.Node) bool {
			if call, ok := node.(*syntax.CallExpr); ok && len(call.Args) > 1 {
				name, literal := literalValue(call.Args[0])
				if literal && filepath.Base(name) == "sed" {
					inPlace := false
					for _, arg := range call.Args[1:] {
						value, ok := literalValue(arg)
						if ok && (strings.HasPrefix(value, "-") && strings.Contains(value, "i") && !strings.HasPrefix(value, "--") || value == "--in-place" || strings.HasPrefix(value, "--in-place=")) {
							inPlace = true
						}
					}
					for _, arg := range call.Args[1:] {
						value, ok := literalValue(arg)
						raw := shellWordText(decoded, arg)
						if inPlace && (ok && (protectedSettingsTarget.MatchString(value) || explicitSettingsTarget(decoded, value)) || strings.Contains(raw, ".claude/agents/")) {
							if !ok {
								value = raw
							}
							unknownAgentDefinitions[strings.Trim(value, "\"'")] = true
						}
					}
				} else if literal && slices.Contains([]string{"cp", "mv", "install", "tee"}, filepath.Base(name)) {
					base := filepath.Base(name)
					var positional []string
					targetDirectory := ""
					for i := 1; i < len(call.Args); i++ {
						arg := call.Args[i]
						value, ok := literalValue(arg)
						if ok && (value == "-t" || value == "--target-directory") && i+1 < len(call.Args) {
							targetDirectory, _ = literalValue(call.Args[i+1])
							i++
						} else if ok && strings.HasPrefix(value, "--target-directory=") {
							targetDirectory = strings.TrimPrefix(value, "--target-directory=")
						} else if ok && !strings.HasPrefix(value, "-") {
							positional = append(positional, value)
						} else if strings.Contains(shellWordText(decoded, arg), ".claude/agents/") {
							positional = append(positional, shellWordText(decoded, arg))
						}
					}
					if targetDirectory != "" {
						for _, source := range positional {
							if base == "cp" {
								unknownAgentDefinitions[filepath.Join(targetDirectory, filepath.Base(source))] = true
							}
						}
						if base == "cp" && len(positional) == 0 {
							unknownAgentDefinitions[filepath.Join(targetDirectory, "unknown.md")] = true
						}
					} else if len(positional) > 0 {
						targets := positional
						if base == "cp" {
							targets = positional[len(positional)-1:]
							if len(positional) > 1 && (strings.TrimSuffix(positional[len(positional)-1], "/") == ".claude/agents" || strings.HasSuffix(positional[len(positional)-1], "/agents")) {
								// A directory destination creates a file named after each source.
								targets = nil
								for _, source := range positional[:len(positional)-1] {
									targets = append(targets, filepath.Join(positional[len(positional)-1], filepath.Base(source)))
								}
							}
						}
						for _, target := range targets {
							if protectedSettingsTarget.MatchString(target) || explicitSettingsTarget(decoded, target) {
								unknownAgentDefinitions[strings.Trim(target, "\"'")] = true
							}
						}
					}
				}
				return true
			}
			stmt, ok := node.(*syntax.Stmt)
			if !ok {
				return true
			}
			stmtHasHeredoc := false
			for _, redirect := range stmt.Redirs {
				stmtHasHeredoc = stmtHasHeredoc || redirect.Hdoc != nil
			}
			for _, redirect := range stmt.Redirs {
				if redirect.Word == nil {
					continue
				}
				target, literal := literalValue(redirect.Word)
				if !literal || (!protectedSettingsTarget.MatchString(target) && !explicitSettingsTarget(decoded, target)) {
					continue
				}
				if redirect.Op.String() == ">>" || (!stmtHasHeredoc && !parsedSettingsContents && (redirect.Op.String() == ">" || redirect.Op.String() == ">|")) {
					unknownAgentDefinitions[strings.Trim(target, "\"'")] = true
				}
			}
			return true
		})
	}
	// Heredoc body bytes are literal when its delimiter is quoted. Parse the
	// original command here: decoded normalizes shell escape sequences used by
	// printf/settings analysis and would corrupt quoted heredoc contents.
	if file, err := syntax.NewParser().Parse(strings.NewReader(command), ""); err == nil {
		catFunctionDeclared := false
		syntax.Walk(file, func(node syntax.Node) bool {
			if function, ok := node.(*syntax.FuncDecl); ok {
				if function.Name != nil && function.Name.Value == "cat" {
					catFunctionDeclared = true
				}
				for _, name := range function.Names {
					catFunctionDeclared = catFunctionDeclared || name.Value == "cat"
				}
			}
			return true
		})
		syntax.Walk(file, func(node syntax.Node) bool {
			stmt, ok := node.(*syntax.Stmt)
			if !ok {
				return true
			}
			var targets []string
			var body string
			bodyQuoted := false
			var stdinHeredoc *syntax.Redirect
			stdinRedirected := false
			hasHeredoc := false
			for _, redirect := range stmt.Redirs {
				if redirect.Hdoc != nil {
					hasHeredoc = true
					if redirect.N == nil || redirect.N.Value == "0" {
						stdinHeredoc = redirect
						stdinRedirected = true
					}
					continue
				}
				fd0 := redirect.N != nil && redirect.N.Value == "0" || redirect.N == nil && slices.Contains([]string{"<", "<<<", "<&", "<>"}, redirect.Op.String())
				if fd0 {
					stdinRedirected = true
					stdinHeredoc = nil
				}
				if redirect.Word == nil {
					continue
				}
				target, literal := literalValue(redirect.Word)
				if literal && slices.Contains([]string{">", ">>", ">|", "&>", "&>>"}, redirect.Op.String()) {
					targets = append(targets, strings.Trim(target, `"'`))
				}
			}
			if stdinHeredoc != nil && stdinRedirected {
				body = stdinHeredoc.Hdoc.Lit()
				bodyQuoted = heredocDelimiterQuoted(stdinHeredoc)
				if stdinHeredoc.Op == syntax.DashHdoc {
					lines := strings.Split(body, "\n")
					for i := range lines {
						lines[i] = strings.TrimLeft(lines[i], "\t")
					}
					body = strings.Join(lines, "\n")
				}
			}
			consumerIsCat := false
			if call, ok := stmt.Cmd.(*syntax.CallExpr); ok && len(call.Args) > 0 {
				if name, literal := literalValue(call.Args[0]); literal {
					base := filepath.Base(name)
					if base == "cat" {
						consumerIsCat = !catFunctionDeclared && len(call.Args) == 1
						if len(call.Args) == 2 {
							consumerIsCat = !catFunctionDeclared && call.Args[1].Lit() == "-"
						}
					}
					if base == "tee" {
						for _, arg := range call.Args[1:] {
							if target, literal := literalValue(arg); literal && !strings.HasPrefix(target, "-") {
								targets = append(targets, strings.Trim(target, `"'`))
							}
						}
						consumerIsCat = true
					}
				}
			}
			consumerIsCat = consumerIsCat && stdinHeredoc != nil && stdinRedirected
			if body != "" {
				for _, target := range targets {
					if !writtenAgentTarget.MatchString(target) {
						continue
					}
					if !consumerIsCat || !agentFrontmatterBlock.Match([]byte(body)) || (!bodyQuoted && strings.ContainsAny(body, "$"+string(rune(96)))) {
						continue
					}
					_, model := agentFrontmatter([]byte(body), target)
					recognizedAgentDefinitions[target] = true
					if model != "" && !strings.EqualFold(model, "inherit") {
						found = append(found, modelCandidate{value: model})
					}
				}
			}
			if hasHeredoc && (body == "" || stdinHeredoc == nil || !consumerIsCat || !agentFrontmatterBlock.Match([]byte(body)) || !bodyQuoted && strings.ContainsAny(body, "$"+string(rune(96)))) {
				for _, target := range targets {
					if writtenAgentTarget.MatchString(target) {
						unknownAgentDefinitions[target] = true
					}
				}
			}
			return true
		})
	}
	for _, target := range protectedWriteTargets(command) {
		target = strings.Trim(target, "\"'")
		isAgent := writtenAgentTarget.MatchString(target)
		isParsedSettings := (protectedSettingsTarget.MatchString(target) || explicitSettingsTarget(command, target)) && parsedSettingsContents && parsedSettingsModels
		if (!isAgent && !isParsedSettings) || (isAgent && !recognizedAgentDefinitions[target]) || unknownAgentDefinitions[target] {
			return append(found, modelCandidate{value: "written settings or agent contents", unknownWrite: true})
		}
	}
	return found
}

func heredocDelimiterQuoted(redirect *syntax.Redirect) bool {
	for _, part := range redirect.Word.Parts {
		switch part.(type) {
		case *syntax.SglQuoted, *syntax.DblQuoted:
			return true
		}
	}
	return false
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

// claudeConfigDir — CLAUDE_CONFIG_DIR запроса: относительный путь — от cwd
// запроса, как его открыл бы процесс хука
func claudeConfigDir(ctx context.Context) string {
	request, ok := hookctx.Request(ctx)
	if !ok {
		return ""
	}
	return request.Path(hookctx.EnvClaudeConfigDir)
}

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

// modelEnvNames — переменные Claude Code, которые явно задают модель.
var modelEnvNames = map[string]struct{}{
	"ANTHROPIC_MODEL": {}, "ANTHROPIC_DEFAULT_MODEL": {},
	"ANTHROPIC_DEFAULT_OPUS_MODEL": {}, "ANTHROPIC_DEFAULT_SONNET_MODEL": {},
	"ANTHROPIC_DEFAULT_FABLE_MODEL": {}, "CLAUDE_CODE_SUBAGENT_MODEL": {},
	"CLAUDE_CODE_WORKFLOW_SUBAGENT_MODEL": {},
	"ANTHROPIC_DEFAULT_HAIKU_MODEL":       {}, "ANTHROPIC_SMALL_FAST_MODEL": {},
	"CLAUDE_CODE_AUTO_MODE_MODEL": {}, "CLAUDE_CODE_BG_CLASSIFIER_MODEL": {},
}

// ModelEnvironmentNames returns the environment keys inspected by model_guard.
func ModelEnvironmentNames() []string {
	return slices.Sorted(maps.Keys(modelEnvNames))
}

// modelEnvPattern — имена modelEnvNames альтернативой регулярного выражения
func modelEnvPattern() string {
	names := slices.Sorted(maps.Keys(modelEnvNames))
	return strings.Join(names, "|")
}

// envCandidate — кандидат из присваивания value переменной name; не
// переменная модели или пустое значение — нет кандидата
func envCandidate(name, value string, dynamic bool) (modelCandidate, bool) {
	_, ok := modelEnvNames[name]
	if !ok || value == "" {
		return modelCandidate{}, false
	}
	return modelCandidate{value: value, dynamic: dynamic}, true
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
	// `script` не относится к оболочкам shellcmd и его командная строка
	// не попадает в callEnvModels; разбираем её здесь.
	for i := range calls {
		c := &calls[i]
		if c.Name == "script" {
			for _, command := range scriptCommandStrings(c.Args) {
				found = append(found, nestedEnvModels(command, logger)...)
			}
		}
	}
	return found
}

func shellCommandStrings(args []string) []string {
	commandMode := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			if commandMode && i+1 < len(args) {
				return []string{args[i+1]}
			}
			return nil
		}
		if arg == "--command" {
			commandMode = true
			continue
		}
		if arg == "--rcfile" || arg == "--init-file" || arg == "--startup-file" {
			i++
			continue
		}
		if arg == "-" || arg == "+" {
			if commandMode && i+1 < len(args) {
				// bash -c -/+ word assigns -/+ to $0; word is the command.
				return []string{args[i+1]}
			}
			return nil
		}
		if !strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "+") {
			if commandMode {
				return []string{arg}
			}
			return nil
		}
		if strings.HasPrefix(arg, "--") || len(arg) < 2 {
			continue
		}
		for _, option := range arg[1:] {
			switch option {
			case 'c':
				commandMode = true
			case 'o', 'O':
				// Bash consumes the following argv word for -o/-O even
				// when the option occurs in a short-option cluster.
				i++
			}
		}
	}
	return nil
}

func scriptCommandStrings(args []string) []string {
	// getopt_long accepts an unambiguous prefix of a long option. Keep the
	// argument behavior alongside the option names so abbreviations are
	// handled exactly like their full spelling.
	longOptions := []struct {
		name        string
		requiresArg bool
		optionalArg bool
	}{
		{name: "append"},
		{name: "command", requiresArg: true},
		{name: "return"},
		{name: "flush"},
		{name: "quiet"},
		{name: "force"},
		{name: "log-in", requiresArg: true},
		{name: "log-out", requiresArg: true},
		{name: "log-io", requiresArg: true},
		{name: "log-timing", optionalArg: true},
		{name: "logging-format", requiresArg: true},
		{name: "echo", requiresArg: true},
		{name: "output-limit", requiresArg: true},
		{name: "shell", requiresArg: true},
		{name: "session-command"},
		{name: "help"},
		{name: "version"},
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return nil
		}
		if arg == "-c" {
			if i+1 < len(args) {
				return []string{args[i+1]}
			}
			return nil
		}
		if slices.Contains([]string{"-I", "-O", "-B", "-T", "-m", "-E", "-o", "--log-in", "--log-out", "--log-io", "--log-timing", "--logging-format", "--echo", "--output-limit"}, arg) {
			i++
			continue
		}
		if after, ok := strings.CutPrefix(arg, "--"); ok {
			name, value, hasValue := strings.Cut(after, "=")
			var match *struct {
				name        string
				requiresArg bool
				optionalArg bool
			}
			matches := 0
			for j := range longOptions {
				if strings.HasPrefix(longOptions[j].name, name) {
					match = &longOptions[j]
					matches++
				}
			}
			if matches == 1 {
				if match.name == "command" {
					if hasValue {
						return []string{value}
					}
					if i+1 < len(args) {
						return []string{args[i+1]}
					}
					return nil
				}
				if match.requiresArg && !hasValue || match.optionalArg && !hasValue {
					i++
				}
			}
			continue
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			continue
		}
		// Разбираем короткие опции как getopt: параметрная опция
		// поглощает остаток слова, а -c — отдельный флаг.
		cluster := arg[1:]
		for pos := 0; pos < len(cluster); pos++ {
			option := cluster[pos]
			if option == 'c' {
				if pos+1 < len(cluster) {
					return []string{cluster[pos+1:]}
				}
				if i+1 < len(args) {
					return []string{args[i+1]}
				}
				return nil
			}
			if strings.ContainsRune("IOBTmEo", rune(option)) {
				if pos+1 == len(cluster) {
					i++
				}
				break
			}
			if option == 't' {
				// -t has an optional attached argument; it never consumes
				// the next word when written by itself.
				break
			}
		}
	}
	return nil
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

// ClaudeModelArgFlags — флаги claude, которые model_guard читает вместе со
// значением в следующем слове: модель, настройки, агенты и каталоги агентов.
// Остальные слова командной строки claude модель не задают
func ClaudeModelArgFlags() []string {
	return append([]string{"--model", "--add-dir", "--plugin-dir"}, valueFlags...)
}

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
	if name == "claude" {
		args = splitValueFlags(args)
	}
	var agentOverrides map[string]bool
	if name == "claude" {
		agentOverrides = agentOverrideNames(args, files.settings)
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
			found = append(found, agentOverrideModels(args, next.text, files.settings)...)
			definitions := agentFlagCandidates(next, files)
			if agentOverrides[next.text] {
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

func agentOverrideNames(args []word, readSettings func(string, shellcmd.Tilde, string) [][]byte) map[string]bool {
	names := map[string]bool{}
	for i := 0; i+1 < len(args); i++ {
		if args[i].text != "--agents" {
			continue
		}
		arg := args[i+1]
		raws := [][]byte{[]byte(arg.text)}
		if !strings.HasPrefix(strings.TrimSpace(arg.text), "{") {
			if !arg.literal || !claudeArgIsLiteral(arg) || arg.tilde&(shellcmd.TildeOther|shellcmd.TildeModeUnknown) != 0 {
				continue
			}
			raws = readSettings(arg.text, arg.tilde, "")
		}
		for _, raw := range raws {
			if parsed, ok := parseAgentsJSON(raw); ok {
				for name := range parsed {
					names[name] = true
				}
			}
		}
	}
	return names
}

func agentOverrideModels(args []word, name string, readSettings func(string, shellcmd.Tilde, string) [][]byte) []modelCandidate {
	var found []modelCandidate
	for i := 0; i+1 < len(args); i++ {
		if args[i].text != "--agents" {
			continue
		}
		arg := args[i+1]
		raws := [][]byte{[]byte(arg.text)}
		if !strings.HasPrefix(strings.TrimSpace(arg.text), "{") {
			if readSettings == nil || !arg.literal || !claudeArgIsLiteral(arg) {
				continue
			}
			raws = readSettings(arg.text, arg.tilde, "")
		}
		for _, raw := range raws {
			if parsed, ok := parseAgentsJSON(raw); ok {
				found = append(found, parsed[name]...)
			}
		}
	}
	return found
}

func parseAgentsJSON(raw []byte) (map[string][]modelCandidate, bool) {
	var agents map[string]json.RawMessage
	if json.Unmarshal(raw, &agents) != nil || agents == nil {
		return nil, false
	}
	models := make(map[string][]modelCandidate, len(agents))
	for name, rawAgent := range agents {
		models[name] = nil
		var agent struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(rawAgent, &agent) == nil && strings.TrimSpace(agent.Model) != "" && !strings.EqualFold(agent.Model, "inherit") {
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

// writtenOverrides finds model IDs embedded in settings JSON in shell text.
var writtenOverrides = regexp.MustCompile(`"modelOverrides"\s*:\s*\{([^}]*)\}`)
var writtenOverrideValue = regexp.MustCompile(`"[^"]+"\s*:\s*"([^"]*)"`)

func writtenOverrideCandidates(raw []byte) []modelCandidate {
	var found []modelCandidate
	for _, object := range writtenOverrides.FindAllSubmatch(raw, -1) {
		for _, value := range writtenOverrideValue.FindAllSubmatch(object[1], -1) {
			if strings.TrimSpace(string(value[1])) != "" {
				found = append(found, modelCandidate{value: string(value[1])})
			}
		}
	}
	return found
}

// jsonObjects extracts brace-delimited objects from shell text while respecting
// quoted strings and escapes. The settings parser decides which objects matter.
func jsonObjects(text string) [][]byte {
	var objects [][]byte
	start, depth := -1, 0
	quoted, escaped := false, false
	for i := 0; i < len(text); i++ {
		c := text[i]
		if quoted {
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				quoted = false
			}
			continue
		}
		if c == '"' {
			quoted = true
			continue
		}
		if c == '{' {
			if depth == 0 {
				start = i
			}
			depth++
		} else if c == '}' && depth > 0 {
			depth--
			if depth == 0 {
				objects = append(objects, []byte(text[start:i+1]))
			}
		}
	}
	return objects
}

// jsonModelCandidates — модели в JSON флага claude: --settings (строка JSON
// или файл: model и env.ANTHROPIC_MODEL…) и --agents (model каждого агента).
// Файла --settings ещё нет — модели в тексте команды, которая его пишет.
// Не JSON — моделей нет: такой запуск claude сам не начнётся
func jsonModelCandidates(flag string, arg word, readSettings func(string, shellcmd.Tilde, string) [][]byte, agent func(string) []modelCandidate) []modelCandidate {
	if flag == "--agents" {
		inlineJSON := strings.HasPrefix(strings.TrimSpace(arg.text), "{")
		if !arg.literal || (!inlineJSON && !claudeArgIsLiteral(arg)) {
			return []modelCandidate{{value: arg.text, unknownAgents: true}}
		}
		raws := [][]byte{[]byte(arg.text)}
		if !inlineJSON {
			if arg.tilde&(shellcmd.TildeOther|shellcmd.TildeModeUnknown) != 0 {
				return []modelCandidate{{value: arg.text, unknownAgents: true}}
			}
			raws = readSettings(arg.text, arg.tilde, "")
			if len(raws) == 0 {
				return []modelCandidate{{value: arg.text, unknownAgents: true}}
			}
		}
		var found []modelCandidate
		for _, raw := range raws {
			models, ok := parseAgentsJSON(raw)
			if !ok && !inlineJSON {
				return []modelCandidate{{value: arg.text, unknownAgents: true}}
			}
			found = append(found, flattenAgentModels(models)...)
		}
		return found
	}
	if !arg.literal {
		// Путь файла из переменной не прочесть, а JSON с подстановкой — модель
		// переменной, если в нём есть поле модели
		if jsonModelKey.MatchString(arg.text) {
			return []modelCandidate{{value: arg.text, dynamic: true}}
		}
		return nil
	}
	raws := [][]byte{[]byte(arg.text)}
	if !strings.HasPrefix(strings.TrimSpace(arg.text), "{") {
		if flag != "--settings" {
			return nil
		}
		if arg.tilde&(shellcmd.TildeOther|shellcmd.TildeModeUnknown) != 0 {
			return []modelCandidate{{value: arg.text, dynamic: true}}
		}
		raws = readSettings(arg.text, arg.tilde, "")
		if len(raws) == 0 {
			return []modelCandidate{{value: arg.text, unknownSettings: true}}
		}
	}
	var found []modelCandidate
	for _, raw := range raws {
		found = append(found, settingsCandidates(raw, agent)...)
	}
	return found
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
			found = append(found, writtenOverrideCandidates(raw)...)
		}
		return found
	}
	var model string
	if json.Unmarshal(settings["model"], &model) == nil {
		found.add(model)
	}
	var overrides map[string]json.RawMessage
	rawOverrides := settings["modelOverrides"]
	if len(rawOverrides) != 0 && strings.TrimSpace(string(rawOverrides)) != "null" {
		if err := json.Unmarshal(rawOverrides, &overrides); err != nil {
			found = append(found, modelCandidate{value: "modelOverrides", unknownSettings: true})
		}
	}
	if overrides != nil {
		for _, key := range slices.Sorted(maps.Keys(overrides)) {
			rawModel := overrides[key]
			if strings.TrimSpace(string(rawModel)) == "null" {
				continue
			}
			var model string
			if json.Unmarshal(rawModel, &model) == nil {
				found.add(model)
			} else {
				found = append(found, modelCandidate{value: "modelOverrides", unknownSettings: true})
			}
		}
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

func (t *ModelGuardTool) approvedPath(ctx context.Context) string {
	if request, ok := hookctx.Request(ctx); ok {
		if path := request.Path(hookctx.EnvModelsApproved); path != "" {
			return path
		}
		if t.path != "" {
			return request.Abs(t.path)
		}
		if home := request.Path(hookctx.EnvHome); home != "" {
			return filepath.Join(home, ".config", "models", "approved")
		}
		t.Logger().Warn("не удалось определить домашний каталог для списка моделей", "error", errors.New("$HOME is not defined"))
		return request.Abs(defaultApprovedModelsPath)
	}
	return ""
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
