package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/aiseeq/chooks/internal/core"
	"github.com/aiseeq/chooks/internal/desktop"
	"github.com/aiseeq/chooks/internal/hookctx"
	"github.com/aiseeq/chooks/internal/hookproxy"
	"github.com/aiseeq/chooks/internal/modellaunch"
	"github.com/aiseeq/chooks/internal/processor"
	"github.com/aiseeq/chooks/internal/skillreload"
	"github.com/aiseeq/chooks/internal/statusline"
	"github.com/aiseeq/chooks/internal/textstyle"
	"github.com/aiseeq/chooks/internal/tools"
	"github.com/aiseeq/chooks/internal/tools/notifier"
)

// Exit-коды, которые понимает Claude Code:
// 0 — операция разрешена, 1 — ошибка самого хука (не блокирует),
// 2 — операция заблокирована, stderr передается модели
const (
	exitAllowed = 0
	exitError   = 1
	exitBlocked = 2
)

// Version подставляется через ldflags при сборке.
var Version = "dev"

type cliTerminationKey struct{}

func main() {
	os.Exit(runCLI())
}

// runCLI — CLI в процессе: запрос — сам процесс, причина запасного пути
// прокси (CHOOKS_FALLBACK) — в ctx, из env она снята
func runCLI() int {
	// Некоторые команды не используют cwd и работают даже после удаления
	// текущего каталога, поэтому ошибка Getwd не мешает запуску CLI.
	request, _ := hookctx.FromProcess()
	ctx := context.WithValue(context.Background(), cliTerminationKey{}, (func(int))(os.Exit))
	ctx = context.WithValue(ctx, fallbackReasonKey{}, takeFallbackReason(&request))
	return execute(ctx, request)
}

// execute запускает CLI и возвращает exit-код для Claude Code
func execute(ctx context.Context, request hookctx.Context) int {
	exitCode := exitAllowed

	rootCmd := &cobra.Command{
		Use:           "chooks",
		Short:         "Claude Code hooks processor",
		Long:          "Обработчик хуков Claude Code: уведомления о завершении и ожидании ответа, строка статуса, проверка стиля Jira-комментариев и черновиков текста.",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	rootCmd.SetContext(hookctx.WithRequest(ctx, request))
	rootCmd.SetIn(request.Stdin)
	rootCmd.SetOut(request.Stdout)
	rootCmd.SetErr(request.Stderr)

	rootCmd.PersistentFlags().StringP("config", "c", "", "путь к файлу конфигурации")
	rootCmd.PersistentFlags().BoolP("verbose", "v", false, "подробный вывод")
	rootCmd.PersistentFlags().Duration("timeout", 5*time.Second, "таймаут обработки")

	preToolUse := newHookCmd(hookPreToolUse, "Проверить вызов инструмента до запуска (PreToolUse): kill, rm, rsync, git, модель, стиль Jira", &exitCode)
	preToolUse.Long = preToolUseLong
	rootCmd.AddCommand(
		preToolUse,
		newHookCmd(hookStop, "Обработать Stop hook", &exitCode),
		newHookCmd(hookStopFailure, "Обработать StopFailure hook (ход оборван ошибкой API)", &exitCode),
		newHookCmd(hookNotification, "Обработать Notification hook", &exitCode),
		newHookCmd(hookUserPromptSubmit, "Обработать UserPromptSubmit hook", &exitCode),
		newHookCmd(hookPostToolBatch, "Обработать PostToolBatch hook", &exitCode),

		newHookCmd(hookUnderstandCheck, "Сверить письмо другой сессии или правку инструкций с репликой пользователя (PreToolUse)", &exitCode),
		newHookCmd(hookWebFetchFallback, "Подменить отказ сайта в ответе WebFetch текстом из webtext (PostToolUse), на упавший WebFetch добавить его контекстом (PostToolUseFailure)", &exitCode),
		newHookCmd(hookNoPrompt, "Отказать окну, ждущему ввода человека (PermissionRequest, Elicitation)", &exitCode),
		newHookCmd(hookUnsentCheck, "Не выпустить ответ «текст не отправлен» без reddy history в ходе (Stop)", &exitCode),
		newHookCmd(hookClaimStop, "Не выпустить ответ с утверждениями без проверки: судья claim-check по ходу, флаги — агенту (Stop)", &exitCode),
		newNightHintCmd(&exitCode),
		newNotifyCmd(),
		newStatusLineCmd(&exitCode),
		newLimitsCmd(),
		newAccountCmd(),
		newConfigCmd(),
		newVersionCmd(),
		newStyleCmd(&exitCode),
		newModelScanCmd(),
		newSkillScanCmd(),

		newSaidBenchCmd(),
		newClaimCheckInputCmd(),
		newClaimCheckBenchCmd(),
		newUnderstandCasesCmd(),
		newUnderstandBenchCmd(),
		newCorpusSweepCmd(),
		newDeliverAlertCmd(),
		newSkillReloadCleanupCmd(),
		newServeCmd(),
		newShadowReportCmd(&exitCode),
	)

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(request.Stderr, "chooks: %v\n", err)
		if exitCode == exitAllowed {
			exitCode = exitError
		}
	}

	return exitCode
}

func newSkillReloadCleanupCmd() *cobra.Command {
	var stateDir, textsDir, nowArg string
	cmd := &cobra.Command{
		Use:    "skill-reload-cleanup",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if stateDir == "" || textsDir == "" || nowArg == "" {
				return errors.New("state-dir, texts-dir and now are required")
			}
			now, err := time.Parse(time.RFC3339Nano, nowArg)
			if err != nil {
				return fmt.Errorf("invalid cleanup time: %w", err)
			}
			request, err := commandRequest(cmd)
			if err != nil {
				return err
			}
			config, err := core.LoadConfigForRequest(commandConfigPath(cmd), request)
			if err != nil {
				return err
			}
			logger, err := core.NewLoggerIn(config.Logger, request.Stderr, request.Location())
			if err != nil {
				return err
			}
			defer closeRequestLogger(logger)
			if err := skillreload.CleanupStale(stateDir, textsDir, now); err != nil {
				logger.Error("skill reload cleanup failed", "error", err)
			}
			return nil
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&stateDir, "state-dir", "", "каталог записей skill-reload")
	flags.StringVar(&textsDir, "texts-dir", "", "каталог текстов skill-reload")
	flags.StringVar(&nowArg, "now", "", "время чистки в RFC3339Nano")
	return cmd
}

