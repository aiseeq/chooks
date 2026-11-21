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
	"time"

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
		{"запасной поиск", "Bash", "", `claude --model sonnet -p "hi`, true},
		{"запасной поиск, переменная", "Bash", "", `ANTHROPIC_MODEL=sonnet claude -p "hi`, true},
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
		// присваивания по разбору mvdan/sh и вызовам shellcmd (тикет kill-guard-shellcmd)
		{"переменная перед обёрткой", "Bash", "", "ANTHROPIC_MODEL=sonnet timeout 5 claude -p hi", true},
		{"переменная перед оболочкой", "Bash", "", "ANTHROPIC_MODEL=sonnet bash -c 'claude -p hi'", true},
		{"переменная в ssh", "Bash", "", "ssh pc 'ANTHROPIC_MODEL=sonnet claude -p hi'", true},
		{"env с опцией", "Bash", "", "env -u X ANTHROPIC_MODEL=sonnet claude -p hi", true},
		{"переменная в here-doc оболочки", "Bash", "", "bash <<'EOF'\nANTHROPIC_MODEL=sonnet claude -p hi\nEOF", true},
		{"переменная в here-doc через конвейер", "Bash", "", "cat <<'EOF' | sudo bash\nANTHROPIC_MODEL=sonnet claude -p hi\nEOF", true},
		{"переменная у команды с claude в $(…)", "Bash", "", `ANTHROPIC_MODEL=sonnet python3 x.py "$(claude --model opus -p hi)"`, false},
		{"запасной поиск, одобренная", "Bash", "", `claude --model opus -p "hi`, false},
		{"запасной поиск без агента", "Bash", "", `echo --model sonnet "hi`, false},
		// env перед оболочкой задаёт модель для claude в её скрипте; пустое
		// значение — модель по умолчанию; присваивание в промпте за env — не
		// присваивание
		{"env перед оболочкой", "Bash", "", `env ANTHROPIC_MODEL=sonnet bash -c 'claude -p hi'`, true},
		{"env у другой команды", "Bash", "", "env ANTHROPIC_MODEL=sonnet python3 x.py; claude --model opus -p hi", false},
		{"env в промпте", "Bash", "", "env FOO=1 claude -p ANTHROPIC_MODEL=sonnet", false},
		{"пустая переменная", "Bash", "", "ANTHROPIC_MODEL= claude -p hi", false},
		{"пустая переменная в кавычках", "Bash", "", "ANTHROPIC_MODEL='' claude -p hi", false},
		{"env пустой переменной", "Bash", "", "env ANTHROPIC_MODEL= claude -p hi", false},
		// незакрытый here-doc: shellcmd его дописывает, mvdan/sh — нет;
		// присваивание ищется в тексте
		{"незакрытый here-doc", "Bash", "", "ANTHROPIC_MODEL=sonnet claude -p hi <<EOF\nx", true},
		{"незакрытый here-doc, одобренная", "Bash", "", "ANTHROPIC_MODEL=opus claude -p hi <<EOF\nx", false},
		{"незакрытый here-doc, пустая", "Bash", "", "ANTHROPIC_MODEL= claude -p hi <<EOF\nx", false},
		// setsid — обёртка, ksh, mksh и fish — оболочки (akit v1.76.1)
		{"setsid", "Bash", "", "setsid -f claude --model sonnet -p hi", true},
		{"setsid сокращённый ключ", "Bash", "", "setsid --fo claude --model sonnet -p hi", true},
		{"setsid одобренная", "Bash", "", "setsid -f claude --model opus -p hi", false},
		{"переменная перед setsid", "Bash", "", "ANTHROPIC_MODEL=sonnet setsid claude -p hi", true},
		{"ksh -c", "Bash", "", "ksh -c 'claude --model sonnet -p hi'", true},
		{"mksh -c", "Bash", "", "mksh -c 'claude --model sonnet -p hi'", true},
		{"fish -c", "Bash", "", "fish -c 'claude --model sonnet -p hi'", true},
		{"fish из echo", "Bash", "", "echo 'claude --model sonnet -p hi' | fish", true},
		{"fish here-string", "Bash", "", "fish <<< 'claude --model sonnet -p hi'", true},
		{"fish -c одобренная", "Bash", "", "fish -c 'claude --model opus -p hi'", false},
		{"переменная в ksh -c", "Bash", "", "ksh -c 'ANTHROPIC_MODEL=sonnet claude -p hi'", true},
		{"env перед fish", "Bash", "", "env ANTHROPIC_MODEL=sonnet fish -c 'claude -p hi'", true},
		// fish вне подмножества akit: разбор даёт только вызов fish и ошибку,
		// модели ищутся в тексте
		{"fish and", "Bash", "", "fish -c 'claude --model sonnet -p hi; and true'", true},
		{"fish set -x", "Bash", "", "fish -c 'set -x ANTHROPIC_MODEL sonnet; claude -p hi'", true},
		{"fish set -gx", "Bash", "", "fish -c 'set -gx ANTHROPIC_MODEL sonnet; claude -p hi; and true'", true},
		{"fish set без ключа", "Bash", "", "fish -c 'set ANTHROPIC_MODEL sonnet; claude -p hi'", true},
		{"fish set -gx одобренной", "Bash", "", "fish -c 'set -gx ANTHROPIC_MODEL opus; claude -p hi'", false},
		{"fish and одобренная", "Bash", "", "fish -c 'claude --model opus -p hi; and true'", false},
		{"fish and codex -m", "Bash", "", "fish -c 'codex exec -m gpt-5 hi; and true'", true},
		{"fish and codex -m одобренная", "Bash", "", "fish -c 'codex exec -m gpt-6-luna hi; and true'", false},
		// текст ищется только в неразобранном скрипте: set -x внутри fish -c
		// не задаёт модель claude за ним; модель из разобранной части
		// команды проверяется и при ошибке в другой
		{"fish set -x до claude снаружи", "Bash", "", "fish -c 'set -x ANTHROPIC_MODEL sonnet' ; claude -p hi", false},
		{"settings рядом с неразобранным fish", "Bash", "", `claude -p hi --settings '{"model":"sonnet"}'; fish -c 'a; and b'`, true},
		{"переменная в bash -c с неразобранным fish", "Bash", "", `bash -c 'ANTHROPIC_MODEL=sonnet claude -p hi; fish -c "a; and b"'`, true},
		// --model у программы не из modelClients — не модель агента
		{"--model у другой программы", "Bash", "", "python3 bench.py --model sonnet", false},
		// присваивание после опций sudo (akit v1.76.2, shellcmd-sudo-assign)
		{"sudo с присваиванием", "Bash", "", "sudo X=1 claude --model sonnet -p hi", true},
		{"sudo -u с присваиванием", "Bash", "", "sudo -u aisee X=1 claude --model sonnet -p hi", true},
		{"переменная модели после sudo", "Bash", "", "sudo ANTHROPIC_MODEL=sonnet claude -p hi", true},
		{"переменная одобренной после sudo", "Bash", "", "sudo ANTHROPIC_MODEL=opus claude -p hi", false},
		// присваивание за промежуточной обёрткой и за sudo -- (тикет kill-guard-gaps-3)
		{"переменная модели за timeout и sudo", "Bash", "", "timeout 5 sudo ANTHROPIC_MODEL=sonnet claude -p hi", true},
		{"переменная одобренной за timeout и sudo", "Bash", "", "timeout 5 sudo ANTHROPIC_MODEL=opus claude -p hi", false},
		{"переменная модели за nice env", "Bash", "", "nice -n 5 env ANTHROPIC_MODEL=sonnet claude -p hi", true},
		{"переменная модели за sudo --", "Bash", "", "sudo -- ANTHROPIC_MODEL=sonnet claude -p hi", true},
		{"переменная модели с / за sudo --", "Bash", "", "sudo -- ANTHROPIC_MODEL=a/sonnet claude -p hi", true},
		{"переменная одобренной за sudo --", "Bash", "", "sudo -- ANTHROPIC_MODEL=opus claude -p hi", false},
		{"--model за sudo -- X=1", "Bash", "", "sudo -- X=1 claude --model sonnet -p hi", true},
		{"--model за sudo -- X=1 и путём к claude", "Bash", "", "sudo -- X=1 /usr/local/bin/claude --model sonnet -p hi", true},
		{"--model одобренной за sudo -- X=1", "Bash", "", "sudo -- X=1 claude --model opus -p hi", false},
		{"переменная модели за timeout и sudo --", "Bash", "", "timeout 5 sudo -u aisee -- ANTHROPIC_MODEL=sonnet claude -p hi", true},
		{"оболочка за sudo --", "Bash", "", "sudo -- ANTHROPIC_MODEL=sonnet bash -c 'claude -p hi'", true},
		{"переменная модели за nice и sudo --", "Bash", "", "nice sudo -- ANTHROPIC_MODEL=sonnet claude -p hi", true},
		{"переменная модели за sudo -- перед timeout", "Bash", "", "sudo -- ANTHROPIC_MODEL=sonnet timeout 5 claude -p hi", true},
		{"переменная модели второй за sudo --", "Bash", "", "sudo -- X=1 ANTHROPIC_MODEL=sonnet claude -p hi", true},
		{"переменная одобренной второй за sudo --", "Bash", "", "sudo -- X=1 ANTHROPIC_MODEL=opus claude -p hi", false},
		{"переменная модели за sudo -- без claude", "Bash", "", "sudo -- ANTHROPIC_MODEL=sonnet make test", false},
		{"sudo -- в тексте echo", "Bash", "", "echo sudo -- ANTHROPIC_MODEL=sonnet claude --model sonnet", false},
		{"sudo -- без claude", "Bash", "", "sudo -- ANTHROPIC_MODEL=sonnet make test", false},
		// codex -m сразу за именем в неразобранном fish
		{"fish and codex -m сразу", "Bash", "", "fish -c 'true; and codex -m gpt-5 exec hi'", true},
		{"fish and codex -m одобренной сразу", "Bash", "", "fish -c 'true; and codex -m gpt-6-luna exec hi'", false},
		// echo печатает текст, а не запускает агента
		{"fish echo с --model", "Bash", "", "fish -c 'echo x; and echo claude --model sonnet'", false},
		{"fish printf с -m", "Bash", "", "fish -c 'true; and printf \"%s\" codex -m gpt-5'", false},
		{"fish запуск после echo", "Bash", "", "fish -c 'echo x; and claude --model sonnet'", true},
		{"fish запуск после not", "Bash", "", "fish -c 'not claude --model sonnet; and true'", true},
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
			// Подсказка — отдельной строкой, не повтором причины; текст —
			// литералом: он называет место правила в ~/.claude/CLAUDE.md
			if tc.reject && (!slices.Equal(got.Suggestions, []string{wantModelGuardHint}) || strings.Contains(got.Violations[0].Message, wantModelGuardHint)) {
				t.Errorf("подсказка: %q %q", got.Violations[0].Message, got.Suggestions)
			}
		})
	}
}

