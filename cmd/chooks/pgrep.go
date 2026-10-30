package main

import (
	"encoding/json"
	"errors"
	"io"

	"github.com/aiseeq/akit/shellcmd"
	"github.com/aiseeq/chooks/internal/core"
	"github.com/aiseeq/chooks/internal/tools"
)

// pgrepAncestorsTool — ключ в config.yaml; без ключа включено, а действует
// там, куда settings.json направляет PreToolUse (Bash, Monitor)
const pgrepAncestorsTool = "pgrep_ancestors"

// preToolUseOutput — ответ PreToolUse с подменой входа инструмента. Без
// permissionDecision: решение о разрешении остаётся за обычным порядком.
// Без additionalContext: хуки-подсказчики пользователь отклонил 25.10
type preToolUseOutput struct {
	HookSpecificOutput struct {
		HookEventName string         `json:"hookEventName"`
		UpdatedInput  map[string]any `json:"updatedInput"`
	} `json:"hookSpecificOutput"`
}

// rewritePgrep добавляет -A к pgrep -f в команде Bash и Monitor (почему —
// tools.RewritePgrepAncestors). Сбой только логируется: команда тогда идёт
// как написана
func rewritePgrep(input *core.ToolInput, config *core.Config, logger core.Logger, out io.Writer) {
	if tool, exists := config.Tools[pgrepAncestorsTool]; exists && !tool.Enabled {
		return
	}
	if input.ToolName != "Bash" && input.ToolName != "Monitor" {
		return
	}
	var toolInput map[string]any
	if err := json.Unmarshal(input.ToolInput, &toolInput); err != nil {
		logger.Warn("pgrep rewrite: tool input unreadable", "tool", input.ToolName, "error", err)
		return
	}
	command, ok := toolInput["command"].(string)
	if !ok {
		return
	}
	rewritten, changed, err := tools.RewritePgrepAncestors(command)
	var nested *shellcmd.NestedError
	switch {
	case errors.As(err, &nested):
		// Битый вложенный скрипт упадёт сам, остальные pgrep переписаны
		logger.Warn("pgrep rewrite: nested script unparsed", "tool", input.ToolName, "error", err)
	case err != nil:
		logger.Warn("pgrep rewrite failed", "tool", input.ToolName, "error", err)
		return
	}
	if !changed {
		return
	}
	toolInput["command"] = rewritten

	var output preToolUseOutput
	output.HookSpecificOutput.HookEventName = "PreToolUse"
	output.HookSpecificOutput.UpdatedInput = toolInput
	if err := json.NewEncoder(out).Encode(output); err != nil {
		logger.Warn("pgrep rewrite not written", "error", err)
		return
	}
	logger.Info("pgrep rewritten", "session", input.SessionID, "tool", input.ToolName)
}
