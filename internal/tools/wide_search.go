package tools

import (
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/aiseeq/akit/shellcmd"
)

// WideSearchHintText — подсказка на обход большого дерева диска (тикет
// wide-search-hint, «да» пользователя 20.11 12:17 CET на рекомендацию лида):
// такие обходы фризили диск у разных сессий 20–21.11
const WideSearchHintText = "дерево большое: код — `git grep` в репо, транскрипты — `tl`, имена файлов — `plocate` (индекс ночной; ~/.cache и транскрипты в нём исключены), иначе сузь путь"

// wideRoots — большие деревья относительно домашнего каталога; обход их
// самих или их предков (~, /) — широкий, обход внутри одного репо или
// своего scratchpad — нет. claude-1000 — все scratchpad сессий сразу,
// .local/state/worker, транскрипты .claude/projects и прогоны Сна
// .claude/improve/runs (1,4 ГБ) — из обходов 20–21.11; sc2 и sc2/demu
// (корпуса и данные demu, grep -rl прочитал 568 МБ 21.11)
var wideRoots = []string{"", ".cache", ".cache/claude-tmp", ".cache/claude-tmp/claude-1000", "git", "work",
	".local/state", ".local/state/worker", ".claude/projects", ".claude/improve/runs", "sc2", "sc2/demu"}

// shallowSearchMaxDepth — глубина find и rg, до которой обход не считается
// широким (тикеты wide-search-shallow-find, wide-search-depth-flags)
const shallowSearchMaxDepth = 3

// WideSearch — первый в команде рекурсивный обход большого дерева: grep -r,
// rg, find, du, ls -R по пути из wideRoots или его предку. Относительный
// путь и вызов без пути — от cwd (cd и pushd с известным путём, cd без
// пути — домой, меняют его для следующих вызовов в порядке shellcmd.Calls;
// cd в подоболочке тоже, cd - и cd -P путь не меняют — для подсказки это
// допустимая неточность). Пустая
// строка — обхода нет
func WideSearch(command, cwd, home string) (string, error) {
	calls, err := shellcmd.Calls(command)
	if err != nil {
		return "", fmt.Errorf("wide search: %w", err)
	}
	for i := range calls {
		c := &calls[i]
		if c.Name == "cd" || c.Name == "pushd" {
			cwd = changeDir(c, cwd, home)
			continue
		}
		if shallowTraversal(c) {
			if !hasUnknownRecursivePath(calls, home) {
				continue
			}
		}
		paths, ok := walkPaths(c)
		if !ok {
			continue
		}
		if len(paths) == 0 {
			// xargs/parallel feed positional paths to the command. Without a
			// path argument, only xargs without replacement appends input paths.
			if slices.Contains(c.Via, "parallel") || xargsAppendsInput(c, calls) {
				continue
			}
			paths = []walkArg{{text: "."}}
		}
		for _, p := range paths {
			text, known := knownPath(p.text, home)
			if !known {
				continue
			}
			if dir := resolvePath(text, p.tilde, cwd, home); isWideRoot(dir, home) {
				return c.Name + " " + dir, nil
			}
		}
	}
	return "", nil
}

// xargsAppendsInput reports whether the xargs wrapper feeding c appends its
// input as positional arguments. A shell -c between xargs and c gets them as
// script arguments, not as paths of c: a path of c from "$1" or "$@" is
// already in its argv, and without one c searches the cwd.
func xargsAppendsInput(c *shellcmd.Call, calls []shellcmd.Call) bool {
	xargs := -1
	for i, via := range c.Via {
		if via == "xargs" {
			xargs = i
		}
	}
	if xargs < 0 {
		return false
	}
	options := xargsOptions(*c, calls)
	if slices.ContainsFunc(options, replaceOption) {
		return false
	}
	return !slices.Contains(c.Via[xargs+1:], "-c")
}

