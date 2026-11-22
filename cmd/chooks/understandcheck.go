package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/aiseeq/chooks/internal/core"
	"github.com/aiseeq/chooks/internal/hookctx"
	"github.com/aiseeq/chooks/internal/questioncheck"
	"github.com/aiseeq/chooks/internal/understandcheck"
)

// understandCheckTool — ключ в config.yaml. Отсутствие ключа — включено:
// включает проверку строка хука в settings.json
const understandCheckTool = "understand_check"

// Судья проверки понимания — сравнение на 182 случаях сухого прогона 02.11
// (тикет understand-check-dryrun): opus/medium дал 63 верные поправки и 35
// ложных против 66 и 44 у opus/high, пропусков 4 против 1
const (
	understandModel  = "opus"
	understandEffort = "medium"
)

// understandBudget — сколько ждать судью. Таймаут хука в settings.json — 40 с:
// запас на чтение хвоста транскрипта и запуск
const understandBudget = 30 * time.Second

// preToolDenyOutput — ответ PreToolUse, отклоняющий вызов: агент получает
// причину вместо результата инструмента
type preToolDenyOutput struct {
	HookSpecificOutput preToolDeny `json:"hookSpecificOutput"`
}

type preToolDeny struct {
	HookEventName            string `json:"hookEventName"`
	PermissionDecision       string `json:"permissionDecision"`
	PermissionDecisionReason string `json:"permissionDecisionReason"`
}

// runUnderstandCheck сверяет письмо или правку инструкций с репликой
// пользователя. Сбои только логируются: вызов проходит
func runUnderstandCheck(ctx context.Context, data []byte, config *core.Config, logger core.Logger, out io.Writer) {
	if tool, exists := config.Tools[understandCheckTool]; exists && !tool.Enabled {
		return
	}
	input, err := core.ParseToolInput(data)
	if err != nil {
		logger.Warn("understand check input unreadable, call let through", "error", err)
		return
	}
	// transcript_path остаётся как его дал запрос: он пишется в журнал и в
	// текст ошибки, а открывается от cwd запроса (ReadTurn)
	hook, err := newUnderstandHook(ctx, logger)
	if err != nil {
		logger.Warn("understand check unavailable", "error", err)
		return
	}
	outcome := hook.Run(ctx, understandCall(ctx, input, logger))
	writeUnderstandOutcome(outcome, input, logger, out)
}

// writeUnderstandOutcome печатает отказ, если судья нашёл расхождение
func writeUnderstandOutcome(outcome understandcheck.Outcome, input *core.ToolInput, logger core.Logger, out io.Writer) {
	switch {
	case outcome.Deny:
		logger.Info("understand check denied call", deniedCallFields(input)...)
		deny := preToolDenyOutput{HookSpecificOutput: preToolDeny{
			HookEventName:            "PreToolUse",
			PermissionDecision:       "deny",
			PermissionDecisionReason: outcome.Reason,
		}}
		if err := json.NewEncoder(out).Encode(deny); err != nil {
			logger.Warn("understand check deny not printed", "error", err)
		}
	case outcome.Skip != "":
		logger.Debug("understand check skipped", "session", input.SessionID, "tool", input.ToolName, "reason", outcome.Skip)
	default:
		logger.Info("understand check let call through", "session", input.SessionID, "tool", input.ToolName, "verdict", outcome.Verdict)
	}
}

// understandCall — вызов из входа PreToolUse; имя сессии — по CLAUDE_PID,
// только для журнала
func understandCall(ctx context.Context, input *core.ToolInput, logger core.Logger) understandcheck.Call {
	call := understandcheck.Call{
		SessionID:      input.SessionID,
		TranscriptPath: input.TranscriptPath,
		AgentID:        input.AgentID,
		Tool:           input.ToolName,
		Input:          input.ToolInput,
	}
	request, hasRequest := hookctx.Request(ctx)
	if !hasRequest {
		return call
	}
	pid := request.Getenv("CLAUDE_PID")
	if pid != "" {
		home := request.Getenv("HOME")
		if home == "" {
			return call
		}
		home = request.Abs(home)
		var err error
		call.Name, err = questioncheck.SessionName(filepath.Join(home, ".claude"), pid)
		if err != nil {
			logger.Debug("understand check session name unknown", "pid", pid, "error", err)
		}
	}
	return call
}

// newUnderstandHook собирает проверку с путями по умолчанию
func newUnderstandHook(ctx context.Context, logger core.Logger) (understandcheck.Hook, error) {
	request, ok := hookctx.Request(ctx)
	if !ok {
		return understandcheck.Hook{}, fmt.Errorf("hook request is missing")
	}
	journal, denied, err := understandcheck.DefaultPaths(request)
	if err != nil {
		return understandcheck.Hook{}, err
	}
	judge, err := newJudgeForRequest(understandModel, understandEffort, request)
	if err != nil {
		return understandcheck.Hook{}, err
	}
	judge.Prompt = understandcheck.Prompt
	return understandcheck.Hook{
		Judge:       judge,
		JournalPath: journal,
		DeniedDir:   denied,
		Budget:      understandBudget,
		Now:         time.Now,
		Warn:        logger.Warn,
	}, nil
}
