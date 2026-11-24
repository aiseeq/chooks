package tools

import (
	"path/filepath"
	"regexp"
	"slices"
	"strings"

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
var agentsPathInText = regexp.MustCompile(`(?i)(?:^|[^a-z0-9_.-])agents/|/agents(?:$|[^a-z0-9_.-])|(?:^|[/~=:{,\s"'])\.claude/?(?:$|[\s"'),;|&>}])`)

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
	parent   map[syntax.Node]syntax.Node
	events   []agentWriteEvent
	writers  map[*syntax.Stmt]bool
	shadow   bool
	unknown  bool
	reason   string
	readOnly bool
}

// writtenAgentDefinitionCandidates — модели из распознанных записей в
// определения агентов и признак записи, содержимое которой не проверить
func writtenAgentDefinitionCandidates(command string) ([]modelCandidate, bool) {
	models, reason := agentWriteCandidates(command)
	return models, reason != ""
}

func agentWriteCandidates(command string) ([]modelCandidate, string) {
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		return nil, unparsedAgentReason(command)
	}
	scan := &agentWriteScan{parent: map[syntax.Node]syntax.Node{}, writers: map[*syntax.Stmt]bool{}, readOnly: readOnlyClaudeDirectory(file)}
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
			if scan.reason == "" {
				scan.reason = wordOwnText(word)
			}
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
			if scan.reason == "" {
				scan.reason = event.target
			}
			continue
		}
		_, model := agentFrontmatter([]byte(body), event.target)
		if model != "" && !strings.EqualFold(model, "inherit") {
			found = append(found, modelCandidate{value: model})
		}
	}
	if scan.unknown && scan.reason == "" {
		scan.reason = command
	}
	return found, scan.reason
}

func unparsedAgentReason(command string) string {
	if mentionsAgentsPath(command) {
		return command
	}
	return ""
}

// Для перехода в .claude неизвестные команды считаются пишущими: их
// относительные пути и побочные эффекты невозможно установить по словам.
func readOnlyClaudeDirectory(file *syntax.File) bool {
	safe := true
	syntax.Walk(file, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.Redirect:
			if writingRedirect(n) {
				v, ok := literalValue(n.Word)
				if !ok || v != "/dev/null" {
					safe = false
				}
			}
		case *syntax.CallExpr:
			if len(n.Args) == 0 {
				safe = false
				break
			}
			name, ok := literalValue(n.Args[0])
			if !ok {
				safe = false
				break
			}
			switch filepath.Base(name) {
			case "cd", "pushd", "jq", "cut", "head", "tail", "cat", "grep", "rg", "ls", "pwd", "claude", "cl":
			case "git":
				i := 1
				if len(n.Args) > 2 && wordOwnText(n.Args[1]) == "-C" {
					i = 3
				}
				if i >= len(n.Args) || !slices.Contains([]string{"log", "status", "diff", "show", "ls-files", "blame"}, wordOwnText(n.Args[i])) {
					safe = false
				}
			default:
				safe = false
			}
			for _, arg := range n.Args[1:] {
				v := wordOwnText(arg)
				if strings.HasPrefix(v, "--output") || v == "-o" || v == "--in-place" || v == "-i" && filepath.Base(name) == "sed" {
					safe = false
				}
			}
		}
		return true
	})
	return safe
}

func (s *agentWriteScan) visit(node syntax.Node) {
	switch n := node.(type) {
	case *syntax.FuncDecl:
		if n.Name != nil && n.Name.Value == "cat" {
			s.shadow = true
		}
		for _, name := range n.Names {
			s.shadow = s.shadow || name.Value == "cat"
		}
	case *syntax.Assign:
		if n.Name != nil && slices.Contains(catShadowVariables, n.Name.Value) {
			s.shadow = true
		}
	case *syntax.CallExpr:
		if len(n.Args) == 0 {
			return
		}
		name, literal := literalValue(n.Args[0])
		if !literal {
			return
		}
		if slices.Contains(catShadowCommands, filepath.Base(name)) {
			s.shadow = true
		}
		if filepath.Base(name) == "shopt" {
			for _, arg := range n.Args[1:] {
				s.shadow = s.shadow || strings.Contains(wordOwnText(arg), "expand_aliases")
			}
		}
	case *syntax.Stmt:
		for _, redirect := range n.Redirs {
			if !writingRedirect(redirect) {
				continue
			}
			if target, literal := literalValue(redirect.Word); literal {
				target = strings.Trim(target, `"'`)
				if writtenAgentTarget.MatchString(target) {
					s.events = append(s.events, agentWriteEvent{target: filepath.Clean(target), stmt: n, redir: redirect})
					s.writers[n] = true
					continue
				}
			}
			if mentionsAgentsPath(wordOwnText(redirect.Word)) {
				s.unknown = true
				if s.reason == "" {
					s.reason = wordOwnText(redirect.Word)
				}
				s.writers[n] = true
			}
		}
	}
}

