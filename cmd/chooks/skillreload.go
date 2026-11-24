package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/aiseeq/chooks/internal/core"
	"github.com/aiseeq/chooks/internal/hookctx"
	"github.com/aiseeq/chooks/internal/selflaunch"
	"github.com/aiseeq/chooks/internal/skillreload"
)

// Имена событий в hookSpecificOutput: по ним Claude Code понимает, к какому
// событию относится additionalContext
const (
	eventUserPromptSubmit = "UserPromptSubmit"
	eventPostToolBatch    = "PostToolBatch"
)

// skillReloadTool — ключ в config.yaml. Отсутствие ключа — включено:
// установленный config.yaml при обновлении не перезаписывается, а
// напоминание ничего не блокирует
const skillReloadTool = "skill_reload"

// skillScanBudget — сколько времени событие тратит на чтение транскрипта.
// Таймаут хука 3 секунды; первое чтение сессии в сотни МБ растягивается на
// несколько событий, напоминания ждут, пока транскрипт не дочитан
const skillScanBudget = time.Second

// hookContextOutput — ответ хука с текстом для контекста агента
type hookContextOutput struct {
	HookSpecificOutput struct {
		HookEventName     string `json:"hookEventName"`
		AdditionalContext string `json:"additionalContext"`
	} `json:"hookSpecificOutput"`
}

// remindStaleSkills кладёт в контекст агента правки скиллов и файлов
// инструкций, сделанные после того, как их текст попал в контекст: diff, а
// при большой правке — просьбу перечитать. Сбой только логируется: выдача не
// повод показывать человеку ошибку хука на каждом ходу
func remindStaleSkills(ctx context.Context, eventName string, input *core.ToolInput, config *core.Config, logger core.Logger, deadline time.Time, timer *hookTimer, request hookctx.Context) {
	if tool, exists := config.Tools[skillReloadTool]; exists && !tool.Enabled {
		return
	}
	if input.SessionID == "" || input.TranscriptPath == "" {
		return
	}
	stateDir, err := skillreload.StateDir(request)
	if err != nil {
		logger.Warn("skill reload state dir unavailable", "error", err)
		return
	}

	if !timer.now().Before(timer.deadline(optionalBudget)) {
		logger.Warn("skill reload skipped: hook budget spent", "hook", eventName, "session", input.SessionID, "agent", input.AgentID)
		return
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	startCleanup := func(stateDir, textsDir string, now time.Time) error {
		return selflaunch.StartSkillReloadCleanup(stateDir, textsDir, config.Logger.LogFile, now)
	}
	checker := skillreload.Checker{Context: ctx, StateDir: stateDir, Abs: request.Abs, Location: request.Location(), ScanBudget: skillScanBudget, Deadline: deadline, Substage: timer.setSubstage, StartCleanup: startCleanup}
	var result skillreload.Result
	if awaitSkillCheck(ctx, timer, logger, eventName, input, func() {
		result, err = checker.Check(skillreload.Event{
			SessionID: input.SessionID, AgentID: input.AgentID, AgentType: input.AgentType, TranscriptPath: input.TranscriptPath,
		})
	}) {
		finishSkillCheck(eventName, input, logger, result, err, request.Stdout, request.Location())
	}
}

// awaitSkillCheck ограничивает ожидание Check абсолютным сроком от старта
// процесса. Ушедшая горутина может закончить начатую запись, но хук её не ждёт.
// Паника в Check — паника запроса, пока его ждут, и строка в лог демона после
// (goRequest): процесс демона она не роняет (ревью 2б-1, п. 5)
func awaitSkillCheck(ctx context.Context, timer *hookTimer, logger core.Logger, eventName string, input *core.ToolInput, check func()) bool {
	if !timer.now().Before(timer.deadline(optionalBudget)) {
		logger.Warn("skill reload skipped: hook budget spent", "hook", eventName, "session", input.SessionID, "agent", input.AgentID)
		return false
	}
	task := goRequest(ctx, check)
	wait := time.Until(timer.deadline(skillHardBudget))
	if wait <= 0 {
		task.leave()
		warnSkillAbandoned(timer, logger, eventName, input)
		return false
	}
	select {
	case <-task.done:
		task.wait()
		timer.setSubstage("")
		return true
	case <-time.After(wait):
		task.leave()
		warnSkillAbandoned(timer, logger, eventName, input)
		return false
	}
}

func warnSkillAbandoned(timer *hookTimer, logger core.Logger, eventName string, input *core.ToolInput) {
	logger.Warn("skill reload abandoned: hook budget spent", "hook", eventName, "session", input.SessionID,
		"agent", input.AgentID, "substage", timer.currentSubstage())
}

func finishSkillCheck(eventName string, input *core.ToolInput, logger core.Logger, result skillreload.Result, err error, out io.Writer, loc *time.Location) {
	if errors.Is(err, skillreload.ErrNoTime) {
		// Напоминание останется для следующего события; сбой чтения
		// транскрипта, случившийся после срока, — в поле error
		logger.Warn("skill reload skipped: hook budget spent", "hook", eventName, "session", input.SessionID,
			"agent", input.AgentID, "error", err)
		return
	}
	if errors.Is(err, core.ErrLockBusy) {
		logger.Warn("skill reload skipped: state locked by another hook", "hook", eventName, "session", input.SessionID, "agent", input.AgentID, "error", err)
		return
	}
	if err != nil {
		logger.Warn("skill reload check failed", "hook", eventName, "session", input.SessionID, "agent", input.AgentID, "error", err)
		return
	}
	for _, warning := range result.Warnings {
		logger.Warn("skill reload check degraded", "hook", eventName, "session", input.SessionID, "agent", input.AgentID, "error", warning)
	}
	if len(result.Notices) == 0 {
		return
	}

	names := make([]string, 0, len(result.Notices))
	diffs := 0
	for i := range result.Notices {
		names = append(names, result.Notices[i].Name)
		if result.Notices[i].Diff != "" {
			diffs++
		}
	}
	var output hookContextOutput
	output.HookSpecificOutput.HookEventName = eventName
	output.HookSpecificOutput.AdditionalContext = skillreload.FormatIn(result.Notices, loc)
	if err := json.NewEncoder(out).Encode(output); err != nil {
		logger.Warn("skill reload reminder not printed", "error", err)
		return
	}
	logger.Info("skill reload notified", "hook", eventName, "session", input.SessionID, "agent", input.AgentID, "files", names, "diffs", diffs)
}

func newSkillScanCmd() *cobra.Command {
	var days int
	cmd := &cobra.Command{
		Use:   "skill-scan [--days N] [корень транскриптов]",
		Short: "Сухой прогон выдачи правок скиллов и CLAUDE.md, сделанных после загрузки в контекст",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root := ""
			if len(args) > 0 {
				root = args[0]
			} else {
				home, err := os.UserHomeDir()
				if err != nil {
					return err
				}
				root = filepath.Join(home, ".claude", "projects")
			}
			now := time.Now()
			result, err := skillreload.DryRun(cmd.Context(), root, days, now)
			if err != nil {
				return err
			}
			skillreload.WriteDryRun(cmd.OutOrStdout(), result)

			preview, err := skillreload.PreviewInstall(root, now.Add(-24*time.Hour))
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout())
			skillreload.WritePreview(cmd.OutOrStdout(), preview)
			return nil
		},
	}
	cmd.Flags().IntVar(&days, "days", 7, "сколько последних дней включить")
	return cmd
}
