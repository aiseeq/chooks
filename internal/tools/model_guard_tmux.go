package tools

import (
	"context"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/aiseeq/akit/shellcmd"
	"github.com/aiseeq/chooks/internal/core"
	"github.com/aiseeq/chooks/internal/hookctx"
	"mvdan.cc/sh/v3/syntax"
)

// Текст, который tmux отдаст оболочке, model_guard проверяет той же проверкой,
// что и команду Bash: из вызовов tmux собирается скрипт, и он судится как
// команда агента. В скрипт идут команда панели (new-session, new-window,
// split-window, respawn-*), run-shell, display-popup, pipe-pane, условие
// if-shell, default-command, глобальная -c, набранное send-keys и #() форматов.
// Окружение панели (-e, set-environment) становится export, каталог (-c, у
// display-popup -d) — cd, а команды tmux, которые выполнятся позже или по
// условию (if-shell, run-shell -C, bind-key, set-hook, хуки в set-option,
// шаблоны command-prompt, confirm-before, choose-*, display-menu), — строкой
// tmux, которую разбирает следующий круг. Всё это — один скрипт: send-keys
// набирает текст в оболочке, которую запустила команда панели или
// default-command с подменённым окружением, а проверка окружения claude судит
// присваивания по всему тексту. Текст, который не разобрать, с claude —
// отказ. Опции tmux, которые ничего не исполняют, не трогаются

// modelCandidates — кандидаты команды и текстов, которые она отдаёт оболочке
// через tmux: текст судится как команда агента
func modelCandidates(ctx context.Context, input *core.ToolInput, logger core.Logger) []modelCandidate {
	found := commandModelCandidates(ctx, input, logger)
	if !core.IsShellTool(input.ToolName) {
		return found
	}
	home, _ := hookctx.UserHomeDir(ctx) //nolint:ignored-error // ошибку записал и отказал commandModelCandidates, пустой home отказывает HOME через tmux
	for _, script := range tmuxShellScripts(input.Command, logger) {
		inner := *input
		inner.Command = script.text
		for _, candidate := range commandModelCandidates(ctx, &inner, logger) {
			if candidate.clientOption != "" {
				candidate = clientOptionCandidate("tmux отдаст оболочке текст, где " + candidate.clientOption)
			}
			found = append(found, candidate)
		}
		found = append(found, tmuxUnparsedCandidates(script, home)...)
	}
	return found
}

// validateTmuxShellPrefix — префикс хуков в тексте, который команда отдаёт
// оболочке через tmux
func (t *ModelGuardTool) validateTmuxShellPrefix(input *core.ToolInput) *core.ValidationResult {
	if !core.IsShellTool(input.ToolName) {
		return nil
	}
	for _, script := range tmuxShellScripts(input.Command, t.Logger()) {
		inner := *input
		inner.Command = script.text
		if result := t.validateShellPrefix(&inner); result != nil {
			return result
		}
	}
	return nil
}

// tmuxNestingLimit — сколько кругов «tmux в тексте tmux» разбирается; глубже
// текст с claude — отказ
const tmuxNestingLimit = 4

// tmuxKind — что команда tmux исполняет
type tmuxKind int

const (
	// tmuxPane — shell-command [argument ...] новой панели; -e, -c
	tmuxPane tmuxKind = iota
	// tmuxRun — run-shell: оболочка или с -C команда tmux; -c
	tmuxRun
	// tmuxPopup — display-popup: -e, каталог -d
	tmuxPopup
	// tmuxIf — if-shell: условие оболочкой, ветви — команды tmux
	tmuxIf
	// tmuxPipe — pipe-pane: shell-command
	tmuxPipe
	// tmuxKeys — send-keys: набранное в панели
	tmuxKeys
	// tmuxOption — set-option, set-window-option: default-command и хуки
	tmuxOption
	// tmuxBound — set-hook, bind-key: имя хука или клавиша, затем команда
	// tmux
	tmuxBound
	// tmuxTemplate — первый операнд — команда tmux (command-prompt,
	// confirm-before, choose-*, display-panes)
	tmuxTemplate
	// tmuxMenu — display-menu: тройки «имя клавиша команда»
	tmuxMenu
	// tmuxEnvironment — set-environment
	tmuxEnvironment
)

type tmuxCommandSpec struct {
	kind tmuxKind
	// values — буквы опций со значением (man tmux 3.8)
	values string
}