func newModelScanCmd() *cobra.Command {
	var days int
	cmd := &cobra.Command{
		Use:   "model-scan [--days N] <корень транскриптов>",
		Short: "Проверить недавние вызовы моделей в транскриптах",
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
			logger, err := core.NewLogger(core.DefaultLoggerConfig())
			if err != nil {
				return err
			}
			config, err := core.LoadConfig(commandConfigPath(cmd))
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}
			guard := config.Tools["model_guard"]
			guard.Enabled = true
			validator, err := tools.NewModelGuardTool(guard, logger)
			if err != nil {
				return err
			}
			result, err := tools.ScanModelTranscripts(cmd.Context(), root, days, validator)
			if err != nil {
				return err
			}
			tools.WriteModelScan(cmd.OutOrStdout(), result)
			return nil
		},
	}
	cmd.Flags().IntVar(&days, "days", 7, "сколько последних дней включить")
	return cmd
}

// Имена подкоманд хуков, как их вызывает ~/.claude/settings.json
const (
	hookPreToolUse       = "pre-tool-use"
	hookStop             = "stop"
	hookStopFailure      = "stop-failure"
	hookNotification     = "notification"
	hookUserPromptSubmit = "user-prompt-submit"
	hookPostToolBatch    = "post-tool-batch"

	hookUnderstandCheck  = "understand-check"
	hookWebFetchFallback = "webfetch-fallback"
	hookNoPrompt         = "no-prompt"
	hookUnsentCheck      = "unsent-check"
	hookClaimStop        = "claim-stop"
)

// newHookCmd создает команду обработки хука, записывающую exit-код по указателю
func newHookCmd(hookType, short string, exitCode *int) *cobra.Command {
	return &cobra.Command{
		Use:   hookType,
		Short: short,
		RunE: func(cmd *cobra.Command, args []string) error {
			code, err := runHook(hookCommandContext(cmd), hookType)
			*exitCode = code
			return err
		},
	}
}

func hookCommandContext(cmd *cobra.Command) context.Context {
	ctx := cmd.Context()
	request, ok := hookctx.Request(ctx)
	if ok {
		flags := cmd.Root().PersistentFlags()
		if flags.Lookup("config") != nil {
			if value, err := flags.GetString("config"); err == nil {
				request.ConfigPath = value
			}
		}
		if flags.Lookup("verbose") != nil {
			if value, err := flags.GetBool("verbose"); err == nil {
				request.Verbose = value
			}
		}
		if flags.Lookup("timeout") != nil {
			if value, err := flags.GetDuration("timeout"); err == nil {
				request.Timeout = value
			}
		}
		return hookctx.WithRequest(ctx, request)
	}
	return ctx
}

func commandRequest(cmd *cobra.Command) (hookctx.Context, error) {
	request, ok := hookctx.Request(cmd.Context())
	if !ok {
		return hookctx.Context{}, fmt.Errorf("hook request is missing")
	}
	return request, nil
}

func commandConfigPath(cmd *cobra.Command) string {
	flags := cmd.Root().PersistentFlags()
	if flags.Lookup("config") == nil {
		return ""
	}
	path, err := flags.GetString("config")
	if err != nil {
		panic(err)
	}
	return path
}

// errLostCall — строка статуса не рисуется: связь прокси с демоном оборвалась
// после 'C' (hookproxy.Lost), строка о том уже в stderr
var errLostCall = errors.New("hook call lost after commit")

// executeHook runs one hook request without relying on the executor's argv.
func executeHook(ctx context.Context, request hookctx.Context, hookType string) int {
	if err := validateHookRequest(ctx, request); err != nil {
		fmt.Fprintf(request.Stderr, "chooks: %v\n", err)
		return exitError
	}
	ctx = hookctx.WithRequest(ctx, request)
	code := exitAllowed
	var err error
	if hookType == hookStatusLine {
		err = runStatusLine(ctx, request)
	} else {
		code, err = runHook(ctx, hookType)
	}
	if errors.Is(err, errLostCall) {
		return exitError
	}
	if err != nil {
		fmt.Fprintf(request.Stderr, "chooks: %v\n", err)
		if code == exitAllowed {
			code = exitError
		}
	}
	return code
}

func validateHookRequest(ctx context.Context, request hookctx.Context) error {
	// The CLI can still handle absolute inputs when its cwd was deleted.
	// An in-process request must have an absolute cwd to avoid using the
	// executor's working directory for relative paths.
	_, cli := ctx.Value(cliTerminationKey{}).(func(int))
	if !cli && !filepath.IsAbs(request.Cwd) {
		return fmt.Errorf("hook request cwd must be absolute")
	}
	return nil
}

// closeRequestLogger закрывает файл лога запроса. Ошибку закрытия не
// показывает: строки уже записаны без буфера (O_APPEND), а у CLI база файл не
// закрывала вовсе, и лишней строки в stderr у неё не было
func closeRequestLogger(logger core.FileLogger) {
	if logger == nil {
		return
	}
	_ = logger.Close()
}

func prepareHookRun(ctx context.Context, hookType string, request hookctx.Context) (context.Context, hookctx.Context, *hookTimer, bool, <-chan struct{}, bool) {
	timer := newHookTimerForRequest(hookType, request)
	if request.Time.IsZero() {
		request.Time = time.Now()
	}
	ctx = hookctx.WithRequest(ctx, request)
	exit, cli := ctx.Value(cliTerminationKey{}).(func(int))
	if cli {
		timer.watchTermination(exit)
		if reason, _ := ctx.Value(fallbackReasonKey{}).(string); reason != "" {
			timer.setExecutor(executorCLI, reason)
		}
	}
	var terminated <-chan struct{}
	if call, ok := ctx.Value(daemonCallKey{}).(*daemonCall); ok {
		terminated = call.terminated
		timer.setExecutor(executorDaemon, "")
	}
	lost := false
	if reason, _ := ctx.Value(fallbackReasonKey{}).(string); cli && hookproxy.Lost(reason) {
		lost = true
		timer.setExecutor(executorDaemon, reason)
	}
	return ctx, request, timer, cli, terminated, lost
}

