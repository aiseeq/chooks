package tools

import (
	"fmt"
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
// короткой опции `-C.claude`, `-tagents` — слитное значение опции). Слева
// от .claude — любой символ, кроме буквы, цифры и `_` (`\.claude`,
// `${X}.claude`, `$(…).claude`), кластер опции (`\-C.claude`, `-""C`, в том
// числе после подстановки: `"$Y"-C.claude`, `${Y}-C.claude`, `$(…)-C.claude`)
// или переменная `$X.claude`: граница, которую не определить по литералу, —
// путь
var agentsPathToken = regexp.MustCompile(`(?i)(?:^|[/=:{,]|^-[a-z]+)agents(?:/|$|[,}])|(?:^|[^a-z0-9_]|(?:^\\?|\$\w*)-[a-z"'\\]+|\$\w+)\.claude/?(?:$|[,}])`)

// agentsPathInText — путь agents/ внутри текста с пробелами (строка
// скрипта, тело heredoc): слово agents без косой рядом — проза, не путь.
// Каталог .claude в тексте ищет claudeDirectoryInText
var agentsPathInText = regexp.MustCompile(`(?i)(?:^|[^a-z0-9_.-])agents/|/agents(?:$|[^a-z0-9_.-])|(?:^|[\s"'])-[a-z]+agents(?:/|$|[^a-z0-9_.-])`)

// claudeDirectoryWord — .claude с правой границей слова в тексте
var claudeDirectoryWord = regexp.MustCompile(`(?i)\.claude/?(?:$|[\s"'),;|&>}])`)

func mentionsAgentsPath(text string) bool {
	if strings.ContainsAny(text, " \t\n") {
		return pathInText(text)
	}
	return agentsPathToken.MatchString(text)
}

// lineContinuation — пара `\` + перевод строки: bash её удаляет и склеивает
// слово (`-\⏎C.claude` — это `-C.claude`)
var lineContinuation = strings.NewReplacer("\\\n", "")

func pathInText(text string) bool {
	text = lineContinuation.Replace(text)
	return agentsPathInText.MatchString(text) || claudeDirectoryInText(text)
}

// claudeDirectoryInText — в тексте есть каталог .claude: .claude в начале
// слова или после символа не из [A-Za-z0-9_]. Не путь только поле или имя
// файла: .claude сразу после идентификатора (cfg.Claude, my-app.claude)
func claudeDirectoryInText(text string) bool {
	for _, match := range claudeDirectoryWord.FindAllStringIndex(text, -1) {
		if claudeDirectoryAfter(text[:match[0]]) {
			return true
		}
	}
	return false
}

// claudeDirectoryAfter — .claude после before — путь. Цепочка
// [A-Za-z0-9_.-] перед .claude делает его идентификатором, только если
// начинается буквой, цифрой или `_` и стоит не сразу за подстановкой (`$`,
// обратная кавычка, `}`, `)`, `]` арифметики `$[…]`): bash подставит туда что
// угодно, и `-` кластера опции (`-${E}C.claude`, `${D}C.claude`) по литералу
// не виден. Обратная косая и кавычки в bash исчезают и цепочку не рвут:
// `\-C.claude`, `-""C.claude`; пару `\` + перевод строки pathInText удаляет
// до обхода
func claudeDirectoryAfter(before string) bool {
	i := len(before)
	var first byte
	for i > 0 {
		c := before[i-1]
		switch {
		case c == '\\' || c == '"' || c == '\'':
		case c == '.' || c == '-' || identifierByte(c):
			first = c
		default:
			return !identifierByte(first) || strings.IndexByte("$`})]", c) >= 0
		}
		i--
	}
	return !identifierByte(first)
}

func identifierByte(c byte) bool {
	return c == '_' || '0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
}

// agentReaders — короткий список программ, которые только читают названные
// пути (опции, которые пишут файл, отсекает readerOptionWrites); только им
// можно работать в каталоге .claude после перехода в него
var agentReaders = []string{
	"cat", "head", "tail", "wc", "grep", "egrep", "fgrep", "diff", "cmp", "stat", "realpath",
	"readlink", "basename", "dirname", "sha256sum", "sha1sum", "md5sum", "du", "echo", "ls", "jq",
	"cut", "pwd",
}

// agentSafeCommands — читатели, проверки и команды, которые создают каталог
// или удаляют файл: содержимого определения агента по названному пути они не
// задают (пишущие и запускающие опции отсекает readerOptionWrites)
var agentSafeCommands = append(slices.Clone(agentReaders),
	"rg", "file", "tree", "less", "more", "test", "[", "mkdir", "rm", "rmdir", "touch")

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

// После перехода в .claude (cd, pushd, git -C) команда пропускается, только
// если в ней нет ничего, что bash раскрывает или исполняет помимо читателей:
// каждое слово — чистый литерал (literalWord), конструкции — простые вызовы,
// &&, `|` и `;`, перенаправления — чтение файла, дубль дескриптора и запись в
// /dev/null. Вызов — переход (cd, pushd), клиент модели без аргументов или
// только с `mcp list` либо читатель из agentReaders (git — с подкомандой из
// agentReaderGitSubcommands) без пишущих опций и без окружения перед
// командой. Всё прочее (export/declare, [[ ]], test, let,
// (( )), функции, подоболочки, { }, ||, &, heredoc) — как и любая неизвестная
// команда: её относительные пути и побочные эффекты по словам не установить
func readOnlyClaudeDirectory(file *syntax.File) bool {
	safe := true
	syntax.Walk(file, func(node syntax.Node) bool {
		if safe && node != nil {
			safe = claudeDirectoryNodeReads(node)
		}
		return safe
	})
	return safe
}

func claudeDirectoryNodeReads(node syntax.Node) bool {
	switch n := node.(type) {
	case *syntax.File, *syntax.Comment, *syntax.Lit, *syntax.SglQuoted, *syntax.DblQuoted:
		return true
	case *syntax.Stmt:
		return !n.Negated && !n.Background && !n.Coprocess && !clientStdinRedirected(n)
	case *syntax.BinaryCmd:
		return (n.Op == syntax.AndStmt || n.Op == syntax.Pipe) && !pipeFeedsModelClient(n)
	case *syntax.Redirect:
		return claudeDirectoryRedirectReads(n)
	case *syntax.Word:
		return literalWord(n)
	case *syntax.CallExpr:
		return claudeDirectoryCallReads(n)
	}
	return false
}

// clientStdinRedirected — у клиента модели перенаправлен ввод: дескриптор 0
// явно (0<file, 0>file) или оператором ввода без номера (<, <>, <&, heredoc,
// <<<). Через ввод приходит промпт; перенаправления stdout и stderr
// проверяет claudeDirectoryRedirectReads
func clientStdinRedirected(stmt *syntax.Stmt) bool {
	call, ok := stmt.Cmd.(*syntax.CallExpr)
	if !ok || !modelClientCall(call) {
		return false
	}
	for _, redirect := range stmt.Redirs {
		if redirect.N != nil {
			if redirect.N.Value == "0" {
				return true
			}
			continue
		}
		switch redirect.Op {
		case syntax.RdrIn, syntax.RdrInOut, syntax.DplIn, syntax.Hdoc, syntax.DashHdoc, syntax.WordHdoc:
			return true
		}
	}
	return false
}

// pipeFeedsModelClient — клиент модели справа от pipe: pipe становится его
// вводом, а с ним приходит промпт
func pipeFeedsModelClient(binary *syntax.BinaryCmd) bool {
	if binary.Op != syntax.Pipe {
		return false
	}
	stmt := binary.Y
	call, ok := stmt.Cmd.(*syntax.CallExpr)
	return ok && modelClientCall(call)
}

// modelClientCall — вызов клиента модели (claude, cl)
func modelClientCall(call *syntax.CallExpr) bool {
	name, ok := readerProgram(call)
	return ok && (name == "claude" || name == "cl")
}

// claudeDirectoryRedirectReads — перенаправление читает литеральный файл,
// дублирует дескриптор (2>&1) или пишет в /dev/null; heredoc и here-string —
// нет: их тело — ещё один текст для разбора
func claudeDirectoryRedirectReads(redirect *syntax.Redirect) bool {
	switch redirect.Op {
	case syntax.RdrIn:
		return true
	case syntax.DplIn, syntax.DplOut:
		value, literal := literalValue(redirect.Word)
		if literal && value != "" && strings.Trim(value, "0123456789-") == "" {
			return true
		}
	}
	if writingRedirect(redirect) {
		value, literal := literalValue(redirect.Word)
		return literal && value == "/dev/null"
	}
	return false
}

// literalWord — слово bash передаёт программе как есть: только литерал без
// `$`, обратной кавычки, `\`, глобов и фигурных скобок, одинарные кавычки без
// `$` и обратной кавычки внутри, двойные кавычки с таким же литералом. Тильда
// допустима только в начале слова и перед `/` (`~/.claude`)
func literalWord(word *syntax.Word) bool {
	for i, part := range word.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			value := p.Value
			if i == 0 && (value == "~" || strings.HasPrefix(value, "~/")) {
				value = value[1:]
			}
			if strings.ContainsAny(value, "$`\\*?[{}") || strings.HasPrefix(value, "~") {
				return false
			}
		case *syntax.SglQuoted:
			if p.Dollar || strings.ContainsAny(p.Value, "$`") {
				return false
			}
		case *syntax.DblQuoted:
			if p.Dollar || !quotedLiteral(p.Parts) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func quotedLiteral(parts []syntax.WordPart) bool {
	for _, part := range parts {
		lit, ok := part.(*syntax.Lit)
		if !ok || strings.ContainsAny(lit.Value, "$`\\") {
			return false
		}
	}
	return true
}

// textSubstitutes — в тексте есть `$(` или обратная кавычка: в арифметике
// и при повторном разборе (test -v 'a[$(…)]', env -S) это исполнение
func textSubstitutes(text string) bool {
	return strings.Contains(text, "$(") || strings.Contains(text, "`")
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
	case "cd", "pushd", "popd":
		return true
	case "claude", "cl":
		return clientReads(call.Args[1:])
	}
	return readsOnly(call, agentReaders, agentReaderGitSubcommands)
}

// unsafeClaudeClientOptions — блокирует настройки, не проверяемые прежним
// анализатором, и их формы в коротких кластерах. В print mode неизвестные
// флаги закрываются; model_guard сохраняет проверку настроек, агентов и
// директорий, для которых уже умеет разбирать значения. Полный CLI-парсер не
// нужен: кластер вроде `-p-settings=…` отклоняется целиком.
func unsafeClaudeClientOptions(command string) (bool, error) {
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		return false, fmt.Errorf("parse shell command for Claude client options: %w", err)
	}
	unsafe := false
	syntax.Walk(file, func(node syntax.Node) bool {
		call, ok := node.(*syntax.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		name, literal := literalValue(call.Args[0])
		if !literal || !slices.Contains([]string{"claude", "cl"}, filepath.Base(name)) {
			return true
		}
		if !clientInvocationSafe(call.Args[1:]) {
			unsafe = true
			return false
		}
		return true
	})
	return unsafe, nil
}

func clientInvocationSafe(args []*syntax.Word) bool {
	if len(args) == 0 {
		return true
	}
	if len(args) == 2 && wordOwnText(args[0]) == "mcp" && wordOwnText(args[1]) == "list" {
		return true
	}
	if containsUnsafeClaudeOption(args) {
		return false
	}
	if claudeDebugFileTargetsSettings(args) {
		return false
	}
	if clientUtilityInvocation(args) {
		return true
	}
	if hasClaudePrintMode(args) {
		return clientPrintArgumentsSafe(args)
	}
	return true
}

func hasClaudePrintMode(args []*syntax.Word) bool {
	for i := range args {
		if value := wordOwnText(args[i]); value == "-p" || value == "--print" {
			return true
		}
	}
	return false
}

func clientPrintArgumentsSafe(args []*syntax.Word) bool {
	for i := 0; i < len(args); i++ {
		if !clientPrintArgumentSafe(args, &i) {
			return false
		}
	}
	return true
}

func clientPrintArgumentSafe(args []*syntax.Word, index *int) bool {
	value := wordOwnText(args[*index])
	switch {
	case value == "-p" || value == "--print", strings.HasPrefix(value, "--model="):
		return true
	case value == "--model":
		return consumeClaudeOptionValue(args, index, true)
	case knownValidatedClaudeOption(value):
		return strings.Contains(value, "=") || consumeClaudeOptionValue(args, index, false)
	case safeClaudePrintOption(value):
		return safeClaudePrintOptionValue(args, index, value)
	case strings.HasPrefix(value, "-"):
		// Includes short-flag clusters such as -p-settings=… and
		// -c-mcp-config=…; unknown options fail closed.
		return false
	default:
		return true
	}
}

func consumeClaudeOptionValue(args []*syntax.Word, index *int, rejectOptionValue bool) bool {
	if *index+1 >= len(args) {
		return false
	}
	if rejectOptionValue && strings.HasPrefix(wordOwnText(args[*index+1]), "-") {
		return false
	}
	*index++
	return true
}

func safeClaudePrintOptionValue(args []*syntax.Word, index *int, value string) bool {
	if !claudePrintOptionTakesValue(value) || strings.Contains(value, "=") {
		return true
	}
	if *index+1 >= len(args) || strings.HasPrefix(wordOwnText(args[*index+1]), "-") {
		return claudePrintOptionHasOptionalValue(value)
	}
	*index++
	return true
}

func knownValidatedClaudeOption(value string) bool {
	name, _, _ := strings.Cut(value, "=")
	return slices.Contains([]string{
		"--settings", "--agents", "--agent", "--mcp-config", "--plugin-dir",
		"--add-dir", "--debug-file",
	}, name)
}

// safeClaudePrintOption — print-флаги, влияющие на формат/контекст запроса,
// продолжение сессии, бюджеты, системный prompt и доступные инструменты, но
// не подменяющие модель, settings, MCP или хуки. Неизвестные флаги остаются
// запрещены по умолчанию.
func safeClaudePrintOption(value string) bool {
	name, _, _ := strings.Cut(value, "=")
	return slices.Contains([]string{
		"-c", "--continue", "-r", "--resume", "--fork-session", "--from-pr",
		"--output-format", "--input-format", "--verbose", "--max-budget-usd", "--max-turns",
		"--effort", "--permission-mode", "--permission-prompts",
		"--dangerously-skip-permissions", "--allow-dangerously-skip-permissions",
		"--allowedTools", "--allowed-tools", "--disallowedTools", "--disallowed-tools",
		"--tools", "--system-prompt", "--append-system-prompt", "--json-schema",
		"--autocompact", "--name", "-n", "--session-id", "--no-session-persistence",
		"--include-hook-events", "--include-partial-messages", "--forward-subagent-text",
		"--replay-user-messages", "--disable-slash-commands", "--ax-screen-reader",
		"--exclude-dynamic-system-prompt-sections",
	}, name)
}

func claudePrintOptionTakesValue(value string) bool {
	name, _, _ := strings.Cut(value, "=")
	return slices.Contains([]string{
		"-r", "--resume", "--from-pr", "--output-format", "--input-format",
		"--max-budget-usd", "--max-turns", "--effort", "--permission-mode", "--permission-prompts",
		"--allowedTools", "--allowed-tools", "--disallowedTools", "--disallowed-tools",
		"--tools", "--system-prompt", "--append-system-prompt", "--json-schema",
		"--autocompact", "--name", "-n", "--session-id",
	}, name)
}

func claudePrintOptionHasOptionalValue(value string) bool {
	name, _, _ := strings.Cut(value, "=")
	return slices.Contains([]string{"-r", "--resume", "--from-pr"}, name)
}

func claudeDebugFileTargetsSettings(args []*syntax.Word) bool {
	for i, arg := range args {
		value := wordOwnText(arg)
		path := ""
		if after, ok := strings.CutPrefix(value, "--debug-file="); ok {
			path = after
		} else if value == "--debug-file" && i+1 < len(args) {
			path = wordOwnText(args[i+1])
		}
		if filepath.Base(path) == "settings.json" {
			return true
		}
	}
	return false
}

func clientUtilityInvocation(args []*syntax.Word) bool {
	values := make([]string, len(args))
	for i, arg := range args {
		values[i] = wordOwnText(arg)
	}
	switch {
	case len(values) == 1 && slices.Contains([]string{"--version", "--help", "-h"}, values[0]):
		return true
	case len(values) == 2 && values[0] == "auth" && values[1] == "status":
		return true
	case len(values) >= 1 && slices.Contains([]string{"--cl-ls", "--cl-check", "--cl-help"}, values[0]):
		return true
	case len(values) == 1 && values[0] == "ls":
		return true
	}
	return false
}

var unsafeClaudeOptions = []string{
	"--bare", "--setting-sources", "--plugin-url", "--managed-settings",
	"--rewind-files", "--file", "--worktree",
}

var clusteredSensitiveClaudeOptions = []string{
	"settings", "agents", "agent", "mcp-config", "plugin-dir", "debug-file",
}

var unparsedUnsafeClaudeOption = regexp.MustCompile(`(?m)(?:^|[\s;&|])--(?:bare|setting-sources|plugin-url|managed-settings|rewind-files|file|worktree|settings|agents|agent|mcp-config|plugin-dir|debug-file)(?:=|[\s]|$)|(?:^|[\s;&|])-[[:alpha:]-]*(?:settings|agents|agent|mcp-config|plugin-dir|debug-file)(?:=|[\s]|$)`)

func unparsedClaudeHasUnsafeOption(command string) bool {
	return unparsedUnsafeClaudeOption.MatchString(command)
}

func containsUnsafeClaudeOption(args []*syntax.Word) bool {
	for _, arg := range args {
		value := wordOwnText(arg)
		if !strings.HasPrefix(value, "-") {
			continue
		}
		if strings.HasPrefix(value, "--") {
			name, _, _ := strings.Cut(value, "=")
			if slices.Contains(unsafeClaudeOptions, name) {
				return true
			}
		} else {
			// В коротком кластере остаток может повторно разбираться как
			// длинная опция: -p-setting-sources, -p-settings и т. п.
			cluster := value[1:]
			for _, option := range unsafeClaudeOptions {
				if strings.Contains(cluster, strings.TrimLeft(option, "-")) {
					return true
				}
			}
			for _, option := range clusteredSensitiveClaudeOptions {
				if strings.Contains(cluster, option) {
					return true
				}
			}
		}
	}
	return false
}

// clientReads — клиент модели в каталоге .claude только читает без
// аргументов (голый `claude`) или с аргументами ровно `mcp list`. Ограничения
// на stdin проверяются по узлам команды и pipe в разобранном AST. Любой
// другой аргумент — отказ: опции пишут файлы и снимают хуки (--bare,
// --worktree, кластер -p-settings=…), а подкоманды, в том числе скрытые,
// растут с версией claude — списки их не догоняют
func clientReads(args []*syntax.Word) bool {
	if len(args) == 0 {
		return true
	}
	return len(args) == 2 && wordOwnText(args[0]) == "mcp" && wordOwnText(args[1]) == "list"
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
// less -o/-O, --log-file, -k и --lesskey* (секция #env задаёт LESSOPEN),
// file -C/--compile; у test/[ — операнд -v (test -v 'a[$(…)]' исполняет
// индекс, а `$(` можно спрятать в $'\x24(' или '$'\() и `$(` в аргументе
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
		return treeOptionWrites(value, short)
	case "rg":
		return strings.HasPrefix(value, "--pre")
	case "less":
		return lessOptionWrites(value, short, long)
	case "file":
		return fileOptionWrites(value, short, long)
	case "test", "[":
		return testOptionWrites(value)
	}
	return false
}

