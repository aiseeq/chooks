package questioncheck

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/aiseeq/akit/session"
)

// SessionName читает имя сессии процесса claude из реестра Claude Code
// ~/.claude/sessions/<pid>.json: имя, заданное /rename или --name. Pid хук
// получает в CLAUDE_PID. Пустой pid — сессия без имени: признак не прочитан,
// и это ошибка — решает вызывающий
func SessionName(claudeDir, pid string) (string, error) {
	pid = strings.TrimSpace(pid)
	if pid == "" {
		return "", fmt.Errorf("CLAUDE_PID is empty")
	}
	number, err := strconv.Atoi(pid)
	if err != nil {
		return "", fmt.Errorf("CLAUDE_PID %q is not a pid: %w", pid, err)
	}
	info, err := session.Registry{Dir: filepath.Join(claudeDir, "sessions")}.Get(number)
	if err != nil {
		return "", fmt.Errorf("read session record: %w", err)
	}
	if info == nil {
		return "", fmt.Errorf("read session record: pid %d: %w", number, os.ErrNotExist)
	}
	return info.Name, nil
}

// ProjectDirName — имя каталога проекта в ~/.claude/projects: путь, в
// котором Claude Code заменяет «/» и «.» на «-»
func ProjectDirName(dir string) string {
	return strings.NewReplacer("/", "-", ".", "-").Replace(filepath.Clean(dir))
}
