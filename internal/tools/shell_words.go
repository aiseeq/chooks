package tools

import (
	"path/filepath"
	"strings"
)

// word слово оболочки без кавычек; literal — в нём нет подстановок $ и `
type word struct {
	text    string
	literal bool
}

// shellLexer грубый разбор оболочки: слова и простые команды
type shellLexer struct {
	cmds    [][]word
	cur     []word
	buf     strings.Builder
	literal bool
	inWord  bool
	quote   rune
	// redirTarget — следующее слово цель перенаправления (файл или номер
	// дескриптора), а не аргумент команды
	redirTarget bool
	// hereDoc — цель перенаправления — разделитель here-doc (<< или <<-)
	hereDoc  bool
	stripTab bool
	hereDocs []hereDoc
}

// hereDoc разделитель тела here-doc; stripTab — <<-, строки тела и
// разделитель могут начинаться табуляцией
type hereDoc struct {
	name     string
	stripTab bool
}

func (l *shellLexer) flushWord() {
	if l.inWord {
		switch {
		case l.hereDoc:
			l.hereDocs = append(l.hereDocs, hereDoc{name: l.buf.String(), stripTab: l.stripTab})
		case !l.redirTarget:
			l.cur = append(l.cur, word{text: l.buf.String(), literal: l.literal})
		}
		l.redirTarget, l.hereDoc = false, false
	}
	l.buf.Reset()
	l.literal, l.inWord = true, false
}

func (l *shellLexer) flushCmd() {
	l.flushWord()
	l.redirTarget, l.hereDoc = false, false
	if len(l.cur) > 0 {
		l.cmds = append(l.cmds, l.cur)
	}
	l.cur = nil
}

// redirect разбирает перенаправление, начатое с runes[i] (> < или &>), и
// возвращает индекс его последнего символа. Номер дескриптора перед ним
// (2>/dev/null) и цель после (2>&1, >/dev/null) в аргументы не попадают:
// иначе kill PID 2>&1 видел бы цели «2>» и «1»
func (l *shellLexer) redirect(runes []rune, i int) int {
	if l.inWord && l.literal && isDigits(l.buf.String()) {
		l.buf.Reset()
		l.inWord = false
	}
	l.flushWord()
	l.redirTarget = true
	switch {
	case runes[i] == '&':
		i++ // &> и &>>
	case runes[i] == '<' && next(runes, i) == '<' && next(runes, i+1) == '<':
		return i + 2 // here-string <<<
	case runes[i] == '<' && next(runes, i) == '<':
		i++
		l.hereDoc, l.stripTab = true, next(runes, i) == '-'
		if l.stripTab {
			i++
		}
		return i
	}
	for strings.ContainsRune(">&|", next(runes, i)) {
		i++ // >> >& >| <&
	}
	return i
}

// next символ после runes[i]; за концом строки — 0
func next(runes []rune, i int) rune {
	if i+1 < len(runes) {
		return runes[i+1]
	}
	return 0
}

// isRedirect — с runes[i] начинается перенаправление; >( и <( — подстановка
// процесса, её команда разбирается как обычная
func isRedirect(runes []rune, i int) bool {
	r := runes[i]
	if r == '&' {
		return i+1 < len(runes) && runes[i+1] == '>' && (i+2 >= len(runes) || runes[i+2] != '(')
	}
	return (r == '>' || r == '<') && (i+1 >= len(runes) || runes[i+1] != '(')
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// add добавляет символ слова; $ и ` делают слово вычисляемым
func (l *shellLexer) add(r rune) {
	if r == '$' || r == '`' {
		l.literal = false
	}
	l.buf.WriteRune(r)
	l.inWord = true
}

// quoted обрабатывает символ внутри кавычек
func (l *shellLexer) quoted(r rune) {
	switch {
	case r == l.quote:
		l.quote = 0
	case l.quote == '\'':
		l.buf.WriteRune(r)
	default:
		l.add(r)
	}
}

// splitSimpleCommands режет команду на простые по ; & | ( ) и переводу строки
// вне кавычек, перенаправления в слова не попадают. Разбор грубый, но для
// поиска kill и его аргументов достаточен
func splitSimpleCommands(command string) [][]word {
	return lexShell(command).cmds
}

// lexShell разбирает команду на простые команды и разделители here-doc
func lexShell(command string) *shellLexer {
	l := &shellLexer{literal: true}
	runes := []rune(command)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case l.quote != 0:
			l.quoted(r)
		case r == '\'' || r == '"':
			l.quote, l.inWord = r, true
		case isRedirect(runes, i):
			i = l.redirect(runes, i)
		case strings.ContainsRune(";&|\n()", r):
			l.flushCmd()
		case r == ' ' || r == '\t':
			l.flushWord()
		default:
			l.add(r)
		}
	}
	l.flushCmd()
	return l
}

// unwrapCommand снимает обёртки sudo, timeout, nohup, env, xargs, time и
// ключевые слова оболочки перед командой (if, do, !, {) и возвращает имя
// утилиты и её аргументы
func unwrapCommand(cmd []word) (name string, args []word, viaXargs bool) {
	for len(cmd) > 0 {
		head := filepath.Base(cmd[0].text)
		cmd = cmd[1:]
		switch head {
		case "if", "then", "else", "elif", "while", "until", "do", "!", "{", "builtin":
		case "sudo", "nohup", "env", "exec", "command", "nice", "setsid", "time":
			for len(cmd) > 0 && (strings.HasPrefix(cmd[0].text, "-") || strings.Contains(cmd[0].text, "=")) {
				cmd = cmd[1:]
			}
		case "timeout":
			for len(cmd) > 0 && strings.HasPrefix(cmd[0].text, "-") {
				cmd = cmd[1:]
			}
			if len(cmd) > 0 {
				cmd = cmd[1:] // длительность
			}
		case "xargs":
			viaXargs = true
			for len(cmd) > 0 && strings.HasPrefix(cmd[0].text, "-") {
				cmd = cmd[1:]
			}
		default:
			return head, cmd, viaXargs
		}
	}
	return "", nil, viaXargs
}

// innerScript скрипт, который команда исполнит как команды оболочки:
// аргумент после -c у sh, bash и подобных (флаги -lc, -ec тоже), аргументы
// eval через пробел
func innerScript(name string, args []word) (string, bool) {
	if name == "eval" {
		texts := make([]string, 0, len(args))
		for _, a := range args {
			texts = append(texts, a.text)
		}
		return strings.Join(texts, " "), len(texts) > 0
	}
	if name == "ssh" || !shellNames[name] {
		return "", false
	}
	withC := false
	for i := 0; i < len(args); i++ {
		a := args[i].text
		switch {
		case a == "-o" || a == "+o":
			i++ // имя опции set -o
		case a == "--":
			if withC && i+1 < len(args) {
				return args[i+1].text, true
			}
			return "", false
		case strings.HasPrefix(a, "-") || strings.HasPrefix(a, "+"):
			// -c, -lc, -ec; длинные опции (--norc) и +x не в счёт
			withC = withC || (a[0] == '-' && a[1] != '-' && strings.ContainsRune(a[1:], 'c'))
		default:
			return a, withC
		}
	}
	return "", false
}
