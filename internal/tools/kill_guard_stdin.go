package tools

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/aiseeq/akit/shellcmd"
	"github.com/aiseeq/chooks/internal/core"
	"mvdan.cc/sh/v3/syntax"
)

// stdinSource откуда вызов получает ввод
type stdinSource int

const (
	// stdinOwn — ввод не задан в тексте: тот же, что у агента
	stdinOwn stdinSource = iota
	// stdinPipe — из пайпа, текст которого до запуска не виден
	stdinPipe
	// stdinFile — из пайпа cat с литеральными файлами (cat build.sh | sh) —
	// как sh < build.sh
	stdinFile
	// stdinRedirect — из перенаправления (<, <<, <<<): его проверяют по
	// Redirs вызова
	stdinRedirect
)

// stdinSpan участок текста, у которого ввод задан: правая часть пайпа или
// команда с перенаправлением ввода. Вызов внутри (и в скрипте sh -c внутри)
// наследует этот ввод
type stdinSpan struct {
	start, end int
	src        stdinSource
}

// stdinScope текст, к которому отнесены места вызовов (Offset), и ввод
// текста целиком: у команды — свой, у текста за run0, pkexec — ввод обёртки
type stdinScope struct {
	text  string
	spans []stdinSpan
	src   stdinSource
}

// shellCOption опция -c оболочки, возможно в связке (-c, -ec, -lc): слово за
// ней — скрипт
var shellCOption = regexp.MustCompile(`^-[a-zA-Z]*c[a-zA-Z]*$`)

// stdinPathRe пути к своему вводу после path.Clean: /dev/stdin, /dev/fd/0,
// /proc/self/fd/0, /proc/$$/fd/0, /proc/thread-self/fd/0
var stdinPathRe = regexp.MustCompile(`^/(dev/stdin|dev/fd/0+|proc/[^/]+/fd/0+)$`)

// stdinPath — слово (операнд оболочки) называет её ввод: - или путь к нему
// в любом написании (/dev/fd/./0, //dev/stdin, "/proc/$$/fd/0")
func stdinPath(word string) bool {
	w := strings.NewReplacer(`"`, "", `'`, "").Replace(word)
	return w == "-" || strings.HasPrefix(w, "/") && stdinPathRe.MatchString(path.Clean(w))
}

// stdinSpans участки text (места — со сдвигом base) с заданным вводом, в том
// числе внутри литеральных скриптов -c (sh -c 'a | b', run0 sh -c '…'):
// места вызовов в них разбор отдаёт в тексте команды
func stdinSpans(log core.Logger, text string, base, depth int) ([]stdinSpan, error) {
	file, err := syntax.NewParser().Parse(strings.NewReader(text), "")
	if err != nil {
		return nil, fmt.Errorf("разбор bash: %w", err)
	}
	var spans []stdinSpan
	syntax.Walk(file, func(n syntax.Node) bool {
		switch x := n.(type) {
		case *syntax.BinaryCmd:
			if x.Op == syntax.Pipe || x.Op == syntax.PipeAll {
				src := stdinPipe
				if catFiles(x.X) {
					src = stdinFile
				}
				spans = append(spans, stdinSpan{base + int(x.Y.Pos().Offset()), base + int(x.Y.End().Offset()), src})
			}
		case *syntax.Stmt:
			if src, ok := redirectInput(x.Redirs); ok {
				spans = append(spans, stdinSpan{base + int(x.Pos().Offset()), base + int(x.End().Offset()), src})
			}
		case *syntax.CallExpr:
			if depth > 0 {
				spans = append(spans, scriptSpans(log, x, base, depth)...)
			}
		}
		return true
	})
	return spans, nil
}

// scriptSpans участки с заданным вводом в литеральных скриптах -c вызова x.
// Скрипт, который не разбирается, пропускается: его вызовы наследуют ввод x
func scriptSpans(log core.Logger, x *syntax.CallExpr, base, depth int) []stdinSpan {
	var spans []stdinSpan
	for i := 1; i < len(x.Args); i++ {
		if !shellCOption.MatchString(x.Args[i-1].Lit()) {
			continue
		}
		script, at, ok := scriptText(x.Args[i])
		if !ok {
			continue
		}
		inner, err := stdinSpans(log, script, base+at, depth-1)
		if err != nil {
			// такой скрипт не разберёт и shellcmd: вызовов в нём нет
			log.Debug("kill_guard: скрипт -c не разобран, участков ввода в нём нет", "error", err)
			continue
		}
		spans = append(spans, inner...)
	}
	return spans
}