// shallowFind — у find литеральный -maxdepth от 0 до 3 (тикет
// wide-search-shallow-find); GNU find берёт его в любом месте выражения
func shallowFind(args []string) bool {
	var maxDepth string
	foundDepth := false
	recursiveExec := false
	for i := 0; i < len(args); i++ {
		if isFindExec(args[i]) {
			end := findExecEnd(args, i+1)
			if execWalksRecursively(args[i+1 : end]) {
				recursiveExec = true
			}
			i = end
			continue
		}
		if args[i] != "-maxdepth" || i+1 >= len(args) {
			continue
		}
		maxDepth, foundDepth = args[i+1], true
		i++
	}
	if !foundDepth || recursiveExec {
		return false
	}
	return depthInRange(maxDepth)
}

func shallowTraversal(c *shellcmd.Call) bool {
	switch c.Name {
	case "find":
		return shallowFind(c.Args)
	case "rg":
		return shallowDepth(c.Args)
	default:
		return false
	}
}

// shallowDepth — литеральная глубина rg (-d N, -dN, --max-depth[=]N,
// --maxdepth[=]N); действует последняя, как у самого rg.
func shallowDepth(args []string) bool {
	var depth string
	found := false
	for i := 0; i < len(args); i++ {
		value, hasDepth, skip, stop := rgDepthArg(args, i)
		if stop {
			break
		}
		if hasDepth {
			depth, found = value, true
		}
		i += skip
	}
	return found && depthInRange(depth)
}

func rgDepthArg(args []string, i int) (depth string, found bool, skip int, stop bool) {
	a := args[i]
	if a == "--" {
		return "", false, 0, true
	}
	if value, ok := rgDepthValue(a); ok {
		return value, true, 0, false
	}
	if strings.HasPrefix(a, "--") {
		return rgLongDepthArg(args, i)
	}
	return rgShortDepthArg(args, i)
}

func rgLongDepthArg(args []string, i int) (depth string, found bool, skip int, stop bool) {
	a := args[i]
	if a == "--max-depth" || a == "--maxdepth" {
		if i+1 < len(args) {
			return args[i+1], true, 1, false
		}
		return "", true, 0, false
	}
	name, _, hasValue := strings.Cut(a, "=")
	if slices.Contains(rgValueOpts, name) && !hasValue && i+1 < len(args) {
		return "", false, 1, false
	}
	return "", false, 0, false
}

func rgShortDepthArg(args []string, i int) (depth string, found bool, skip int, stop bool) {
	a := args[i]
	if len(a) < 2 || a[0] != '-' {
		return "", false, 0, false
	}
	for j := 1; j < len(a); j++ {
		opt := "-" + a[j:j+1]
		if !slices.Contains(rgValueOpts, opt) {
			continue
		}
		if opt == "-d" {
			if j+1 < len(a) {
				return a[j+1:], true, 0, false
			}
			if i+1 < len(args) {
				return args[i+1], true, 1, false
			}
			return "", true, 0, false
		}
		if j == len(a)-1 && i+1 < len(args) {
			return "", false, 1, false
		}
		return "", false, 0, false
	}
	return "", false, 0, false
}

func rgDepthValue(arg string) (string, bool) {
	for _, name := range []string{"--max-depth", "--maxdepth"} {
		if arg == name {
			return "", false
		}
		if value, ok := strings.CutPrefix(arg, name+"="); ok {
			return value, true
		}
	}
	return "", false
}

// depthInRange — глубина литералом от 0 до shallowSearchMaxDepth
func depthInRange(depth string) bool {
	n, err := strconv.Atoi(depth)
	return err == nil && n >= 0 && n <= shallowSearchMaxDepth
}

func isFindExec(arg string) bool {
	switch arg {
	case "-exec", "-execdir", "-ok", "-okdir":
		return true
	default:
		return false
	}
}

func findExecEnd(args []string, start int) int {
	for i := start; i < len(args); i++ {
		if args[i] == ";" || args[i] == "+" && i > start && args[i-1] == "{}" {
			return i
		}
	}
	return len(args)
}

