package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aiseeq/chooks/internal/core"
	"github.com/aiseeq/chooks/internal/desktop"
	"github.com/aiseeq/chooks/internal/hookctx"
	"github.com/aiseeq/chooks/internal/textstyle"
	"github.com/aiseeq/chooks/internal/tools/notifier"
)

// stopInput собирает вход Stop по схеме Claude Code 2.1.284
func stopInput(t *testing.T, sessionID, transcript string, tasks []map[string]any, crons []map[string]any) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"session_id":             sessionID,
		"transcript_path":        transcript,
		"cwd":                    "/tmp/project",
		"hook_event_name":        "Stop",
		"stop_hook_active":       false,
		"last_assistant_message": "готово",
		"background_tasks":       tasks,
		"session_crons":          crons,
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func silentConfig(t *testing.T) (*core.Config, core.Logger) {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("CLAUDE_PID", "")

	config := &core.Config{Tools: map[string]core.ToolConfig{"notifier": {Enabled: false}}}
	logger, err := core.NewLogger(core.LoggerConfig{Level: "debug"})
	if err != nil {
		t.Fatal(err)
	}
	return config, logger
}

// Stop не открывает транскрипт: путь указывает на FIFO, и любая попытка его
// прочитать повесила бы хук до таймаута теста
func TestRunSessionEvent_StopDoesNotReadTranscript(t *testing.T) {
	config, logger := silentConfig(t)

	fifo := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}

	done := make(chan struct{})
	go func() {
		runSessionEvent(core.EventStop, stopInput(t, "s1", fifo, []map[string]any{}, []map[string]any{}), config, logger)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop-хук ждёт транскрипт")
	}

	state, err := core.LoadSessionStateIn(core.RuntimeDir(), "s1")
	if err != nil || state != core.StateDone {
		t.Errorf("состояние после остановки: %q, %v", state, err)
	}
}

// Остановка при живой фоновой задаче — пауза; следующая остановка без задач —
// конец работы
func TestRunSessionEvent_PauseThenDone(t *testing.T) {
	config, logger := silentConfig(t)
	shell := []map[string]any{{"id": "b1", "type": "shell", "status": "running", "description": "sleep 40", "command": "sleep 40"}}

	runSessionEvent(core.EventStop, stopInput(t, "s2", "/nonexistent.jsonl", shell, []map[string]any{}), config, logger)
	if state, err := core.LoadSessionStateIn(core.RuntimeDir(), "s2"); err != nil || state != core.StatePaused {
		t.Fatalf("при живой задаче ожидалась пауза: %q, %v", state, err)
	}
	if background, err := core.LoadBackgroundIn(core.RuntimeDir(), "s2"); err != nil || len(background.Tasks) != 1 || background.Tasks[0].Description != "sleep 40" {
		t.Fatalf("строке статуса нужно описание задачи: %+v, %v", background, err)
	}

	idle, err := json.Marshal(map[string]any{"session_id": "s2", "notification_type": "idle_prompt", "message": "Claude is waiting for your input"})
	if err != nil {
		t.Fatal(err)
	}
	runSessionEvent(core.EventNotification, idle, config, logger)
	if state, err := core.LoadSessionStateIn(core.RuntimeDir(), "s2"); err != nil || state != core.StatePaused {
		t.Fatalf("напоминание при паузе не меняет состояние: %q, %v", state, err)
	}

	runSessionEvent(core.EventStop, stopInput(t, "s2", "/nonexistent.jsonl", []map[string]any{}, []map[string]any{}), config, logger)
	if state, err := core.LoadSessionStateIn(core.RuntimeDir(), "s2"); err != nil || state != core.StateDone {
		t.Errorf("после последней задачи ожидалось «готово»: %q, %v", state, err)
	}
}

// Сбой учёта фоновых задач не превращает остановку в «готово»: Claude Code
// сам сказал, что задача жива, и сессия остаётся на паузе
func TestRunSessionEvent_BrokenTaskBookkeepingKeepsPause(t *testing.T) {
	config, logger := silentConfig(t)

	dir := filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "chooks", "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "s3.tasks"), []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}

	shell := []map[string]any{{"id": "b1", "type": "shell", "status": "running"}}
	runSessionEvent(core.EventStop, stopInput(t, "s3", "/nonexistent.jsonl", shell, []map[string]any{}), config, logger)
	if state, err := core.LoadSessionStateIn(core.RuntimeDir(), "s3"); err != nil || state != core.StatePaused {
		t.Errorf("при сбое учёта ожидалась пауза: %q, %v", state, err)
	}
}

