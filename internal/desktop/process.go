package desktop

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/aiseeq/chooks/internal/procfs"
)

// maxAncestors ограничивает подъём по дереву процессов: цепочка от хука
// до эмулятора терминала короткая, а зацикливаться на битом /proc нельзя
const maxAncestors = 16

// procDir — корень /proc; тесты подставляют своё дерево процессов
var procDir = "/proc"

// ProcessAncestors возвращает PID процесса и всех его предков, начиная с самого процесса.
// Окно терминала принадлежит одному из них: какому именно — решает оконный менеджер,
// поэтому список передаётся целиком и не требует знания конкретных эмуляторов терминала.
// Если цепочка оборвалась на нечитаемом процессе, возвращается собранная часть
// вместе с ошибкой: окно может принадлежать и ей
func ProcessAncestors(pid int) ([]int, error) {
	return ProcessAncestorsIn(procfs.OS{}, pid)
}

// ProcessAncestorsIn — ProcessAncestors по /proc из proc: запрос хука
// читает его через hookctx.Context.ProcFS (тень демона — ответы записи CLI)
func ProcessAncestorsIn(proc procfs.FS, pid int) ([]int, error) {
	ancestors := make([]int, 0, maxAncestors)
	seen := make(map[int]bool, maxAncestors)

	for len(ancestors) < maxAncestors && pid > 1 && !seen[pid] {
		seen[pid] = true
		ancestors = append(ancestors, pid)

		parent, err := parentPID(proc, pid)
		if err != nil {
			return ancestors, err
		}
		pid = parent
	}

	return ancestors, nil
}

// processNames возвращает имена процесса и его предков, начиная с самого процесса
func processNames(proc procfs.FS, pid int) ([]string, error) {
	ancestors, err := ProcessAncestorsIn(proc, pid)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(ancestors))
	for _, ancestor := range ancestors {
		commPath := procDir + "/" + strconv.Itoa(ancestor) + "/comm"
		data, err := proc.ReadFile(commPath)
		if err != nil {
			return nil, fmt.Errorf("cannot read %s: %w", commPath, err)
		}
		names = append(names, strings.TrimSpace(string(data)))
	}
	return names, nil
}

// parentPID читает PPid процесса из /proc/<pid>/stat
func parentPID(proc procfs.FS, pid int) (int, error) {
	statPath := procDir + "/" + strconv.Itoa(pid) + "/stat"
	data, err := proc.ReadFile(statPath)
	if err != nil {
		return 0, fmt.Errorf("cannot read %s: %w", statPath, err)
	}

	// Формат: pid (comm) state ppid ... — comm может содержать пробелы и скобки,
	// поэтому разбор начинается после последней закрывающей скобки
	stat := string(data)
	end := strings.LastIndex(stat, ")")
	if end == -1 || end+2 >= len(stat) {
		return 0, fmt.Errorf("unexpected format of %s: %q", statPath, stat)
	}

	fields := strings.Fields(stat[end+2:])
	if len(fields) < 2 {
		return 0, fmt.Errorf("unexpected format of %s: %q", statPath, stat)
	}

	parent, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, fmt.Errorf("unexpected ppid in %s: %w", statPath, err)
	}

	return parent, nil
}
