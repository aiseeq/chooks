package tools

import "strings"

// CorpusSweepVerdict — результат проверки команды на поиск по корпусу.
type CorpusSweepVerdict struct {
	Blocked bool     `json:"blocked"`
	Sweeps  []string `json:"sweeps"`
	Points  []string `json:"points"`
}

var corpusSearchTools = map[string]bool{
	"rg": true, "grep": true, "egrep": true, "fgrep": true, "zgrep": true,
	"ugrep": true, "ag": true, "find": true, "fd": true, "xargs": true,
}

var corpusRoots = []string{".claude/projects", ".codex", ".local/state/worker"}

// CheckCorpusSweep находит обращения к корпусу транскриптов и решает,
// затрагивает ли команда широкую выборку.
func CheckCorpusSweep(command, home string) CorpusSweepVerdict {
	script := &shScript{}
	parseShell(command, false, script)
	cmds := withInlineScripts(script.cmds)
	var verdict CorpusSweepVerdict
	hasSearch := false
	for _, c := range cmds {
		words := c.words
		command := commandWords(words)
		if len(command) > 0 && corpusSearchTools[commandName(command[0].raw)] {
			hasSearch = true
		}
		// commandWords пропускает xargs как обёртку; учитываем его только
		// перед найденным именем команды, а не среди обычных аргументов.
		commandIndex := len(words) - len(command)
		for i, word := range words {
			if i < commandIndex && !word.redirect && commandName(word.raw) == "xargs" {
				hasSearch = true
			}
		}
		for _, word := range words {
			if word.redirect {
				continue
			}
			value := unquoteWord(word.text)
			if i := strings.IndexByte(value, '='); i >= 0 {
				value = value[i+1:]
			}
			if path, ok := corpusPath(value, home); ok {
				if corpusPoint(path) {
					verdict.Points = append(verdict.Points, path)
				} else {
					verdict.Sweeps = append(verdict.Sweeps, path)
				}
			}
		}
	}
	if !hasSearch {
		verdict.Sweeps = nil
	}
	verdict.Blocked = hasSearch && len(verdict.Sweeps) > 0
	return verdict
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
