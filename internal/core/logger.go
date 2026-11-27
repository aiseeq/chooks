package core

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aiseeq/akit/secretmask"
)

// defaultMaxLogSizeMB размер лога, после которого выполняется ротация.
// Без него файл растет неограниченно: хук вызывается на каждую операцию Claude Code
const defaultMaxLogSizeMB = 16

// LoggerConfig конфигурация логгера
type LoggerConfig struct {
	Level     string `yaml:"level"`       // debug, info, warn, error
	Format    string `yaml:"format"`      // text, json
	Output    string `yaml:"output"`      // stdout, stderr, file
	LogFile   string `yaml:"file"`        // путь к файлу лога (если output = file)
	MaxSizeMB int    `yaml:"max_size_mb"` // порог ротации, 0 — отключить ротацию
}

// DefaultLoggerConfig возвращает конфигурацию логгера по умолчанию
func DefaultLoggerConfig() LoggerConfig {
	return LoggerConfig{
		Level:     "info",
		Format:    "text",
		Output:    "stderr",
		MaxSizeMB: defaultMaxLogSizeMB,
	}
}

// applyLoggerDefaults подставляет значения по умолчанию для незаполненных полей
func applyLoggerDefaults(config *LoggerConfig) {
	defaults := DefaultLoggerConfig()
	if config.Level == "" {
		config.Level = defaults.Level
	}
	if config.Format == "" {
		config.Format = defaults.Format
	}
	if config.Output == "" {
		config.Output = defaults.Output
	}
	if config.MaxSizeMB == 0 {
		config.MaxSizeMB = defaults.MaxSizeMB
	}
}

// Logger интерфейс для структурированного логирования
type Logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
	With(args ...any) Logger
}

// FileLogger — логгер запроса, который владеет открытым им файлом лога.
// Close закрывает файл по концу обработки запроса: в долгоживущем процессе
// дескриптор иначе висит до финализатора GC. Вывод в stderr Close не трогает
type FileLogger interface {
	Logger
	Close() error
}

// slogLogger обертка вокруг slog.Logger; file — открытый им файл лога или nil
type slogLogger struct {
	logger *slog.Logger
	file   io.Closer
}

// NewLogger создает logger по конфигурации
func NewLogger(config LoggerConfig) (Logger, error) {
	return NewLoggerTo(config, os.Stderr)
}

// NewLoggerTo sends console logs and rotation errors to the request's stderr.
// Файл лога (output: file) закрывает вызывающий через Close
func NewLoggerTo(config LoggerConfig, stderr io.Writer) (FileLogger, error) {
	return NewLoggerIn(config, stderr, nil)
}

// NewLoggerIn — NewLoggerTo, у которого время строк — в поясе loc (TZ
// запроса), а не в time.Local процесса; nil — time.Local, как у NewLoggerTo
func NewLoggerIn(config LoggerConfig, stderr io.Writer, loc *time.Location) (FileLogger, error) {
	applyLoggerDefaults(&config)

	writer, err := openLogWriter(config, stderr)
	if err != nil {
		return nil, err
	}
	return newSlogLogger(config, writer, loc), nil
}

// NewLongLivedLoggerIn — логгер долгоживущего процесса (демон chooks serve):
// файл лога открывается на каждую строку с той же ротацией, что у CLI
// (AppendLine), — после ротации, сделанной хуком CLI, строки идут в новый
// файл, а не в chooks.log.1 (ревью 2б-1, п. 1). Вывод не в файл — как у
// NewLoggerIn
func NewLongLivedLoggerIn(config LoggerConfig, stderr io.Writer, loc *time.Location) (FileLogger, error) {
	applyLoggerDefaults(&config)
	if config.Output != "file" {
		return NewLoggerIn(config, stderr, loc)
	}
	if config.LogFile == "" {
		return nil, fmt.Errorf("log file path is required when output is 'file'")
	}
	if err := os.MkdirAll(filepath.Dir(config.LogFile), 0o755); err != nil {
		return nil, fmt.Errorf("failed to create log directory: %w", err)
	}
	return newSlogLogger(config, &appendWriter{path: config.LogFile, maxSizeMB: config.MaxSizeMB}, loc), nil
}

// appendWriter — запись строки лога через AppendLine: открыть, ротировать,
// дописать, закрыть. slog пишет запись одним Write
type appendWriter struct {
	path      string
	maxSizeMB int
}

func (w *appendWriter) Write(p []byte) (int, error) {
	line := strings.TrimSuffix(string(p), "\n")
	if err := AppendLine(w.path, w.maxSizeMB, line); err != nil {
		return 0, err
	}
	return len(p), nil
}

