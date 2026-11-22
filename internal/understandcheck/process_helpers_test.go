package understandcheck

import (
	"encoding/json"
	"os"
)

func bashRuleFile(command string) string {
	home, _ := os.UserHomeDir()
	file, err := bashRuleFileForHome(command, home)
	warnBashError(os.Stderr, err)
	return file
}

func classify(tool string, raw json.RawMessage) (Kind, string, string, bool) {
	file := ""
	if command, ok := bashCommand(tool, raw); ok {
		file = bashRuleFile(command)
	}
	return classifyWithBash(tool, raw, file)
}
