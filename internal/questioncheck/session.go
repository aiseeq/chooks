package questioncheck

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aiseeq/akit/session"
)

// Until — конец эксперимента: после 06.11 23:59 по местному хук ничего не
// делает. Рамка пользователя 22.10 17:42 CEST (сессия lead): «Проверка
// вопросов моделью, неделю, в моей сессии и у work-admin?» — «да. Главное
// потом не забыть сделать вывод и принять окончательное решение». Итог 30.10:
// «Б» — оставить и донастроить судью, замер через неделю (lead 30.10 11:12
// CET, decisions.md question-checker-tune)
var Until = time.Date(untilYear, time.November, 7, 0, 0, 0, 0, time.Local)

// untilYear — год конца эксперимента
const untilYear = 2026

// Target — сессия, в которой проверка работает: имя сессии Claude Code и
// каталог, из которого она запущена (по нему назван каталог проекта
// транскрипта)
type Target struct {
	Name string
	Dir  string
}

// DefaultTargets — lead (~) и work-admin (~/work); остальным сессиям хук —
// no-op без вызова модели
func DefaultTargets(home string) []Target {
	return []Target{
		{Name: "lead", Dir: home},
		{Name: "work-admin", Dir: filepath.Join(home, "work")},
	}
}

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

// MatchTarget находит цель по имени сессии и сверяет её с каталогом проекта
// транскрипта: имя сессии одно, а каталог проекта подтверждает, что это та же
// работа (lead в ~, work-admin в ~/work). Расхождение — не цель
func MatchTarget(targets []Target, name, transcriptPath string) (Target, bool) {
	project := filepath.Base(filepath.Dir(transcriptPath))
	for _, target := range targets {
		if target.Name == name && project == ProjectDirName(target.Dir) {
			return target, true
		}
	}
	return Target{}, false
}

// ProjectDirName — имя каталога проекта в ~/.claude/projects: путь, в
// котором Claude Code заменяет «/» и «.» на «-»
func ProjectDirName(dir string) string {
	return strings.NewReplacer("/", "-", ".", "-").Replace(filepath.Clean(dir))
}
