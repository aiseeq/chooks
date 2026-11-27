package tools

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/aiseeq/akit/shellcmd"
	"github.com/aiseeq/chooks/internal/core"
	"mvdan.cc/sh/v3/syntax"
)

// Источники модели и хуков claude мимо его argv, которые видны в самой
// команде агента: файл настроек каталога запуска, запись такого файла
// инструментом Write или Edit, имя вызова, которое не равно claude
// (запускатель пакета, бинарь версии, screen, имя из подстановки).
// Ввод stream-json судит scanClaudeArguments (streamJSONRefusal). Смену
// модели и окружения отдельным вызовом (tmux set-buffer, send-keys
// '/model …') текстом одной команды не увидеть: это остаток проверки

// hookSettingsReasons — чем настройки raw снимают хуки claude:
// disableAllHooks: true (hookSwitchOn) или переменная env из
// claudeEnvironmentEffects. По коду бинаря 2.1.332 disableAllHooks
// неуправляемых настроек оставляет только управляемые хуки (/etc/claude-code);
// allowManagedHooksOnly действует только из управляемых настроек
// (policySettings), здесь его нет. Файл не JSON claude не примет — причин нет
func hookSettingsReasons(raw []byte) []string {
	var settings map[string]json.RawMessage
	if json.Unmarshal(raw, &settings) != nil {
		return nil
	}
	var reasons []string
	if hookSwitchOn(settings["disableAllHooks"]) {
		reasons = append(reasons, "disableAllHooks снимает хуки claude (остаются только управляемые)")
	}
	var env map[string]json.RawMessage
	if json.Unmarshal(settings["env"], &env) == nil {
		for _, name := range slices.Sorted(func(yield func(string) bool) {
			for name := range env {
				if !yield(name) {
					return
				}
			}
		}) {
			if effect, ok := claudeEnvironmentEffects[name]; ok {
				reasons = append(reasons, fmt.Sprintf("env %s %s", name, effect))
			}
		}
	}
	return reasons
}

// hookSwitchOn — disableAllHooks включён: загрузчик хуков бинаря 2.1.332
// сравнивает слитые неуправляемые настройки строго, disableAllHooks===true;
// "true", 1 и прочее хуки не снимают (нестрогое po() — только у
// управляемых настроек родителя)
func hookSwitchOn(raw json.RawMessage) bool {
	return strings.TrimSpace(string(raw)) == "true"
}

// hookSettingsCandidates — отказ файлу настроек, который прочтёт claude этой
// команды (каталог запуска, ~/.claude, CLAUDE_CONFIG_DIR), если файл снимает
// хуки
func hookSettingsCandidates(path string, raw []byte) []modelCandidate {
	var found []modelCandidate
	for _, reason := range hookSettingsReasons(raw) {
		found = append(found, clientOptionCandidate(fmt.Sprintf("настройки %s: %s; claude, запущенный с ними, работает без хуков chooks — убери ключ из файла или запусти claude из другого каталога", path, reason)))
	}
	return found
}

// launchSettingsPaths — файлы настроек проекта, которые прочтёт claude,
// запущенный в cwd, по коду бинаря 2.1.332 (FQn, lmd, fo): projectSettings —
// <cwd>/.claude/settings.json, без подъёма к родителям; localSettings —
// settings.local.json в каноническом git-корне (uW: у worktree — главный
// репозиторий, кроме корня, равного дому) и в самом cwd, если он другой.
// Проверку владельца корня (amd) хук не повторяет: корень судится всегда
func launchSettingsPaths(cwd string, homes []string, logger core.Logger) []string {
	cwd = filepath.Clean(cwd)
	paths := []string{filepath.Join(cwd, ".claude", "settings.json"), filepath.Join(cwd, ".claude", "settings.local.json")}
	root, err := canonicalGitRoot(cwd)
	if err != nil {
		logger.Warn("model_guard: git-корень каталога запуска не найден, его settings.local.json не проверен", "cwd", cwd, "error", err)
		return paths
	}
	if root != "" && root != cwd && !slices.Contains(homes, root) {
		paths = append(paths, filepath.Join(root, ".claude", "settings.local.json"))
	}
	return paths
}

