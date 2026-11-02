package main

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/aiseeq/chooks/internal/core"
)

// slowFloor — меньше этого хук не медленный ни при каком таймауте: самый
// короткий таймаут хука в settings.json — 3 с, половина — 1,5 с. Ниже порога
// settings.json не читается
const slowFloor = 1500 * time.Millisecond

// optionalBudget — сколько от старта процесса хук с таймаутом 3 с даёт
// необязательной работе (напоминанию о скиллах): остаток таймаута — запас
// на вывод и на запуск процесса под нагрузкой
const optionalBudget = 2 * time.Second

// killedExitCode — код выхода по SIGTERM, как у процесса без обработчика
const killedExitCode = 128 + int(syscall.SIGTERM)

// hookTimer отмечает стадии хука. Успешный запуск Claude Code и так пишет
// в транскрипт с длительностью, а убитый по таймауту — нигде: его стадии
// и время уходят в лог при SIGTERM, а запуск дольше половины таймаута —
// строкой при завершении
type hookTimer struct {
	mu      sync.Mutex
	hook    string
	start   time.Time
	mark    time.Time
	current string
	stages  []string
	logger  core.Logger
	session string
	// startErr — старт процесса не прочитан, отсчёт идёт от main
	startErr error
	now      func() time.Time
	// settingsPath — где взять таймаут хука; читается только у медленного
	settingsPath func() (string, error)
}

// newHookTimer начинает отсчёт со старта процесса: запуск бинаря под
// свопом — тоже часть таймаута
func newHookTimer(hook string) *hookTimer {
	t := &hookTimer{hook: hook, now: time.Now, settingsPath: userSettingsPath}
	t.mark = t.now()
	start, err := processStart()
	if err != nil {
		// Явный failover: без старта процесса отсчёт от main, сбой — в лог
		// вместе со строкой о медленном запуске
		start = t.mark
		t.startErr = err
	}
	t.start = start
	t.stages = append(t.stages, formatStage("startup", t.mark.Sub(start)))
	t.current = "input"
	return t
}

// stage закрывает текущую стадию и начинает name
func (t *hookTimer) stage(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	t.stages = append(t.stages, formatStage(t.current, now.Sub(t.mark)))
	t.mark = now
	t.current = name
}

// setSession — сессия известна после разбора входа
func (t *hookTimer) setSession(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.session = id
}

// setLogger — лог появляется после чтения конфига
func (t *hookTimer) setLogger(logger core.Logger) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.logger = logger
}

// deadline — срок необязательной работы от старта процесса
func (t *hookTimer) deadline(budget time.Duration) time.Time {
	return t.start.Add(budget)
}

// watchTermination пишет в лог, на какой стадии хук убит по таймауту:
// Claude Code шлёт SIGTERM (проба 02.11, claude 2.1.316) и выбрасывает вывод
func (t *hookTimer) watchTermination() {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM)
	go func() {
		<-signals
		t.mu.Lock()
		elapsed := t.now().Sub(t.start)
		current := t.current
		session := t.session
		stages := append(append([]string{}, t.stages...), formatStage(current+"(running)", t.now().Sub(t.mark)))
		logger := t.logger
		t.mu.Unlock()
		args := []string{"hook", t.hook, "session", session, "elapsed_ms", strconv.FormatInt(elapsed.Milliseconds(), 10),
			"stage", current, "stages", strings.Join(stages, ",")}
		if logger != nil {
			logger.Warn("hook killed by SIGTERM (timeout)", toAny(args)...)
		} else {
			fmt.Fprintf(os.Stderr, "chooks: hook killed by SIGTERM: %s\n", strings.Join(args, " "))
		}
		os.Exit(killedExitCode)
	}()
}

