package tools

import (
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/aiseeq/chooks/internal/core"
	"mvdan.cc/sh/v3/syntax"
)

// Запись в определение агента разбирается белым списком: содержимое известно,
// только если в команде на этот файл ровно одна запись и она ровно формы
// `cat > agents/x.md <<'EOF'` (см. knownAgentWriteBody). Всё прочее, что пишет
// или может писать в агента, каталог agents или .claude (другой писатель,
// другое перенаправление, cp/mv/install в каталог, переменная с путём, путь
// в данных для xargs и т. п.), — «содержимое не проверить». Разбор settings
// этот ответ не меняет.

// agentsPathToken — слово-путь с компонентом agents или каталогом .claude
// (`{`, `,` — фигурные скобки bash, `=`/`:` — значение опции)
var agentsPathToken = regexp.MustCompile(`(?i)(?:^|[/=:{,])agents(?:/|$|[,}])|(?:^|[/=:{,])\.claude/?(?:$|[,}])`)

// agentsPathInText — путь agents/ или каталог .claude внутри текста с
// пробелами (строка скрипта, тело heredoc): слово agents без косой рядом —
// проза, не путь
var agentsPathInText = regexp.MustCompile(`(?i)(?:^|[^a-z0-9_.-])agents/|/agents(?:$|[^a-z0-9_.-])|\.claude/?(?:$|[\s"'),;|&>}])`)

func mentionsAgentsPath(text string) bool {
	if strings.ContainsAny(text, " \t\n") {
		return agentsPathInText.MatchString(text)
	}
	return agentsPathToken.MatchString(text)
}

// agentSafeCommands только читают названные пути, создают каталог или
// удаляют файл: содержимого определения агента они не задают
var agentSafeCommands = []string{
	"cat", "head", "tail", "wc", "grep", "egrep", "fgrep", "rg", "diff", "cmp", "stat", "file",
	"test", "[", "realpath", "readlink", "basename", "dirname", "sha256sum", "sha1sum", "md5sum",
	"du", "tree", "echo", "ls", "mkdir", "rm", "rmdir", "touch", "less", "more",
}

var agentSafeGitSubcommands = []string{"add", "diff", "log", "show", "status", "ls-files", "blame", "rm"}

// commandWrappers запускают следующую за опциями команду
var commandWrappers = []string{"env", "timeout", "nohup", "nice", "command", "exec", "stdbuf", "time", "sudo", "ionice", "setsid"}

// catShadowCommands могут подменить cat функцией, alias или PATH
var catShadowCommands = []string{"alias", "hash", "enable", "source", ".", "eval"}

var catShadowVariables = []string{"PATH", "BASH_ENV", "ENV"}

type agentWriteEvent struct {
	target string
	stmt   *syntax.Stmt
	redir  *syntax.Redirect
}

type agentWriteScan struct {
	parent  map[syntax.Node]syntax.Node
	events  []agentWriteEvent
	writers map[*syntax.Stmt]bool
	shadow  bool
	unknown bool
}

// writtenAgentDefinitionCandidates — модели из распознанных записей в
// определения агентов и признак записи, содержимое которой не проверить
func writtenAgentDefinitionCandidates(command string, logger core.Logger) ([]modelCandidate, bool) {
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		logger.Debug("model_guard: команда записи определения агента не разобрана", "error", err)
		return nil, mentionsAgentsPath(command)
	}
	scan := &agentWriteScan{parent: map[syntax.Node]syntax.Node{}, writers: map[*syntax.Stmt]bool{}}
	var stack []syntax.Node
	syntax.Walk(file, func(node syntax.Node) bool {
		if node == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		if len(stack) > 0 {
			scan.parent[node] = stack[len(stack)-1]
		}
		stack = append(stack, node)
		scan.visit(node)
		return true
	})
	syntax.Walk(file, func(node syntax.Node) bool {
		if word, ok := node.(*syntax.Word); ok && !scan.wordAccounted(word) {
			scan.unknown = true
		}
		return true
	})
	var found []modelCandidate
	count := map[string]int{}
	for _, event := range scan.events {
		count[event.target]++
	}
	for _, event := range scan.events {
		body, known := knownAgentWriteBody(event.stmt, event.redir)
		if !known || scan.shadow || count[event.target] != 1 {
			scan.unknown = true
			continue
		}
		_, model := agentFrontmatter([]byte(body), event.target)
		if model != "" && !strings.EqualFold(model, "inherit") {
			found = append(found, modelCandidate{value: model})
		}
	}
	return found, scan.unknown
}

