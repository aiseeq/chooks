package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aiseeq/chooks/internal/atomicfile"
	"github.com/aiseeq/chooks/internal/claimcheck"
	"github.com/aiseeq/chooks/internal/core"
	"github.com/aiseeq/chooks/internal/desktop"
	"github.com/aiseeq/chooks/internal/hookctx"
	"github.com/aiseeq/chooks/internal/questioncheck"
	"github.com/aiseeq/chooks/internal/tools/notifier"
	"github.com/aiseeq/chooks/internal/understandcheck"
)

// claimCheckTool — ключ в config.yaml. Отсутствие ключа — включено: ответы
// включает строка claim-stop в settings.json, письма — строка understand-check
const claimCheckTool = "claim_check"

// Судья claim-check — luna/high, промпт v3: пользователь 22.11 23:41 CET (tl
// 27d21ddd:21337) «Включайте хук на luna high v3»
const (
	claimModel  = "luna"
	claimEffort = "high"
)

// Бюджеты судьи вместе с ожиданием места: ответ — 100 с при таймауте
// claim-stop в settings 120 с, письмо — 75 с при таймауте understand-check
// 90 с (configs/claim-check-hooks.json)
const (
	claimStopBudget = 100 * time.Second
	claimSendBudget = 75 * time.Second
)

// claimDepsKey — ключ ctx запроса с claimDeps: тест подставляет судей,
// уведомление и сроки одному запросу
type claimDepsKey struct{}

// claimDeps — подмена для claim-check и understand-check одного запроса;
// пустое поле — настоящее
type claimDeps struct {
	judge            claimcheck.Asker
	understand       understandcheck.Asker
	notify           func(title, message string) error
	stopBudget       time.Duration
	sendBudget       time.Duration
	understandBudget time.Duration
	slots            int
}

func claimDepsOf(ctx context.Context) claimDeps {
	deps, _ := ctx.Value(claimDepsKey{}).(claimDeps)
	return deps
}

func claimEnabled(config *core.Config) bool {
	tool, exists := config.Tools[claimCheckTool]
	return !exists || tool.Enabled
}

// codexAsker — CLIJudge с учёткой вызова: CODEX_HOME — каталог учётки
type codexAsker struct{ judge questioncheck.CLIJudge }

func (a codexAsker) Model() string { return a.judge.Model() }

func (a codexAsker) Ask(ctx context.Context, account, input string) (string, questioncheck.Usage, error) {
	judge := a.judge
	judge.CodexHome = account
	return judge.Ask(ctx, input)
}

// newClaimHook собирает проверку с путями запроса
func newClaimHook(ctx context.Context, config *core.Config, logger core.Logger, budget time.Duration) (claimcheck.Hook, error) {
	request, ok := hookctx.Request(ctx)
	if !ok {
		return claimcheck.Hook{}, fmt.Errorf("hook request is missing")
	}
	stateDir, err := request.StateDir()
	if err != nil {
		return claimcheck.Hook{}, err
	}
	slotDir := filepath.Join(request.RuntimeDir(), "claimcheck")
	deps := claimDepsOf(ctx)
	judge := deps.judge
	if judge == nil {
		cli, err := newJudgeForRequest(claimModel, claimEffort, request)
		if err != nil {
			return claimcheck.Hook{}, err
		}
		cli.Prompt = claimcheck.JudgePrompt
		cli.OutputSchema, err = writeClaimSchema(slotDir)
		if err != nil {
			return claimcheck.Hook{}, err
		}
		judge = codexAsker{judge: cli}
	}
	accounts, err := claimAccounts(config, request)
	if err != nil {
		return claimcheck.Hook{}, err
	}
	workerMarks, err := workerAccountMarks(request)
	if err != nil {
		return claimcheck.Hook{}, err
	}
	notify := deps.notify
	if notify == nil {
		notify = func(title, message string) error { return claimAlert(request, title, message) }
	}
	return claimcheck.Hook{
		Judge:       judge,
		Accounts:    accounts,
		WorkerMarks: workerMarks,
		StateDir:    stateDir,
		SlotDir:     slotDir,
		Slots:       deps.slots,
		Budget:      budget,
		Now:         func() time.Time { return time.Now().In(request.Location()) },
		Warn:        logger.Warn,
		Notify:      notify,
	}, nil
}

// writeClaimSchema кладёт схему ответа рядом с местами судей: codex читает
// её из файла. Файл переписывается, только если отличается
func writeClaimSchema(dir string) (string, error) {
	path := filepath.Join(dir, "verdict-schema.json")
	if current, err := os.ReadFile(path); err == nil && bytes.Equal(current, claimcheck.Schema) {
		return path, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create claim check dir: %w", err)
	}
	if err := atomicfile.Write(path, claimcheck.Schema, 0o600); err != nil {
		return "", fmt.Errorf("write judge schema: %w", err)
	}
	return path, nil
}

// claimAccounts — каталоги учёток codex из config.yaml или по умолчанию:
// рабочая ~/.codex, затем личная ~/.codex-personal (пользователь 03.11 12:48,
// tl 07482c31:314: «Использовать сейчас эти подписки ТОЛЬКО для luna»)
func claimAccounts(config *core.Config, request hookctx.Context) ([]string, error) {
	homes := config.Tools[claimCheckTool].CodexHomes
	if len(homes) == 0 {
		homes = []string{"~/.codex", "~/.codex-personal"}
	}
	home := request.Path(hookctx.EnvHome)
	out := make([]string, 0, len(homes))
	for _, dir := range homes {
		if dir == "~" || strings.HasPrefix(dir, "~/") {
			if home == "" {
				return nil, fmt.Errorf("cannot expand %q: $HOME is not defined", dir)
			}
			dir = filepath.Join(home, strings.TrimPrefix(dir, "~"))
		}
		out = append(out, request.Abs(dir))
	}
	return out, nil
}