// runStyle запускает команду style и возвращает её вывод и exit-код
func runStyle(t *testing.T, stdin string, args ...string) (string, int) {
	t.Helper()
	configPath := os.Getenv("CHOOKS_TEST_CONFIG")
	if configPath == "" {
		// Конфиг пользователя (его префиксы ключей) не влияет на тесты:
		// файла нет — значения по умолчанию
		configPath = filepath.Join(t.TempDir(), "none.yaml")
	}
	exitCode := exitAllowed
	cmd := newStyleCmd(&exitCode)
	cmd.PersistentFlags().String("config", configPath, "test config")
	var out strings.Builder
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil && exitCode == exitAllowed {
		t.Fatalf("ошибка без exit-кода: %v", err)
	}
	return out.String(), exitCode
}

func TestStyleCmdExitCodes(t *testing.T) {
	out, code := runStyle(t, "Ок\nГотово — выкачено\n", "-")
	if code != exitStyleViolations || !strings.HasPrefix(out, "2: длинное тире") {
		t.Fatalf("нарушение: код %d, вывод %q", code, out)
	}

	draft := filepath.Join(t.TempDir(), "draft.txt")
	if err := os.WriteFile(draft, []byte("Выкатил в тест - глянь =)"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, code := runStyle(t, "", draft); code != exitAllowed || out != "" {
		t.Fatalf("чистый черновик: код %d, вывод %q", code, out)
	}

	if _, code := runStyle(t, "", filepath.Join(t.TempDir(), "missing.txt")); code != exitStyleError {
		t.Fatalf("непрочитанный файл: код %d, нужно %d", code, exitStyleError)
	}
}

func TestApplyNotifyUrgency(t *testing.T) {
	tests := []struct {
		name        string
		urgency     string
		timeoutSet  bool
		want        desktop.Urgency
		wantTimeout time.Duration
		wantErr     bool
	}{
		{name: "normal keeps timeout", urgency: "normal", want: desktop.UrgencyNormal, wantTimeout: 30 * time.Second},
		{name: "low with timeout", urgency: "low", timeoutSet: true, want: desktop.UrgencyLow, wantTimeout: 30 * time.Second},
		{name: "critical never expires", urgency: "critical", want: desktop.UrgencyCritical, wantTimeout: 0},
		{name: "critical with explicit timeout", urgency: "critical", timeoutSet: true, wantErr: true},
		{name: "unknown urgency", urgency: "orange", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			alert := desktop.Alert{Timeout: 30 * time.Second}
			err := applyNotifyUrgency(&alert, tt.urgency, tt.timeoutSet)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ожидалась ошибка, получено %+v", alert)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if alert.Urgency != tt.want || alert.Timeout != tt.wantTimeout {
				t.Errorf("получено %s/%s, ожидалось %s/%s", alert.Urgency, alert.Timeout, tt.want, tt.wantTimeout)
			}
		})
	}
}

// Срочность доходит до фоновой подкоманды: её флаги принимают то,
// что собирает WatchArgs
func TestWatchArgsCarryUrgency(t *testing.T) {
	alert := desktop.Alert{Title: "t", Urgency: desktop.UrgencyCritical, ActivatePIDs: []int{1, 2}, ActionLabel: "go"}
	args := desktop.WatchArgs(notifier.WatchCommand, alert)
	if args[0] != notifier.WatchCommand {
		t.Fatalf("первым аргументом должна быть подкоманда, получено %q", args[0])
	}

	cmd := newDeliverAlertCmd()
	if err := cmd.ParseFlags(args[1:]); err != nil {
		t.Fatalf("подкоманда не принимает свои аргументы: %v", err)
	}
	urgency, err := cmd.Flags().GetString("urgency")
	if err != nil {
		t.Fatal(err)
	}
	if urgency != "critical" {
		t.Errorf("--urgency: получено %q", urgency)
	}
	timeout, err := cmd.Flags().GetDuration("timeout")
	if err != nil {
		t.Fatal(err)
	}
	if timeout != 0 {
		t.Errorf("--timeout: получено %s, ожидался 0", timeout)
	}
}