// tmuxCommands — команды tmux, которые исполняют текст оболочкой или
// команды tmux; прочие судятся только по #() форматов
var tmuxCommands = map[string]tmuxCommandSpec{
	"new-session":       {tmuxPane, "cefFnstxy"},
	"new-window":        {tmuxPane, "ceFnt"},
	"split-window":      {tmuxPane, "BceFlmpRsStT"},
	"respawn-pane":      {tmuxPane, "cet"},
	"respawn-window":    {tmuxPane, "cet"},
	"run-shell":         {tmuxRun, "cdt"},
	"display-popup":     {tmuxPopup, "bcdehsStTwxy"},
	"if-shell":          {tmuxIf, "t"},
	"pipe-pane":         {tmuxPipe, "t"},
	"send-keys":         {tmuxKeys, "cNt"},
	"set-option":        {tmuxOption, "t"},
	"set-window-option": {tmuxOption, "t"},
	"set-hook":          {tmuxBound, "Bt"},
	"bind-key":          {tmuxBound, "NT"},
	"command-prompt":    {tmuxTemplate, "IptT"},
	"confirm-before":    {tmuxTemplate, "cpt"},
	"choose-tree":       {tmuxTemplate, "FfKOt"},
	"choose-client":     {tmuxTemplate, "FfKOt"},
	"choose-buffer":     {tmuxTemplate, "FfKOt"},
	"display-panes":     {tmuxTemplate, "dst"},
	"display-menu":      {tmuxMenu, "bcCHsStTxy"},
	"set-environment":   {tmuxEnvironment, "t"},
}

// tmuxAliases — алиасы команд из tmuxCommands
var tmuxAliases = map[string]string{
	"new": "new-session", "neww": "new-window", "splitw": "split-window", "respawnp": "respawn-pane",
	"respawnw": "respawn-window", "run": "run-shell", "popup": "display-popup", "if": "if-shell",
	"pipep": "pipe-pane", "send": "send-keys", "set": "set-option", "setw": "set-window-option",
	"bind": "bind-key", "confirm": "confirm-before", "displayp": "display-panes", "menu": "display-menu",
	"setenv": "set-environment",
}

// tmuxCommandName — полное имя команды: алиас, имя или однозначное начало
// имени среди tmuxCommands. Начало, которое tmux сочтёт неоднозначным из-за
// команды вне набора, судится как эта команда: лишний разбор, не пропуск
func tmuxCommandName(name string) (string, bool) {
	if full, ok := tmuxAliases[name]; ok {
		return full, true
	}
	if _, ok := tmuxCommands[name]; ok {
		return name, true
	}
	found := ""
	for full := range tmuxCommands {
		if name != "" && strings.HasPrefix(full, name) {
			if found != "" {
				return "", false
			}
			found = full
		}
	}
	return found, found != ""
}

// tmuxHookOption — имя опции-хука tmux (set-option after-new-window[0] …)
var tmuxHookOption = regexp.MustCompile(`^(?:after-[a-z-]+|alert-(?:activity|bell|silence)|client-[a-z-]+|command-error|pane-(?:died|exited|focus-in|focus-out|mode-changed|set-clipboard|title-changed)|session-(?:closed|created|renamed|window-changed)|window-(?:layout-changed|linked|pane-changed|renamed|resized|unlinked))(?:\[\d*\])?$`)

// tmuxKeyName — слово send-keys, которое tmux примет за имя клавиши: C-x,
// M-x, ^x, имена вроде Escape, F1, NPage
var tmuxKeyName = regexp.MustCompile(`(?i)^(?:(?:[CMS]-)+.|\^.|(?:(?:[CMS]-)*(?:enter|escape|tab|btab|space|bspace|up|down|left|right|home|end|ic|dc|insert|delete|npage|ppage|pagedown|pageup|pgdn|pgup|f[0-9]{1,2}|kp[a-z0-9*/+.-]*|any|mouse[a-z0-9]*|wheel[a-z]*)))$`)

// tmuxScript — то, что одна команда агента отдаёт оболочкам через tmux
type tmuxScript struct {
	lines []string
	typed strings.Builder
	env   tmuxEnvironmentFacts
}