// runHook выполняет основную логику хука
func runHook(ctx context.Context, hookType string) (code int, err error) {
	request, ok := hookctx.Request(ctx)
	if !ok {
		return exitError, fmt.Errorf("hook request is missing")
	}
	if err := validateHookRequest(ctx, request); err != nil {
		return exitError, err
	}
	ctx, request, timer, cli, terminated, lost := prepareHookRun(ctx, hookType, request)
	// Файл лога закрывает владелец запроса. У CLI это процесс: обработчик
	// SIGTERM подписан до выхода и после возврата runHook ещё пишет строку
	// «hook killed/cancelled by SIGTERM», поэтому файл, как в базе, закрывает
	// ОС при выходе. Запрос без своего обработчика (executeHook в демоне)
	// закрывает файл сам — последним, после строк таймера, и на пути ошибки
	var logger core.FileLogger
	defer func() {
		status := "ok"
		switch {
		case lost:
			status = statusLost
		case err != nil || code == exitError:
			status = "error"
		}
		// Прокси сообщил о SIGTERM ('T'), и обработчик вернулся по отмене:
		// исход — как у CLI под SIGTERM (killed или cancelled), а не «error».
		// Пишется здесь, а не сторожем: строка уходит до закрытия лога запроса
		select {
		case <-terminated:
			timer.reportTermination()
		default:
		}
		timer.finish(status)
		if !cli {
			closeRequestLogger(logger)
		}
	}()

	data, err := io.ReadAll(request.Stdin)
	if err != nil {
		return exitError, fmt.Errorf("failed to read input: %w", err)
	}

	session := sessionIDOf(data)
	timer.setSession(session)

	timer.stage("config")
	config, err := core.LoadConfigForRequest(request.ConfigPath, request)
	if err != nil {
		return exitError, fmt.Errorf("failed to load config: %w", err)
	}

	logger, err = core.NewLoggerIn(config.Logger, request.Stderr, request.Location())
	if err != nil {
		return exitError, fmt.Errorf("failed to create logger: %w", err)
	}
	timer.setLogger(logger)
	timer.setDurations(config.Logger)
	noteCLIFallback(ctx, config, request, hookType, logger)
	if lost {
		reason, _ := ctx.Value(fallbackReasonKey{}).(string)
		_, _ = io.WriteString(request.Stderr, hookproxy.LostMessage(reason))
		return exitError, nil
	}

	// Тень: CLI, исполняющий хук сам (не запасной путь прокси), шлёт демону
	// запись для сравнения (этап 2б-3а)
	var shadow *shadowSession
	if reason, _ := ctx.Value(fallbackReasonKey{}).(string); cli && reason == "" && config.Daemon.Shadow {
		if hook, covered := shadowHooks[hookType]; covered {
			ctx, request, shadow = beginShadow(ctx, hook, hookType, data, session, config, request)
		}
	}

	timer.stage("work")
	code, err = dispatchHook(ctx, hookType, data, config, logger, timer, request)
	if shadow != nil {
		timer.stage("shadow")
		shadow.send(code, err)
	}
	return code, err
}

// dispatchHook — работа подкоманды хука после конфига и лога: её же зовёт
// теневое исполнение демона (shadowDispatch) для сравниваемых подкоманд
func dispatchHook(ctx context.Context, hookType string, data []byte, config *core.Config, logger core.Logger, timer *hookTimer, request hookctx.Context) (int, error) {
	switch hookType {
	case hookPreToolUse:
		hookCtx, cancel := context.WithTimeout(ctx, request.Timeout)
		defer cancel()
		return runPreToolUse(hookCtx, data, config, logger)
	case hookUserPromptSubmit:
		// Отправка запроса ничего не блокирует: она отмечает, что работа
		// возобновилась (без неё ответ без единого вызова инструмента остался
		// бы незамеченным), и напоминает о скиллах, изменённых после вызова.
		// Срабатывает и на сообщение другой сессии, и посреди хода
		return runUserPromptSubmit(ctx, hookType, data, config, logger, timer)
	case hookPostToolBatch:
		// Пакет инструментов — место напоминания в длинном ходе и у субагентов,
		// до которых UserPromptSubmit не доходит. Он же снимает «ждёт ответа»:
		// ответ на запрос разрешения или AskUserQuestion не проходит через
		// UserPromptSubmit, а пакет после ответа значит, что работа идёт.
		// Пакет субагента — не ответ: главный ход может стоять на вопросе
		input := parseSessionInputForRequest(logger, hookType, data, &request)
		timer.stage("state")
		if input.AgentID == "" {
			if _, err := core.SwapSessionStateIn(request.RuntimeDir(), input.SessionID, core.StateWaiting, core.StateWorking); err != nil {
				logger.Warn("session state not resumed", "session", input.SessionID, "error", err)
			}
		}
		timer.stage("skills")
		remindStaleSkills(ctx, eventPostToolBatch, input, config, logger, timer.deadline(optionalBudget), timer, request)
		return exitAllowed, nil
	case hookStop, hookStopFailure, hookNotification:
		capture := shadowCaptureFrom(ctx)
		decision := runSessionEventForRequest(sessionEvents[hookType], data, config, logger, &request, capture)
		if capture != nil {
			capture.mu.Lock()
			capture.decision = &decision
			capture.mu.Unlock()
		}
		return exitAllowed, nil

	case hookUnderstandCheck:
		runUnderstandCheck(ctx, data, config, logger, request.Stdout)
		return exitAllowed, nil
	case hookWebFetchFallback:
		runWebFetchFallback(ctx, data, config, logger, request.Stdout)
		return exitAllowed, nil
	case hookUnsentCheck:
		runUnsentCheck(parseSessionInputForRequest(logger, hookType, data, &request), logger, request.Stdout)
		return exitAllowed, nil
	case hookClaimStop:
		runClaimStop(ctx, data, config, logger, request.Stdout)
		return exitAllowed, nil
	case hookNightHint:
		defaultNightHint(ctx, data, config, logger, timer.deadline(optionalBudget), request)
		return exitAllowed, nil
	case hookNoPrompt:
		if err := runNoPrompt(data, logger, request.Stdout); err != nil {
			logger.Error("prompt left to the dialog", "hook", hookNoPrompt, "error", err)
			return exitError, err
		}
		return exitAllowed, nil
	default:
		return exitError, fmt.Errorf("unknown hook type: %s", hookType)
	}
}

