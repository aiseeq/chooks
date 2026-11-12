package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/aiseeq/chooks/internal/core"
)

func modelGuardForTest(t *testing.T, contents string) *ModelGuardTool {
	t.Helper()
	path := filepath.Join(t.TempDir(), "approved")
	if contents != "\x00" {
		if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("MODELS_APPROVED", path)
	logger, err := core.NewLogger(core.DefaultLoggerConfig())
	if err != nil {
		t.Fatal(err)
	}
	tool, _ := NewModelGuardTool(core.ToolConfig{Enabled: true}, logger)
	return tool
}

func TestModelGuard(t *testing.T) {
	tool := modelGuardForTest(t, "opus claude-opus-5-5\nsol gpt-6.1-sol\nluna gpt-6-luna\n")
	cases := []struct {
		name, tool, raw, command string
		reject                   bool
	}{
		{"Agent без модели", "Agent", `{}`, "", false},
		{"Agent имя", "Agent", `{"model":"opus"}`, "", false},
		{"Agent id", "Agent", `{"model":"claude-opus-5-5"}`, "", false},
		{"Agent sonnet", "Agent", `{"model":"sonnet"}`, "", true},
		{"Agent haiku", "Agent", `{"model":"haiku"}`, "", true},
		{"claude sonnet", "Bash", "", "claude -p --model sonnet \"x\"", true},
		{"claude approved", "Bash", "", "claude --model=opus", false},
		{"codex luna", "Bash", "", "codex exec -m gpt-6-luna", false},
		{"codex unknown", "Bash", "", "codex -m gpt-5", true},
		{"opencode provider", "Bash", "", "opencode run -m openai/gpt-6-luna", false},
		{"opencode unknown", "Bash", "", "opencode run -m deepseek/deepseek-v4", true},
		{"timeout", "Bash", "", "timeout 60 claude --model haiku", true},
		{"codex config", "Bash", "", "codex -c model=gpt-4o", true},
		{"dynamic", "Bash", "", `claude --model "$M"`, true},
		{"echo", "Bash", "", "echo 'claude --model sonnet'", false},
		{"grep", "Bash", "", "grep -- '--model sonnet' f", false},
		{"here-doc", "Bash", "", "cat <<'EOF'\nclaude --model sonnet\nEOF", false},
		{"here-doc с табуляцией", "Bash", "", "cat <<-EOF >out\n\tclaude --model sonnet\n\tEOF", false},
		{"here-doc через пробел", "Bash", "", "cat << \"EOF\" 2>&1\nclaude --model sonnet\nEOF", false},
		{"команда после here-doc", "Bash", "", "cat <<EOF\nx\nEOF\nclaude --model sonnet", true},
		{"ls", "Bash", "", "ls", false},
		// Monitor исполняет command оболочкой (тикет kill-guard-gaps)
		{"Monitor sonnet", "Monitor", "", "claude --model sonnet", true},
		{"if claude", "Bash", "", "if claude -p --model sonnet x; then echo; fi", true},
		// Ревизия хуков 11.11 п.3: модель без --model
		{"переменная окружения", "Bash", "", "ANTHROPIC_MODEL=sonnet claude -p hi", true},
		{"export переменной", "Bash", "", "export ANTHROPIC_MODEL='claude-sonnet-5-5'; claude -p hi", true},
		{"переменная одобренной", "Bash", "", "ANTHROPIC_MODEL=opus claude -p hi", false},
		{"переменная субагентов", "Bash", "", "CLAUDE_CODE_SUBAGENT_MODEL=haiku claude -p hi", true},
		{"переменная из переменной", "Bash", "", `ANTHROPIC_MODEL="$M" claude -p hi`, true},
		{"переменная без claude", "Bash", "", "ANTHROPIC_MODEL=sonnet env | grep MODEL", false},
		{"bash -c", "Bash", "", `bash -c "claude --model sonnet -p hi"`, true},
		{"ssh", "Bash", "", `ssh pc 'claude --model sonnet -p hi'`, true},
		{"here-doc в оболочку", "Bash", "", "bash <<'EOF'\nclaude --model sonnet -p hi\nEOF", true},
		{"settings json", "Bash", "", `claude -p hi --settings '{"model":"sonnet"}'`, true},
		{"settings json env", "Bash", "", `claude -p hi --settings '{"env":{"ANTHROPIC_MODEL":"sonnet"}}'`, true},
		{"settings json одобренной", "Bash", "", `claude -p hi --settings '{"model":"opus"}'`, false},
		{"fallback-model", "Bash", "", "claude -p hi --model opus --fallback-model sonnet", true},
		// Ревью 12.11: упоминание переменной — не присваивание
		{"переменная в промпте", "Bash", "", `claude -p "объясни, почему ANTHROPIC_MODEL=sonnet не работает" --model opus`, false},
		{"переменная в here-doc промпта", "Bash", "", "cat <<'EOF' | claude --model opus -p\nревью: ANTHROPIC_MODEL=sonnet claude -p hi обходил guard\nEOF", false},
		{"переменная в $(here-doc)", "Bash", "", "claude --model opus -p \"$(cat <<'EOF'\nANTHROPIC_MODEL=sonnet\nEOF\n)\"", false},
		{"переменная в файле скрипта", "Bash", "", "cat > /tmp/x.sh <<'X'\nANTHROPIC_MODEL=sonnet claude -p hi\nX\nclaude --model opus -p review", false},
		{"переменная у другой команды", "Bash", "", "ANTHROPIC_MODEL=sonnet python3 bench.py; claude --model opus -p hi", false},
		{"переменная в echo", "Bash", "", "echo 'ANTHROPIC_MODEL=sonnet' >> /tmp/notes.md && claude --model opus -p hi", false},
		{"одобренная в кавычках", "Bash", "", "ANTHROPIC_MODEL='opus' claude -p hi", false},
		{"одобренная с переносом строки", "Bash", "", "ANTHROPIC_MODEL=opus \\\n  claude -p hi", false},
		{"переменная в bash -c", "Bash", "", `bash -c 'ANTHROPIC_MODEL=sonnet claude -p hi'`, true},
		{"env переменной", "Bash", "", "env ANTHROPIC_MODEL=sonnet claude -p hi", true},
		{"declare -x", "Bash", "", "declare -x ANTHROPIC_MODEL=sonnet; claude -p hi", true},
		{"запасной лексер", "Bash", "", `claude --model sonnet -p "hi`, true},
		{"запасной лексер, переменная", "Bash", "", `ANTHROPIC_MODEL=sonnet claude -p "hi`, true},
		{"agents inherit", "Bash", "", `claude -p hi --agents '{"r":{"description":"d","prompt":"p","model":"inherit"}}'`, false},
		{"agents не JSON", "Bash", "", `claude -p hi --agents agents.json`, false},
		{"settings=", "Bash", "", `claude -p hi --settings='{"model":"sonnet"}'`, true},
		{"fallback-model=", "Bash", "", "claude -p hi --fallback-model=sonnet", true},
		{"agents=", "Bash", "", `claude -p hi --agents='{"r":{"description":"d","prompt":"p","model":"haiku"}}'`, true},
		{"settings поле не того типа", "Bash", "", `claude -p hi --settings '{"model":"sonnet","env":{"X":1}}'`, true},
		{"settings файл из той же команды", "Bash", "", `echo '{"model":"sonnet"}' > /tmp/mg-absent-s.json && claude -p hi --settings /tmp/mg-absent-s.json`, true},
		{"settings файл из той же команды, одобренная", "Bash", "", `echo '{"model":"opus"}' > /tmp/mg-absent-s.json && claude -p hi --settings /tmp/mg-absent-s.json`, false},
		// 12.11 сухой прогон: путь настроек из переменной — не модель
		{"settings путь из переменной", "Bash", "", `S=/tmp/p; claude -p hi --settings $S/a.json`, false},
		{"settings json с подстановкой", "Bash", "", `claude -p hi --settings "{\"model\":\"$M\"}"`, true},
		{"agents json", "Bash", "", `claude -p hi --agents '{"r":{"description":"d","prompt":"p","model":"haiku"}}'`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := &core.ToolInput{ToolName: tc.tool, Command: tc.command, ToolInput: []byte(tc.raw)}
			got, err := tool.ValidateTool(context.Background(), input)
			if err != nil {
				t.Fatal(err)
			}
			if got.IsValid == tc.reject {
				t.Fatalf("valid=%v violations=%v", got.IsValid, got.Violations)
			}
			// Подсказка — отдельной строкой, не повтором причины
			if tc.reject && (!slices.Equal(got.Suggestions, []string{modelGuardHint}) || strings.Contains(got.Violations[0].Message, modelGuardHint)) {
				t.Errorf("подсказка: %q %q", got.Violations[0].Message, got.Suggestions)
			}
		})
	}
}

