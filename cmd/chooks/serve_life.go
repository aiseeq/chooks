package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/aiseeq/chooks/internal/core"
	"github.com/aiseeq/chooks/internal/desktop"
	"github.com/aiseeq/chooks/internal/fallbackwatch"
	"github.com/aiseeq/chooks/internal/hookctx"
	"github.com/aiseeq/chooks/internal/tools/notifier"
)

// Жизнь демона вне запроса: сокет от systemd, сверка своего исполняемого
// файла, строка stats; и сторона CLI — счёт запасного пути с уведомлением

// listenFDsStart — первый дескриптор, переданный systemd (sd_listen_fds)
const listenFDsStart = 3

// activationListener — сокет, переданный systemd (LISTEN_PID, LISTEN_FDS).
// Переменные снимаются с env процесса: дети демона их не наследуют. Не для
// этого процесса (LISTEN_PID другой) или без LISTEN_FDS — nil
func activationListener() (*net.UnixListener, bool, error) {
	pid, fds := os.Getenv("LISTEN_PID"), os.Getenv("LISTEN_FDS")
	for _, key := range []string{"LISTEN_PID", "LISTEN_FDS", "LISTEN_FDNAMES"} {
		if err := os.Unsetenv(key); err != nil {
			return nil, false, fmt.Errorf("unset %s: %w", key, err)
		}
	}
	if fds == "" || pid != strconv.Itoa(os.Getpid()) {
		return nil, false, nil
	}
	if n, err := strconv.Atoi(fds); err != nil || n != 1 {
		return nil, false, fmt.Errorf("LISTEN_FDS=%q: ожидается один сокет (configs/systemd/chooks.socket)", fds)
	}
	file := os.NewFile(listenFDsStart, "chooks.socket")
	listener, err := net.FileListener(file)
	// FileListener держит свою копию дескриптора (с CLOEXEC), исходная —
	// без CLOEXEC, её закрываем
	if closeErr := file.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, false, fmt.Errorf("socket from systemd: %w", err)
	}
	unixListener, ok := listener.(*net.UnixListener)
	if !ok {
		_ = listener.Close()
		return nil, false, fmt.Errorf("socket from systemd is %s, want unix", listener.Addr().Network())
	}
	// Файл сокета принадлежит systemd: демон его не удаляет
	unixListener.SetUnlinkOnClose(false)
	return unixListener, true, nil
}

// exeChecker — сверка исполняемого файла демона: true — заменён, приём
// прекращается; reason — для лога
type exeChecker interface {
	changed() (bool, string)
}

// exeIdentity — inode и mtime исполняемого файла
type exeIdentity struct {
	dev, ino uint64
	mtime    unix.Timespec
}

func identityOf(st unix.Stat_t) exeIdentity {
	return exeIdentity{dev: st.Dev, ino: st.Ino, mtime: st.Mtim}
}

// exeWatch — сверка исполняемого файла демона (противник этапа 2б,
// сценарий 5). running — файл, который исполняет процесс (stat
// /proc/self/exe видит его, даже если путь уже заменён); path — путь, куда
// graft install кладёт новый. Замена во время старта (путь уже другой, чем
// образ процесса) видна на первом же accept
type exeWatch struct {
	path    string
	running exeIdentity
}

func newExeWatch() (*exeWatch, error) {
	var st unix.Stat_t
	if err := unix.Stat("/proc/self/exe", &st); err != nil {
		return nil, fmt.Errorf("stat /proc/self/exe: %w", err)
	}
	path, err := os.Readlink("/proc/self/exe")
	if err != nil {
		return nil, fmt.Errorf("readlink /proc/self/exe: %w", err)
	}
	return &exeWatch{path: strings.TrimSuffix(path, " (deleted)"), running: identityOf(st)}, nil
}

// changed — по пути лежит не тот файл, что исполняется: заменён (inode или
// mtime) или файла по пути нет. В причине — сколько прошло от mtime нового файла:
// замер окна между установкой и её обнаружением
func (w *exeWatch) changed() (bool, string) {
	var st unix.Stat_t
	if err := unix.Stat(w.path, &st); err != nil {
		return true, w.path + ": stat: " + err.Error()
	}
	if got := identityOf(st); got != w.running {
		since := time.Since(time.Unix(got.mtime.Unix()))
		return true, fmt.Sprintf("%s: inode %d mtime %d.%09d (%d ms ago), running inode %d mtime %d.%09d", w.path,
			got.ino, got.mtime.Sec, got.mtime.Nsec, since.Milliseconds(), w.running.ino, w.running.mtime.Sec, w.running.mtime.Nsec)
	}
	return false, ""
}

// acceptWaiter ждёт, пока в очереди сокета появится соединение, не принимая
// его: сверка исполняемого файла идёт до accept. RawConn слушающего сокета
// Read не даёт (EINVAL), поэтому ждётся копия дескриптора через os.File:
// она в том же netpoller, close будит ожидание
type acceptWaiter struct {
	file *os.File
	raw  syscall.RawConn
	once sync.Once
}