func treeOptionWrites(value string, short bool) bool {
	return short && strings.ContainsAny(value, "oR") || strings.HasPrefix(value, "--o")
}

func lessOptionWrites(value string, short bool, long string) bool {
	if short {
		return strings.ContainsAny(value, "oOk")
	}
	return strings.HasPrefix(value, "--") && len(long) >= 3 && lessLongOptionWrites(long)
}

func lessLongOptionWrites(long string) bool {
	return longOptionIs(long, "log-file") || longOptionIs(long, "LOG-FILE") || longOptionIs(long, "lesskey")
}

func fileOptionWrites(value string, short bool, long string) bool {
	return short && strings.Contains(value, "C") || strings.HasPrefix(value, "--") && len(long) >= 3 && longOptionIs(long, "compile")
}

func testOptionWrites(value string) bool {
	return value == "-v" || textSubstitutes(value)
}

// longOptionIs — имя длинной опции (без --) — сама option, её сокращение
// или более длинное имя того же семейства (--lesskey-src при option lesskey)
func longOptionIs(name, option string) bool {
	return strings.HasPrefix(option, name) || strings.HasPrefix(name, option)
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
	case *syntax.UnaryTest:
		return owner.Op != syntax.TsVarSet && !textSubstitutes(wordOwnText(word))
	case *syntax.TestClause, *syntax.BinaryTest, *syntax.ParenTest:
		return !textSubstitutes(wordOwnText(word))
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
		return !clientOptionValue(call, word)
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
			next, ok := npxClaudePackage(call.Args, i+1)
			if !ok {
				return false
			}
			i, base = next, "claude"
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

// npxClaudePackage — индекс пакета claude-code в `npx [-y|--yes] [--]
// @anthropic-ai/claude-code …`; иные опции npx (-p/--package, -c/--call)
// запускают другую команду — не клиент
func npxClaudePackage(args []*syntax.Word, start int) (int, bool) {
	for i := start; i < len(args); i++ {
		value, literal := literalValue(args[i])
		switch {
		case !literal:
			return 0, false
		case value == "-y" || value == "--yes" || value == "--":
		case value == "@anthropic-ai/claude-code":
			return i, true
		default:
			return 0, false
		}
	}
	return 0, false
}

// clientOptionRuns — опция claude/cl задаёт команды, которые клиент
// запустит: hooks в --settings, серверы в --mcp-config, плагины в
// --plugin-dir
func clientOptionRuns(value string) bool {
	for _, option := range []string{"--settings", "--mcp-config", "--plugin-dir"} {
		if value == option || strings.HasPrefix(value, option+"=") {
			return true
		}
	}
	return false
}

// clientOptionValue — слово — значение опции клиента из clientOptionRuns
// (у --mcp-config значений несколько: до следующей опции)
func clientOptionValue(call *syntax.CallExpr, word *syntax.Word) bool {
	if clientOptionRuns(wordOwnText(word)) {
		return true
	}
	values, variadic := 0, false
	for _, arg := range call.Args {
		value := wordOwnText(arg)
		switch {
		case clientOptionRuns(value):
			values, variadic = 1, value == "--mcp-config"
			continue
		case strings.HasPrefix(value, "-"):
			values, variadic = 0, false
			continue
		}
		if arg == word && (values > 0 || variadic) {
			return true
		}
		values--
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
// значением и без (длинную опцию можно сократить до однозначного начала);
// refuse — опции, значение которых исполняется или куда пишется вывод
// (env -S, time -o): за ними команду не установить
type wrapperOptions struct {
	shortValue, shortFlags, shortRefuse string
	longValue, longFlags, longRefuse    []string
}

var commandWrapperOptions = map[string]wrapperOptions{
	"sudo": {
		shortValue: "aCcDghpRrTtUu", shortFlags: "AbBEeHiKklNnPSsVv",
		longValue: []string{"auth-type", "chdir", "chroot", "close-from", "command-timeout", "group", "host", "login-class", "other-user", "prompt", "role", "type", "user"},
		longFlags: []string{"askpass", "background", "bell", "edit", "help", "list", "login", "no-update", "non-interactive", "preserve-env", "preserve-groups", "remove-timestamp", "reset-timestamp", "set-home", "shell", "stdin", "validate", "version"},
	},
	"exec": {shortValue: "a", shortFlags: "cl"},
	"env": {
		shortValue: "aCPu", shortFlags: "0iv", shortRefuse: "S",
		longValue: []string{"argv0", "chdir", "unset"}, longRefuse: []string{"split-string"},
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
	"time":    {shortValue: "f", shortFlags: "apqvV", shortRefuse: "o", longValue: []string{"format"}, longFlags: []string{"append", "help", "portability", "quiet", "verbose", "version"}, longRefuse: []string{"output"}},
	"nohup":   {longFlags: []string{"help", "version"}},
	"command": {shortFlags: "pvV"},
	"setsid":  {shortFlags: "cfw", longFlags: []string{"ctty", "fork", "help", "version", "wait"}},
}

// wrappedCommand — индекс команды, которую запускает обёртка, по словам
// после неё. Неизвестная опция, опция из refuse и нелитерал — не
// установить (false). Слово с `=` — присваивание только у env и sudo, у
// прочих обёрток это программа (`nohup /tmp/a=b/../usr/bin/tee`)
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
		if strings.Contains(value, "=") && (wrapper == "env" || wrapper == "sudo") {
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
		return longWrapperOption(opts, value)
	}
	return shortWrapperOptions(opts, value[1:])
}

// longWrapperOption — длинная опция обёртки (в том числе сокращённая):
// берёт ли она следующее слово значением; неоднозначное сокращение с
// опцией со значением считается берущим значение
func longWrapperOption(opts wrapperOptions, value string) (bool, bool) {
	name, _, attached := strings.Cut(strings.TrimPrefix(value, "--"), "=")
	for _, option := range opts.longRefuse {
		if strings.HasPrefix(option, name) {
			return false, false
		}
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
func shortWrapperOptions(opts wrapperOptions, cluster string) (bool, bool) {
	for i, letter := range cluster {
		if strings.ContainsRune(opts.shortRefuse, letter) {
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
			if agentsPathToken.MatchString(field) || pathInText(field) {
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
