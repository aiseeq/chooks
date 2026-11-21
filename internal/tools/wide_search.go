package tools

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/aiseeq/akit/shellcmd"
)

// WideSearchHintText — подсказка на обход большого дерева диска (тикет
// wide-search-hint, «да» пользователя 20.11 12:17 CET на рекомендацию лида):
// такие обходы фризили диск у разных сессий 20–21.11
const WideSearchHintText = "дерево большое: код — `git grep` в репо, транскрипты — `tl`, имена файлов — `plocate` (индекс ночной; ~/.cache и транскрипты в нём исключены), иначе сузь путь"

// wideRoots — большие деревья относительно домашнего каталога; обход их
// самих или их предков (~, /) — широкий, обход внутри одного репо или
// своего scratchpad — нет
var wideRoots = []string{"", ".cache", ".cache/claude-tmp", "git", "work", ".local/state"}

// WideSearch — первый в команде рекурсивный обход большого дерева: grep -r,
// rg, find, du, ls -R по пути из wideRoots или его предку. Относительный
// путь и вызов без пути — от cwd (cd с путём без подстановки меняет его для
// следующих вызовов в порядке shellcmd.Calls; cd в подоболочке тоже — для
// подсказки это допустимая неточность). Пустая строка — обхода нет
func WideSearch(command, cwd, home string) (string, error) {
	calls, err := shellcmd.Calls(command)
	if err != nil {
		return "", fmt.Errorf("wide search: %w", err)
	}
	for i := range calls {
		c := &calls[i]
		if c.Name == "cd" && len(c.Args) == 1 && !dynamicWord(c.Args[0]) {
			cwd = resolvePath(c.Args[0], c.ArgTilde(0), cwd, home)
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
			if p.dynamic {
				continue
			}
			if dir := resolvePath(p.text, p.tilde, cwd, home); isWideRoot(dir, home) {
				return c.Name + " " + dir, nil
			}
		}
	}
	return "", nil
}

type walkArg struct {
	text    string
	tilde   shellcmd.Tilde
	dynamic bool
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
		slices.Contains(c.Args, "--directories=recurse")
	if !recursive {
		return nil, false
	}
	patternGiven := hasFlag(c.Args, 'e', "--regexp") || hasFlag(c.Args, 'f', "--file")
	return positional(c, []string{"-e", "-f", "-m", "-A", "-B", "-C", "-d", "-D", "--regexp", "--file", "--max-count", "--context", "--after-context", "--before-context", "--include", "--exclude", "--exclude-dir", "--directories", "--devices", "--label"}, !patternGiven), true
}

func rgPaths(c *shellcmd.Call) []walkArg {
	patternGiven := slices.Contains(c.Args, "--files") || hasFlag(c.Args, 'e', "--regexp") || hasFlag(c.Args, 'f', "--file")
	return positional(c, []string{"-e", "-f", "-g", "-t", "-T", "-m", "-A", "-B", "-C", "-M", "-j", "-d", "-E", "-r", "--regexp", "--file", "--glob", "--iglob", "--type", "--type-not", "--max-count", "--context", "--after-context", "--before-context", "--max-columns", "--threads", "--max-depth", "--encoding", "--replace", "--type-add", "--sort", "--sortr", "--ignore-file", "--pre", "--pre-glob"}, !patternGiven)
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
		paths = append(paths, walkArg{text: a, tilde: c.ArgTilde(i), dynamic: dynamicWord(a)})
	}
	return paths
}

// dynamicWord — слово с подстановкой: его путь до запуска не известен.
// ArgLiteral тут строже нужного (тильда, список через && дают false), а
// подсказка ничего не запрещает
func dynamicWord(a string) bool {
	return strings.ContainsAny(a, "$`")
}

// positional — слова вызова, не опции и не значения опций valueOpts; skip
// — первое такое слово (шаблон grep и rg) не путь
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
			if slices.Contains(valueOpts, a) {
				i++
			}
			continue
		}
		if skip {
			skip = false
			continue
		}
		out = append(out, walkArg{text: a, tilde: c.ArgTilde(i), dynamic: dynamicWord(a)})
	}
	return out
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