func newAcceptWaiter(listener *net.UnixListener) (*acceptWaiter, error) {
	control, err := listener.SyscallConn()
	if err != nil {
		return nil, fmt.Errorf("listener fd: %w", err)
	}
	dup := -1
	var dupErr error
	if err := control.Control(func(fd uintptr) {
		dup, dupErr = unix.FcntlInt(fd, unix.F_DUPFD_CLOEXEC, 0)
	}); err != nil {
		return nil, err
	}
	if dupErr != nil {
		return nil, fmt.Errorf("dup listener: %w", dupErr)
	}
	if err := unix.SetNonblock(dup, true); err != nil {
		_ = unix.Close(dup)
		return nil, fmt.Errorf("dup listener: %w", err)
	}
	file := os.NewFile(uintptr(dup), "listener-wait")
	raw, err := file.SyscallConn()
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("dup listener: %w", err)
	}
	return &acceptWaiter{file: file, raw: raw}, nil
}

// wait возвращается, когда соединение ждёт accept; после close — ошибка
func (w *acceptWaiter) wait() error {
	var pollErr error
	err := w.raw.Read(func(fd uintptr) bool {
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		for {
			n, err := unix.Poll(fds, 0)
			if err == unix.EINTR {
				continue
			}
			if err != nil {
				pollErr = err
				return true
			}
			return n > 0
		}
	})
	if err != nil {
		return err
	}
	return pollErr
}

func (w *acceptWaiter) close() {
	w.once.Do(func() { _ = w.file.Close() })
}

// reportStats пишет строку stats при старте и каждые statsEvery до отмены ctx
func (s *server) reportStats(ctx context.Context) {
	s.logStats()
	ticker := time.NewTicker(s.statsEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.logStats()
		}
	}
}

// logStats — RSS, живая куча после GC, горутины и fd демона, счётчики
// запросов. RSS — отдельно от кучи: Go держит освобождённые страницы
func (s *server) logStats() {
	runtime.GC()
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	fields := []any{
		"heap_live_bytes", mem.HeapAlloc, "goroutines", runtime.NumGoroutine(),
		"served", s.served.Load(), "refused", s.refused.Load(), "in_flight", len(s.sem), "stuck", s.stuck.Load(),
		"uptime_s", int64(time.Since(s.started).Seconds()), "version", Version,
	}
	rss, err := selfRSS()
	if err != nil {
		fields = append(fields, "rss_error", err)
	}
	fds, fdErr := selfFDs()
	if fdErr != nil {
		fields = append(fields, "fds_error", fdErr)
	}
	s.logger.Info("daemon: stats", append([]any{"rss_kb", rss, "fds", fds}, fields...)...)
}

// selfRSS — VmRSS процесса в КБ из /proc/self/status
func selfRSS() (int64, error) {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0, fmt.Errorf("rss: %w", err)
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		value, ok := strings.CutPrefix(scanner.Text(), "VmRSS:")
		if !ok {
			continue
		}
		kb, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(value), " kB"), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("VmRSS %q: %w", value, err)
		}
		return kb, nil
	}
	return 0, fmt.Errorf("no VmRSS in /proc/self/status")
}

// selfFDs — число открытых дескрипторов процесса
func selfFDs() (int, error) {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return 0, fmt.Errorf("fds: %w", err)
	}
	return len(entries), nil
}

// noteCLIFallback — сторона CLI запасного пути (противник этапа 2б,
// сценарий 7): строка в chooks.log, вызов в счёт fallbackwatch и, когда
// уходов в CLI за час больше порога daemon.fallback_alert, одно уведомление
// на рабочий стол не чаще раза в час. Счёт не зависит от демона: при
// остановленном, убитом или зависшем демоне считает CLI
func noteCLIFallback(ctx context.Context, config *core.Config, request hookctx.Context, hookType string, logger core.Logger) {
	reason, _ := ctx.Value(fallbackReasonKey{}).(string)
	if reason == "" {
		return
	}
	logCLIFallback(config, request, hookType, reason)
	dir, err := request.StateDir()
	if err != nil {
		logger.Warn("hook fallback not counted", "hook", hookType, "error", err)
		return
	}
	decision, err := fallbackwatch.Record(dir, config.Daemon.FallbackThreshold(), fallbackwatch.Event{At: time.Now(), Hook: hookType, Reason: reason})
	if err != nil {
		logger.Warn("hook fallback not counted", "hook", hookType, "error", err)
		return
	}
	if decision.StateReset != nil {
		logger.Warn("hook fallback count started anew", "hook", hookType, "error", decision.StateReset)
	}
	if !decision.Alert {
		return
	}
	alert := desktop.Alert{
		Title:   "chooks: хуки идут мимо демона",
		Message: fmt.Sprintf("За час %d вызовов хуков исполнил CLI мимо демона chooks serve. Причины: %s", decision.Count, decision.Summary()),
		AppName: "chooks", Icon: "dialog-warning", Timeout: 30 * time.Second, Urgency: desktop.UrgencyNormal, Desktop: true,
	}
	fields := []any{"hook", hookType, "count", decision.Count, "threshold", config.Daemon.FallbackThreshold(), "reasons", decision.Summary()}
	executable, err := os.Executable()
	if err == nil {
		err = desktop.DeliverInBackgroundForRequest(executable, notifier.WatchCommand, alert, request)
	}
	if err != nil {
		fields = append(fields, "delivery_error", err)
	}
	logger.Warn("daemon fallback alert", fields...)
}
