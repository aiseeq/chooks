package core

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/aiseeq/chooks/internal/hookctx"
	"gopkg.in/yaml.v3"
)

// Config основная конфигурация хуков
type Config struct {
	Tools  map[string]ToolConfig `yaml:"tools"`
	Logger LoggerConfig          `yaml:"logger"`
	Style  StyleConfig           `yaml:"style"`
	Daemon DaemonConfig          `yaml:"daemon,omitempty"`
}

// DefaultFallbackAlert — порог уведомления о запасном пути по умолчанию:
// уходов в CLI за скользящий час (тикет hooks-daemon, этап 2б-2)
const DefaultFallbackAlert = 10

// DaemonConfig — демон хуков chooks serve и его запасной путь
type DaemonConfig struct {
	// FallbackAlert — сколько вызовов прокси chooks-hook, исполненных CLI мимо
	// демона, терпится за скользящий час: на следующем — одно уведомление на
	// рабочий стол, не чаще раза в час. 0 — DefaultFallbackAlert,
	// отрицательное — без уведомления
	FallbackAlert int `yaml:"fallback_alert"`
	// Shadow — CLI, исполнив сравниваемый хук, шлёт демону теневую запись
	// (запрос, свой результат, время, ответы /proc, состояние до и после), и
	// демон сравнивает с ней своё исполнение без эффектов (этап 2б-3а).
	// По умолчанию выключено
	Shadow bool `yaml:"shadow"`
}

// FallbackThreshold — порог уведомления о запасном пути; 0 — уведомления нет
func (d DaemonConfig) FallbackThreshold() int {
	switch {
	case d.FallbackAlert < 0:
		return 0
	case d.FallbackAlert == 0:
		return DefaultFallbackAlert
	default:
		return d.FallbackAlert
	}
}

// StyleConfig — ключи задач для chooks style: префиксы проектов и адрес
// задачи. Имена рабочих проектов — только в конфиге пользователя, не в коде
type StyleConfig struct {
	// KeyPrefixes — префиксы ключей задач («PROJ»); --keys их заменяет
	KeyPrefixes []string `yaml:"key_prefixes"`
	// IssueURL — адрес задачи без ключа («https://jira.example/browse/»)
	// для префиксов KeyPrefixes: черновик получает голый URL с ключом
	IssueURL string `yaml:"issue_url"`
}

// ToolConfig конфигурация инструмента
type ToolConfig struct {
	Enabled bool `yaml:"enabled"`
	// RuntimeLaunchGuard включает проверку модели самого процесса claude
	// при UserPromptSubmit. nil означает включено.
	RuntimeLaunchGuard *bool `yaml:"runtime_launch_guard"`

	// Специфичные для notifier
	Sound   bool `yaml:"sound"`
	Desktop bool `yaml:"desktop"`
	// ActivateOnClick переводит фокус на окно терминала по клику на уведомление (KDE)
	ActivateOnClick bool `yaml:"activate_on_click"`

	// UserOnly (model_guard) — имена моделей из ~/.config/models/approved,
	// которые запускает только пользователь: агент не задаёт их ни Agent,
	// ни клиенту в команде (пользователь 13.11 и 20.11 о Fable)
	UserOnly []string `yaml:"user_only"`

	// CodexHomes (claim_check) — каталоги учёток codex (CODEX_HOME) по
	// порядку: учётка без кредитов метится на час, судья идёт на следующую.
	// Пусто — ~/.codex, затем ~/.codex-personal; ~/ раскрывается по HOME запроса
	CodexHomes []string `yaml:"codex_homes"`
}

// LoadConfig загружает конфигурацию из файла.
// Если файл отсутствует, возвращается конфигурация по умолчанию без записи на диск:
// хук вызывается часто и параллельно, поэтому побочные эффекты здесь недопустимы
func LoadConfig(configPath string) (*Config, error) {
	if configPath == "" {
		var err error
		configPath, err = DefaultConfigPath()
		if err != nil {
			return nil, err
		}
	}
	config, present, err := loadConfig(configPath)
	if err != nil || !present {
		return config, err
	}
	if err := expandConfigPaths(config); err != nil {
		return nil, fmt.Errorf("failed to expand paths in %s: %w", configPath, err)
	}
	return finishConfig(config, configPath)
}

// LoadConfigForRequest загружает конфигурацию по окружению запроса (флаг или
// $XDG_CONFIG_HOME по правилам UserConfigDir из пакета os). Относительный путь
// открывается от cwd запроса (hookctx.Context.Abs), в ошибках — открытый путь
func LoadConfigForRequest(configPath string, request hookctx.Context) (*Config, error) {
	if configPath == "" {
		dir, err := request.ConfigDir()
		if err != nil {
			return nil, fmt.Errorf("failed to resolve default config path: %w", err)
		}
		configPath = filepath.Join(dir, "chooks", "config.yaml")
	}
	configPath = request.Abs(configPath)
	expand := func(config *Config) error {
		path := config.Logger.LogFile
		if strings.HasPrefix(path, "~/") {
			home := request.Path(hookctx.EnvHome)
			if home == "" {
				return fmt.Errorf("cannot expand ~ in %q: $HOME is not defined", path)
			}
			path = filepath.Join(home, path[2:])
		}
		if path != "" {
			path = request.Abs(path)
		}
		config.Logger.LogFile = path
		return nil
	}
	config, present, err := loadConfig(configPath)
	if err != nil || !present {
		return config, err
	}
	if err := expand(config); err != nil {
		return nil, fmt.Errorf("failed to expand paths in %s: %w", configPath, err)
	}
	return finishConfig(config, configPath)
}