func (s *agentWriteScan) visit(node syntax.Node) {
	switch n := node.(type) {
	case *syntax.FuncDecl:
		s.shadow = s.shadow || shadowsCat(n)
	case *syntax.Assign:
		if n.Name != nil && slices.Contains(catShadowVariables, n.Name.Value) {
			s.shadow = true
		}
	case *syntax.CallExpr:
		s.shadow = s.shadow || callShadowsCat(n)
	case *syntax.Stmt:
		s.scanStatementWrites(n)
	}
}

func shadowsCat(decl *syntax.FuncDecl) bool {
	if decl.Name != nil && decl.Name.Value == "cat" {
		return true
	}
	for _, name := range decl.Names {
		if name.Value == "cat" {
			return true
		}
	}
	return false
}

func callShadowsCat(call *syntax.CallExpr) bool {
	if len(call.Args) == 0 {
		return false
	}
	name, literal := literalValue(call.Args[0])
	if !literal {
		return false
	}
	base := filepath.Base(name)
	if slices.Contains(catShadowCommands, base) {
		return true
	}
	if base != "shopt" {
		return false
	}
	for _, arg := range call.Args[1:] {
		if strings.Contains(wordOwnText(arg), "expand_aliases") {
			return true
		}
	}
	return false
}

func (s *agentWriteScan) scanStatementWrites(stmt *syntax.Stmt) {
	for _, redirect := range stmt.Redirs {
		if !writingRedirect(redirect) {
			continue
		}
		if target, literal := literalValue(redirect.Word); literal {
			target = strings.Trim(target, `"'`)
			if writtenAgentTarget.MatchString(target) {
				s.events = append(s.events, agentWriteEvent{target: filepath.Clean(target), stmt: stmt, redir: redirect})
				s.writers[stmt] = true
				continue
			}
		}
		if mentionsAgentsPath(wordOwnText(redirect.Word)) {
			s.unknown = true
			s.writers[stmt] = true
		}
	}
}

// knownAgentWriteBody — тело записи, если она ровно разрешённой формы:
// простая команда cat (без аргументов или с одним -), без присваиваний
// перед ней; ровно два перенаправления: stdout `>` или `>|` в цель (fd 1)
// и heredoc на fd 0 с кавычками в ограничителе (или без кавычек, но тело
// без $, ` и \ — bash его не меняет); тело с frontmatter
func knownAgentWriteBody(stmt *syntax.Stmt, target *syntax.Redirect) (string, bool) {
	_, ok := knownCatCall(stmt)
	if !ok || !knownAgentOutput(target) || len(stmt.Redirs) != 2 {
		return "", false
	}
	heredoc := agentHeredoc(stmt, target)
	if heredoc == nil || heredoc.Hdoc == nil || heredoc.N != nil && heredoc.N.Value != "0" {
		return "", false
	}
	body := heredoc.Hdoc.Lit()
	if !heredocDelimiterQuoted(heredoc) && strings.ContainsAny(body, "$`\\") {
		return "", false
	}
	if heredoc.Op == syntax.DashHdoc {
		lines := strings.Split(body, "\n")
		for i := range lines {
			lines[i] = strings.TrimLeft(lines[i], "\t")
		}
		body = strings.Join(lines, "\n")
	}
	if !agentFrontmatterBlock.MatchString(body) {
		return "", false
	}
	return body, true
}

func knownCatCall(stmt *syntax.Stmt) (*syntax.CallExpr, bool) {
	call, ok := stmt.Cmd.(*syntax.CallExpr)
	if !ok || len(call.Assigns) > 0 || len(call.Args) == 0 || len(call.Args) > 2 {
		return nil, false
	}
	if name, literal := literalValue(call.Args[0]); !literal || !slices.Contains([]string{"cat", "/bin/cat", "/usr/bin/cat"}, name) {
		return nil, false
	}
	if len(call.Args) == 2 {
		if dash, literal := literalValue(call.Args[1]); !literal || dash != "-" {
			return nil, false
		}
	}
	return call, true
}

func knownAgentOutput(target *syntax.Redirect) bool {
	return (target.Op == syntax.RdrOut || target.Op == syntax.RdrClob) && (target.N == nil || target.N.Value == "1")
}

func agentHeredoc(stmt *syntax.Stmt, target *syntax.Redirect) *syntax.Redirect {
	heredoc := stmt.Redirs[0]
	if heredoc == target {
		heredoc = stmt.Redirs[1]
	}
	return heredoc
}