func execWalksRecursively(args []string) bool {
	if len(args) == 0 {
		return false
	}
	parsed, err := shellcmd.Calls(literalShellWords(args))
	if err != nil && len(parsed) == 0 {
		return false
	}
	for i := range parsed {
		c := &parsed[i]

		if shallowTraversal(c) {
			continue
		}
		if _, recursive := walkPaths(c); recursive {
			return true
		}
	}
	return false
}

// hasUnknownRecursivePath keeps a shallow find wide when another recursive
// call in the command can consume paths produced at runtime.
func hasUnknownRecursivePath(calls []shellcmd.Call, home string) bool {
	for i := range calls {
		c := &calls[i]
		if slices.Contains(c.Via, "find") {
			continue
		}
		if c.Name == "parallel" {
			if parallelHasUnknownRecursivePath(c, home) {
				return true
			}
			continue
		}
		if recursivePathUnknown(c, home, slices.Contains(c.Via, "xargs") || slices.Contains(c.Via, "parallel")) {
			return true
		}
	}
	return false
}

func parallelHasUnknownRecursivePath(c *shellcmd.Call, home string) bool {
	if len(c.Args) == 0 {
		return false
	}
	start := parallelCommandStart(c.Args)
	if start >= len(c.Args) {
		return false
	}
	// GNU parallel accepts a command as either multiple words or one shell
	// script string, for example: parallel 'du -s {}'.
	commandText := c.Args[start]
	if strings.ContainsAny(commandText, " \t\n;&|<>$`()") {
		commandText = strings.Trim(commandText, "'\"")
	} else {
		commandText = literalShellWords(c.Args[start:])
	}
	command, parsed := parseParallelShellCommand(commandText)
	if !parsed {
		return true
	}
	for i := range command {
		if recursivePathUnknown(&command[i], home, true) {
			return true
		}
	}
	return false
}

// parseParallelShellCommand reports parse failure separately so callers can
// conservatively treat an unparsed recursive command as having an unknown path.
func parseParallelShellCommand(script string) ([]shellcmd.Call, bool) {
	calls, err := shellcmd.Calls(script)
	return calls, err == nil
}

// parallelCommandStart skips parallel options before its command. Options
// with a separate value consume the following argument; attached values (for
// example -j4 and --jobs=4) stay in the option word.
func parallelCommandStart(args []string) int {
	valueOptions := map[string]bool{
		"-j": true, "--jobs": true, "-n": true, "--max-args": true,
		"-P": true, "--max-procs": true, "-L": true, "--max-lines": true,
		"-d": true, "--delimiter": true, "-S": true, "--sshlogin": true,
		"-a": true, "--arg-file": true, "--sshloginfile": true, "--joblog": true,
		"--results": true, "--workdir": true, "--env": true, "--block": true,
		"--recstart": true, "--recend": true, "--regexp": true, "--tagstring": true,
		"--halt": true, "--timeout": true, "--delay": true, "--memfree": true,
		"--load": true, "--nice": true, "--filter": true, "--return": true,
		"--transferfile": true, "--cleanup": true, "--trc": true, "--basefile": true,
		"--record-env": true, "-C": true, "--colsep": true, "-I": true, "-E": true,
		"--retries": true, "--tmpdir": true, "--header": true, "--termseq": true,
		"--sshdelay": true, "--group-by": true, "--compress-program": true,
		"--limit": true, "--memsuspend": true,
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return i + 1
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			return i
		}
		if valueOptions[arg] {
			i++
		}
	}
	return len(args)
}

func recursivePathUnknown(c *shellcmd.Call, home string, implicitPath bool) bool {
	paths, recursive := walkPaths(c)
	if !recursive {
		return false
	}
	if implicitPath {
		return true
	}
	if len(paths) == 0 {
		return false
	}
	for _, p := range paths {
		if p.text == "{}" {
			return true
		}
		if _, known := knownPath(p.text, home); !known {
			return true
		}
	}
	return false
}