// canonicalGitRoot — канонический git-корень dir без запуска git: ближний
// вверх каталог с .git; у файла .git (worktree) — главный репозиторий из
// gitdir: и commondir. "" без ошибки — dir не в репозитории
func canonicalGitRoot(dir string) (string, error) {
	for {
		dotGit := filepath.Join(dir, ".git")
		info, err := os.Lstat(dotGit)
		switch {
		case err == nil && info.IsDir():
			return dir, nil
		case err == nil:
			return worktreeMainRoot(dir, dotGit)
		case !errors.Is(err, fs.ErrNotExist):
			return "", err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", nil
		}
		dir = parent
	}
}

// worktreeMainRoot — корень главного репозитория worktree dir по файлу
// .git (gitdir: …) и файлу commondir его каталога git; без commondir
// (подмодуль) — сам dir
func worktreeMainRoot(dir, dotGit string) (string, error) {
	raw, err := os.ReadFile(dotGit)
	if err != nil {
		return "", fmt.Errorf("файл .git worktree: %w", err)
	}
	gitDir, ok := strings.CutPrefix(strings.TrimSpace(string(raw)), "gitdir:")
	if !ok {
		return "", fmt.Errorf("%s: нет строки gitdir:", dotGit)
	}
	gitDir = resolveFrom(strings.TrimSpace(gitDir), dir)
	common, err := os.ReadFile(filepath.Join(gitDir, "commondir"))
	if errors.Is(err, fs.ErrNotExist) {
		return dir, nil
	}
	if err != nil {
		return "", fmt.Errorf("commondir worktree %s: %w", dir, err)
	}
	commonDir := resolveFrom(strings.TrimSpace(string(common)), gitDir)
	if filepath.Base(commonDir) != ".git" {
		return dir, nil
	}
	return filepath.Dir(commonDir), nil
}

// resolveFrom — путь path, относительный — от каталога base
func resolveFrom(path, base string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(base, path)
}

// settingsFileTools — инструменты записи файла, которые судит model_guard
var settingsFileTools = []string{"Write", "Edit"}

// claudeSettingsFile — путь — файл настроек claude: settings.json или
// settings.local.json в каталоге .claude (проект, ~/.claude) или в
// CLAUDE_CONFIG_DIR
func claudeSettingsFile(path, configDir string) bool {
	base := filepath.Base(path)
	if base != "settings.json" && base != "settings.local.json" {
		return false
	}
	dir := filepath.Dir(path)
	return filepath.Base(dir) == ".claude" || configDir != "" && dir == filepath.Clean(configDir)
}

