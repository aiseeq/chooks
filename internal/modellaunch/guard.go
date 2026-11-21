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
	Comm(pid int) (string, error)
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
	claudePID, cmdline, cmdlineErr, err := g.findClaude(hookPID)
	if err != nil {
		g.logDecision(0, "unknown", "ancestry_unreadable", "unknown", "skipped")
		return "", nil
	}
	if claudePID == 0 {
		g.logDecision(0, "unknown", "claude_not_found", "unknown", "skipped")
		return "", nil
	}
	procEnv, err := g.proc.Environ(claudePID)
	if err != nil {
		g.logger.Warn("modellaunch: окружение claude не прочитано, проверка пропущена", "pid", claudePID, "error", err)
		g.logDecision(claudePID, "unknown", "claude_environ_unreadable", "unknown", "skipped")
		return "", nil
	}
	agent, basis := agentEnvironment(procEnv)
	if !agent {
		parentPID, parentErr := g.proc.Parent(claudePID)
		if parentErr != nil {
			g.logDecision(claudePID, "unknown", "ancestor_read_incomplete", "not_checked", "skipped")
			return "", nil
		}
		var complete bool
		agent, complete = g.hasClaudeAncestor(parentPID)
		if agent {
			basis = "ancestor_claude"
		} else if complete {
			basis = "no_agent_markers_or_claude_ancestor"
		} else {
			g.logDecision(claudePID, "unknown", "ancestor_read_incomplete", "not_checked", "skipped")
			return "", nil
		}
	}
	return g.checkModel(ctx, claudePID, agent, basis, cmdline, cmdlineErr, procEnv)
}

func (g *Guard) checkModel(ctx context.Context, pid int, agent bool, basis string, cmdline []string, cmdlineErr error, procEnv []string) (string, error) {
	if cmdlineErr != nil {
		g.logger.Warn("modellaunch: cmdline claude не прочитан, проверка пропущена", "pid", pid, "error", cmdlineErr)
		g.logDecision(pid, agentLabel(agent), basis, "unknown", "skipped")
		return "", nil
	}
	cwd, cwdErr := g.proc.Cwd(pid)
	if cwdErr != nil && agent {
		g.logDecision(pid, "agent", basis, "unknown", "skipped_cwd_unreadable")
		return "", nil
	}
	if cwdErr != nil {
		basis += ";cwd_unreadable=" + cwdErr.Error()
	}
	command := literalCommand(cmdline, procEnv)
	if command == "" && agent {
		g.logDecision(pid, "agent", basis, "unknown", "skipped_cmdline_invalid")
		return "", nil
	}
	input := &core.ToolInput{ToolName: "Bash", Command: command, CWD: cwd}
	model := "unknown"
	if cwdErr == nil && command != "" {
		model = strings.Join(g.models.CandidateModels(input), ",")
		if model == "" {
			model = "unspecified"
		}
	}
	if !agent {
		g.logDecision(pid, "user", basis, model, "allowed")
		return "", nil
	}
	result, err := g.models.ValidateTool(ctx, input)
	if err != nil {
		g.logDecision(pid, "agent", basis, model, "error")
		return "", err
	}
	if result.IsValid {
		g.logDecision(pid, "agent", basis, model, "allowed")
		return "", nil
	}
	g.logDecision(pid, "agent", basis, model, "rejected")
	if len(result.Violations) == 0 {
		return rejectionPrefix, nil
	}
	return rejectionPrefix + ": " + result.Violations[0].Message, nil
}

func (g *Guard) findClaude(hookPID int) (int, []string, error, error) {
	seen := map[int]bool{}
	pid := hookPID
	for pid > 0 && !seen[pid] {
		seen[pid] = true
		parent, err := g.proc.Parent(pid)
		if err != nil {
			return 0, nil, nil, fmt.Errorf("parent %d: %w", pid, err)
		}
		if parent <= 1 || seen[parent] {
			break
		}
		exe, exeErr := g.proc.Exe(parent)
		comm, commErr := g.proc.Comm(parent)
		argv, argvErr := g.proc.Cmdline(parent)
		if isClaude(exe, exeErr, comm, commErr, argv, argvErr) {
			return parent, argv, argvErr, nil
		}
		pid = parent
	}
	return 0, nil, nil, nil
}

func (g *Guard) hasClaudeAncestor(pid int) (bool, bool) {
	seen := map[int]bool{}
	for pid > 1 && !seen[pid] {
		seen[pid] = true
		exe, exeErr := g.proc.Exe(pid)
		comm, commErr := g.proc.Comm(pid)
		argv, argvErr := g.proc.Cmdline(pid)
		if isClaude(exe, exeErr, comm, commErr, argv, argvErr) {
			return true, true
		}
		parent, err := g.proc.Parent(pid)
		if err != nil {
			return false, false
		}
		pid = parent
	}
	return false, true
}

func isClaude(exe string, exeErr error, comm string, commErr error, argv []string, argvErr error) bool {
	if exeErr == nil && filepath.Base(exe) == "claude" {
		return true
	}
	if commErr == nil && strings.TrimSpace(comm) == "claude" {
		return true
	}
	if argvErr != nil {
		return false
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

func agentEnvironment(env []string) (bool, string) {
	for _, item := range env {
		key, value, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		switch key {
		case "CLAUDECODE":
			if value == "1" {
				return true, key
			}
		case "CLAUDE_CODE_SESSION_ID", "CLAUDE_PID":
			return true, key
		}
	}
	return false, ""
}

func agentLabel(agent bool) string {
	if agent {
		return "agent"
	}
	return "user"
}

func (g *Guard) logDecision(pid int, actor, basis, model, outcome string) {
	g.logger.Info("modellaunch: runtime model decision", "claude_pid", pid, "actor", actor, "basis", basis, "model", model, "result", outcome)
}

func literalCommand(argv, env []string) string {
	if len(argv) == 0 {
		return ""
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
	return "env " + strings.Join(words, " ")
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

func (p ProcReader) Comm(pid int) (string, error) {
	data, err := readFile(filepath.Join(p.root(), strconv.Itoa(pid), "comm"))
	return strings.TrimSpace(string(data)), err
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
