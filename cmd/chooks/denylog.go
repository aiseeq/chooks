package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aiseeq/akit/secretmask"

	"github.com/aiseeq/chooks/internal/core"
	"github.com/aiseeq/chooks/internal/questioncheck"
)

// deniedCommandRunes — предел команды в строке об отказе: heredoc и Write
// бывают в сотни КБ, а для разбора «что агент хотел» хватает начала
const deniedCommandRunes = 2000

// deniedCommand — что хотел отказанный вызов, для журнала отказов (читает обход
// Сна not-taken): команда Bash, у остальных инструментов tool_input целиком.
// Секреты скрыты до среза, значений в лог не идёт
func deniedCommand(input *core.ToolInput) (string, error) {
	if input.ToolName == "Bash" {
		return questioncheck.Excerpt(input.Command, deniedCommandRunes), nil
	}
	text, err := maskedToolInput(input.ToolInput)
	if err != nil {
		return "", err
	}
	return questioncheck.Excerpt(text, deniedCommandRunes), nil
}

// deniedCallFields — поля строки об отказе. Команду, которую не разобрать, в журнал не
// несём: вместо неё причина
func deniedCallFields(input *core.ToolInput) []any {
	fields := []any{"session", input.SessionID, "tool", input.ToolName}
	command, err := deniedCommand(input)
	if err != nil {
		return append(fields, "command_error", err.Error())
	}
	return append(fields, "command", command)
}

// maskedToolInput маскирует каждую строку tool_input разобранной: в сыром JSON перевод
// строки — литерал \n, он склеивается со следующим словом, и маска не узнаёт инструмент
// в "#!/bin/sh\ncurl -u a:b". Значение рядом с ключом (`"password":"x"`) скрывает
// следующий проход Excerpt по собранному JSON
func maskedToolInput(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber() // большое число не превращается в 5e+06
	var value any
	if err := decoder.Decode(&value); err != nil {
		return "", fmt.Errorf("decode tool_input: %w", err)
	}
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false) // команда читается как есть: & и < без &
	if err := encoder.Encode(maskStrings(value)); err != nil {
		return "", fmt.Errorf("encode tool_input: %w", err)
	}
	return strings.TrimSuffix(out.String(), "\n"), nil
}

func maskStrings(value any) any {
	switch typed := value.(type) {
	case string:
		masked, _ := secretmask.Mask(typed)
		return masked
	case []any:
		for i, item := range typed {
			typed[i] = maskStrings(item)
		}
	case map[string]any:
		for key, item := range typed {
			typed[key] = maskStrings(item)
		}
	}
	return value
}