// fileToolInput — поля Write и Edit
type fileToolInput struct {
	FilePath   string `json:"file_path"`
	Content    string `json:"content"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

// settingsWriteCandidates — Write или Edit файла настроек claude: содержимое
// после записи судится как настройки — модель и снятие хуков. У Edit оно —
// файл, каким его оставит Edit (editedContent)
func settingsWriteCandidates(input *core.ToolInput, configDir string, agent func(string) []modelCandidate, logger core.Logger) []modelCandidate {
	if len(input.ToolInput) == 0 {
		return nil
	}
	var fields fileToolInput
	if err := json.Unmarshal(input.ToolInput, &fields); err != nil {
		// claude проверяет поля инструмента по схеме до хука: сюда доходит
		// только сломанный ввод, и он может писать файл настроек
		logger.Warn("model_guard: поля записи файла не разобраны", "tool", input.ToolName, "error", err)
		return []modelCandidate{clientOptionCandidate(fmt.Sprintf("поля %s не разобраны (%v): запись файла настроек claude не проверить", input.ToolName, err))}
	}
	if fields.FilePath == "" {
		return nil
	}
	path := fields.FilePath
	if !filepath.IsAbs(path) && input.CWD != "" {
		path = filepath.Join(input.CWD, path)
	}
	path = filepath.Clean(path)
	if !claudeSettingsFile(path, configDir) {
		return nil
	}
	content := fields.Content
	if input.ToolName == "Edit" {
		edited, ok := editedContent(path, fields, logger)
		if !ok {
			return nil
		}
		content = edited
	}
	found := settingsCandidates([]byte(content), agent)
	for _, reason := range hookSettingsReasons([]byte(content)) {
		found = append(found, clientOptionCandidate(fmt.Sprintf("запись %s: %s; убери ключ — claude, запущенный с этими настройками, работает без хуков chooks", path, reason)))
	}
	return found
}

// editedContent — файл после Edit, как его выполнит claude: пустой
// old_string создаёт файл с new_string, если файла нет или в нём одни
// пробелы, иначе Edit откажет; непустой заменяет первое вхождение (или все
// при replace_all). false — Edit не выполнится, судить нечего
func editedContent(path string, fields fileToolInput, logger core.Logger) (string, bool) {
	current, err := os.ReadFile(path)
	missing := errors.Is(err, fs.ErrNotExist)
	if err != nil && !missing {
		logger.Debug("model_guard: правимый файл настроек не прочитан, Edit не выполнится", "path", path, "error", err)
		return "", false
	}
	if fields.OldString == "" {
		return fields.NewString, strings.TrimSpace(string(current)) == ""
	}
	if !strings.Contains(string(current), fields.OldString) {
		return "", false
	}
	count := 1
	if fields.ReplaceAll {
		count = -1
	}
	return strings.Replace(string(current), fields.OldString, fields.NewString, count), true
}

// claudeCodePackage — спецификация пакета claude-code: имя с версией или
// меткой после @ или без них
func claudeCodePackage(spec string) bool {
	return spec == "@anthropic-ai/claude-code" || strings.HasPrefix(spec, "@anthropic-ai/claude-code@")
}

// packageRunners — запускатели пакетов и их подкоманды, после которых идёт
// пакет или bin (nil — пакет сразу после опций)
var packageRunners = map[string][]string{
	"npx": nil, "bunx": nil, "pnpx": nil,
	"pnpm": {"dlx", "exec"}, "npm": {"exec", "x"}, "yarn": {"dlx", "exec"}, "bun": {"x"},
}

// packageRunnerClaude — индекс первого аргумента claude в вызове
// запускателя пакета: первый операнд после опций — пакет claude-code (с
// версией) или bin claude (npx -p <пакет> claude; значение -p/--package
// операндом не считается). -c/--call исполняет команду оболочки — не
// клиент. За операндом снимаются bin claude и «--» (npm exec <пакет> --
// <аргументы>)
func packageRunnerClaude(name string, args []string) (int, bool) {
	subcommands, ok := packageRunners[name]
	if !ok {
		return 0, false
	}
	start := 0
	if subcommands != nil {
		if len(args) == 0 || !slices.Contains(subcommands, args[0]) {
			return 0, false
		}
		start = 1
	}
	at := -1
scan:
	for i := start; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-c" || arg == "--call" || strings.HasPrefix(arg, "--call="):
			// команда оболочки вместо bin — не клиент
			return 0, false
		case arg == "-p" || arg == "--package":
			i++
		case arg == "--" || strings.HasPrefix(arg, "-"):
		case claudeCodePackage(arg) || arg == "claude":
			at = i
			break scan
		default:
			return 0, false
		}
	}
	if at < 0 {
		return 0, false
	}
	next := at + 1
	for next < len(args) && (args[next] == "claude" || args[next] == "--") && next <= at+2 {
		next++
	}
	return next, true
}

// screenValueOptions — буквы опций screen, которые берут значение
const screenValueOptions = "cehpSTts"

// screenProgram — индекс программы, которую запускает screen [опции]; -X и
// -Q шлют команду уже запущенному screen — программы нет
func screenProgram(args []string) (int, bool) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-X" || arg == "-Q":
			return 0, false
		case arg == "-Logfile":
			i++
		case strings.HasPrefix(arg, "-") && len(arg) > 1:
			letters := arg[1:]
			if at := strings.IndexAny(letters, screenValueOptions); at >= 0 && at == len(letters)-1 {
				i++
			}
		default:
			return i, true
		}
	}
	return 0, false
}

// claudeVersionName — имя бинаря версии claude
// (~/.local/share/claude/versions/2.1.332)
var claudeVersionName = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+[0-9A-Za-z.+-]*$`)