// style --doc: разметка сводки не нарушение, ключ без ссылки и относительная
// дата — находки; без --doc вывод прежний (снят бинарём до правки 15.11)
func TestStyleCmdDoc(t *testing.T) {
	clean := "## Итоги встречи 14.11\n\n| Задача | Кто |\n|---|---|\n" +
		"| [PROJ-12](https://jira.example/browse/PROJ-12) | Егор |\n\n" +
		"- **Решение:** выкатка в пятницу 21.11\n- проверка во вторник, 18.11\n"
	if out, code := runStyle(t, clean, "--doc", "--keys", "PROJ", "-"); code != exitAllowed || out != "" {
		t.Fatalf("чистая сводка: код %d, вывод %q", code, out)
	}
	dirty := "Срок по PROJ-34 сдвинут\nСозвон в пятницу\nЗавтра выкатка\nБыстрее в 3,5 раза\nГотово — выкачено\nСозвон в пятницу 14.11\n"
	out, code := runStyle(t, dirty, "--doc", "--keys", "PROJ", "-")
	want := "1: ключ задачи без ссылки: \"PROJ-34\", нужно: ссылка markdown: '[KEY-12](https://…/browse/KEY-12)'\n" +
		"2: относительная дата: \"в пятницу\", нужно: число: '14.11', 'пт 14.11'\n" +
		"3: относительная дата: \"Завтра\", нужно: число: '14.11', 'пт 14.11'\n" +
		"4: десятичная запятая: \"3,5\", нужно: десятичная точка: '2.5'\n" +
		"5: длинное тире: \"Готово — выкачено\", нужно: обычный дефис с пробелами: ' - '\n"
	if code != exitStyleViolations || out != want {
		t.Fatalf("сводка с находками: код %d, вывод\n%s\nнужно\n%s", code, out, want)
	}

	oldClean := "1: markdown-заголовок: \"## Итоги встречи 14.11\", нужно: обычный текст без решёток\n" +
		"3: markdown-таблица: \"| Задача | Кто |\", нужно: строки '- ' или текст без таблицы\n" +
		"4: markdown-таблица: \"|---|---|\", нужно: строки '- ' или текст без таблицы\n" +
		"5: markdown-таблица: \"| [PROJ-12](https://jira.example/browse/PROJ-12) | Егор |\", нужно: строки '- ' или текст без таблицы\n" +
		"5: markdown-ссылка: \"[PROJ-12](https://jira.example/browse/PROJ-12)\", нужно: голая ссылка или ключ тикета\n" +
		"7: markdown-жирный: \"**Решение:**\", нужно: обычный текст без звёздочек\n"
	if out, code := runStyle(t, clean, "-"); code != exitStyleViolations || out != oldClean {
		t.Errorf("без --doc на сводке: код %d, вывод\n%s", code, out)
	}
	oldDirty := "4: десятичная запятая: \"3,5\", нужно: десятичная точка: '2.5'\n" +
		"5: длинное тире: \"Готово — выкачено\", нужно: обычный дефис с пробелами: ' - '\n"
	if out, code := runStyle(t, dirty, "-"); code != exitStyleViolations || out != oldDirty {
		t.Errorf("без --doc на сводке с находками: код %d, вывод\n%s", code, out)
	}
}

// --keys: префиксы без пробелов и в любом регистре
func TestStyleCmdDocKeys(t *testing.T) {
	out, code := runStyle(t, "OPS-1 сдвинут\nOTHER-2 чужой\nPROJ-3 тоже\n", "--doc", "--keys", " proj , ops", "-")
	want := "1: ключ задачи без ссылки: \"OPS-1\""
	if code != exitStyleViolations || !strings.HasPrefix(out, want) || strings.Contains(out, "OTHER-2") || !strings.Contains(out, "3: ключ задачи без ссылки: \"PROJ-3\"") {
		t.Fatalf("--keys: код %d, вывод\n%s", code, out)
	}
}

