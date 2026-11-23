package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"golang.org/x/sys/unix"

	"github.com/aiseeq/chooks/internal/core"
	"github.com/aiseeq/chooks/internal/hookctx"
	"github.com/aiseeq/chooks/internal/hookproxy"
	"github.com/aiseeq/chooks/internal/hookwire"
)

// hookStatusLine — подкоманда строки статуса: исполняется той же
// executeHook, что и хуки
const hookStatusLine = "statusline"

// hookTimeouts — подкоманды, которые обслуживает демон, и запасной таймаут
// хука, когда его нет в settings.json запроса (hookTimeout): у statusLine
// таймаута в settings нет вовсе. Значения — из configs/settings-snippet.json;
// understand-check и night-hint в сниппете нет — их таймауты из
// ~/.claude/settings.json на 22.11 (3 с у night-hint); understand-check (90 с)
// и claim-stop (120 с) — из configs/claim-check-hooks.json. У statusLine
// таймаута в settings нет: 5 с. Подкоманды вне таблицы демон не обслуживает —
// «в CLI»
var hookTimeouts = map[string]time.Duration{
	hookPreToolUse:       3 * time.Second,
	hookNoPrompt:         5 * time.Second,
	hookWebFetchFallback: 60 * time.Second,
	hookUserPromptSubmit: 3 * time.Second,
	hookPostToolBatch:    3 * time.Second,
	hookNotification:     3 * time.Second,
	hookStop:             3 * time.Second,
	hookUnsentCheck:      5 * time.Second,
	hookStopFailure:      3 * time.Second,
	hookUnderstandCheck:  90 * time.Second,
	hookClaimStop:        120 * time.Second,
	hookNightHint:        3 * time.Second,
	hookStatusLine:       5 * time.Second,
}

const (
	// serveDeadlineMargin — запас до таймаута хука: срок запроса в демоне —
	// старт прокси + таймаут − запас, чтобы ответ успел дойти до Claude Code
	serveDeadlineMargin = 500 * time.Millisecond
	// serveStuckGrace — сколько после срока запроса демон ждёт обработчик,
	// прежде чем ответить без него: обработчик, не слушающий отмену, держит
	// слот, но прокси получает ответ до SIGTERM от Claude Code (срок + запас)
	serveStuckGrace = serveDeadlineMargin / 2
	// serveMaxInFlight — предел запросов, исполняемых одновременно; сверх
	// него — «в CLI: busy»: зависшие обработчики не вытесняют остальные хуки
	serveMaxInFlight = 128
	// serveReadTimeout — срок чтения запроса после accept
	serveReadTimeout = 2 * time.Second
	// serveCommitTimeout — срок ожидания 'C' после «принято»
	serveCommitTimeout = 2 * time.Second
	// serveWriteTimeout — срок записи результата прокси
	serveWriteTimeout = 5 * time.Second
	// serveStatsEvery — как часто демон пишет в лог RSS, живую кучу,
	// горутины и fd (противник этапа 2б, сценарий 4: утечки — замерами на
	// живом трафике, без синтетической нагрузки)
	serveStatsEvery = time.Hour
)

// Причины отмены запроса после 'C'
var (
	errProxyGone       = errors.New("proxy gone")
	errProxyTerminated = errors.New("proxy got SIGTERM")
)

// hookRunner исполняет принятый запрос; у демона — executeHook, тест
// подставляет свой
type hookRunner func(ctx context.Context, request hookctx.Context, hookType string) int

// server — демон chooks serve: принимает запросы прокси по unix-сокету
type server struct {
	logger   core.Logger
	run      hookRunner
	sem      chan struct{}
	inFlight sync.WaitGroup
	uid      int
	// start — старт процесса прокси, от него отсчитывается срок запроса
	start func(pid int) (time.Time, error)
	// exe — сверка своего исполняемого файла на каждом accept (exeWatch);
	// nil — без сверки (не прочитан при старте)
	exe exeChecker
	// commitTimeout и stuckGrace — serveCommitTimeout и serveStuckGrace;
	// тест укорачивает
	commitTimeout, stuckGrace time.Duration
	statsEvery                time.Duration
	started                   time.Time
	// Счётчики для строки stats
	served, refused, stuck atomic.Int64
	// handling — обработчиков handle идёт сейчас (без отказов «upgrade»)
	handling atomic.Int64
}

