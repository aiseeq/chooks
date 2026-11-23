package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aiseeq/chooks/internal/claimcheck"

	"github.com/aiseeq/chooks/internal/core"
	"github.com/aiseeq/chooks/internal/hookctx"
	"github.com/aiseeq/chooks/internal/questioncheck"
	"github.com/aiseeq/chooks/internal/understandcheck"
)

// understandCheckTool — ключ в config.yaml. Отсутствие ключа — включено:
// включает проверку строка хука в settings.json
const understandCheckTool = "understand_check"

// Судья проверки понимания — opus/high: пользователь 22.11 (tl
// 27d21ddd:21210) «Ни одну из моделей нельзя использовать менее чем на
// high». Сухой прогон 02.11 на 182 случаях (тикет understand-check-dryrun):
// high — 66 верных поправок, 44 ложных, 1 пропуск; medium — 63, 35 и 4
const (
	understandModel  = "opus"
	understandEffort = "high"
)

// understandBudget — сколько ждать судью понимания. Таймаут хука в
// settings.json — 90 с: судья claim-check письма идёт параллельно со своим
// сроком 75 с (configs/claim-check-hooks.json)
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
// пользователя; письмо SendMessage параллельно судит claim-check. Решения
// объединяются: отказ любого судьи — отказ, причины обоих в одном ответе,
// сбой одного не стирает отказ другого. Сбои только логируются: вызов проходит
func runUnderstandCheck(ctx context.Context, data []byte, config *core.Config, logger core.Logger, out io.Writer) {
	tool, exists := config.Tools[understandCheckTool]
	understandOn := !exists || tool.Enabled
	input, err := core.ParseToolInput(data)
	if err != nil {
		logger.Warn("understand check input unreadable, call let through", "error", err)
		return
	}
	// claim-check открывает транскрипт сам: путь — от cwd запроса.
	// understand-check получает путь как его дал запрос (ReadTurn)
	claimInput := *input
	if request, ok := hookctx.Request(ctx); ok {
		resolveHookInput(&claimInput, request)
	}
	results := runChecks(ctx, input, &claimInput, understandOn, config, logger)
	writeCheckOutcomes(results.understand, results.claim, input, logger, out)
}

type checkResults struct {
	understand understandcheck.Outcome
	claim      claimcheck.Outcome
}

// runChecks запускает судей понимания и claim-check параллельно, у каждого
// свой срок
func runChecks(ctx context.Context, input, claimInput *core.ToolInput, understandOn bool, config *core.Config, logger core.Logger) checkResults {
	var results checkResults
	claimOn := input.ToolName == "SendMessage" && input.AgentID == "" && claimEnabled(config)
	var wg sync.WaitGroup
	if claimOn {
		wg.Go(func() { results.claim = runClaimSend(ctx, claimInput, config, logger) })
	}
	if understandOn {
		// transcript_path остаётся как его дал запрос: он пишется в журнал и
		// в текст ошибки, а открывается от cwd запроса (ReadTurn)
		hook, err := newUnderstandHook(ctx, logger)
		if err != nil {
			logger.Warn("understand check unavailable", "error", err)
		} else {
			results.understand = hook.Run(ctx, understandCall(ctx, input, logger))
		}
	}
	wg.Wait()
	return results
}

// writeUnderstandOutcome печатает отказ, если судья нашёл расхождение
func writeUnderstandOutcome(outcome understandcheck.Outcome, input *core.ToolInput, logger core.Logger, out io.Writer) {
	writeCheckOutcomes(outcome, claimcheck.Outcome{}, input, logger, out)
}

// writeCheckOutcomes печатает один отказ с причинами всех отказавших судей
func writeCheckOutcomes(outcome understandcheck.Outcome, claim claimcheck.Outcome, input *core.ToolInput, logger core.Logger, out io.Writer) {
	switch {
	case outcome.Deny:
		logger.Info("understand check denied call", deniedCallFields(input)...)
	case outcome.Skip != "":
		logger.Debug("understand check skipped", "session", input.SessionID, "tool", input.ToolName, "reason", outcome.Skip)
	default:
		logger.Info("understand check let call through", "session", input.SessionID, "tool", input.ToolName, "verdict", outcome.Verdict)
	}
	var reasons []string
	if outcome.Deny {
		reasons = append(reasons, outcome.Reason)
	}
	if claim.Block {
		reasons = append(reasons, claim.Reason)
	}
	if len(reasons) == 0 {
		return
	}
	deny := preToolDenyOutput{HookSpecificOutput: preToolDeny{
		HookEventName:            "PreToolUse",
		PermissionDecision:       "deny",
		PermissionDecisionReason: strings.Join(reasons, "\n\n"),
	}}
	if err := json.NewEncoder(out).Encode(deny); err != nil {
		logger.Warn("check deny not printed", "error", err)
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
		home := request.Path(hookctx.EnvHome)
		if home == "" {
			return call
		}
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
	deps := claimDepsOf(ctx)
	budget := understandBudget
	if deps.understandBudget > 0 {
		budget = deps.understandBudget
	}
	var judge understandcheck.Asker = deps.understand
	if judge == nil {
		cli, err := newJudgeForRequest(understandModel, understandEffort, request)
		if err != nil {
			return understandcheck.Hook{}, err
		}
		cli.Prompt = understandcheck.Prompt
		judge = cli
	}
	return understandcheck.Hook{
		Judge:       judge,
		JournalPath: journal,
		DeniedDir:   denied,
		Budget:      budget,
		// Время журнала пишется со смещением: пояс — запроса, как у CLI
		Now:  func() time.Time { return time.Now().In(request.Location()) },
		Warn: logger.Warn,
	}, nil
}
