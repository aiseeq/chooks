package statusline

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// env подменяет os.Getenv набором переменных
func env(vars map[string]string) func(string) string {
	return func(key string) string { return vars[key] }
}

func parseSettings(t *testing.T, raw string) compactSettings {
	t.Helper()
	var settings compactSettings
	if err := json.Unmarshal([]byte(raw), &settings); err != nil {
		t.Fatal(err)
	}
	return settings
}

func TestCompactThreshold(t *testing.T) {
	const window = 1000000
	tests := []struct {
		name     string
		model    string
		settings string
		env      map[string]string
		want     int
	}{
		{name: "autoCompactWindow", settings: `{"autoCompactWindow":300000}`, want: 267000},
		{name: "без настройки — полное окно", settings: `{}`, want: 967000},
		{name: "auto — полное окно", settings: `{"autoCompactWindow":"auto"}`, want: 967000},
		{name: "вне пределов — не задано", settings: `{"autoCompactWindow":50000}`, want: 967000},
		{
			name: "окно модели важнее общего", model: "claude-opus-5-5[1m]",
			settings: `{"autoCompactWindow":300000,"modelSettings":{"claude-opus-5-5":{"autoCompactWindow":500000}}}`,
			want:     467000,
		},
		{
			name: "переменная окружения важнее settings", settings: `{"autoCompactWindow":300000}`,
			env: map[string]string{"CLAUDE_CODE_AUTO_COMPACT_WINDOW": "200000"}, want: 167000,
		},
		{
			name: "переменная окружения прижимается к пределу", settings: `{}`,
			env: map[string]string{"CLAUDE_CODE_AUTO_COMPACT_WINDOW": "5000"}, want: 67000,
		},
		{
			name: "процент опускает порог", settings: `{"autoCompactWindow":300000}`,
			env: map[string]string{"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE": "50"}, want: 140000,
		},
		{name: "сжатие выключено в settings", settings: `{"autoCompactEnabled":false,"autoCompactWindow":300000}`, want: 0},
		{
			name: "сжатие выключено переменной", settings: `{"autoCompactWindow":300000}`,
			env: map[string]string{"DISABLE_AUTO_COMPACT": "1"}, want: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := compactThreshold(window, tt.model, parseSettings(t, tt.settings), env(tt.env))
			if got != tt.want {
				t.Errorf("порог %d, ожидался %d", got, tt.want)
			}
		})
	}

	// Окно сжатия больше окна модели — порог по окну модели
	if got := compactThreshold(200000, "", parseSettings(t, `{"autoCompactWindow":300000}`), env(nil)); got != 167000 {
		t.Errorf("окно 200 тыс.: порог %d, ожидался 167000", got)
	}
	// Окно модели неизвестно — порога нет
	if got := compactThreshold(0, "", parseSettings(t, `{"autoCompactWindow":300000}`), env(nil)); got != 0 {
		t.Errorf("без окна модели порог %d, ожидался 0", got)
	}
}

func TestContextUsed(t *testing.T) {
	input := Input{}
	input.ContextWindow.UsedPercentage = 26
	input.ContextWindow.CurrentUsage = &ContextUsage{InputTokens: 2000, CacheCreationInputTokens: 8000, CacheReadInputTokens: 250000}

	if got := contextUsed(input, 267000); fmt.Sprintf("%.0f", got) != "97" {
		t.Errorf("до порога 267000: %.1f%%, ожидалось 97%%", got)
	}
	// Без порога — доля всего окна от Claude Code
	if got := contextUsed(input, 0); got != 26 {
		t.Errorf("без порога: %.1f%%, ожидалось 26%%", got)
	}
	input.ContextWindow.CurrentUsage = nil
	if got := contextUsed(input, 267000); got != 26 {
		t.Errorf("без токенов: %.1f%%, ожидалось 26%%", got)
	}
}

// Кадр demu-master 25.10: Claude Code пишет «3% until auto-compact», а полоса
// показывала 26% зелёным. Теперь она меряет до порога сжатия: 97% красным
func TestRenderContextBarUntilCompaction(t *testing.T) {
	isolateRender(t)
	settings := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "settings.json")
	if err := os.WriteFile(settings, []byte(`{"autoCompactWindow":300000}`), 0o600); err != nil {
		t.Fatal(err)
	}

	input := fmt.Sprintf(`{"session_id":"s1","cwd":%q,"model":{"id":"claude-opus-5-5[1m]","display_name":"Opus 5 (1M context)"},`+
		`"context_window":{"used_percentage":26,"context_window_size":1000000,`+
		`"current_usage":{"input_tokens":2000,"cache_creation_input_tokens":8000,"cache_read_input_tokens":250000}}}`, t.TempDir())
	line, err := Render(strings.NewReader(input), testLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plain(line), "ctx ▓▓▓▓▓▓▓▓▓░ 97%") {
		t.Errorf("полоса должна мерить до порога сжатия: %q", plain(line))
	}
	if !strings.Contains(line, contextShades[len(contextShades)-1]+"▓") {
		t.Errorf("97%% до сжатия — самый густой оттенок полосы: %q", line)
	}
}
