package tools

import (
	"fmt"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/aiseeq/akit/shellcmd"
	"github.com/aiseeq/chooks/internal/core"
	"github.com/aiseeq/chooks/internal/hookctx"
	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/syntax"
)

// Опции клиента claude судятся по вызовам, которые нашёл разбор с обёртками
// (modelCallCandidates: env, timeout, bash -c, npx @anthropic-ai/claude-code,
// command, exec), с -p и без: из Bash-инструмента stdout не терминал, и
// claude без -p тоже работает без диалога. Набор опций — белый список
// claudeOptions: опция вне него (скрытая, новая в следующей версии, снимающая
// хуки или подменяющая настройки) — отказ с её именем. Слова читаются по
// правилам commander, которым claude разбирает argv: кластер коротких флагов
// (-p-settings=x — это -p и --settings=x), значение опции берёт следующее
// слово целиком (--append-system-prompt '--bare'), после «--» — только
// промпт, --help последним словом — справка без запуска. Слово, которое
// подстановка может разбить на несколько или которое начинается подстановкой
// с неизвестным значением, — отказ: за ним может стоять опция. Проверка
// запуска (modellaunch) этих отказов не делает: она судит модель по argv
// процесса, а не команду агента

// claudeArity — сколько слов берёт опция claude
type claudeArity int

const (
	claudeFlag claudeArity = iota
	claudeValue
	claudeOptionalValue
	// claudeValues — первое слово всегда, дальше — слова до следующей опции
	claudeValues
)

// claudeOptionKind — что model_guard делает со значением опции
type claudeOptionKind int

const (
	claudeSafe claudeOptionKind = iota
	// claudeJudged — значение судит candidatesFromArgs: модель, агенты,
	// каталоги агентов
	claudeJudged
	// claudeRefusedJudged — отказ в PreToolUse, но модель из значения судится
	// и в проверке запуска: --settings, --plugin-dir
	claudeRefusedJudged
	// claudeDebugFile — путь, куда claude пишет журнал отладки
	claudeDebugFile
)

type claudeOption struct {
	arity claudeArity
	kind  claudeOptionKind
}

// claudeOptions — опции claude 2.1.332 (claude --help; скрытые --thinking,
// --max-thinking-tokens, --system-prompt-file, --append-system-prompt-file,
// --permission-prompt-tool, --max-turns, --advisor — по коду бинаря), которые
// не подменяют настройки, MCP, плагины и хуки. Модель (--model,
// --fallback-model, --advisor), агенты и каталоги агентов судит
// candidatesFromArgs, путь --debug-file — debugFileRefusal
var claudeOptions = map[string]claudeOption{
	"--print": {}, "--continue": {}, "--help": {}, "--version": {}, "--verbose": {},
	"--allow-dangerously-skip-permissions": {}, "--dangerously-skip-permissions": {},
	"--ax-screen-reader": {}, "--brief": {}, "--chrome": {}, "--no-chrome": {}, "--ide": {},
	"--disable-slash-commands": {}, "--exclude-dynamic-system-prompt-sections": {},
	"--fork-session": {}, "--forward-subagent-text": {}, "--include-hook-events": {},
	"--include-partial-messages": {}, "--no-session-persistence": {}, "--replay-user-messages": {},
	"--strict-mcp-config": {}, "--bg": {}, "--background": {},

	"--allowedTools": {arity: claudeValues}, "--allowed-tools": {arity: claudeValues},
	"--disallowedTools": {arity: claudeValues}, "--disallowed-tools": {arity: claudeValues},
	"--tools": {arity: claudeValues}, "--betas": {arity: claudeValues},
	"--append-system-prompt": {arity: claudeValue}, "--append-system-prompt-file": {arity: claudeValue},
	"--system-prompt": {arity: claudeValue}, "--system-prompt-file": {arity: claudeValue},
	"--system-prompt-snapshot": {arity: claudeValue}, "--autocompact": {arity: claudeValue},
	"--effort": {arity: claudeValue}, "--input-format": {arity: claudeValue},
	"--output-format": {arity: claudeValue}, "--json-schema": {arity: claudeValue},
	"--max-budget-usd": {arity: claudeValue}, "--max-turns": {arity: claudeValue},
	"--name": {arity: claudeValue}, "--permission-mode": {arity: claudeValue},
	"--permission-prompts": {arity: claudeValue}, "--permission-prompt-tool": {arity: claudeValue},
	"--session-id": {arity: claudeValue}, "--thinking": {arity: claudeValue},
	"--max-thinking-tokens": {arity: claudeValue},

	"--resume": {arity: claudeOptionalValue}, "--from-pr": {arity: claudeOptionalValue},
	"--debug": {arity: claudeOptionalValue}, "--prompt-suggestions": {arity: claudeOptionalValue},

	"--model": {arity: claudeValue, kind: claudeJudged}, "--fallback-model": {arity: claudeValue, kind: claudeJudged},
	"--advisor": {arity: claudeValue, kind: claudeJudged}, "--agent": {arity: claudeValue, kind: claudeJudged},
	"--agents": {arity: claudeValue, kind: claudeJudged}, "--add-dir": {arity: claudeValues, kind: claudeJudged},
	"--settings":   {arity: claudeValue, kind: claudeRefusedJudged},
	"--plugin-dir": {arity: claudeValue, kind: claudeRefusedJudged},
	"--debug-file": {arity: claudeValue, kind: claudeDebugFile},
}

// claudeRefusedOptions — что делают опции вне claudeOptions, которыми claude
// снимает хуки или подменяет настройки, MCP и плагины (claude --help
// 2.1.332): отказ называет это, прочим опциям вне набора — «не в списке
// известных»
var claudeRefusedOptions = map[string]string{
	"--bare":            "снимает хуки и плагины (минимальный режим, CLAUDE_CODE_SIMPLE)",
	"--safe-mode":       "снимает хуки, плагины, MCP-серверы, агенты и CLAUDE.md",
	"--restricted":      "не читает файлы настроек: хуки из ~/.claude/settings.json не работают",
	"--settings":        "берёт настройки (хуки, модель, env) из файла или строки JSON",
	"--setting-sources": "выбирает, какие файлы настроек читать: без user хуки из ~/.claude не работают",
	"--mcp-config":      "добавляет MCP-серверы",
	"--plugin-dir":      "добавляет плагин с его хуками и агентами",
	"--plugin-url":      "скачивает и добавляет плагин с его хуками и агентами",
}

// claudeHookWords — слова, которые claude ищет в argv сам, помимо commander:
// --bare и --safe-mode до первого «--» (по коду бинаря 2.1.332: includes,
// ZQt), --restricted — в replConfigArgv, где граница «--» по коду не видна.
// Значение опции, равное такому слову, тоже снимает хуки
var claudeHookWords = []string{"--bare", "--safe-mode", "--restricted"}

// claudeShortOptions — короткие опции из claudeOptions; -w (--worktree) в
// набор не входит
var claudeShortOptions = map[byte]string{
	'c': "--continue", 'd': "--debug", 'h': "--help", 'n': "--name", 'p': "--print", 'r': "--resume", 'v': "--version",
}

// claudeCommands — подкоманды claude (claude --help и .command в коде
// бинаря): первое слово-операнд с таким именем запускает подкоманду, а не
// промпт
var claudeCommands = []string{
	"agents", "attach", "auth", "auto-mode", "config", "doctor", "gateway", "import", "install", "kill",
	"logs", "mcp", "plugin", "plugins", "project", "purge", "remote-control", "respawn", "rm", "sandbox",
	"serve", "setup", "setup-token", "stop", "ultrareview", "update", "upgrade", "worktree-hook", "xaa",
}

