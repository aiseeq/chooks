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
		if c.Name == "find" && shallowFind(c.Args) {
			continue
		}
		paths, ok := walkPaths(c)
		if !ok {
			continue
		}
		if len(paths) == 0 {
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
	n, err := strconv.Atoi(maxDepth)
	return err == nil && n <= 3
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
		if args[i] == ";" || args[i] == `\;` || args[i] == "+" && i > start && args[i-1] == "{}" {
			return i
		}
	}
	return len(args)
}

func execWalksRecursively(args []string) bool {
	if len(args) == 0 {
		return false
	}
	_, recursive := walkPaths(&shellcmd.Call{Name: args[0], Args: args[1:]})
	return recursive
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
	case "tree":
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
	return positional(c, []string{"-e", "-f", "-g", "-t", "-T", "-m", "-A", "-B", "-C", "-M", "-j", "-d", "-E", "-r", "--regexp", "--file", "--glob", "--iglob", "--type", "--type-not", "--max-count", "--context", "--after-context", "--before-context", "--max-columns", "--threads", "--max-depth", "--max-filesize", "--encoding", "--replace", "--type-add", "--type-clear", "--sort", "--sortr", "--ignore-file", "--pre", "--pre-glob", "--color", "--colors", "--context-separator", "--field-context-separator", "--field-match-separator", "--path-separator", "--dfa-size-limit", "--regex-size-limit", "--engine", "--hostname-bin", "--hyperlink-format"}, !patternGiven)
}

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
