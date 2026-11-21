package tools

import (
	"slices"
	"strings"
)

func parallelComplexOption(name string) bool {
	switch name {
	case "col-sep", "delimiter", "null", "link", "max-replace-args", "max-args", "max-lines",
		"L", "X", "m", "header", "csv", "xargs", "sql", "sql-master", "sql-and-worker", "sql-worker":
		return true
	default:
		return slices.Contains(parallelReplaceOptions, name)
	}
}

func parallelComplexSources(p parallelCommand, sets []parallelSet) bool {
	stdinSources, files := parallelArgFileCounts(sets)
	listSources, wordStdin, wordFiles, linked, combinations := parallelWordSourceCounts(p)
	stdinSources += wordStdin
	files += wordFiles
	return p.complexSource || linked || files > 0 || stdinSources > 1 || stdinSources > 0 && listSources > 0 || combinations > maxParallelSourceCombinations
}

func parallelArgFileCounts(sets []parallelSet) (stdin, files int) {
	for _, o := range sets {
		if o.name != "arg-file" {
			continue
		}
		if parallelStdinFile(o.value) {
			stdin++
		} else {
			files++
		}
	}
	return stdin, files
}

func parallelSourceWrapper(p parallelCommand) bool {
	if !(p.listed || p.argInput) || len(p.words) == 0 {
		return false
	}
	end := slices.IndexFunc(p.words, p.sourceWord)
	if end < 0 {
		end = len(p.words)
	}
	if end == 0 {
		return false
	}
	name := p.words[0]
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	switch name {
	case "nice", "env", "timeout", "stdbuf", "setsid", "flock", "tmux", "nohup",
		"ionice", "chrt", "taskset", "command", "exec", "sudo", "doas", "pkexec",
		"su":
		return true
	default:
		return false
	}
}

func parallelSensitiveText(text string) bool {
	return parallelProtectedCommandRe.MatchString(text) || parallelShellCRe.MatchString(text) || signalWordRe.MatchString(text)
}