// scriptText текст скрипта в слове 'текст' или "текст" без подстановок и
// \ — и место его начала: тогда места в нём совпадают с местами в команде
func scriptText(w *syntax.Word) (string, int, bool) {
	if len(w.Parts) != 1 {
		return "", 0, false
	}
	switch p := w.Parts[0].(type) {
	case *syntax.SglQuoted:
		if p.Dollar {
			return "", 0, false
		}
		return p.Value, int(p.Pos().Offset()) + 1, true
	case *syntax.DblQuoted:
		if len(p.Parts) != 1 {
			return "", 0, false
		}
		lit, ok := p.Parts[0].(*syntax.Lit)
		if !ok || strings.Contains(lit.Value, `\`) {
			return "", 0, false
		}
		return lit.Value, int(p.Pos().Offset()) + 1, true
	}
	return "", 0, false
}

// redirectInput ввод команды по её перенаправлениям (последнее действует):
// <, <<, <<-, <<<, <> — перенаправление, <& (копия дескриптора) — не виден,
// как пайп. ok — ввод перенаправлен
func redirectInput(redirs []*syntax.Redirect) (stdinSource, bool) {
	for _, r := range slices.Backward(redirs) {
		if r.N != nil && r.N.Value != "0" {
			continue
		}
		switch r.Op {
		case syntax.DplIn:
			return stdinPipe, true
		case syntax.RdrIn, syntax.RdrInOut, syntax.Hdoc, syntax.DashHdoc, syntax.WordHdoc:
			return stdinRedirect, true
		}
	}
	return stdinOwn, false
}

// catFiles — левая часть пайпа: cat с литеральными файлами без опций и
// перенаправлений (cat build.sh, cat a.sh b.sh): текст скрипта лежит в
// файле, как у sh < build.sh
func catFiles(st *syntax.Stmt) bool {
	call, ok := st.Cmd.(*syntax.CallExpr)
	if !ok || len(st.Redirs) > 0 || len(call.Assigns) > 0 || len(call.Args) < 2 || call.Args[0].Lit() != "cat" {
		return false
	}
	for _, w := range call.Args[1:] {
		v, literal := literalValue(w)
		if !literal || v == "" || strings.HasPrefix(v, "-") || stdinPath(v) {
			return false
		}
	}
	return true
}

// stdinScopeOf ввод текста команды: участки считаются при первом вопросе.
// Ошибка разбора bash (другой диалект) — участков нет: ввод вызовов — как
// его отметил shellcmd
func (s *killScan) stdinScopeOf() *stdinScope {
	if s.stdin == nil {
		s.stdin = &stdinScope{text: s.command}
		spans, err := stdinSpans(s.tool.Logger(), s.command, 0, 4)
		if err != nil {
			s.tool.Logger().Warn("kill_guard: ввод вызовов из текста не проверен, разбор bash не прошёл", "error", err)
			return s.stdin
		}
		s.stdin.spans = spans
	}
	return s.stdin
}

// stdinOf откуда вызов c получает ввод: своё перенаправление, самый узкий
// участок текста с заданным вводом вокруг c, ввод всего текста (за run0 —
// ввод обёртки) или отметка разбора (ScriptInput — пайп). За xargs и
// parallel из пайпа это не ввод команды, а её аргументы, текста которых не
// видно (… | xargs systemd-run --pipe bash: строки «-c», «kill 1» станут
// скриптом), — тот же пайп
func (s *killScan) stdinOf(c shellcmd.Call) stdinSource {
	for _, r := range slices.Backward(c.Redirs) {
		switch {
		case r.N != "" && r.N != "0" || !strings.HasPrefix(r.Op, "<"):
		case r.Op == "<&":
			// копия дескриптора (<&3, exec 3< "$F"): что в нём, не видно
			return stdinPipe
		default:
			return stdinRedirect
		}
	}
	scope := s.stdinScopeOf()
	best := -1
	for i, sp := range scope.spans {
		if c.Offset < sp.start || c.Offset >= sp.end {
			continue
		}
		if best < 0 || sp.end-sp.start <= scope.spans[best].end-scope.spans[best].start {
			best = i
		}
	}
	switch {
	case c.Offset >= 0 && best >= 0:
		return scope.spans[best].src
	case scope.src != stdinOwn || !c.ScriptInput:
		return scope.src
	}
	return stdinPipe
}