func runUserPromptSubmit(ctx context.Context, hookType string, data []byte, config *core.Config, logger core.Logger, timer *hookTimer) (int, error) {
	request, ok := hookctx.Request(ctx)
	if !ok {
		return exitError, fmt.Errorf("hook request is missing")
	}
	input := parseSessionInputForRequest(logger, hookType, data, &request)
	message, err := checkRuntimeLaunchGuard(ctx, request.PID, config.Tools["model_guard"], logger)
	if err != nil {
		return exitError, err
	}
	if message != "" {
		return exitBlocked, fmt.Errorf("%s", message)
	}
	timer.stage("state")
	saveStateIn(logger, request.RuntimeDir(), input.SessionID, core.StateWorking)
	timer.stage("skills")
	remindStaleSkills(ctx, eventUserPromptSubmit, input, config, logger, timer.deadline(optionalBudget), timer, request)
	return exitAllowed, nil
}

// checkRuntimeLaunchGuard — отказ claude, запущенному агентом на модели вне
// списка. Паника в проверке не блокирует реплику: строка Error в лог и пропуск.
// hookPID — процесс хука запроса: от него идёт подъём к claude сессии
func checkRuntimeLaunchGuard(ctx context.Context, hookPID int, config core.ToolConfig, logger core.Logger) (message string, err error) {
	if config.RuntimeLaunchGuard != nil && !*config.RuntimeLaunchGuard {
		return "", nil
	}
	defer func() {
		if r := recover(); r != nil {
			logger.Error("modellaunch: паника в проверке модели claude, проверка пропущена", "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
			message, err = "", nil
		}
	}()
	guard, err := newRuntimeGuard(ctx, config, logger)
	if err != nil {
		return "", err
	}
	return guard.Check(ctx, hookPID)
}

// runtimeGuardKey — ключ ctx запроса с runtimeGuardDeps: тест подставляет
// дерево процессов и судью одному запросу, не трогая параллельные
type runtimeGuardKey struct{}

// runtimeGuardDeps — подмена для проверки модели claude одного запроса; пустое
// поле — настоящее: /proc машины и model_guard
type runtimeGuardDeps struct {
	proc  modellaunch.ProcessReader
	judge modellaunch.Judge
}

// newRuntimeGuard строит проверку модели claude сессии: model_guard судит
// команду из /proc
func newRuntimeGuard(ctx context.Context, config core.ToolConfig, logger core.Logger) (*modellaunch.Guard, error) {
	deps, _ := ctx.Value(runtimeGuardKey{}).(runtimeGuardDeps)
	proc := deps.proc
	if proc == nil {
		proc = modellaunch.ProcReader{}
	}
	judge := deps.judge
	if judge == nil {
		config.Enabled = true
		modelGuard, err := tools.NewModelGuardTool(config, logger)
		if err != nil {
			return nil, err
		}
		judge = modelGuard
	}
	return modellaunch.New(proc, judge, logger), nil
}

// sessionIDOf — сессия события для строк о длительности. Нечитаемый вход
// разбирает и пишет в лог сам обработчик хука; здесь он — пустая сессия
func sessionIDOf(data []byte) string {
	var head struct {
		SessionID string `json:"session_id"`
	}
	//nolint:silent-error-handling // вход целиком разбирает обработчик хука и пишет сбой в лог сам; сессия здесь — только подпись строки длительности
	if err := json.Unmarshal(data, &head); err != nil {
		return ""
	}
	return head.SessionID
}

// runPreToolUse прогоняет вызов инструмента через проверки
func runPreToolUse(ctx context.Context, data []byte, config *core.Config, logger core.Logger) (int, error) {
	request, ok := hookctx.Request(ctx)
	if !ok {
		return exitError, fmt.Errorf("hook request is missing")
	}
	input, err := core.ParseToolInput(data)
	if err != nil {
		return exitError, fmt.Errorf("failed to parse input: %w", err)
	}
	resolveHookInput(input, request)

	engine, err := processor.NewForRequest(config, logger, request)
	if err != nil {
		return exitError, fmt.Errorf("failed to create processor: %w", err)
	}

	response, err := engine.ProcessPreToolUse(ctx, input)
	if err != nil {
		logger.Error("hook processing failed", "hook_type", hookPreToolUse, "error", err)
		return exitError, err
	}
	if capture := shadowCaptureFrom(ctx); capture != nil {
		capture.action, capture.message = string(response.Action), response.Message
	}

	printResponse(logger, input, response, request.Stderr, request.Verbose)

	if response.Action == core.HookActionAllow {
		writeAllowedOutputContext(ctx, input, config, logger, request.Stdout)
		return exitAllowed, nil
	}
	// Warn и Block одинаково возвращают 2: только этот код доносит сообщение до модели
	return exitBlocked, nil
}

// runSessionEventForRequest обрабатывает остановку и уведомление Claude Code. Всё
// нужное приходит в stdin: транскрипт не читается, звук и уведомление уходят
// в отдельный процесс, поэтому хук укладывается в миллисекунды. Сбои здесь
// только логируются: событие сессии нечего блокировать, а ошибка хука
// показалась бы человеку в интерфейсе на каждой остановке
// capture — тень (этап 2б-3а), nil — без тени
func runSessionEventForRequest(eventName string, data []byte, config *core.Config, logger core.Logger, request *hookctx.Context, capture *shadowCapture) notifier.Decision {
	return announceSessionEventWith(eventName, parseSessionInputForRequest(logger, eventName, data, request), config, logger, request, capture)
}

// sessionEvents — событие сессии подкоманды
var sessionEvents = map[string]string{
	hookStop:         core.EventStop,
	hookStopFailure:  core.EventStopFailure,
	hookNotification: core.EventNotification,
}

// announceSessionEvent решает по событию сессии, запоминает состояние и зовёт
// человека
func announceSessionEvent(eventName string, input *core.ToolInput, config *core.Config, logger core.Logger, request *hookctx.Context) notifier.Decision {
	return announceSessionEventWith(eventName, input, config, logger, request, nil)
}

// announceSessionEventWith — announceSessionEvent под тенью: эффекты
// уведомления записываются в capture, у демона — без исполнения
func announceSessionEventWith(eventName string, input *core.ToolInput, config *core.Config, logger core.Logger, request *hookctx.Context, capture *shadowCapture) notifier.Decision {
	// Неинтерактивную сессию человек не ждёт вовсе — ни её остановку, ни её
	// вопросы: ответить в неё всё равно некому
	if request == nil {
		logger.Error("hook request is missing")
		return notifier.Decision{Reason: "hook request is missing"}
	}
	printMode, err := core.PrintModeSessionIn(request.ProcFS(), request.Getenv("CLAUDE_PID"))
	if err != nil {
		logger.Warn("session mode unknown, assuming interactive", "error", err)
	}
	if printMode {
		logger.Info("alert muted", "hook", eventName, "reason", "неинтерактивная сессия claude -p")
		return notifier.Decision{Reason: "неинтерактивная сессия claude -p"}
	}

	// Строка статуса рисуется отдельным процессом и о ходе сессии не знает —
	// состояние для неё оставляют хуки. Предыдущее состояние нужно решению:
	// по нему виден переход, а не только новое состояние
	previous, recorded, err := core.LookupSessionStateIn(request.RuntimeDir(), input.SessionID)
	if err != nil {
		// Явный failover: без записи переход не виден, и решение принимается
		// по StateUnknown — оно зовёт человека, лишний звонок лучше пропущенного
		logger.Warn("session state unavailable", "session", input.SessionID, "error", err)
		previous = core.StateUnknown
	} else if !recorded {
		// Хук ещё не отработал: для решения это работа
		previous = core.StateWorking
	}

	event := notifier.Event{Name: eventName, Input: input, Previous: previous, Fresh: err == nil && !recorded && input.SessionID != ""}
	if eventName == core.EventStop && input.BackgroundTasks != nil {
		var crons []core.SessionCron
		if input.SessionCrons != nil {
			crons = *input.SessionCrons
		}
		var active []core.BackgroundTask
		// Время задач пишется со смещением пояса запроса, как у CLI
		active, err = core.RecordBackgroundIn(request.RuntimeDir(), input.SessionID, *input.BackgroundTasks, crons, request.Now().In(request.Location()))
		if err != nil {
			// Failover на список Claude Code: брошенные задачи не отличить от
			// живых, а про все задачи списка Claude Code сам сказал, что они живы
			logger.Warn("background task bookkeeping failed, every listed task counts as active", "session", input.SessionID, "error", err)
			active = *input.BackgroundTasks
		}
		event.ActiveTasks = len(active)
	}

	decision := notifier.Decide(event)
	logDecision(logger, event, decision)

	if decision.State != "" {
		saveStateIn(logger, request.RuntimeDir(), input.SessionID, decision.State)
	}
	if capture != nil {
		capture.snapshotAfter(*request)
	}

	toolConfig, exists := config.Tools["notifier"]
	if !exists || !toolConfig.Enabled {
		return decision
	}
	tool := notifier.NewForRequest(toolConfig, logger, *request)
	if capture != nil {
		tool = tool.WithEffects(capture.notifierEffects(realNotifierEffects()))
	}
	if err := tool.Announce(eventName, input, decision); err != nil {
		logger.Warn("failed to deliver alert", "hook", eventName, "error", err)
	}
	return decision
}

// parseSessionInput разбирает вход события сессии. Нечитаемый вход — ошибка
// в логе, но не повод молчать: уведомить можно и без деталей события
func parseSessionInput(logger core.Logger, hookType string, data []byte) *core.ToolInput {
	input, err := core.ParseToolInput(data)
	if err != nil {
		logger.Error("session event input unreadable, handling it without details", "hook", hookType, "error", err)
		return &core.ToolInput{}
	}
	return input
}

func parseSessionInputForRequest(logger core.Logger, hookType string, data []byte, request *hookctx.Context) *core.ToolInput {
	input := parseSessionInput(logger, hookType, data)
	if request != nil {
		resolveHookInput(input, *request)
	}
	return input
}

func resolveHookInput(input *core.ToolInput, request hookctx.Context) {
	if input.CWD != "" && !filepath.IsAbs(input.CWD) {
		input.CWD = request.Abs(input.CWD)
	}
	if input.TranscriptPath != "" && !filepath.IsAbs(input.TranscriptPath) {
		input.TranscriptPath = request.Abs(input.TranscriptPath)
	}
}

// saveStateIn запоминает состояние сессии для строки статуса и следующего
// события. Сбой хранилища хук не срывает: код ошибки Claude Code показал бы
// человеку на каждом событии, а уведомление важнее записи
func saveStateIn(logger core.Logger, runtimeDir, sessionID string, state core.SessionState) {
	//nolint:log-and-return-zero // событие сессии не блокируется ошибкой записи состояния
	if err := core.SaveSessionStateIn(runtimeDir, sessionID, state); err != nil {
		logger.Warn("session state not saved", "session", sessionID, "error", err)
	}
}

// logDecision записывает решение по событию сессии вместе с тем, из чего оно
// сделано: событие приходит и уходит бесследно, а разбирают его постфактум
// («почему позвонило», «почему не позвонило»)
func logDecision(logger core.Logger, event notifier.Event, decision notifier.Decision) {
	input := event.Input
	fields := []any{
		"hook", event.Name,
		"session", input.SessionID,
		"previous_state", string(event.Previous),
		"state", string(decision.State),
	}

	switch event.Name {
	case core.EventStop:
		fields = append(fields, "active_tasks", event.ActiveTasks)
		if input.BackgroundTasks != nil {
			fields = append(fields, "background_tasks", taskTypes(*input.BackgroundTasks))
		}
		if input.SessionCrons != nil {
			fields = append(fields, "session_crons", len(*input.SessionCrons))
		}
	case core.EventNotification:
		fields = append(fields, "notification_type", input.NotificationType)
	}

	if decision.Reason != "" {
		fields = append(fields, "reason", decision.Reason)
	}

	if decision.Alert {
		logger.Info("alert allowed", fields...)
		return
	}
	logger.Info("alert muted", fields...)
}

// taskTypes перечисляет типы фоновых задач: shell, subagent, monitor…
// Описания и команды в лог не идут
func taskTypes(tasks []core.BackgroundTask) string {
	types := make([]string, 0, len(tasks))
	for _, task := range tasks {
		types = append(types, task.Type)
	}
	return strings.Join(types, ",")
}

// printResponse выводит результат: stderr читает Claude Code при exit-коде 2
func printResponse(logger core.Logger, input *core.ToolInput, response *core.HookResponse, stderr io.Writer, verbose bool) {
	switch response.Action {
	case core.HookActionBlock, core.HookActionWarn:
		logger.Warn("hook blocked operation", append(deniedCallFields(input),
			"action", string(response.Action),
			"message", response.Message,
			"violations", len(response.Violations),
		)...)

		fmt.Fprintln(stderr, response.Message)
		for _, suggestion := range response.Suggestions {
			fmt.Fprintf(stderr, "  • %s\n", suggestion)
		}
	case core.HookActionAllow:
		if verbose {
			fmt.Fprintln(stderr, "✅ проверки пройдены")
		}
	}

	if !verbose {
		return
	}

	for _, violation := range response.Violations {
		fmt.Fprintf(stderr, "  [%s] %s:%d %s\n",
			violation.Severity, violation.Type, violation.Line, violation.Message)
	}
	fmt.Fprintf(stderr, "⏱  %.1f ms\n", response.ProcessTime)
}

// newConfigCmd создает команду управления конфигурацией
func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Управление конфигурацией",
	}

	cmd.AddCommand(
		&cobra.Command{
			Use:   "show",
			Short: "Показать текущую конфигурацию",
			RunE: func(cmd *cobra.Command, args []string) error {
				return showConfig(commandConfigPath(cmd))
			},
		},
		&cobra.Command{
			Use:   "validate",
			Short: "Проверить файл конфигурации",
			RunE: func(cmd *cobra.Command, args []string) error {
				path, err := configPathOrDefault(commandConfigPath(cmd))
				if err != nil {
					return err
				}
				if _, err := core.LoadConfig(path); err != nil {
					return err
				}
				fmt.Printf("конфигурация корректна: %s\n", path)
				return nil
			},
		},
		&cobra.Command{
			Use:   "init",
			Short: "Создать конфигурацию по умолчанию",
			RunE: func(cmd *cobra.Command, args []string) error {
				path, err := configPathOrDefault(commandConfigPath(cmd))
				if err != nil {
					return err
				}
				if _, err := os.Stat(path); err == nil {
					return fmt.Errorf("файл уже существует: %s", path)
				}
				if err := core.SaveConfig(core.DefaultConfig(), path); err != nil {
					return err
				}
				fmt.Printf("конфигурация создана: %s\n", path)
				return nil
			},
		},
	)

	return cmd
}

