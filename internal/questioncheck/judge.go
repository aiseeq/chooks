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
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/aiseeq/akit/secretmask"
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
	// Effort — усилие рассуждения (low, medium, high)
	Effort string
	// Dir — пустой каталог запуска: в нём нет CLAUDE.md и AGENTS.md
	Dir string
	// Prompt — промпт судьи
	Prompt string
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
func judgeEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "CLAUDE_CODE_SESSION_NAME=") || strings.HasPrefix(kv, "CLAUDE_PID=") ||
			strings.HasPrefix(kv, "CLAUDE_CODE_SESSION_ID=") || strings.HasPrefix(kv, "CLAUDECODE=") {
			continue
		}
		env = append(env, kv)
	}
	return env
}

func (j CLIJudge) askClaude(ctx context.Context, input string) (string, Usage, error) {
	cmd := exec.CommandContext(ctx, "claude", "-p",
		"--model", j.Name,
		"--system-prompt", j.Prompt,
		"--tools", "",
		"--setting-sources", "",
		"--strict-mcp-config",
		"--no-session-persistence",
		"--disable-slash-commands",
		"--effort", j.Effort,
		"--output-format", "json")
	out, err := runCLI(cmd, j.Dir, input)
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
	cmd := exec.CommandContext(ctx, "codex", "exec",
		"--json", "--skip-git-repo-check", "--ephemeral",
		"--ignore-user-config", "--ignore-rules",
		"--sandbox", "read-only",
		"-m", j.ID,
		"-c", fmt.Sprintf("model_reasoning_effort=%q", j.Effort),
		"-c", `web_search="disabled"`,
		"-c", "project_doc_max_bytes=0",
		"-")
	out, err := runCLI(cmd, j.Dir, j.Prompt+"\n\nКоманд не запускай и файлов не читай: всё нужное ниже.\n\n"+input)
	if err != nil {
		return "", Usage{}, err
	}
	var message string
	var usage Usage
	scanner := bufio.NewScanner(bytes.NewReader(out))
	scanner.Buffer(make([]byte, 1<<20), 16<<20)
	for scanner.Scan() {
		var event struct {
			Type string `json:"type"`
			Item struct {
				Type string `json:"type"`
				Text string `json:"text"`
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
		case event.Type == "turn.completed":
			// Кэш codex входит в input_tokens: некэшированная часть — разность
			usage.InputTokens += event.Usage.InputTokens - event.Usage.CachedInputTokens
			usage.CachedInputTokens += event.Usage.CachedInputTokens
			// reasoning_output_tokens — часть output_tokens, не добавка
			usage.OutputTokens += event.Usage.OutputTokens
		}
	}
	if err := scanner.Err(); err != nil {
		return "", usage, fmt.Errorf("read codex output: %w", err)
	}
	return message, usage, nil
}

// runCLI запускает вызов с текстом на stdin; ошибка несёт хвост stderr
func runCLI(cmd *exec.Cmd, dir, stdin string) ([]byte, error) {
	cmd.Dir = dir
	cmd.Env = judgeEnv()
	// По таймауту контекст убивает сам CLI, а его потомки держат stdout:
	// без WaitDelay хук ждал бы их и выходил за свой таймаут
	cmd.WaitDelay = 2 * time.Second
	cmd.Stdin = strings.NewReader(stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s failed: %w: %s", cmd.Args[0], err, Excerpt(strings.TrimSpace(stderr.String()), 300))
	}
	return out, nil
}
