package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/aiseeq/chooks/internal/core"
	"github.com/aiseeq/chooks/internal/nighthint"
)

// hookNightHint — подкоманда ночной подсказки; включает её регистрация в
// settings.json, установка бинаря ничего не включает
const hookNightHint = "night-hint"

// nightHintTool — ключ в config.yaml; без ключа включено
const nightHintTool = "night_hint"

// nightHintEvent — поля stdin ночной подсказки
type nightHintEvent struct {
	HookEventName        string `json:"hook_event_name"`
	SessionID            string `json:"session_id"`
	ToolName             string `json:"tool_name"`
	StopHookActive       bool   `json:"stop_hook_active"`
	LastAssistantMessage string `json:"last_assistant_message"`
	Source               string `json:"source"`
}

// nightHintDeps — живые время, признак блокировки и записи
func nightHintDeps() (nighthint.Deps, error) {
	dir, err := nighthint.DefaultDir()
	if err != nil {
		return nighthint.Deps{}, err
	}
	return nighthint.Deps{Now: time.Now, Locked: nighthint.ScreenLocked, Store: nighthint.Store{Dir: dir}}, nil
}

func newNightHintCmd(exitCode *int) *cobra.Command {
	var probe bool
	cmd := &cobra.Command{
		Use:   hookNightHint,
		Short: "Ночью при заблокированном экране подсказать сессии: пользователь спит, вопрос — тикетом админу пространства (SessionStart, UserPromptSubmit, PreToolUse AskUserQuestion, Stop с ❓)",
		Long: "Подсказка в additionalContext, не отказ. Признак «спит» — экран заблокирован (LockedHint сессии seat0 в logind) и местное время 22:00–09:00. " +
			"SessionStart и UserPromptSubmit — раз за ночь на сессию, PreToolUse AskUserQuestion — на каждый вызов, Stop — если в последнем ответе есть строка с ❓, раз за ход. " +
			"--probe печатает признак блокировки и время его получения.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if probe {
				return probeNightHint(cmd.Context(), cmd.OutOrStdout())
			}
			code, err := runHook(cmd.Context(), hookNightHint)
			*exitCode = code
			return err
		},
	}
	cmd.Flags().BoolVar(&probe, "probe", false, "напечатать признак блокировки экрана, время вызова и, ночь ли сейчас")
	return cmd
}

// probeNightHint — живая проба признака на машине
func probeNightHint(ctx context.Context, out io.Writer) error {
	start := time.Now()
	locked, err := nighthint.ScreenLocked(ctx)
	elapsed := time.Since(start)
	if err != nil {
		return fmt.Errorf("screen lock state unknown after %s: %w", elapsed.Round(time.Microsecond), err)
	}
	fmt.Fprintf(out, "locked=%t elapsed=%s night=%t now=%s\n", locked, elapsed.Round(time.Microsecond), nighthint.IsNight(start), start.Format("15:04:05 MST"))
	return nil
}

// runNightHint печатает подсказку для события из stdin. Сбои только
// логируются: подсказка ничего не держит, а ошибка хука показалась бы
// человеку на каждом ходу
func runNightHint(ctx context.Context, data []byte, config *core.Config, logger core.Logger, out io.Writer, deps nighthint.Deps) {
	if tool, exists := config.Tools[nightHintTool]; exists && !tool.Enabled {
		return
	}
	var input nightHintEvent
	if err := json.Unmarshal(data, &input); err != nil {
		logger.Warn("night hint input unreadable", "hook", hookNightHint, "error", err)
		return
	}
	// Неинтерактивную сессию (судья, партия claude -p) человек не ждёт:
	// спросить в ней некого, подсказка только сбила бы задачу
	printMode, err := core.PrintModeSession()
	if err != nil {
		logger.Warn("session mode unknown, assuming interactive", "hook", hookNightHint, "error", err)
	}
	if printMode {
		logger.Debug("night hint skipped", "event", input.HookEventName, "session", input.SessionID, "reason", "неинтерактивная сессия claude -p")
		return
	}

	result, err := nighthint.Run(ctx, deps, nighthint.Event{
		Name:           input.HookEventName,
		SessionID:      input.SessionID,
		ToolName:       input.ToolName,
		LastMessage:    input.LastAssistantMessage,
		StopHookActive: input.StopHookActive,
		Source:         input.Source,
	})
	if err != nil {
		logger.Warn("night hint failed", "event", input.HookEventName, "session", input.SessionID, "error", err)
		return
	}
	if result.Damaged != nil {
		logger.Warn("night hint record replaced", "event", input.HookEventName, "session", input.SessionID, "error", result.Damaged)
	}
	if result.LockErr != nil {
		logger.Warn("night hint skipped: screen lock state unknown", "event", input.HookEventName, "session", input.SessionID, "error", result.LockErr)
		return
	}
	if result.Context == "" {
		logger.Debug("night hint skipped", "event", input.HookEventName, "session", input.SessionID, "reason", result.Skip)
		return
	}

	logger.Info("night hint given", "event", input.HookEventName, "session", input.SessionID, "tool", input.ToolName)
	var output hookContextOutput
	output.HookSpecificOutput.HookEventName = input.HookEventName
	output.HookSpecificOutput.AdditionalContext = result.Context
	if err := json.NewEncoder(out).Encode(output); err != nil {
		logger.Warn("night hint not printed", "error", err)
	}
	if err := deps.Store.Sweep(deps.Now()); err != nil {
		logger.Warn("night hint stale records not swept", "error", err)
	}
}

// defaultNightHint — обработчик для runHook: живые зависимости
func defaultNightHint(ctx context.Context, data []byte, config *core.Config, logger core.Logger) {
	deps, err := nightHintDeps()
	if err != nil {
		logger.Warn("night hint state dir unavailable", "error", err)
		return
	}
	runNightHint(ctx, data, config, logger, os.Stdout, deps)
}
