package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiseeq/chooks/internal/core"
	"github.com/aiseeq/chooks/internal/understandcheck"
)

// fileLogger — настоящий логгер в файл: журнал отказов читают как файл
func fileLogger(t *testing.T) (core.Logger, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "chooks.log")
	logger, err := core.NewLogger(core.LoggerConfig{Level: "debug", Output: "file", LogFile: path})
	if err != nil {
		t.Fatal(err)
	}
	return logger, path
}

func readLog(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// fakeGitHubToken собран из частей: целиком в исходнике его не должно быть
func fakeGitHubToken() string { return "ghp" + "_" + "abcDEF123456789xyzQWEabcDEF12345678" }

func maskedCommand(t *testing.T, input *core.ToolInput) string {
	t.Helper()
	got, err := deniedCommand(input)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func assertDenyLine(t *testing.T, log string, want ...string) {
	t.Helper()
	for _, part := range want {
		if !strings.Contains(log, part) {
			t.Errorf("no %q in the log:\n%s", part, log)
		}
	}
	if strings.Contains(log, fakeGitHubToken()) {
		t.Errorf("secret value in the log:\n%s", log)
	}
}

func TestRunPreToolUse_DenyLogsSessionToolMaskedCommand(t *testing.T) {
	config := &core.Config{Tools: map[string]core.ToolConfig{"kill_guard": {Enabled: true}}}
	logger, path := fileLogger(t)
	data, err := json.Marshal(map[string]any{
		"session_id": "sess-1",
		"tool_name":  "Bash",
		"tool_input": map[string]string{"command": `TOKEN=` + fakeGitHubToken() + `; pkill -f "$NAME"`},
	})
	if err != nil {
		t.Fatal(err)
	}

	code, err := runPreToolUse(context.Background(), data, config, logger)
	if err != nil {
		t.Fatal(err)
	}
	if code != exitBlocked {
		t.Fatalf("exit code %d, want the call denied", code)
	}
	assertDenyLine(t, readLog(t, path), "hook blocked operation", "session=sess-1", "tool=Bash", "pkill", "[скрыто:github]")
}

func TestWriteUnderstandOutcome_DenyLogsMaskedCommand(t *testing.T) {
	logger, path := fileLogger(t)
	input := &core.ToolInput{SessionID: "sess-2", ToolName: "Bash", Command: "echo " + fakeGitHubToken()}

	writeUnderstandOutcome(understandcheck.Outcome{Deny: true, Reason: "поправка"}, input, logger, &bytes.Buffer{})

	assertDenyLine(t, readLog(t, path), "understand check denied call", "session=sess-2", "tool=Bash", "[скрыто:github]")
}

func TestRunNoPrompt_DenyLogsMaskedCommand(t *testing.T) {
	logger, path := fileLogger(t)
	data, err := json.Marshal(map[string]any{
		"session_id":      "sess-3",
		"hook_event_name": "PermissionRequest",
		"tool_name":       "Bash",
		"tool_input":      map[string]string{"command": "echo " + fakeGitHubToken()},
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := runNoPrompt(data, logger, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	assertDenyLine(t, readLog(t, path), "prompt denied", "session=sess-3", "tool=Bash", "[скрыто:github]")
}

// Окно отказывает и при неразборчивом tool_input: строка остаётся, без команды
func TestRunNoPrompt_UnreadableToolInputStillDenies(t *testing.T) {
	logger, path := fileLogger(t)
	data := []byte(`{"session_id":"s","hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":42}`)

	var out bytes.Buffer
	if err := runNoPrompt(data, logger, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"deny"`) {
		t.Errorf("no deny in the output: %s", out.String())
	}
	assertDenyLine(t, readLog(t, path), "command unreadable", "prompt denied")
}

func TestDeniedCommand(t *testing.T) {
	long := strings.Repeat("я", deniedCommandRunes+10)
	tests := map[string]struct {
		input *core.ToolInput
		want  string
	}{
		"bash — сама команда": {
			&core.ToolInput{ToolName: "Bash", Command: "ls", ToolInput: json.RawMessage(`{"command":"ls","timeout":5}`)},
			"ls",
		},
		"другой инструмент — tool_input": {
			&core.ToolInput{ToolName: "Write", ToolInput: json.RawMessage(`{"file_path":"a"}`)},
			`{"file_path":"a"}`,
		},
		"срез с пометкой": {
			&core.ToolInput{ToolName: "Bash", Command: long},
			long[:deniedCommandRunes*len("я")] + " …[обрезано]",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := maskedCommand(t, tt.input); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// Строки пробы из тикета akit secretmask-command-context (и формы из выпуска v1.37.0):
// значение hunter2pass не должно дойти до журнала ни в Bash, ни в tool_input другого инструмента
var commandsWithPassword = []string{
	`curl -H 'Authorization: Bearer hunter2pass' https://x`,
	`curl -H "Authorization: Bearer hunter2pass" https://x`,
	`curl -u admin:hunter2pass https://x`,
	`curl -su admin:hunter2pass https://x`,
	`PGPASSWORD=hunter2pass psql -h db`,
	`mysql --password=hunter2pass -u root`,
	`mysql --password hunter2pass -u root`,
	`mysql -phunter2pass`,
	`mariadb -phunter2pass`,
	`sshpass -p hunter2pass ssh host`,
	`psql postgres://u:` + "hunter2pass" + `@db/x`, // по частям, как fakeGitHubToken: целиком в исходнике ключа нет
	`export API_TOKEN=hunter2pass`,
	`curl -d 'password=hunter2pass' x`,
	`curl -d '{"password":"hunter2pass"}' x`,
	`curl -d '{"Authorization":"Bearer hunter2pass"}' x`,
	`ssh h "mysql -phunter2pass"`,
	`deploy --token hunter2pass`,
	"mysql \\\n  --password=hunter2pass -u root",
}

func TestDeniedCommand_HidesPasswordsAndTokens(t *testing.T) {
	for _, command := range commandsWithPassword {
		t.Run(command, func(t *testing.T) {
			toolInput, err := json.Marshal(map[string]string{"content": command})
			if err != nil {
				t.Fatal(err)
			}
			inputs := map[string]*core.ToolInput{
				"bash":  {ToolName: "Bash", Command: command},
				"other": {ToolName: "Write", ToolInput: toolInput},
			}
			for tool, input := range inputs {
				got := maskedCommand(t, input)
				if strings.Contains(got, "hunter2pass") {
					t.Errorf("%s: secret value in %q", tool, got)
				}
				if !strings.Contains(got, "[скрыто:") {
					t.Errorf("%s: no mask in %q", tool, got)
				}
			}
		})
	}
}

// Журналу нужна команда как есть: обычные команды маска не трогает
func TestDeniedCommand_KeepsPlainCommands(t *testing.T) {
	for _, command := range []string{`psql -h db -p 5432`, `grep -p foo`, `TOKEN_COUNT=5`, `go test ./...`} {
		if got := maskedCommand(t, &core.ToolInput{ToolName: "Bash", Command: command}); got != command {
			t.Errorf("got %q, want %q", got, command)
		}
	}
}

// Вся дорога до файла: отказ хука с паролем в команде оставляет в журнале команду без значения
func TestRunPreToolUse_DenyLogHidesPassword(t *testing.T) {
	config := &core.Config{Tools: map[string]core.ToolConfig{"kill_guard": {Enabled: true}}}
	logger, path := fileLogger(t)
	data, err := json.Marshal(map[string]any{
		"session_id": "sess-4",
		"tool_name":  "Bash",
		"tool_input": map[string]string{"command": `mysql -phunter2pass; pkill -f "$NAME"`},
	})
	if err != nil {
		t.Fatal(err)
	}

	code, err := runPreToolUse(context.Background(), data, config, logger)
	if err != nil {
		t.Fatal(err)
	}
	if code != exitBlocked {
		t.Fatalf("exit code %d, want the call denied", code)
	}
	log := readLog(t, path)
	assertDenyLine(t, log, "hook blocked operation", "session=sess-4", "tool=Bash", "pkill", "[скрыто:password]")
	if strings.Contains(log, "hunter2pass") {
		t.Errorf("secret value in the log:\n%s", log)
	}
}

// Перевод строки в tool_input — литерал \n в сыром JSON: маска должна видеть его переводом строки
func TestDeniedCommand_HidesPasswordInMultilineToolInput(t *testing.T) {
	tests := map[string]map[string]any{
		"Write, curl":   {"file_path": "/x/a.sh", "content": "#!/bin/sh\ncurl -u admin:hunter2pass https://x\n"},
		"Write, mysql":  {"content": "cd /x\nmysql -phunter2pass\n"},
		"Monitor, tab":  {"command": "cd /x\n\tsshpass -p hunter2pass ssh host"},
		"Agent, prompt": {"prompt": "запусти\nmysql -p'hunter2pass'"},
		"вложенное":     {"edits": []any{map[string]any{"new_string": "x\ncurl -u a:hunter2pass y"}}},
		"поле по ключу": {"password": "hunter2pass"},
	}
	for name, fields := range tests {
		t.Run(name, func(t *testing.T) {
			raw, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			got := maskedCommand(t, &core.ToolInput{ToolName: "Write", ToolInput: raw})
			if strings.Contains(got, "hunter2pass") {
				t.Errorf("secret value in %q", got)
			}
			if !strings.Contains(got, "[скрыто:") {
				t.Errorf("no mask in %q", got)
			}
		})
	}
}

// Пути второй независимой проверки: tool_input строкой и команда в ключе JSON
func TestDeniedCommand_HidesPasswordInOddToolInput(t *testing.T) {
	for name, raw := range map[string]string{
		"строка":    `"{\"script\": \"#!/bin/sh\\ncurl -u admin:hunter2pass https://x\"}"`,
		"ключ JSON": `{"#!/bin/sh\ncurl -u admin:hunter2pass https://x":1}`,
		"массив":    `["#!/bin/sh\ncurl -u admin:hunter2pass https://x"]`,
	} {
		t.Run(name, func(t *testing.T) {
			got := maskedCommand(t, &core.ToolInput{ToolName: "Write", ToolInput: json.RawMessage(raw)})
			if strings.Contains(got, "hunter2pass") {
				t.Errorf("secret value in %q", got)
			}
		})
	}
}

// Что маска скрывает в команде, она скрывает и в строке tool_input. Формы третьей независимой
// проверки: в сыром JSON каждая оставляет значение видным, b15d848 это вернул
func TestDeniedCommand_ToolInputHidesWhatBashHides(t *testing.T) {
	forms := []string{
		"api_token: abcdefghij\npassword: hunter2pass\n",
		"secret: abcdefghij\npassword: hunter2pass\n",
		"API_TOKEN=$VAULT_TOKEN\nPASSWORD=hunter2pass\n",
		"mysql --password \\\n  hunter2pass -u root",
		"deploy --password\thunter2pass",
		"sshpass -V\nsshpass -p hunter2pass ssh host",
		`curl -u admin:"hunter2pass x" https://y`,
	}
	for _, form := range forms {
		t.Run(form, func(t *testing.T) {
			if got := maskedCommand(t, &core.ToolInput{ToolName: "Bash", Command: form}); strings.Contains(got, "hunter2pass") {
				t.Fatalf("bash: secret value in %q", got)
			}
			for _, field := range []string{"content", "new_string", "prompt"} {
				raw, err := json.Marshal(map[string]string{field: form})
				if err != nil {
					t.Fatal(err)
				}
				if got := maskedCommand(t, &core.ToolInput{ToolName: "Write", ToolInput: raw}); strings.Contains(got, "hunter2pass") {
					t.Errorf("%s: secret value in %q", field, got)
				}
			}
		})
	}
}

func TestDeniedCommand_ToolInputStaysReadable(t *testing.T) {
	tests := map[string]struct{ raw, want string }{
		"число, & и не-ASCII как есть": {`{"a":"x && y","n":5000000,"z":"я"}`, `{"a":"x && y","n":5000000,"z":"я"}`},
		"пустой": {``, ``},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := maskedCommand(t, &core.ToolInput{ToolName: "Write", ToolInput: json.RawMessage(tt.raw)})
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// Неразобранный tool_input в журнал не идёт: в строке об отказе вместо команды причина
func TestDeniedCallFields_UnreadableToolInputNotLogged(t *testing.T) {
	logger, path := fileLogger(t)
	input := &core.ToolInput{SessionID: "sess-5", ToolName: "Write", ToolInput: json.RawMessage(`{"secret":"hunter2pass"`)}

	logger.Info("denied", deniedCallFields(input)...)

	log := readLog(t, path)
	assertDenyLine(t, log, "session=sess-5", "tool=Write", "command_error=")
	if strings.Contains(log, "hunter2pass") {
		t.Errorf("secret value in the log:\n%s", log)
	}
}