// showConfig печатает состояние валидаторов и инструментов
func showConfig(configPath string) error {
	path, err := configPathOrDefault(configPath)
	if err != nil {
		return err
	}
	config, err := core.LoadConfig(path)
	if err != nil {
		return err
	}

	fmt.Printf("Конфигурация: %s\n", path)
	if config.Logger.Output == "file" {
		fmt.Printf("Логи: %s (уровень %s, ротация %d МБ)\n\n",
			config.Logger.LogFile, config.Logger.Level, config.Logger.MaxSizeMB)
	} else {
		fmt.Printf("Логи: %s (уровень %s)\n\n", config.Logger.Output, config.Logger.Level)
	}

	fmt.Println("Инструменты:")
	for _, name := range slices.Sorted(maps.Keys(config.Tools)) {
		fmt.Printf("  %-20s %s\n", name, enabledLabel(config.Tools[name].Enabled))
	}

	return nil
}

// configPathOrDefault возвращает заданный путь конфигурации либо путь по умолчанию
func configPathOrDefault(configPath string) (string, error) {
	if configPath != "" {
		return configPath, nil
	}
	return core.DefaultConfigPath()
}

// enabledLabel возвращает текстовое состояние компонента
func enabledLabel(enabled bool) string {
	if enabled {
		return "включён"
	}
	return "выключен"
}