// changeDir — cwd после cd или pushd c; путь с неизвестной подстановкой
// оставляет прежний
func changeDir(c *shellcmd.Call, cwd, home string) string {
	dir, _ := changeDirKnown(c, cwd, home)
	return dir
}

func changeDirKnown(c *shellcmd.Call, cwd, home string) (string, bool) {
	if len(c.Args) == 0 {
		if c.Name == "cd" {
			return home, home != ""
		}
		return cwd, false
	}
	i := 0
	for i < len(c.Args) && (c.Args[i] == "-P" || c.Args[i] == "-L") {
		i++
	}
	if i < len(c.Args) && c.Args[i] == "--" {
		i++
	}
	if i >= len(c.Args) {
		return cwd, false
	}
	if i+1 != len(c.Args) || c.Args[i] == "-" {
		return cwd, false
	}
	text, known := knownPath(c.Args[i], home)
	if !known {
		return cwd, false
	}
	return resolvePath(text, c.ArgTilde(i), cwd, home), true
}

type walkArg struct {
	text  string
	tilde shellcmd.Tilde
}

// walkPaths — пути, которые обходит вызов c; ok=false — вызов не
// рекурсивный обход
func walkPaths(c *shellcmd.Call) ([]walkArg, bool) {
	switch c.Name {
	case "grep", "egrep", "fgrep":
		return grepPaths(c)
	case "rg":
		return rgPaths(c), true
	case "find":
		return findPaths(c), true
	case "du":
		return positional(c, []string{"-d", "-B", "-t", "-X", "--exclude-from", "--files0-from"}, false), true
	case "ls":
		if !hasFlag(c.Args, 'R', "--recursive") {
			return nil, false
		}
		return positional(c, nil, false), true
	}
	return nil, false
}

func grepPaths(c *shellcmd.Call) ([]walkArg, bool) {
	recursive := hasFlag(c.Args, 'r', "--recursive") || hasFlag(c.Args, 'R', "--dereference-recursive") ||
		directoriesRecurse(c.Args)
	if !recursive {
		return nil, false
	}
	patternGiven := hasFlag(c.Args, 'e', "--regexp") || hasFlag(c.Args, 'f', "--file")
	return positional(c, []string{"-e", "-f", "-m", "-A", "-B", "-C", "-d", "-D", "--regexp", "--file", "--max-count", "--context", "--after-context", "--before-context", "--include", "--exclude", "--exclude-dir", "--exclude-from", "--directories", "--devices", "--label", "--binary-files", "--group-separator"}, !patternGiven), true
}

// directoriesRecurse — grep -d recurse в записях -d recurse, --directories
// recurse, --directories=recurse (-drecurse ловит hasFlag по букве r)
func directoriesRecurse(args []string) bool {
	for i, a := range args {
		if a == "--" {
			return false
		}
		next := i+1 < len(args) && args[i+1] == "recurse"
		if a == "--directories=recurse" || (a == "-d" || a == "--directories") && next {
			return true
		}
	}
	return false
}

func rgPaths(c *shellcmd.Call) []walkArg {
	patternGiven := slices.Contains(c.Args, "--files") || hasFlag(c.Args, 'e', "--regexp") || hasFlag(c.Args, 'f', "--file")
	return positional(c, rgValueOpts, !patternGiven)
}

var rgValueOpts = []string{"-e", "-f", "-g", "-t", "-T", "-m", "-A", "-B", "-C", "-M", "-j", "-d", "-E", "-r", "--regexp", "--file", "--glob", "--iglob", "--type", "--type-not", "--max-count", "--context", "--after-context", "--before-context", "--max-columns", "--threads", "--max-depth", "--maxdepth", "--max-filesize", "--encoding", "--replace", "--type-add", "--type-clear", "--sort", "--sortr", "--ignore-file", "--pre", "--pre-glob", "--color", "--colors", "--context-separator", "--field-context-separator", "--field-match-separator", "--path-separator", "--dfa-size-limit", "--regex-size-limit", "--engine", "--hostname-bin", "--hyperlink-format"}

