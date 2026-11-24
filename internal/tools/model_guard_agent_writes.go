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
// (`{`, `,` — фигурные скобки bash, `=`/`:` — значение опции, кластер
// короткой опции `-C.claude`, `-tagents` — слитное значение опции)
var agentsPathToken = regexp.MustCompile(`(?i)(?:^|[/=:{,]|^-[a-z]+)agents(?:/|$|[,}])|(?:^|[/=:{,]|^-[a-z]+)\.claude/?(?:$|[,}])`)

// agentsPathInText — путь agents/ или каталог .claude внутри текста с
// пробелами (строка скрипта, тело heredoc): слово agents без косой рядом —
// проза, не путь; слева от .claude — граница пути или кластер короткой
// опции со слитным значением (`tar -C.claude`)
var agentsPathInText = regexp.MustCompile(`(?i)(?:^|[^a-z0-9_.-])agents/|/agents(?:$|[^a-z0-9_.-])|(?:^|[\s"'])-[a-z]+agents(?:/|$|[^a-z0-9_.-])|(?:^|[/~=:{,\s"']|(?:^|[\s"'])-[a-z]+)\.claude/?(?:$|[\s"'),;|&>}])`)

func mentionsAgentsPath(text string) bool {
	if strings.ContainsAny(text, " \t\n") {
		return agentsPathInText.MatchString(text)
	}
	return agentsPathToken.MatchString(text)
}

// agentReaders только читают названные пути (опции, которые пишут файл или
// запускают программу, отсекает readerOptionWrites)
var agentReaders = []string{
	"cat", "head", "tail", "wc", "grep", "egrep", "fgrep", "rg", "diff", "cmp", "stat", "file",
	"test", "[", "realpath", "readlink", "basename", "dirname", "sha256sum", "sha1sum", "md5sum",
	"du", "tree", "echo", "ls", "less", "more", "jq", "cut", "pwd",
}

// agentSafeCommands — читатели и команды, которые создают каталог или
// удаляют файл: содержимого определения агента они не задают
var agentSafeCommands = append(slices.Clone(agentReaders), "mkdir", "rm", "rmdir", "touch")

var agentReaderGitSubcommands = []string{"diff", "log", "show", "status", "ls-files", "blame"}

var agentSafeGitSubcommands = append(slices.Clone(agentReaderGitSubcommands), "add", "rm")

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
// Заведомо только читают переход (cd, pushd), клиент модели без
// --debug-file и читатель из agentReaders без пишущих и запускающих
// опций; окружение перед командой и export/declare/readonly/local
// (GIT_EXTERNAL_DIFF, GIT_CONFIG_*, GIT_PAGER, PAGER) могут запустить
// программу из «читателя», поэтому делают команду пишущей
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
		case *syntax.DeclClause:
			safe = false
		case *syntax.CallExpr:
			safe = safe && claudeDirectoryCallReads(n)
		}
		return true
	})
	return safe
}

func claudeDirectoryCallReads(call *syntax.CallExpr) bool {
	if len(call.Assigns) > 0 {
		return false
	}
	name, ok := readerProgram(call)
	if !ok {
		return false
	}
	switch name {
	case "cd", "pushd":
		return true
	case "claude", "cl":
		for _, arg := range call.Args[1:] {
			if clientOptionWrites(name, wordOwnText(arg)) {
				return false
			}
		}
		return true
	}
	return readsOnly(call, agentReaders, agentReaderGitSubcommands)
}

// readerProgram — имя программы простой команды для сверки с белыми
// списками: путь /bin и /usr/bin снят, иной путь (`./jq`, `/tmp/w/cat`)
// остаётся в имени, и такая программа ни в один список не попадает — она
// может быть чем угодно
func readerProgram(call *syntax.CallExpr) (string, bool) {
	if len(call.Args) == 0 {
		return "", false
	}
	name, literal := literalValue(call.Args[0])
	if !literal {
		return "", false
	}
	for _, dir := range []string{"/usr/bin/", "/bin/"} {
		if rest, found := strings.CutPrefix(name, dir); found {
			return rest, true
		}
	}
	return name, true
}

// readsOnly — вызов читателя из commands или git с подкомандой из
// gitSubcommands, без опций, которые пишут файл или запускают программу
func readsOnly(call *syntax.CallExpr, commands, gitSubcommands []string) bool {
	name, ok := readerProgram(call)
	if !ok {
		return false
	}
	args := call.Args[1:]
	if name == "git" {
		i := 0
		for i < len(args) {
			switch wordOwnText(args[i]) {
			case "-C":
				i += 2
				continue
			case "--no-pager", "-P":
				i++
				continue
			}
			break
		}
		if i >= len(args) {
			return false
		}
		sub, literal := literalValue(args[i])
		if !literal || !slices.Contains(gitSubcommands, sub) {
			return false
		}
	} else if !slices.Contains(commands, name) {
		return false
	}
	for _, arg := range args {
		if readerOptionWrites(name, wordOwnText(arg)) {
			return false
		}
	}
	return true
}

