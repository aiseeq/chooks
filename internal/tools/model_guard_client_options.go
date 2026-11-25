package tools

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/aiseeq/akit/shellcmd"
	"github.com/aiseeq/chooks/internal/core"
	"github.com/aiseeq/chooks/internal/hookctx"
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
	"--restricted": {}, "--strict-mcp-config": {}, "--bg": {}, "--background": {},

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

// claudeReadingCommands — подкоманды, которые только читают: остальные
// добавляют MCP-серверы и плагины, меняют настройки, вход или сам клиент
var claudeReadingCommands = [][]string{{"mcp", "list"}, {"mcp", "get", ""}, {"auth", "status"}, {"plugin", "list"}, {"plugins", "list"}, {"agents"}}

// claudeCommandOptions — опции подкоманды, которые не подменяют модель,
// настройки, MCP и плагины: claude agents --json [--all] [--cwd <путь>]
// печатает список сессий; прочие опции agents (--model, --mcp-config,
// --settings, --plugin-dir) судятся как опции claude
var claudeCommandOptions = map[string]map[string]claudeOption{
	"agents": {"--json": {}, "--all": {}, "--cwd": {arity: claudeValue}},
}

// claudeConfigEnvironment — переменные, с которыми claude снимает хуки или
// берёт настройки не из ~/.claude: CLAUDE_CODE_SIMPLE — то же, что --bare,
// CLAUDE_CODE_SAFE_MODE — --safe-mode (по коду бинаря 2.1.332),
// CLAUDE_CONFIG_DIR — другой каталог настроек и хуков,
// CLAUDE_CODE_PLUGIN_DIRS — каталоги плагинов, CLAUDE_CODE_RESTRICT_PERSONAL_CONFIG —
// личные хуки не исполняются (по тексту в бинаре, не исполнено)
var claudeConfigEnvironment = []string{
	"CLAUDE_CODE_SIMPLE", "CLAUDE_CODE_SAFE_MODE", "CLAUDE_CONFIG_DIR", "CLAUDE_CODE_PLUGIN_DIRS",
	"CLAUDE_CODE_RESTRICT_PERSONAL_CONFIG",
}

// argumentFeeders — обёртки, которые дописывают клиенту слова из ввода: их
// опции по команде не видны
var argumentFeeders = []string{"xargs", "parallel"}

// claudeArg — слово аргумента claude: text — его значение после кавычек,
// подстановка заменена известным значением переменной или `$`; splits —
// подстановка без кавычек, которую bash может разбить на несколько слов;
// unresolved — в text осталась неизвестная подстановка; mayBeOption — слово
// начинается подстановкой, значение которой может начаться с `-`: commander
// примет его за опцию
type claudeArg struct {
	word        word
	text        string
	dynamic     bool
	splits      bool
	unresolved  bool
	mayBeOption bool
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
	variadic := ""
	for len(queue) > 0 {
		arg := queue[0]
		queue = queue[1:]
		unknownBefore := parsed.unknownLast
		parsed.unknownLast = false
		if arg.err != nil {
			parsed.refuse(fmt.Sprintf("слово %s не разобрано (%v): опции клиента claude не проверить", arg.word.text, arg.err))
			parsed.unknownLast = true
			continue
		}
		if arg.splits {
			parsed.refuse(fmt.Sprintf("слово %s: подстановка может дать несколько слов, опции клиента claude не проверить", arg.word.text))
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
			parsed.refuse(fmt.Sprintf("слово %s начинается подстановкой: её значение с - claude примет за опцию; значение опции пиши через =, промпт — после --", arg.word.text))
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
	return fmt.Sprintf("опция %s клиента claude вне проверенного набора: такие опции подменяют модель, настройки, MCP, плагины или снимают хуки", name)
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
			p.refuse(fmt.Sprintf("значение %s опции %s: подстановка может дать несколько слов, опции клиента claude не проверить", queue[0].word.text, name))
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
	p.refuse(fmt.Sprintf("подкоманда claude %s меняет настройки, MCP, плагины или сам клиент; без отказа — только mcp list, mcp get, auth status, plugin list", strings.Join(texts, " ")))
}

// newClaudeArg — значение слова вызова. Литеральное слово shellcmd отдаёт
// значением, слово с подстановкой — исходным текстом: его кавычки снимаются,
// простая переменная ($X, ${X}) с одним известным значением (присваивание в
// команде или env запроса) подставляется
func newClaudeArg(arg word, values func(string) ([]string, bool)) claudeArg {
	parsed := claudeArg{word: arg, text: arg.text}
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
			b.WriteString(p.Value)
		case *syntax.SglQuoted:
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			a.renderParts(b, p.Parts, true, values)
		case *syntax.ParamExp:
			a.renderParam(b, p, quoted, values)
		case *syntax.CmdSubst:
			body, known := heredocCatValue(p)
			if !known {
				a.unknownPart(b, quoted)
				continue
			}
			a.splits = a.splits || !quoted && (body == "" || strings.ContainsAny(body, " \t\n*?["))
			b.WriteString(body)
		default:
			a.unknownPart(b, quoted)
		}
	}
}