// workerAccountMarks — метки учёток worker (llmark worker codexaccounts.go):
// work — ~/.codex, personal — ~/.codex-personal; корень — $WORKER_STATE или
// ~/.local/state/worker. Метка worker «нет кредитов» — та же учётка у той же
// подписки: хук не тратит на неё первый вызов
func workerAccountMarks(request hookctx.Context) (map[string]string, error) {
	home := request.Path(hookctx.EnvHome)
	if home == "" {
		return nil, fmt.Errorf("cannot resolve worker account marks: $HOME is not defined")
	}
	root := request.Getenv("WORKER_STATE")
	if root == "" {
		root = filepath.Join(home, ".local", "state", "worker")
	}
	root = request.Abs(root)
	return map[string]string{
		filepath.Join(home, ".codex"):          filepath.Join(root, "accounts", "work.json"),
		filepath.Join(home, ".codex-personal"): filepath.Join(root, "accounts", "personal.json"),
	}, nil
}

// claimAlert — уведомление на рабочий стол о сбоях судьи подряд, в фоне
func claimAlert(request hookctx.Context, title, message string) error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to locate own executable: %w", err)
	}
	return desktop.DeliverInBackgroundForRequest(executable, notifier.WatchCommand, warningAlert(title, message), request)
}

// warningAlert — предупреждение chooks на рабочий стол: сбои судьи
// claim-check, уход хуков мимо демона
func warningAlert(title, message string) desktop.Alert {
	return desktop.Alert{Title: title, Message: message, AppName: "chooks", Icon: "dialog-warning",
		Urgency: desktop.UrgencyNormal, Desktop: true}
}

// sessionNameOf — имя сессии Claude Code по CLAUDE_PID, только для журнала
func sessionNameOf(ctx context.Context, logger core.Logger) string {
	request, ok := hookctx.Request(ctx)
	if !ok {
		return ""
	}
	pid := request.Getenv("CLAUDE_PID")
	home := request.Path(hookctx.EnvHome)
	if pid == "" || home == "" {
		return ""
	}
	name, err := questioncheck.SessionName(filepath.Join(home, ".claude"), pid)
	if err != nil {
		logger.Debug("claim check session name unknown", "pid", pid, "error", err)
		return ""
	}
	return name
}

// runClaimStop — Stop-хук claim-stop: ответ с утверждениями без проверки не
// заканчивает ход, агент получает флаги. Сбой судьи ход не держит
func runClaimStop(ctx context.Context, data []byte, config *core.Config, logger core.Logger, out io.Writer) {
	if !claimEnabled(config) {
		return
	}
	request, ok := hookctx.Request(ctx)
	if !ok {
		logger.Warn("claim check: hook request is missing")
		return
	}
	input := parseSessionInputForRequest(logger, hookClaimStop, data, &request)
	if input.AgentID != "" {
		return
	}
	budget := claimStopBudget
	if deps := claimDepsOf(ctx); deps.stopBudget > 0 {
		budget = deps.stopBudget
	}
	hook, err := newClaimHook(ctx, config, logger, budget)
	if err != nil {
		logger.Warn("claim check unavailable, stop let through", "error", err)
		return
	}
	outcome := hook.Stop(ctx, claimcheck.StopEvent{
		SessionID:      input.SessionID,
		Name:           sessionNameOf(ctx, logger),
		TranscriptPath: input.TranscriptPath,
		StopHookActive: input.StopHookActive,
		LastMessage:    input.LastAssistantMessage,
	})
	logger.Info("claim check stop", "session", input.SessionID, "outcome", outcome.Result)
	if !outcome.Block {
		return
	}
	if err := json.NewEncoder(out).Encode(stopBlockOutput{Decision: "block", Reason: outcome.Reason}); err != nil {
		logger.Warn("claim check block not printed", "error", err)
	}
}

// runClaimSend судит письмо SendMessage; не письмо или пустой текст — без
// судьи
func runClaimSend(ctx context.Context, input *core.ToolInput, config *core.Config, logger core.Logger) claimcheck.Outcome {
	var call struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(input.ToolInput, &call); err != nil {
		logger.Warn("claim check letter unreadable, call let through", "error", err)
		return claimcheck.Outcome{Result: claimcheck.OutcomeError}
	}
	if strings.TrimSpace(call.Message) == "" {
		return claimcheck.Outcome{Result: claimcheck.OutcomeSkip}
	}
	budget := claimSendBudget
	if deps := claimDepsOf(ctx); deps.sendBudget > 0 {
		budget = deps.sendBudget
	}
	hook, err := newClaimHook(ctx, config, logger, budget)
	if err != nil {
		logger.Warn("claim check unavailable, letter let through", "error", err)
		return claimcheck.Outcome{Result: claimcheck.OutcomeError}
	}
	outcome := hook.Send(ctx, claimcheck.SendEvent{
		SessionID:      input.SessionID,
		Name:           sessionNameOf(ctx, logger),
		TranscriptPath: input.TranscriptPath,
		ToolUseID:      input.ToolUseID,
		Text:           call.Message,
	})
	logger.Info("claim check send", "session", input.SessionID, "outcome", outcome.Result)
	return outcome
}