// tmuxEnvironmentFacts — окружение, которое tmux задаёт командам, сверх
// export в тексте
type tmuxEnvironmentFacts struct {
	// loaders — переменные, которые меняют сам запуск (LD_*, PATH, BASH_ENV,
	// NODE_OPTIONS): проверка команды их у claude не судит
	loaders []string
	// homes — значения HOME из -e и set-environment
	homes []string
	// loose — HOME или окружение задано не словом -e HOME=…: set-environment
	// HOME, связка опций, панель словами argv, -e без =
	loose bool
}

// tmuxShellText — текст для проверки, переменные-загрузчики его окружения и
// признак, что круги tmux кончились, а в тексте ещё есть tmux
type tmuxShellText struct {
	text    string
	env     tmuxEnvironmentFacts
	tooDeep bool
}

// tmuxShellScripts — тексты, которые tmux из команды отдаст оболочке, по
// кругам вложенности. Скрипт, который не разобрать, делится на части: так
// неразобранный набор send-keys без claude не прячет команду панели
func tmuxShellScripts(command string, logger core.Logger) []tmuxShellText {
	var out []tmuxShellText
	pending := []string{command}
	for depth := 0; len(pending) > 0; depth++ {
		var next []string
		for _, text := range pending {
			for _, script := range tmuxScriptsOf(text, logger) {
				script.tooDeep = depth >= tmuxNestingLimit
				out = append(out, script)
				if !script.tooDeep {
					next = append(next, script.text)
				}
			}
		}
		pending = next
	}
	return out
}

// tmuxScriptsOf — скрипт вызовов tmux одной команды: целиком, а если он не
// разбирается — частями
func tmuxScriptsOf(command string, logger core.Logger) []tmuxShellText {
	if !strings.Contains(command, "tmux") {
		return nil
	}
	calls, err := shellcmd.Calls(command)
	//nolint:fallback-return // при ошибке во вложенном скрипте shellcmd отдаёт вызовы, которые разобрал: неразобранный текст с claude судит unparsedClientCandidates, а найденные tmux проверяются здесь, чтобы чужая ошибка их не прятала
	if err != nil {
		logger.Warn("model_guard: команда с tmux разобрана не целиком, проверены найденные вызовы", "error", err)
	}
	var script tmuxScript
	for i := range calls {
		if calls[i].Name == "tmux" {
			script.call(calls[i].Args)
		}
	}
	parts := slices.Clone(script.lines)
	if typed := script.typed.String(); typed != "" {
		parts = append(parts, typed)
	}
	if len(parts) == 0 {
		return nil
	}
	assignments := tmuxOuterAssignments(command)
	whole := assignments + strings.Join(parts, "\n")
	if _, err := syntax.NewParser().Parse(strings.NewReader(whole), ""); err == nil {
		return []tmuxShellText{{text: whole, env: script.env}}
	}
	out := make([]tmuxShellText, len(parts))
	for i, part := range parts {
		out[i] = tmuxShellText{text: assignments + part, env: script.env}
	}
	return out
}

// tmuxOuterAssignments — присваивания и объявления (export, declare, local,
// readonly, typeset) команды, по строке: текст, который tmux получит, раскрыт
// оболочкой команды, и подстановки в нём ($C, "$CMD") проверка видит с их
// значениями. Окружение, заданное командой, наследует и сервер tmux, который
// она запускает. Берутся все: присваивание ниже по тексту может исполниться
// раньше вызова tmux (функция, цикл)
func tmuxOuterAssignments(command string) string {
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	//nolint:error-masking,silent-error-handling // присваивания неразобранной команды не видны: её с claude судит unparsedClientCandidates, а текст tmux — tmuxUnparsedCandidates
	if err != nil {
		return ""
	}
	var b strings.Builder
	syntax.Walk(file, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.CallExpr:
			// X=1 cmd задаёт X только cmd
			if len(n.Args) == 0 {
				b.WriteString(command[n.Pos().Offset():n.End().Offset()] + "\n")
			}
		case *syntax.DeclClause:
			b.WriteString(command[n.Pos().Offset():n.End().Offset()] + "\n")
		}
		return true
	})
	return b.String()
}

// call — один вызов tmux: глобальные опции, затем команды через ;
func (s *tmuxScript) call(raw []string) {
	args := make([]string, len(raw))
	for i, arg := range raw {
		args[i] = tmuxArgText(arg)
		s.formats(args[i])
	}
	flags, rest := tmuxFlags(args, "cfLST")
	for _, text := range flags['c'] {
		s.lines = append(s.lines, text)
	}
	for _, command := range tmuxCommandList(rest) {
		s.command(command)
	}
}

