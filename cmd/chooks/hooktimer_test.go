package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/aiseeq/akit/proc"
	"github.com/aiseeq/chooks/internal/core"
)

// captureLogger запоминает предупреждения и сообщения INFO
type captureLogger struct {
	mu    sync.Mutex
	warns []string
	infos []string
}

func (l *captureLogger) Debug(string, ...any) {}
func (l *captureLogger) Info(msg string, _ ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.infos = append(l.infos, msg)
}
func (l *captureLogger) Error(string, ...any) {}
func (l *captureLogger) Warn(msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warns = append(l.warns, msg+" "+fmt.Sprintln(args...))
}
func (l *captureLogger) With(...any) core.Logger { return l }

// fakeClock — часы, которые двигает тест
type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time          { return c.now }
func (c *fakeClock) advance(d time.Duration) { c.now = c.now.Add(d) }

func newTestTimer(t *testing.T, hook, settings string) (*hookTimer, *fakeClock, *captureLogger) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{now: time.Date(2026, 11, 2, 18, 45, 23, 0, time.UTC)}
	logger := &captureLogger{}
	timer := &hookTimer{
		hook:         hook,
		start:        clock.now,
		mark:         clock.now,
		current:      "input",
		now:          clock.Now,
		settingsPath: func() (string, error) { return path, nil },
	}
	timer.setLogger(logger)
	return timer, clock, logger
}

const upsSettings = `{"hooks":{"UserPromptSubmit":[{"hooks":[{"command":"$HOME/bin/chooks user-prompt-submit","timeout":3}]}],
"Stop":[{"hooks":[{"command":"$HOME/bin/chooks question-check","timeout":60}]}]}}`

func TestHookTimer_SlowOverHalfTimeout(t *testing.T) {
	timer, clock, logger := newTestTimer(t, "user-prompt-submit", upsSettings)
	clock.advance(10 * time.Millisecond)
	timer.stage("state")
	clock.advance(1600 * time.Millisecond)
	timer.stage("skills")
	clock.advance(20 * time.Millisecond)
	timer.finish("ok")

	if len(logger.warns) != 1 {
		t.Fatalf("ожидалась одна строка о медленном хуке, получено %q", logger.warns)
	}
	line := logger.warns[0]
	for _, want := range []string{"over half of timeout", "elapsed_ms 1630", "timeout_ms 3000", "input=10,state=1600,skills=20"} {
		if !strings.Contains(line, want) {
			t.Errorf("в строке нет %q: %s", want, line)
		}
	}
}

func TestHookTimer_QuietWhenFast(t *testing.T) {
	// Ниже порога settings.json не читается вовсе
	timer, clock, logger := newTestTimer(t, "user-prompt-submit", upsSettings)
	timer.settingsPath = func() (string, error) { return "", errors.New("must not be read") }
	clock.advance(1400 * time.Millisecond)
	timer.finish("ok")
	if len(logger.warns) != 0 {
		t.Errorf("быстрый хук не пишет: %q", logger.warns)
	}

	// 2 с при таймауте 60 с — меньше половины
	timer, clock, logger = newTestTimer(t, "question-check", upsSettings)
	clock.advance(2 * time.Second)
	timer.finish("ok")
	if len(logger.warns) != 0 {
		t.Errorf("question-check 2 с из 60 не медленный: %q", logger.warns)
	}
}

func TestHookTimer_TimeoutUnknown(t *testing.T) {
	timer, clock, logger := newTestTimer(t, "post-tool-batch", upsSettings)
	clock.advance(2 * time.Second)
	timer.finish("ok")
	if len(logger.warns) != 1 || !strings.Contains(logger.warns[0], "timeout unknown") {
		t.Errorf("хука нет в settings — строка с ошибкой, получено %q", logger.warns)
	}
}

func TestHookTimeout_Shortest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	settings := `{"hooks":{"PostToolUse":[{"hooks":[{"command":"chooks webfetch-fallback","timeout":60}]}],
"PostToolUseFailure":[{"hooks":[{"command":"chooks webfetch-fallback","timeout":30}]}],
"Stop":[{"hooks":[{"command":"chooks stop"}]}]}}`
	if err := os.WriteFile(path, []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := hookTimeout(path, "webfetch-fallback")
	if err != nil || got != 30*time.Second {
		t.Errorf("самый короткий из двух: %s, %v", got, err)
	}
	if _, err := hookTimeout(path, "stop"); err == nil {
		t.Error("хук без timeout — ошибка, а не выдуманный срок")
	}
}

func TestParseStartTicks(t *testing.T) {
	fields := make([]string, 50)
	for i := range fields {
		fields[i] = "0"
	}
	fields[19] = "4242" // поле 22 — 20-е после «)»
	stat := "3542860 (chooks ) x) " + strings.Join(fields, " ")
	got, err := parseStartTicks(stat)
	if err != nil || got != 4242 {
		t.Errorf("получено %d, %v", got, err)
	}
	if _, err := parseStartTicks("no command"); err == nil {
		t.Error("строка без имени процесса — ошибка")
	}
}

func TestProcessStart(t *testing.T) {
	start, err := processStart()
	if err != nil {
		t.Fatal(err)
	}
	if age := time.Since(start); age < 0 || age > time.Hour {
		t.Errorf("старт тестового процесса %s назад", age)
	}
	if got, err := parseUptime("12345.67 99.00\n"); err != nil || got != 12345670*time.Millisecond {
		t.Errorf("uptime: %s, %v", got, err)
	}
	if _, err := parseUptime(""); err == nil {
		t.Error("пустой /proc/uptime — ошибка")
	}
}