// claudeReadingCommands — подкоманды, которые только читают; "" — любое слово
// (имя сервера, сессии). Остальные добавляют MCP-серверы и плагины, меняют
// настройки, вход или сам клиент либо не проверены
var claudeReadingCommands = [][]string{
	{"mcp", "list"}, {"mcp", "get", ""}, {"auth", "status"}, {"plugin", "list"}, {"plugins", "list"},
	{"agents"}, {"logs", ""}, {"doctor"},
}

// claudeCommandOptions — опции подкоманды, которые не подменяют модель,
// настройки, MCP и плагины: claude agents --json [--all] [--cwd <путь>]
// печатает список сессий; прочие опции agents (--model, --mcp-config,
// --settings, --plugin-dir) судятся как опции claude
var claudeCommandOptions = map[string]map[string]claudeOption{
	"agents": {"--json": {}, "--all": {}, "--cwd": {arity: claudeValue}},
}

// claudeEnvironmentName — переменная claude (CLAUDE_*, ANTHROPIC_*) в тексте
// команды. Такие переменные у вызова claude судятся белым списком: claude
// 2.1.332 читает их сотни, и новая в следующей версии может снять хуки
var claudeEnvironmentName = regexp.MustCompile(`\b(?:CLAUDE|ANTHROPIC)_[A-Za-z0-9_]+`)

// environmentHazard inspects the complete command when it invokes a client.
func environmentHazard(command string, client bool) (bool, error) {
	// tmuxClientWord видит и claude-code: npx @anthropic-ai/claude-code
	if !tmuxClientWord.MatchString(command) {
		return false, nil
	}
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	// error-string-compare: safe — the parser exposes no sentinel for an unclosed here-document.
	if err != nil && strings.Contains(err.Error(), "unclosed here-document") {
		if before, _, ok := strings.Cut(command, "<<"); ok {
			file, err = syntax.NewParser().Parse(strings.NewReader(before), "")
		}
	}
	if err != nil {
		return false, fmt.Errorf("parse environment: %w", err)
	}
	if !client && !wrapperHasClient(file, command) {
		return false, nil
	}
	hazard := false
	syntax.Walk(file, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.DeclClause:
			hazard = hazard || dynamicDeclaration(command, n)
		case *syntax.CallExpr:
			hazard = hazard || dynamicEnvironmentCall(command, n)
		}
		return true
	})
	return hazard, nil
}

