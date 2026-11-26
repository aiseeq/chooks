package tools

import (
	"fmt"
	"strings"

	"github.com/aiseeq/akit/shellcmd"
	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/syntax"
)

func shellANSIValue(value string) (string, error) {
	for i := 0; i+2 < len(value); i++ {
		if value[i] != '\\' {
			continue
		}
		if value[i+1] == 'c' && value[i+2]&0x1f == 0 {
			return "", fmt.Errorf("ANSI-строка содержит NUL; убери управляющее экранирование и укажи значение литералом")
		}
		i++
	}
	decoded, _, err := expand.Format(nil, value, nil)
	if err != nil {
		return "", fmt.Errorf("decode ANSI string: %w", err)
	}
	if strings.ContainsRune(decoded, 0) {
		return "", fmt.Errorf("ANSI-строка содержит NUL; убери \\0 и укажи значение литералом")
	}
	return decoded, nil
}

// word слово команды; literal — в нём нет подстановок $ и `; braces — bash
// раскрывает в нём фигурные скобки ({a,b}, {1..3}) в несколько слов
type word struct {
	text              string
	literal           bool
	shellLiteral      bool
	shellLiteralKnown bool
	tilde             shellcmd.Tilde
	braces            bool
	invalidANSI       bool
	ansiEscapedQuote  bool
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

// wholeSubstitution — слово из shellcmd.Call.Args целиком одна подстановка:
// $X, "$X", ${X:-…}, "$(…)", `…`. Его текст при запуске — значение целиком,
// и как скрипт оно не видно. Подстановка рядом с литералом ("make -C $DIR",
// "$HOME/bin/tool") — нет
func wholeSubstitution(arg string) bool {
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
	parts := words[0].Parts
	if len(parts) == 1 {
		if dq, ok := parts[0].(*syntax.DblQuoted); ok {
			parts = dq.Parts
		}
	}
	if len(parts) != 1 {
		return false
	}
	switch parts[0].(type) {
	case *syntax.ParamExp, *syntax.CmdSubst:
		return true
	}
	return false
}

// literalValue значение слова без кавычек; false — в слове есть подстановка
func literalValue(w *syntax.Word) (string, bool) {
	var b strings.Builder
	for _, part := range w.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			b.WriteString(p.Value)
		case *syntax.SglQuoted:
			if p.Dollar {
				value, err := expand.Literal(nil, &syntax.Word{Parts: []syntax.WordPart{p}})
				if err != nil {
					// error-masked-as-false-bool: safe — false means the value is unknown and callers refuse or conservatively analyse it.
					return "", false
				}
				b.WriteString(value)
			} else {
				b.WriteString(p.Value)
			}
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