func heredocDelimiterQuoted(redirect *syntax.Redirect) bool {
	for _, part := range redirect.Word.Parts {
		switch part.(type) {
		case *syntax.SglQuoted, *syntax.DblQuoted:
			return true
		}
	}
	return false
}

// writingRedirect — перенаправление, которое открывает файл на запись
func writingRedirect(redirect *syntax.Redirect) bool {
	switch redirect.Op {
	case syntax.RdrOut, syntax.AppOut, syntax.RdrClob, syntax.RdrAll, syntax.AppAll, syntax.RdrAllClob, syntax.AppAllClob, syntax.RdrInOut:
		return redirect.Word != nil
	case syntax.DplOut:
		value, literal := literalValue(redirect.Word)
		return !literal || strings.Trim(value, "0123456789-") != ""
	}
	return false
}

// stdoutRedirected — stdout команды уходит не в терминал, а в файл или
// чужой дескриптор
func stdoutRedirected(stmt *syntax.Stmt) bool {
	for _, redirect := range stmt.Redirs {
		stdout := redirect.N == nil || redirect.N.Value == "1"
		switch redirect.Op {
		case syntax.RdrAll, syntax.AppAll, syntax.RdrAllClob, syntax.AppAllClob:
			return true
		case syntax.RdrOut, syntax.AppOut, syntax.RdrClob:
			if stdout {
				return true
			}
		case syntax.RdrInOut:
			if redirect.N != nil && redirect.N.Value == "1" {
				return true
			}
		case syntax.DplOut:
			if value, _ := literalValue(redirect.Word); stdout && value != "2" {
				return true
			}
		}
	}
	return false
}

// wordOwnText — текст слова без вложенных команд: параметры и подстановки
// заменены на $, значения по умолчанию ${X:-…} оставлены (вложенные
// команды проверяются своими словами)
func wordOwnText(word *syntax.Word) string {
	if word == nil {
		return ""
	}
	var b strings.Builder
	var parts func([]syntax.WordPart)
	parts = func(list []syntax.WordPart) {
		for _, part := range list {
			switch p := part.(type) {
			case *syntax.Lit:
				b.WriteString(p.Value)
			case *syntax.SglQuoted:
				b.WriteString(p.Value)
			case *syntax.DblQuoted:
				parts(p.Parts)
			case *syntax.ParamExp:
				b.WriteString("$")
				if p.Exp != nil && p.Exp.Word != nil {
					parts(p.Exp.Word.Parts)
				}
				if p.Repl != nil {
					if p.Repl.Orig != nil {
						parts(p.Repl.Orig.Parts)
					}
					if p.Repl.With != nil {
						parts(p.Repl.With.Parts)
					}
				}
			case *syntax.ExtGlob:
				b.WriteString(p.Pattern.Value)
			default:
				b.WriteString("$")
			}
		}
	}
	parts(word.Parts)
	return b.String()
}

// wordAccounted — слово не упоминает путь агентов, или упоминание учтено:
// это цель записи (её решают события), аргумент клиента модели, читателя,
// чей вывод никуда не уходит, источник cp/mv/install, слово проверки [[ ]]
func (s *agentWriteScan) wordAccounted(word *syntax.Word) bool {
	switch s.parent[word].(type) {
	case *syntax.ParamExp:
		return true
	}
	if !mentionsAgentsPath(wordOwnText(word)) {
		return true
	}
	switch owner := s.parent[word].(type) {
	case *syntax.CallExpr:
		return s.callAccountsWord(owner, word)
	case *syntax.Assign:
		call, ok := s.parent[owner].(*syntax.CallExpr)
		return ok && s.modelClient(call)
	case *syntax.Redirect:
		stmt, ok := s.parent[owner].(*syntax.Stmt)
		if !ok {
			return false
		}
		if s.writers[stmt] {
			return true
		}
		if writingRedirect(owner) && owner.Word == word {
			return false
		}
		call, ok := stmt.Cmd.(*syntax.CallExpr)
		return ok && (s.modelClient(call) || s.safeReader(call) && s.harmless(call))
	case *syntax.TestClause, *syntax.BinaryTest, *syntax.UnaryTest, *syntax.ParenTest:
		return true
	}
	return false
}

func (s *agentWriteScan) callAccountsWord(call *syntax.CallExpr, word *syntax.Word) bool {
	if stmt, ok := s.parent[call].(*syntax.Stmt); ok && s.writers[stmt] {
		return true
	}
	if s.modelClient(call) {
		return true
	}
	if s.safeReader(call) {
		return s.harmless(call)
	}
	return copySource(call, word)
}

