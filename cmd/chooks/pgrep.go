package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/aiseeq/akit/shellcmd"
	"github.com/aiseeq/chooks/internal/core"
	"github.com/aiseeq/chooks/internal/tools"
)

// pgrepAncestorsTool — ключ в config.yaml; без ключа включено, а действует
// там, куда settings.json направляет PreToolUse (Bash, Monitor)
const pgrepAncestorsTool = "pgrep_ancestors"

// preToolUseOutput — ответ PreToolUse: подмена входа инструмента (pgrep) и
// подсказка модели (широкий обход диска). Без permissionDecision: решение о
// разрешении остаётся за обычным порядком. Подсказка допустима по решению
// fallback-hook-not-hint: запрещён только отказ, прерывающий агента
type preToolUseOutput struct {
	HookSpecificOutput struct {
		HookEventName     string         `json:"hookEventName"`
		UpdatedInput      map[string]any `json:"updatedInput,omitempty"`
		AdditionalContext string         `json:"additionalContext,omitempty"`
	} `json:"hookSpecificOutput"`
}

// writeAllowedOutput пишет ответ разрешённого вызова: подмену pgrep и
// подсказку о широком обходе одним JSON; нечего сказать — ничего
func writeAllowedOutput(input *core.ToolInput, config *core.Config, logger core.Logger, out io.Writer) {
	updated, err := rewritePgrep(input, config, logger)
	if err != nil {
		logger.Warn("pre-tool-use: pgrep not rewritten", "tool", input.ToolName, "error", err)
	}
	hint, err := wideSearchContext(input, config, logger)
	if err != nil {
		logger.Warn("pre-tool-use: wide search not checked", "tool", input.ToolName, "error", err)
	}
	if updated == nil && hint == "" {
		return
	}
	var output preToolUseOutput
	output.HookSpecificOutput.HookEventName = "PreToolUse"
	output.HookSpecificOutput.UpdatedInput = updated
	output.HookSpecificOutput.AdditionalContext = hint
	if err := json.NewEncoder(out).Encode(output); err != nil {
		logger.Warn("pre-tool-use output not written", "error", err)
		return
	}
	if updated != nil {
		logger.Info("pgrep rewritten", "session", input.SessionID, "tool", input.ToolName)
	}
}

// rewritePgrep добавляет -A к pgrep -f в команде Bash и Monitor (почему —
// tools.RewritePgrepAncestors) и возвращает вход с подменой; nil — подменять
// нечего. При ошибке команда идёт как написана
func rewritePgrep(input *core.ToolInput, config *core.Config, logger core.Logger) (map[string]any, error) {
	if tool, exists := config.Tools[pgrepAncestorsTool]; exists && !tool.Enabled {
		return nil, nil
	}
	if input.ToolName != "Bash" && input.ToolName != "Monitor" {
		return nil, nil
	}
	var toolInput map[string]any
	if err := json.Unmarshal(input.ToolInput, &toolInput); err != nil {
		return nil, fmt.Errorf("tool input unreadable: %w", err)
	}
	command, ok := toolInput["command"].(string)
	if !ok {
		return nil, nil
	}
	rewritten, changed, err := tools.RewritePgrepAncestors(command)
	var nested *shellcmd.NestedError
	switch {
	case errors.As(err, &nested):
		// Битый вложенный скрипт упадёт сам, остальные pgrep переписаны
		logger.Warn("pgrep rewrite: nested script unparsed", "tool", input.ToolName, "error", err)
	case err != nil:
		return nil, err
	}
	if !changed {
		return nil, nil
	}
	toolInput["command"] = rewritten
	return toolInput, nil
}
