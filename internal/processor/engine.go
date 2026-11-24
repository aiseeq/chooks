package processor

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/aiseeq/chooks/internal/core"
	"github.com/aiseeq/chooks/internal/hookctx"
	"github.com/aiseeq/chooks/internal/tools"
)

// Engine прогоняет вызов инструмента через проверки перед выполнением
type Engine struct {
	logger core.Logger
	tools  []core.ToolValidator
}

// NewForRequest creates guards with the hook's process and environment.
func NewForRequest(config *core.Config, logger core.Logger, request hookctx.Context) (*Engine, error) {
	engine := &Engine{logger: logger.With("component", "engine")}
	if err := engine.initToolsForRequest(config, request); err != nil {
		return nil, err
	}
	engine.logger.Debug("engine initialized", "tools", len(engine.tools))
	return engine, nil
}

// toolConstructor — проверка PreToolUse: ключ в config.yaml и конструктор
type toolConstructor struct {
	name         string
	onWhenAbsent bool
}

var constructors = []toolConstructor{
	{"git_guard", true}, {"rsync_guard", true}, {"jira_style", false},
	{"kill_guard", false}, {"model_guard", false}, {"rm_guard", false}, {"write_guard", false},
}

// ToolNames — ключи всех проверок PreToolUse движка в config.yaml
func ToolNames() []string {
	names := make([]string, 0, len(constructors))
	for _, c := range constructors {
		names = append(names, c.name)
	}
	return names
}

// DefaultOnNames — ключи проверок, включённых без ключа в config.yaml
func DefaultOnNames() []string {
	var names []string
	for _, c := range constructors {
		if c.onWhenAbsent {
			names = append(names, c.name)
		}
	}
	return names
}

// initToolsForRequest initializes each guard with request state.
func (e *Engine) initToolsForRequest(config *core.Config, request hookctx.Context) error {
	for _, constructor := range constructors {
		toolConfig, exists := config.Tools[constructor.name]
		if !exists && constructor.onWhenAbsent {
			toolConfig, exists = core.ToolConfig{Enabled: true}, true
		}
		if !exists || !toolConfig.Enabled {
			continue
		}
		var tool core.ToolValidator
		var err error
		switch constructor.name {
		case "git_guard":
			tool, err = tools.NewGitGuardToolForRequest(toolConfig, e.logger, request)
		case "rm_guard":
			tool, err = tools.NewRmGuardToolForRequest(toolConfig, e.logger, request)
		case "kill_guard":
			tool, err = tools.NewKillGuardToolForRequest(toolConfig, e.logger, request)
		case "rsync_guard":
			tool, err = tools.NewRsyncGuardTool(toolConfig, e.logger)
		case "jira_style":
			tool, err = tools.NewJiraStyleTool(toolConfig, e.logger)
		case "model_guard":
			tool, err = tools.NewModelGuardTool(toolConfig, e.logger)
		case "write_guard":
			tool, err = tools.NewWriteGuardTool(toolConfig, e.logger)
		default:
			return fmt.Errorf("unknown tool %q", constructor.name)
		}
		if err != nil {
			return fmt.Errorf("failed to create %s tool: %w", constructor.name, err)
		}
		e.tools = append(e.tools, tool)
	}
	return nil
}

// ProcessPreToolUse обрабатывает PreToolUse хук
func (e *Engine) ProcessPreToolUse(ctx context.Context, input *core.ToolInput) (*core.HookResponse, error) {
	if _, ok := hookctx.Request(ctx); !ok {
		return nil, fmt.Errorf("hook request is missing")
	}
	start := time.Now()
	e.logger.Debug("processing pre-tool-use hook", "tool", input.ToolName)

	var violations []core.Violation
	var suggestions []string

	for _, tool := range e.tools {
		if !slices.Contains(tool.SupportedTools(), input.ToolName) {
			continue
		}

		result, err := tool.ValidateTool(ctx, input)
		if err != nil {
			// Сбой одной проверки не должен отключать остальные
			e.logger.Error("tool validator failed", "tool", tool.Name(), "error", err)
			continue
		}
		violations = append(violations, result.Violations...)
		suggestions = append(suggestions, result.Suggestions...)
	}

	return buildResponse(violations, suggestions, start), nil
}

// buildResponse собирает ответ хука по найденным нарушениям
func buildResponse(violations []core.Violation, suggestions []string, start time.Time) *core.HookResponse {
	level := core.HighestLevel(violations)

	action := core.HookActionAllow
	switch level {
	case core.LevelCritical:
		action = core.HookActionBlock
	case core.LevelWarning:
		action = core.HookActionWarn
	}

	return &core.HookResponse{
		Action:      action,
		Message:     buildMessage(action, violations),
		Suggestions: deduplicate(suggestions),
		Level:       level,
		Violations:  violations,
		Timestamp:   start,
		ProcessTime: float64(time.Since(start).Microseconds()) / 1000,
	}
}

// buildMessage формирует сообщение ответа из нарушений
func buildMessage(action core.HookAction, violations []core.Violation) string {
	if action == core.HookActionAllow {
		return "Operation allowed"
	}

	for _, violation := range violations {
		if violation.Severity == core.LevelCritical || action == core.HookActionWarn {
			if violation.Line > 0 {
				return fmt.Sprintf("%s (строка %d)", violation.Message, violation.Line)
			}
			return violation.Message
		}
	}

	return "Operation blocked"
}

// deduplicate удаляет дублирующиеся предложения, сохраняя порядок
func deduplicate(suggestions []string) []string {
	seen := make(map[string]bool, len(suggestions))
	unique := make([]string, 0, len(suggestions))
	for _, suggestion := range suggestions {
		if !seen[suggestion] {
			seen[suggestion] = true
			unique = append(unique, suggestion)
		}
	}
	return unique
}