// copySource — слово — источник cp/mv/install: назначение — последний
// литерал, каталога назначения через -t нет. Само назначение не учтено
// никогда, поэтому назначение в agents или .claude даёт отказ
func copySource(call *syntax.CallExpr, word *syntax.Word) bool {
	name, literal := literalValue(call.Args[0])
	if !literal || !slices.Contains([]string{"cp", "mv", "install"}, filepath.Base(name)) || len(call.Args) < 3 {
		return false
	}
	for _, arg := range call.Args[1:] {
		value, ok := literalValue(arg)
		if value == "--" {
			break
		}
		if ok && (strings.HasPrefix(value, "--t") || strings.HasPrefix(value, "-") && !strings.HasPrefix(value, "--") && strings.Contains(value, "t")) {
			return false
		}
	}
	destination := call.Args[len(call.Args)-1]
	value, ok := literalValue(destination)
	if !ok || word == destination || strings.HasPrefix(value, "-") {
		return false
	}
	previous, ok := literalValue(call.Args[len(call.Args)-2])
	return ok && (previous == "--" || !strings.HasPrefix(previous, "-"))
}

func (s *agentWriteScan) safeReader(call *syntax.CallExpr) bool {
	if len(call.Args) == 0 {
		return false
	}
	name, literal := literalValue(call.Args[0])
	if !literal {
		return false
	}
	base := filepath.Base(name)
	if base == "git" && len(call.Args) > 1 {
		sub, ok := literalValue(call.Args[1])
		return ok && slices.Contains(agentSafeGitSubcommands, sub)
	}
	return slices.Contains(agentSafeCommands, base)
}

// modelClient — вызов клиента модели, в том числе за env, timeout и т. п.
func (s *agentWriteScan) modelClient(call *syntax.CallExpr) bool {
	wrapped := false
	for _, arg := range call.Args {
		value, literal := literalValue(arg)
		if !literal {
			return false
		}
		if wrapped && (strings.HasPrefix(value, "-") || strings.Contains(value, "=") || strings.Trim(value, "0123456789.smhd") == "") {
			continue
		}
		base := filepath.Base(value)
		if slices.Contains(commandWrappers, base) {
			wrapped = true
			continue
		}
		if base == "npx" {
			return hasClaudeCodePackage(call.Args)
		}
		return slices.Contains(modelClients, base)
	}
	return false
}

func hasClaudeCodePackage(args []*syntax.Word) bool {
	for i := range args {
		if value, ok := literalValue(args[i]); ok && value == "@anthropic-ai/claude-code" {
			return true
		}
	}
	return false
}

// harmless — вывод читателя идёт в терминал, в другой безвредный читатель
// или в аргумент клиента модели, а не в файл, переменную или писателя
func (s *agentWriteScan) harmless(call *syntax.CallExpr) bool {
	var node syntax.Node = call
	for {
		parent := s.parent[node]
		switch p := parent.(type) {
		case nil, *syntax.File:
			return true
		case *syntax.Stmt:
			if stdoutRedirected(p) {
				return false
			}
		case *syntax.BinaryCmd:
			if (p.Op == syntax.Pipe || p.Op == syntax.PipeAll) && node == syntax.Node(p.X) {
				next, ok := p.Y.Cmd.(*syntax.CallExpr)
				return ok && (s.modelClient(next) || s.safeReader(next) && s.harmless(next))
			}
		case *syntax.CmdSubst, *syntax.ProcSubst:
			return s.substitutionHarmless(p)
		}
		node = parent
	}
}

// substitutionHarmless — результат $(…) становится аргументом клиента
// модели или безвредного читателя (в том числе телом его heredoc)
func (s *agentWriteScan) substitutionHarmless(substitution syntax.Node) bool {
	node := substitution
	for {
		parent := s.parent[node]
		switch p := parent.(type) {
		case *syntax.CallExpr:
			return s.modelClient(p) || s.safeReader(p) && s.harmless(p)
		case *syntax.Redirect:
			if writingRedirect(p) {
				return false
			}
			stmt, ok := s.parent[p].(*syntax.Stmt)
			if !ok {
				return false
			}
			call, ok := stmt.Cmd.(*syntax.CallExpr)
			return ok && (s.modelClient(call) || s.safeReader(call) && s.harmless(call))
		case *syntax.Word, *syntax.DblQuoted, *syntax.ParamExp:
			node = parent
		default:
			return false
		}
	}
}