func TestModelGuardApprovedListErrors(t *testing.T) {
	for _, tc := range []struct{ name, contents, want string }{{"missing", "\x00", "не найден"}, {"empty", "# models\n", "пуст"}, {"bad line", "opus claude-opus-5-5 extra\n", "строка 1"}, {"id на слэш", "x waibee/\n", "строка 1: id без имени модели"}, {"id только вариант", "x [1m]\n", "строка 1: id без имени модели"}, {"id на слэш с вариантом", "x waibee/[1m]\n", "строка 1: id без имени модели"}} {
		t.Run(tc.name, func(t *testing.T) {
			tool := modelGuardForTest(t, tc.contents)
			got, err := tool.ValidateTool(context.Background(), &core.ToolInput{ToolName: "Bash", Command: "claude --model sonnet"})
			if err != nil {
				t.Fatal(err)
			}
			if got.IsValid || !strings.Contains(got.Violations[0].Message, tc.want) {
				t.Fatalf("result=%+v want %q", got, tc.want)
			}
		})
	}
}

func TestModelGuardScan(t *testing.T) {
	dir := t.TempDir()
	approved := filepath.Join(dir, "approved")
	if err := os.WriteFile(approved, []byte("opus claude-opus-5-5\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MODELS_APPROVED", approved)
	transcripts := filepath.Join(dir, "projects", "demo")
	if err := os.MkdirAll(transcripts, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(transcripts, "session.jsonl")
	data := []string{
		`{"type":"assistant","timestamp":"2026-10-21T10:00:00Z","message":{"content":[{"type":"tool_use","name":"Agent","input":{"model":"sonnet"}}]}}`,
		`{"type":"assistant","timestamp":"2026-10-21T10:01:00Z","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"claude --model opus"}}]}}`,
		`{"type":"assistant","timestamp":"2026-10-21T10:02:00Z","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"echo 'claude --model sonnet'"}}]}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(data, "\n")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	logger, _ := core.NewLogger(core.DefaultLoggerConfig())
	tool, _ := NewModelGuardTool(core.ToolConfig{Enabled: true}, logger)
	result, err := ScanModelTranscripts(context.Background(), filepath.Join(dir, "projects"), 7, tool)
	if err != nil {
		t.Fatal(err)
	}
	if result.Checked != 3 || result.Rejected != 1 || result.Mentions != 1 {
		t.Fatalf("scan=%+v", result)
	}
	var out strings.Builder
	WriteModelScan(&out, result)
	if !strings.Contains(out.String(), fmt.Sprintf("%d", result.Rejected)) {
		t.Fatal(out.String())
	}
}

// Строка списка с id через шлюз: id содержит «/», кандидат нормализуется.
func TestModelGuardApprovedIDWithSlash(t *testing.T) {
	models := []approvedModel{
		{"opus", "claude-opus-5-5"},
		{"deepseek", "waibee/deepseek/deepseek-v4.1-flash-1017"},
	}
	cases := []struct {
		candidate string
		want      bool
	}{
		{"waibee/deepseek/deepseek-v4.1-flash-1017", true},
		{"WAIBEE/deepseek/DeepSeek-v4.1-flash-1017", true},
		{"deepseek-v4.1-flash-1017", true},
		{"deepseek", true},
		{"anthropic/claude-opus-5-5", true},
		{"waibee/deepseek/deepseek-v4.1-flash-1017[1m]", true},
		{"openai/opus", true},
		{"waibee/openai/gpt-x", false},
		{"waibee/deepseek/deepseek-v4", false},
		{"waibee", false},
		{"", false},
		{"any/", false},
		// Шлюз назван в строке списка: тот же конец id под другим префиксом - другой маршрут.
		{"openrouter/deepseek/deepseek-v4.1-flash-1017", false},
		{"deepseek/deepseek-v4.1-flash-1017", false},
		{"other/deepseek-v4.1-flash-1017", false},
	}
	for _, tc := range cases {
		t.Run(tc.candidate, func(t *testing.T) {
			if got := isApproved(tc.candidate, models); got != tc.want {
				t.Fatalf("isApproved(%q) = %v, want %v", tc.candidate, got, tc.want)
			}
		})
	}
}

func TestModelGuardApprovedIDWithVariant(t *testing.T) {
	models := []approvedModel{{"opus", "anthropic/claude-opus-5-5[1m]"}}
	for _, candidate := range []string{"anthropic/claude-opus-5-5[1m]", "anthropic/claude-opus-5-5", "claude-opus-5-5[1m]", "claude-opus-5-5"} {
		if !isApproved(candidate, models) {
			t.Errorf("isApproved(%q) = false, want true", candidate)
		}
	}
	if isApproved("other/claude-opus-5-5[1m]", models) {
		t.Error("другой префикс при id с вариантом пропущен")
	}
}

func TestUnguardedMentionsKeepRoute(t *testing.T) {
	path := filepath.Join(t.TempDir(), "approved")
	if err := os.WriteFile(path, []byte("deepseek waibee/deepseek/deepseek-v4.1-flash-1017\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		command string
		want    []string
	}{
		{"echo --model waibee/deepseek/deepseek-v4.1-flash-1017", nil},
		{"echo --model deepseek-v4.1-flash-1017", nil},
		{"echo --model openrouter/deepseek/deepseek-v4.1-flash-1017", []string{"deepseek-v4.1-flash-1017"}},
	}
	for _, tc := range cases {
		t.Run(tc.command, func(t *testing.T) {
			if got := unguardedModelMentions(tc.command, path); !slices.Equal(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// --settings файлом: модель из файла настроек
func TestModelGuardSettingsFile(t *testing.T) {
	tool := modelGuardForTest(t, "opus claude-opus-5-5\n")
	dir := t.TempDir()
	settings := filepath.Join(dir, "s.json")
	if err := os.WriteFile(settings, []byte(`{"model":"sonnet"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	for command, reject := range map[string]bool{
		"claude -p hi --settings " + settings:              true,
		"claude -p hi --settings " + dir + "/missing.json": false,
		// FIFO не читается: чтение повесило бы хук (ревью 12.11)
		"claude -p hi --settings " + fifo: false,
	} {
		got, err := tool.ValidateTool(context.Background(), &core.ToolInput{ToolName: "Bash", Command: command})
		if err != nil {
			t.Fatal(err)
		}
		if got.IsValid == reject {
			t.Errorf("%s: valid=%v %v", command, got.IsValid, got.Violations)
		}
	}
}
