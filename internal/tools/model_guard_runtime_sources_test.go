package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiseeq/chooks/internal/core"
)

func TestModelGuardStreamJSONInput(t *testing.T) {
	tool := modelGuardForTest(t, "opus claude-opus-5-5\n")
	runClientOptionCases(t, tool, []clientOptionCase{
		{"file input", "claude -p --model opus --input-format stream-json --output-format stream-json --verbose < /tmp/claude-tmp/ctl.jsonl", false, "stream-json"},
		{"pipe input", `printf '%s\n' '{"type":"control_request"}' | claude -p --model opus --input-format stream-json`, false, "control_request"},
		{"attached here-doc", "claude -p --model opus --input-format=stream-json <<'EOF'\n{}\nEOF", false, "stream-json"},
		{"known variable", `F=stream-json; claude -p --model opus --input-format "$F" < /tmp/claude-tmp/ctl.jsonl`, false, "stream-json"},
		{"unknown variable", `claude -p --model opus --input-format="$CHOOKS_TEST_FORMAT" hi`, false, "stream-json"},
		{"wrapped", "timeout 60 claude -p --model opus --input-format stream-json < /tmp/claude-tmp/ctl.jsonl", false, "stream-json"},
		{"text input", "claude -p --model opus --input-format text hi", true, ""},
		{"stream-json output", "claude -p --model opus --output-format stream-json --verbose hi", true, ""},
		{"stream-json prompt", "claude -p --model opus 'stream-json'", true, ""},
	})
}

