package core

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/aiseeq/chooks/internal/procfs"
)

// PrintModeSessionIn сообщает, что хук вызвала неинтерактивная сессия `claude -p`.
// Режим определяется по командной строке процесса claude из /proc запроса.
// Пустой pid — сессия без CLAUDE_PID, считается интерактивной
func PrintModeSessionIn(proc procfs.FS, pid string) (bool, error) {
	if pid == "" {
		return false, nil
	}
	// Имя пришло из окружения: в путь оно попадать не должно
	safe := filepath.Base(strings.TrimSpace(pid))
	raw, err := proc.ReadFile(filepath.Join("/proc", safe, "cmdline"))
	if err != nil {
		return false, fmt.Errorf("cannot read cmdline of CLAUDE_PID %q: %w", pid, err)
	}
	args := string(raw)
	for more := true; more; {
		var arg string
		arg, args, more = strings.Cut(args, "\x00")
		if arg == "-p" || arg == "--print" {
			return true, nil
		}
	}
	return false, nil
}

// SessionAddDirs дополнительные рабочие папки сессии — значения --add-dir из
// командной строки процесса claude с данным pid (CLAUDE_PID): --add-dir=X и
// вариадическое --add-dir X Y …, до следующего флага. Относительные пути
// пропускаем: их база — cwd claude на старте, а не папка проекта. Папки,
// добавленные в беседе через /add-dir, в командной строке не видны.
// Пустой pid — папок нет
func SessionAddDirs(pid string) ([]string, error) {
	return SessionAddDirsIn(procfs.OS{}, pid)
}

// SessionAddDirsIn — SessionAddDirs по /proc запроса
func SessionAddDirsIn(proc procfs.FS, pid string) ([]string, error) {
	if pid == "" {
		return nil, nil
	}
	safe := filepath.Base(strings.TrimSpace(pid))
	raw, err := proc.ReadFile(filepath.Join("/proc", safe, "cmdline"))
	if err != nil {
		return nil, fmt.Errorf("cannot read cmdline of CLAUDE_PID %q: %w", pid, err)
	}
	return addDirArgs(strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")), nil
}

// addDirArgs абсолютные значения --add-dir из аргументов claude
func addDirArgs(args []string) []string {
	var dirs []string
	add := func(dir string) {
		if filepath.IsAbs(dir) {
			dirs = append(dirs, filepath.Clean(dir))
		}
	}
	variadic := false
	for _, arg := range args {
		switch {
		case arg == "--add-dir":
			variadic = true
		case strings.HasPrefix(arg, "--add-dir="):
			variadic = false
			add(strings.TrimPrefix(arg, "--add-dir="))
		case strings.HasPrefix(arg, "-"):
			variadic = false
		case variadic:
			add(arg)
		}
	}
	return dirs
}
