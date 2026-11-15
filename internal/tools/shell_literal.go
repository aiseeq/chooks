package tools

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// word слово команды; literal — в нём нет подстановок $ и `
type word struct {
	text    string
	literal bool
}

// computedArg — слово из shellcmd.Call.Args вычисляется при запуске.
// shellcmd отдаёт литеральное слово значением, а слово с подстановкой —
// исходным текстом, и этот текст разбирается в одно слово с подстановкой.
// Значение литерала, которое само так выглядит ('$X'), тоже считается
// вычисляемым: отказ, а не пропуск. Признака литерала по слову в shellcmd
// нет (akit shellcmd-arg-literal-per-call)
func computedArg(arg string) bool {
	if !strings.ContainsAny(arg, "$`") {
		return false
	}
	var words []*syntax.Word
	err := syntax.NewParser().Words(strings.NewReader(arg), func(w *syntax.Word) bool {
		words = append(words, w)
		return true
	})
	if err != nil || len(words) != 1 {
		return false
	}
	_, literal := literalValue(words[0])
	return !literal
}

// literalValue значение слова без кавычек; false — в слове есть подстановка
func literalValue(w *syntax.Word) (string, bool) {
	var b strings.Builder
	for _, part := range w.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			b.WriteString(p.Value)
		case *syntax.SglQuoted:
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			for _, inner := range p.Parts {
				lit, ok := inner.(*syntax.Lit)
				if !ok {
					return "", false
				}
				b.WriteString(lit.Value)
			}
		default:
			return "", false
		}
	}
	return b.String(), true
}