// newNotifyCmd создает команду отправки уведомления из сторонних инструментов.
// Claude Code присылает события сам, а другим агентам (например opencode)
// нужен способ показать такое же уведомление с переходом к окну по клику
func newNotifyCmd() *cobra.Command {
	var (
		alert     desktop.Alert
		windowPID int
		activate  bool
		urgency   string
	)

	cmd := &cobra.Command{
		Use:   "notify",
		Short: "Показать уведомление с переходом к окну по клику",
		Long: `Показывает уведомление и по клику переводит фокус на окно терминала.

Окно определяется по процессу: указанному через --window-pid либо самому вызывающему.
Команда возвращает управление сразу, не дожидаясь ни звука, ни клика.

Срочность --urgency critical — уведомление, которое не закрывается само
(в KDE Plasma — с оранжевой полосой): висит, пока его не закроют; --timeout
с ним не задаётся.

Пример вызова из плагина стороннего агента:
  chooks notify --title "opencode ждёт ответа" --message "Проект: projecta" --window-pid $$`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if alert.Title == "" {
				return fmt.Errorf("не задан --title")
			}
			if err := applyNotifyUrgency(&alert, urgency, cmd.Flags().Changed("timeout")); err != nil {
				return err
			}

			if activate {
				if windowPID == 0 {
					windowPID = os.Getpid()
				}
				// Окно принадлежит одному из процессов в цепочке до эмулятора
				// терминала
				ancestors, err := desktop.ProcessAncestors(windowPID)
				if err != nil {
					// Best effort: собранная часть цепочки — настоящие PID, окно
					// может быть у них; без окна клик просто ничего не активирует
					fmt.Fprintf(os.Stderr, "chooks: process ancestry incomplete: %v\n", err)
				}
				alert.ActivatePIDs = ancestors
			}

			executable, err := os.Executable()
			if err != nil {
				return fmt.Errorf("failed to locate own executable: %w", err)
			}

			return desktop.DeliverInBackground(executable, notifier.WatchCommand, alert)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&alert.Title, "title", "", "заголовок уведомления")
	flags.StringVar(&alert.Message, "message", "", "текст уведомления")
	flags.StringVar(&alert.AppName, "app-name", "chooks", "имя приложения в уведомлении")
	flags.StringVar(&alert.Icon, "icon", "utilities-terminal", "значок уведомления")
	flags.DurationVar(&alert.Timeout, "timeout", 30*time.Second, "время показа уведомления (не для critical)")
	flags.StringVar(&urgency, "urgency", desktop.UrgencyNormal.String(), "срочность: low, normal или critical (не закрывается само)")
	flags.BoolVar(&alert.Sound, "sound", true, "проиграть звук")
	flags.BoolVar(&alert.Desktop, "desktop", true, "показать уведомление")
	flags.IntVar(&windowPID, "window-pid", 0, "процесс, чьё окно активируется по клику (по умолчанию — вызывающий)")
	flags.BoolVar(&activate, "activate-window", true, "переводить фокус на окно по клику")
	flags.StringVar(&alert.ActionLabel, "action-label", "Перейти к окну", "подпись действия уведомления")

	return cmd
}

