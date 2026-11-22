package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/aiseeq/chooks/internal/core"
	"github.com/aiseeq/chooks/internal/hookctx"
	"github.com/aiseeq/chooks/internal/nighthint"
	"github.com/aiseeq/chooks/internal/skillreload"
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

// skillStateWait — сколько ждать блокировку записи skill-reload: её держат
// UserPromptSubmit и PostToolBatch на время чтения транскрипта (бюджет
// skillScanBudget). Ожидание кончается и раньше — к сроку хука (deadline у
// autonomousSkillCalled)
const skillStateWait = time.Second

// nightHintDeps — живые время, признак блокировки, записи и признак вызова
// скилла autonomous-work по записи skill-reload; deadline — срок хука для
// чтения записи
func nightHintDepsForRequest(config *core.Config, deadline time.Time, request hookctx.Context) (nighthint.Deps, error) {
	state, err := request.StateDir()
	if err != nil {
		return nighthint.Deps{}, err
	}
	skillDir, err := skillreload.StateDir(request)
	if err != nil {
		return nighthint.Deps{}, err
	}
	skillCalled := autonomousSkillCalledIn(config, deadline, func() (string, error) { return skillDir, nil })
	return nighthint.Deps{Now: time.Now, Locked: nighthint.ScreenLocked, LidClosed: nighthint.LidClosed, Store: nighthint.Store{Dir: filepath.Join(state, "night-hint")}, SkillCalled: skillCalled}, nil
}

// autonomousSkillCalled — признак вызова скилла autonomous-work в сессии по
// записи хука skill-reload. Записи нет — скилл не вызван: вызов Skill всегда
// даёт PostToolBatch, а он запись создаёт. Первый ход новой сессии записи ещё
// не имеет — её создаёт user-prompt-submit параллельно с night-hint на том же
// UserPromptSubmit, — и этот ход подсказку получает. Выключенный skill_reload
// записей не ведёт: признак — ErrSkillUnknown, подсказка идёт по правилу «раз
// за ночь»; так же — при сбое чтения существующей записи.
//
// Запись ждётся не дольше deadline — срока хука от старта процесса, как у
// skill-reload: до неё UserPromptSubmit уже ждал запись night-hint дважды (по
// lockWait) и logind (lockTimeout), и без срока сумма выходила за таймаут хука
// 3 с из settings.json (тикет night-hint-night-work-budget). Срок наступил —
// запись не читается, подсказка идёт по правилу «раз за ночь», причина — в лог
func autonomousSkillCalledIn(config *core.Config, deadline time.Time, stateDir func() (string, error)) func(sessionID string) (bool, error) {
	return func(sessionID string) (bool, error) {
		if tool, exists := config.Tools[skillReloadTool]; exists && !tool.Enabled {
			return false, fmt.Errorf("%s disabled in config: %w", skillReloadTool, nighthint.ErrSkillUnknown)
		}
		now := time.Now()
		if !now.Before(deadline) {
			return false, fmt.Errorf("skill-reload record not read, hook deadline %s passed: %w", deadline.Format("15:04:05.000"), skillreload.ErrNoTime)
		}
		dir, err := stateDir()
		if err != nil {
			return false, err
		}
		wait := now.Add(skillStateWait)
		if deadline.Before(wait) {
			wait = deadline
		}
		// LockFile ограничивает ожидание блокировки, но обычное чтение после
		// захвата может задержаться на диске. Оно только читает запись, поэтому
		// уносим его в горутину и возвращаемся к абсолютному сроку хука.
		type skillCallResult struct {
			called bool
			err    error
		}
		result := make(chan skillCallResult, 1)
		go func() {
			called, err := skillreload.SkillCalled(dir, sessionID, nighthint.AutonomousSkills(), wait)
			result <- skillCallResult{called: called, err: err}
		}()
		var called bool
		select {
		case answer := <-result:
			called, err = answer.called, answer.err
		case <-time.After(time.Until(deadline)):
			return false, fmt.Errorf("skill-reload record read abandoned at hook deadline: %w", skillreload.ErrNoTime)
		}
		if errors.Is(err, skillreload.ErrNoState) {
			return false, nil
		}
		return called, err
	}
}

