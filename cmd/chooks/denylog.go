package main

import (
	"github.com/aiseeq/chooks/internal/core"
	"github.com/aiseeq/chooks/internal/questioncheck"
)

// deniedCommandRunes — предел команды в строке об отказе: heredoc и Write
// бывают в сотни КБ, а для разбора «что агент хотел» хватает начала
const deniedCommandRunes = 2000

// deniedCommand — что хотел отказанный вызов, для журнала отказов (читает обход
// Сна not-taken): команда Bash, у остальных инструментов tool_input целиком.
// Секреты скрыты до среза, значений в лог не идёт
func deniedCommand(input *core.ToolInput) string {
	text := input.Command
	if input.ToolName != "Bash" {
		text = string(input.ToolInput)
	}
	return questioncheck.Excerpt(text, deniedCommandRunes)
}

// promptDeniedCommand — то же для события окна разрешения. Неразборчивый
// tool_input не отменяет отказ, только оставляет строку без команды
func promptDeniedCommand(data []byte, logger core.Logger) string {
	input, err := core.ParseToolInput(data)
	if err != nil {
		logger.Warn("prompt denied, command unreadable", "error", err)
		return ""
	}
	return deniedCommand(input)
}