// findPaths — начальные точки find: слова до первого выражения
func findPaths(c *shellcmd.Call) []walkArg {
	var paths []walkArg
	for i, a := range c.Args {
		if i == 0 && (a == "-H" || a == "-L" || a == "-P") {
			continue
		}
		if strings.HasPrefix(a, "-") || a == "(" || a == "!" {
			break
		}
		paths = append(paths, walkArg{text: a, tilde: c.ArgTilde(i)})
	}
	return paths
}

// knownPath — путь слова до запуска: $HOME и ${HOME} в начале — home,
// кавычки вокруг слова с подстановкой сняты (shellcmd отдаёт такое слово
// исходным текстом); другая подстановка — путь не известен. ArgLiteral тут
// строже нужного (тильда, список через && дают false), а подсказка ничего
// не запрещает
func knownPath(a, home string) (string, bool) {
	if len(a) > 1 && a[0] == '"' && a[len(a)-1] == '"' {
		a = a[1 : len(a)-1]
	}
	for _, v := range []string{"${HOME}", "$HOME"} {
		rest, ok := strings.CutPrefix(a, v)
		if ok && (rest == "" || rest[0] == '/') {
			a = home + rest
			break
		}
	}
	return a, !strings.ContainsAny(a, "$`")
}

// positional — слова вызова, не опции и не значения опций valueOpts; skip
// — первое такое слово (шаблон grep и rg) не путь. Короткая опция со
// значением в связке (-rne x) берёт остаток связки или следующее слово
func positional(c *shellcmd.Call, valueOpts []string, skip bool) []walkArg {
	var out []walkArg
	options := true
	for i := 0; i < len(c.Args); i++ {
		a := c.Args[i]
		switch {
		case options && a == "--":
			options = false
			continue
		case options && strings.HasPrefix(a, "-") && a != "-":
			if takesNext(a, valueOpts) {
				i++
			}
			continue
		}
		if skip {
			skip = false
			continue
		}
		out = append(out, walkArg{text: a, tilde: c.ArgTilde(i)})
	}
	return out
}

// takesNext — опция a берёт следующее слово значением: длинная или
// короткая из valueOpts целиком, либо связка коротких, где первая со
// значением стоит последней (-rne x; -rnex — значение «x» в самой связке)
func takesNext(a string, valueOpts []string) bool {
	if slices.Contains(valueOpts, a) {
		return true
	}
	if strings.HasPrefix(a, "--") {
		return false
	}
	for j := 1; j < len(a); j++ {
		if slices.Contains(valueOpts, "-"+a[j:j+1]) {
			return j == len(a)-1
		}
	}
	return false
}

// hasFlag — короткий флаг short (в том числе в связке -rn) или длинный long
func hasFlag(args []string, short rune, long string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if a == long || strings.HasPrefix(a, long+"=") {
			return true
		}
		if len(a) > 1 && a[0] == '-' && a[1] != '-' && strings.ContainsRune(a[1:], short) {
			return true
		}
	}
	return false
}

// resolvePath — абсолютный чистый путь слова: ~ — home, относительный — от
// cwd; глоб — каталог до первого метасимвола (ещё глубже он не уходит)
func resolvePath(text string, tilde shellcmd.Tilde, cwd, home string) string {
	if i := strings.IndexAny(text, "*?["); i >= 0 {
		text = filepath.Dir(text[:i] + "x")
	}
	text = tildePath(text, tilde, home)
	if !filepath.IsAbs(text) {
		text = filepath.Join(cwd, text)
	}
	return filepath.Clean(text)
}

// isWideRoot — dir одно из wideRoots или их предок
func isWideRoot(dir, home string) bool {
	if home == "" {
		return false
	}
	for _, r := range wideRoots {
		root := filepath.Join(home, r)
		if dir == root || strings.HasPrefix(root, strings.TrimSuffix(dir, "/")+"/") {
			return true
		}
	}
	return false
}