// knownAgentWriteBody — тело записи, если она ровно разрешённой формы:
// простая команда cat (без аргументов или с одним -), без присваиваний
// перед ней; ровно два перенаправления: stdout `>` или `>|` в цель (fd 1)
// и heredoc на fd 0 с кавычками в ограничителе (или без кавычек, но тело
// без $, ` и \ — bash его не меняет); тело с frontmatter
func knownAgentWriteBody(stmt *syntax.Stmt, target *syntax.Redirect) (string, bool) {
	call, ok := stmt.Cmd.(*syntax.CallExpr)
	if !ok || len(call.Assigns) > 0 || len(call.Args) == 0 || len(call.Args) > 2 {
		return "", false
	}
	if name, literal := literalValue(call.Args[0]); !literal || !slices.Contains([]string{"cat", "/bin/cat", "/usr/bin/cat"}, name) {
		return "", false
	}
	if len(call.Args) == 2 {
		if dash, literal := literalValue(call.Args[1]); !literal || dash != "-" {
			return "", false
		}
	}
	if target.Op != syntax.RdrOut && target.Op != syntax.RdrClob || target.N != nil && target.N.Value != "1" {
		return "", false
	}
	if len(stmt.Redirs) != 2 {
		return "", false
	}
	heredoc := stmt.Redirs[0]
	if heredoc == target {
		heredoc = stmt.Redirs[1]
	}
	if heredoc.Hdoc == nil || heredoc.N != nil && heredoc.N.Value != "0" {
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
	if s.readOnly && len(call.Args) > 1 {
		name := wordOwnText(call.Args[0])
		value := wordOwnText(word)
		if !strings.Contains(strings.ToLower(value), "agents") && (name == "cd" || name == "pushd" || name == "git" && len(call.Args) > 2 && wordOwnText(call.Args[1]) == "-C" && call.Args[2] == word) {
			return true
		}
	}
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
	for _, arg := range call.Args[1:] {
		v := wordOwnText(arg)
		if base == "tree" && (v == "-o" || strings.HasPrefix(v, "-o")) || base == "git" && strings.HasPrefix(v, "--output") {
			return false
		}
	}
	if base == "git" && len(call.Args) > 1 {
		sub, ok := literalValue(call.Args[1])
		return ok && slices.Contains(agentSafeGitSubcommands, sub)
	}
	return slices.Contains(agentSafeCommands, base)
}

// modelClient — вызов клиента модели, в том числе за env, timeout и т. п.
func (s *agentWriteScan) modelClient(call *syntax.CallExpr) bool {
	wrapped := false
	wrapper := ""
	skip := false
	for i, arg := range call.Args {
		value, literal := literalValue(arg)
		if !literal {
			return false
		}
		if skip {
			skip = false
			continue
		}
		if wrapped && strings.HasPrefix(value, "-") {
			if wrapper == "time" && (value == "-o" || strings.HasPrefix(value, "--output") || strings.HasPrefix(value, "-o")) {
				return false
			}
			opts := map[string][]string{
				"sudo": {"-u", "-g", "-C", "-D", "-R", "-T", "-h", "-p", "-r", "-t", "--user", "--group", "--chdir", "--chroot", "--command-timeout", "--close-from", "--host", "--prompt", "--role", "--type"},
				"exec": {"-a"}, "env": {"-u", "-C", "-S", "--unset", "--chdir", "--split-string"},
				"timeout": {"-s", "-k", "--signal", "--kill-after"}, "nice": {"-n", "--adjustment"},
				"ionice": {"-c", "-n", "-p", "-P", "-u", "--class", "--classdata", "--pid", "--pgid", "--uid"},
				"stdbuf": {"-i", "-o", "-e", "--input", "--output", "--error"}, "time": {"-f", "-o", "--format", "--output"},
			}
			skip = slices.Contains(opts[wrapper], value)
			continue
		}
		if wrapped && (strings.Contains(value, "=") || strings.Trim(value, "0123456789.smhd") == "") {
			continue
		}
		base := filepath.Base(value)
		if slices.Contains(commandWrappers, base) {
			wrapped = true
			wrapper = base
			continue
		}
		if base == "npx" {
			for _, rest := range call.Args {
				if v, ok := literalValue(rest); ok && v == "@anthropic-ai/claude-code" {
					return true
				}
			}
			return false
		}
		if base == "codex" {
			for _, rest := range call.Args[i+1:] {
				v := wordOwnText(rest)
				if v == "-o" || strings.HasPrefix(v, "--output-last-message") || strings.HasPrefix(v, "-o") {
					return false
				}
			}
		}
		return slices.Contains(modelClients, base)
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