func newServer(logger core.Logger) *server {
	s := &server{logger: logger, run: executeHook, sem: make(chan struct{}, serveMaxInFlight), uid: os.Getuid(), start: processStartPID,
		commitTimeout: serveCommitTimeout, stuckGrace: serveStuckGrace, statsEvery: serveStatsEvery, started: time.Now()}
	if exe, err := newExeWatch(); err != nil {
		logger.Warn("daemon: own executable unreadable, update after install not detected", "error", err)
	} else {
		s.exe = exe
	}
	return s
}

func newServeCmd() *cobra.Command {
	var socket string
	cmd := &cobra.Command{
		Use:   "serve [--socket <путь>]",
		Short: "Демон хуков: исполнять вызовы прокси chooks-hook, пришедшие по unix-сокету",
		Long: `Слушает unix-сокет и исполняет вызовы хуков от прокси chooks-hook той же
функцией, что CLI, с env, cwd, pid и stdin вызова. Сокет — от systemd
(LISTEN_FDS, configs/systemd/chooks.socket) или свой по --socket (права 0600).
Вызов, который демон не может исполнить так же, как CLI (чужой uid или pid,
относительные пути в env, transcript_path не обычный файл, неизвестная
подкоманда), отдаётся прокси «в CLI» до «принято».

На каждом accept демон сверяет inode и mtime своего исполняемого файла:
сменился (graft install) — выходит с кодом 0, а следующее соединение
systemd отдаёт новому процессу. Если идут принятые вызовы, демон их
дообслуживает и до их конца отвечает новым соединениям «в CLI: upgrade».
Обрыв связи после «принято» CLI не повторяет, а записывает. SIGTERM и
SIGINT: перестать принимать, дообслужить идущие вызовы. Раз в час — строка
stats в лог: RSS, живая куча после GC, горутины, fd.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			request, err := commandRequest(cmd)
			if err != nil {
				return err
			}
			listener, activated, err := activationListener()
			if err != nil {
				return err
			}
			if listener == nil {
				if socket == "" {
					return fmt.Errorf("не задан --socket, и systemd не передал сокет (LISTEN_FDS)")
				}
			} else if socket != "" {
				_ = listener.Close()
				return fmt.Errorf("--socket не задаётся, когда сокет передал systemd (LISTEN_FDS)")
			}
			config, err := core.LoadConfigForRequest(commandConfigPath(cmd), request)
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}
			logger, err := core.NewLongLivedLoggerIn(config.Logger, request.Stderr, request.Location())
			if err != nil {
				return fmt.Errorf("failed to create logger: %w", err)
			}
			defer closeRequestLogger(logger)
			if listener == nil {
				if listener, err = listenSocket(socket); err != nil {
					return err
				}
			}
			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
			defer stop()
			where := socket
			if activated {
				where = "systemd:" + listener.Addr().String()
			}
			logger.Info("daemon: serving", "socket", where, "pid", os.Getpid(), "version", Version)
			err = newServer(logger).serve(ctx, listener)
			logger.Info("daemon: stopped", "socket", where, "version", Version, "error", err)
			return err
		},
	}
	cmd.Flags().StringVar(&socket, "socket", "", "путь unix-сокета (без systemd)")
	return cmd
}

// listenSocket слушает path с правами 0600. Оставшийся сокет без демона
// удаляется; живой демон на нём — ошибка: два демона на одном пути не
// поднимаются
func listenSocket(path string) (*net.UnixListener, error) {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s существует и не сокет", path)
		}
		if conn, err := net.DialTimeout("unix", path, time.Second); err == nil {
			_ = conn.Close()
			return nil, fmt.Errorf("%s уже обслуживает другой демон", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale socket: %w", err)
		}
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		return nil, err
	}
	return listener, nil
}

// serve принимает соединения до отмены ctx или до смены своего исполняемого
// файла, затем ждёт идущие запросы. Запросы ctx сервера не наследуют:
// остановка демона их не обрывает. Зависшие обработчики (serveStuckGrace) не
// ждутся: их прокси уже получил ответ.
//
// Смена файла без идущих запросов — выход сразу: соединение остаётся в
// очереди сокета, и systemd отдаёт его новому процессу. С идущими запросами
// демон дообслуживает их и до их конца принимает новые соединения, отвечая
// «в CLI: upgrade»: без accept прокси ждал бы «принято» до своего окна и
// уходил бы в CLI по «ack: timed out», который идёт в счёт уведомления
// (ревью 8f10b59, п. 2)
func (s *server) serve(ctx context.Context, listener *net.UnixListener) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// drained — смена файла застала идущие запросы, и они дообслужены:
	// приём кончается
	drained, drain := context.WithCancel(context.Background())
	defer drain()
	upgrading := false
	var handlers sync.WaitGroup
	// upgrade — файл сменился: true — выйти сразу (запросов нет), иначе
	// приём до конца идущих
	upgrade := func() bool {
		if s.handling.Load() == 0 {
			return true
		}
		upgrading = true
		go func() {
			handlers.Wait()
			drain()
		}()
		return false
	}
	waiter, err := newAcceptWaiter(listener)
	if err != nil {
		_ = listener.Close()
		return err
	}
	defer waiter.close()
	closeListener := func() {
		_ = listener.Close()
		waiter.close()
	}
	stop := context.AfterFunc(ctx, closeListener)
	defer stop()
	stopDrained := context.AfterFunc(drained, closeListener)
	defer stopDrained()
	go s.reportStats(ctx)
	for {
		if waitErr := waiter.wait(); waitErr != nil {
			if ctx.Err() == nil && drained.Err() == nil {
				err = waitErr
			}
			break
		}
		// Сверка до accept: без идущих запросов соединение остаётся в очереди
		// сокета, и при сокете от systemd его примет новый процесс
		if !upgrading && s.exeChanged("before accept") && upgrade() {
			break
		}
		conn, acceptErr := listener.AcceptUnix()
		if acceptErr != nil {
			if ctx.Err() == nil && drained.Err() == nil {
				err = acceptErr
			}
			break
		}
		// Замена между сверкой и accept или дообслуживание после неё:
		// принятое соединение — «в CLI», старый код его не исполняет
		if !upgrading && s.exeChanged("after accept") {
			s.refuseUpgrade(conn)
			if upgrade() {
				break
			}
			continue
		}
		if upgrading {
			s.refuseUpgrade(conn)
			continue
		}
		s.handling.Add(1)
		handlers.Add(1)
		s.inFlight.Go(func() {
			defer handlers.Done()
			defer s.handling.Add(-1)
			s.handle(conn)
		})
	}
	_ = listener.Close()
	s.inFlight.Wait()
	return err
}

// upgradeReason — причина «в CLI» после замены исполняемого файла демона
const upgradeReason = "upgrade"

// refuseUpgrade отвечает «в CLI: upgrade» и закрывает соединение
func (s *server) refuseUpgrade(conn *net.UnixConn) {
	s.inFlight.Go(func() {
		defer conn.Close()
		s.refuse(conn, "", upgradeReason)
	})
}

// exeChanged — исполняемый файл демона заменён: строка в лог, приём
// прекращается
func (s *server) exeChanged(when string) bool {
	if s.exe == nil {
		return false
	}
	changed, reason := s.exe.changed()
	if changed {
		s.logger.Info("daemon: executable changed, stop accepting", "when", when, "reason", reason, "version", Version)
	}
	return changed
}

// handle обслуживает одно соединение: проверки до «принято», 'C' от прокси,
// исполнение, результат
func (s *server) handle(conn *net.UnixConn) {
	defer conn.Close()
	accepted := time.Now()
	cred, err := peerCred(conn)
	if err != nil {
		s.refuse(conn, "", "peercred: "+err.Error())
		return
	}
	if int(cred.Uid) != s.uid {
		s.refuse(conn, "", "uid")
		return
	}
	_ = conn.SetReadDeadline(accepted.Add(serveReadTimeout))
	wire, err := hookwire.ReadRequest(conn)
	if err != nil {
		reason := "read: " + err.Error()
		switch {
		case errors.Is(err, hookwire.ErrTooLarge):
			reason = "size"
		case errors.Is(err, hookwire.ErrMagic), errors.Is(err, hookwire.ErrMalformed):
			reason = "protocol"
		}
		s.refuse(conn, "", reason)
		return
	}
	admitted, reason := admitRequest(wire, int(cred.Pid))
	if reason != "" {
		s.refuse(conn, wire.Subcommand, reason)
		return
	}
	select {
	case s.sem <- struct{}{}:
	default:
		reason := "busy"
		if stuck := s.stuck.Load(); stuck > 0 {
			reason = fmt.Sprintf("busy: %d stuck", stuck)
		}
		s.refuse(conn, wire.Subcommand, reason)
		return
	}
	// Слот освобождает тот, кто дождался обработчика: handle или, у
	// зависшего, его сторож
	release := func() { <-s.sem }
	deadline := s.requestDeadline(wire.PID, wire.Subcommand, admitted.env, accepted)
	if !time.Now().Before(deadline) {
		release()
		s.refuse(conn, wire.Subcommand, "deadline")
		return
	}

	if !s.acceptAndCommit(conn, wire) {
		release()
		return
	}

	ctx, cancelCause := context.WithCancelCause(context.Background())
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	_ = conn.SetReadDeadline(time.Time{})
	// После 'C' прокси молчит до 'T' (свой SIGTERM): любое чтение (обрыв,
	// закрытие, 'T', лишние байты) — прокси больше не ждёт, работа запроса
	// отменяется
	terminated, gone := make(chan struct{}), make(chan struct{})
	go func() {
		var b [1]byte
		n, _ := conn.Read(b[:])
		if n == 1 && b[0] == hookwire.Terminate {
			close(terminated)
			cancelCause(errProxyTerminated)
			return
		}
		close(gone)
		cancelCause(errProxyGone)
	}()
	call := &daemonCall{terminated: terminated, orphan: func(value any, stack []byte) {
		s.logger.Error("daemon: panic in hook goroutine after its request answered", "hook", wire.Subcommand, "pid", wire.PID, "panic", fmt.Sprint(value), "stack", string(stack))
	}}
	ctx = context.WithValue(ctx, daemonCallKey{}, call)
	stdout, stderr := &outputBuffer{}, &outputBuffer{}
	request := hookctx.New(bytes.NewReader(admitted.stdin), stdout, stderr, admitted.env, wire.Cwd, wire.PID, wire.PPID)
	request.ConfigPath, request.Verbose, request.Timeout = admitted.config, admitted.verbose, admitted.timeout
	code := s.executeWatched(ctx, request, wire, deadline, release)
	// Вывод после ответа (брошенные горутины обработчика) отбрасывается
	stdout.seal()
	stderr.seal()
	if stdout.truncatedOutput() || stderr.truncatedOutput() {
		s.logger.Warn("daemon: hook output truncated", "hook", wire.Subcommand, "pid", wire.PID, "limit", hookwire.MaxOutput)
	}
	select {
	case <-gone:
		s.logger.Info("daemon: request cancelled, proxy gone", "hook", wire.Subcommand, "pid", wire.PID)
	case <-terminated:
		s.logger.Info("daemon: request cancelled, proxy got SIGTERM", "hook", wire.Subcommand, "pid", wire.PID)
	default:
	}
	s.served.Add(1)
	s.logger.Debug("daemon: request done", "hook", wire.Subcommand, "pid", wire.PID, "code", code,
		"elapsed_ms", time.Since(accepted).Milliseconds(), "version", Version)
	_ = conn.SetWriteDeadline(time.Now().Add(serveWriteTimeout))
	if _, err := conn.Write(hookwire.EncodeResult(code, stdout.Bytes(), stderr.Bytes())); err != nil {
		s.logger.Info("daemon: result not delivered", "hook", wire.Subcommand, "pid", wire.PID, "error", err)
	}
}

// acceptAndCommit отвечает «принято» и ждёт 'C'. Исполнение — только после
// 'C': прокси, не дождавшийся «принято», ушёл в CLI, и второй раз запрос не
// исполняется. 'C' не пришёл к сроку — 'F' «late»: прокси, приславший его
// позже, уйдёт в CLI, а не потеряет вызов (ревью 2б-1, п. 8)
func (s *server) acceptAndCommit(conn *net.UnixConn, wire hookwire.Request) bool {
	_ = conn.SetWriteDeadline(time.Now().Add(s.commitTimeout))
	if _, err := conn.Write([]byte{hookwire.Accepted}); err != nil {
		s.logger.Debug("daemon: proxy left before accept", "hook", wire.Subcommand, "error", err)
		return false
	}
	_ = conn.SetReadDeadline(time.Now().Add(s.commitTimeout))
	var commit [1]byte
	if _, err := io.ReadFull(conn, commit[:]); err != nil || commit[0] != hookwire.Commit {
		if errors.Is(err, os.ErrDeadlineExceeded) {
			s.refuse(conn, wire.Subcommand, hookproxy.LateReason)
		}
		s.logger.Info("daemon: request left to CLI by proxy before commit", "hook", wire.Subcommand, "pid", wire.PID, "error", err)
		return false
	}
	return true
}

// stuckMessage — stderr ответа без зависшего обработчика
const stuckMessage = "chooks: обработчик хука не вернулся к сроку запроса, демон ответил без него\n"

// executeWatched исполняет запрос и ждёт его не дольше срока + stuckGrace
// (ревью 2б-1, п. 4). Обработчик, не слушающий отмену, держит слот до своего
// конца, но не молча: строка Error в лог, счётчик stuck в строке stats и в
// причине «busy», прокси — код 1 со строкой stderr до SIGTERM от Claude Code.
// release освобождает слот
func (s *server) executeWatched(ctx context.Context, request hookctx.Context, wire hookwire.Request, deadline time.Time, release func()) int {
	result := make(chan int, 1)
	go func() { result <- s.execute(ctx, request, wire.Subcommand) }()
	watch := time.NewTimer(time.Until(deadline) + s.stuckGrace)
	defer watch.Stop()
	select {
	case code := <-result:
		release()
		return code
	case <-watch.C:
	}
	stuck := s.stuck.Add(1)
	stuckAt := time.Now()
	s.logger.Error("daemon: hook handler stuck past its deadline, slot held", "hook", wire.Subcommand, "pid", wire.PID,
		"stuck", stuck, "slots", cap(s.sem))
	_, _ = io.WriteString(request.Stderr, stuckMessage)
	go func() {
		code := <-result
		release()
		left := s.stuck.Add(-1)
		s.logger.Info("daemon: stuck hook handler returned", "hook", wire.Subcommand, "pid", wire.PID, "code", code,
			"late_ms", time.Since(stuckAt).Milliseconds(), "stuck", left)
	}()
	return exitError
}

// execute исполняет запрос с перехватом паники. Ответ на панику — как у CLI:
// «panic: …» и стек в stderr, код 2 (код выхода Go на панике), строка Error
// в лог демона. Паника горутины запроса (goRequest) — с её исходным стеком
func (s *server) execute(ctx context.Context, request hookctx.Context, hookType string) (code int) {
	defer func() {
		if r := recover(); r != nil {
			value, stack := r, debug.Stack()
			if p, ok := r.(goroutinePanic); ok {
				value, stack = p.value, p.stack
			}
			fmt.Fprintf(request.Stderr, "panic: %v\n\n%s", value, stack)
			s.logger.Error("daemon: panic in hook request", "hook", hookType, "pid", request.PID, "panic", fmt.Sprint(value), "stack", string(stack))
			code = exitBlocked
		}
	}()
	return s.run(ctx, request, hookType)
}

// refuse отвечает «в CLI: reason» и пишет причину в лог демона
func (s *server) refuse(conn *net.UnixConn, hookType, reason string) {
	s.refused.Add(1)
	s.logger.Info("daemon: request left to CLI", "hook", hookType, "reason", reason)
	_ = conn.SetWriteDeadline(time.Now().Add(serveCommitTimeout))
	if _, err := conn.Write(hookwire.EncodeFallback(reason)); err != nil {
		s.logger.Debug("daemon: refusal not delivered", "hook", hookType, "reason", reason, "error", err)
	}
}

// requestDeadline — срок запроса: старт процесса прокси (с него Claude Code
// отсчитывает таймаут, а прокси жив до ответа) + таймаут хука − запас.
// Таймаут — из settings.json запроса, как у CLI (hookTimeout, ревью 2б-1,
// п. 3); нет там — запасной из hookTimeouts. Старт не прочитан — от accept
func (s *server) requestDeadline(pid int, hookType string, env []string, accepted time.Time) time.Time {
	start, err := s.start(pid)
	if err != nil || start.After(accepted) {
		start = accepted
	}
	return start.Add(requestHookTimeout(env, hookType) - serveDeadlineMargin)
}

// requestHookTimeout — таймаут подкоманды из settings.json по env запроса;
// не прочитан или не задан — запасной из hookTimeouts
func requestHookTimeout(env []string, hookType string) time.Duration {
	path, err := settingsPathFor(hookctx.New(nil, nil, nil, env, "/", 0, 0))
	if err != nil {
		return hookTimeouts[hookType]
	}
	timeout, err := hookTimeout(path, hookType)
	if err != nil {
		return hookTimeouts[hookType]
	}
	return timeout
}

// admitted — проверенный запрос: env без CHOOKS_FALLBACK и флаги хука
type admitted struct {
	env     []string
	stdin   []byte
	config  string
	verbose bool
	timeout time.Duration
}

// admitRequest — проверки до «принято». Непустая причина — «в CLI»: демон
// исполнил бы запрос не так, как CLI, или запрос не от прокси, что его прислал
func admitRequest(wire hookwire.Request, peerPID int) (admitted, string) {
	if wire.PID != peerPID {
		return admitted{}, "pid"
	}
	ppid, err := procParent(wire.PID)
	if err != nil {
		return admitted{}, "ppid: " + err.Error()
	}
	if ppid != wire.PPID {
		return admitted{}, "ppid"
	}
	if _, ok := hookTimeouts[wire.Subcommand]; !ok {
		return admitted{}, "subcommand"
	}
	a := admitted{stdin: wire.Stdin, env: withoutEnv(wire.Env, hookproxy.EnvFallback)}
	flags := pflag.NewFlagSet(wire.Subcommand, pflag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVarP(&a.config, "config", "c", "", "")
	flags.BoolVarP(&a.verbose, "verbose", "v", false, "")
	flags.DurationVar(&a.timeout, "timeout", 5*time.Second, "")
	// Разбор и его ошибку делает CLI: у него текст ошибки и код выхода
	if err := flags.Parse(wire.Args); err != nil {
		return admitted{}, "argv: " + err.Error()
	}
	if flags.NArg() > 0 {
		return admitted{}, "argv: positional"
	}
	if a.config != "" && !filepath.IsAbs(a.config) {
		return admitted{}, "config: relative"
	}
	if reason := daemonEnvRefusal(a.env); reason != "" {
		return admitted{}, "env: " + reason
	}
	if reason := cwdRefusal(wire.Cwd, wire.PID); reason != "" {
		return admitted{}, "cwd: " + reason
	}
	if reason := transcriptRefusal(wire.Stdin); reason != "" {
		return admitted{}, "transcript: " + reason
	}
	return a, ""
}

// daemonAbsoluteEnv — переменные-пути, относительное значение которых демон
// не обслуживает (тикет hooks-daemon, записи 07:40 и 10:39 22.11): CLI
// разрешает их от своего cwd, демон — только повтором. ZONEINFO,
// XDG_CONFIG_HOME, XDG_CACHE_HOME и CLAUDE_PROJECT_DIR — сверх списка, по тому
// же правилу
var daemonAbsoluteEnv = []string{
	string(hookctx.EnvHome), string(hookctx.EnvRuntimeDir), string(hookctx.EnvTmpDir), string(hookctx.EnvStateHome),
	string(hookctx.EnvClaudeConfigDir), string(hookctx.EnvModelsApproved), string(hookctx.EnvClaudeCodeTmpDir),
	"XDG_CONFIG_HOME", "XDG_CACHE_HOME", "ZONEINFO", "CLAUDE_PROJECT_DIR",
}

// daemonBusEnv — адреса D-Bus: путь сокета или nonce-файла в них должен быть
// абсолютным
var daemonBusEnv = []string{"DBUS_SESSION_BUS_ADDRESS", "DBUS_SYSTEM_BUS_ADDRESS"}

// daemonEnvRefusal — правило абсолютного env: непустая причина — запрос
// исполняет CLI
func daemonEnvRefusal(env []string) string {
	request := hookctx.New(nil, nil, nil, env, "/", 0, 0)
	for _, key := range daemonAbsoluteEnv {
		if value := request.Getenv(key); value != "" && !filepath.IsAbs(value) {
			return key + " relative"
		}
	}
	if path, ok := request.LookupEnv("PATH"); ok && path != "" {
		for _, dir := range filepath.SplitList(path) {
			if dir == "" {
				return "PATH empty component"
			}
			if !filepath.IsAbs(dir) {
				return "PATH relative component"
			}
		}
	}
	if strings.Contains(request.Getenv("GODEBUG"), "execerrdot") {
		return "GODEBUG execerrdot"
	}
	for _, key := range daemonBusEnv {
		if busAddressRelative(request.Getenv(key)) {
			return key + " relative"
		}
	}
	return ""
}

// busAddressRelative — в адресе D-Bus есть относительный путь: unix:path=
// (кроме абстрактного @), dir=, tmpdir= или noncefile=
func busAddressRelative(address string) bool {
	for entry := range strings.SplitSeq(address, ";") {
		_, keys, ok := strings.Cut(entry, ":")
		if !ok {
			continue
		}
		for pair := range strings.SplitSeq(keys, ",") {
			key, value, ok := strings.Cut(pair, "=")
			if !ok {
				continue
			}
			switch key {
			case "path", "dir", "tmpdir", "noncefile":
			default:
				continue
			}
			value = unescapeBusValue(value)
			if value == "" || (key == "path" && strings.HasPrefix(value, "@")) {
				continue
			}
			if !filepath.IsAbs(value) {
				return true
			}
		}
	}
	return false
}

// unescapeBusValue снимает %XX адреса D-Bus; неразборное значение — как есть
// (по нему проверяется только первый символ)
func unescapeBusValue(value string) string {
	var out strings.Builder
	for i := 0; i < len(value); i++ {
		if value[i] == '%' && i+2 < len(value) {
			if b, err := strconv.ParseUint(value[i+1:i+3], 16, 8); err == nil {
				out.WriteByte(byte(b))
				i += 2
				continue
			}
		}
		out.WriteByte(value[i])
	}
	return out.String()
}

// cwdRefusal — cwd запроса не тот каталог, где стоит прокси: не абсолютный
// (удалён — os.Getwd прокси не дал пути), переименован или подменён
func cwdRefusal(cwd string, pid int) string {
	if !filepath.IsAbs(cwd) {
		return "not absolute"
	}
	var want, got unix.Stat_t
	if err := unix.Stat(filepath.Join("/proc", strconv.Itoa(pid), "cwd"), &want); err != nil {
		return "proxy cwd: " + err.Error()
	}
	if err := unix.Stat(cwd, &got); err != nil {
		return err.Error()
	}
	if want.Dev != got.Dev || want.Ino != got.Ino {
		return "not the proxy cwd"
	}
	return ""
}

// transcriptRefusal — transcript_path входа (и транскрипт агента по agent_id)
// не обычный файл: открытие FIFO без писателя не отменяется ctx и держало бы
// обработчик демона. Относительный путь — тоже в CLI (правило абсолютных
// путей). Нет файла — не причина: хук откроет его так же, как CLI
func transcriptRefusal(stdin []byte) string {
	var input struct {
		TranscriptPath string `json:"transcript_path"`
		AgentID        string `json:"agent_id"`
	}
	//nolint:silent-error-handling // нечитаемый вход разбирает и пишет в лог сам хук; проверять здесь нечего
	if err := json.Unmarshal(stdin, &input); err != nil {
		return ""
	}
	if input.TranscriptPath == "" {
		return ""
	}
	if !filepath.IsAbs(input.TranscriptPath) {
		return "relative"
	}
	for _, path := range []string{input.TranscriptPath, core.AgentTranscript(input.TranscriptPath, input.AgentID)} {
		if path == "" {
			continue
		}
		if info, err := os.Stat(path); err == nil && !info.Mode().IsRegular() {
			return "not a regular file"
		}
	}
	return ""
}

// procParent — ppid процесса из /proc/<pid>/stat
func procParent(pid int) (int, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, err
	}
	end := bytes.LastIndexByte(data, ')')
	if end < 0 {
		return 0, fmt.Errorf("malformed /proc/%d/stat", pid)
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) < 2 {
		return 0, fmt.Errorf("malformed /proc/%d/stat", pid)
	}
	return strconv.Atoi(fields[1])
}

// peerCred — uid и pid процесса на том конце сокета (SO_PEERCRED)
func peerCred(conn *net.UnixConn) (*unix.Ucred, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return nil, err
	}
	var cred *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return nil, err
	}
	return cred, credErr
}

func withoutEnv(env []string, key string) []string {
	out := make([]string, 0, len(env))
	for _, entry := range env {
		if !strings.HasPrefix(entry, key+"=") {
			out = append(out, entry)
		}
	}
	return out
}

// outputBuffer — stdout или stderr запроса: пишут обработчик и его
// горутины; сверх hookwire.MaxOutput байты отбрасываются, после ответа
// (seal) — тоже
type outputBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	truncated bool
	sealed    bool
}

func (b *outputBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.sealed {
		return len(p), nil
	}
	if room := hookwire.MaxOutput - b.buf.Len(); len(p) > room {
		b.truncated = true
		b.buf.Write(p[:max(room, 0)])
		return len(p), nil
	}
	b.buf.Write(p)
	return len(p), nil
}

// seal — ответ ушёл: дальнейшие записи отбрасываются
func (b *outputBuffer) seal() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sealed = true
}

func (b *outputBuffer) truncatedOutput() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.truncated
}

func (b *outputBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.buf.Bytes())
}

// fallbackReasonKey — ключ ctx CLI с причиной запасного пути прокси
type fallbackReasonKey struct{}

// takeFallbackReason снимает CHOOKS_FALLBACK с env запроса CLI и процесса:
// дочерние процессы хука (судья claude -p и его хуки) её не наследуют, и
// следующий вызов не выглядит запасным путём
func takeFallbackReason(request *hookctx.Context) string {
	reason, ok := request.LookupEnv(hookproxy.EnvFallback)
	if !ok {
		return ""
	}
	request.Env = withoutEnv(request.Env, hookproxy.EnvFallback)
	//nolint:silent-error-handling // Unsetenv падает только на имени с «=» или NUL; имя — константа
	_ = os.Unsetenv(hookproxy.EnvFallback)
	return reason
}

// logCLIFallback пишет в chooks.log строку о запасном пути: CLI исполняет
// вызов, который прокси не отдал демону. Только при логе в файл: строка в
// stderr попала бы в вывод хука
func logCLIFallback(config *core.Config, request hookctx.Context, hookType, reason string) {
	if config.Logger.Output != "file" || config.Logger.LogFile == "" {
		return
	}
	line := fmt.Sprintf("time=%q level=INFO msg=%q hook=%s reason=%q pid=%d",
		time.Now().In(request.Location()).Format(time.DateTime), "hook via CLI fallback", hookType, reason, request.PID)
	if err := core.AppendLine(config.Logger.LogFile, config.Logger.MaxSizeMB, line); err != nil {
		fmt.Fprintf(request.Stderr, "chooks: fallback line not logged: %v\n", err)
	}
}