// Процесс-помощник для TestHookTimer_SIGTERM: ставит обработчик и ждёт
func TestHookTimerHelper(t *testing.T) {
	if os.Getenv("CHOOKS_HOOKTIMER_HELPER") != "1" {
		t.Skip("помощник для TestHookTimer_SIGTERM")
	}
	timer := newHookTimer("user-prompt-submit")
	timer.setSession("helper-session")
	timer.setDurations(core.LoggerConfig{Output: "file", LogFile: os.Getenv("CHOOKS_HOOKTIMER_LOG")})
	timer.watchTermination()
	timer.stage("skills")
	fmt.Println("ready")
	// Родитель держит stdin открытым: выход отсюда — только по SIGTERM
	if _, err := io.ReadAll(os.Stdin); err != nil {
		t.Fatal(err)
	}
	t.Fatal("stdin закрыт без SIGTERM")
}

// Claude Code по таймауту шлёт хуку SIGTERM: хук пишет стадию и выходит 143.
// Таймаут в пустом settings не прочитан — запуск считается убитым по таймауту
func TestHookTimer_SIGTERM(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestHookTimerHelper$")
	logDir := t.TempDir()
	cmd.Env = append(os.Environ(), "CHOOKS_HOOKTIMER_HELPER=1", "CHOOKS_HOOKTIMER_LOG="+filepath.Join(logDir, "chooks.log"), "CLAUDE_CONFIG_DIR="+t.TempDir())
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(stdout)
	ready := false
	for !ready && scanner.Scan() {
		ready = scanner.Text() == "ready"
	}
	if !ready {
		t.Fatal("помощник не дошёл до ожидания сигнала")
	}
	// SIGTERM — то, что проверяется: хук должен сам выйти по нему. Запас до
	// SIGKILL большой: убитый по запасу помощник даст другой код выхода
	if _, err := proc.StopChild(cmd.Process.Pid, 10*time.Second, func() error { return cmd.Process.Signal(syscall.SIGTERM) }); err != nil {
		t.Fatal(err)
	}
	err = cmd.Wait()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != killedExitCode {
		t.Fatalf("ожидался выход %d, получено %v", killedExitCode, err)
	}
	for _, want := range []string{"terminated by SIGTERM, killed", "stage skills", "skills(running)=", "timeout_error"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("в stderr нет %q: %s", want, stderr.String())
		}
	}
	durations, err := os.ReadFile(filepath.Join(logDir, durationsFile))
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Split(strings.TrimSpace(string(durations)), "\t")
	if len(fields) != 6 || fields[1] != "user-prompt-submit" || fields[2] != "helper-session" || fields[5] != "killed" {
		t.Errorf("строка длительности убитого хука: %q", durations)
	}
}

// Длительность пишется на каждый запуск, и быстрый тоже: Claude Code
// не пишет в транскрипт тихие запуски не-Stop хуков
func TestHookTimer_RecordsEveryRun(t *testing.T) {
	timer, clock, logger := newTestTimer(t, "post-tool-batch", upsSettings)
	logDir := t.TempDir()
	timer.setDurations(core.LoggerConfig{Output: "file", LogFile: filepath.Join(logDir, "chooks.log")})
	timer.setSession("s1")
	clock.advance(12 * time.Millisecond)
	timer.finish("ok")

	data, err := os.ReadFile(filepath.Join(logDir, durationsFile))
	if err != nil {
		t.Fatal(err)
	}
	want := "2026-11-02T18:45:23.012Z\tpost-tool-batch\ts1\t12\t0\tok\n"
	if string(data) != want {
		t.Errorf("получено %q, ожидалось %q", data, want)
	}
	if len(logger.warns) != 0 {
		t.Errorf("быстрый запуск не пишет в лог: %q", logger.warns)
	}

	// Лог не в файле (stderr) — файла длительностей нет
	quiet, clock, _ := newTestTimer(t, "stop", upsSettings)
	quiet.setDurations(core.LoggerConfig{Output: "stderr"})
	clock.advance(time.Millisecond)
	quiet.finish("ok")
	if quiet.durations != "" {
		t.Errorf("без файла лога длительности некуда писать: %q", quiet.durations)
	}
}

// Хук, вернувший ошибку, помечен error: «ok» значит, что хук отработал
func TestRunHook_ErrorStatus(t *testing.T) {
	logDir := t.TempDir()
	configFile := filepath.Join(t.TempDir(), "config.yaml")
	writeConfig := "logger:\n  output: \"file\"\n  file: \"" + filepath.Join(logDir, "chooks.log") + "\"\n"
	if err := os.WriteFile(configFile, []byte(writeConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	previous := configPath
	configPath = configFile
	t.Cleanup(func() { configPath = previous })
	stdin, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	oldStdin := os.Stdin
	os.Stdin = stdin
	t.Cleanup(func() { os.Stdin = oldStdin })

	if _, err := runHook(t.Context(), "no-such-hook"); err == nil {
		t.Fatal("неизвестный хук должен давать ошибку")
	}
	data, err := os.ReadFile(filepath.Join(logDir, durationsFile))
	if err != nil {
		t.Fatal(err)
	}
	if fields := strings.Split(strings.TrimSpace(string(data)), "\t"); len(fields) != 6 || fields[1] != "no-such-hook" || fields[5] != "error" {
		t.Errorf("строка длительности: %q", data)
	}
}