// heredocCatValue — значение $(cat <<'EOF' … EOF): тело here-doc без
// подстановок, без хвостовых переводов строки. Иное — не известно, как и `\`
// в теле с ограничителем без кавычек: bash снимает это экранирование
func heredocCatValue(subst *syntax.CmdSubst) (string, bool) {
	if len(subst.Stmts) != 1 {
		return "", false
	}
	stmt := subst.Stmts[0]
	call, ok := stmt.Cmd.(*syntax.CallExpr)
	if !ok || stmt.Negated || stmt.Background || stmt.Coprocess || len(call.Assigns) != 0 || len(call.Args) != 1 || len(stmt.Redirs) != 1 {
		return "", false
	}
	if name, literal := literalValue(call.Args[0]); !literal || name != "cat" {
		return "", false
	}
	redir := stmt.Redirs[0]
	if redir.Op != syntax.Hdoc || redir.Hdoc == nil {
		return "", false
	}
	body, literal := literalValue(redir.Hdoc)
	quoted := slices.ContainsFunc(redir.Word.Parts, func(part syntax.WordPart) bool {
		_, lit := part.(*syntax.Lit)
		return !lit
	})
	if !literal || strings.Contains(body, `\`) && !quoted {
		return "", false
	}
	return strings.TrimRight(body, "\n"), true
}

// renderParam — $X: известное значение, одно из нескольких или неизвестное
func (a *claudeArg) renderParam(b *strings.Builder, p *syntax.ParamExp, quoted bool, values func(string) ([]string, bool)) {
	found := simpleParamValues(p, values)
	if len(found) == 0 {
		a.unknownPart(b, quoted)
		// "$@" и "${a[@]}" дают несколько слов и в кавычках
		a.splits = a.splits || p.Index != nil || p.Param != nil && (p.Param.Value == "@" || p.Param.Value == "*")
		return
	}
	// без кавычек пустое значение убирает слово, пробел и маска дают
	// несколько слов
	a.splits = a.splits || !quoted && slices.ContainsFunc(found, func(value string) bool {
		return value == "" || strings.ContainsAny(value, " \t\n*?[")
	})
	if len(found) == 1 {
		b.WriteString(found[0])
		return
	}
	// несколько значений (цикл for): слово одно, но какое — не известно
	a.mayBeOption = a.mayBeOption || b.Len() == 0 && slices.ContainsFunc(found, func(value string) bool { return strings.HasPrefix(value, "-") })
	b.WriteString("$")
	a.unresolved = true
}

// unknownPart — подстановка с неизвестным значением
func (a *claudeArg) unknownPart(b *strings.Builder, quoted bool) {
	a.mayBeOption = a.mayBeOption || b.Len() == 0
	b.WriteString("$")
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
func clientWordValues(command string, request hookctx.Context, logger core.Logger) func(string) ([]string, bool) {
	return func(name string) ([]string, bool) {
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
		if inherited, ok := request.LookupEnv(name); ok {
			values = append(values, inherited)
		}
		return values, !dynamic && !loopDynamic && len(values) > 0
	}
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
	for _, dir := range tempDirs {
		if dir != "" && strings.HasPrefix(path, filepath.Clean(dir)+string(filepath.Separator)) {
			return ""
		}
	}
	return fmt.Sprintf("--debug-file %s пишет вне временного каталога (%s): там могут быть настройки, агенты и хуки", path, strings.Join(tempDirs, ", "))
}

// clientEnvironmentCandidates — отказ переменным claudeConfigEnvironment с
// непустым или неизвестным значением в команде с клиентом claude
func clientEnvironmentCandidates(command string, logger core.Logger) []modelCandidate {
	var found []modelCandidate
	for _, name := range claudeConfigEnvironment {
		values, dynamic := shellEnvValues(command, name, logger)
		if dynamic || slices.ContainsFunc(values, func(value string) bool { return value != "" }) {
			found = append(found, clientOptionCandidate(fmt.Sprintf("переменная %s у клиента claude снимает хуки или берёт настройки не из ~/.claude", name)))
		}
	}
	return found
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