// useConfig — путь конфига, как у --config, до конца теста
func useConfig(t *testing.T, path string) {
	t.Helper()
	t.Setenv("CHOOKS_TEST_CONFIG", path)
}

// styleConfig кладёт конфиг chooks с телом body и делает его конфигом теста
func styleConfig(t *testing.T, body string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	useConfig(t, path)
}

// Ключ задачи в черновике: вне URL — находка с голым URL из конфига, внутри
// URL — нет; --keys заменяет префиксы конфига; без префиксов вывод прежний
func TestStyleCmdDraftKey(t *testing.T) {
	styleConfig(t, "style:\n  key_prefixes: [PROJ]\n  issue_url: https://jira.example/browse/\n")
	out, code := runStyle(t, "Готово\nсм. PROJ-12\n", "-")
	want := "2: ключ задачи без ссылки: \"PROJ-12\", нужно: голый URL: 'https://jira.example/browse/PROJ-12'\n"
	if code != exitStyleViolations || out != want {
		t.Fatalf("ключ вне URL: код %d, вывод %q", code, out)
	}
	if out, code := runStyle(t, "см. https://jira.example/browse/PROJ-12\n", "-"); code != exitAllowed || out != "" {
		t.Errorf("ключ в URL: код %d, вывод %q", code, out)
	}
	if out, code := runStyle(t, "OPS-3 и PROJ-4\n", "--keys", "ops", "-"); code != exitStyleViolations || !strings.Contains(out, "OPS-3") || strings.Contains(out, "PROJ-4") {
		t.Errorf("--keys без --doc: код %d, вывод %q", code, out)
	}
	if out, code := runStyle(t, "OTHER-5 чужой\n", "-"); code != exitAllowed || out != "" {
		t.Errorf("чужой префикс: код %d, вывод %q", code, out)
	}
	// --doc без --keys берёт префиксы конфига: чужой ключ не находка
	if out, code := runStyle(t, "OTHER-5 и PROJ-6\n", "--doc", "-"); code != exitStyleViolations || strings.Contains(out, "OTHER-5") || !strings.Contains(out, "PROJ-6") {
		t.Errorf("--doc с префиксами конфига: код %d, вывод %q", code, out)
	}
}

// Без префиксов (ни --keys, ни конфига) ключ в черновике не ищется: UTF-8
// и SHA-256 не находки; адреса нет — в подсказке шаблон
func TestStyleCmdDraftKeyDefaults(t *testing.T) {
	if out, code := runStyle(t, "кодировка UTF-8, хеш SHA-256, PROJ-1\n", "-"); code != exitAllowed || out != "" {
		t.Errorf("без префиксов: код %d, вывод %q", code, out)
	}
	out, code := runStyle(t, "PROJ-1\n", "--keys", "PROJ", "-")
	if code != exitStyleViolations || !strings.Contains(out, "голый URL: 'https://…/browse/PROJ-1'") {
		t.Errorf("без адреса: код %d, вывод %q", code, out)
	}
}

// Каждый ключ строки — своя находка с адресом своего префикса: issue_url
// конфига — только для префиксов конфига, префикс из --keys вне конфига
// получает шаблон адреса
func TestStyleCmdDraftKeysPerPrefix(t *testing.T) {
	styleConfig(t, "style:\n  key_prefixes: [proj]\n  issue_url: https://jira.example/browse/\n")
	out, code := runStyle(t, "OPS-3 и PROJ-4, снова OPS-3\n", "--keys", "ops,proj", "-")
	want := "1: ключ задачи без ссылки: \"OPS-3\", нужно: голый URL: 'https://…/browse/OPS-3'\n" +
		"1: ключ задачи без ссылки: \"PROJ-4\", нужно: голый URL: 'https://jira.example/browse/PROJ-4'\n"
	if code != exitStyleViolations || out != want {
		t.Fatalf("ключи по префиксам: код %d, вывод\n%s\nнужно\n%s", code, out, want)
	}
}