func TestModelGuardStreamJSONModelsOnly(t *testing.T) {
	tool := modelGuardForTest(t, "opus claude-opus-5-5\n").ModelsOnly()
	result, err := tool.ValidateTool(gitGuardTestContext(t), &core.ToolInput{ToolName: "Bash", Command: "claude -p --model opus --input-format stream-json", CWD: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsValid {
		t.Fatalf("проверка запуска судит только модель: %v", result.Violations)
	}
}

// settingsProject — каталог проекта (с .git) с файлом .claude/<name>
func settingsProject(t *testing.T, name, contents string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".claude", name), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestModelGuardProjectSettingsRemoveHooks(t *testing.T) {
	tool := modelGuardForTest(t, "opus claude-opus-5-5\n")
	cases := []struct {
		name, file, contents string
		allowed              bool
		reason               string
	}{
		{"disable all hooks", "settings.json", `{"disableAllHooks":true}`, false, "disableAllHooks"},
		{"disable all hooks local", "settings.local.json", `{"disableAllHooks": true }`, false, "settings.local.json: disableAllHooks"},
		// загрузчик хуков сравнивает disableAllHooks===true
		{"disable all hooks string", "settings.local.json", `{"disableAllHooks":"true"}`, true, ""},
		{"disable all hooks number", "settings.json", `{"disableAllHooks":1}`, true, ""},
		// allowManagedHooksOnly действует только из управляемых настроек
		{"managed hooks only", "settings.json", `{"allowManagedHooksOnly":true}`, true, ""},
		{"simple env", "settings.json", `{"env":{"GOFLAGS":"-p=1","CLAUDE_CODE_SIMPLE":"1"}}`, false, "env CLAUDE_CODE_SIMPLE"},
		{"hooks off false", "settings.json", `{"disableAllHooks":false}`, true, ""},
		{"hooks off false string", "settings.json", `{"disableAllHooks":"false"}`, true, ""},
		{"hooks off null", "settings.json", `{"disableAllHooks":null,"env":{"GOFLAGS":"-p=1"}}`, true, ""},
		{"not json", "settings.json", `disableAllHooks: true`, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := settingsProject(t, tc.file, tc.contents)
			for _, command := range []string{"cd " + dir + " && claude -p --model opus hi", "env -C " + dir + " claude -p --model opus hi"} {
				result, err := tool.ValidateTool(gitGuardTestContext(t), &core.ToolInput{ToolName: "Bash", Command: command, CWD: t.TempDir()})
				if err != nil {
					t.Fatal(err)
				}
				if result.IsValid != tc.allowed {
					t.Fatalf("%s: allowed=%v, want %v; violations=%v", command, result.IsValid, tc.allowed, result.Violations)
				}
				if tc.reason != "" && !strings.Contains(result.Violations[0].Message, tc.reason) {
					t.Fatalf("%s: reason %q lacks %q", command, result.Violations[0].Message, tc.reason)
				}
			}
		})
	}
}

func TestModelGuardProjectSettingsWithoutClient(t *testing.T) {
	tool := modelGuardForTest(t, "opus claude-opus-5-5\n")
	dir := settingsProject(t, "settings.json", `{"disableAllHooks":true}`)
	result, err := tool.ValidateTool(gitGuardTestContext(t), &core.ToolInput{ToolName: "Bash", Command: "cd " + dir + " && cat .claude/settings.json", CWD: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsValid {
		t.Fatalf("без claude файл настроек не судится: %v", result.Violations)
	}
}

func fileToolResult(t *testing.T, tool *ModelGuardTool, name, cwd string, fields map[string]any) *core.ValidationResult {
	t.Helper()
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	result, err := tool.ValidateTool(gitGuardTestContext(t), &core.ToolInput{ToolName: name, ToolInput: raw, CWD: cwd})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestModelGuardSettingsWriteTool(t *testing.T) {
	tool := modelGuardForTest(t, "opus claude-opus-5-5\n")
	if !strings.Contains(strings.Join(tool.SupportedTools(), " "), "Write") || !strings.Contains(strings.Join(tool.SupportedTools(), " "), "Edit") {
		t.Fatalf("model_guard не судит Write и Edit: %v", tool.SupportedTools())
	}
	dir := t.TempDir()
	settings := filepath.Join(dir, ".claude", "settings.json")
	cases := []struct {
		name, path, content string
		allowed             bool
		reason              string
	}{
		{"disable all hooks", settings, `{"disableAllHooks":true}`, false, "запись " + settings + ": disableAllHooks"},
		{"local settings", filepath.Join(dir, ".claude", "settings.local.json"), `{"permissions":{},"disableAllHooks":true}`, false, "disableAllHooks"},
		{"string value", filepath.Join(dir, ".claude", "settings.local.json"), `{"disableAllHooks":"yes"}`, true, ""},
		{"relative path", ".claude/settings.json", `{"disableAllHooks":true}`, false, "запись " + settings},
		{"simple env", settings, `{"env":{"CLAUDE_CODE_SAFE_MODE":"1"}}`, false, "CLAUDE_CODE_SAFE_MODE"},
		{"model", settings, `{"model":"claude-haiku-4-5"}`, false, "claude-haiku-4-5"},
		{"approved model", settings, `{"model":"opus","permissions":{"allow":["Bash"]}}`, true, ""},
		{"hooks kept", settings, `{"disableAllHooks":false}`, true, ""},
		{"other file", filepath.Join(dir, "notes.json"), `{"disableAllHooks":true}`, true, ""},
		{"other directory", filepath.Join(dir, "cfg", "settings.json"), `{"disableAllHooks":true}`, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := fileToolResult(t, tool, "Write", dir, map[string]any{"file_path": tc.path, "content": tc.content})
			if result.IsValid != tc.allowed {
				t.Fatalf("%s: allowed=%v, want %v; violations=%v", tc.content, result.IsValid, tc.allowed, result.Violations)
			}
			if tc.reason != "" && !strings.Contains(result.Violations[0].Message, tc.reason) {
				t.Fatalf("reason %q lacks %q", result.Violations[0].Message, tc.reason)
			}
		})
	}
	for raw, allowed := range map[string]bool{"": true, `{"file_path":5}`: false} {
		result, err := tool.ValidateTool(gitGuardTestContext(t), &core.ToolInput{ToolName: "Write", ToolInput: json.RawMessage(raw), CWD: dir})
		if err != nil {
			t.Fatal(err)
		}
		if result.IsValid != allowed {
			t.Errorf("Write %q: allowed=%v, want %v; violations=%v", raw, result.IsValid, allowed, result.Violations)
		}
	}
}

func TestModelGuardSettingsEditTool(t *testing.T) {
	tool := modelGuardForTest(t, "opus claude-opus-5-5\n")
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ".claude", "settings.json")
	if err := os.WriteFile(path, []byte(`{"a":false,"disableAllHooks":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, old, new string
		all            bool
		allowed        bool
	}{
		{"turns hooks off", `"disableAllHooks":false`, `"disableAllHooks":true`, false, false},
		{"first match only", "false", "true", false, true},
		{"replace all", "false", "true", true, false},
		{"old text missing", "no-such-text", `"disableAllHooks":true`, false, true},
		{"empty old text", "", `{"disableAllHooks":true}`, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := fileToolResult(t, tool, "Edit", dir, map[string]any{"file_path": path, "old_string": tc.old, "new_string": tc.new, "replace_all": tc.all})
			if result.IsValid != tc.allowed {
				t.Fatalf("allowed=%v, want %v; violations=%v", result.IsValid, tc.allowed, result.Violations)
			}
		})
	}
	missing := fileToolResult(t, tool, "Edit", dir, map[string]any{"file_path": filepath.Join(dir, "x", ".claude", "settings.json"), "old_string": "a", "new_string": `"disableAllHooks":true`})
	if !missing.IsValid {
		t.Fatalf("Edit несуществующего файла не выполнится: %v", missing.Violations)
	}
	// пустой old_string создаёт файл, если его нет или в нём одни пробелы
	created := fileToolResult(t, tool, "Edit", dir, map[string]any{"file_path": filepath.Join(dir, "y", ".claude", "settings.json"), "old_string": "", "new_string": `{"disableAllHooks":true}`})
	if created.IsValid {
		t.Fatal("Edit с пустым old_string создаёт файл настроек без хуков")
	}
	blank := filepath.Join(dir, "z", ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(blank), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blank, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if result := fileToolResult(t, tool, "Edit", dir, map[string]any{"file_path": blank, "old_string": "", "new_string": `{"disableAllHooks":true}`}); result.IsValid {
		t.Fatal("Edit с пустым old_string пишет файл из одних пробелов")
	}
}

func TestClaudeSettingsFile(t *testing.T) {
	cases := []struct {
		path, config string
		want         bool
	}{
		{"/p/.claude/settings.json", "", true},
		{"/p/.claude/settings.local.json", "", true},
		{"/p/.claude/other.json", "", false},
		{"/cfg/settings.json", "/cfg", true},
		{"/cfg/settings.json", "/cfg/", true},
		{"/cfg/settings.json", "", false},
		{"/cfg/sub/settings.json", "/cfg", false},
		{"settings.json", "", false},
	}
	for _, tc := range cases {
		if got := claudeSettingsFile(tc.path, tc.config); got != tc.want {
			t.Errorf("claudeSettingsFile(%q, %q) = %v, want %v", tc.path, tc.config, got, tc.want)
		}
	}
}

func TestModelGuardClientInvocationNames(t *testing.T) {
	tool := modelGuardForTest(t, "opus claude-opus-5-5\n")
	runClientOptionCases(t, tool, []clientOptionCase{
		{"npx latest", "npx @anthropic-ai/claude-code@latest -p --model claude-haiku-4-5 hi", false, "claude-haiku-4-5"},
		{"npx version", "npx @anthropic-ai/claude-code@2.1.332 --bare -p --model opus hi", false, "--bare"},
		{"npx package option", "npx -p @anthropic-ai/claude-code claude --bare -p hi", false, "--bare"},
		{"npx package attached", "npx --package=@anthropic-ai/claude-code claude --bare -p hi", false, "--bare"},
		{"bunx", "bunx @anthropic-ai/claude-code --bare -p hi", false, "--bare"},
		{"pnpm dlx", "pnpm dlx @anthropic-ai/claude-code --bare -p hi", false, "--bare"},
		{"pnpm exec bin", "pnpm exec claude --bare -p hi", false, "--bare"},
		{"npm exec dashes", "npm exec @anthropic-ai/claude-code -- --bare -p hi", false, "--bare"},
		{"npm x", "npm x -- @anthropic-ai/claude-code --bare -p hi", false, "--bare"},
		{"yarn dlx", "yarn dlx @anthropic-ai/claude-code --bare -p hi", false, "--bare"},
		{"bun x", "bun x @anthropic-ai/claude-code --bare -p hi", false, "--bare"},
		{"version binary", "/home/x/.local/share/claude/versions/2.1.332 --bare -p hi", false, "--bare"},
		{"version binary model", "/home/x/.local/share/claude/versions/2.1.332 -p --model claude-haiku-4-5 hi", false, "claude-haiku-4-5"},
		{"screen", "screen -dm claude --bare -p hi", false, "--bare"},
		{"screen session name", "screen -dmS s claude -p --model claude-haiku-4-5 hi", false, "claude-haiku-4-5"},
		{"screen value option", "screen -c /tmp/claude-tmp/rc claude --bare -p hi", false, "--bare"},
		{"screen log file", "screen -L -Logfile /tmp/claude-tmp/l claude --bare -p hi", false, "--bare"},
		{"screen version binary", "screen -dm /home/x/.local/share/claude/versions/2.1.332 --bare -p hi", false, "--bare"},
		{"command -v", `"$(command -v claude)" --bare -p hi`, false, "--bare"},
		{"backticks", "`which claude` -p --model claude-haiku-4-5 hi", false, "claude-haiku-4-5"},
		{"type -P", "$(type -P claude) --bare -p hi", false, "--bare"},
		{"readlink variable", `B=$(readlink -f "$(command -v claude)"); $B --bare -p hi`, false, "--bare"},
		{"quoted variable", `B=$(ls -d ~/.local/share/claude/versions/* | tail -1); "$B" --bare -p hi`, false, "--bare"},
		{"loop variable", "for b in $(which -a claude); do $b --bare -p hi; done", false, "--bare"},
		{"nested shell", `bash -c '"$(command -v claude)" --bare -p hi'`, false, "--bare"},
		{"wrapper", `timeout 60 "$(which claude)" --bare -p hi`, false, "--bare"},
		{"model env prefix", `ANTHROPIC_MODEL=claude-haiku-4-5 "$(command -v claude)" -p hi`, false, "claude-haiku-4-5"},
		{"home prefix", `HOME=/tmp/claude-tmp/x "$(command -v claude)" -p --model opus hi`, false, "HOME"},
		{"stream-json", `"$(which claude)" -p --model opus --input-format stream-json < /tmp/claude-tmp/ctl.jsonl`, false, "stream-json"},
		{"codex substitution", `"$(which codex)" exec -m gpt-x hi`, false, "gpt-x"},
		{"npx version option", "npx -y @anthropic-ai/claude-code --version", true, ""},
		{"npx approved", "npx @anthropic-ai/claude-code@latest -p --model opus hi", true, ""},
		{"npm view", "npm view @anthropic-ai/claude-code version", true, ""},
		{"npm install", "npm install -g @anthropic-ai/claude-code", true, ""},
		{"npm without subcommand", "npm", true, ""},
		{"pnpm exec other bin", "pnpm exec eslint --bare", true, ""},
		{"npx other package", "npx prettier --bare", true, ""},
		{"substitution version", `"$(command -v claude)" --version`, true, ""},
		{"substitution approved", `"$(command -v claude)" -p --model opus hi`, true, ""},
		{"path argument", `B=$(readlink -f "$(command -v claude)"); grep -a -c x "$B"`, true, ""},
		{"other substitution", "$(go env GOPATH)/bin/x --bare", true, ""},
		{"other variable", "X=$(mktemp); $X --bare", true, ""},
		{"other version binary", "/opt/tool/versions/2.1.332 --bare", true, ""},
		{"script among versions", "/home/x/.local/share/claude/versions/cleanup.sh --bare", true, ""},
		{"variable with modifier", "B=$(command -v claude); ${B%x} --bare", false, "--bare"},
		{"other variable name", "XB=$(command -v claude); $B --bare", true, ""},
		{"screen list", "screen -ls", true, ""},
		{"screen remote command", "screen -S s -X stuff 'claude --bare'", true, ""},
		{"screen other program", "screen -dm htop --bare", true, ""},
		{"screen approved", "screen -dmS s claude -p --model opus hi", true, ""},
	})
}

func TestPackageRunnerClaude(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
		ok   bool
	}{
		{"npx", []string{"@anthropic-ai/claude-code", "-p"}, 1, true},
		{"npx", []string{"-y", "@anthropic-ai/claude-code@latest", "--", "-p"}, 3, true},
		{"npx", []string{"-p", "@anthropic-ai/claude-code", "claude", "--", "x"}, 4, true},
		{"npx", []string{"@anthropic-ai/claude-code", "x", "claude"}, 1, true},
		{"npx", []string{"claude", "-p"}, 1, true},
		{"npx", []string{"-y", "claude", "-p"}, 2, true},
		{"npx", []string{"--package=@anthropic-ai/claude-code@latest", "--", "-p"}, 0, false},
		{"npx", []string{"-p", "@anthropic-ai/claude-code", "-c", "claude --bare"}, 0, false},
		{"npx", []string{"--call=claude --bare", "@anthropic-ai/claude-code"}, 0, false},
		{"npx", []string{"-y", "-p", "@anthropic-ai/claude-code", "tee", "x"}, 0, false},
		{"npx", []string{"--package", "x", "claude", "-p"}, 3, true},
		{"npx", []string{"@anthropic-ai/claude-code", "--", "claude", "claude"}, 3, true},
		{"npx", []string{"-y", "prettier", "claude"}, 0, false},
		{"npx", []string{"-y"}, 0, false},
		{"npm", []string{"exec", "--", "@anthropic-ai/claude-code", "-p"}, 3, true},
		{"npm", []string{"view", "@anthropic-ai/claude-code"}, 0, false},
		{"npm", nil, 0, false},
		{"node", []string{"@anthropic-ai/claude-code"}, 0, false},
	}
	for _, tc := range cases {
		got, ok := packageRunnerClaude(tc.name, tc.args)
		if got != tc.want || ok != tc.ok {
			t.Errorf("packageRunnerClaude(%q, %q) = %d, %v; want %d, %v", tc.name, tc.args, got, ok, tc.want, tc.ok)
		}
	}
}

func TestClientInvocationScreenRunner(t *testing.T) {
	dialect, offset, ok := clientInvocation("screen", []string{"-dm", "npx", "@anthropic-ai/claude-code", "-p"}, "screen -dm npx @anthropic-ai/claude-code -p")
	if dialect != "claude" || offset != 3 || !ok {
		t.Fatalf("clientInvocation = %q, %d, %v; want claude, 3, true", dialect, offset, ok)
	}
}

func TestScreenProgram(t *testing.T) {
	cases := []struct {
		args []string
		want int
		ok   bool
	}{
		{[]string{"-dm", "claude"}, 1, true},
		{[]string{"-dmS", "s", "claude"}, 2, true},
		{[]string{"-S", "s", "-dm", "claude"}, 3, true},
		{[]string{"-Sdm", "claude"}, 1, true},
		{[]string{"-Logfile", "f", "claude"}, 2, true},
		{[]string{"-X", "stuff", "claude"}, 0, false},
		{[]string{"-S", "s", "-Q", "windows"}, 0, false},
		{[]string{"-ls"}, 0, false},
	}
	for _, tc := range cases {
		got, ok := screenProgram(tc.args)
		if got != tc.want || ok != tc.ok {
			t.Errorf("screenProgram(%q) = %d, %v; want %d, %v", tc.args, got, ok, tc.want, tc.ok)
		}
	}
}

func TestModelGuardShellWriteRemovesHooks(t *testing.T) {
	tool := modelGuardForTest(t, "opus claude-opus-5-5\n")
	runClientOptionCases(t, tool, []clientOptionCase{
		{"redirect json", `printf '{"disableAllHooks":true}' > /tmp/claude-tmp/p/.claude/settings.json`, false, "команда пишет файл настроек claude: disableAllHooks"},
		{"here-doc local", "cat > .claude/settings.local.json <<'EOF'\n{\"permissions\":{},\"disableAllHooks\": true}\nEOF", false, "disableAllHooks"},
		{"here-doc string value", "cat > .claude/settings.local.json <<'EOF'\n{\"permissions\":{},\"disableAllHooks\":\"1\"}\nEOF", true, ""},
		{"jq string value", `jq '.disableAllHooks = "true"' s.json > .claude/settings.json`, true, ""},
		{"settings outside .claude", `grep -rn 'disableAllHooks = true' internal > /tmp/claude-tmp/p/settings.json`, true, ""},
		{"tee env", `echo '{"env":{"CLAUDE_CODE_SIMPLE":"1"}}' | tee .claude/settings.json`, false, "env CLAUDE_CODE_SIMPLE"},
		{"jq assignment", `jq '.disableAllHooks = true' s.json > .claude/settings.json`, false, "disableAllHooks"},
		{"managed only", `echo '{"allowManagedHooksOnly": true}' > .claude/settings.json`, true, ""},
		{"hooks kept", `printf '{"disableAllHooks":false}' > .claude/settings.json`, true, ""},
		{"hooks kept string", `jq '.disableAllHooks = "false"' s.json > .claude/settings.json`, true, ""},
		{"hooks null", `jq '.disableAllHooks = null' s.json > .claude/settings.json`, true, ""},
		{"other file", `printf '{"disableAllHooks":true}' > /tmp/claude-tmp/p/notes.json`, true, ""},
		{"agent file", `printf '{"disableAllHooks":true,"see":"settings"}' > /tmp/claude-tmp/p/.claude/agents/r.md`, true, ""},
		{"read only", `grep -n disableAllHooks .claude/settings.json`, true, ""},
	})
}

func TestModelGuardComputedClientEnvironment(t *testing.T) {
	tool := modelGuardForTest(t, "opus claude-opus-5-5\n")
	runClientOptionCases(t, tool, []clientOptionCase{
		{"variable from substitution", `B=$(readlink -f "$(command -v claude)"); $B -p --model opus hi`, true, ""},
		{"substitution", `"$(command -v claude)" -p --model opus hi`, true, ""},
		// путь с переменной — не имя из подстановки: неизвестная программа
		// может менять окружение, как и до computedClientWord
		{"path with variable", "S=/tmp/claude-tmp/x; $S/cl --cl-ls > $S/o; for p in $(jq -r .pid /tmp/claude-tmp/s.json); do kill -0 $p; done; $S/cl --cl-ls --under x", false, "после неизвестной команды"},
	})
}