// clientInvocation — диалект клиента модели в вызове и индекс первого
// аргумента клиента в args: имя из modelClients, бинарь версии claude,
// запускатель пакета claude-code, программа за screen
func clientInvocation(name string, args []string, raw string) (string, int, bool) {
	if slices.Contains(modelClients, name) {
		return clientDialect(name), 0, true
	}
	if claudeVersionName.MatchString(name) && strings.Contains(raw, "/claude/versions/"+name) {
		return "claude", 0, true
	}
	if offset, ok := packageRunnerClaude(name, args); ok {
		return "claude", offset, true
	}
	if name == "screen" {
		if i, ok := screenProgram(args); ok {
			if dialect, offset, ok := clientInvocation(filepath.Base(args[i]), args[i+1:], args[i]); ok {
				return dialect, i + 1 + offset, true
			}
		}
	}
	return "", 0, false
}

// computedClientCall — вызов, имя программы которого — подстановка или
// переменная ("$(command -v claude)", `which claude`, $B после
// B=$(readlink -f "$(which claude)"), $b цикла for b in $(which -a claude)),
// а её текст или текст присваивания переменной называет клиента модели.
// Возвращает вызов того же текста с именем клиента вместо слова программы:
// его судит тот же разбор, что прямой вызов. nil без ошибки — вызов не
// клиента
func computedClientCall(call *shellcmd.Call, command string, logger core.Logger) (*shellcmd.Call, error) {
	if call.Name != "" && !strings.ContainsAny(call.Name, "$`") {
		return nil, nil
	}
	program, err := computedProgramWord(call)
	if program == nil || err != nil {
		return nil, err
	}
	client := computedClientName(call.Raw, command, program, logger)
	if client == "" {
		return nil, nil
	}
	rewritten := call.Raw[:program.Pos().Offset()] + client + call.Raw[program.End().Offset():]
	calls, err := shellcmd.Calls(rewritten)
	if err != nil {
		return nil, fmt.Errorf("вызов %q с именем %s не разобран: %w", rewritten, client, err)
	}
	for i := range calls {
		if calls[i].Name == client && slices.Equal(calls[i].Args, call.Args) {
			return &calls[i], nil
		}
	}
	return nil, nil
}

// computedProgramWord — слово программы вызова в его тексте: перед словами
// аргументов первой команды текста, где их хватает; nil без ошибки — такого
// слова нет. Литерал computedClientName не назовёт клиентом, кроме самого
// имени клиента, — тогда разбор с ним тот же
func computedProgramWord(call *shellcmd.Call) (*syntax.Word, error) {
	file, err := syntax.NewParser().Parse(strings.NewReader(call.Raw), "")
	if err != nil {
		return nil, fmt.Errorf("текст вызова %q не разобран: %w", call.Raw, err)
	}
	var program *syntax.Word
	syntax.Walk(file, func(node syntax.Node) bool {
		expr, ok := node.(*syntax.CallExpr)
		if program != nil || !ok {
			return program == nil
		}
		at := len(expr.Args) - len(call.Args) - 1
		if at < 0 {
			return true
		}
		program = expr.Args[at]
		return false
	})
	return program, nil
}

