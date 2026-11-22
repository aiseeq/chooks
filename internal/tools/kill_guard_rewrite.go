package tools

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/aiseeq/akit/shellcmd"
	"mvdan.cc/sh/v3/syntax"
)

// uniqueLongOptionName разрешает полное имя или однозначное сокращение,
// как getopt_long для systemd-run и run0.
func uniqueLongOptionName(name string, values, flags []string) string {
	var found string
	for _, n := range slices.Concat(values, flags) {
		if len(n) < 2 {
			continue
		}
		switch {
		case n == name:
			return n
		case name != "" && strings.HasPrefix(n, name):
			if found != "" {
				return ""
			}
			found = n
		}
	}
	return found
}

// maxNestedRewrites предел переписываний systemd-run в одной команде
const maxNestedRewrites = 16

// safeValueRe слово, которое переносится в текст команды без кавычек в
// любом месте (и внутри '…', "…" вложенного скрипта)
var safeValueRe = regexp.MustCompile(`^[A-Za-z0-9_.@=:/,+%-]*$`)

// callWords места слов вызова c в text: at — начало текста вызова (c.Raw),
// spans — слова от имени (spans[0]) и его аргументов, относительно at.
// spans nil — c нет в тексте словом; err — c.Raw не разобран на слова
func callWords(text string, c shellcmd.Call) (at int, spans [][2]int, err error) {
	if c.Offset < 0 {
		return 0, nil, nil
	}
	var words [][2]int
	err = syntax.NewParser().Words(strings.NewReader(c.Raw), func(w *syntax.Word) bool {
		words = append(words, [2]int{int(w.Pos().Offset()), int(w.End().Offset())})
		return true
	})
	if err != nil {
		return 0, nil, fmt.Errorf("слова вызова в %q: %w", c.Raw, err)
	}
	for k, w := range words {
		at = c.Offset - w[0]
		if at < 0 || at+len(c.Raw) > len(text) || text[at:at+len(c.Raw)] != c.Raw || len(words)-k-1 < len(c.Args) {
			continue
		}
		return at, words[k:], nil
	}
	return 0, nil, nil
}

// scriptValues опции script (util-linux) со значением отдельным словом
var scriptValues = []string{"c", "command", "E", "echo", "I", "log-in", "O", "log-out", "B", "log-io", "T", "log-timing",
	"m", "logging-format", "o", "output-limit"}

// scriptShell — script без -c (--command) запускает оболочку ($SHELL),
// которая читает его ввод: script -q /dev/null
func scriptShell(args []string) bool {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return true
		case strings.HasPrefix(a, "--"):
			name, _, inline := strings.Cut(a[2:], "=")
			if name == "command" || name == "help" || name == "version" {
				return false
			}
			if !inline && slices.Contains(scriptValues, name) {
				i++
			}
		case strings.HasPrefix(a, "-") && a != "-":
			letter, rest, next := shortValue(a, scriptValues)
			if letter == "c" || strings.ContainsAny(strings.TrimSuffix(a[1:], letter+rest), "hV") {
				return false
			}
			if next {
				i++
			}
		}
	}
	return true
}

// rewrittenCommand текст, которым проверяется вызов c вместо него самого:
// pkexec и machinectl shell с командой — их команда (elevatedCommand);
// программа, которая запускает оболочку на своём вводе (script без -c,
// machinectl shell без команды), — эта оболочка (sh); systemd-run --shell,
// не переписанный в тексте, — systemd-run --pipe bash; run0 --via-shell —
// скрипт sh -c из слов его команды
func rewrittenCommand(c shellcmd.Call) (string, bool) {
	switch c.Name {
	case "run0":
		_, start, viaShell := run0Options(c.Args)
		if !viaShell || start >= len(c.Args) {
			return "", false
		}
		return "sh -c " + singleQuotedWord(strings.Join(c.Args[start:], " ")), true
	case "systemd-run":
		return systemdRunShellCommand(c.Args)
	case "script":
		if scriptShell(c.Args) {
			return "sh", true
		}
		return "", false
	}
	return elevatedCommand(c)
}

// inputRedirs перенаправления ввода вызова c текстом для разбора вместе с
// его командой: тело heredoc, <<< и < разбор прочтёт у оболочки за
// systemd-run так же, как в исходной команде. Тело heredoc — под
// ограничителем в кавычках: подстановки в нём оболочка за обёрткой видит
// текстом, как скрипт
func inputRedirs(c shellcmd.Call) (string, error) {
	var out strings.Builder
	var bodies []string
	for _, r := range c.Redirs {
		if r.N != "" && r.N != "0" || !strings.HasPrefix(r.Op, "<") {
			continue
		}
		switch r.Op {
		case "<<", "<<-":
			end := heredocEnd(r.Body)
			out.WriteString(" " + r.Op + "'" + end + "'")
			bodies = append(bodies, heredocText(r.Body, end))
		default:
			word, err := redirWord(r.Target)
			if err != nil {
				return "", err
			}
			out.WriteString(" " + r.Op + " " + word)
		}
	}
	for _, b := range bodies {
		out.WriteString("\n" + b)
	}
	return out.String(), nil
}

// heredocText тело heredoc со строкой ограничителя end в конце
func heredocText(body, end string) string {
	if body == "" || strings.HasSuffix(body, "\n") {
		return body + end
	}
	return body + "\n" + end
}

// heredocEnd ограничитель тела heredoc в переписанной команде: слово,
// которого нет в теле, — иначе тело кончилось бы раньше
func heredocEnd(body string) string {
	for n := 0; ; n++ {
		if end := "CHOOKS_END" + strings.Repeat("_", n); !strings.Contains(body, end) {
			return end
		}
	}
}

// redirWord цель перенаправления словом оболочки: исходный текст слова с
// подстановкой или кавычками ("$F", <(…)) — как есть, значение литерала — в
// кавычках. err — значение в кавычки не поставить (нулевой байт)
func redirWord(target string) (string, error) {
	if strings.ContainsAny(target, "$`\"'\\") || strings.HasPrefix(target, "<(") || strings.HasPrefix(target, ">(") {
		return target, nil
	}
	q, err := syntax.Quote(target, syntax.LangBash)
	if err != nil {
		return "", fmt.Errorf("цель %q перенаправления ввода: %w", target, err)
	}
	return q, nil
}

// systemdRunCommandStart место первого слова команды в словах systemd-run
// (опции до первого операнда или --, "+" в getopt); len(args) — команды нет
func systemdRunCommandStart(args []string) int {
	return parseSystemdRunOptions(plainWords(args)).start
}