// finish пишет строку, если хук шёл дольше половины своего таймаута
func (t *hookTimer) finish() {
	t.mu.Lock()
	now := t.now()
	t.stages = append(t.stages, formatStage(t.current, now.Sub(t.mark)))
	elapsed := now.Sub(t.start)
	stages := strings.Join(t.stages, ",")
	logger := t.logger
	session := t.session
	t.mu.Unlock()

	if elapsed < slowFloor || logger == nil {
		return
	}
	args := []any{"hook", t.hook, "session", session, "elapsed_ms", elapsed.Milliseconds(), "stages", stages}
	if t.startErr != nil {
		args = append(args, "process_start_error", t.startErr)
	}
	settingsPath, err := t.settingsPath()
	if err != nil {
		logger.Warn("hook slow, timeout unknown", append(args, "error", err)...)
		return
	}
	timeout, err := hookTimeout(settingsPath, t.hook)
	if err != nil {
		logger.Warn("hook slow, timeout unknown", append(args, "error", err)...)
		return
	}
	if elapsed*2 > timeout {
		logger.Warn("hook slow: over half of timeout", append(args, "timeout_ms", timeout.Milliseconds())...)
	}
}

// hookTimeout — самый короткий таймаут подкоманды в settings.json
func hookTimeout(settingsPath, subcommand string) (time.Duration, error) {
	// Ошибка в другом событии не отменяет найденный таймаут
	hooks, parseErr := core.SettingsHooks(settingsPath)
	var shortest time.Duration
	for _, hook := range hooks {
		if hook.Subcommand != subcommand {
			continue
		}
		if hook.Timeout == 0 {
			return 0, fmt.Errorf("hook %s (%s) has no timeout in %s", subcommand, hook.Event, settingsPath)
		}
		if shortest == 0 || hook.Timeout < shortest {
			shortest = hook.Timeout
		}
	}
	if shortest == 0 {
		return 0, errors.Join(fmt.Errorf("hook %s is not in %s", subcommand, settingsPath), parseErr)
	}
	return shortest, nil
}

// userSettingsPath — settings.json Claude Code пользователя
func userSettingsPath() (string, error) {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, "settings.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot resolve home dir: %w", err)
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

func formatStage(name string, d time.Duration) string {
	return name + "=" + strconv.FormatInt(d.Milliseconds(), 10)
}

func toAny(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

// clockTicks — USER_HZ: единица starttime в /proc; на Linux x86_64 и arm64
// ядро отдаёт её всегда равной 100
const clockTicks = 100

// processStart — время старта процесса по /proc: его возраст — время от
// загрузки (/proc/uptime) минус starttime в тиках от загрузки. Оба в одних
// часах с точностью 10 мс; btime из /proc/stat округлён до секунды и дал бы
// ошибку до секунды
func processStart() (time.Time, error) {
	now := time.Now()
	uptimeData, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return time.Time{}, fmt.Errorf("read /proc/uptime: %w", err)
	}
	stat, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return time.Time{}, fmt.Errorf("read /proc/self/stat: %w", err)
	}
	uptime, err := parseUptime(string(uptimeData))
	if err != nil {
		return time.Time{}, err
	}
	ticks, err := parseStartTicks(string(stat))
	if err != nil {
		return time.Time{}, err
	}
	age := uptime - time.Duration(ticks)*time.Second/clockTicks
	if age < 0 {
		return time.Time{}, fmt.Errorf("process started after uptime: uptime %s, starttime %d ticks", uptime, ticks)
	}
	return now.Add(-age), nil
}

// parseUptime — первое поле /proc/uptime: секунды от загрузки с дробью
func parseUptime(data string) (time.Duration, error) {
	fields := strings.Fields(data)
	if len(fields) == 0 {
		return 0, fmt.Errorf("empty /proc/uptime")
	}
	seconds, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, fmt.Errorf("parse /proc/uptime: %w", err)
	}
	return time.Duration(seconds * float64(time.Second)), nil
}

// parseStartTicks — поле 22 /proc/<pid>/stat. Имя процесса в скобках может
// содержать пробелы, поэтому счёт полей — после последней «)»
func parseStartTicks(stat string) (int64, error) {
	end := strings.LastIndexByte(stat, ')')
	if end < 0 {
		return 0, fmt.Errorf("unexpected /proc/self/stat: no command name")
	}
	// После «)» идут поля с 3-го: state — индекс 0, starttime (22) — индекс 19
	fields := strings.Fields(stat[end+1:])
	const startIndex = 22 - 3
	if len(fields) <= startIndex {
		return 0, fmt.Errorf("unexpected /proc/self/stat: %d fields after command", len(fields))
	}
	ticks, err := strconv.ParseInt(fields[startIndex], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse starttime: %w", err)
	}
	return ticks, nil
}