// mentionedClient — клиент модели, который называет текст отдельным словом
func mentionedClient(text string) string {
	if m := modelClientWord.FindStringSubmatch(text); m != nil {
		return m[2]
	}
	return ""
}

// simpleParamName — имя переменной слова $X, ${X}, "$X", в том числе с
// модификатором (${X%…}, ${X:-…}): значение — то же имя или его часть
func simpleParamName(w *syntax.Word) string {
	parts := w.Parts
	if len(parts) == 1 {
		if quoted, ok := parts[0].(*syntax.DblQuoted); ok {
			parts = quoted.Parts
		}
	}
	if len(parts) != 1 {
		return ""
	}
	param, ok := parts[0].(*syntax.ParamExp)
	if !ok || param.Param == nil {
		return ""
	}
	return param.Param.Value
}

// variableClient — клиент модели, которого называет значение переменной
// name в command: текст записи значения (variableWriterTexts); значение —
// ссылка на другую переменную (C2=$C) разбирается дальше, до variableDepth
// шагов. Значение с подстановкой clientWordValues и shellcmd.Assigns не
// отдают (Unknown без текста), поэтому нужен текст записи
func variableClient(command, name string, depth int, logger core.Logger) string {
	for _, text := range variableWriterTexts(command, name, 0, logger) {
		if client := mentionedClient(text); client != "" {
			return client
		}
		if m := variableReference.FindStringSubmatch(text); m != nil && depth < variableDepth {
			if client := variableClient(command, m[1], depth+1, logger); client != "" {
				return client
			}
		}
	}
	return ""
}

// variableReference — значение — ссылка на переменную целиком: $C, ${C}, "$C"
var variableReference = regexp.MustCompile(`^"?\$\{?([A-Za-z_][A-Za-z0-9_]*)\}?"?$`)

// variableDepth — предел цепочки ссылок C2=$C и вложенных оболочек
const variableDepth = 4

// variableWriterTexts — тексты, из которых script пишет переменную name:
// присваивание (и в export/declare/local), список for name in, аргументы
// printf -v name, перенаправления read … name (< <(…), <<<…); во вложенных
// скриптах оболочки (bash -c) — так же
func variableWriterTexts(script, name string, depth int, logger core.Logger) []string {
	file, err := syntax.NewParser().Parse(strings.NewReader(script), "")
	if err != nil {
		logger.Debug("model_guard: скрипт не разобран, запись переменной не найдена", "name", name, "error", err)
		return nil
	}
	text := func(n syntax.Node) string {
		return script[n.Pos().Offset():n.End().Offset()]
	}
	var texts []string
	syntax.Walk(file, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.Assign:
			if n.Name != nil && n.Name.Value == name && n.Value != nil {
				texts = append(texts, text(n.Value))
			}
		case *syntax.ForClause:
			if iter, ok := n.Loop.(*syntax.WordIter); ok && iter.Name.Value == name {
				for _, item := range iter.Items {
					texts = append(texts, text(item))
				}
			}
		case *syntax.Stmt:
			texts = append(texts, builtinWriterTexts(n, name, text)...)
		}
		return true
	})
	if depth >= variableDepth {
		return texts
	}
	calls, err := shellcmd.Calls(script)
	// fallback-return: safe — shellcmd отдаёт вызовы, разобранные до ошибки; их вложенные скрипты дают больше текстов записи, отказ не теряется
	if err != nil {
		logger.Warn("model_guard: вложенные скрипты разобраны не все, запись переменной ищется в разобранных", "name", name, "error", err)
	}
	for i := range calls {
		if shellcmd.IsShell(calls[i].Name) {
			for _, nested := range shellCommandStrings(calls[i].Args) {
				texts = append(texts, variableWriterTexts(nested, name, depth+1, logger)...)
			}
		}
	}
	return texts
}

