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

// Исход запуска, оборванного SIGTERM, в файле длительностей
const (
	statusKilled    = "killed"    // по таймауту
	statusCancelled = "cancelled" // отмена хода, до таймаута
)

// timeoutSlack — насколько раньше своего таймаута SIGTERM считается всё же
// таймаутом: таймер Claude Code стартует раньше процесса, отсчёт которого под
// нагрузкой отстаёт (запуск до 269 мс за окно 03-05.11)
const timeoutSlack = time.Second

// hookTimer отмечает стадии хука. Claude Code пишет длительность в
// транскрипт только у Stop-хуков и у запусков, которые что-то вывели, а
// убитый по таймауту — нигде. Поэтому длительность каждого запуска уходит
// строкой в файл длительностей, а стадии — в лог: при SIGTERM и у запуска
// дольше половины таймаута
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
	startup      time.Duration
	// durations — файл длительностей; пусто — лог не в файле (тесты, отладка)
	durations      string
	durationsMaxMB int
}

// durationsFile — файл длительностей рядом с логом: в основной лог строка
// на каждый запуск (порядка 100 тыс. в сутки) вытеснила бы остальное
const durationsFile = "chooks-hooks.tsv"

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
	t.startup = t.mark.Sub(start)
	t.stages = append(t.stages, formatStage("startup", t.startup))
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

// setDurations включает файл длительностей рядом с файлом лога
func (t *hookTimer) setDurations(logger core.LoggerConfig) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if logger.Output != "file" || logger.LogFile == "" {
		return
	}
	t.durations = filepath.Join(filepath.Dir(logger.LogFile), durationsFile)
	t.durationsMaxMB = logger.MaxSizeMB
}

// recordDuration пишет строку TSV: время, хук, сессия, мс всего, мс запуска,
// исход (ok, error, killed или cancelled). Запуск, убитый или упавший до чтения конфига,
// строки не оставляет: путь файла — из конфига. Сбой записи — в лог: хук от
// него не страдает
func (t *hookTimer) recordDuration(at time.Time, session string, elapsed time.Duration, status string) {
	t.mu.Lock()
	path, maxMB, logger, startup := t.durations, t.durationsMaxMB, t.logger, t.startup
	t.mu.Unlock()
	if path == "" {
		return
	}
	line := strings.Join([]string{
		at.Format("2006-01-02T15:04:05.000Z07:00"), t.hook, session,
		strconv.FormatInt(elapsed.Milliseconds(), 10), strconv.FormatInt(startup.Milliseconds(), 10), status,
	}, "\t")
	if err := core.AppendLine(path, maxMB, line); err != nil && logger != nil {
		logger.Warn("hook duration not recorded", "hook", t.hook, "error", err)
	}
}

// deadline — срок необязательной работы от старта процесса
func (t *hookTimer) deadline(budget time.Duration) time.Time {
	return t.start.Add(budget)
}

// terminationStatus отличает таймаут от отмены хода по таймауту хука из
// settings. Явный failover: таймаут не прочитан — считается таймаутом, ошибка
// уходит в лог той же строкой: лишняя тревога лучше пропущенной
func (t *hookTimer) terminationStatus(elapsed time.Duration) (string, error) {
	settingsPath, err := t.settingsPath()
	if err != nil {
		return statusKilled, err
	}
	timeout, err := hookTimeout(settingsPath, t.hook)
	if err != nil {
		return statusKilled, err
	}
	if elapsed < timeout-timeoutSlack {
		return statusCancelled, nil
	}
	return statusKilled, nil
}

// watchTermination пишет в лог, на какой стадии хук оборван: Claude Code шлёт
// SIGTERM по таймауту (проба 02.11, claude 2.1.316) и при отмене хода
// (hook_cancelled в транскрипте, 04.11, claude 2.1.317) и выбрасывает вывод
func (t *hookTimer) watchTermination() {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM)
	go func() {
		<-signals
		t.reportTermination()
		os.Exit(killedExitCode)
	}()
}

// reportTermination пишет исход запуска, оборванного SIGTERM, в лог и в файл
// длительностей
func (t *hookTimer) reportTermination() {
	t.mu.Lock()
	at := t.now()
	elapsed := at.Sub(t.start)
	current := t.current
	session := t.session
	stages := append(append([]string{}, t.stages...), formatStage(current+"(running)", t.now().Sub(t.mark)))
	logger := t.logger
	t.mu.Unlock()
	args := []string{"hook", t.hook, "session", session, "elapsed_ms", strconv.FormatInt(elapsed.Milliseconds(), 10),
		"stage", current, "stages", strings.Join(stages, ",")}
	status, statusErr := t.terminationStatus(elapsed)
	if statusErr != nil {
		args = append(args, "timeout_error", statusErr.Error())
	}
	switch {
	case logger == nil:
		fmt.Fprintf(os.Stderr, "chooks: hook terminated by SIGTERM, %s: %s\n", status, strings.Join(args, " "))
	case status == statusCancelled:
		logger.Info("hook cancelled by SIGTERM before timeout", toAny(args)...)
	default:
		logger.Warn("hook killed by SIGTERM (timeout)", toAny(args)...)
	}
	t.recordDuration(at, session, elapsed, status)
}

// finish пишет длительность запуска, а в лог — строку, если хук шёл дольше
// половины своего таймаута
func (t *hookTimer) finish(status string) {
	t.mu.Lock()
	now := t.now()
	t.stages = append(t.stages, formatStage(t.current, now.Sub(t.mark)))
	elapsed := now.Sub(t.start)
	stages := strings.Join(t.stages, ",")
	logger := t.logger
	session := t.session
	t.mu.Unlock()

	t.recordDuration(now, session, elapsed, status)
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