func wrapperHasClient(file *syntax.File, command string) bool {
	found := false
	syntax.Walk(file, func(node syntax.Node) bool {
		call, ok := node.(*syntax.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		args, env := shellNestedCommandArgs(command, call, "env")
		if !env {
			return true
		}
		for _, arg := range args {
			value, literal := literalValue(arg)
			if literal && claudeClientWord.MatchString(value) {
				found = true
			}
		}
		return true
	})
	return found
}

func dynamicDeclaration(command string, decl *syntax.DeclClause) bool {
	for _, arg := range decl.Args {
		start, end := int(arg.Pos().Offset()), int(arg.End().Offset())
		if start < 0 || end > len(command) || start > end {
			continue
		}
		raw := command[start:end]
		name, _, _ := strings.Cut(raw, "=")
		if strings.HasPrefix(raw, "-n") || arg.Name == nil && strings.ContainsAny(name, "$`{") {
			return true
		}
	}
	return false
}

func dynamicEnvironmentCall(command string, call *syntax.CallExpr) bool {
	if len(call.Args) == 0 {
		return false
	}
	if args, ok := shellNestedCommandArgs(command, call, "env"); ok && envWordsHazard(args) {
		return true
	}
	if args, ok := shellWrappedEnvArgs(command, call); ok && envWordsHazard(args) {
		return true
	}
	name, literal := shellEffectiveCommand(call.Args)
	if !literal {
		return false
	}
	for i, arg := range call.Args[1:] {
		_, literal := literalValue(arg)
		switch name {
		case "printf":
			if i > 0 && !literal {
				prev, _ := literalValue(call.Args[i])
				if prev == "-v" {
					return true
				}
			}
		default:
			if shellVariableWriter(name) && !literal && !literalAssignmentName(arg) {
				return true
			}
		}
	}
	return false
}

// A computed value after a literal NAME= does not compute the target name.
func literalAssignmentName(word *syntax.Word) bool {
	if len(word.Parts) == 0 {
		return false
	}
	part, ok := word.Parts[0].(*syntax.Lit)
	if !ok {
		return false
	}
	name, _, assigned := strings.Cut(part.Value, "=")
	return assigned && syntax.ValidName(name)
}

// shellWrappedEnvArgs — слова после env, который shellcmd снял обёрткой
// вызова (timeout 9 env -u HOME claude: вызов — claude, env — в Via)
func shellWrappedEnvArgs(source string, call *syntax.CallExpr) ([]*syntax.Word, bool) {
	start, end := int(call.Pos().Offset()), int(call.End().Offset())
	if start < 0 || end > len(source) || start > end {
		return nil, false
	}
	calls, err := shellcmd.Calls(source[start:end])
	if err != nil {
		// error-masked-as-false-bool: safe — неразобранный вызов отказывает основной разбор
		return nil, false
	}
	wrapped := false
	for i := range calls {
		wrapped = wrapped || slices.Contains(calls[i].Via, "env")
	}
	if !wrapped {
		return nil, false
	}
	for i, arg := range call.Args {
		if value, literal := literalValue(arg); literal && value == "env" {
			return call.Args[i+1:], true
		}
	}
	return nil, false
}

// shellNestedCommandArgs locates the unwrapped program using shellcmd's own
// wrapper parser and maps its arguments back to their syntax nodes.
func shellNestedCommandArgs(source string, call *syntax.CallExpr, name string) ([]*syntax.Word, bool) {
	start, end := int(call.Pos().Offset()), int(call.End().Offset())
	if start < 0 || end > len(source) || start > end {
		return nil, false
	}
	calls, err := shellcmd.Calls(source[start:end])
	if err != nil {
		// error-masked-as-false-bool: safe — failure leaves the wrapper unresolved for the main parser to refuse.
		return nil, false
	}
	for i := range calls {
		if calls[i].Name != name || calls[i].Offset < 0 {
			continue
		}
		for j, arg := range call.Args {
			if int(arg.Pos().Offset()) == start+calls[i].Offset {
				return call.Args[j+1:], true
			}
		}
	}
	return nil, false
}

func unsafeEnvironmentName(name string) bool {
	if name == "CLAUDE_CODE_EFFORT_LEVEL" {
		return false
	}
	if name == "HOME" || name == "PATH" || name == "BASH_ENV" || name == "ENV" || name == "BUN_OPTIONS" || name == "NODE_OPTIONS" || strings.HasPrefix(name, "LD_") {
		return true
	}
	_, model := modelEnvNames[name]
	return model || claudeEnvironmentName.MatchString(name) && !slices.Contains(claudeEnvironmentAllowed, name)
}

func envWordsHazard(args []*syntax.Word) bool {
	values := make([]envArgument, len(args))
	for i, arg := range args {
		value, literal := literalValue(arg)
		values[i] = envArgument{value, literal}
	}
	return envArgumentsHazard(values, false)
}

type envArgument struct {
	value   string
	literal bool
}

// cyclomatic-complexity: safe — env options, assignments and command are distinct shell boundaries.
func envArgumentsHazard(args []envArgument, checkAssignments bool) bool {
	for i := 0; i < len(args); i++ {
		value := args[i].value
		if !args[i].literal {
			return true
		}
		if value == "-S" || value == "--split-string" {
			if i+1 >= len(args) || !args[i+1].literal {
				return true
			}
			return envSplitHazard(args[i+1].value)
		}
		if strings.HasPrefix(value, "-S") {
			return envSplitHazard(value[2:])
		}
		if split, ok := strings.CutPrefix(value, "--split-string="); ok {
			return envSplitHazard(split)
		}
		if name, ok := strings.CutPrefix(value, "--unset="); ok {
			if name == "" || name == "HOME" {
				return true
			}
			continue
		}
		if name, ok := strings.CutPrefix(value, "-u"); ok && name != "" {
			if name == "HOME" {
				return true
			}
			continue
		}
		if value == "-u" || value == "--unset" || value == "-C" || value == "--chdir" {
			i++
			if i >= len(args) || !args[i].literal || (value == "-u" || value == "--unset") && args[i].value == "HOME" {
				return true
			}
			continue
		}
		if strings.HasPrefix(value, "-") {
			continue
		}
		if name, _, assigned := strings.Cut(value, "="); assigned {
			if checkAssignments && unsafeEnvironmentName(name) {
				return true
			}
			continue
		}
		return false
	}
	return false
}

// env -S accepts its own quoting and escapes. Only a fully literal split
// whose environment and client are visible is safe to hand to shellcmd.
func envSplitHazard(text string) bool {
	var words []*syntax.Word
	for word, err := range syntax.NewParser().WordsSeq(strings.NewReader(text)) {
		// error-masking: safe — a split string that cannot be parsed is refused.
		if err != nil {
			return true
		}
		words = append(words, word)
	}
	if len(words) == 0 {
		return true
	}
	values := make([]envArgument, len(words))
	for i, word := range words {
		value, literal := literalValue(word)
		values[i] = envArgument{value, literal}
	}
	if envArgumentsHazard(values, true) {
		return true
	}
	commandSeen := false
	for _, arg := range values {
		value := arg.value
		if !arg.literal {
			return true
		}
		if commandSeen {
			continue
		}
		if _, _, assigned := strings.Cut(value, "="); assigned {
			continue
		}
		if value != "claude" && value != "cl" {
			return true
		}
		commandSeen = true
	}
	return !commandSeen
}

// claudeEnvironmentAllowed — переменные claude, которые не снимают хуки и не
// подменяют настройки, MCP и плагины. Модель (modelEnvNames) и
// CLAUDE_CODE_SHELL_PREFIX model_guard судит отдельно
var claudeEnvironmentAllowed = []string{
	"CLAUDE_CODE_SHELL_PREFIX", "CLAUDE_CODE_SUBAGENT_MODEL_FORCE", "CLAUDE_CODE_ADDITIONAL_DIRECTORIES_CLAUDE_MD",
	"CLAUDE_CODE_DISABLE_FEEDBACK_SURVEY", "CLAUDE_AX_SCREEN_READER", "CLAUDE_AX_STARTUP_QUIET_MS",
}

// claudeEnvironmentEffects — что делают переменные, которыми claude снимает
// хуки или берёт настройки не из ~/.claude: SIMPLE, SAFE_MODE, RESTRICTED —
// по коду бинаря 2.1.332, прочее — по его тексту (не исполнено). Отказ
// называет это, прочим вне claudeEnvironmentAllowed — «не в списке
// известных». HOME судится с ними: claude берёт настройки из $HOME/.claude
var claudeEnvironmentEffects = map[string]string{
	"CLAUDE_CODE_SIMPLE":                   "то же, что --bare: снимает хуки и плагины",
	"CLAUDE_CODE_SAFE_MODE":                "то же, что --safe-mode: снимает хуки, плагины и MCP-серверы",
	"CLAUDE_CODE_RESTRICTED":               "то же, что --restricted: не читает файлы настроек, хуки не работают",
	"CLAUDE_CONFIG_DIR":                    "берёт настройки и хуки не из ~/.claude",
	"HOME":                                 "берёт настройки и хуки из .claude другого каталога",
	"CLAUDE_CODE_PLUGIN_DIRS":              "добавляет каталоги плагинов с их хуками",
	"CLAUDE_CODE_RESTRICT_PERSONAL_CONFIG": "не исполняет личные хуки",
	"CLAUDE_CODE_MANAGED_SETTINGS_PATH":    "берёт управляемые настройки (и их хуки) из другого каталога",
}

// argumentFeeders — обёртки, которые дописывают клиенту слова из ввода: их
// опции по команде не видны
var argumentFeeders = []string{"xargs", "parallel"}

// claudeArg — слово аргумента claude: text — его значение после кавычек,
// подстановка заменена известным значением переменной или `$`; splits —
// подстановка без кавычек, которую bash может разбить на несколько слов;
// unresolved — в text осталась неизвестная подстановка; mayBeOption — слово
// начинается подстановкой, значение которой может начаться с `-`: commander
// примет его за опцию; hookWord — слово получило отказ как возможное
// claudeHookWords; pattern — регулярное выражение значений слова с
// подстановкой: литерал, `.*` за неизвестной частью, перечень значений
// переменной цикла
type claudeArg struct {
	word        word
	text        string
	pattern     string
	dynamic     bool
	splits      bool
	unresolved  bool
	mayBeOption bool
	hookWord    bool
	err         error
}

// claudeArguments — разбор слов claude: judged — пары «опция значение» для
// candidatesFromArgs, refusals — отказы с именем опции
//
// help — последнее слово -h или --help в месте опции: claude печатает справку
// и ничего не запускает; unknownLast — прошлое слово — опция вне набора или
// подстановка, они могут взять --help значением
type claudeArguments struct {
	judged      []word
	operands    []claudeArg
	debugFiles  []claudeArg
	agents      []claudeArg
	refusals    []string
	help        bool
	unknownLast bool
}

func clientOptionCandidate(reason string) modelCandidate {
	return modelCandidate{value: reason, clientOption: reason}
}

// claudeCallCandidates — отказы по словам вызова claude и пары опций для
// candidatesFromArgs
func claudeCallCandidates(call *shellcmd.Call, args []word, files modelFiles, cwd, home string) ([]modelCandidate, []word) {
	if call.Name == "cl" && len(args) > 0 && strings.HasPrefix(args[0].text, "--cl-") {
		return nil, nil
	}
	parsed := scanClaudeArguments(args, files.envValues)
	if parsed.help {
		// справка: claude печатает её до разбора остального и ничего не
		// запускает; модель, ввод обёртки и путь журнала судятся и тут
		parsed.refusals = nil
	} else {
		parsed.refuseCommand()
	}
	for _, feeder := range argumentFeeders {
		if slices.Contains(call.Via, feeder) {
			parsed.refuse(fmt.Sprintf("слова клиента claude дописывает %s из ввода: его опции не проверить", feeder))
		}
	}
	for _, path := range parsed.debugFiles {
		if reason := debugFileRefusal(path, files.tempDirs, cwd, home); reason != "" {
			parsed.refuse(reason)
		}
	}
	for _, definition := range parsed.agents {
		if reason := agentsRefusal(definition, files.settings, cwd); reason != "" {
			parsed.refuse(reason)
		}
	}
	found := make([]modelCandidate, 0, len(parsed.refusals))
	for _, reason := range parsed.refusals {
		found = append(found, clientOptionCandidate(reason))
	}
	return found, parsed.judged
}

func scanClaudeArguments(args []word, values func(string) ([]string, bool)) claudeArguments {
	var parsed claudeArguments
	queue := make([]claudeArg, len(args))
	for i, arg := range args {
		queue[i] = newClaudeArg(arg, values)
	}
	parsed.refuseHookWords(queue)
	variadic := ""
	for len(queue) > 0 {
		arg := queue[0]
		queue = queue[1:]
		unknownBefore := parsed.unknownLast
		parsed.unknownLast = false
		// слово, которое не разобрано, тоже splits
		if arg.splits {
			parsed.refuse(splitsRefusal(arg))
			parsed.unknownLast = true
			continue
		}
		if variadic != "" && !claudeMaybeOption(arg.text) && !arg.mayBeOption {
			parsed.value(variadic, arg)
			continue
		}
		variadic = ""
		switch {
		case arg.text == "--":
			parsed.operands = append(parsed.operands, queue...)
			queue = nil
		case arg.mayBeOption:
			if !arg.hookWord {
				parsed.refuse(fmt.Sprintf("слово %s начинается подстановкой: её значение с - claude примет за опцию; значение опции пиши --opt=\"$VALUE\", промпт — после --", arg.word.text))
			}
			parsed.unknownLast = true
		case !claudeMaybeOption(arg.text) || !claudeOptionStart(arg.text[1]):
			parsed.operands = append(parsed.operands, arg)
		default:
			parsed.help = parsed.help || (arg.text == "--help" || arg.text == "-h") && len(queue) == 0 && !unknownBefore
			queue, variadic = parsed.option(arg, queue)
		}
	}
	return parsed
}

// refuseHookWords — слово argv, которое есть или может оказаться (подстановка
// с неизвестным значением) одним из claudeHookWords: claude ищет его сам, и
// в месте значения опции тоже. --bare и --safe-mode — до первого «--»
func (p *claudeArguments) refuseHookWords(args []claudeArg) {
	beforeDashDash := true
	for i := range args {
		arg := &args[i]
		for _, name := range claudeHookWords {
			// после «--» — только --restricted литералом: промпт из
			// подстановки там — исход отказа «пиши промпт после --»
			if !beforeDashDash && (name != "--restricted" || arg.unresolved) || !arg.couldBe(name) {
				continue
			}
			arg.hookWord = true
			if arg.unresolved {
				p.refuse(fmt.Sprintf("слово %s может оказаться %s: claude ищет это слово в argv сам, и в месте значения опции, и %s; значение опции пиши --opt=\"$VALUE\", промпт — после --", arg.word.text, name, claudeRefusedOptions[name]))
				continue
			}
			// тот же текст, что у опции: слово в месте опции даёт один отказ
			p.refuse(refusedOption(name))
		}
		beforeDashDash = beforeDashDash && arg.text != "--"
	}
}

// couldBe — значение слова равно text или может ему равняться (pattern)
func (a claudeArg) couldBe(text string) bool {
	if !a.unresolved {
		return a.text == text
	}
	return regexp.MustCompile(`(?s)^(?:` + a.pattern + `)$`).MatchString(text)
}

// write — известная часть слова
func (a *claudeArg) write(b *strings.Builder, text string) {
	b.WriteString(text)
	a.pattern += regexp.QuoteMeta(text)
}

// splitsRefusal — слово, которое bash может разбить на несколько
func splitsRefusal(arg claudeArg) string {
	if arg.err != nil {
		return fmt.Sprintf("слово %s не разобрано (%v): опции клиента claude не проверить", arg.word.text, arg.err)
	}
	return fmt.Sprintf("слово %s без кавычек bash может разбить на несколько слов (подстановка, фигурные скобки или маска) либо убрать пустое слово: за ними может стоять опция claude; возьми слово в кавычки (например --opt=\"$VALUE\") или убери пустую подстановку", arg.word.text)
}

// lookup — опция подкоманды (claudeCommandOptions) или claude
func (p *claudeArguments) lookup(name string) (claudeOption, bool) {
	if len(p.operands) > 0 {
		if spec, ok := claudeCommandOptions[p.operands[0].text][name]; ok {
			return spec, true
		}
	}
	spec, ok := claudeOptions[name]
	return spec, ok
}

// claudeMaybeOption — слово commander разбирает как опцию
func claudeMaybeOption(text string) bool {
	return len(text) > 1 && text[0] == '-'
}

// claudeOptionStart — после `-` идёт имя опции: `-` длинной или буква,
// цифра короткой. Слово вроде «- пункт» опцией claude быть не может: такой
// опции нет, и claude откажет сам
func claudeOptionStart(c byte) bool {
	return c == '-' || identifierByte(c) && c != '_'
}

func (p *claudeArguments) refuse(reason string) {
	if !slices.Contains(p.refusals, reason) {
		p.refusals = append(p.refusals, reason)
	}
}

func refusedOption(name string) string {
	if effect, ok := claudeRefusedOptions[name]; ok {
		return fmt.Sprintf("опция %s клиента claude %s", name, effect)
	}
	return fmt.Sprintf("опция %s клиента claude не в списке известных: в нём только опции, которые не подменяют модель, настройки, MCP, плагины и хуки", name)
}

// option — опция в arg; возвращает остаток слов и имя опции, которая берёт
// следующие слова до очередной опции
func (p *claudeArguments) option(arg claudeArg, queue []claudeArg) ([]claudeArg, string) {
	text := arg.text
	if strings.HasPrefix(text, "--") {
		name, value, attached := strings.Cut(text, "=")
		spec, known := p.lookup(name)
		if !known || attached && spec.arity == claudeFlag {
			p.refuse(refusedOption(name))
			p.unknownLast = true
			return queue, ""
		}
		return p.apply(name, spec, arg, value, attached, queue)
	}
	name, known := claudeShortOptions[text[1]]
	if !known {
		p.refuse(refusedOption("-" + string(text[1])))
		p.unknownLast = true
		return queue, ""
	}
	spec := claudeOptions[name]
	rest := text[2:]
	if rest == "" || spec.arity != claudeFlag {
		return p.apply(name, spec, arg, rest, rest != "", queue)
	}
	// остаток кластера commander разбирает заново: -p-settings=x — это -p
	// и --settings=x
	p.apply(name, spec, arg, "", false, nil)
	next := arg
	next.text = "-" + rest
	return append([]claudeArg{next}, queue...), ""
}

func (p *claudeArguments) apply(name string, spec claudeOption, arg claudeArg, attachedValue string, attached bool, queue []claudeArg) ([]claudeArg, string) {
	if spec.kind == claudeRefusedJudged {
		p.refuse(refusedOption(name))
	}
	switch {
	case attached:
		p.value(name, attachedArg(arg, attachedValue))
	case spec.arity == claudeValue || spec.arity == claudeValues:
		if len(queue) == 0 {
			return queue, ""
		}
		if queue[0].splits {
			p.refuse(splitsRefusal(queue[0]))
		}
		p.value(name, queue[0])
		queue = queue[1:]
	case spec.arity == claudeOptionalValue && len(queue) > 0 && !claudeMaybeOption(queue[0].text) && !queue[0].splits && !queue[0].mayBeOption:
		queue = queue[1:]
	}
	if spec.arity == claudeValues && !attached {
		return queue, name
	}
	return queue, ""
}

// attachedArg — значение из --opt=value или кластера -nvalue: слово с
// признаками исходного
func attachedArg(arg claudeArg, value string) claudeArg {
	attached := arg
	attached.text = value
	attached.word.text = value
	attached.word.literal = !arg.dynamic && !strings.ContainsAny(value, "$`")
	return attached
}

func (p *claudeArguments) value(name string, arg claudeArg) {
	switch claudeOptions[name].kind {
	case claudeJudged, claudeRefusedJudged:
		p.judged = append(p.judged, word{text: name, literal: true, shellLiteral: true, shellLiteralKnown: true}, arg.word)
		if name == "--agents" {
			p.agents = append(p.agents, arg)
		}
	case claudeDebugFile:
		p.debugFiles = append(p.debugFiles, arg)
	}
}

// refuseCommand — первое слово-операнд — подкоманда claude: пропуск только
// подкоманде из claudeReadingCommands
func (p *claudeArguments) refuseCommand() {
	if len(p.operands) == 0 || !slices.Contains(claudeCommands, p.operands[0].text) {
		return
	}
	texts := make([]string, len(p.operands))
	for i, operand := range p.operands {
		texts[i] = operand.text
	}
	for _, form := range claudeReadingCommands {
		if len(form) == len(texts) && slices.Equal(form[:len(form)-1], texts[:len(form)-1]) && (form[len(form)-1] == "" || form[len(form)-1] == texts[len(texts)-1]) {
			return
		}
	}
	forms := make([]string, len(claudeReadingCommands))
	for i, form := range claudeReadingCommands {
		forms[i] = strings.Join(form, " ")
		if form[len(form)-1] == "" {
			forms[i] = strings.Join(form[:len(form)-1], " ") + " <имя>"
		}
	}
	p.refuse(fmt.Sprintf("подкоманда claude %s не в списке читающих: без отказа — только %s", strings.Join(texts, " "), strings.Join(forms, ", ")))
}

// newClaudeArg — значение слова вызова. Литеральное слово shellcmd отдаёт
// значением, слово с подстановкой — исходным текстом: его кавычки снимаются,
// простая переменная ($X, ${X}) с одним известным значением (присваивание в
// команде или env запроса) подставляется
func newClaudeArg(arg word, values func(string) ([]string, bool)) claudeArg {
	parsed := claudeArg{word: arg, text: arg.text, splits: arg.braces}
	if arg.invalidANSI {
		parsed.err = fmt.Errorf("ANSI-строка содержит NUL; убери \\0 и укажи значение литералом")
		parsed.splits = true
		return parsed
	}
	if !computedArg(arg.text) {
		return parsed
	}
	parsed.dynamic = true
	var words []*syntax.Word
	for w, err := range syntax.NewParser().WordsSeq(strings.NewReader(arg.text)) {
		if err != nil {
			// слово не разобрать: сколько слов из него выйдет, не известно
			parsed.err = err
			parsed.splits = true
			return parsed
		}
		words = append(words, w)
	}
	if len(words) != 1 {
		parsed.splits = true
		return parsed
	}
	var b strings.Builder
	parsed.renderParts(&b, words[0].Parts, false, values)
	parsed.text = b.String()
	return parsed
}

func (a *claudeArg) renderParts(b *strings.Builder, parts []syntax.WordPart, quoted bool, values func(string) ([]string, bool)) {
	for _, part := range parts {
		switch p := part.(type) {
		case *syntax.Lit:
			a.write(b, p.Value)
		case *syntax.SglQuoted:
			if p.Dollar {
				value, err := shellANSIValue(p.Value)
				if err != nil {
					a.err = err
					a.splits = true
					continue
				}
				a.write(b, value)
			} else {
				a.write(b, p.Value)
			}
		case *syntax.DblQuoted:
			a.renderParts(b, p.Parts, true, values)
		case *syntax.ParamExp:
			a.renderParam(b, p, quoted, values)
		case *syntax.CmdSubst:
			hdoc, known := heredocCat(p)
			if !known {
				a.unknownPart(b, quoted)
				continue
			}
			// тело here-doc — как слово в двойных кавычках; $(…) снимает
			// хвостовые переводы строки
			start, patternStart := b.Len(), len(a.pattern)
			a.renderParts(b, hdoc.Parts, true, values)
			text := b.String()
			body := strings.TrimRight(text[start:], "\n")
			b.Reset()
			b.WriteString(text[:start] + body)
			// QuoteMeta перевод строки не экранирует
			a.pattern = a.pattern[:patternStart] + strings.TrimRight(a.pattern[patternStart:], "\n")
			a.splits = a.splits || !quoted && (body == "" || strings.ContainsAny(body, " \t\n*?[$"))
		default:
			a.unknownPart(b, quoted)
		}
	}
}

// heredocCat — тело here-doc в $(cat <<EOF … EOF). Иное — не известно, как и
// `\` в теле с ограничителем без кавычек: bash снимает это экранирование
// (`\` с переводом строки склеивает строки). В теле с ограничителем в
// кавычках подстановок нет
func heredocCat(subst *syntax.CmdSubst) (*syntax.Word, bool) {
	if len(subst.Stmts) != 1 {
		return nil, false
	}
	stmt := subst.Stmts[0]
	call, ok := stmt.Cmd.(*syntax.CallExpr)
	if !ok || stmt.Negated || stmt.Background || stmt.Coprocess || len(call.Assigns) != 0 || len(call.Args) != 1 || len(stmt.Redirs) != 1 {
		return nil, false
	}
	if name, literal := literalValue(call.Args[0]); !literal || name != "cat" {
		return nil, false
	}
	redir := stmt.Redirs[0]
	if redir.Op != syntax.Hdoc || redir.Hdoc == nil {
		return nil, false
	}
	quoted := slices.ContainsFunc(redir.Word.Parts, func(part syntax.WordPart) bool {
		_, lit := part.(*syntax.Lit)
		return !lit
	})
	escaped := slices.ContainsFunc(redir.Hdoc.Parts, func(part syntax.WordPart) bool {
		lit, ok := part.(*syntax.Lit)
		return ok && strings.Contains(lit.Value, `\`)
	})
	return redir.Hdoc, quoted || !escaped
}

// renderParam — $X: известное значение, одно из нескольких или неизвестное
func (a *claudeArg) renderParam(b *strings.Builder, p *syntax.ParamExp, quoted bool, values func(string) ([]string, bool)) {
	if !quoted && values != nil {
		if _, changed := values("\x00IFS"); changed {
			a.unknownPart(b, false)
			return
		}
	}
	found := simpleParamValues(p, values)
	if len(found) == 0 {
		a.unknownPart(b, quoted)
		// "$@" и "${a[@]}" дают несколько слов и в кавычках
		a.splits = a.splits || p.Index != nil || p.Param != nil && (p.Param.Value == "@" || p.Param.Value == "*")
		return
	}
	// A joined, unquoted value is ambiguous to users and shell word splitting;
	// the quoted --opt="$VALUE" form preserves its argument boundary.
	if !quoted && b.Len() > 0 {
		a.splits = true
	}
	// без кавычек пустое значение убирает слово, пробел и маска дают
	// несколько слов
	a.splits = a.splits || !quoted && slices.ContainsFunc(found, func(value string) bool {
		return value == "" || strings.ContainsAny(value, " \t\n*?[")
	})
	if len(found) == 1 {
		a.write(b, found[0])
		return
	}
	// несколько значений (цикл for): слово одно, но какое — не известно
	a.mayBeOption = a.mayBeOption || b.Len() == 0 && slices.ContainsFunc(found, func(value string) bool { return strings.HasPrefix(value, "-") })
	b.WriteString("$")
	alternatives := make([]string, len(found))
	for i, value := range found {
		alternatives[i] = regexp.QuoteMeta(value)
	}
	a.pattern += "(?:" + strings.Join(alternatives, "|") + ")"
	a.unresolved = true
}

// unknownPart — подстановка с неизвестным значением
func (a *claudeArg) unknownPart(b *strings.Builder, quoted bool) {
	a.mayBeOption = a.mayBeOption || b.Len() == 0
	b.WriteString("$")
	a.pattern += ".*"
	a.unresolved = true
	a.splits = a.splits || !quoted
}

// simpleParamValues — известные значения $X или ${X}: присваивания в команде,
// слова цикла for, env запроса. Значение с `\` не известно: присваивание
// отдаёт его до снятия экранирования (o=\--bare)
func simpleParamValues(p *syntax.ParamExp, values func(string) ([]string, bool)) []string {
	if values == nil || p.Param == nil || p.Excl || p.Length || p.Width || p.Index != nil || p.Slice != nil || p.Repl != nil || p.Exp != nil || p.Names != 0 || p.NestedParam != nil {
		return nil
	}
	found, known := values(p.Param.Value)
	if !known || slices.ContainsFunc(found, func(value string) bool { return strings.Contains(value, `\`) }) {
		return nil
	}
	slices.Sort(found)
	return slices.Compact(found)
}

// clientWordValues — значения переменной для слов клиента claude:
// присваивания и циклы for в команде, env запроса. Второй результат — все
// значения известны
// cyclomatic-complexity: safe — each branch distinguishes a shell write, inherited value, or literal alternative.
func clientWordValues(command string, request hookctx.Context, logger core.Logger) func(string) ([]string, bool) {
	assigns, assignsErr := shellcmd.Assigns("IFS=\n" + command)
	ifsChanged := false
	for _, assign := range assigns {
		ifsChanged = ifsChanged || assign.Name == "IFS" && (assign.Offset != 0 || assign.Unknown)
	}
	return func(name string) ([]string, bool) {
		if name == "\x00IFS" {
			return nil, ifsChanged || assignsErr != nil
		}
		if slices.Contains([]string{"_", "BASH_REMATCH", "OPTARG", "MAPFILE", "REPLY", "FUNCNAME"}, name) {
			return nil, false
		}
		// Sentinel makes implicit writes (read, printf -v, unset) visible too.
		prefix := name + "=\n"
		marked, err := shellcmd.Assigns(prefix + command)
		if err != nil {
			return nil, false
		}
		conditional := false
		for _, assign := range marked {
			if assign.Name == name && (assign.Unknown || assign.Append) {
				// A for-loop itself marks its iterator unknown. Its literal
				// alternatives are handled below, provided no other write or
				// global uncertainty can change them.
				file, parseErr := syntax.NewParser().Parse(strings.NewReader(command), "")
				if parseErr != nil {
					logger.Warn("model_guard: цикл не разобран", "error", parseErr)
					return nil, false
				}
				if !literalLoopOnly(file, command, name, logger) {
					return nil, false
				}
				loop, _ := forLoopValues(file, name)
				conditional = len(loop) == 0
				break
			}
		}
		file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
		if err != nil {
			// неразобранная команда: значения не известны, слово с этой
			// переменной claude может принять за опцию
			logger.Warn("model_guard: команда не разобрана, значения переменной для слов claude не известны", "name", name, "error", err)
			return nil, false
		}
		values, dynamic := shellEnvValues(command, name, logger)
		loopValues, loopDynamic := forLoopValues(file, name)
		values = append(values, loopValues...)
		guaranteed := conditional && assignmentBeforeEveryClient(file, command, name)
		if inherited, ok := request.LookupEnv(name); ok && !guaranteed {
			values = append(values, inherited)
		} else if conditional && !guaranteed {
			values = append(values, "")
		}
		return values, !dynamic && !loopDynamic && len(values) > 0
	}
}

func assignmentBeforeEveryClient(file *syntax.File, command, name string) bool {
	calls, err := shellcmd.Calls(command)
	if err != nil {
		// error-masked-as-false-bool: safe — false means the assignment is not proven to dominate the client.
		return false
	}
	count := 0
	for i := range calls {
		call := &calls[i]
		if !slices.Contains([]string{"claude", "cl"}, call.Name) {
			continue
		}
		dominates := false
		syntax.Walk(file, func(node syntax.Node) bool {
			bin, ok := node.(*syntax.BinaryCmd)
			if ok && bin.Op == syntax.AndStmt && int(bin.Y.Pos().Offset()) <= call.Offset && call.Offset < int(bin.Y.End().Offset()) && assignmentOnSuccess(bin.X, name) {
				dominates = true
			}
			return true
		})
		if !dominates {
			return false
		}
		count++
	}
	return count > 0
}

func assignmentOnSuccess(stmt *syntax.Stmt, name string) bool {
	switch node := stmt.Cmd.(type) {
	case *syntax.BinaryCmd:
		return node.Op == syntax.AndStmt && (assignmentOnSuccess(node.X, name) || assignmentOnSuccess(node.Y, name))
	case *syntax.CallExpr:
		if len(node.Args) != 0 {
			return false
		}
		for _, assign := range node.Assigns {
			if assign.Name != nil && assign.Name.Value == name && !assign.Append && assign.Value != nil {
				_, literal := literalValue(assign.Value)
				return literal
			}
		}
	}
	return false
}

func literalLoopOnly(file *syntax.File, command, name string, logger core.Logger) bool {
	loop, dynamic := forLoopValues(file, name)
	if dynamic {
		return false
	}
	marked, err := shellcmd.Assigns(environmentControl + "=\n" + command)
	if err != nil {
		logger.Warn("model_guard: присваивания цикла не разобраны", "error", err)
		return false
	}
	hasLiteral := len(loop) > 0
	for _, assign := range marked {
		if assign.Name == environmentControl && assign.Unknown {
			return false
		}
		if assign.Name == name {
			if assign.Append || strings.ContainsAny(assign.Value, "$`\\") {
				return false
			}
			hasLiteral = true
		}
	}
	if !hasLiteral {
		return false
	}
	return !hasDynamicValueWrite(file)
}

func hasDynamicValueWrite(file *syntax.File) bool {
	unsafe := false
	syntax.Walk(file, func(node syntax.Node) bool {
		switch node.(type) {
		case *syntax.ArithmCmd, *syntax.ArithmExp:
			unsafe = true
		}
		if param, ok := node.(*syntax.ParamExp); ok && param.Exp != nil && (param.Exp.Op.String() == ":=" || param.Exp.Op.String() == "=") {
			unsafe = true
		}
		if decl, ok := node.(*syntax.DeclClause); ok {
			if decl.Variant.Value == "readonly" {
				unsafe = true
			}
			for _, arg := range decl.Args {
				if arg.Value != nil {
					value, literal := literalValue(arg.Value)
					unsafe = unsafe || literal && strings.HasPrefix(value, "-") && strings.ContainsAny(value, "ilurn")
				}
			}
		}
		if call, ok := node.(*syntax.CallExpr); ok && len(call.Args) > 0 {
			cmd, literal := shellEffectiveCommand(call.Args)
			if !literal || shellVariableWriter(cmd) {
				unsafe = true
			}
		}
		return true
	})
	return unsafe
}

func shellVariableWriter(name string) bool {
	return slices.Contains([]string{"read", "printf", "mapfile", "readarray", "unset", "getopts", "wait", "export", "declare", "typeset", "readonly", "local"}, name)
}

// shellEffectiveCommand resolves quote removal and the shell's builtin/command
// wrappers. A dynamic command could itself be a shell variable writer.
func shellEffectiveCommand(args []*syntax.Word) (string, bool) {
	for i := 0; i < len(args); i++ {
		if _, literal := literalValue(args[i]); !literal {
			return "", false
		}
		value, err := shellStaticValue(args[i])
		if err != nil {
			// error-masked-as-false-bool: safe — false means the command is unknown and is refused.
			return "", false
		}
		if value != "builtin" && value != "command" {
			return filepath.Base(value), true
		}
		// quadratic-loop: safe — i only advances and each wrapper option is visited once.
		for i+1 < len(args) {
			option, literal := literalValue(args[i+1])
			if !literal || !strings.HasPrefix(option, "-") {
				break
			}
			i++
			if option == "--" {
				break
			}
		}
	}
	return "", false
}

func shellStaticValue(word *syntax.Word) (string, error) {
	var out strings.Builder
	for _, part := range word.Parts {
		if lit, ok := part.(*syntax.Lit); ok {
			for i := 0; i < len(lit.Value); i++ {
				if lit.Value[i] == '\\' && i+1 < len(lit.Value) {
					i++
				}
				out.WriteByte(lit.Value[i])
			}
			continue
		}
		value, err := expand.Literal(nil, &syntax.Word{Parts: []syntax.WordPart{part}})
		if err != nil {
			return "", fmt.Errorf("decode shell word: %w", err)
		}
		out.WriteString(value)
	}
	return out.String(), nil
}

// clientTempDirs — временные каталоги, куда claude может писать журнал
// --debug-file
func clientTempDirs(request hookctx.Context) []string {
	dirs := []string{"/tmp", request.Path(hookctx.EnvTmpDir), request.Path(hookctx.EnvClaudeCodeTmpDir)}
	return slices.DeleteFunc(dirs, func(dir string) bool { return dir == "" })
}

// forLoopValues — слова циклов `for name in …` в команде; цикл без in, слово
// с подстановкой, экранированием, скобками или маской — значение не известно
// (второй результат)
func forLoopValues(file *syntax.File, name string) ([]string, bool) {
	var values []string
	dynamic := false
	syntax.Walk(file, func(node syntax.Node) bool {
		loop, ok := node.(*syntax.ForClause)
		if !ok {
			return true
		}
		iter, ok := loop.Loop.(*syntax.WordIter)
		if !ok || iter.Name.Value != name {
			return true
		}
		if !iter.InPos.IsValid() {
			dynamic = true
		}
		for _, item := range iter.Items {
			value, literal := literalValue(item)
			if !literal || slices.ContainsFunc(item.Parts, func(part syntax.WordPart) bool {
				lit, ok := part.(*syntax.Lit)
				return ok && strings.ContainsAny(lit.Value, `\{*?[`)
			}) {
				dynamic = true
				continue
			}
			values = append(values, value)
		}
		return true
	})
	return values, dynamic
}

// debugFileRefusal — отказ пути --debug-file: claude пишет туда журнал, и
// путь вне временного каталога может быть settings.json, определением агента,
// бинарём хука или его конфигом. Неизвестный путь (подстановка без
// известного значения) — отказ
func debugFileRefusal(arg claudeArg, tempDirs []string, cwd, home string) string {
	if arg.unresolved {
		return fmt.Sprintf("путь --debug-file %s не установить: claude пишет туда журнал, укажи путь во временном каталоге литералом", arg.word.text)
	}
	path := tildePath(arg.text, arg.word.tilde, home)
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	path = filepath.Clean(path)
	if slices.Contains(strings.Split(path, string(filepath.Separator)), ".claude") {
		return fmt.Sprintf("--debug-file %s пишет в каталог .claude: там настройки, агенты и хуки", path)
	}
	for _, dir := range tempDirs {
		if dir != "" && strings.HasPrefix(path, filepath.Clean(dir)+string(filepath.Separator)) {
			return ""
		}
	}
	return fmt.Sprintf("--debug-file %s пишет вне временного каталога (%s): там могут быть настройки, агенты и хуки", path, strings.Join(tempDirs, ", "))
}

// agentsRefusal — определение --agents (строка JSON или файл), где агент
// задаёт mcpServers или hooks: свои MCP-серверы и хуки агента (схема агента в
// коде бинаря 2.1.332). Определение, которое не прочесть, судит model_guard
// (строку или файл --agents не разобрать)
func agentsRefusal(arg claudeArg, read func(string, shellcmd.Tilde, string) [][]byte, cwd string) string {
	if arg.word.ansiEscapedQuote {
		return "в --agents использован апостроф с экранированием внутри $'…': передай JSON в двойных кавычках или файлом"
	}
	if arg.unresolved {
		return ""
	}
	raws := [][]byte{[]byte(arg.text)}
	inlineJSON := inlineAgentsJSON(arg.text)
	if !inlineJSON {
		if read == nil {
			return ""
		}
		raws = read(arg.text, arg.word.tilde, cwd)
	}
	for _, raw := range raws {
		agents, ok := parseAgentsJSON(raw)
		if !ok {
			if inlineJSON {
				return "строку --agents не разобрать: откажись от запуска, пока JSON не исправлен"
			}
			return "файл --agents не прочитать: откажись от запуска, пока JSON не исправлен"
		}
		for _, name := range slices.Sorted(maps.Keys(agents)) {
			for _, key := range []string{"mcpServers", "hooks"} {
				if _, ok := agents[name][key]; ok {
					return fmt.Sprintf("агент %s в --agents задаёт %s: агент приносит свои MCP-серверы и хуки; задай агенту только модель, промпт и инструменты", name, key)
				}
			}
		}
	}
	return ""
}

// clientEnvironmentCandidates — отказ переменным claude вне
// claudeEnvironmentAllowed и HOME, которые команда с клиентом claude задаёт
// (присваивание, export, env, declare -x, set -a) любым значением, кроме
// прежнего: HOME — домашний каталог, CLAUDE_CONFIG_DIR — ~/.claude. Значение
// без вида (read, printf -v, source, eval) — unknownEnvironmentCandidates
func clientEnvironmentCandidates(command string, calls []shellcmd.Call, home string, logger core.Logger) []modelCandidate {
	names := clientEnvironmentNames(command)
	var found []modelCandidate
	for _, name := range names {
		values, dynamic := shellEnvValues(command, name, logger)
		if dynamic || slices.ContainsFunc(values, func(value string) bool { return !defaultEnvironmentValue(name, value, home) }) {
			found = append(found, clientOptionCandidate(environmentRefusal(name)))
		}
	}
	unknown, err := unknownEnvironmentCandidates(command, calls, names)
	if err != nil {
		// неразобранную команду с claude судит unparsedClientCandidates
		logger.Warn("model_guard: присваивания окружения claude не разобраны", "error", err)
		return found
	}
	return append(found, unknown...)
}

// environmentControl — имя, которого нет в команде: если и его присваивание
// Assigns пометил Unknown, учёт снят весь (eval, source, вложенный скрипт,
// команда из подстановки)
const environmentControl = "CHOOKS_ENVIRONMENT_CONTROL"

// environmentSourcing — команды, которые задают переменные оболочки, где
// потом запустится claude, текстом, которого в команде нет
var environmentSourcing = []string{"source", ".", "eval"}

// clientEnvironmentNames — HOME и переменные claude из текста команды вне
// claudeEnvironmentAllowed и modelEnvNames
func clientEnvironmentNames(command string) []string {
	names := append(claudeEnvironmentName.FindAllString(command, -1), "HOME")
	if file, err := syntax.NewParser().Parse(strings.NewReader(command), ""); err == nil {
		syntax.Walk(file, func(node syntax.Node) bool {
			if w, ok := node.(*syntax.Word); ok {
				if value, literal := literalValue(w); literal {
					names = append(names, claudeEnvironmentName.FindAllString(value, -1)...)
				}
			}
			return true
		})
	}
	names = slices.DeleteFunc(names, func(name string) bool {
		_, model := modelEnvNames[name]
		return model || slices.Contains(claudeEnvironmentAllowed, name)
	})
	slices.Sort(names)
	return slices.Compact(names)
}

// defaultEnvironmentValue — значение, с которым claude читает прежние
// настройки: HOME — домашний каталог, CLAUDE_CONFIG_DIR — ~/.claude
func defaultEnvironmentValue(name, value, home string) bool {
	if after, ok := strings.CutPrefix(value, "~"); ok && (after == "" || strings.HasPrefix(after, "/")) {
		value = home + after
	}
	value = filepath.Clean(value)
	switch name {
	case "HOME":
		return value == filepath.Clean(home)
	case "CLAUDE_CONFIG_DIR":
		return value == filepath.Join(home, ".claude")
	}
	return false
}

func environmentRefusal(name string) string {
	if effect, ok := claudeEnvironmentEffects[name]; ok {
		return fmt.Sprintf("переменная %s у клиента claude %s", name, effect)
	}
	return fmt.Sprintf("переменная %s у клиента claude не в списке известных: в нём только переменные, которые не снимают хуки и не подменяют настройки, MCP и плагины; убери это присваивание из окружения клиента", name)
}

// unknownEnvironmentCandidates — переменные names, которые команда задаёт без
// видимого значения: read, printf -v, mapfile, getopts (по имени), source и
// eval (любые). Копия команды начинается присваиванием этих имён и
// environmentControl, и akit shellcmd.Assigns помечает его Unknown, если
// дальше значение могло смениться. При снятом учёте source/eval могут задать
// любые имена; в остальных случаях судятся имена, видимые в команде.
func unknownEnvironmentCandidates(command string, calls []shellcmd.Call, names []string) ([]modelCandidate, error) {
	sentinel := strings.Join(append(slices.Clone(names), environmentControl), "= ") + "=\n"
	assigns, err := shellcmd.Assigns(sentinel + command)
	if err != nil {
		return nil, fmt.Errorf("shellcmd assigns: %w", err)
	}
	var unknown []string
	for _, assign := range assigns {
		if assign.Offset < len(sentinel) && assign.Unknown {
			unknown = append(unknown, assign.Name)
		}
	}
	if slices.Contains(unknown, environmentControl) {
		sourcing := false
		for i := range calls {
			sourcing = sourcing || slices.Contains(environmentSourcing, calls[i].Name)
		}
		if !sourcing {
			if name := namedEnvironmentWrite(calls, names); name != "" {
				return []modelCandidate{clientOptionCandidate(fmt.Sprintf("окружение клиента claude может задать %s, но значение не проверить; задай его литералом или запусти claude отдельной командой", name))}, nil
			}
			if environmentControlUnknown(command) {
				return []modelCandidate{clientOptionCandidate("окружение клиента claude не проверить после неизвестной команды; запусти claude отдельной командой и задай окружение литералами")}, nil
			}
			return nil, nil
		}
		return []modelCandidate{clientOptionCandidate("команда с source или eval может задать окружение клиента claude (HOME, CLAUDE_*): его не проверить; задай переменные в команде литералом")}, nil
	}
	if len(unknown) == 0 {
		return nil, nil
	}
	if len(unknown) == 1 && strings.Contains(command, "unset "+unknown[0]) {
		return []modelCandidate{clientOptionCandidate(fmt.Sprintf("команда снимает переменную %s у клиента claude; убери unset или задай окружение явно", unknown[0]))}, nil
	}
	return []modelCandidate{clientOptionCandidate(fmt.Sprintf("переменные %s у клиента claude команда задаёт без видимого значения (read, printf -v): их не проверить", strings.Join(unknown, ", ")))}, nil
}

// A known external command or a child shell cannot modify the calling shell.
// Replace only that command word before asking Assigns about the whole command.
// cyclomatic-complexity: safe — each branch conservatively identifies whether a preceding command can change the shell environment.
func environmentControlUnknown(command string) bool {
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	// error-masking: safe — an unparsed environment is refused.
	if err != nil {
		return true
	}
	knownCommands := map[string]string{}
	functions := map[string]bool{}
	conditional := false
	syntax.Walk(file, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.FuncDecl:
			if n.Name != nil {
				functions[n.Name.Value] = true
			}
			return false
		case *syntax.IfClause, *syntax.CaseClause, *syntax.WhileClause, *syntax.ForClause, *syntax.BinaryCmd:
			conditional = true
		}
		return true
	})
	modified := []byte(command)
	syntax.Walk(file, func(node syntax.Node) bool {
		call, ok := node.(*syntax.CallExpr)
		if !ok {
			return true
		}
		for _, assign := range call.Assigns {
			if assign.Name != nil && assign.Value != nil {
				value, literal := literalValue(assign.Value)
				if literal && !assign.Append {
					knownCommands[assign.Name.Value] = value
				} else {
					delete(knownCommands, assign.Name.Value)
				}
			}
		}
		if len(call.Args) == 0 {
			return true
		}
		name, literal := literalValue(call.Args[0])
		if literal && (shellVariableWriter(name) || slices.Contains(environmentSourcing, name)) {
			clear(knownCommands)
		}
		safeChild := literal && (shellcmd.IsShell(filepath.Base(name)) || wrappedChildShell(command, call))
		parts := call.Args[0].Parts
		if len(parts) == 1 {
			if quoted, ok := parts[0].(*syntax.DblQuoted); ok {
				parts = quoted.Parts
			}
		}
		if !safeChild && len(parts) == 1 {
			p, ok := parts[0].(*syntax.ParamExp)
			if ok && p.Param != nil && p.Exp == nil && !conditional {
				value, exists := knownCommands[p.Param.Value]
				safeChild = exists && shellSafeExternal(value) && !functions[filepath.Base(value)]
			}
		}
		if !safeChild {
			if !literal {
				clear(knownCommands)
			}
			return true
		}
		start, end := int(call.Args[0].Pos().Offset()), int(call.Args[0].End().Offset())
		if start >= 0 && end <= len(modified) && start < end {
			modified[start] = ':'
			for i := start + 1; i < end; i++ {
				modified[i] = ' '
			}
		}
		return true
	})
	marked, err := shellcmd.Assigns(environmentControl + "=\n" + string(modified))
	// error-masking: safe — true means the unparsed environment is refused.
	if err != nil {
		return true
	}
	for _, assign := range marked {
		if assign.Name == environmentControl && assign.Unknown {
			return true
		}
	}
	return false
}

