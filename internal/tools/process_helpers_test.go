package tools

import (
	"os"

	"github.com/aiseeq/chooks/internal/core"
)

// Конструкторы от состояния своего процесса нужны только тестам: хук
// строит проверки от запроса (NewKillGuardToolForPID, NewRmGuardToolForRequest,
// NewGitGuardToolForRequest)

// NewKillGuardTool создает защиту от убийства сессии
func NewKillGuardTool(config core.ToolConfig, logger core.Logger) (*KillGuardTool, error) {
	return NewKillGuardToolForPID(config, logger, os.Getpid())
}

// NewGitGuardTool создаёт запрет разрушительных git-команд суб-агентам
func NewGitGuardTool(config core.ToolConfig, logger core.Logger) (*GitGuardTool, error) {
	return &GitGuardTool{BaseTool: NewBaseTool("git_guard", config.Enabled, core.ShellTools, logger), tempRoots: tempRoots()}, nil
}

// tempRoots — временные каталоги: репо в них — клон или проба суб-агента
// (scratchpad Claude Code — в CLAUDE_CODE_TMPDIR), деревья мастеров там не
// лежат. Сухой прогон 12.11: 112 из 122 отказов суб-агентам — их клоны в
// scratchpad
func tempRoots() []string {
	roots := []string{os.TempDir(), "/tmp"}
	if dir := os.Getenv("CLAUDE_CODE_TMPDIR"); dir != "" {
		roots = append(roots, dir)
	}
	return roots
}

// NewRmGuardTool создает проверку rm по переменной
func NewRmGuardTool(config core.ToolConfig, logger core.Logger) (*RmGuardTool, error) {
	return newRmGuardTool(config, logger, os.LookupEnv)
}