// builtinWriterTexts — тексты записи name встроенной командой stmt: у
// printf -v name — её аргументы, у read и mapfile с name — перенаправления
func builtinWriterTexts(stmt *syntax.Stmt, name string, text func(syntax.Node) string) []string {
	call, ok := stmt.Cmd.(*syntax.CallExpr)
	if !ok || len(call.Args) == 0 {
		return nil
	}
	program, _ := literalValue(call.Args[0])
	var texts []string
	switch program {
	case "printf":
		texts = printfWriterTexts(call, name, text)
	case "read", "mapfile", "readarray":
		if slices.ContainsFunc(call.Args[1:], func(arg *syntax.Word) bool {
			value, _ := literalValue(arg)
			return value == name
		}) {
			for _, redirect := range stmt.Redirs {
				if redirect.Word != nil {
					texts = append(texts, text(redirect.Word))
				}
				if redirect.Hdoc != nil {
					texts = append(texts, text(redirect.Hdoc))
				}
			}
		}
	}
	return texts
}

// computedClientWord — слово программы в script — подстановка команды или
// переменная целиком, которая называет клиента модели. Путь с переменной
// ($S/cl) shellcmd разбирает сам, когда значение известно
func computedClientWord(script string, w *syntax.Word, logger core.Logger) bool {
	return computedClient(script, w, logger) != ""
}

// computedClient — клиент модели, которого называет слово программы w в
// script — подстановка команды или переменная целиком; "" — не называет
func computedClient(script string, w *syntax.Word, logger core.Logger) string {
	if simpleParamName(w) == "" && !wordHasCmdSubst(w) {
		return ""
	}
	return computedClientName(script, script, w, logger)
}

// wordHasCmdSubst — в слове есть подстановка команды $(…) или `…`
func wordHasCmdSubst(w *syntax.Word) bool {
	found := false
	syntax.Walk(w, func(node syntax.Node) bool {
		if _, ok := node.(*syntax.CmdSubst); ok {
			found = true
		}
		return !found
	})
	return found
}

// computedClientName — клиент модели, которого называет слово w текста
// source или, у переменной, её присваивание в command; "" — не называет
func computedClientName(source, command string, w *syntax.Word, logger core.Logger) string {
	start, end := int(w.Pos().Offset()), int(w.End().Offset())
	if start < 0 || end > len(source) || start >= end {
		return ""
	}
	if client := mentionedClient(source[start:end]); client != "" {
		return client
	}
	if name := simpleParamName(w); name != "" {
		return variableClient(command, name, 0, logger)
	}
	return ""
}

// hookSettingsMention — ключ, который снимает хуки, с присвоенным значением
// в тексте команды не JSON-объектом (jq '.disableAllHooks = true')
var hookSettingsMention = regexp.MustCompile(`disableAllHooks["'\\]*\s*[:=]\s*(["'\\]*)([A-Za-z0-9]*)`)

// writtenHookSettingsCandidates — команда пишет файл настроек claude
// (перенаправление, tee, cp, sed -i — protectedWriteTargetSet), и её текст
// задаёт ключ, который снимает хуки: JSON-объект с ним (hookSettingsReasons)
// или присваивание ключу литерала true без кавычек. Цель — файл настроек в
// каталоге .claude, как у Write (claudeSettingsFile без CLAUDE_CONFIG_DIR).
// Запись судится и без вызова claude в команде: файл прочтёт любой claude из
// этого каталога
func writtenHookSettingsCandidates(command string) []modelCandidate {
	if !strings.Contains(command, "settings") {
		return nil
	}
	targets, _ := protectedWriteTargetSet(command)
	if !slices.ContainsFunc(targets, func(target string) bool {
		return claudeSettingsFile(filepath.Clean(strings.Trim(target, `"'`)), "")
	}) {
		return nil
	}
	var reasons []string
	for _, object := range jsonObjects(command) {
		reasons = append(reasons, hookSettingsReasons(object)...)
	}
	for _, m := range hookSettingsMention.FindAllStringSubmatch(command, -1) {
		if m[1] == "" && m[2] == "true" {
			reasons = append(reasons, "disableAllHooks снимает хуки claude (остаются только управляемые)")
		}
	}
	slices.Sort(reasons)
	var found []modelCandidate
	for _, reason := range slices.Compact(reasons) {
		found = append(found, clientOptionCandidate("команда пишет файл настроек claude: "+reason+"; убери ключ — claude, запущенный с этими настройками, работает без хуков chooks"))
	}
	return found
}