func shellSafeExternal(name string) bool {
	if name == "" || strings.ContainsAny(name, " \t\n$`\\") {
		return false
	}
	name = filepath.Base(name)
	return !shellVariableWriter(name) && !slices.Contains([]string{"eval", "source", ".", "set", "cd", "pushd", "popd", "builtin", "command", "trap", "alias", "shopt", "exec"}, name)
}

func wrappedChildShell(prefix string, call *syntax.CallExpr) bool {
	start, end := int(call.Pos().Offset()), int(call.End().Offset())
	if start < 0 || start > end || end > len(prefix) {
		return false
	}
	calls, err := shellcmd.Calls(prefix[start:end])
	if err != nil {
		// error-masked-as-false-bool: safe — false keeps the command classified as unknown.
		return false
	}
	child := false
	for i := range calls {
		if slices.Contains([]string{"claude", "cl"}, calls[i].Name) {
			return false
		}
		child = child || shellcmd.IsShell(calls[i].Name) || slices.ContainsFunc(calls[i].Via, shellcmd.IsShell)
	}
	return child
}

func namedEnvironmentWrite(calls []shellcmd.Call, names []string) string {
	for i := range calls {
		call := &calls[i]
		if !shellVariableWriter(call.Name) {
			continue
		}
		for _, name := range names {
			if slices.Contains(call.Args, name) {
				return name
			}
		}
	}
	return ""
}

