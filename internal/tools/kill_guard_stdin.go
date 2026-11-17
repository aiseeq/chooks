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
	// feed — команда, в которой разбор прочтёт скрипт этого ввода
	// (literalFeed): «левая часть пайпа | sh» (echo make | sh) или «sh <<<
	// слово»; feedVia — начало Via у вызовов скрипта в ней
	feed    string
	feedVia []string
}

// stdinScope текст, к которому отнесены места вызовов (Offset), и ввод
// текста целиком: у команды — свой, у текста за run0, pkexec — ввод обёртки
type stdinScope struct {
	text  string
	spans []stdinSpan
	in    stdinSpan
}

// shellCOption опция -c оболочки, возможно в связке (-c, -ec, -lc): слово за
// ней — скрипт
var shellCOption = regexp.MustCompile(`^-[a-zA-Z]*c[a-zA-Z]*$`)

// stdinPathRe пути к своему вводу после path.Clean: /dev/stdin, /dev/fd/0,
// /proc/self/fd/0, /proc/$$/fd/0, /proc/thread-self/fd/0
var stdinPathRe = regexp.MustCompile(`^/(dev/stdin|dev/fd/0+|proc/[^/]+/fd/0+)$`)

// fdPathRe путь к дескриптору после path.Clean: /dev/fd/N, /proc/*/fd/N
// (exec 3< "$F"; cat /dev/fd/3) — что в нём, не видно
var fdPathRe = regexp.MustCompile(`^/(dev/fd|proc/[^/]+/fd)/`)

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
				// |& добавляет к вводу вывод ошибок: ни файл, ни литерал
				sp := stdinSpan{start: base + int(x.Y.Pos().Offset()), end: base + int(x.Y.End().Offset()), src: stdinPipe}
				switch {
				case x.Op == syntax.PipeAll:
				case catFiles(x.X):
					sp.src = stdinFile
				default:
					sp.feed, sp.feedVia = text[x.X.Pos().Offset():x.X.End().Offset()]+" | sh", []string{"pipe", "sh"}
				}
				spans = append(spans, sp)
			}
		case *syntax.Stmt:
			if sp, ok := redirectInput(text, x.Redirs); ok {
				sp.start, sp.end = base+int(x.Pos().Offset()), base+int(x.End().Offset())
				spans = append(spans, sp)
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

// redirectInput ввод команды по её перенаправлениям (последнее действует;
// text — текст, в котором стоят слова перенаправлений): redirectSource по
// оператору и исходному тексту цели. ok — ввод перенаправлен
func redirectInput(text string, redirs []*syntax.Redirect) (stdinSpan, bool) {
	for _, r := range slices.Backward(redirs) {
		if r.N != nil && r.N.Value != "0" {
			continue
		}
		switch r.Op {
		case syntax.DplIn, syntax.RdrIn, syntax.RdrInOut, syntax.WordHdoc:
			return redirectSource(r.Op.String(), text[r.Word.Pos().Offset():r.Word.End().Offset()]), true
		case syntax.Hdoc, syntax.DashHdoc:
			return stdinSpan{src: stdinRedirect}, true
		}
	}
	return stdinSpan{}, false
}

// redirectSource ввод из перенаправления op с целью target — так же, как у
// оболочки прямо под systemd-run (systemdRunInputReason): <& — копия
// дескриптора, не видна; < и <> из подстановки ("$F", "$DIR/a.sh") и <<< из
// одной подстановки ("$CMD") — не видны, как пайп; <<< с иным текстом —
// перенаправление, а его текст разбор прочтёт как скрипт (literalFeed);
// < литерального файла, heredoc — перенаправление
func redirectSource(op, target string) stdinSpan {
	switch {
	case op == "<&":
		return stdinSpan{src: stdinPipe}
	case (op == "<" || op == "<>") && computedArg(target), op == "<<<" && wholeSubstitution(target):
		return stdinSpan{src: stdinPipe}
	case op == "<<<":
		return stdinSpan{src: stdinRedirect, feed: "sh <<< " + target, feedVia: []string{"sh"}}
	}
	return stdinSpan{src: stdinRedirect}
}

// catFiles — левая часть пайпа: cat с литеральными файлами без опций и
// перенаправлений (cat build.sh, cat a.sh b.sh): текст скрипта лежит в
// файле, как у sh < build.sh. Не файл скрипта: маска (*.sh — какие файлы,
// видно при запуске), ввод и дескриптор (/dev/fd/3, /proc/self/fd/3)
func catFiles(st *syntax.Stmt) bool {
	call, ok := st.Cmd.(*syntax.CallExpr)
	if !ok || len(st.Redirs) > 0 || len(call.Assigns) > 0 || len(call.Args) < 2 || call.Args[0].Lit() != "cat" {
		return false
	}
	for _, w := range call.Args[1:] {
		v, literal := literalValue(w)
		if !literal || v == "" || strings.HasPrefix(v, "-") || stdinPath(v) || fdPathRe.MatchString(path.Clean(v)) ||
			slices.ContainsFunc(w.Parts, globPart) {
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
func (s *killScan) stdinOf(c shellcmd.Call) stdinSpan {
	in := s.inputOf(c)
	if in.src != stdinOwn && in.src != stdinRedirect && afterXargs(c) {
		return stdinSpan{src: stdinPipe}
	}
	return in
}

// afterXargs — c идёт за xargs или parallel: слова их ввода — аргументы c
func afterXargs(c shellcmd.Call) bool {
	return slices.Contains(c.Via, "xargs") || slices.Contains(c.Via, "parallel")
}

// inputOf ввод c без поправки на xargs (stdinOf)
func (s *killScan) inputOf(c shellcmd.Call) stdinSpan {
	for _, r := range slices.Backward(c.Redirs) {
		if (r.N == "" || r.N == "0") && strings.HasPrefix(r.Op, "<") {
			target, err := redirTargetWord(r)
			if err != nil {
				// значение с байтами, которых bash в кавычках не держит:
				// текст не прочесть — как невидимый ввод
				s.tool.Logger().Warn("kill_guard: текст <<< не прочитан", "error", err)
				return stdinSpan{src: stdinPipe}
			}
			return redirectSource(r.Op, target)
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
		return scope.spans[best]
	case scope.in.src != stdinOwn || !c.ScriptInput:
		return scope.in
	}
	return stdinSpan{src: stdinPipe}
}

// literalFeed вызовы скрипта, который оболочка c под systemd-run читает из
// пайпа с литеральным текстом или из <<< (echo make | sh -c 'systemd-run
// --pipe bash', sh -c 'echo make | run0 systemd-run --pipe bash', sh -c
// 'systemd-run --pipe bash' <<< make): разбор shellcmd читает
// такой скрипт только у пайпа прямо в оболочку, а сквозь sh -c и run0 — нет.
// Ввод разбирается в команде feed (stdinSpan). Вызовы идут с Via цепочки
// c и «pipe» и без места: их проверяет s.call, как вызовы скрипта. nil —
// скрипт не литеральный, уже прочитан разбором или c — сам вызов такого
// скрипта
func (s *killScan) literalFeed(c shellcmd.Call, calls []shellcmd.Call) []shellcmd.Call {
	if !slices.Contains(c.Via, "systemd-run") || slices.Contains(c.Via, "pipe") || !shellcmd.IsShell(c.Name) {
		return nil
	}
	if _, runner := scriptArgs(c); runner || !shellReadsInput(c.Args) || scriptBodyParsed(c, calls) {
		return nil
	}
	in := s.stdinOf(c)
	if in.feed == "" || afterXargs(c) {
		return nil
	}
	parsed, err := shellcmd.Calls(in.feed)
	if err != nil {
		s.tool.Logger().Debug("kill_guard: ввод из пайпа не разобран, скрипт не виден", "feed", in.feed, "error", err)
		return nil
	}
	var body []shellcmd.Call
	for _, x := range parsed {
		if len(x.Via) >= len(in.feedVia) && slices.Equal(x.Via[:len(in.feedVia)], in.feedVia) {
			x.Via = slices.Concat(c.Via, []string{c.Name, "pipe"}, x.Via[len(in.feedVia):])
			x.Offset = -1
			body = append(body, x)
		}
	}
	return body
}

// globPart часть слова без кавычек с символом маски (*, ?, [)
func globPart(p syntax.WordPart) bool {
	lit, ok := p.(*syntax.Lit)
	return ok && strings.ContainsAny(lit.Value, "*?[")
}

// redirTargetWord цель перенаправления shellcmd словом оболочки: у
// литерального слова Target — значение (его надо взять в кавычки), иначе —
// исходный текст
func redirTargetWord(r shellcmd.Redir) (string, error) {
	if r.Op != "<<<" || strings.ContainsAny(r.Target, "$`\"'") {
		return r.Target, nil
	}
	q, err := syntax.Quote(r.Target, syntax.LangBash)
	if err != nil {
		return "", fmt.Errorf("слово <<< в кавычки: %w", err)
	}
	return q, nil
}