func newSlogLogger(config LoggerConfig, writer io.Writer, loc *time.Location) *slogLogger {
	opts := &slog.HandlerOptions{
		Level: parseLogLevel(config.Level),
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				at := a.Value.Time()
				if loc != nil {
					at = at.In(loc)
				}
				a.Value = slog.StringValue(at.Format("2006-01-02 15:04:05"))
				return a
			}
			return maskAttr(a)
		},
	}

	var handler slog.Handler
	if config.Format == "json" {
		handler = slog.NewJSONHandler(writer, opts)
	} else {
		handler = slog.NewTextHandler(writer, opts)
	}

	logger := &slogLogger{logger: slog.New(handler)}
	if file, ok := writer.(*os.File); ok && config.Output == "file" {
		logger.file = file
	}
	return logger
}

// Close закрывает файл лога, открытый NewLoggerTo; у вывода в stderr — ничего.
// Логгеры от With делят файл с родителем и его не закрывают
func (l *slogLogger) Close() error {
	if l.file == nil {
		return nil
	}
	file := l.file
	l.file = nil
	if err := file.Close(); err != nil {
		return fmt.Errorf("close log file: %w", err)
	}
	return nil
}

// maskAttr скрывает секреты в значении поля: журнал читают обходы и суб-агенты, а поля
// вроде message и error бывают с куском команды. Значение Any (error, Stringer) идёт строкой
func maskAttr(a slog.Attr) slog.Attr {
	var text string
	switch value := a.Value.Resolve(); value.Kind() {
	case slog.KindString:
		text = value.String()
	case slog.KindAny:
		text = fmt.Sprintf("%+v", value.Any())
	default:
		return a
	}
	masked, _ := secretmask.Mask(text)
	a.Value = slog.StringValue(masked)
	return a
}

// parseLogLevel преобразует текстовый уровень в slog.Level
func parseLogLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// openLogWriter открывает поток вывода логов
func openLogWriter(config LoggerConfig, stderr io.Writer) (io.Writer, error) {
	switch config.Output {
	case "stdout":
		// stdout зарезервирован под протокол обмена с Claude Code
		return stderr, nil
	case "file":
		if config.LogFile == "" {
			return nil, fmt.Errorf("log file path is required when output is 'file'")
		}
		if err := os.MkdirAll(filepath.Dir(config.LogFile), 0o755); err != nil {
			return nil, fmt.Errorf("failed to create log directory: %w", err)
		}
		// Неудавшаяся ротация логирование не останавливает: лог просто растёт дальше
		if err := rotateLogIfNeeded(config.LogFile, config.MaxSizeMB); err != nil {
			fmt.Fprintf(stderr, "chooks: log rotation failed: %v\n", err)
		}

		file, err := os.OpenFile(config.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return nil, fmt.Errorf("failed to open log file: %w", err)
		}
		return file, nil
	default:
		return stderr, nil
	}
}

// AppendLine дописывает строку в файл с той же ротацией, что у лога.
// Одна запись O_APPEND короче страницы не перемешивается со строками других
// процессов
func AppendLine(path string, maxSizeMB int, line string) error {
	return appendLineWithRename(path, maxSizeMB, line, os.Rename)
}

func appendLineWithRename(path string, maxSizeMB int, line string, rename func(string, string) error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("cannot create directory for %s: %w", path, err)
	}
	if err := rotateLogIfNeededWithRename(path, maxSizeMB, rename); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("cannot open %s: %w", path, err)
	}
	if _, err := file.WriteString(line + "\n"); err != nil {
		return errors.Join(fmt.Errorf("cannot write %s: %w", path, err), file.Close())
	}
	return file.Close()
}

// rotateLogIfNeeded переименовывает лог в *.1 при превышении порога размера.
// Отсутствующий лог ротации не требует
func rotateLogIfNeeded(logFile string, maxSizeMB int) error {
	return rotateLogIfNeededWithRename(logFile, maxSizeMB, os.Rename)
}

func rotateLogIfNeededWithRename(logFile string, maxSizeMB int, rename func(string, string) error) error {
	if maxSizeMB <= 0 {
		return nil
	}

	info, err := os.Stat(logFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("cannot stat %s: %w", logFile, err)
	}
	if info.Size() < int64(maxSizeMB)*1024*1024 {
		return nil
	}

	if err := rename(logFile, logFile+".1"); err != nil {
		// Другой процесс мог уже ротировать этот файл после нашего Stat.
		// В таком случае новый лог нужно открыть и дописать строку.
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("cannot rename %s: %w", logFile, err)
	}
	return nil
}

// Debug логирует сообщение уровня debug
func (l *slogLogger) Debug(msg string, args ...any) {
	l.logger.Debug(msg, args...)
}

// Info логирует сообщение уровня info
func (l *slogLogger) Info(msg string, args ...any) {
	l.logger.Info(msg, args...)
}

// Warn логирует сообщение уровня warning
func (l *slogLogger) Warn(msg string, args ...any) {
	l.logger.Warn(msg, args...)
}

// Error логирует сообщение уровня error
func (l *slogLogger) Error(msg string, args ...any) {
	l.logger.Error(msg, args...)
}

// With создает новый logger с дополнительными атрибутами
func (l *slogLogger) With(args ...any) Logger {
	return &slogLogger{logger: l.logger.With(args...)}
}
