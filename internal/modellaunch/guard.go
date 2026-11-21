// Package modellaunch checks the model of the Claude process that invoked a hook.
package modellaunch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/aiseeq/chooks/internal/core"
	"github.com/aiseeq/chooks/internal/tools"
)

const rejectionPrefix = "claude запущен агентом на модели вне списка разрешённых"

// ProcessReader reads the process attributes used to identify and inspect Claude.
type ProcessReader interface {
	Parent(pid int) (int, error)
	Exe(pid int) (string, error)
	Environ(pid int) ([]string, error)
	Cmdline(pid int) ([]string, error)
	Cwd(pid int) (string, error)
}

// Guard checks the nearest Claude ancestor of the hook process.
type Guard struct {
	proc   ProcessReader
	models *tools.ModelGuardTool
	logger core.Logger
}

func New(proc ProcessReader, models *tools.ModelGuardTool, logger core.Logger) *Guard {
	return &Guard{proc: proc, models: models, logger: logger}
}

// Check returns a rejection message only when an agent launched Claude with a
// model that model_guard does not approve. Proc read failures fail open.
func (g *Guard) Check(ctx context.Context, hookPID int) (string, error) {
	claudePID, hasClaudeAncestor, err := g.findClaude(hookPID)
	if err != nil {
		g.logger.Warn("modellaunch: /proc не прочитан, проверка пропущена", "error", err)
		return "", nil
	}
	if claudePID == 0 {
		g.logger.Warn("modellaunch: процесс claude среди предков хука не найден")
		return "", nil
	}
	procEnv, err := g.proc.Environ(claudePID)
	if err != nil {
		g.logger.Warn("modellaunch: окружение claude не прочитано, проверка пропущена", "error", err)
		return "", nil
	}
	if !agentEnvironment(procEnv) && !hasClaudeAncestor {
		return "", nil
	}

	cmdline, err := g.proc.Cmdline(claudePID)
	if err != nil {
		g.logger.Warn("modellaunch: cmdline claude не прочитан, проверка пропущена", "error", err)
		return "", nil
	}
	cwd, err := g.proc.Cwd(claudePID)
	if err != nil {
		g.logger.Warn("modellaunch: cwd claude не прочитан, проверка пропущена", "error", err)
		return "", nil
	}
	command, err := literalCommand(cmdline, procEnv)
	if err != nil {
		g.logger.Warn("modellaunch: команда claude не собрана, проверка пропущена", "error", err)
		return "", nil
	}
	result, err := g.models.ValidateTool(ctx, &core.ToolInput{ToolName: "Bash", Command: command, CWD: cwd})
	if err != nil {
		return "", err
	}
	if result.IsValid {
		return "", nil
	}
	if len(result.Violations) == 0 {
		return rejectionPrefix, nil
	}
	return rejectionPrefix + ": " + result.Violations[0].Message, nil
}

func (g *Guard) findClaude(hookPID int) (int, bool, error) {
	seen := map[int]bool{}
	pid := hookPID
	claudePID := 0
	hasClaudeAncestor := false
	for pid > 0 && !seen[pid] {
		seen[pid] = true
		parent, err := g.proc.Parent(pid)
		if err != nil {
			return 0, false, fmt.Errorf("parent %d: %w", pid, err)
		}
		if parent <= 1 || seen[parent] {
			break
		}
		exe, err := g.proc.Exe(parent)
		if err != nil {
			return 0, false, fmt.Errorf("exe %d: %w", parent, err)
		}
		argv, err := g.proc.Cmdline(parent)
		if err != nil {
			return 0, false, fmt.Errorf("cmdline %d: %w", parent, err)
		}
		if isClaude(exe, argv) {
			if claudePID == 0 {
				claudePID = parent
			} else {
				hasClaudeAncestor = true
			}
		}
		pid = parent
	}
	return claudePID, hasClaudeAncestor, nil
}

func isClaude(exe string, argv []string) bool {
	if filepath.Base(exe) == "claude" {
		return true
	}
	if len(argv) == 0 {
		return false
	}
	if filepath.Base(argv[0]) == "claude" {
		return true
	}
	if len(argv) < 2 {
		return false
	}
	// Claude Code's launcher is a Node script: argv[0] is node and argv[1]
	// names the installed claude entry point.
	return filepath.Base(argv[1]) == "claude" || filepath.Base(argv[1]) == "claude.js"
}

func agentEnvironment(env []string) bool {
	for _, item := range env {
		key, value, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		switch key {
		case "CLAUDECODE":
			if value == "1" {
				return true
			}
		case "CLAUDE_CODE_SESSION_ID", "CLAUDE_PID":
			if value != "" {
				return true
			}
		}
	}
	return false
}

func literalCommand(argv, env []string) (string, error) {
	if len(argv) == 0 {
		return "", fmt.Errorf("empty cmdline")
	}
	args := argv
	if filepath.Base(argv[0]) != "claude" && len(argv) > 1 && (filepath.Base(argv[1]) == "claude" || filepath.Base(argv[1]) == "claude.js") {
		args = argv[1:]
	}
	values := make(map[string]string, len(env))
	for _, item := range env {
		key, value, ok := strings.Cut(item, "=")
		if ok {
			values[key] = value
		}
	}
	keys := append(tools.ModelEnvironmentNames(), "CLAUDE_CONFIG_DIR", "HOME")
	var words []string
	for _, key := range keys {
		if value, ok := values[key]; ok {
			words = append(words, shellQuote(key+"="+value))
		}
	}
	for _, arg := range args {
		words = append(words, shellQuote(arg))
	}
	return "env " + strings.Join(words, " "), nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

// ProcReader is the Linux /proc implementation of ProcessReader.
type ProcReader struct{ Root string }

func (p ProcReader) root() string {
	if p.Root == "" {
		return "/proc"
	}
	return p.Root
}

func (p ProcReader) Parent(pid int) (int, error) {
	data, err := readFile(filepath.Join(p.root(), strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, err
	}
	close := strings.LastIndex(string(data), ")")
	if close < 0 {
		return 0, fmt.Errorf("malformed stat")
	}
	fields := strings.Fields(string(data[close+1:]))
	if len(fields) < 2 {
		return 0, fmt.Errorf("malformed stat")
	}
	return strconv.Atoi(fields[1])
}

func (p ProcReader) Exe(pid int) (string, error) {
	return readLink(filepath.Join(p.root(), strconv.Itoa(pid), "exe"))
}

func (p ProcReader) Environ(pid int) ([]string, error) {
	return splitNULFile(filepath.Join(p.root(), strconv.Itoa(pid), "environ"))
}

func (p ProcReader) Cmdline(pid int) ([]string, error) {
	return splitNULFile(filepath.Join(p.root(), strconv.Itoa(pid), "cmdline"))
}

func (p ProcReader) Cwd(pid int) (string, error) {
	return readLink(filepath.Join(p.root(), strconv.Itoa(pid), "cwd"))
}

func readFile(path string) ([]byte, error) { return os.ReadFile(path) }

func readLink(path string) (string, error) { return os.Readlink(path) }

func splitNULFile(path string) ([]string, error) {
	data, err := readFile(path)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(string(data), "\x00")
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts, nil
}
