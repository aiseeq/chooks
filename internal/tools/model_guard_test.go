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
		{"Agent без модели", "Agent", `{"subagent_type":"fighter"}`, "", false},
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
			if c.reject && c.name == "Agent fable" && !strings.Contains(result.Violations[0].Message, "запускает только пользователь") {
				t.Fatalf("причина: %s", result.Violations[0].Message)
			}
		})
	}
}