// С --doc черновик не проверяется: результат этой проверки не нужен
func TestStyleCmdDocSkipsDraftCheck(t *testing.T) {
	saved := checkDraft
	t.Cleanup(func() { checkDraft = saved })
	called := false
	checkDraft = func(text string, prefixes []string, issueURLs map[string]string) []textstyle.Finding {
		called = true
		return saved(text, prefixes, issueURLs)
	}
	if _, code := runStyle(t, "Готово — выкачено\n", "--doc", "-"); code != exitStyleViolations || called {
		t.Errorf("--doc: код %d, черновик проверен: %v", code, called)
	}
	if _, code := runStyle(t, "Готово — выкачено\n", "-"); code != exitStyleViolations || !called {
		t.Errorf("без --doc: код %d, черновик проверен: %v", code, called)
	}
}

// Битый конфиг — ошибка команды (код 2), не молчаливая проверка без ключей
func TestStyleCmdBadConfig(t *testing.T) {
	styleConfig(t, "style:\n  key_prefix: [PROJ]\n")
	exitCode := exitAllowed
	cmd := newStyleCmd(&exitCode)
	cmd.PersistentFlags().String("config", os.Getenv("CHOOKS_TEST_CONFIG"), "test config")
	cmd.SetIn(strings.NewReader("текст\n"))
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"-"})
	err := cmd.Execute()
	if exitCode != exitStyleError || err == nil || !strings.Contains(err.Error(), "key_prefix") {
		t.Errorf("битый конфиг: код %d, ошибка %v", exitCode, err)
	}
}

// Пустой префикс в --keys («PROJ,») отбрасывается: «-5» не ключ
func TestStyleCmdDocKeysEmptyPrefix(t *testing.T) {
	if out, code := runStyle(t, "диапазон 3 -5\n", "--doc", "--keys", "PROJ,", "-"); code != exitAllowed || out != "" {
		t.Fatalf("пустой префикс: код %d, вывод %q", code, out)
	}
}

// runtimeProc — дерево процессов для проверки модели claude: хук — процесс
// запроса, его родитель — claude, запущенный агентом на haiku
type runtimeProc struct{ hook int }

func (p runtimeProc) Parent(pid int) (int, error) {
	if pid == p.hook {
		return 90, nil
	}
	return 1, nil
}
func (runtimeProc) Exe(int) (string, error)  { return "/opt/claude/claude", nil }
func (runtimeProc) Comm(int) (string, error) { return "claude", nil }
func (runtimeProc) Environ(int) ([]string, error) {
	return []string{"CLAUDECODE=1"}, nil
}
func (runtimeProc) Cmdline(int) ([]string, error) {
	return []string{"claude", "--model", "haiku"}, nil
}
func (runtimeProc) Cwd(int) (string, error) { return "/", nil }

type panicJudge struct{}

func (panicJudge) ValidateTool(context.Context, *core.ToolInput) (*core.ValidationResult, error) {
	panic("judge broke")
}
func (panicJudge) CandidateModels(context.Context, *core.ToolInput) []string { panic("judge broke") }

// Паника судьи модели не блокирует реплику: rc 0 и строка Error в логе
func TestRunUserPromptSubmit_RuntimeGuardPanicPasses(t *testing.T) {
	config, _ := silentConfig(t)
	request := runtimeGuardRequest(t, t.TempDir())
	logger, logPath := fileLogger(t)
	ctx := context.WithValue(hookctx.WithRequest(context.Background(), request), runtimeGuardKey{}, runtimeGuardDeps{proc: runtimeProc{hook: request.PID}, judge: panicJudge{}})
	data := []byte(`{"hook_event_name":"UserPromptSubmit","session_id":"panic-test","prompt":"x"}`)
	code, err := runUserPromptSubmit(ctx, hookUserPromptSubmit, data, config, logger, newHookTimerForRequest(hookUserPromptSubmit, request))
	if code != exitAllowed || err != nil {
		t.Fatalf("runUserPromptSubmit = %d, %v; want %d, nil", code, err, exitAllowed)
	}
	log := readLog(t, logPath)
	if !strings.Contains(log, "level=ERROR") || !strings.Contains(log, "паника в проверке модели claude") || !strings.Contains(log, "judge broke") {
		t.Fatalf("log lacks panic error line:\n%s", log)
	}
}
