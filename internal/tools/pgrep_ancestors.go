package tools

import (
	"regexp"
	"strings"
	"unicode"
)

// pgrepAncestorsFlag исключает из ответа pgrep его предков (здесь procps-ng 4.0.6)
const pgrepAncestorsFlag = "-A"

// pgrepPrefixes — слова, после которых следующее слово — снова команда
var pgrepPrefixes = map[string]bool{
	"!": true, "do": true, "then": true, "else": true, "elif": true, "if": true, "while": true, "until": true,
	"time": true, "command": true, "exec": true, "sudo": true, "nohup": true, "nice": true, "{": true,
}

var durationWord = regexp.MustCompile(`^[0-9.]+[smhd]?$`)

// RewritePgrepAncestors добавляет -A к каждому pgrep с -f в позиции команды.
// Claude Code запускает команду как bash -c '… <команда>', и argv этого
// шелла содержит шаблон: pgrep -f находит шелл, цикл «ждать, пока процесс
// кончится» не кончается никогда, проверка «процесс жив» всегда «да». За
// 19–26.10 так висели 32 фоновых цикла из 50 (отчёт bg-loops.md, «да»
// пользователя 26.10 17:59 CET: «"Рекомендую А" - согласен»). -A вместо
// скобок [x]yz: исключает и подоболочки $(…), и повтор шаблона в команде.
// Не трогает pgrep в кавычках (там он чаще уходит по ssh, где procps может
// быть старым) и команды с here-doc: разбор по ним грубый
func RewritePgrepAncestors(command string) (string, bool) {
	if !strings.Contains(command, "pgrep") || strings.Contains(command, "<<") {
		return command, false
	}
	var out strings.Builder
	changed := false
	last := 0
	for _, pos := range pgrepCommandPositions(command) {
		after := pos + len("pgrep")
		if !needsAncestors(command[after:]) {
			continue
		}
		out.WriteString(command[last:after])
		out.WriteString(" " + pgrepAncestorsFlag)
		last = after
		changed = true
	}
	if !changed {
		return command, false
	}
	out.WriteString(command[last:])
	return out.String(), true
}

// pgrepValueFlags — короткие ключи pgrep со значением следующим словом
const pgrepValueFlags = "dFgGPrstUu"

// needsAncestors разбирает ключи после pgrep: есть -f или --full и нет -A
func needsAncestors(rest string) bool {
	full := false
	fields := strings.Fields(rest)
	for i := 0; i < len(fields); i++ {
		field := fields[i]
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
		if strings.ContainsAny(field, ";|&)") {
			break
		}
	}
	return full
}

// pgrepCommandPositions — байтовые позиции слова pgrep, стоящего командой:
// вне кавычек (в том числе внутри $(…) в двойных кавычках и в скрипте
// bash -c) и первым словом простой команды или после префикса вроде sudo,
// timeout 5, !
func pgrepCommandPositions(command string) []int {
	// Контексты: 'c' — код, 's' — одинарные кавычки, 'd' — двойные
	s := &pgrepScanner{command: command, stack: []byte{'c'}}
	for i := 0; i < len(command); i++ {
		switch s.stack[len(s.stack)-1] {
		case 's':
			if command[i] == '\'' {
				s.pop()
			}
		case 'd':
			i = s.stepDouble(i)
		default:
			i = s.stepCode(i)
		}
	}
	return s.positions
}

// pgrepScanner — состояние разбора: стек контекстов и слова текущей
// простой команды до позиции
type pgrepScanner struct {
	command   string
	stack     []byte
	words     []string
	word      strings.Builder
	positions []int
}

func (s *pgrepScanner) pop() {
	if len(s.stack) > 1 {
		s.stack = s.stack[:len(s.stack)-1]
	}
}

func (s *pgrepScanner) flush() {
	if s.word.Len() > 0 {
		s.words = append(s.words, s.word.String())
		s.word.Reset()
	}
}

// newCommand — дальше начинается новая простая команда
func (s *pgrepScanner) newCommand() {
	s.flush()
	s.words = nil
}