// newDeliverAlertCmd создает команду фоновой доставки оповещения.
// Хук запускает её отдельным процессом и сразу завершается: ожидание клика
// по уведомлению длится дольше, чем Claude Code готов ждать хук
func newDeliverAlertCmd() *cobra.Command {
	var (
		alert       desktop.Alert
		pids        string
		actionLabel string
		urgency     string
	)

	cmd := &cobra.Command{
		Use:    notifier.WatchCommand,
		Short:  "Показать уведомление и обработать клик по нему",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			activatePIDs, err := desktop.ParseInts(pids)
			if err != nil {
				return err
			}
			alert.ActivatePIDs = activatePIDs
			alert.ActionLabel = actionLabel
			alert.Urgency, err = desktop.ParseUrgency(urgency)
			if err != nil {
				return err
			}
			return desktop.Deliver(cmd.Context(), alert)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&alert.Title, "title", "", "заголовок уведомления")
	flags.StringVar(&alert.Message, "message", "", "текст уведомления")
	flags.StringVar(&alert.AppName, "app-name", "Claude Code", "имя приложения")
	flags.StringVar(&alert.Icon, "icon", "utilities-terminal", "значок уведомления")
	flags.DurationVar(&alert.Timeout, "timeout", 10*time.Second, "время показа уведомления")
	flags.BoolVar(&alert.Sound, "sound", true, "проиграть звук")
	flags.BoolVar(&alert.Desktop, "desktop", true, "показать уведомление")
	flags.StringVar(&pids, "pids", "", "процессы, чьё окно активируется по клику")
	flags.StringVar(&actionLabel, "action-label", "", "подпись действия уведомления")
	flags.StringVar(&urgency, "urgency", "", "срочность: low, normal или critical")

	return cmd
}

// applyNotifyUrgency выставляет срочность уведомления команды notify.
// Критичное не истекает само (таймаут 0), поэтому явный --timeout с ним —
// противоречие, а не повод молча выбрать одно из двух
func applyNotifyUrgency(alert *desktop.Alert, name string, timeoutSet bool) error {
	urgency, err := desktop.ParseUrgency(name)
	if err != nil {
		return fmt.Errorf("--urgency: %w", err)
	}
	alert.Urgency = urgency
	if urgency != desktop.UrgencyCritical {
		return nil
	}
	if timeoutSet {
		return fmt.Errorf("--timeout не задаётся с --urgency critical: такое уведомление висит, пока его не закроют")
	}
	alert.Timeout = 0
	return nil
}

// newStatusLineCmd создает команду отрисовки строки статуса Claude Code
func newStatusLineCmd(exitCode *int) *cobra.Command {
	return &cobra.Command{
		Use:   "statusline",
		Short: "Отрисовать строку статуса Claude Code",
		Long: `Читает JSON сессии со stdin и печатает две строки статуса:
плашку с именем проекта и подробности о ветке, модели, контексте
и недельном лимите подписки.

Подключается в ~/.claude/settings.json:
  "statusLine": {"type": "command", "command": "~/bin/chooks statusline"}`,
		RunE: func(cmd *cobra.Command, args []string) error {
			request, ok := hookctx.Request(hookCommandContext(cmd))
			if !ok {
				return fmt.Errorf("missing hook request context")
			}
			*exitCode = executeHook(cmd.Context(), request, "statusline")
			return nil
		},
	}
}

func runStatusLine(ctx context.Context, request hookctx.Context) error {
	config, err := core.LoadConfigForRequest(request.ConfigPath, request)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	logger, err := core.NewLoggerIn(config.Logger, request.Stderr, request.Location())
	if err != nil {
		return fmt.Errorf("failed to create logger: %w", err)
	}
	defer closeRequestLogger(logger)
	noteCLIFallback(ctx, config, request, hookStatusLine, logger)
	// Обрыв после 'C': строку уже рисовал демон, второй раз её не рисуют
	// (ротация учёток из строки статуса не запускается дважды)
	if reason, _ := ctx.Value(fallbackReasonKey{}).(string); hookproxy.Lost(reason) {
		_, _ = io.WriteString(request.Stderr, hookproxy.LostMessage(reason))
		return errLostCall
	}
	line, err := statusline.Render(ctx, request.Stdin, logger)
	if err != nil {
		return err
	}
	fmt.Fprintln(request.Stdout, line)
	return nil
}

