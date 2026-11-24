// Package questioncheck — судья-модель для проверок хуков: вызов одобренной
// модели через её CLI (CLIJudge), выдержки из журнала реплик пользователя и
// сверка сообщения агента с его словами (said-bench), журнал срабатываний.
// Имя — от снятой 07.11 проверки вопросов (question-check), из которой это
// выросло
package questioncheck

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/aiseeq/akit/proc"
	"github.com/aiseeq/akit/secretmask"
	"github.com/aiseeq/chooks/internal/hookctx"
)

// Usage — токены вызова: по ним итог недели считает цену по ценам модели
type Usage struct {
	InputTokens       int `json:"input_tokens"`
	CachedInputTokens int `json:"cached_input_tokens,omitempty"`
	CacheWriteTokens  int `json:"cache_write_tokens,omitempty"`
	OutputTokens      int `json:"output_tokens"`
}

// CLIJudge зовёт модель через её CLI: opus — `claude -p` без инструментов и
// со своим системным промптом, sol и luna — `codex exec` в пустом каталоге
// без пользовательского конфига. Подписки те же, что у агентов
type CLIJudge struct {
	// Name — имя модели из ~/.config/models/approved (opus, sol, luna), ID — её id там
	Name string
	ID   string
	// Effort — усилие рассуждения: high или xhigh (не ниже high)
	Effort string
	// Dir — пустой каталог запуска: в нём нет CLAUDE.md и AGENTS.md
	Dir string
	// Prompt — промпт судьи
	Prompt string
	// CodexHome — каталог учётки codex (CODEX_HOME) для вызова; пусто —
	// CODEX_HOME из env запроса или умолчание codex (~/.codex). Каталог берётся
	// как есть: вход и auth.json судья не трогает
	CodexHome string
	// OutputSchema — путь к JSON-схеме ответа для `codex exec --output-schema`;
	// пусто — без схемы
	OutputSchema string
}

// Add — сумма токенов двух вызовов
func (u Usage) Add(other Usage) Usage {
	return Usage{InputTokens: u.InputTokens + other.InputTokens, CachedInputTokens: u.CachedInputTokens + other.CachedInputTokens,
		CacheWriteTokens: u.CacheWriteTokens + other.CacheWriteTokens, OutputTokens: u.OutputTokens + other.OutputTokens}
}

// Model — имя модели для журнала
func (j CLIJudge) Model() string {
	return j.Name + "/" + j.Effort
}

// Ask отдаёт модели промпт и случай и возвращает её ответ как есть. Это
// единственная дверь к модели: значения секретов скрываются здесь для любого
// входа, у моделей кроме opus текст уходит другому поставщику
func (j CLIJudge) Ask(ctx context.Context, input string) (string, Usage, error) {
	if j.Prompt == "" {
		return "", Usage{}, fmt.Errorf("judge %s has no prompt", j.Model())
	}
	input, _ = secretmask.Mask(input)
	if j.Name == "opus" {
		return j.askClaude(ctx, input)
	}
	return j.askCodex(ctx, input)
}

// judgeEnv — окружение вызова без признаков сессии-родителя: вложенный
// claude иначе принял бы чужое имя сессии и CLAUDE_PID
func judgeEnv(source []string) []string {
	env := make([]string, 0, len(source))
	for _, kv := range source {
		if strings.HasPrefix(kv, "CLAUDE_CODE_SESSION_NAME=") || strings.HasPrefix(kv, "CLAUDE_PID=") ||
			strings.HasPrefix(kv, "CLAUDE_CODE_SESSION_ID=") || strings.HasPrefix(kv, "CLAUDECODE=") {
			continue
		}
		env = append(env, kv)
	}
	return env
}