func (s *pgrepScanner) subshellAt(i int) bool {
	return s.command[i] == '$' && i+1 < len(s.command) && s.command[i+1] == '('
}

// stepDouble — символ в двойных кавычках; возвращает последний разобранный
func (s *pgrepScanner) stepDouble(i int) int {
	switch {
	case s.command[i] == '\\':
		return i + 1
	case s.command[i] == '"':
		s.pop()
	case s.subshellAt(i):
		s.stack = append(s.stack, 'c')
		s.word.Reset()
		s.words = nil
		return i + 1
	}
	return i
}

// stepCode — символ кода; возвращает последний разобранный
func (s *pgrepScanner) stepCode(i int) int {
	command := s.command
	ch := command[i]
	switch {
	case ch == '\\':
		end := min(i+2, len(command))
		s.word.WriteString(command[i:end])
		return end - 1
	case (ch == '\'' || ch == '"') && s.word.Len() == 0 && shellScriptNext(s.words):
		// bash -c '…': скрипт в кавычках — тоже код, и его шелл тоже
		// предок pgrep
		end := closingQuote(command, i)
		for _, pos := range pgrepCommandPositions(command[i+1 : end]) {
			s.positions = append(s.positions, i+1+pos)
		}
		s.word.WriteString(command[i:min(end+1, len(command))])
		return end
	case ch == '\'' || ch == '"':
		s.word.WriteByte(ch)
		s.stack = append(s.stack, map[byte]byte{'\'': 's', '"': 'd'}[ch])
	case s.subshellAt(i):
		s.newCommand()
		s.stack = append(s.stack, 'c')
		return i + 1
	case strings.IndexByte("(;|&\n`", ch) >= 0:
		s.newCommand()
		if ch == '(' {
			s.stack = append(s.stack, 'c')
		}
	case ch == ')':
		s.newCommand()
		s.pop()
	case unicode.IsSpace(rune(ch)):
		s.flush()
	default:
		if s.word.Len() == 0 && pgrepWordAt(command, i) && commandPosition(s.words) {
			s.positions = append(s.positions, i)
		}
		s.word.WriteByte(ch)
	}
	return i
}

// pgrepWordAt — с позиции i стоит отдельное слово pgrep
func pgrepWordAt(command string, i int) bool {
	end := i + len("pgrep")
	return strings.HasPrefix(command[i:], "pgrep") && (end == len(command) || unicode.IsSpace(rune(command[end])))
}

var shellDashC = regexp.MustCompile(`^-[a-z]*c[a-z]*$`)

// shellScriptNext — следующее слово — скрипт для bash -c или sh -c, причём
// сам шелл стоит командой (timeout 900 bash -c, bash -euo pipefail -c)
func shellScriptNext(words []string) bool {
	if len(words) < 2 || !shellDashC.MatchString(words[len(words)-1]) {
		return false
	}
	for i, w := range words {
		if w == "bash" || w == "sh" {
			return commandPosition(words[:i])
		}
	}
	return false
}

// closingQuote — позиция кавычки, закрывающей открытую в start, или конец
// строки. В двойных кавычках учитывает \"; кавычки внутри $(…) — нет
func closingQuote(command string, start int) int {
	quote := command[start]
	for i := start + 1; i < len(command); i++ {
		switch {
		case quote == '"' && command[i] == '\\':
			i++
		case command[i] == quote:
			return i
		}
	}
	return len(command)
}

// commandPosition — следующее слово стоит командой: слов перед ним нет или
// все они — префиксы (sudo, !, do) и timeout с ключами и сроком
func commandPosition(words []string) bool {
	for i := 0; i < len(words); i++ {
		w := words[i]
		switch {
		case pgrepPrefixes[w]:
		case strings.Contains(w, "=") && i == 0:
			// Присваивание перед командой: VAR=1 pgrep …
		case w == "timeout":
			for i+1 < len(words) && strings.HasPrefix(words[i+1], "-") {
				i++
			}
			if i+1 >= len(words) || !durationWord.MatchString(words[i+1]) {
				return false
			}
			i++
		default:
			return false
		}
	}
	return true
}