// formats — #() в слове: tmux исполняет его оболочкой, когда раскрывает
// формат (display -p, -F, строка статуса)
func (s *tmuxScript) formats(text string) {
	for {
		start := strings.Index(text, "#(")
		if start < 0 {
			return
		}
		depth, end := 0, -1
		for i := start + 1; i < len(text) && end < 0; i++ {
			switch text[i] {
			case '(':
				depth++
			case ')':
				depth--
				if depth == 0 {
					end = i
				}
			}
		}
		if end < 0 {
			s.lines = append(s.lines, text[start+2:])
			return
		}
		s.lines = append(s.lines, text[start+2:end])
		text = text[end+1:]
	}
}

// command — одна команда tmux из цепочки
func (s *tmuxScript) command(words []string) {
	if len(words) == 0 {
		return
	}
	name, ok := tmuxCommandName(words[0])
	if !ok {
		return
	}
	spec := tmuxCommands[name]
	flags, operands := tmuxFlags(words[1:], spec.values)
	switch spec.kind {
	case tmuxPane:
		s.environment(words, flags['e'], len(operands) > 1)
		s.directory(flags['c'])
		s.shellCommand(operands)
	case tmuxRun:
		s.directory(flags['c'])
		if _, ok := flags['C']; ok {
			s.tmuxCommand(operands)
		} else {
			s.shellCommand(operands)
		}
	case tmuxPopup:
		s.environment(words, flags['e'], len(operands) > 1)
		s.directory(flags['d'])
		s.shellCommand(operands)
	case tmuxIf:
		s.shellCommand(operands[:min(1, len(operands))])
		for _, branch := range operands[min(1, len(operands)):] {
			s.tmuxCommand([]string{branch})
		}
	case tmuxPipe:
		s.shellCommand(operands)
	case tmuxKeys:
		s.keys(flags, operands)
	case tmuxOption:
		s.option(operands)
	case tmuxBound:
		s.tmuxCommand(operands[min(1, len(operands)):])
	case tmuxTemplate:
		s.tmuxCommand(operands[:min(1, len(operands))])
	case tmuxMenu:
		for i := 2; i < len(operands); i += 3 {
			s.tmuxCommand(operands[i : i+1])
		}
	case tmuxEnvironment:
		s.setEnvironment(operands)
	}
}

// shellCommand — shell-command [argument ...]: одно слово tmux отдаёт
// sh -c, несколько — запускает как argv
func (s *tmuxScript) shellCommand(operands []string) {
	s.line("", operands)
}

// tmuxCommand — команда tmux строкой для следующего круга. Одно слово tmux
// разбирает своим разбором, похожим на разбор оболочки: ; и перевод строки
// делят его на команды, и каждая идёт своей строкой tmux, иначе оболочка
// следующего круга сочла бы вторую своей командой. Несколько слов — это уже
// слова одной команды
func (s *tmuxScript) tmuxCommand(operands []string) {
	if len(operands) == 1 {
		if file, err := syntax.NewParser().Parse(strings.NewReader(operands[0]), ""); err == nil {
			for _, stmt := range file.Stmts {
				text := operands[0][stmt.Pos().Offset():stmt.End().Offset()]
				s.lines = append(s.lines, "tmux "+text)
			}
			return
		}
	}
	s.line("tmux ", operands)
}

// line — строка скрипта из операндов после prefix: одно слово — текстом,
// несколько — словами в кавычках, ни одного — строки нет
func (s *tmuxScript) line(prefix string, operands []string) {
	switch len(operands) {
	case 0:
	case 1:
		s.lines = append(s.lines, prefix+operands[0])
	default:
		quoted := make([]string, len(operands))
		for i, operand := range operands {
			quoted[i] = singleQuotedWord(operand)
		}
		s.lines = append(s.lines, prefix+strings.Join(quoted, " "))
	}
}

// environment — -e NAME=VALUE: окружение команды панели. HOME без отказа —
// только словом -e HOME=… (или -eHOME=…) при команде одним словом; слово без
// = tmux описывает только как NAME=VALUE
func (s *tmuxScript) environment(words, values []string, argv bool) {
	for _, value := range values {
		name, assigned, ok := strings.Cut(value, "=")
		if !ok {
			s.env.loose = true
			continue
		}
		if name == "HOME" {
			s.env.homes = append(s.env.homes, assigned)
			s.env.loose = s.env.loose || argv || slices.ContainsFunc(words, tmuxEnvironmentCluster)
		}
		s.export(name, assigned)
	}
}

