package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aiseeq/chooks/internal/core"
	"github.com/aiseeq/chooks/internal/hookctx"
	"github.com/aiseeq/chooks/internal/tools"
)

// wideSearchHintTool — ключ в config.yaml; без ключа включено (тикет
// wide-search-hint, «да» пользователя 20.11 12:17 CET), действует там, куда
// settings.json направляет PreToolUse (Bash, Monitor)
const wideSearchHintTool = "wide_search_hint"

// wideSearchContext — подсказка модели, если команда рекурсивно обходит
// большое дерево диска (tools.WideSearch); вызов не прерывается. Пустая
// строка — подсказки нет, при ошибке её тоже нет
func wideSearchContext(ctx context.Context, input *core.ToolInput, config *core.Config, logger core.Logger) (string, error) {
	if tool, exists := config.Tools[wideSearchHintTool]; exists && !tool.Enabled {
		return "", nil
	}
	if input.ToolName != "Bash" && input.ToolName != "Monitor" {
		return "", nil
	}
	var toolInput struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(input.ToolInput, &toolInput); err != nil {
		return "", fmt.Errorf("tool input unreadable: %w", err)
	}
	home, err := hookctx.UserHomeDir(ctx)
	if err != nil {
		return "", fmt.Errorf("home unknown: %w", err)
	}
	walk, err := tools.WideSearch(toolInput.Command, input.CWD, home)
	if err != nil || walk == "" {
		return "", err
	}
	logger.Info("wide search hint", "session", input.SessionID, "walk", walk)
	return walk + ": " + tools.WideSearchHintText, nil
}