func (j CLIJudge) askClaude(ctx context.Context, input string) (string, Usage, error) {
	path, err := judgeExecutable(ctx, "claude")
	if err != nil {
		return "", Usage{}, err
	}
	cmd := exec.CommandContext(ctx, path, "-p",
		"--model", j.Name,
		"--system-prompt", j.Prompt,
		"--tools", "",
		"--setting-sources", "",
		"--strict-mcp-config",
		"--no-session-persistence",
		"--disable-slash-commands",
		"--effort", j.Effort,
		"--output-format", "json")
	cmd.Args[0] = "claude"
	out, err := runCLI(ctx, cmd, j.Dir, input)
	if err != nil {
		return "", Usage{}, err
	}
	var result struct {
		Result  string `json:"result"`
		IsError bool   `json:"is_error"`
		Usage   struct {
			InputTokens              int `json:"input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			OutputTokens             int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return "", Usage{}, fmt.Errorf("claude output is not JSON: %w", err)
	}
	usage := Usage{
		InputTokens:       result.Usage.InputTokens,
		CachedInputTokens: result.Usage.CacheReadInputTokens,
		CacheWriteTokens:  result.Usage.CacheCreationInputTokens,
		OutputTokens:      result.Usage.OutputTokens,
	}
	if result.IsError {
		return "", usage, fmt.Errorf("claude returned an error: %s", Excerpt(result.Result, 300))
	}
	return result.Result, usage, nil
}

func (j CLIJudge) askCodex(ctx context.Context, input string) (string, Usage, error) {
	path, err := judgeExecutable(ctx, "codex")
	if err != nil {
		return "", Usage{}, err
	}
	args := []string{"exec",
		"--json", "--skip-git-repo-check", "--ephemeral",
		"--ignore-user-config", "--ignore-rules",
		"--sandbox", "read-only",
		"-m", j.ID,
		"-c", fmt.Sprintf("model_reasoning_effort=%q", j.Effort),
		"-c", `web_search="disabled"`,
		"-c", "project_doc_max_bytes=0"}
	if j.OutputSchema != "" {
		args = append(args, "--output-schema", j.OutputSchema)
	}
	cmd := exec.CommandContext(ctx, path, append(args, "-")...)
	cmd.Args[0] = "codex"
	var env []string
	if j.CodexHome != "" {
		env = []string{"CODEX_HOME=" + j.CodexHome}
	}
	// stdout разбирается и при ненулевом коде: отказ хода (кредиты учётки)
	// codex пишет событием turn.failed или error в stdout, а stderr может его
	// не нести (ревью Terra 5ec62fa, п. 4)
	out, runErr := runCLIEnv(ctx, cmd, j.Dir, j.Prompt+"\n\nКоманд не запускай и файлов не читай: всё нужное ниже.\n\n"+input, env)
	var message, failed string
	var usage Usage
	scanner := bufio.NewScanner(bytes.NewReader(out))
	scanner.Buffer(make([]byte, 1<<20), 16<<20)
	for scanner.Scan() {
		var event struct {
			Type    string `json:"type"`
			Message string `json:"message"`
			Error   struct {
				Message string `json:"message"`
			} `json:"error"`
			Item struct {
				Type    string `json:"type"`
				Text    string `json:"text"`
				Message string `json:"message"`
			} `json:"item"`
			Usage struct {
				InputTokens       int `json:"input_tokens"`
				CachedInputTokens int `json:"cached_input_tokens"`
				OutputTokens      int `json:"output_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			continue
		}
		switch {
		case event.Type == "item.completed" && event.Item.Type == "agent_message":
			message = event.Item.Text
		// Отказ хода (кредиты учётки, вход, лимит) codex пишет событием, а не
		// кодом выхода: без него ответ был бы пустым без причины
		case event.Type == "turn.failed":
			failed = event.Error.Message
		case event.Type == "error" && failed == "":
			failed = event.Message
		case event.Type == "turn.completed":
			failed = ""
			// Кэш codex входит в input_tokens: некэшированная часть — разность
			usage.InputTokens += event.Usage.InputTokens - event.Usage.CachedInputTokens
			usage.CachedInputTokens += event.Usage.CachedInputTokens
			// reasoning_output_tokens — часть output_tokens, не добавка
			usage.OutputTokens += event.Usage.OutputTokens
		}
	}
	scanErr := scanner.Err()
	if runErr != nil {
		if failed != "" {
			return "", usage, fmt.Errorf("codex turn failed: %s: %w", Excerpt(failed, 300), runErr)
		}
		return "", usage, runErr
	}
	if scanErr != nil {
		return "", usage, fmt.Errorf("read codex output: %w", scanErr)
	}
	if failed != "" && message == "" {
		return "", usage, fmt.Errorf("codex turn failed: %s", Excerpt(failed, 300))
	}
	return message, usage, nil
}