// tmuxEnvironmentCluster — -e в связке опций после другой буквы (-de)
func tmuxEnvironmentCluster(word string) bool {
	return len(word) > 2 && word[0] == '-' && word[1] != 'e' && word[1] != '-' && strings.ContainsRune(word[2:], 'e')
}

// export — переменная окружения команд tmux: имя литералом, значение в
// кавычках, чтобы проверка видела присваивание
func (s *tmuxScript) export(name, value string) {
	// HOME и модель судит проверка текста по значению
	if _, model := modelEnvNames[name]; unsafeEnvironmentName(name) && !model && name != "HOME" {
		s.env.loaders = append(s.env.loaders, name)
	}
	if syntax.ValidName(name) {
		s.lines = append(s.lines, "export "+name+"="+singleQuotedWord(value))
		return
	}
	s.lines = append(s.lines, "export "+singleQuotedWord(name+"="+value))
}

// directory — каталог запуска: настройки проекта claude читает оттуда
func (s *tmuxScript) directory(values []string) {
	for _, value := range values {
		s.lines = append(s.lines, "cd "+singleQuotedWord(value))
	}
}

// setEnvironment — set-environment NAME [VALUE]: окружение новых панелей.
// -u и -r (снять переменную) судятся как пустое значение: HOME — отказ, как
// env -u HOME, переменная claude — как её присваивание. Без имени tmux
// команду не исполняет
func (s *tmuxScript) setEnvironment(operands []string) {
	if len(operands) == 0 {
		return
	}
	s.env.loose = s.env.loose || operands[0] == "HOME"
	value := ""
	if len(operands) > 1 {
		value = operands[1]
	}
	s.export(operands[0], value)
}

// option — set-option NAME VALUE: default-command исполняет оболочка новых
// панелей, хук — tmux
func (s *tmuxScript) option(operands []string) {
	if len(operands) < 2 {
		return
	}
	switch {
	case operands[0] == "default-command":
		s.lines = append(s.lines, operands[1])
	case tmuxHookOption.MatchString(operands[0]):
		s.tmuxCommand(operands[1:2])
	}
}

// keys — send-keys: слово — имя клавиши или набираемый текст (-l — всё
// текст, -H — байты шестнадцатеричными числами, -X — команда режима
// копирования, не набор). Space и Tab — свои знаки; Enter отдаёт строку
// оболочке, прочие клавиши (C-c, Escape) обрывают её — тоже конец строки
func (s *tmuxScript) keys(flags map[byte][]string, operands []string) {
	if _, ok := flags['X']; ok {
		return
	}
	_, literal := flags['l']
	_, hex := flags['H']
	for _, key := range operands {
		switch {
		case hex:
			s.typed.WriteString(tmuxHexKey(key))
		case literal || !tmuxKeyName.MatchString(key):
			s.typed.WriteString(key)
		case strings.EqualFold(key, "space"):
			s.typed.WriteString(" ")
		case strings.EqualFold(key, "tab"):
			s.typed.WriteString("\t")
		default:
			s.typed.WriteString("\n")
		}
	}
}

// tmuxHexKey — байт send-keys -H (шестнадцатеричное число до ff, как
// strtol в tmux); прочее tmux отвергает, оно остаётся как есть
func tmuxHexKey(key string) string {
	if b, err := strconv.ParseUint(key, 16, 8); err == nil {
		return string([]byte{byte(b)})
	}
	return key
}

// tmuxFlags — опции команды tmux по правилам getopt: связка букв, значение
// буквы из values — остаток связки или следующее слово; разбор кончается на
// первом слове не с «-», на «-» и после «--»
func tmuxFlags(args []string, values string) (map[byte][]string, []string) {
	flags := map[byte][]string{}
	i := 0
	for ; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			i++
			break
		}
		if len(a) < 2 || a[0] != '-' {
			break
		}
		for j := 1; j < len(a); j++ {
			c := a[j]
			if strings.IndexByte(values, c) < 0 {
				flags[c] = append(flags[c], "")
				continue
			}
			value := a[j+1:]
			if value == "" && i+1 < len(args) {
				i++
				value = args[i]
			}
			flags[c] = append(flags[c], value)
			break
		}
	}
	return flags, args[min(i, len(args)):]
}

