package tools

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/aiseeq/chooks/internal/core"
)

type clientOptionCase struct {
	name, command string
	allowed       bool
	// reason — подстрока текста отказа (имя опции, переменной, подкоманды)
	reason string
}

func runClientOptionCases(t *testing.T, tool *ModelGuardTool, cases []clientOptionCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := tool.ValidateTool(gitGuardTestContext(t), &core.ToolInput{ToolName: "Bash", Command: tc.command, CWD: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			if result.IsValid != tc.allowed {
				t.Fatalf("%s: allowed=%v, want %v; violations=%v", tc.command, result.IsValid, tc.allowed, result.Violations)
			}
			if tc.reason != "" && !strings.Contains(result.Violations[0].Message, tc.reason) {
				t.Fatalf("%s: reason %q lacks %q", tc.command, result.Violations[0].Message, tc.reason)
			}
		})
	}
}

func TestAgentsRefusalRejectsUnparseableJSON(t *testing.T) {
	cases := []struct {
		name, raw string
	}{
		{"invalid top-level JSON", `{"x":`},
		{"agent is not an object", `{"x":"hooks"}`},
		{"null top-level", `null`},
		{"null agent", `{"x":null}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if reason := agentsRefusal(claudeArg{text: tc.raw}, nil, t.TempDir()); reason == "" {
				t.Fatal("agentsRefusal accepted unparseable --agents JSON")
			}
		})
	}
}

func TestAgentsRefusalLabelsInlineUnparseableJSON(t *testing.T) {
	reason := agentsRefusal(claudeArg{text: `{"x":`}, nil, t.TempDir())
	want := "строку --agents не разобрать: откажись от запуска, пока JSON не исправлен"
	if reason != want {
		t.Fatalf("agentsRefusal() = %q, want %q", reason, want)
	}
}

// Пункты 1–7 ревью f9e6d56: снятие хуков и подмена настроек — отказ, в том
// числе без -p и за обёртками
func TestModelGuardClientOptionsRefused(t *testing.T) {
	tool := modelGuardForTest(t, "opus claude-opus-5-5\n")
	runClientOptionCases(t, tool, []clientOptionCase{
		{"safe mode", "claude --safe-mode hi", false, "--safe-mode"},
		{"safe mode in cluster", "claude -p-safe-mode hi", false, "--safe-mode"},
		{"simple env", "CLAUDE_CODE_SIMPLE=1 claude -p hi", false, "CLAUDE_CODE_SIMPLE у клиента claude то же, что --bare"},
		{"safe mode env", "CLAUDE_CODE_SAFE_MODE=1 claude -p hi", false, "CLAUDE_CODE_SAFE_MODE у клиента claude то же, что --safe-mode"},
		{"config dir env", "CLAUDE_CONFIG_DIR=/tmp/claude-tmp/x claude -p hi", false, "CLAUDE_CONFIG_DIR"},
		{"exported simple env", "export CLAUDE_CODE_SIMPLE=1; claude -p --model opus hi", false, "CLAUDE_CODE_SIMPLE"},
		{"env wrapper simple env", "env CLAUDE_CODE_SIMPLE=1 claude -p --model opus hi", false, "CLAUDE_CODE_SIMPLE"},
		{"advisor without print", "claude --advisor claude-haiku-4-5 hi", false, "claude-haiku-4-5"},
		{"advisor in cluster", "claude -p-advisor=claude-haiku-4-5 hi", false, "claude-haiku-4-5"},
		{"advisor attached", "claude -p --model opus --advisor=claude-haiku-4-5 hi", false, "claude-haiku-4-5"},
		{"model attached", "claude -p --model=claude-haiku-4-5 hi", false, "claude-haiku-4-5"},
		{"project config root", "claude --project-config-root /tmp/claude-tmp/x hi", false, "--project-config-root"},
		{"hidden options", "claude --client-data-url https://example.invalid/x --allow-other-models hi", false, "--client-data-url"},
		{"env wrapper", "env claude --bare -p hi", false, "--bare"},
		{"timeout wrapper", "timeout 60 claude --bare -p hi", false, "--bare"},
		{"bash -c", "bash -c 'claude --bare -p hi'", false, "--bare"},
		{"npx", "npx @anthropic-ai/claude-code --bare -p hi", false, "--bare"},
		{"command", "command claude --bare -p hi", false, "--bare"},
		{"exec", "exec claude --bare -p hi", false, "--bare"},
		{"xargs", "echo hi | xargs claude -p --model opus", false, "xargs"},
		{"escaped dash", `claude \--bare -p hi`, false, "--bare"},
		{"which claude", "$(which claude) --bare -p hi", false, "--bare"},
		{"settings json", `claude -p --settings '{"disableAllHooks":true}' hi`, false, "--settings"},
		{"settings attached", "claude -p --model opus --settings=/tmp/claude-tmp/x.json hi", false, "--settings"},
		{"mcp config", "claude -p --mcp-config /tmp/claude-tmp/mcp.json hi", false, "--mcp-config"},
		{"plugin dir", "claude -p --plugin-dir /tmp/claude-tmp/x hi", false, "--plugin-dir"},
		{"worktree short", "claude -w hi", false, "-w"},
		{"worktree cluster", "claude -pw hi", false, "-w"},
		{"worktree long", "claude --worktree hi", false, "--worktree"},
		{"flag with value", "claude -p --model opus --verbose=x hi", false, "--verbose"},
		{"optional value stops at option", "claude -p --model opus -r --safe-mode hi", false, "--safe-mode"},
		{"debug file in settings", "claude -p --model opus --debug-file=/home/x/.claude/settings.local.json hi", false, "--debug-file"},
		{"debug file on hook binary", "claude -p --model opus --debug-file=/home/x/bin/chooks hi", false, "--debug-file"},
		{"debug file through variable", `D=/home/x/.claude/settings.json; claude -p --model opus --debug-file="$D" hi`, false, "/home/x/.claude/settings.json"},
		{"debug file unknown variable", "claude -p --model opus --debug-file $UNSET_CHOOKS_TEST_DIR/d.log hi", false, "--debug-file"},
		{"debug file variable with two values", "S=/var/x; S=/tmp/claude-tmp/a; claude -p --model opus --debug-file $S/d.log hi", false, "--debug-file"},
		{"debug file under home", "claude -p --model opus --debug-file ~/.claude/settings.json hi", false, "--debug-file"},
		{"debug file quoted unknown variable", `claude -p --model opus --debug-file "$UNSET_CHOOKS_TEST_DIR" hi`, false, "не установить"},
		{"debug file in cluster", "claude -p-debug-file=/home/x/.claude/settings.json hi", false, "--debug-file"},
		{"mcp add", "claude mcp add x -- /bin/true", false, "mcp add"},
		{"mcp get without name", "claude mcp get", false, "mcp get"},
		{"plugin install", "claude plugin install foo", false, "plugin install"},
		{"auth login", "claude auth login", false, "auth login"},
		{"update", "claude -p --model opus update", false, "update"},
		{"unquoted unknown variable", "claude -p --model opus $CHOOKS_TEST_ARGS hi", false, "$CHOOKS_TEST_ARGS"},
		{"unquoted command substitution", "claude -p --model opus $(cat /tmp/claude-tmp/p.md)", false, "несколько слов"},
		{"quoted positional list", `claude -p --model opus "$@"`, false, "несколько слов"},
		{"unquoted value with spaces", "A='opus --bare'; claude -p --append-system-prompt $A hi", false, "несколько слов"},
		{"unparsed", "claude -p --model opus --bare 'unterminated", false, "не разобрана"},
		{"quoted substitution prompt", `claude -p --model opus "$(cat /tmp/claude-tmp/p.md)"`, false, "после --"},
		{"optional value unknown", `claude -p --model opus -r "$CHOOKS_TEST_UNSET" hi`, false, "может оказаться --bare"},
		{"optional value unknown names forms", `claude -p --model opus --resume "$CHOOKS_TEST_UNSET" hi`, false, "(--opt=…), промпт — после --"},
		{"substitution with suffix", `claude -p --model opus "$CHOOKS_TEST_UNSET suffix"`, false, "начинается подстановкой"},
		{"optional value splits", "claude -p --model opus -r $CHOOKS_TEST_UNSET hi", false, "несколько слов"},
		{"optional value splits after literal", "claude -p --model opus -r x$CHOOKS_TEST_UNSET hi", false, "несколько слов"},
		{"variadic stops at unknown", `claude -p --model opus --allowedTools Bash "$CHOOKS_TEST_UNSET suffix" hi`, false, "начинается подстановкой"},
		{"loop option", "for o in -c --bare; do claude -p --model opus $o hi; done", false, "может оказаться --bare"},
		{"loop option with suffix", `for o in -c -d; do claude -p --model opus "${o}x" hi; done`, false, "начинается подстановкой"},
		{"loop escaped item", `for o in \--bare x; do claude -p --model opus $o hi; done`, false, "несколько слов"},
		{"loop without in", "o=x; f() { for o; do claude -p --model opus $o hi; done; }; f --bare", false, "несколько слов"},
		{"loop brace item", "for o in {--bare,x}; do claude -p --model opus $o hi; done", false, "несколько слов"},
		{"loop of other variable", "for m in a; do claude -p --model opus $o hi; done", false, "несколько слов"},
		{"escaped assignment", `o=\--bare; claude -p --model opus $o hi`, false, "несколько слов"},
		{"empty unquoted value", "e=''; claude $e mcp add x -- /bin/true", false, "несколько слов"},
		{"quoted array", `a=(--bare); claude -p --model opus hi "${a[@]}"`, false, "несколько слов"},
		{"variadic stops at option", "claude -p --model opus --allowedTools Bash --bare hi", false, "--bare"},
		{"help after unknown short option", "claude -p hi -w --help", false, "-w"},
		{"loop value with space", "for o in 'a --bare' b; do claude -p --model opus $o; done", false, "несколько слов"},
		{"help after unknown option", "claude -p hi --project-config-root --help", false, "--project-config-root"},
		{"help not last", "claude --help --bare", false, "--bare"},
		{"help keeps model", "claude --model claude-haiku-4-5 --help", false, "claude-haiku-4-5"},
		{"help after split word", "claude -p hi $CHOOKS_TEST_UNSET --help", false, "несколько слов"},
		{"help after substitution", `claude -p hi "$CHOOKS_TEST_UNSET x" --help`, false, "начинается подстановкой"},
		{"help keeps debug file", "claude --debug-file /home/x/.claude/settings.json --help", false, "--debug-file"},
		{"help keeps feeder", "echo --help | xargs claude --bare", false, "xargs"},
		{"agents with mcp", "claude agents --mcp-config /tmp/claude-tmp/mcp.json", false, "--mcp-config"},
		{"agents json with model", "claude agents --json --model claude-haiku-4-5", false, "claude-haiku-4-5"},
		{"agents option outside agents", "claude --json -p hi", false, "--json"},
		{"cl option after name", "cl -n x --cl-ls", false, "--cl-ls"},
		{"associative key before client", "declare -A R; R[gh:a/b#1]=x; claude --bare -p hi", false, "--bare"},
		{"subscript with space stays unparsed", "R[a b:c]=x; claude -p --model opus hi", false, "не разобрана"},
		{"associative key and inherited value", `declare -A R; R[gh:a#1]=x; claude -p --model opus "$HOME"`, false, "может оказаться --bare"},
		{"heredoc of other command", "claude -p --model opus \"$(tac <<'EOF'\nhi\nEOF\n)\"", false, "может оказаться --bare"},
		{"heredoc with tab strip", "claude -p --model opus \"$(cat <<-'EOF'\nhi\nEOF\n)\"", false, "может оказаться --bare"},
		{"heredoc escape unquoted", "claude -p --model opus \"$(cat <<EOF\n\\x\nEOF\n)\"", false, "может оказаться --bare"},
		{"heredoc variable first", "claude -p --model opus \"$(cat <<EOF\n$CHOOKS_TEST_UNSET\nEOF\n)\"", false, "может оказаться --bare"},
		{"unquoted heredoc with variable", "claude -p --model opus $(cat <<EOF\nhi $CHOOKS_TEST_UNSET\nEOF\n)", false, "несколько слов"},
		{"heredoc option", "claude -p --model opus \"$(cat <<'EOF'\n--bare\nEOF\n)\"", false, "--bare"},
		{"unquoted heredoc splits", "claude -p --model opus $(cat <<'EOF'\nhi --bare\nEOF\n)", false, "несколько слов"},
		{"unquoted heredoc empty", "claude -p --model opus $(cat <<'EOF'\n\nEOF\n) hi", false, "несколько слов"},
		// ревью eb4fda4, п. 1–2: --restricted и слова, которые claude ищет в argv сам
		{"restricted", "claude -p --model opus --restricted hi", false, "не читает файлы настроек"},
		{"restricted first", "claude --restricted --tools Bash -p --model opus hi", false, "--restricted"},
		{"restricted after double dash", "claude -p --model opus -- --restricted", false, "--restricted"},
		{"restricted env", "CLAUDE_CODE_RESTRICTED=1 claude -p --model opus --tools Bash hi", false, "CLAUDE_CODE_RESTRICTED"},
		{"bare as value", "claude -p --model opus --append-system-prompt --bare hi", false, "--bare"},
		{"bare as short value", "claude -p --model opus -n --bare hi", false, "--bare"},
		{"safe mode as value", "claude -p --model opus --name --safe-mode hi", false, "--safe-mode"},
		{"bare as debug file", "claude -p --model opus --debug-file --bare hi", false, "--bare"},
		{"bare as variadic value", "claude -p --model opus --add-dir --bare -- hi", false, "--bare"},
		{"restricted as value", "claude -p --model opus --append-system-prompt --restricted hi", false, "--restricted"},
		{"substitution as required value", `claude -p --model opus --session-id "$(uuidgen)" hi`, false, "может оказаться --bare"},

		// п. 4, 7: HOME и переменные claude — белым списком
		{"home", "HOME=/tmp/claude-tmp/fakehome claude -p --model opus hi", false, "HOME у клиента claude берёт настройки и хуки из .claude другого каталога"},
		{"exported home", "export HOME=/tmp/claude-tmp/fakehome; claude -p --model opus hi", false, "HOME"},
		{"env home", "env HOME=/tmp/claude-tmp/fakehome claude -p --model opus hi", false, "HOME"},

		{"unknown simple env", "CLAUDE_CODE_SIMPLE=$CHOOKS_TEST_UNSET claude -p --model opus hi", false, "то же, что --bare"},
		{"plugin dirs env", "CLAUDE_CODE_PLUGIN_DIRS=/tmp/x claude -p --model opus hi", false, "каталоги плагинов"},
		{"personal config env", "CLAUDE_CODE_RESTRICT_PERSONAL_CONFIG=1 claude -p --model opus hi", false, "личные хуки"},
		{"managed settings env", "CLAUDE_CODE_MANAGED_SETTINGS_PATH=/tmp/x claude -p --model opus hi", false, "управляемые настройки"},
		{"config dir elsewhere", "CLAUDE_CONFIG_DIR=/tmp/claude-tmp/x claude -p --model opus hi", false, "не из ~/.claude"},
		{"empty config dir", "CLAUDE_CONFIG_DIR= claude -p --model opus hi", false, "CLAUDE_CONFIG_DIR у клиента claude берёт настройки и хуки не из ~/.claude"},
		{"empty exported config dir", "export CLAUDE_CONFIG_DIR=; claude -p --model opus hi", false, "CLAUDE_CONFIG_DIR у клиента claude"},
		{"empty simple env", "CLAUDE_CODE_SIMPLE= claude -p --model opus hi", false, "CLAUDE_CODE_SIMPLE у клиента claude"},
		{"unknown claude env", "ANTHROPIC_BASE_URL=http://127.0.0.1:9 claude -p --model opus hi", false, "ANTHROPIC_BASE_URL у клиента claude не в списке известных"},
		{"read env", "read -r CLAUDE_CODE_SIMPLE <<< 1; export CLAUDE_CODE_SIMPLE; claude -p --model opus hi", false, "переменные CLAUDE_CODE_SIMPLE у клиента claude команда задаёт без видимого значения"},
		{"printf env", "printf -v CLAUDE_CODE_SIMPLE 1; export CLAUDE_CODE_SIMPLE; claude -p --model opus hi", false, "CLAUDE_CODE_SIMPLE у клиента claude команда задаёт"},
		{"source env", "source /tmp/claude-tmp/env.sh; claude -p --model opus hi", false, "команда с source или eval"},
		{"dot env", ". /tmp/claude-tmp/env.sh && claude -p --model opus hi", false, "команда с source или eval"},
		{"eval env", `eval "$CHOOKS_TEST_UNSET"; claude -p --model opus hi`, false, "команда с source или eval"},
		// п. 9: фигурные скобки и .claude проекта в журнале
		{"brace option", "claude -p --model opus {--bare,} hi", false, "в кавычки"},
		{"brace settings", "claude -p --model opus {--settings,/tmp/x.json} hi", false, "фигурные скобки"},
		{"debug file in project claude dir", "claude -p --model opus --debug-file /tmp/claude-tmp/w/.claude/settings.json hi", false, "каталог .claude"},
		{"debug file beside temp dir", "claude -p --model opus --debug-file /tmpx/d.log hi", false, "вне временного каталога"},
		// п. 10: тексты отказа
		{"unknown harmless option", "claude -p --model opus --thinking-display summarized hi", false, "--thinking-display клиента claude не в списке известных"},
		{"writing subcommand lists readers", "claude config set -g model opus", false, "не в списке читающих: без отказа — только mcp list, mcp get <имя>, auth status, plugin list, plugins list, agents, logs <имя>, doctor"},
		{"split word asks for quotes", "claude -p --model opus $CHOOKS_TEST_UNSET hi", false, "возьми слово в кавычки"},
	})
}

// Пункт 8 ревью f9e6d56 и разбор слов по правилам commander: пропуск
func TestModelGuardClientOptionsAllowed(t *testing.T) {
	tool := modelGuardForTest(t, "opus claude-opus-5-5\n")
	runClientOptionCases(t, tool, []clientOptionCase{
		{"approved model", "claude -p --model opus hi", true, ""},
		{"no print", "claude --model opus hi", true, ""},
		{"debug", "claude -p --model opus --debug hi", true, ""},
		{"debug filter", "claude -p --model opus -d hooks hi", true, ""},
		{"debug filter attached", "claude -p --model opus -dhooks hi", true, ""},
		{"fallback approved", "claude -p --model opus --fallback-model opus hi", true, ""},
		{"advisor approved", "claude -p --model opus --advisor opus hi", true, ""},
		{"betas", "claude -p --model opus --betas x hi", true, ""},

		{"strict mcp", "claude -p --model opus --strict-mcp-config hi", true, ""},
		{"ide chrome brief", "claude -p --model opus --ide --chrome --no-chrome --brief hi", true, ""},
		{"prompt suggestions", "claude -p --model opus --prompt-suggestions hi", true, ""},
		{"system prompt snapshot", "claude -p --model opus --system-prompt-snapshot off hi", true, ""},
		{"dash prompt after double dash", "claude -p --model opus -- '-x is a prompt'", true, ""},
		{"dash prompt", "claude -p --model opus '- bullet prompt'", true, ""},
		{"variadic tools", "claude -p --model opus --allowedTools Bash Grep -- hi", true, ""},
		{"variadic takes command word", "claude -p --model opus --allowedTools Bash update", true, ""},
		{"value starting with dash", "claude -p --model opus --append-system-prompt '--bare minimum' hi", true, ""},
		{"short value starting with dash", "claude -p --model opus -n--bare hi", true, ""},
		{"short value attached", "claude -p --model opus -nprobe hi", true, ""},
		{"resume value", "claude -p --model opus -r abc hi", true, ""},
		{"resume takes command word", "claude -p --model opus -r update", true, ""},
		{"resume without value", "claude -p --model opus -r", true, ""},
		{"continue cluster", "claude --model opus -cp hi", true, ""},
		{"builtin agent", "claude -p --model opus --agent general-purpose hi", true, ""},
		{"thinking", "claude -p --model opus --thinking enabled --max-thinking-tokens 1000 hi", true, ""},
		{"system prompt file", "claude -p --model opus --system-prompt-file /tmp/claude-tmp/p.md hi", true, ""},
		{"permission prompt tool", "claude -p --model opus --permission-prompt-tool mcp__x__y hi", true, ""},
		{"output and hook events", "claude -p --model opus --output-format=stream-json --verbose --include-hook-events hi", true, ""},
		{"debug file in tmp", "claude -p --model opus --debug-file /tmp/claude-tmp/debug.log hi", true, ""},
		{"debug file relative to temp cwd", "claude -p --model opus --debug-file d.log hi", true, ""},
		{"print long", "claude --print --model opus hi", true, ""},
		{"debug file through assigned variable", "S=/tmp/claude-tmp/s; claude -p --model opus --debug-file $S/debug.txt hi", true, ""},
		{"add dirs", "claude -p --model opus --add-dir /tmp/claude-tmp/a /tmp/claude-tmp/b -- hi", true, ""},
		{"substitution prompt after double dash", `claude -p --model opus -- "$(cat /tmp/claude-tmp/p.md)"`, true, ""},
		{"substitution after literal", `claude -p --model opus "prompt: $(cat /tmp/claude-tmp/p.md)"`, true, ""},
		{"substitution attached to required value", `claude -p --model opus --session-id="$(uuidgen)" hi`, true, ""},
		{"substitution with prefix as required value", `claude -p --model opus --append-system-prompt "Контекст: $(cat /tmp/claude-tmp/p.md)" hi`, true, ""},
		{"unquoted heredoc with variable inside", "claude -p --model opus \"$(cat <<EOF\nhello $CHOOKS_TEST_UNSET\nEOF\n)\"", true, ""},
		{"bare value inside word", "claude -p --model opus --append-system-prompt 'not --bare' hi", true, ""},
		{"logs", "claude logs abc", true, ""},
		{"doctor", "claude doctor", true, ""},
		{"default config dir", "CLAUDE_CONFIG_DIR=~/.claude claude -p --model opus hi", true, ""},
		{"claude md env", "CLAUDE_CODE_ADDITIONAL_DIRECTORIES_CLAUDE_MD=1 claude -p --model opus --add-dir /tmp/claude-tmp/a hi", true, ""},
		{"model env judged elsewhere", "ANTHROPIC_MODEL=opus claude -p hi", true, ""},

		{"nested script keeps environment", "bash -c 'echo claude'; claude -p --model opus hi", true, ""},
		{"read of other variable", "read -r X <<< 1; claude -p --model opus hi", true, ""},
		{"read of assigned other variable", "X=1; read -r X <<< 2; claude -p --model opus hi", true, ""},
		{"default config dir with slash", "CLAUDE_CONFIG_DIR=~/.claude/ claude -p --model opus hi", true, ""},
		{"bare in prompt after double dash", "claude -p --model opus -- hi --bare", true, ""},
		{"quoted heredoc with backslash", "claude -p --model opus \"$(cat <<'EOF'\nline \\ end\nEOF\n)\"", true, ""},
		{"substitution prompt after double dash may not be restricted", `claude -p --model opus -- "$CHOOKS_TEST_UNSET"`, true, ""},
		{"env mentioned in prompt", "claude -p --model opus 'объясни CLAUDE_CODE_SIMPLE'", true, ""},
		{"agents without hooks", `claude -p --model opus --agents '{"x":{"description":"d","prompt":"p","model":"opus"}}' --agent x hi`, true, ""},
		{"quoted brace", `claude -p --model opus "{--bare,}" hi`, true, ""},
		{"substitution attached to optional", `claude -p --model opus --resume="$CHOOKS_TEST_UNSET" hi`, true, ""},
		{"loop values", "for m in default auto; do claude -p --model opus --permission-mode $m hi; done", true, ""},
		{"loop values as prompt", "for p in one two; do claude -p --model opus $p; done", true, ""},
		{"loop dash values inside word", `for o in -a -b; do claude -p --model opus "x$o"; done`, true, ""},
		{"help after unknown option and word", "claude --bogus x --help", true, ""},
		{"heredoc prompt", "claude -p --model opus \"$(cat <<'EOF'\n- review\nEOF\n)\"", true, ""},
		{"associative key and client text", "declare -A R; R[gh:a/claude-code#1]=\"claude -p у нас мало\"; R[gh:b#2]+=x; k=gh; echo \"${R[$k]}\"", true, ""},
		{"unquoted heredoc word", "claude -p --model opus $(cat <<'EOF'\nhi\nEOF\n)", true, ""},
		{"inherited value", `claude -p --model opus "$HOME"`, true, ""},
		{"subcommand short help", "claude plugin validate -h", true, ""},
		{"subcommand help", "claude respawn --help", true, ""},
		{"plugin help", "claude plugin --help", true, ""},
		{"background help", "claude --bg --help", true, ""},
		{"help after value option", "claude --model opus --append-system-prompt x --help", true, ""},
		{"agents", "claude agents", true, ""},
		{"agents json", "claude agents --json --all --cwd /tmp/claude-tmp", true, ""},
		{"unquoted known variable", "A=/tmp/claude-tmp/a; claude -p --model opus --add-dir $A hi", true, ""},
		{"bare after double dash", "claude -p --model opus hi -- --bare", true, ""},

		{"printed", "echo claude --bare", true, ""},
		{"printed unparsed", "echo claude 'unterminated", true, ""},
		{"help", "claude --help", true, ""},
		{"version", "claude -v", true, ""},
		{"mcp list", "claude mcp list", true, ""},
		{"mcp get", "claude mcp get x", true, ""},
		{"auth status", "claude auth status", true, ""},
		{"plugin list", "claude plugin list", true, ""},
		{"cl utility", "cl --cl-ls --under /tmp/claude-tmp --json", true, ""},
		{"cl passes options", "cl -n probe --model opus", true, ""},
	})
}

// Ревью eb4fda4, п. 6: агент в --agents со своими MCP-серверами или хуками —
// отказ, строкой и из файла
func TestModelGuardClientAgentDefinitions(t *testing.T) {
	tool := modelGuardForTest(t, "opus claude-opus-5-5\n")
	dir := t.TempDir()
	withServers := filepath.Join(dir, "servers.json")
	plain := filepath.Join(dir, "plain.json")
	if err := os.WriteFile(withServers, []byte(`{"a":{"description":"d","prompt":"p","model":"opus"},"x":{"description":"d","prompt":"p","model":"opus","mcpServers":{"m":{"command":"/bin/true"}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plain, []byte(`{"x":{"description":"d","prompt":"p","model":"opus"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	runClientOptionCases(t, tool, []clientOptionCase{
		{"inline mcp servers", `claude -p --model opus --agents '{"x":{"description":"d","prompt":"p","model":"opus","mcpServers":{}}}' --agent x hi`, false, "агент x в --agents задаёт mcpServers"},
		{"inline hooks", `claude -p --model opus --agents '{"x":{"description":"d","prompt":"p","model":"opus","hooks":{}}}' --agent x hi`, false, "агент x в --agents задаёт hooks"},
		{"mcp servers with escaped newline", `claude -p --model opus --agents '{"x":{"description":"d","prompt":"a\nb","mcpServers":["m"]}}' hi`, false, "mcpServers"},
		{"hooks with escaped newline", `claude -p --model opus --agents '{"x":{"description":"d","prompt":"a\nb","hooks":{}}}' hi`, false, "hooks"},
		{"escaped quote slash and backslash keep refusal", `claude -p --model opus --agents '{"x":{"description":"d","prompt":"a\"b\\c\/d\ne","hooks":{}}}' hi`, false, "hooks"},
		{"unparseable inline agents refused", "claude -p --model opus --agents '{\"x\":{\"description\":\"d\",\"prompt\":\"a\nb\",\"hooks\":{}}}' hi", false, "не разобрать"},
		{"file mcp servers", "claude -p --model opus --agents " + withServers + " --agent x hi", false, "агент x в --agents задаёт mcpServers"},
		{"file without servers", "claude -p --model opus --agents " + plain + " --agent x hi", true, ""},
		{"agents not objects", `claude -p --model opus --agents '{"x":"hooks"}' hi`, false, "не разобрать"},
		{"agents without model allowed with escaped newline", `claude -p --model opus --agents '{"x":{"description":"d","prompt":"a\nb"}}' hi`, true, ""},
	})
}

// Слово-подстановка в месте опции, которое может оказаться --bare, получает
// один отказ: с формами --opt=… и промпта после --, без второго текста о
// подстановке в месте опции
func TestModelGuardHookWordRefusedOnce(t *testing.T) {
	tool := modelGuardForTest(t, "opus claude-opus-5-5\n")
	result, err := tool.ValidateTool(gitGuardTestContext(t), &core.ToolInput{ToolName: "Bash", Command: `claude -p --model opus "$CHOOKS_TEST_UNSET"`, CWD: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsValid || len(result.Violations) != 1 {
		t.Fatalf("valid=%v violations=%v, want one refusal", result.IsValid, result.Violations)
	}
	message := result.Violations[0].Message
	if !strings.Contains(message, "может оказаться --bare") || strings.Contains(message, "начинается подстановкой") {
		t.Fatalf("message %q, want only the hook word refusal", message)
	}
}

// Проверка запуска (modellaunch) судит только модель: опции и окружение
// клиента — дело PreToolUse, а --settings ставит и cl пользователя
func TestModelGuardModelsOnlySkipsClientOptions(t *testing.T) {
	tool := modelGuardForTest(t, "opus claude-opus-5-5\n").ModelsOnly()
	runClientOptionCases(t, tool, []clientOptionCase{
		{"bare", "claude --bare --model opus -p hi", true, ""},
		{"simple env", "CLAUDE_CODE_SIMPLE=1 claude -p --model opus hi", true, ""},
		{"model still judged", "claude --bare --model claude-haiku-4-5 -p hi", false, "claude-haiku-4-5"},
		{"settings model still judged", `claude --settings '{"model":"claude-haiku-4-5"}' -p hi`, false, "claude-haiku-4-5"},
		{"settings approved model", `claude --settings '{"model":"opus"}' -p hi`, true, ""},
		{"unparsed approved model", `claude --model opus -p "hi`, true, ""},
		{"unparsed unapproved model", `claude --model claude-haiku-4-5 -p "hi`, false, "claude-haiku-4-5"},
		{"fish approved model", "fish -c 'claude --model opus -p hi; and true'", true, ""},
		{"fish env approved model", "fish -c 'set -gx ANTHROPIC_MODEL opus; claude -p hi'", true, ""},
	})
}

func TestModelGuardCandidateModelsSkipClientOptions(t *testing.T) {
	tool := modelGuardForTest(t, "opus claude-opus-5-5\n")
	got := tool.CandidateModels(gitGuardTestContext(t), &core.ToolInput{ToolName: "Bash", Command: "claude --bare --model opus -p hi", CWD: t.TempDir()})
	if !slices.Contains(got, "opus") || slices.ContainsFunc(got, func(value string) bool { return strings.Contains(value, "--bare") }) {
		t.Fatalf("CandidateModels = %q, want opus without the --bare refusal", got)
	}
}

// settingsModelJudge — модель в --settings и --plugin-dir судит и проверка
// запуска (ModelsOnly): cl пользователя ставит --settings сам. В PreToolUse
// сами опции — отказ (TestModelGuardClientOptionsRefused), и прежние случаи о
// модели в этих опциях идут через ModelsOnly: отказ по опции не скрывает суд
// модели
func settingsModelJudge(tool *ModelGuardTool, command string) *ModelGuardTool {
	if !strings.Contains(command, "--settings") && !strings.Contains(command, "--plugin-dir") {
		return tool
	}
	judge := *tool
	judge.modelsOnly = true
	return &judge
}