// readerOptionWrites — опция читателя пишет файл или запускает программу:
// --output у git и прочих, tree -o (и в кластере -fo) и -R, rg --pre,
// less -o/-O и --log-file
func readerOptionWrites(name, value string) bool {
	if strings.HasPrefix(value, "--output") {
		return true
	}
	short := strings.HasPrefix(value, "-") && !strings.HasPrefix(value, "--")
	long := strings.TrimPrefix(value, "--")
	if before, _, found := strings.Cut(long, "="); found {
		long = before
	}
	switch name {
	case "tree":
		return short && strings.ContainsAny(value, "oR") || strings.HasPrefix(value, "--o")
	case "rg":
		return strings.HasPrefix(value, "--pre")
	case "less":
		if short {
			return strings.ContainsAny(value, "oO")
		}
		return strings.HasPrefix(value, "--") && len(long) >= 3 && (strings.HasPrefix("log-file", long) || strings.HasPrefix("LOG-FILE", long))
	}
	return false
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
			if s.reason == "" {
				s.reason = wordOwnText(redirect.Word)
			}
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
	return readsOnly(call, agentSafeCommands, agentSafeGitSubcommands)
}

// modelClient — вызов клиента модели, в том числе за env, timeout и т. п.
// Значения опций обёрток (sudo -u, exec -a, env -u/-a, timeout -s, в
// кластере `-Eu`, сокращённой длинной опцией `--us`) — не имя клиента
func (s *agentWriteScan) modelClient(call *syntax.CallExpr) bool {
	for i := 0; i < len(call.Args); i++ {
		value, literal := literalValue(call.Args[i])
		if !literal {
			return false
		}
		base := filepath.Base(value)
		if slices.Contains(commandWrappers, base) {
			next, ok := wrappedCommand(base, call.Args, i+1)
			if !ok {
				return false
			}
			i = next - 1
			continue
		}
		if base == "npx" {
			for _, rest := range call.Args {
				if value, ok := literalValue(rest); ok && value == "@anthropic-ai/claude-code" {
					return true
				}
			}
			return false
		}
		for _, rest := range call.Args[i+1:] {
			if clientOptionWrites(base, wordOwnText(rest)) {
				return false
			}
		}
		return slices.Contains(modelClients, base)
	}
	return false
}

// clientOptionWrites — опция клиента пишет файл: codex -o и
// --output-last-message, claude/cl --debug-file
func clientOptionWrites(client, value string) bool {
	switch client {
	case "codex":
		return strings.HasPrefix(value, "-o") || strings.HasPrefix(value, "--output-last-message")
	case "claude", "cl":
		return strings.HasPrefix(value, "--debug-file")
	}
	return false
}

// wrapperOptions — опции обёртки: короткие со значением и без, длинные со
// значением и без (длинную опцию можно сократить до однозначного начала)
type wrapperOptions struct {
	shortValue, shortFlags string
	longValue, longFlags   []string
}

var commandWrapperOptions = map[string]wrapperOptions{
	"sudo": {
		shortValue: "aCcDghpRrTtUu", shortFlags: "AbBEeHiKklNnPSsVv",
		longValue: []string{"auth-type", "chdir", "chroot", "close-from", "command-timeout", "group", "host", "login-class", "other-user", "prompt", "role", "type", "user"},
		longFlags: []string{"askpass", "background", "bell", "edit", "help", "list", "login", "no-update", "non-interactive", "preserve-env", "preserve-groups", "remove-timestamp", "reset-timestamp", "set-home", "shell", "stdin", "validate", "version"},
	},
	"exec": {shortValue: "a", shortFlags: "cl"},
	"env": {
		shortValue: "aCPSu", shortFlags: "0iv",
		longValue: []string{"argv0", "chdir", "split-string", "unset"},
		longFlags: []string{"block-signal", "debug", "default-signal", "help", "ignore-environment", "ignore-signal", "list-signal-handling", "null", "version"},
	},
	"timeout": {
		shortValue: "ks", shortFlags: "fpv",
		longValue: []string{"kill-after", "signal"},
		longFlags: []string{"foreground", "help", "preserve-status", "verbose", "version"},
	},
	"nice":    {shortValue: "n", longValue: []string{"adjustment"}, longFlags: []string{"help", "version"}},
	"ionice":  {shortValue: "cnpPu", shortFlags: "thV", longValue: []string{"class", "classdata", "pgid", "pid", "uid"}, longFlags: []string{"help", "ignore", "version"}},
	"stdbuf":  {shortValue: "eio", longValue: []string{"error", "input", "output"}, longFlags: []string{"help", "version"}},
	"time":    {shortValue: "fo", shortFlags: "apqvV", longValue: []string{"format", "output"}, longFlags: []string{"append", "help", "portability", "quiet", "verbose", "version"}},
	"nohup":   {longFlags: []string{"help", "version"}},
	"command": {shortFlags: "pvV"},
	"setsid":  {shortFlags: "cfw", longFlags: []string{"ctty", "fork", "help", "version", "wait"}},
}