// причина отказа: модель из переменной — «задана переменной»; тело here-doc
// за снятой обёрткой (sudo bash) с теми же перенаправлениями — один раз;
// у одинаковых вызовов — по разу на вызов
func TestModelGuardEnvReasons(t *testing.T) {
	tool := modelGuardForTest(t, "opus claude-opus-5-5\n")
	for _, tc := range []struct {
		command, want string
		count         int
	}{
		{`ANTHROPIC_MODEL="$M" claude -p hi`, "модель задана переменной, назови её литералом", 1},
		// fish вне подмножества akit: присваивание из переменной найдено в тексте
		{"fish -c 'set -x ANTHROPIC_MODEL $M; claude -p hi; and true'", "модель задана переменной, назови её литералом", 1},
		{"sudo bash <<'EOF'\nANTHROPIC_MODEL=sonnet claude -p hi\nEOF", `модель "sonnet" не одобрена`, 1},
		{"env ANTHROPIC_MODEL=sonnet claude -p hi; env ANTHROPIC_MODEL=sonnet claude -p hi", `модель "sonnet" не одобрена`, 2},
	} {
		got, err := tool.ValidateTool(context.Background(), &core.ToolInput{ToolName: "Bash", Command: tc.command})
		if err != nil {
			t.Fatal(err)
		}
		if got.IsValid || strings.Count(got.Violations[0].Message, tc.want) != tc.count {
			t.Errorf("%q: %+v, want %d× %q", tc.command, got.Violations, tc.count, tc.want)
		}
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

func TestModelGuardSettingsTildeForms(t *testing.T) {
	tool := modelGuardForTest(t, "opus claude-opus-5-5\n")
	cwd, home := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	write := func(path, model string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(`{"model":"`+model+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(home, "x"), "opus")
	write(filepath.Join(cwd, "~", "x"), "sonnet")
	write(filepath.Join(home, "q:~", "x", "m.json"), "sonnet")
	write(filepath.Join(home, "q:"+home, "x", "m.json"), "opus")
	for _, tc := range []struct {
		name, command string
		reject        bool
	}{
		{"expanded", "claude --settings ~/x", false},
		{"quoted literal", `claude --settings "~/x"`, true},
		{"escaped literal", `claude --settings \~/x`, true},
		{"only leading tilde expands", `claude --settings ~/q:~/x/m.json -p hi`, true},
		{"неизвестный пользователь", "claude --settings ~root/x", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tool.ValidateTool(context.Background(), &core.ToolInput{ToolName: "Bash", Command: tc.command, CWD: cwd})
			if err != nil {
				t.Fatal(err)
			}
			if got.IsValid == tc.reject {
				t.Fatalf("valid=%v violations=%v, want reject=%v", got.IsValid, got.Violations, tc.reject)
			}
		})
	}
}

// wantModelGuardHint ожидаемая подсказка отказа: раздел и первые слова
// правила о моделях в ~/.claude/CLAUDE.md (номер строки плавает)
const wantModelGuardHint = `Правило ~/.claude/CLAUDE.md, раздел «Что разрешает «да»»: «Модели бери только из ~/.config/models/approved» (кому какая — в шапке файла); бойцы GPT — через worker; новая модель — только через бенчмарк llmark и одобрение пользователя`

// TestModelGuardUserOnly — модель из user_only (Fable) агент не задаёт ни
// Agent, ни клиенту в команде; без поля model суб-агент наследует модель
// сессии (тикет model-guard-fable-user-only)
func TestModelGuardUserOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "approved")
	if err := os.WriteFile(path, []byte("opus claude-opus-5-5\nfable claude-fable-5-2\nluna gpt-6-luna\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MODELS_APPROVED", path)
	logger, err := core.NewLogger(core.DefaultLoggerConfig())
	if err != nil {
		t.Fatal(err)
	}
	tool, _ := NewModelGuardTool(core.ToolConfig{Enabled: true, UserOnly: []string{"fable"}}, logger)
	cases := []struct {
		name, tool, raw, command string
		reject                   bool
	}{
		{"Agent fable", "Agent", `{"model":"fable","subagent_type":"fighter"}`, "", true},
		{"Agent id fable", "Agent", `{"model":"claude-fable-5-2"}`, "", true},
		{"Agent fable [1m]", "Agent", `{"model":"claude-fable-5-2[1m]"}`, "", true},
		{"Agent opus", "Agent", `{"model":"opus"}`, "", false},
		{"Agent без модели", "Agent", `{"subagent_type":"general-purpose"}`, "", false},
		{"claude -p fable", "Bash", "", "claude -p --model fable hi", true},
		{"cl id fable", "Bash", "", "cl --model claude-fable-5-2", true},
		{"cl --model=fable", "Bash", "", "cl --model=fable", true},
		{"cl opus", "Bash", "", "cl --model opus", false},
		{"cl без модели", "Bash", "", "cl", false},
		{"переменная модели", "Bash", "", "ANTHROPIC_MODEL=fable claude -p hi", true},
		{"переменная у cl", "Bash", "", "CLAUDE_CODE_SUBAGENT_MODEL=claude-fable-5-2 cl", true},
		{"worker ask fable", "Bash", "", "worker ask --model fable --class x --msg hi", true},
		{"worker run fable", "Bash", "", "worker run --brief b.md --dir d --class c --model fable", true},
		{"worker luna", "Bash", "", "worker run --brief b.md --dir d --class c --model luna", false},
		{"worker без модели", "Bash", "", "worker run --diff DO --brief b.md --dir d --class c", false},
		{"worker --msg -m", "Bash", "", "worker continue 1 --msg -m hello", false},
		// обходы приёмки 20.11: переменные модели вне прежнего списка
		{"ANTHROPIC_DEFAULT_MODEL", "Bash", "", "ANTHROPIC_DEFAULT_MODEL=claude-fable-5-2 claude -p hi", true},
		{"WORKFLOW_SUBAGENT_MODEL", "Bash", "", "CLAUDE_CODE_WORKFLOW_SUBAGENT_MODEL=fable claude -p hi", true},
		{"DEFAULT_FABLE_MODEL", "Bash", "", "env ANTHROPIC_DEFAULT_FABLE_MODEL=fable claude -p hi", true},
		{"HAIKU fable", "Bash", "", "ANTHROPIC_DEFAULT_HAIKU_MODEL=claude-fable-5-2 claude -p hi", true},
		{"SMALL_FAST fable", "Bash", "", "ANTHROPIC_SMALL_FAST_MODEL=fable claude -p hi", true},
		{"HAIKU в settings env", "Bash", "", `claude -p --settings '{"env":{"ANTHROPIC_SMALL_FAST_MODEL":"fable"}}' hi`, true},
		{"HAIKU своя модель", "Bash", "", "ANTHROPIC_DEFAULT_HAIKU_MODEL=claude-haiku-5-5 claude -p hi", false},
		{"HAIKU из переменной", "Bash", "", `ANTHROPIC_SMALL_FAST_MODEL="$H" claude -p hi`, false},
		{"присваивание, потом export", "Bash", "", "ANTHROPIC_MODEL=fable; export ANTHROPIC_MODEL; claude -p hi", true},
		{"присваивание opus, потом export", "Bash", "", "ANTHROPIC_MODEL=opus; export ANTHROPIC_MODEL; claude -p hi", false},
		{"классификатор fable", "Bash", "", "CLAUDE_CODE_AUTO_MODE_MODEL=fable claude -p hi", true},
		{"фоновый классификатор fable", "Bash", "", "CLAUDE_CODE_BG_CLASSIFIER_MODEL=claude-fable-5-2 claude -p hi", true},
		{"значение модели из предыдущего присваивания", "Bash", "", "F=fable; ANTHROPIC_SMALL_FAST_MODEL=$F claude -p hi", true},
		{"значение классификатора из предыдущего присваивания", "Bash", "", "F=fable; export CLAUDE_CODE_BG_CLASSIFIER_MODEL=$F; claude -p hi", true},
		{"codex -m=fable", "Bash", "", "codex exec -m=fable hi", true},
		{"codex --config=model=fable", "Bash", "", "codex exec --config=model=fable hi", true},
		{"codex разрешённая -m", "Bash", "", "codex exec -m=luna hi", false},
		{"codex разрешённая config", "Bash", "", "codex exec --config=model=luna hi", false},
		{"HAIKU в пишущемся settings", "Bash", "", `echo '{"env":{"ANTHROPIC_DEFAULT_HAIKU_MODEL":"claude-haiku-5-5"}}' > /tmp/mg-absent-h.json; claude --settings /tmp/mg-absent-h.json -p hi`, false},
		{"fable в пишущемся settings env", "Bash", "", `echo '{"env":{"ANTHROPIC_SMALL_FAST_MODEL":"fable"}}' > /tmp/mg-absent-f.json; claude --settings /tmp/mg-absent-f.json -p hi`, true},
		{"--fallback-model у codex", "Bash", "", "codex exec --fallback-model fable hi", false},
		{"--fallback-model у claude", "Bash", "", "claude -p --fallback-model fable hi", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			input := &core.ToolInput{ToolName: c.tool, ToolInput: []byte(c.raw), Command: c.command}
			if c.tool == "Agent" {
				input.Command = ""
			}
			result, err := tool.ValidateTool(context.Background(), input)
			if err != nil {
				t.Fatal(err)
			}
			if result.IsValid == c.reject {
				t.Fatalf("reject=%v, want %v: %+v", !result.IsValid, c.reject, result.Violations)
			}
			if c.reject && (strings.Contains(c.name, "нет такого") || strings.Contains(c.name, "неизвестный") || strings.Contains(c.name, "missing-agent")) && !strings.Contains(result.Violations[0].Message, "определение агента") {
				t.Fatalf("причина неизвестного агента: %+v", result.Violations)
			}
			if c.reject && (strings.Contains(c.name, "cd к проекту") || strings.Contains(c.name, "add-dir") || strings.Contains(c.name, "settings")) && !strings.Contains(result.Violations[0].Message, "запускает только пользователь") {
				t.Fatalf("модель агента/settings не распознана: %+v", result.Violations)
			}
			if c.reject && c.name == "Agent fable" && !strings.Contains(result.Violations[0].Message, "запускает только пользователь") {
				t.Fatalf("причина: %s", result.Violations[0].Message)
			}
		})
	}
}