// literalSubscript — индекс в присваивании элементу массива из одних
// литеральных знаков, без пробелов, кавычек, подстановок и знаков управления:
// R[gh:owner/repo#1]=… (ключ ассоциативного массива)
var literalSubscript = regexp.MustCompile(`(?:^|[\s;&|(])[A-Za-z_][A-Za-z0-9_]*\[([^\s\]$` + "`" + `;&|<>()'"\\]+)\]\+?=`)

// blankLiteralSubscripts — команда, где такие индексы заменены нулями той же
// длины. mvdan разбирает индекс как арифметику и не разбирает ключ
// ассоциативного массива с `:` или `#`, а bash такую команду исполняет; ключ
// из литеральных знаков ничего не запускает, и разбор без него видит те же
// вызовы
func blankLiteralSubscripts(command string) (string, bool) {
	matches := literalSubscript.FindAllStringSubmatchIndex(command, -1)
	if len(matches) == 0 {
		return command, false
	}
	blanked := []byte(command)
	for _, match := range matches {
		for i := match[2]; i < match[3]; i++ {
			blanked[i] = '0'
		}
	}
	return string(blanked), true
}

// claudeClientWord — слово claude или cl в тексте команды (не .claude/ и не
// claude-hooks)
var claudeClientWord = regexp.MustCompile(`(^|[^[:alnum:]_.-])(claude|cl)([^[:alnum:]_.-]|$)`)

// unparsedClientCandidates — команда (или вложенный скрипт) не разобрана, а
// в тексте есть запуск claude: его опции и окружение не проверить. Слово в
// echo/printf — текст, не запуск
func unparsedClientCandidates(text string) []modelCandidate {
	for _, match := range claudeClientWord.FindAllStringSubmatchIndex(text, -1) {
		if !printedMention(text, match[4]) {
			return []modelCandidate{clientOptionCandidate("команда с клиентом claude не разобрана: его опции и окружение не проверить")}
		}
	}
	return nil
}