// wrappedCommand — индекс команды, которую запускает обёртка, по словам
// после неё. Неизвестная опция, нелитерал и time -o (запись в файл) — не
// установить (false)
func wrappedCommand(wrapper string, args []*syntax.Word, start int) (int, bool) {
	durationSeen := wrapper != "timeout"
	for i := start; i < len(args); i++ {
		value, literal := literalValue(args[i])
		if !literal {
			return 0, false
		}
		switch {
		case !strings.HasPrefix(value, "-") || value == "-" && wrapper != "env":
		case value == "--":
			continue
		default:
			takesValue, ok := wrapperOption(wrapper, value)
			if !ok {
				return 0, false
			}
			if takesValue {
				i++
			}
			continue
		}
		if strings.Contains(value, "=") {
			continue
		}
		if !durationSeen {
			durationSeen = true
			continue
		}
		return i, true
	}
	return len(args), true
}

// wrapperOption — слово-опция обёртки (не `--`): берёт ли она следующее
// слово значением и известна ли она. `env -` — то же, что -i; `nice -10` —
// старая запись приоритета
func wrapperOption(wrapper, value string) (bool, bool) {
	opts := commandWrapperOptions[wrapper]
	switch {
	case wrapper == "env" && value == "-":
		return false, true
	case wrapper == "nice" && strings.Trim(value[1:], "0123456789") == "":
		return false, true
	case strings.HasPrefix(value, "--"):
		return longWrapperOption(wrapper, opts, value)
	}
	return shortWrapperOptions(wrapper, opts, value[1:])
}

// longWrapperOption — длинная опция обёртки (в том числе сокращённая):
// берёт ли она следующее слово значением; неоднозначное сокращение с
// опцией со значением считается берущим значение
func longWrapperOption(wrapper string, opts wrapperOptions, value string) (bool, bool) {
	name, _, attached := strings.Cut(strings.TrimPrefix(value, "--"), "=")
	if wrapper == "time" && name != "" && strings.HasPrefix("output", name) {
		return false, false
	}
	if slices.Contains(opts.longValue, name) {
		return !attached, true
	}
	if slices.Contains(opts.longFlags, name) {
		return false, true
	}
	known := false
	for _, option := range opts.longValue {
		if strings.HasPrefix(option, name) {
			return !attached, true
		}
	}
	for _, option := range opts.longFlags {
		known = known || strings.HasPrefix(option, name)
	}
	return false, known && name != ""
}

// shortWrapperOptions — кластер коротких опций обёртки (`-Eu`, `-la`):
// берёт ли последняя буква кластера следующее слово значением; буква со
// значением в середине кластера забирает его остаток
func shortWrapperOptions(wrapper string, opts wrapperOptions, cluster string) (bool, bool) {
	for i, letter := range cluster {
		if wrapper == "time" && letter == 'o' {
			return false, false
		}
		if strings.ContainsRune(opts.shortValue, letter) {
			return i == len(cluster)-1, true
		}
		if !strings.ContainsRune(opts.shortFlags, letter) {
			return false, false
		}
	}
	return false, true
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

// reasonLimit — предел длины слова-причины в тексте отказа, в рунах
const reasonLimit = 120

// reasonExcerpt — слово-причина отказа: из текста с пробелами (тело
// bash -c, heredoc, вся команда) — первое слово с путём агентов или
// .claude, обрезанное до reasonLimit рун
func reasonExcerpt(text string) string {
	excerpt := text
	if strings.ContainsAny(text, " \t\n") {
		for field := range strings.FieldsSeq(text) {
			if agentsPathToken.MatchString(field) || agentsPathInText.MatchString(field) {
				excerpt = field
				break
			}
		}
	}
	runes := []rune(excerpt)
	if len(runes) > reasonLimit {
		return string(runes[:reasonLimit]) + "…"
	}
	return excerpt
}