// tmuxCommandList — команды tmux через ;: отдельное слово ;, слово на ; (не
// \;) — конец команды
func tmuxCommandList(args []string) [][]string {
	var out [][]string
	var current []string
	for _, a := range args {
		body, ends := strings.CutSuffix(a, ";")
		switch {
		case !ends:
			current = append(current, a)
		case strings.HasSuffix(body, `\`):
			current = append(current, strings.TrimSuffix(body, `\`)+";")
		default:
			if body != "" {
				current = append(current, body)
			}
			out = append(out, current)
			current = nil
		}
	}
	return append(out, current)
}

// tmuxArgText — слово, которое получит tmux. Литеральное слово shellcmd
// отдаёт значением, слово с подстановкой — исходником: у него снимаются
// кавычки, а подстановки остаются как есть, и проверка текста видит их
// неизвестными
func tmuxArgText(arg string) string {
	if !strings.ContainsAny(arg, "$`") {
		return arg
	}
	var words []*syntax.Word
	for w, err := range syntax.NewParser().WordsSeq(strings.NewReader(arg)) {
		//nolint:silent-error-handling // неразобранное слово tmux получит как есть, и текст с claude в нём судит tmuxUnparsedCandidates
		if err != nil {
			return arg
		}
		words = append(words, w)
	}
	if len(words) != 1 {
		return arg
	}
	var b strings.Builder
	tmuxWordParts(&b, arg, words[0].Parts)
	return b.String()
}

func tmuxWordParts(b *strings.Builder, source string, parts []syntax.WordPart) {
	for _, part := range parts {
		switch p := part.(type) {
		case *syntax.SglQuoted:
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			tmuxWordParts(b, source, p.Parts)
		default:
			b.WriteString(source[p.Pos().Offset():p.End().Offset()])
		}
	}
}

// tmuxClientWord — claude, claude-code или cl словом в тексте
var tmuxClientWord = regexp.MustCompile(`(^|[^[:alnum:]_.-])(claude-code|claude|cl)([^[:alnum:]_.-]|$)`)

// tmuxUnparsedCandidates — текст, который tmux отдаст оболочке, не разобран
// (или вложен глубже tmuxNestingLimit), а в нём claude: его опции и
// окружение не проверить
func tmuxUnparsedCandidates(script tmuxShellText, home string) []modelCandidate {
	if !tmuxClientText(script.text) {
		return nil
	}
	var found []modelCandidate
	if len(script.env.loaders) > 0 {
		found = append(found, clientOptionCandidate("tmux задаёт команде с claude переменную "+strings.Join(script.env.loaders, ", ")+": она меняет сам запуск клиента (загрузчик, оболочка, node), и хуки claude не проверить; убери её из -e и set-environment"))
	}
	if script.env.loose || len(script.env.homes) > 0 && (slices.ContainsFunc(script.env.homes, func(value string) bool { return value != home }) || !tmuxPrintClients(script.text)) {
		found = append(found, clientOptionCandidate("tmux задаёт окружение команде с claude (HOME, -e без =): настройки и хуки claude не проверить; HOME через tmux допустим только словом -e HOME=<домашний каталог> у claude -p"))
	}
	if !script.tooDeep {
		if _, err := syntax.NewParser().Parse(strings.NewReader(script.text), ""); err == nil {
			return found
		}
	}
	return append(found, clientOptionCandidate("текст, который tmux отдаст оболочке, не разобран, а в нём есть claude: его опции и окружение не проверить"))
}

// tmuxPrintClients — все вызовы claude и cl в тексте с -p или --print
func tmuxPrintClients(text string) bool {
	calls, err := shellcmd.Calls(text)
	if err != nil {
		// error-masked-as-false-bool: safe — false здесь отказ: HOME через tmux пропускается только при разобранных вызовах
		return false
	}
	for i := range calls {
		if (calls[i].Name == "claude" || calls[i].Name == "cl") && !slices.Contains(calls[i].Args, "-p") && !slices.Contains(calls[i].Args, "--print") {
			return false
		}
	}
	return true
}

// tmuxClientText — в тексте claude, claude-code или cl, и не словом echo
// или printf
func tmuxClientText(text string) bool {
	for _, match := range tmuxClientWord.FindAllStringSubmatchIndex(text, -1) {
		if !printedMention(text, match[4]) {
			return true
		}
	}
	return false
}