// newLimitsCmd создает команду сводки недельного лимита по учёткам
func newLimitsCmd() *cobra.Command {
	var steps, pool bool
	cmd := &cobra.Command{
		Use:   "limits",
		Short: "Показать неделю Claude по каждой учётке",
		Long: `Печатает лимиты из снимка строки статуса
(~/.local/state/chooks/limits.json) по каждой учётке хранилища
(chooks account): почта, неделя - доля и сброс, срок входа.
Неделя приписывается учётке, активной в момент замера; недели без учётки
(до хранилища) - строками без почты, по сбросу недели.

С --steps печатает журнал шагов (limits-history.jsonl) таблицей с табуляцией:
момент, когда доля впервые дошла до значения, - по нему считается цена процента.

С --pool-json печатает прогноз пула учёток JSON: темп, когда все учётки
опустеют (dryAt), чей сброс вернёт расход (refillAt), план ротации. Вход
учёток прогноз не учитывает: его продлевает пользователь. Его читает
страница Сна.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			request, err := commandRequest(cmd)
			if err != nil {
				return err
			}
			if pool {
				return statusline.WritePoolJSON(cmd.OutOrStdout(), request)
			}
			return statusline.WriteLimits(cmd.OutOrStdout(), steps, request)
		},
	}
	cmd.Flags().BoolVar(&steps, "steps", false, "журнал шагов вместо сводки")
	cmd.Flags().BoolVar(&pool, "pool-json", false, "прогноз пула учёток JSON вместо сводки")
	cmd.MarkFlagsMutuallyExclusive("steps", "pool-json")
	return cmd
}

// newVersionCmd создает команду вывода версии
func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Показать версию",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("chooks %s\n", Version)
		},
	}
}

// Exit-коды команды style: 1 значит, что в черновике есть нарушения, 2 -
// черновик не прочитан. Код ошибки отличается, чтобы непрочитанный файл не
// выглядел замечаниями к тексту
const (
	exitStyleViolations = 1
	exitStyleError      = 2
)

// checkDraft проверка черновика команды style; тест подменяет её, чтобы
// увидеть, что документ (--doc) черновиком не проверяется
var checkDraft = textstyle.CheckDraftKeys

// newStyleCmd создает команду проверки черновика текста от имени пользователя
func newStyleCmd(exitCode *int) *cobra.Command {
	var doc bool
	var keys []string
	cmd := &cobra.Command{
		Use:   "style [--doc] [--keys PROJ,...] <файл|->",
		Short: "Проверить черновик текста по форме write-as-user",
		Long: `Ищет в черновике (файл или stdin при "-") маркеры, которые запрещает скилл
write-as-user: длинное и короткое тире, markdown, типографские кавычки, оборот
"X, а не Y", число прописью, десятичную запятую, эмодзи, предисловия.

С --doc проверяет документ markdown (сводку встречи перед импортом в Google
Doc): заголовки, списки, ссылки, таблицы и выделение разрешены; ищет тире,
типографские кавычки, обороты "X, а не Y" и "не X, а Y", десятичную запятую,
ключ задачи вне ссылки markdown и URL, относительную дату (сегодня, завтра,
вчера, на этой неделе, через неделю, день недели без числа после него).

Ключ задачи: префиксы проектов - --keys PROJ,OPS, без него - style.key_prefixes
конфига chooks. В черновике каждый ключ вне URL - находка с подсказкой голого
URL с ключом: style.issue_url конфига для префиксов style.key_prefixes, для
остальных шаблон адреса; без префиксов ключ в черновике не ищется.
В документе без префиксов находка - любой ключ вида ABC-12 (и UTF-8,
SHA-256 тоже).

Печатает найденное с номерами строк. Код выхода 0 - нарушений нет,
1 - есть нарушения, 2 - черновик не прочитан.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			config, err := core.LoadConfig(commandConfigPath(cmd))
			if err != nil {
				*exitCode = exitStyleError
				return fmt.Errorf("failed to load config: %w", err)
			}
			prefixes := keyPrefixes(keys)
			if len(keys) == 0 {
				prefixes = keyPrefixes(config.Style.KeyPrefixes)
			}
			text, err := readDraft(cmd.InOrStdin(), args[0])
			if err != nil {
				*exitCode = exitStyleError
				return err
			}
			var findings []textstyle.Finding
			if doc {
				findings = textstyle.CheckDoc(text, prefixes)
			} else {
				findings = checkDraft(text, prefixes, issueURLs(config.Style))
			}
			out := cmd.OutOrStdout()
			for _, f := range findings {
				fmt.Fprintf(out, "%d: %s: %q, нужно: %s\n", f.Line, f.Marker.Description, f.Fragment, f.Marker.Hint(f.Fragment))
			}
			if len(findings) > 0 {
				*exitCode = exitStyleViolations
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&doc, "doc", false, "документ markdown: разметка разрешена, ищет ключ без ссылки и относительную дату")
	cmd.Flags().StringSliceVar(&keys, "keys", nil, "префиксы ключей проектов через запятую (заменяют style.key_prefixes конфига)")
	return cmd
}

// keyPrefixes — префиксы ключей из --keys: без пробелов, заглавными
// («si, ops» — SI и OPS), пустые отброшены
func keyPrefixes(keys []string) []string {
	var out []string
	for _, k := range keys {
		if k = strings.ToUpper(strings.TrimSpace(k)); k != "" {
			out = append(out, k)
		}
	}
	return out
}

// issueURLs — адрес задачи по префиксу: issue_url конфига ведёт только на
// задачи префиксов конфига, префикс из --keys вне конфига адреса не
// получает
func issueURLs(style core.StyleConfig) map[string]string {
	urls := map[string]string{}
	for _, prefix := range keyPrefixes(style.KeyPrefixes) {
		urls[prefix] = style.IssueURL
	}
	return urls
}

// readDraft читает черновик из файла или stdin ("-")
func readDraft(stdin io.Reader, path string) (string, error) {
	if path == "-" {
		data, err := io.ReadAll(stdin)
		if err != nil {
			return "", fmt.Errorf("read draft from stdin: %w", err)
		}
		return string(data), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read draft: %w", err)
	}
	return string(data), nil
}
