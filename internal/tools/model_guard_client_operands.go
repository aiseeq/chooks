package tools

import (
	"slices"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// clientSubcommands — подкоманды верхнего уровня claude 2.1.331: они
// меняют настройки, MCP, плагины, учётку или установку, а не задают промпт
var clientSubcommands = []string{
	"agents", "auth", "auto-mode", "config", "doctor", "gateway", "import", "install", "mcp",
	"migrate-installer", "plugin", "plugins", "remote-control", "setup-token", "ultrareview",
	"update", "upgrade",
}

// Длинные опции claude 2.1.331 со значением: обязательным, необязательным
// (берёт следующее слово, если оно не опция) и списком (берёт все слова до
// следующей опции). Опция, которой здесь нет, считается флагом: её значение
// становится позиционным словом, и это даёт отказ, а не пропуск
var (
	clientRequiredOptions = []string{
		"advisor", "agent", "agent-color", "agent-id", "agent-name", "agent-type", "agents",
		"append-subagent-system-prompt", "append-subagent-system-prompt-file", "append-system-prompt",
		"append-system-prompt-file", "attach-serve", "autocompact", "client-data-url", "correlation-id",
		"debug-file", "deep-link-cwd-b64", "deep-link-last-fetch", "deep-link-repo", "effort",
		"environment", "fallback-model", "fast-mode", "forward-home-settings", "inherit-permission-mode",
		"initialize-fields", "input-format", "json-schema", "lead-verify-key", "managed-settings",
		"max-budget-usd", "max-thinking-tokens", "max-turns", "metadata", "model", "name", "on-branch",
		"output-format", "parent-session-id", "permission-mode", "permission-prompt-tool",
		"permission-prompts", "plan-mode-instructions", "plugin-dir", "plugin-dir-no-mcp", "plugin-url",
		"pool", "prefill", "prefill-b64", "project-config-root", "ref",
		"remote-control-session-name-prefix", "resume-drops-turn", "resume-session-at", "rewind-files",
		"runner-declares", "sdk-url", "session-began-as-copy", "session-id", "setting-sources",
		"settings", "system-prompt", "system-prompt-file", "system-prompt-snapshot", "task-budget",
		"team-name", "teammate-mode", "thinking", "thinking-display", "watch-artifact",
		"watch-artifact-no-autoreact", "workload",
	}
	clientOptionalOptions = []string{
		"cloud", "debug", "from-pr", "prompt-suggestions", "rc", "remote", "remote-control", "resume",
		"teleport", "worktree",
	}
	clientVariadicOptions = []string{
		"add-dir", "allowed-tools", "allowedTools", "betas", "channels",
		"dangerously-load-development-channels", "disallowed-tools", "disallowedTools", "file",
		"mcp-config", "tools",
	}
)

// Короткие опции claude со значением: -n обязательное, -d, -r, -w
// необязательное; -d2e — отдельный флаг
const (
	clientShortRequired = "n"
	clientShortOptional = "drw"
)

type clientValueKind int

const (
	clientNoValue clientValueKind = iota
	clientRequiredValue
	clientOptionalValue
	clientVariadicValue
)

// clientOperands — позиционные слова вызова claude, как их отделяет
// commander: опции со значением забирают своё значение, `--` завершает
// опции. printing — среди опций есть -p/--print
func clientOperands(args []*syntax.Word) (operands []string, printing bool) {
	for i := 0; i < len(args); i++ {
		value := wordOwnText(args[i])
		kind := clientNoValue
		switch {
		case value == "--":
			for _, rest := range args[i+1:] {
				operands = append(operands, wordOwnText(rest))
			}
			return operands, printing
		case strings.HasPrefix(value, "--"):
			name, _, attached := strings.Cut(value[2:], "=")
			printing = printing || name == "print"
			if !attached {
				kind = clientLongOptionValue(name)
			}
		case strings.HasPrefix(value, "-") && len(value) > 1 && value != "-d2e":
			var short bool
			kind, short = clientShortCluster(value[1:])
			printing = printing || short
		default:
			operands = append(operands, value)
		}
		i = clientSkipValue(args, i, kind)
	}
	return operands, printing
}

func clientLongOptionValue(name string) clientValueKind {
	switch {
	case slices.Contains(clientRequiredOptions, name):
		return clientRequiredValue
	case slices.Contains(clientOptionalOptions, name):
		return clientOptionalValue
	case slices.Contains(clientVariadicOptions, name):
		return clientVariadicValue
	}
	return clientNoValue
}

// clientShortCluster — кластер коротких флагов (`-pc`): вид значения
// последней опции, если значение не слито с ней, и признак -p до неё
func clientShortCluster(cluster string) (clientValueKind, bool) {
	printing := false
	for i, flag := range cluster {
		attached := i+1 < len(cluster)
		switch {
		case strings.ContainsRune(clientShortRequired, flag):
			if attached {
				return clientNoValue, printing
			}
			return clientRequiredValue, printing
		case strings.ContainsRune(clientShortOptional, flag):
			if attached {
				return clientNoValue, printing
			}
			return clientOptionalValue, printing
		case flag == 'p':
			printing = true
		}
	}
	return clientNoValue, printing
}

// clientSkipValue — индекс последнего слова, которое забрала опция args[i]
func clientSkipValue(args []*syntax.Word, i int, kind clientValueKind) int {
	for next := i + 1; next < len(args); next++ {
		if kind == clientNoValue || kind != clientRequiredValue && strings.HasPrefix(wordOwnText(args[next]), "-") {
			return next - 1
		}
		if kind != clientVariadicValue {
			return next
		}
	}
	return len(args) - 1
}
