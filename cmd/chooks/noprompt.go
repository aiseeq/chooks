package main

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/aiseeq/chooks/internal/core"
)

// Отказ окнам, которые останавливают агента до ввода человека: вопрос с
// выбором, запрос разрешения, запрос ввода от MCP-сервера. Пользователь
// 28.10: «"Включаю этот набор?" - да» (~/.claude/decisions.md,
// no-blocking-prompts). Вопрос пользователю остаётся текстом в ответе
const (
	askDeniedReason = "Окно вопроса с выбором отключено пользователем 28.10 (~/.claude/decisions.md, no-blocking-prompts). " +
		"Задай вопрос текстом в ответе по правилу «Вопрос любому человеку» из ~/.claude/CLAUDE.md и закончи ход."
	permissionDeniedMessage = "Окно разрешения отключено пользователем 28.10 (~/.claude/decisions.md, no-blocking-prompts): " +
		"этот вызов не пройдёт. Сделай то же иначе в своих правах или назови в ответе, что нужно и зачем."
)

// noPromptEvent — поля stdin, по которым хук решает и пишет строку в лог
type noPromptEvent struct {
	SessionID      string `json:"session_id"`
	HookEventName  string `json:"hook_event_name"`
	PermissionMode string `json:"permission_mode"`
	AgentID        string `json:"agent_id"`
	ToolName       string `json:"tool_name"`
	MCPServerName  string `json:"mcp_server_name"`
	Mode           string `json:"mode"`
}

// runNoPrompt печатает отказ для события из stdin. Незнакомое событие —
// ошибка регистрации в settings.json: окно тогда остаётся как было
func runNoPrompt(data []byte, logger core.Logger, out io.Writer) error {
	var event noPromptEvent
	if err := json.Unmarshal(data, &event); err != nil {
		return fmt.Errorf("parse input: %w", err)
	}
	output := map[string]any{"hookEventName": event.HookEventName}
	switch event.HookEventName {
	case "PreToolUse":
		if event.ToolName != "AskUserQuestion" {
			return fmt.Errorf("no-prompt: PreToolUse only for AskUserQuestion, got %q", event.ToolName)
		}
		output["permissionDecision"] = "deny"
		output["permissionDecisionReason"] = askDeniedReason
	case "PermissionRequest":
		output["decision"] = map[string]any{"behavior": "deny", "message": permissionDeniedMessage, "interrupt": false}
	case "Elicitation":
		output["action"] = "decline"
	default:
		return fmt.Errorf("no-prompt: unsupported hook event %q", event.HookEventName)
	}
	logger.Info("prompt denied", "session", event.SessionID, "hook", event.HookEventName, "tool", event.ToolName,
		"mcp_server", event.MCPServerName, "mode", event.Mode, "permission_mode", event.PermissionMode, "agent", event.AgentID)
	if err := json.NewEncoder(out).Encode(map[string]any{"hookSpecificOutput": output}); err != nil {
		return fmt.Errorf("print output: %w", err)
	}
	return nil
}
