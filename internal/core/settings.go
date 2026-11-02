package core

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"
)

// SettingsHook — команда хука из settings.json Claude Code
type SettingsHook struct {
	Event string
	// Subcommand — последнее слово команды: путь к бинарю не важен
	Subcommand string
	// Timeout ноль — в settings.json не задан
	Timeout time.Duration
}

// SettingsHooks читает команды хуков из settings.json. Файла нет — хуков нет.
// Событие, которое не разобрать (чужая ошибка в settings.json), не прячет
// остальные: разобранные хуки возвращаются вместе с ошибкой
func SettingsHooks(settingsPath string) ([]SettingsHook, error) {
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read settings: %w", err)
	}
	var settings struct {
		Hooks map[string]json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return nil, fmt.Errorf("parse settings %s: %w", settingsPath, err)
	}
	var hooks []SettingsHook
	var errs []error
	for event, raw := range settings.Hooks {
		var groups []struct {
			Hooks []struct {
				Command string  `json:"command"`
				Timeout float64 `json:"timeout"`
			} `json:"hooks"`
		}
		if err := json.Unmarshal(raw, &groups); err != nil {
			errs = append(errs, fmt.Errorf("parse %s hooks in %s: %w", event, settingsPath, err))
			continue
		}
		for _, group := range groups {
			for _, hook := range group.Hooks {
				fields := strings.Fields(hook.Command)
				if len(fields) < 2 {
					continue
				}
				hooks = append(hooks, SettingsHook{
					Event:      event,
					Subcommand: fields[len(fields)-1],
					Timeout:    time.Duration(hook.Timeout * float64(time.Second)),
				})
			}
		}
	}
	// Порядок событий в карте случаен, а один и тот же файл даёт один ответ
	slices.SortStableFunc(hooks, func(a, b SettingsHook) int {
		return cmp.Or(strings.Compare(a.Event, b.Event), strings.Compare(a.Subcommand, b.Subcommand))
	})
	slices.SortFunc(errs, func(a, b error) int { return strings.Compare(a.Error(), b.Error()) })
	return hooks, errors.Join(errs...)
}
