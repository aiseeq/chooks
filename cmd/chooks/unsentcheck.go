package main

import (
	"encoding/json"
	"io"

	"github.com/aiseeq/chooks/internal/core"
	"github.com/aiseeq/chooks/internal/unsentcheck"
)

// runUnsentCheck — Stop-хук unsent-check: ответ «текст человеку не
// отправлен» без reddy history в ходе не выпускается (пользователь 29.10:
// «Хорошо, включай»). Сбой чтения транскрипта ход не держит: человек без
// ответа хуже непроверенного статуса, сбой остаётся в логе
func runUnsentCheck(input *core.ToolInput, logger core.Logger, out io.Writer) {
	result, err := unsentcheck.Run(unsentcheck.Event{
		TranscriptPath: input.TranscriptPath,
		AgentID:        input.AgentID,
		StopHookActive: input.StopHookActive,
		LastMessage:    input.LastAssistantMessage,
	})
	switch {
	case err != nil:
		logger.Error("unsent check failed, stop let through", "session", input.SessionID, "claim", result.Claim, "error", err)
	case result.Block:
		logger.Info("unsent claim blocked stop", "session", input.SessionID, "claim", result.Claim)
		if err := json.NewEncoder(out).Encode(stopBlockOutput{Decision: "block", Reason: result.Reason}); err != nil {
			logger.Warn("unsent check block not printed", "error", err)
		}
	case result.Skip != "":
		logger.Debug("unsent check skipped", "session", input.SessionID, "reason", result.Skip)
	case result.Claim != "":
		logger.Info("unsent claim let through, chat opened in turn", "session", input.SessionID, "claim", result.Claim)
	}
}
