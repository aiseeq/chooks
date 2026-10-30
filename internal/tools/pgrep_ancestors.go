package tools

import (
	"errors"
	"fmt"
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
// Не трогает pgrep в кавычках (там он чаще уходит по ssh, где procps может
// быть старым) и команды с here-doc: разбор по ним грубый. pgrep в скрипте
// bash -c тоже переписывается: и его шелл, и шелл Claude Code — предки pgrep.
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
	positions := directPgrepPositions(command, calls)
	nested, nestedFailed := nestedPgrepPositions(command, calls)
	positions = append(positions, nested...)
	err = errors.Join(err, nestedFailed)
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

func directPgrepPositions(command string, calls []shellcmd.Call) []int {
	var positions []int
	for _, call := range calls {
		if call.Name != "pgrep" || call.Offset < 0 || !needsAncestors(call.Args) || call.Offset >= len(command) {
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

// nestedPgrepPositions пропускает скрипт, который не разобрался: внешний
// разбор уже назвал его в NestedError. Возвращает ошибки глубже вложенных
func nestedPgrepPositions(command string, calls []shellcmd.Call) ([]int, error) {
	var positions []int
	var failed []error
	for _, call := range calls {
		if call.Offset < 0 || !shellName(call.Name) {
			continue
		}
		script := shellCommandScript(call.Args)
		if script == "" {
			continue
		}
		source, found := scriptSource(command, call.Offset, script)
		if !found {
			continue
		}
		nested, err := shellcmd.Calls(script)
		var nestedErr *shellcmd.NestedError
		if err != nil && !errors.As(err, &nestedErr) {
			continue
		}
		if err != nil {
			failed = append(failed, err)
		}
		for _, nestedCall := range nested {
			if nestedCall.Name != "pgrep" || nestedCall.Offset < 0 || !needsAncestors(nestedCall.Args) {
				continue
			}
			if end, ok := pgrepWordEnd(script, nestedCall.Offset); ok {
				positions = append(positions, source[end-1]+1)
			}
		}
	}
	return positions, errors.Join(failed...)
}

// scriptSource ищет после from слово в кавычках, чьё значение равно script,
// и возвращает для каждого байта значения его позицию в command: в двойных
// кавычках \$ \` \" \\ занимают два байта исходника
func scriptSource(command string, from int, script string) ([]int, bool) {
	for start := from; start < len(command); start++ {
		var source []int
		switch command[start] {
		case '\'':
			source, start = singleQuotedSource(command, start)
		case '"':
			source, start = doubleQuotedSource(command, start)
		default:
			continue
		}
		if len(source) == len(script) && sourceValue(command, source) == script {
			return source, true
		}
	}
	return nil, false
}

// singleQuotedSource и doubleQuotedSource возвращают позиции байтов значения
// и позицию закрывающей кавычки
func singleQuotedSource(command string, open int) ([]int, int) {
	var source []int
	i := open + 1
	for ; i < len(command) && command[i] != '\''; i++ {
		source = append(source, i)
	}
	return source, i
}

func doubleQuotedSource(command string, open int) ([]int, int) {
	var source []int
	i := open + 1
	for ; i < len(command) && command[i] != '"'; i++ {
		if command[i] == '\\' && i+1 < len(command) && strings.IndexByte("$`\"\\", command[i+1]) >= 0 {
			i++
		}
		source = append(source, i)
	}
	return source, i
}

func sourceValue(command string, source []int) string {
	value := make([]byte, len(source))
	for i, pos := range source {
		value[i] = command[pos]
	}
	return string(value)
}

func shellName(name string) bool {
	return name == "bash" || name == "sh" || name == "zsh"
}

func pgrepWordEnd(command string, start int) (int, bool) {
	end := start
	for end < len(command) && !strings.ContainsRune(" \t\r\n;|&()<>\"'", rune(command[end])) {
		end++
	}
	return end, end > start && strings.HasSuffix(command[start:end], "pgrep")
}

// shellCommandScript возвращает аргумент -c у команды оболочки.
func shellCommandScript(args []string) string {
	for i, arg := range args {
		if strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") && strings.Contains(arg[1:], "c") && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
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
