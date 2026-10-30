package tools

import (
	"regexp"
	"slices"
	"strings"

	"github.com/aiseeq/akit/shellcmd"
)

// CorpusSweepVerdict — результат проверки команды на поиск по корпусу.
type CorpusSweepVerdict struct {
	Blocked    bool     `json:"blocked"`
	Sweeps     []string `json:"sweeps"`
	Points     []string `json:"points"`
	ParseError string   `json:"parse_error,omitempty"`
}

var corpusSearchTools = map[string]bool{
	"rg": true, "grep": true, "egrep": true, "fgrep": true, "zgrep": true,
	"ugrep": true, "ag": true, "find": true, "fd": true, "xargs": true,
}

var corpusRoots = []string{".claude/projects", ".codex", ".local/state/worker"}

// optionAssign ключ со значением через =: --glob=…, -o=…
var optionAssign = regexp.MustCompile(`^--?[A-Za-z][\w-]*=`)

// CheckCorpusSweep находит обращения к корпусу транскриптов и решает,
// затрагивает ли команда широкую выборку.
func CheckCorpusSweep(command, home string) CorpusSweepVerdict {
	var verdict CorpusSweepVerdict
	hasSearch := false
	// Ошибка разбора — команда, которую отвергает и bash -n: оболочка её не
	// выполнит, поиска нет. Вызовы — через обёртки, bash -c и $(…)
	calls, err := shellcmd.Calls(command)
	if err != nil {
		verdict.ParseError = err.Error()
	}
	for _, call := range calls {
		if corpusSearchTools[call.Name] {
			hasSearch = true
		}
		for _, arg := range call.Args {
			addCorpusWord(&verdict, arg, home)
		}
	}
	// Присваивания и списки for … in — не аргументы вызова: их слова берёт parseShell
	script := &shScript{}
	parseShell(command, false, script)
	for _, c := range withInlineScripts(script.cmds) {
		for _, word := range c.words {
			if !word.redirect {
				addCorpusWord(&verdict, unquoteWord(word.text), home)
			}
		}
	}
	if !hasSearch {
		verdict.Sweeps = nil
	}
	verdict.Blocked = hasSearch && len(verdict.Sweeps) > 0
	return verdict
}

func addCorpusWord(verdict *CorpusSweepVerdict, value, home string) {
	if assignWord.MatchString(value) || optionAssign.MatchString(value) {
		value = value[strings.IndexByte(value, '=')+1:]
	}
	path, ok := corpusPath(value, home)
	if !ok || slices.Contains(verdict.Sweeps, path) || slices.Contains(verdict.Points, path) {
		return
	}
	if corpusPoint(path) {
		verdict.Points = append(verdict.Points, path)
	} else {
		verdict.Sweeps = append(verdict.Sweeps, path)
	}
}

// corpusPath проверяет путь к корпусу и сохраняет исходное слово.
func corpusPath(word, home string) (string, bool) {
	prefixes := []string{"~/", "$HOME/", "${HOME}/"}
	if home != "" {
		prefixes = append(prefixes, strings.TrimRight(home, "/")+"/")
	}
	for _, root := range corpusRoots {
		for _, prefix := range prefixes {
			candidate := strings.TrimPrefix(word, prefix)
			if candidate == word {
				continue
			}
			if strings.HasPrefix(candidate, root) && (len(candidate) == len(root) || candidate[len(root)] == '/') {
				return word, true
			}
		}
		if strings.HasPrefix(word, root) && (len(word) == len(root) || word[len(root)] == '/') {
			return word, true
		}
	}
	return "", false
}

// corpusPoint — путь к одному файлу, а не к каталогу или набору файлов.
func corpusPoint(path string) bool {
	if strings.ContainsAny(path, "*?[{") || strings.HasSuffix(path, "/") {
		return false
	}
	last := path[strings.LastIndexByte(path, '/')+1:]
	return len(last) > 0 && last[0] != '.' && strings.Contains(last, ".")
}