// modelGuardedTool — инструмент, чьи вызовы судит model_guard: Agent,
// оболочка, запись файла настроек
func modelGuardedTool(name string) bool {
	return name == "Agent" || slices.Contains(settingsFileTools, name) || core.IsShellTool(name)
}

// printfWriterTexts — аргументы printf -v name: тексты, которые printf
// запишет в name
func printfWriterTexts(call *syntax.CallExpr, name string, text func(syntax.Node) string) []string {
	var texts []string
	for i := 1; i+1 < len(call.Args); i++ {
		option, _ := literalValue(call.Args[i])
		target, _ := literalValue(call.Args[i+1])
		if option != "-v" || target != name {
			continue
		}
		for _, arg := range call.Args[i+2:] {
			texts = append(texts, text(arg))
		}
	}
	return texts
}

// claudeClientCall — вызов c запускает claude: имя клиента или запускатель
// (clientInvocation) либо имя из подстановки или переменной, которое
// называет claude (computedClientCall). Один признак клиента для проверок
// вызова, окружения и записи агентов
func claudeClientCall(c *shellcmd.Call, script string, logger core.Logger) bool {
	if dialect, _, ok := clientInvocation(c.Name, c.Args, c.Raw); ok {
		return dialect == "claude"
	}
	resolved, err := computedClientCall(c, script, logger)
	if err != nil {
		logger.Warn("model_guard: имя вызова из подстановки не разобрано", "error", err)
		return false
	}
	return resolved != nil && clientDialect(resolved.Name) == "claude"
}

// discardLogger — логгер без вывода: для проверок, которым логгер запроса
// не передан (environmentControlUnknown)
type discardLogger struct{}

func (discardLogger) Debug(string, ...any)      {}
func (discardLogger) Info(string, ...any)       {}
func (discardLogger) Warn(string, ...any)       {}
func (discardLogger) Error(string, ...any)      {}
func (d discardLogger) With(...any) core.Logger { return d }

// assignedArguments — вызов c, в котором слово-переменная целиком ($D, "$D",
// ${D}) заменено известным литеральным значением из присваивания той же
// команды (shellcmd.Assigns — тот же разбор, что даёт имя вызова $x):
// cd "$D" после D=/путь. Неизвестное значение, дописывание (+=) и тильда
// оставляют слово как есть. У вызова без смещения (Offset -1) присваивание
// не видно (VisibleAt), и вызов не меняется
func assignedArguments(c *shellcmd.Call, assigns []shellcmd.Assign) *shellcmd.Call {
	var args []string
	for i, arg := range c.Args {
		m := variableReference.FindStringSubmatch(arg)
		if m == nil {
			continue
		}
		value, known := "", false
		for _, a := range assigns {
			if a.Name == m[1] && a.VisibleAt(c.Offset) {
				value, known = a.Value, !a.Unknown && !a.Append && !strings.ContainsAny(a.Value, "$`~")
			}
		}
		if !known {
			continue
		}
		if args == nil {
			args = slices.Clone(c.Args)
		}
		args[i] = value
	}
	if args == nil {
		return c
	}
	resolved := *c
	resolved.Args = args
	return &resolved
}