// TestModelGuardAgentDefinition — модель «только пользователь» в
// определении агента: Agent без model и claude --agent берут её оттуда
// (приёмка 20.11, тикет model-guard-user-only-agent-files)
func TestModelGuardSettingsAgentReference(t *testing.T) {
	called := ""
	candidates := settingsCandidates([]byte(`{"agent":"fx"}`), func(name string) []modelCandidate {
		called = name
		return []modelCandidate{{value: "fable", userOnlyCheck: true}}
	})
	if called != "fx" || len(candidates) != 1 || candidates[0].value != "fable" || !candidates[0].userOnlyCheck {
		t.Fatalf("settings agent resolution: name=%q candidates=%+v", called, candidates)
	}
}

func TestModelGuardRoundTwo(t *testing.T) {
	home, root, outside := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	approved := filepath.Join(t.TempDir(), "approved")
	if err := os.WriteFile(approved, []byte("opus claude-opus-5-5\nfable claude-fable-5-2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MODELS_APPROVED", approved)
	oldManaged := managedSettingsPath
	managedSettingsPath = filepath.Join(t.TempDir(), "managed-settings.json")
	t.Cleanup(func() { managedSettingsPath = oldManaged })
	logger, err := core.NewLogger(core.DefaultLoggerConfig())
	if err != nil {
		t.Fatal(err)
	}
	tool, _ := NewModelGuardTool(core.ToolConfig{Enabled: true, UserOnly: []string{"fable"}}, logger)
	write := func(path, contents string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	project := filepath.Join(root, "repo")
	write(filepath.Join(project, ".git", "HEAD"), "ref: refs/heads/test\n")
	sub := filepath.Join(project, "sub")
	write(filepath.Join(sub, "keep"), "")
	write(filepath.Join(project, ".claude/settings.json"), `{"model":"opus","env":{"ANTHROPIC_SMALL_FAST_MODEL":"fable"}}`)
	plain := filepath.Join(t.TempDir(), "plain")
	write(filepath.Join(plain, ".claude/settings.json"), `{"model":"fable"}`)
	normalized := t.TempDir()
	write(filepath.Join(normalized, ".claude/settings.json"), `{"model":"opus"}`)
	write(filepath.Join(normalized, "opusenv/.claude/settings.json"), `{"env":{"ANTHROPIC_SMALL_FAST_MODEL":"fable"}}`)
	unnormalizedCWD := normalized + string(filepath.Separator) + "opusenv" + string(filepath.Separator) + ".."
	for _, path := range projectSettingsPaths(unnormalizedCWD) {
		if path == filepath.Join(normalized, "opusenv", ".claude", "settings.json") {
			t.Fatal("settings from a child of normalized cwd were included")
		}
	}
	homeFable := filepath.Join(t.TempDir(), "fakehome")
	write(filepath.Join(homeFable, ".claude/settings.json"), `{"model":"fable"}`)
	plugOpus, plugFable := filepath.Join(t.TempDir(), "plug-opus"), filepath.Join(t.TempDir(), "plug-fable")
	write(filepath.Join(plugOpus, "agents/rev.md"), "---\nname: rev\nmodel: opus\n---\n")
	write(filepath.Join(plugFable, "agents/rev.md"), "---\nname: rev\nmodel: fable\n---\n")
	cases := []struct {
		name, tool, cwd, command, raw string
		reject                        bool
	}{
		{"for variable assignment", "Bash", outside, `M=opus; for M in fable; do claude --model "$M" -p hi; done`, "", true},
		{"command substitution reassignment", "Bash", outside, `M=opus; M=$(echo fable); claude --model "$M" -p hi`, "", true},
		{"prefix reassignment", "Bash", outside, `export M=fable; M=opus claude --model "$M" -p hi`, "", true},
		{"read reassignment", "Bash", outside, `M=opus; read M <<< fable; claude --model $M -p hi`, "", true},
		{"printf reassignment", "Bash", outside, `M=opus; printf -v M fable; claude --model $M -p hi`, "", true},
		{"subshell assignment", "Bash", outside, `(M=fable; claude --model $M -p hi)`, "", true},
		{"fast model reassignment", "Bash", outside, `F=opus; F=$(echo fable); ANTHROPIC_SMALL_FAST_MODEL=$F claude`, "", true},
		{"single prior assignment", "Bash", outside, `M=opus; claude --model $M -p hi`, "", false},
		{"cd -P", "Bash", outside, "cd -P " + plain + " && claude", "", true},
		{"cd dash separator", "Bash", outside, "cd -- " + plain + " && claude", "", true},
		{"env -C", "Bash", outside, "env -C " + plain + " claude", "", true},
		{"env chdir equals", "Bash", outside, "env --chdir=" + plain + " claude", "", true},
		{"unknown cwd", "Bash", outside, "cd $D && claude --model opus", "", true},
		{"HOME settings", "Bash", outside, "HOME=" + homeFable + " claude", "", true},
		{"repo settings env ignores model override", "Bash", sub, "claude --model opus", "", true},
		{"project model key overridden", "Bash", outside, "cd " + plain + " && claude --model opus", "", false},
		{"cwd parent path excludes child settings", "Bash", unnormalizedCWD, "claude", "", false},
		{"write settings", "Bash", outside, `echo '{"model":"fable"}' > ` + filepath.Join(outside, "settings.json") + "; claude", "", true},
		{"write agent definition", "Bash", outside, "printf -- '---\\nname: x\\nmodel: fable\\n---\\n' > " + filepath.Join(outside, "agents/x.md") + "; claude --agent x", "", true},
		{"copy unknown settings", "Bash", outside, "cp /not/readable/source " + filepath.Join(outside, "settings.json") + "; claude", "", true},
		{"worker -model", "Bash", outside, "worker run -model fable --brief b --dir d --class c", "", true},
		{"worker equals model", "Bash", outside, "worker ask -model=fable --class c --msg hi", "", true},
		{"opencode equals model", "Bash", outside, "opencode -m=fable run hi", "", true},
		{"npx Claude", "Bash", outside, "npx @anthropic-ai/claude-code --model fable -p hi", "", true},
		{"which Claude substitution", "Bash", outside, "$(which claude) --model fable -p hi", "", true},
		{"echo data", "Bash", outside, "echo fable", "", false},
		{"grep data", "Bash", outside, "grep -r fable docs", "", false},
		{"inline agents opus", "Bash", outside, `claude --agents '{"rev":{"model":"opus"}}' --agent rev -p hi`, "", false},
		{"inline agents fable", "Bash", outside, `claude --agents '{"rev":{"model":"fable"}}' --agent rev -p hi`, "", true},
		{"plugin-dir opus", "Bash", outside, "claude --plugin-dir " + plugOpus + " --agent myplug:rev -p hi", "", false},
		{"plugin-dir fable", "Bash", outside, "claude --plugin-dir " + plugFable + " --agent myplug:rev -p hi", "", true},
		{"unknown Agent plugin allowed", "Agent", outside, "", `{"subagent_type":"frontend-design:frontend-design"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := tool.ValidateTool(context.Background(), &core.ToolInput{ToolName: tc.tool, CWD: tc.cwd, Command: tc.command, ToolInput: []byte(tc.raw)})
			if err != nil {
				t.Fatal(err)
			}
			if result.IsValid == tc.reject {
				t.Fatalf("reject=%v want=%v violations=%+v", !result.IsValid, tc.reject, result.Violations)
			}
			if tc.name == "unknown cwd" && !strings.Contains(fmt.Sprint(result.Violations), "каталог запуска неизвестен: модель не проверить") {
				t.Fatalf("unknown cwd did not produce its dedicated refusal: %+v", result.Violations)
			}
		})
	}
}

func TestModelGuardAgentDefinition(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	approvedPath := filepath.Join(home, "approved")
	files := map[string]string{
		approvedPath: "opus claude-opus-5-5\nfable claude-fable-5-2\n",
		filepath.Join(home, ".claude/agents/fx.md"):                                "---\nname: fx\nmodel: fable\n---\nтело\n",
		filepath.Join(home, ".claude/agents/sub/fy.md"):                            "---\nname: other-name\nmodel: \"claude-fable-5-2\"\n---\n",
		filepath.Join(home, ".claude/agents/ok.md"):                                "---\nname: ok\nmodel: opus\n---\nmodel: fable\n",
		filepath.Join(home, ".claude/agents/hk.md"):                                "---\r\nname: hk\r\nmodel: haiku\r\n---\r\n",
		filepath.Join(home, ".claude/agents/in.md"):                                "---\nname: in\nmodel: inherit\n---\n",
		filepath.Join(home, ".claude/agents/nofm.md"):                              "model: fable\n",
		filepath.Join(project, ".claude/agents/px.md"):                             "---\nname: px\nmodel: fable\n---\n",
		filepath.Join(project, ".claude/agents/settings-agent.md"):                 "---\nname: settings-agent\nmodel: fable\n---\n",
		filepath.Join(home, ".claude/agents/byfile.md"):                            "---\nmodel: fable\n---\n",
		filepath.Join(home, ".claude/agents/notes.txt"):                            "---\nname: txt\nmodel: fable\n---\n",
		filepath.Join(home, ".claude/agents/unclosed.md"):                          "---\nname: unclosed\nmodel: fable\n",
		filepath.Join(home, ".claude/agents/noprefix.md"):                          "name: noprefix\n---\nmodel: fable\n---\n",
		filepath.Join(home, ".claude/agents/emptyname.md"):                         "---\nname: \"\"\nmodel: fable\n---\n",
		filepath.Join(home, ".claude/agents/nokey.md"):                             "---\njust text\nname: nokey\nmodel: fable\n---\n",
		filepath.Join(home, ".claude/agents/spaced.md"):                            "---\nname:   spaced-x  \nmodel:  'fable' \n---\n",
		filepath.Join(home, ".claude/agents/badyaml.md"):                           "---\nname : fb\nmodel: fable\nbad: [unclosed\n  name: other\n---\n",
		filepath.Join(home, ".claude/agents/norm.md"):                              "---\nname: f-dash\nmodel: fable\n---\n",
		filepath.Join(home, ".claude/agents/comment.md"):                           "---\nname: fcomment\nmodel: fable # by user\n---\n",
		filepath.Join(home, ".claude/agents/bom.md"):                               "\xef\xbb\xbf---\nname: fbom\nmodel: fable\n---\n",
		filepath.Join(home, ".claude/agents/trail.md"):                             "---  \nname: ftrail\nmodel: fable\n---  \n",
		filepath.Join(home, ".claude/agents/block.md"):                             "---\nname: fblock\nmodel: >-\n  fable\n---\n",
		filepath.Join(home, ".claude/agents/nested.md"):                            "---\nname: fnn\nmetadata:\n  name: other\nmodel: fable\n---\n",
		filepath.Join(home, ".claude/agents/nestmodel.md"):                         "---\nname: fnm\nmodel: opus\nsrv:\n  model: fable\n---\n",
		filepath.Join(home, ".claude/agents/big.md"):                               "---\nname: fbig\ndescription: " + strings.Repeat("д", 3000) + "\nmodel: fable\n---\n" + strings.Repeat("тело\n", 300000),
		filepath.Join(project, "outside/fl.md"):                                    "---\nname: flink\nmodel: fable\n---\n",
		filepath.Join(home, ".claude/agents/long.md"):                              "---\nname: flong\nmodel: fable\ndescription: " + strings.Repeat("д", 100000) + "\n---\n",
		filepath.Join(home, ".claude/agents", strings.Repeat("d/", 20), "deep.md"): "---\nname: fdeep\nmodel: fable\n---\n",
		filepath.Join(project, "cfg/agents/fc.md"):                                 "---\nname: fconfig\nmodel: fable\n---\n",
		filepath.Join(home, ".claude/plugins/cache/market/plugin/agents/pa.md"):    "---\nname: plug-agent\nmodel: fable\n---\n",
		filepath.Join(home, ".claude/agents/crlf.md"):                              "---\r\nname: crlf\r\nmodel: fable\r\n---\r\n",
		filepath.Join(home, ".claude/agents/override.md"):                          "---\nname: override\nmodel: opus\n---\n",
		filepath.Join(home, ".claude/agents/caseupper.md"):                         "---\nname: caseupper\nmodel: FABLE\n---\n",
		filepath.Join(home, ".claude/agents/onemillion.md"):                        "---\nname: onemillion\nmodel: claude-fable-5-2[1m]\n---\n",
	}
	for path, text := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(project, "outside"), filepath.Join(home, ".claude/agents/linkdir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, ".claude/agents"), filepath.Join(home, ".claude/agents/sub/loop")); err != nil {
		t.Fatal(err)
	}
	deep := filepath.Join(project, "a", "b")
	if err := os.MkdirAll(deep, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(project, "cfg"))
	managedSettingsPath = filepath.Join(project, "managed-settings.json")
	t.Cleanup(func() { managedSettingsPath = "/etc/claude-code/managed-settings.json" })
	t.Setenv("MODELS_APPROVED", approvedPath)
	logger, err := core.NewLogger(core.DefaultLoggerConfig())
	if err != nil {
		t.Fatal(err)
	}
	tool, _ := NewModelGuardTool(core.ToolConfig{Enabled: true, UserOnly: []string{"fable"}}, logger)
	cases := []struct {
		name, tool, raw, command, cwd string
		reject                        bool
	}{
		{"Agent fx", "Agent", `{"subagent_type":"fx"}`, "", home, true},
		{"Agent по name во вложенной папке", "Agent", `{"subagent_type":"other-name"}`, "", home, true},
		{"Agent по имени файла при другом name", "Agent", `{"subagent_type":"fy"}`, "", home, false},
		{"Agent без name — имя файла", "Agent", `{"subagent_type":"byfile"}`, "", home, true},
		{"Agent opus в frontmatter, fable в теле", "Agent", `{"subagent_type":"ok"}`, "", home, false},
		{"Agent haiku CRLF", "Agent", `{"subagent_type":"hk"}`, "", home, false},
		{"Agent inherit", "Agent", `{"subagent_type":"in"}`, "", home, false},
		{"Agent без frontmatter", "Agent", `{"subagent_type":"nofm"}`, "", home, false},
		{"Agent не .md", "Agent", `{"subagent_type":"txt"}`, "", home, false},
		{"Agent незакрытый frontmatter", "Agent", `{"subagent_type":"unclosed"}`, "", home, false},
		{"Agent frontmatter не с начала", "Agent", `{"subagent_type":"noprefix"}`, "", home, false},
		{"Agent пустой name — имя файла", "Agent", `{"subagent_type":"emptyname"}`, "", home, true},
		{"Agent строка без двоеточия", "Agent", `{"subagent_type":"nokey"}`, "", home, true},
		{"Agent пробелы и кавычки", "Agent", `{"subagent_type":"spaced-x"}`, "", home, true},
		{"Agent CRLF fable", "Agent", `{"subagent_type":"crlf"}`, "", home, true},
		{"Agent YAML не разобрался — ключи верхнего уровня", "Agent", `{"subagent_type":"fb"}`, "", home, true},
		{"Agent нормализованное имя", "Agent", `{"subagent_type":"F_Dash"}`, "", home, true},
		{"Agent имя NFKC", "Agent", `{"subagent_type":"ｆ-ｄａｓｈ"}`, "", home, true},
		{"Agent комментарий YAML", "Agent", `{"subagent_type":"fcomment"}`, "", home, true},
		{"Agent BOM", "Agent", `{"subagent_type":"fbom"}`, "", home, true},
		{"Agent пробелы после ---", "Agent", `{"subagent_type":"ftrail"}`, "", home, true},
		{"Agent блочный скаляр", "Agent", `{"subagent_type":"fblock"}`, "", home, true},
		{"Agent вложенный name не подменяет", "Agent", `{"subagent_type":"fnn"}`, "", home, true},
		{"Agent вложенный model не считается", "Agent", `{"subagent_type":"fnm"}`, "", home, false},
		{"Agent файл больше 1 МБ — Claude Code не загрузит", "Agent", `{"subagent_type":"fbig"}`, "", home, false},
		{"Agent frontmatter больше 64 КБ", "Agent", `{"subagent_type":"flong"}`, "", home, true},
		{"Agent имя с тире U+2013", "Agent", `{"subagent_type":"f–dash"}`, "", home, true},
		{"Agent глубже 16 каталогов", "Agent", `{"subagent_type":"fdeep"}`, "", home, true},
		{"Agent в симлинке на каталог", "Agent", `{"subagent_type":"flink"}`, "", home, true},
		{"Agent проекта из подкаталога", "Agent", `{"subagent_type":"px"}`, "", deep, true},
		{"Agent из CLAUDE_CONFIG_DIR", "Agent", `{"subagent_type":"fconfig"}`, "", home, true},
		{"Agent FABLE", "Agent", `{"subagent_type":"caseupper"}`, "", home, true},
		{"Agent fable [1m]", "Agent", `{"subagent_type":"onemillion"}`, "", home, true},
		{"Agent нет такого", "Agent", `{"subagent_type":"absent"}`, "", home, false},
		{"Agent встроенный", "Agent", `{"subagent_type":"general-purpose"}`, "", home, false},
		{"Agent fx с model opus", "Agent", `{"subagent_type":"fx","model":"opus"}`, "", home, false},
		{"Agent fx с пустым model", "Agent", `{"subagent_type":"fx","model":""}`, "", home, true},
		{"Agent проекта", "Agent", `{"subagent_type":"px"}`, "", project, true},
		{"Agent проекта из другого каталога", "Agent", `{"subagent_type":"px"}`, "", home, false},
		{"Agent override opus", "Agent", `{"subagent_type":"override"}`, "", home, false},
		{"claude --agent fx", "Bash", "", "claude -p --agent fx hi", home, true},
		{"cl --agent=fx", "Bash", "", "cl --agent=fx", home, true},
		{"claude --agent ok", "Bash", "", "claude -p --agent ok hi", home, false},
		{"claude --agent из переменной", "Bash", "", `claude -p --agent "$A" hi`, home, true},
		{"claude --agent проекта из подкаталога", "Bash", "", "claude -p --agent px hi", deep, true},
		{"claude cd к проекту", "Bash", "", "cd " + project + " && claude -p --agent px hi", home, true},
		{"claude add-dir", "Bash", "", "claude --add-dir " + project + " -p --agent px hi", home, true},
		{"claude неизвестный агент", "Bash", "", "claude --agent missing-agent", home, true},
		{"claude неизвестный plugin", "Bash", "", "claude --agent plugin:missing-agent", home, true},
		{"claude plugin agent fable", "Bash", "", "claude --agent plugin:plug-agent", home, true},
		{"claude переменный config dir", "Bash", "", "CLAUDE_CONFIG_DIR=$DIR claude", home, true},
		{"codex --agent fx", "Bash", "", "codex exec --agent fx hi", home, false},
	}
	for path, value := range map[string]string{
		filepath.Join(project, ".claude/settings.json"):       `{"model":"opus"}`,
		filepath.Join(project, ".claude/settings.local.json"): `{"agent":"settings-agent"}`,
		filepath.Join(project, "cfg/settings.json"):           `{"model":"opus"}`,
		filepath.Join(project, "cfg-fable/settings.json"):     `{"model":"fable"}`,
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cases = append(cases,
		struct {
			name, tool, raw, command, cwd string
			reject                        bool
		}{"claude project settings", "Bash", "", "cd " + project + " && claude", home, true},
		struct {
			name, tool, raw, command, cwd string
			reject                        bool
		}{"explicit model overrides settings model but not agent", "Bash", "", "cd " + project + " && claude --model opus", home, true},
		struct {
			name, tool, raw, command, cwd string
			reject                        bool
		}{"settings config dir", "Bash", "", "CLAUDE_CONFIG_DIR=" + filepath.Join(project, "cfg-fable") + " claude", home, true},
		struct {
			name, tool, raw, command, cwd string
			reject                        bool
		}{"settings managed", "Bash", "", "cd " + project + " && claude", home, true},
	)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			input := &core.ToolInput{ToolName: c.tool, ToolInput: []byte(c.raw), Command: c.command, CWD: c.cwd}
			start := time.Now()
			result, err := tool.ValidateTool(context.Background(), input)
			if err != nil {
				t.Fatal(err)
			}
			// петля симлинков sub/loop не должна доводить обход до срока agentScanBudget
			if elapsed := time.Since(start); elapsed > agentScanBudget/2 {
				t.Fatalf("обход каталогов агентов %v", elapsed)
			}
			if result.IsValid == c.reject {
				t.Fatalf("reject=%v, want %v: %+v", !result.IsValid, c.reject, result.Violations)
			}
			if c.reject && (strings.Contains(c.name, "нет такого") || strings.Contains(c.name, "неизвестный") || strings.Contains(c.name, "missing-agent")) && !strings.Contains(result.Violations[0].Message, "определение агента") {
				t.Fatalf("причина неизвестного агента: %+v", result.Violations)
			}
			if c.reject && (strings.Contains(c.name, "cd к проекту") || strings.Contains(c.name, "add-dir") || strings.Contains(c.name, "project settings")) && !strings.Contains(result.Violations[0].Message, "запускает только пользователь") {
				t.Fatalf("модель агента/settings не распознана: %+v", result.Violations)
			}
		})
	}
	if err := os.WriteFile(managedSettingsPath, []byte(`{"env":{"ANTHROPIC_SMALL_FAST_MODEL":"fable"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "cfg/settings.json"), []byte(`{"model":"opus"}`), 0600); err != nil {
		t.Fatal(err)
	}
	managedResult, err := tool.ValidateTool(context.Background(), &core.ToolInput{ToolName: "Bash", Command: "cd " + home + " && claude", CWD: home})
	if err != nil || managedResult.IsValid {
		t.Fatalf("managed settings: valid=%v err=%v", managedResult.IsValid, err)
	}
	if !strings.Contains(managedResult.Violations[0].Message, "запускает только пользователь") {
		t.Fatalf("managed settings reason: %+v", managedResult.Violations)
	}
}