func newNightHintCmd(exitCode *int) *cobra.Command {
	var probe bool
	cmd := &cobra.Command{
		Use:   hookNightHint,
		Short: "Ночью при заблокированном экране или закрытой крышке подсказать сессии: пользователь спит, вопрос — тикетом админу пространства (SessionStart, UserPromptSubmit, PreToolUse AskUserQuestion, Stop с ❓)",
		Long: "Подсказка в additionalContext, не отказ. Признак «спит» — экран заблокирован (LockedHint сессии seat0) или крышка закрыта (LidClosed logind), и местное время 22:00–09:00. " +
			"SessionStart — раз за ночь на сессию и после сжатия и /clear; UserPromptSubmit — на каждом ходе, пока в сессии не вызван скилл autonomous-work или night-work (sc2go) (по записи skill-reload; записи нет — не вызван), после вызова, при выключенном skill_reload, сбое чтения записи или исчерпанном сроке хука — раз за ночь; " +
			"PreToolUse AskUserQuestion — на каждый вызов, Stop — если в последнем ответе есть строка с ❓, раз за ход. " +
			"--probe печатает признаки блокировки и крышки и время их получения.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if probe {
				return probeNightHint(cmd.Context(), cmd.OutOrStdout())
			}
			code, err := runHook(hookCommandContext(cmd), hookNightHint)
			*exitCode = code
			return err
		},
	}
	cmd.Flags().BoolVar(&probe, "probe", false, "напечатать признаки блокировки экрана и крышки, время вызова и, ночь ли сейчас")
	return cmd
}

// probeNightHint — живая проба признаков на машине.
func probeNightHint(ctx context.Context, out io.Writer) error {
	return probeNightHintWith(ctx, out, nighthint.ScreenLocked, nighthint.LidClosed)
}

func probeNightHintWith(ctx context.Context, out io.Writer, lockedFn, lidFn func(context.Context) (bool, error)) error {
	start := time.Now()
	locked, err := lockedFn(ctx)
	if err != nil {
		return fmt.Errorf("screen lock state unknown after %s: %w", time.Since(start).Round(time.Microsecond), err)
	}
	closed, err := lidFn(ctx)
	elapsed := time.Since(start)
	if err != nil {
		return fmt.Errorf("lid state unknown after %s: %w", elapsed.Round(time.Microsecond), err)
	}
	_, err = fmt.Fprintf(out, "locked=%t lid_closed=%t elapsed=%s night=%t now=%s\n", locked, closed, elapsed.Round(time.Microsecond), nighthint.IsNight(start), start.Format("15:04:05 MST"))
	return err
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
	request, ok := hookctx.Request(ctx)
	if !ok {
		logger.Error("hook request is missing", "hook", hookNightHint)
		return
	}
	printMode, err := core.PrintModeSessionForPID(request.Getenv("CLAUDE_PID"))
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
	if result.SkillErr != nil {
		// Выключенный признак — выбор конфига, сбой чтения записи — нет
		if errors.Is(result.SkillErr, nighthint.ErrSkillUnknown) {
			logger.Debug("night hint once per night: skill call unknown", "event", input.HookEventName, "session", input.SessionID, "reason", result.SkillErr)
		} else {
			logger.Warn("night hint once per night: skill call unknown", "event", input.HookEventName, "session", input.SessionID, "error", result.SkillErr)
		}
	}
	if result.LockErr != nil {
		logger.Warn("night hint skipped: screen lock state unknown", "event", input.HookEventName, "session", input.SessionID, "reason", result.Skip, "error", result.LockErr)
		return
	}
	if result.LidErr != nil {
		logger.Warn("night hint skipped: lid state unknown", "event", input.HookEventName, "session", input.SessionID, "reason", result.Skip, "error", result.LidErr)
		return
	}
	if result.Context == "" {
		if result.PresenceSkipInfo {
			logger.Info("night hint skipped", "event", input.HookEventName, "session", input.SessionID, "reason", result.Skip)
			return
		}
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
func defaultNightHint(ctx context.Context, data []byte, config *core.Config, logger core.Logger, deadline time.Time, request hookctx.Context) {
	deps, err := nightHintDepsForRequest(config, deadline, request)
	if err != nil {
		logger.Warn("night hint state dir unavailable", "error", err)
		return
	}
	runNightHint(ctx, data, config, logger, request.Stdout, deps)
}
