package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/aiseeq/chooks/internal/core"
	"github.com/aiseeq/chooks/internal/webfallback"
)

// webFallbackTool — ключ в config.yaml; без ключа включено, включает
// регистрация подкоманды в settings.json
const webFallbackTool = "webfetch_fallback"

// postToolUseOutput — ответ PostToolUse с подменой результата инструмента
type postToolUseOutput struct {
	HookSpecificOutput struct {
		HookEventName     string             `json:"hookEventName"`
		UpdatedToolOutput *webfallback.Fetch `json:"updatedToolOutput"`
	} `json:"hookSpecificOutput"`
}

// runWebFetchFallback подменяет отказ сайта в ответе WebFetch текстом из
// webtext. Обычный ответ проходит без вызова утилиты; сбой утилиты только
// логируется, и ответ WebFetch остаётся как был
func runWebFetchFallback(ctx context.Context, data []byte, config *core.Config, logger core.Logger, out io.Writer) {
	if tool, exists := config.Tools[webFallbackTool]; exists && !tool.Enabled {
		return
	}
	var event webfallback.Event
	var fetch webfallback.Fetch
	if err := json.Unmarshal(data, &event); err != nil || event.ToolName != "WebFetch" {
		return
	}
	if err := json.Unmarshal(event.ToolResponse, &fetch); err != nil {
		logger.Warn("webfetch response unreadable", "hook", hookWebFetchFallback, "error", err)
		return
	}
	if webfallback.Reason(fetch) == "" {
		return
	}

	start := time.Now()
	runCtx, cancel := context.WithTimeout(ctx, webfallback.Budget)
	defer cancel()
	updated, outcome, err := webfallback.Fallback(runCtx, fetch, webfallback.ExecRunner)
	fields := []any{"session", event.SessionID, "url", webfallback.LogURL(fetch.URL), "reason", outcome.Reason, "ms", time.Since(start).Milliseconds()}
	switch {
	case errors.Is(err, webfallback.ErrNoWebtext):
		logger.Info("webfetch fallback skipped", append(fields, "error", err)...)
		return
	case err != nil:
		logger.Warn("webfetch fallback failed, response left as is", append(fields, "error", err)...)
		return
	}
	logger.Info("webfetch fallback", append(fields, "opened", outcome.Opened, "source", outcome.Source)...)

	var output postToolUseOutput
	output.HookSpecificOutput.HookEventName = "PostToolUse"
	output.HookSpecificOutput.UpdatedToolOutput = updated
	if err := json.NewEncoder(out).Encode(output); err != nil {
		logger.Warn("webfetch fallback output not written", append(fields, "error", err)...)
	}
}