// judgeExecutable — путь CLI судьи по PATH запроса
func judgeExecutable(ctx context.Context, name string) (string, error) {
	if request, ok := hookctx.Request(ctx); ok {
		path, err := request.LookPath(name)
		if err != nil {
			return "", fmt.Errorf("%s failed: %w: ", name, err)
		}
		return path, nil
	}
	return "", fmt.Errorf("hook request is missing")
}

// runCLI запускает вызов с текстом на stdin; ошибка несёт хвост stderr
func runCLI(ctx context.Context, cmd *exec.Cmd, dir, stdin string) ([]byte, error) {
	return runCLIEnv(ctx, cmd, dir, stdin, nil)
}

// runCLIEnv — runCLI с переменными extra поверх env запроса. CLI идёт своей
// группой процессов: по отмене контекста группа убивается целиком, и потомки
// CLI не остаются после хука. При ошибке возвращается и stdout: в нём бывает
// причина отказа
func runCLIEnv(ctx context.Context, cmd *exec.Cmd, dir, stdin string, extra []string) ([]byte, error) {
	cmd.Dir = dir
	request, ok := hookctx.Request(ctx)
	if !ok {
		return nil, fmt.Errorf("hook request is missing")
	}
	cmd.Env = withEnv(judgeEnv(request.Env), extra)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		_, err := proc.StopChildGroup(cmd, 0, syscall.SIGKILL)
		return err
	}
	// По таймауту контекст убивает группу CLI, а потомок вне группы мог бы
	// держать stdout: без WaitDelay хук ждал бы его и выходил за свой таймаут
	cmd.WaitDelay = 2 * time.Second
	cmd.Stdin = strings.NewReader(stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := runTracked(cmd)
	if err != nil {
		return out, fmt.Errorf("%s failed: %w: %s", cmd.Args[0], err, Excerpt(strings.TrimSpace(stderr.String()), 300))
	}
	return out, nil
}

// judgeGroups — группы процессов запущенных судей: по SIGTERM хука
// KillJudgeGroups убивает их до выхода процесса, иначе судья в своей группе
// пережил бы хук (ревью Terra 5ec62fa, п. 1)
var judgeGroups = struct {
	sync.Mutex
	pids        map[int]bool
	terminating bool
}{pids: map[int]bool{}}

// runTracked запускает cmd с учётом его группы и ждёт конца, как cmd.Output
func runTracked(cmd *exec.Cmd) ([]byte, error) {
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	judgeGroups.Lock()
	if judgeGroups.terminating {
		judgeGroups.Unlock()
		return nil, fmt.Errorf("hook is terminating, judge not started")
	}
	if err := cmd.Start(); err != nil {
		judgeGroups.Unlock()
		return nil, fmt.Errorf("start %s: %w", cmd.Args[0], err)
	}
	pid := cmd.Process.Pid
	judgeGroups.pids[pid] = true
	judgeGroups.Unlock()
	err := cmd.Wait()
	judgeGroups.Lock()
	delete(judgeGroups.pids, pid)
	judgeGroups.Unlock()
	return stdout.Bytes(), err
}

// KillJudgeGroups убивает группы всех идущих судей и запрещает новые: его
// зовёт обработчик SIGTERM CLI-хука перед выходом. Возвращает число групп
func KillJudgeGroups() int {
	judgeGroups.Lock()
	defer judgeGroups.Unlock()
	judgeGroups.terminating = true
	pids := slices.Sorted(maps.Keys(judgeGroups.pids))
	for _, pid := range pids {
		if _, err := proc.StopChild(pid, 0, func() error { return syscall.Kill(-pid, syscall.SIGKILL) }); err != nil {
			fmt.Fprintf(os.Stderr, "questioncheck: stop judge group %d: %v\n", pid, err)
		}
	}
	return len(judgeGroups.pids)
}

// withEnv заменяет в env переменные из extra («ИМЯ=значение»)
func withEnv(env, extra []string) []string {
	if len(extra) == 0 {
		return env
	}
	out := make([]string, 0, len(env)+len(extra))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		replaced := false
		for _, e := range extra {
			if n, _, _ := strings.Cut(e, "="); n == name {
				replaced = true
			}
		}
		if !replaced {
			out = append(out, kv)
		}
	}
	return append(out, extra...)
}