// loadConfig читает файл; present — файл есть, иначе значения по умолчанию
func loadConfig(configPath string) (*Config, bool, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return DefaultConfig(), false, nil
		}
		return nil, false, fmt.Errorf("failed to read config file %s: %w", configPath, err)
	}

	// Неизвестные ключи — ошибка: молча проигнорированная опечатка в имени поля
	// означает незаметно отключённую проверку
	var config Config
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	// io.EOF означает пустой файл — работаем на значениях по умолчанию
	if err := decoder.Decode(&config); err != nil && !errors.Is(err, io.EOF) {
		return nil, false, fmt.Errorf("failed to parse config file %s: %w", configPath, err)
	}

	applyLoggerDefaults(&config.Logger)
	return &config, true, nil
}

func finishConfig(config *Config, configPath string) (*Config, error) {
	if err := validateConfig(config); err != nil {
		return nil, fmt.Errorf("config validation failed in %s: %w", configPath, err)
	}

	return config, nil
}

// SaveConfig сохраняет конфигурацию в файл
func SaveConfig(config *Config, configPath string) error {
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	data, err := yaml.Marshal(config)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	if err := os.WriteFile(configPath, data, 0o644); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}

	return nil
}

// DefaultConfig возвращает конфигурацию по умолчанию
func DefaultConfig() *Config {
	return &Config{
		Tools: map[string]ToolConfig{
			"git_guard": {
				Enabled: true,
			},
			"jira_style": {
				Enabled: true,
			},
			"kill_guard": {
				Enabled: true,
			},
			"model_guard": {
				Enabled: false,
			},
			"rm_guard": {
				Enabled: true,
			},
			"rsync_guard": {
				Enabled: true,
			},
			"write_guard": {
				Enabled: true,
			},
			"skill_reload": {
				Enabled: true,
			},

			"notifier": {
				Enabled:         true,
				Sound:           true,
				Desktop:         true,
				ActivateOnClick: true,
			},
		},
		Logger: DefaultLoggerConfig(),
	}
}

// validateConfig проверяет корректность конфигурации
func validateConfig(config *Config) error {
	validLevels := []string{"debug", "info", "warn", "warning", "error"}
	if !containsFold(validLevels, config.Logger.Level) {
		return fmt.Errorf("invalid logger level: %q (expected one of %s)",
			config.Logger.Level, strings.Join(validLevels, ", "))
	}

	validFormats := []string{"text", "json"}
	if !containsFold(validFormats, config.Logger.Format) {
		return fmt.Errorf("invalid logger format: %q (expected one of %s)",
			config.Logger.Format, strings.Join(validFormats, ", "))
	}

	validOutputs := []string{"stdout", "stderr", "file"}
	if !containsFold(validOutputs, config.Logger.Output) {
		return fmt.Errorf("invalid logger output: %q (expected one of %s)",
			config.Logger.Output, strings.Join(validOutputs, ", "))
	}

	if config.Logger.Output == "file" && config.Logger.LogFile == "" {
		return fmt.Errorf("logger.file is required when output is 'file'")
	}

	if config.Logger.MaxSizeMB < 0 {
		return fmt.Errorf("logger.max_size_mb must not be negative, got %d", config.Logger.MaxSizeMB)
	}

	return nil
}

// DefaultConfigPath возвращает путь к конфигурации по умолчанию:
// $XDG_CONFIG_HOME/chooks/config.yaml
func DefaultConfigPath() (string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("failed to resolve default config path: %w", err)
	}
	return filepath.Join(configDir, "chooks", "config.yaml"), nil
}

// containsFold проверяет содержится ли элемент в слайсе без учета регистра
func containsFold(slice []string, item string) bool {
	for _, s := range slice {
		if strings.EqualFold(s, item) {
			return true
		}
	}
	return false
}

// expandPath раскрывает ~ в пути к домашней директории
func expandPath(path string) (string, error) {
	if !strings.HasPrefix(path, "~/") {
		return path, nil
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot expand ~ in %q: %w", path, err)
	}
	return filepath.Join(homeDir, path[2:]), nil
}

// expandConfigPaths применяет expandPath ко всем путям в конфигурации
func expandConfigPaths(config *Config) error {
	logFile, err := expandPath(config.Logger.LogFile)
	if err != nil {
		return err
	}
	config.Logger.LogFile = logFile
	return nil
}
