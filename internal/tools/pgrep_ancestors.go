package tools

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/aiseeq/akit/shellcmd"
)

// pgrepAncestorsFlag исключает из ответа pgrep его предков (здесь procps-ng 4.0.6)
const pgrepAncestorsFlag = "-A"

// RewritePgrepAncestors добавляет -A к каждому pgrep с -f в позиции команды.
// Claude Code запускает команду как bash -c '… <команда>', и argv этого
// шелла содержит шаблон: pgrep -f находит шелл, цикл «ждать, пока процесс
// кончится» не кончается никогда, проверка «процесс жив» всегда «да». За
// 19–26.10 так висели 32 фоновых цикла из 50 (отчёт bg-loops.md, «да»
// пользователя 26.10 17:59 CET: «"Рекомендую А" - согласен»). -A вместо
// скобок [x]yz: исключает и подоболочки $(…), и повтор шаблона в команде.
// Не трогает pgrep, ушедший по ssh (procps там может быть старым), и
// команды с here-doc: разбор по ним грубый. pgrep в скрипте bash -c на любой
// глубине тоже переписывается: и его шелл, и шелл Claude Code — предки pgrep.
// Битый вложенный скрипт (*shellcmd.NestedError) не мешает переписать
// остальные: ошибка возвращается вместе с подменой
func RewritePgrepAncestors(command string) (string, bool, error) {
	if !strings.Contains(command, "pgrep") || strings.Contains(command, "<<") {
		return command, false, nil
	}
	calls, err := shellcmd.Calls(command)
	var nestedErr *shellcmd.NestedError
	if err != nil && !errors.As(err, &nestedErr) {
		return command, false, fmt.Errorf("shellcmd calls: %w", err)
	}
	positions := pgrepPositions(command, calls)
	if len(positions) == 0 {
		return command, false, err
	}
	sort.Ints(positions)
	var out strings.Builder
	out.Grow(len(command) + len(positions)*3)
	last := 0
	for _, pos := range positions {
		out.WriteString(command[last:pos])
		out.WriteString(" " + pgrepAncestorsFlag)
		last = pos
	}
	out.WriteString(command[last:])
	return out.String(), true, err
}

// pgrepPositions — места вставки -A в command: конец слова pgrep. Offset у
// вызовов во вложенных скриптах shellcmd даёт в координатах command
func pgrepPositions(command string, calls []shellcmd.Call) []int {
	var positions []int
	for _, call := range calls {
		if call.Name != "pgrep" || call.Offset < 0 || call.Offset >= len(command) || !needsAncestors(call.Args) || slices.Contains(call.Via, "ssh") {
			continue
		}
		if command[call.Offset] == '\'' || command[call.Offset] == '"' {
			continue
		}
		if end, ok := pgrepWordEnd(command, call.Offset); ok {
			positions = append(positions, end)
		}
	}
	return positions
}

func pgrepWordEnd(command string, start int) (int, bool) {
	end := start
	for end < len(command) && !strings.ContainsRune(" \t\r\n;|&()<>\"'", rune(command[end])) {
		end++
	}
	return end, end > start && strings.HasSuffix(command[start:end], "pgrep")
}

// pgrepValueFlags — короткие ключи pgrep со значением следующим словом
const pgrepValueFlags = "dFgGPrstUu"

// needsAncestors разбирает ключи после pgrep: есть -f или --full и нет -A
func needsAncestors(args []string) bool {
	full := false
	for i := 0; i < len(args); i++ {
		field := args[i]
		if field == "--" || field == "-" || !strings.HasPrefix(field, "-") {
			break
		}
		switch {
		case field == "--full":
			full = true
		case field == "--ignore-ancestors":
			return false
		case strings.HasPrefix(field, "--"):
		default:
			letters := strings.TrimLeft(field, "-")
			if strings.ContainsRune(letters, 'A') {
				return false
			}
			if strings.ContainsRune(letters, 'f') {
				full = true
			}
			if strings.ContainsRune(pgrepValueFlags, rune(letters[len(letters)-1])) {
				i++
			}
		}
	}
	return full
}
