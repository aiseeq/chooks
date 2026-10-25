package statusline

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Порог автосжатия по Claude Code 2.1.310: окно сжатия минус запас на ответ
// модели (min(max_output_tokens, 20000), у нынешних моделей 20000) и буфер
// сводки 13000. С этим порогом сходится его «N% until auto-compact»:
// 260 тыс. токенов при autoCompactWindow 300000 — «3%»
const (
	compactOutputReserve = 20000
	compactSummaryBuffer = 13000
)

// Пределы окна сжатия: значение вне их Claude Code в settings отвергает, а из
// переменной окружения прижимает к краю
const (
	compactWindowMin = 100000
	compactWindowMax = 1000000
)

// compactSettings — ключи settings.json, от которых зависит порог автосжатия
type compactSettings struct {
	AutoCompactEnabled *bool           `json:"autoCompactEnabled"`
	AutoCompactWindow  json.RawMessage `json:"autoCompactWindow"`
	ModelSettings      map[string]struct {
		AutoCompactWindow json.RawMessage `json:"autoCompactWindow"`
	} `json:"modelSettings"`
}

// loadCompactSettings читает settings.json пользователя. Файла нет — порог
// штатный, это не сбой
func loadCompactSettings(claudeDir string) (compactSettings, error) {
	var settings compactSettings
	if claudeDir == "" {
		return settings, errors.New("claude config dir unknown")
	}
	data, err := os.ReadFile(filepath.Join(claudeDir, "settings.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return settings, nil
	}
	if err != nil {
		return settings, fmt.Errorf("read settings: %w", err)
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return settings, fmt.Errorf("parse settings: %w", err)
	}
	return settings, nil
}

// compactThreshold — число токенов контекста, на котором Claude Code сожмёт
// историю; 0 — сжатие выключено или размер окна неизвестен. Окно — как у
// Claude Code: CLAUDE_CODE_AUTO_COMPACT_WINDOW, затем autoCompactWindow
// модели в modelSettings, затем общий, иначе полное окно модели; не больше
// окна модели. CLAUDE_AUTOCOMPACT_PCT_OVERRIDE опускает порог
func compactThreshold(contextWindow int, model string, settings compactSettings, getenv func(string) string) int {
	if contextWindow <= 0 || !compactEnabled(settings, getenv) {
		return 0
	}

	window := contextWindow
	if configured, ok := configuredWindow(model, settings, getenv); ok {
		window = min(window, configured)
	}

	effective := window - compactOutputReserve
	threshold := effective - compactSummaryBuffer
	if pct, err := strconv.ParseFloat(getenv("CLAUDE_AUTOCOMPACT_PCT_OVERRIDE"), 64); err == nil && pct > 0 && pct <= 100 {
		threshold = min(int(float64(effective)*pct/100), threshold)
	}
	if threshold <= 0 {
		return 0
	}
	return threshold
}

// compactEnabled — включено ли автосжатие: autoCompactEnabled не false и
// сжатие не выключено переменной окружения
func compactEnabled(settings compactSettings, getenv func(string) string) bool {
	if settings.AutoCompactEnabled != nil && !*settings.AutoCompactEnabled {
		return false
	}
	return !envTruthy(getenv("DISABLE_AUTO_COMPACT")) && !envTruthy(getenv("DISABLE_COMPACT"))
}

// configuredWindow — окно сжатия, заданное пользователем; false — не задано
// или "auto", тогда окно — полное окно модели
func configuredWindow(model string, settings compactSettings, getenv func(string) string) (int, bool) {
	if tokens, err := strconv.Atoi(strings.TrimSpace(getenv("CLAUDE_CODE_AUTO_COMPACT_WINDOW"))); err == nil {
		return min(max(tokens, compactWindowMin), compactWindowMax), true
	}
	// Ключ modelSettings — каноническое имя модели; id во входе бывает с
	// суффиксом окна: claude-opus-5-5[1m]
	if open := strings.IndexByte(model, '['); open > 0 {
		model = model[:open]
	}
	if perModel, ok := settings.ModelSettings[model]; ok {
		if tokens, ok := windowValue(perModel.AutoCompactWindow); ok {
			return tokens, true
		}
	}
	return windowValue(settings.AutoCompactWindow)
}

// windowValue разбирает autoCompactWindow: число в пределах окна сжатия;
// "auto", пустое и вне пределов — не задано
func windowValue(raw json.RawMessage) (int, bool) {
	var tokens float64
	if json.Unmarshal(raw, &tokens) != nil {
		return 0, false
	}
	if tokens < compactWindowMin || tokens > compactWindowMax {
		return 0, false
	}
	return int(tokens), true
}

// envTruthy — переменная окружения включена: 1, true, yes, on
func envTruthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
