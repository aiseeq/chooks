// Package hookproxy — прокси хука chooks-hook: пересылает вызов хука демону
// chooks serve по unix-сокету и отдаёт его stdout, stderr и код. Демона нет
// или «принято» не пришло в срок — execve нынешнего CLI chooks с тем же stdin.
// Импорты — только os, os/signal, sync, syscall, x/sys/unix, time и hookwire:
// запуск прокси — часть времени каждого хука.
package hookproxy

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/aiseeq/chooks/internal/hookwire"
)

// Переменные env прокси
const (
	// EnvSocket — путь сокета демона вместо $XDG_RUNTIME_DIR/chooks.sock
	// (тесты)
	EnvSocket = "CHOOKS_HOOK_SOCKET"
	// EnvCLI — бинарь запасного пути вместо ~/bin/chooks (тесты)
	EnvCLI = "CHOOKS_HOOK_CLI"
	// EnvFallback — причина запасного пути; её получает CLI
	EnvFallback = "CHOOKS_FALLBACK"
)

// LostPrefix — начало причины, с которой прокси зовёт CLI после обрыва
// связи с демоном после 'C': эффект хука мог уже случиться, и CLI хук не
// исполняет, а только записывает вызов в счёт и в файл длительностей и
// отвечает кодом 1 (ревью 8f10b59, п. 3)
const LostPrefix = "lost: "

// Lost — reason несёт обрыв после 'C': CLI хук не повторяет
func Lost(reason string) bool {
	return strings.HasPrefix(reason, LostPrefix)
}

// LateReason — причина 'F' от демона вместо результата: 'C' пришёл после
// срока демона, запрос не исполнялся
const LateReason = "late"

// Unavailable — reason значит, что демон недоступен: не принял соединение,
// не ответил «принято» в срок, связь оборвалась до или после 'C'. Только
// такие уходы в CLI идут в счёт уведомления; обновление демона («upgrade»)
// и уходы по замыслу (env, subcommand, transcript, size, argv, stdin) — нет
// (ревью 8f10b59, п. 2)
func Unavailable(reason string) bool {
	if reason == commitPrefix+LateReason {
		return false
	}
	for _, prefix := range []string{connectPrefix, ackPrefix, sendPrefix, commitPrefix, LostPrefix} {
		if strings.HasPrefix(reason, prefix) {
			return true
		}
	}
	return false
}

// Начала причин, которые ставит сам прокси: шаг протокола, на котором
// связь с демоном не сложилась
const (
	connectPrefix = "connect: "
	sendPrefix    = "send: "
	ackPrefix     = "ack: "
	commitPrefix  = "commit: "
)

// AckWindow — срок от отправки запроса до «принято». Не пришло — CLI:
// пользователь ждёт демона не дольше этого
const AckWindow = 100 * time.Millisecond

// ackPerMiB — добавка к сроку на каждый МиБ stdin: большой запрос демон
// читает дольше, а прокси не должен уходить в CLI из-за размера
const ackPerMiB = 20 * time.Millisecond

// KilledExitCode — код выхода прокси по SIGTERM, как у CLI chooks
// (killedExitCode): 128 + SIGTERM
const KilledExitCode = 128 + int(syscall.SIGTERM)

// Main — прокси целиком: args — os.Args. Возвращает код выхода; на запасном
// пути не возвращается (execve)
func Main(args []string) int {
	p := &proxy{args: args, fd: -1, term: termState{fd: -1}}
	return p.run()
}

type proxy struct {
	args []string
	fd   int // сокет демона; -1 — нет. Только горутина run
	term termState
}

// termState — что видит горутина SIGTERM: сокет, по которому слать 'T', и
// отправлен ли 'C'. close снимает сокет отсюда до его закрытия: номер fd не
// переиспользуется под 'T'
type termState struct {
	mu        sync.Mutex
	fd        int  // -1 — 'T' слать некуда
	committed bool // 'C' отправлен: исполнение у демона
}

// watchTerm — SIGTERM от Claude Code (таймаут хука или отмена хода). После
// 'C' прокси сообщает демону 'T': демон пишет исход «killed» или «cancelled»,
// как CLI на SIGTERM, и отменяет запрос. Выход — KilledExitCode, как у CLI.
// На запасном пути после execve обработчик снят: SIGTERM получает CLI
func (p *proxy) watchTerm() {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM)
	go func() {
		<-signals
		p.term.mu.Lock()
		if p.term.committed && p.term.fd >= 0 {
			// При сбое доставки демон увидит EOF; прокси обязан завершиться с кодом SIGTERM.
			//nolint:ignored-error // обработчик SIGTERM завершает процесс, вернуть или залогировать ошибку нельзя
			_, _ = syscall.SendmsgN(p.term.fd, []byte{hookwire.Terminate}, nil, nil, syscall.MSG_NOSIGNAL|syscall.MSG_DONTWAIT)
		}
		p.term.mu.Unlock()
		os.Exit(KilledExitCode)
	}()
}

func (p *proxy) run() int {
	p.watchTerm()
	path := socketPath()
	if path == "" {
		return p.fallback(nil, false, connectPrefix+"no socket path")
	}
	fd, err := dial(path, time.Now().Add(AckWindow))
	if err != nil {
		// Демона нет: stdin не тронут, CLI читает его сам
		return p.fallback(nil, false, connectPrefix+err.Error())
	}
	p.fd = fd
	p.term.mu.Lock()
	p.term.fd = fd
	p.term.mu.Unlock()
	stdin, err := readAll(0)
	if err != nil {
		p.close()
		return p.fallback(stdin, true, "stdin: "+err.Error())
	}
	if len(stdin) > hookwire.MaxStdin {
		p.close()
		return p.fallback(stdin, true, "stdin too large")
	}
	cwd, cwdErr := os.Getwd()
	if cwdErr != nil {
		p.close()
		return p.fallback(stdin, true, "cwd: "+cwdErr.Error())
	}
	request := hookwire.Request{Env: os.Environ(), Cwd: cwd, PID: os.Getpid(), PPID: os.Getppid(), Stdin: stdin}
	if len(p.args) > 1 {
		request.Subcommand = p.args[1]
		request.Args = p.args[2:]
	}
	deadline := time.Now().Add(AckWindow + time.Duration(len(stdin)>>20)*ackPerMiB)
	if err := writeAll(p.fd, hookwire.EncodeRequest(request), deadline); err != nil {
		reason := sendPrefix + err.Error()
		refusal, refusalErr := p.earlyRefusal(deadline)
		if refusalErr != nil {
			reason += " (no refusal: " + refusalErr.Error() + ")"
		} else {
			reason = refusal
		}
		p.close()
		return p.fallback(stdin, true, reason)
	}
	var b [1]byte
	if err := readFull(p.fd, b[:], deadline); err != nil {
		p.close()
		return p.fallback(stdin, true, ackPrefix+err.Error())
	}
	switch b[0] {
	case hookwire.Accepted:
	case hookwire.Fallback:
		reason, err := hookwire.ReadReason(deadlineReader{fd: p.fd, deadline: deadline})
		p.close()
		if err != nil {
			return p.fallback(stdin, true, ackPrefix+err.Error())
		}
		return p.fallback(stdin, true, reason)
	default:
		p.close()
		return p.fallback(stdin, true, ackPrefix+"unexpected byte")
	}
	// «Принято» пришло в срок: 'C' отдаёт исполнение демону. Не ушло — демон
	// запрос не исполнит, CLI его исполнит один раз. Запись 'C' и флаг — под
	// одним замком: SIGTERM между ними иначе выходил без 'T' — демон отменял
	// запрос по EOF, но записывал его как оборванный прокси, а не killed
	p.term.mu.Lock()
	commitErr := writeAll(p.fd, []byte{hookwire.Commit}, deadline)
	p.term.committed = commitErr == nil
	p.term.mu.Unlock()
	if commitErr != nil {
		p.close()
		return p.fallback(stdin, true, commitPrefix+commitErr.Error())
	}
	return p.await(stdin)
}

// earlyRefusal — причина 'F', которую демон прислал, не дочитав запрос, и
// закрыл сокет: запись прокси тогда кончается EPIPE, а ответ ждёт в сокете.
// Без этого отказ «upgrade» на дообслуживании выглядел бы обрывом «send:»
// и шёл бы в счёт уведомления. Ошибка — отказа в сокете нет
func (p *proxy) earlyRefusal(deadline time.Time) (string, error) {
	var b [1]byte
	if err := readFull(p.fd, b[:], deadline); err != nil {
		return "", err
	}
	if b[0] != hookwire.Fallback {
		return "", errNoRefusal
	}
	return hookwire.ReadReason(deadlineReader{fd: p.fd, deadline: deadline})
}

// errNoRefusal — после сбоя записи в сокете не 'F'
var errNoRefusal = syscall.EPROTO

// lateReasonWait — срок чтения причины 'F' после 'C': демон пишет её одним
// кадром
const lateReasonWait = time.Second

// await ждёт результат без срока: срок хука держит Claude Code, а демон
// отменяет запрос по своему сроку и по обрыву. Обрыв — CLI без повтора хука
// (lost): эффект хука мог уже случиться. 'F' вместо результата — демон 'C'
// не дождался и запрос не исполнял: CLI исполнит его один раз (ревью 2б-1,
// п. 8)
func (p *proxy) await(stdin []byte) int {
	r := deadlineReader{fd: p.fd}
	if err := setTimeout(p.fd, syscall.SO_RCVTIMEO, 0); err != nil {
		return p.lost(stdin, err)
	}
	var b [1]byte
	if err := readFull(p.fd, b[:], time.Time{}); err != nil {
		return p.lost(stdin, err)
	}
	if b[0] == hookwire.Fallback {
		reason, err := hookwire.ReadReason(deadlineReader{fd: p.fd, deadline: time.Now().Add(lateReasonWait)})
		if err != nil {
			return p.lost(stdin, err)
		}
		p.close()
		return p.fallback(stdin, true, commitPrefix+reason)
	}
	if b[0] != hookwire.Result {
		return p.lost(stdin, syscall.EPROTO)
	}
	code, stdout, stderr, err := hookwire.ReadResult(r)
	if err != nil {
		return p.lost(stdin, err)
	}
	p.close()
	if len(stdout) > 0 {
		if n, err := os.Stdout.Write(stdout); err != nil || n != len(stdout) {
			if err == nil {
				err = io.ErrShortWrite
			}
			// stdout — протокольный ответ Claude Code; сообщаем о потере
			// ответа на stderr, сохраняя код решения демона.
			_, _ = fmt.Fprintf(os.Stderr, "chooks-hook: stdout write failed: %v\n", err)
		}
	}
	if len(stderr) > 0 {
		_, _ = os.Stderr.Write(stderr) //nolint:ignored-error // stderr — последний канал диагностики: ошибку его записи сообщить некуда, код решения демона остаётся
	}
	return code
}

// lost — связь оборвалась после 'C' (демон упал или вышел посреди запроса):
// execve CLI с причиной LostPrefix. CLI хук не повторяет, а пишет вызов в
// счёт уходов и в файл длительностей и выходит с кодом 1 и строкой в stderr
// (LostMessage). CLI не запустился — та же строка от прокси, код 1
func (p *proxy) lost(stdin []byte, err error) int {
	p.close()
	return p.fallback(stdin, true, LostPrefix+err.Error())
}

// LostMessage — строка stderr вызова, оборванного после 'C'
func LostMessage(reason string) string {
	return "chooks-hook: связь с демоном оборвалась после «принято», хук не повторён: " + strings.TrimPrefix(reason, LostPrefix) + "\n"
}

func (p *proxy) close() {
	p.term.mu.Lock()
	p.term.fd, p.term.committed = -1, false
	p.term.mu.Unlock()
	if p.fd >= 0 {
		_ = syscall.Close(p.fd)
		p.fd = -1
	}
}

// fallback — execve CLI с подкомандой и аргументами прокси. consumed — stdin
// уже прочитан: CLI получает его копию через memfd
func (p *proxy) fallback(stdin []byte, consumed bool, reason string) int {
	cli := cliPath()
	if consumed {
		if err := replaceStdin(stdin); err != nil {
			_, _ = os.Stderr.WriteString("chooks-hook: stdin для " + cli + ": " + err.Error() + "\n") //nolint:ignored-error // при ошибке stderr сообщить её больше некуда
			return 1
		}
	}
	argv := append([]string{cli}, p.args[min(1, len(p.args)):]...)
	env := withEnv(os.Environ(), EnvFallback, strings.ReplaceAll(reason, "\n", " "))
	err := syscall.Exec(cli, argv, env)
	if Lost(reason) {
		_, _ = os.Stderr.WriteString(LostMessage(reason)) //nolint:ignored-error // при ошибке stderr сообщить её больше некуда
	}
	_, _ = os.Stderr.WriteString("chooks-hook: запасной путь " + cli + ": " + err.Error() + "\n") //nolint:ignored-error // при ошибке stderr сообщить её больше некуда
	return 1
}

func socketPath() string {
	return SocketPath(os.Getenv)
}

// SocketPath — путь сокета демона по env: $CHOOKS_HOOK_SOCKET, иначе
// $XDG_RUNTIME_DIR/chooks.sock (только абсолютный); "" — пути нет
func SocketPath(getenv func(string) string) string {
	if path := getenv(EnvSocket); path != "" {
		return path
	}
	if dir := getenv("XDG_RUNTIME_DIR"); strings.HasPrefix(dir, "/") {
		return dir + "/chooks.sock"
	}
	return ""
}

// SendShadow — теневая запись CLI демону (этап 2б-3а): connect, затем кадр
// от frame целиком и close. Ответа не ждёт. Connect и запись — каждый не
// дольше timeout; кадр строится после connect: без демона CLI его не
// собирает. Демона нет — ошибка dial сразу; очередь приёма полна — dial к
// сроку; демон не читает (кадр больше буфера сокета) — write к сроку. CLI,
// уже ответивший сам, молча продолжает. Ошибка — *ShadowError с этапом
func SendShadow(path string, timeout time.Duration, frame func() ([]byte, error)) error {
	fd, err := dial(path, time.Now().Add(timeout))
	if err != nil {
		return &ShadowError{Stage: ShadowDial, Err: err}
	}
	defer syscall.Close(fd)
	data, err := frame()
	if err != nil {
		return &ShadowError{Stage: ShadowFrame, Err: err}
	}
	if err := writeAll(fd, data, time.Now().Add(timeout)); err != nil {
		return &ShadowError{Stage: ShadowWrite, Err: err}
	}
	return nil
}

// Этапы отправки тени
const (
	ShadowDial  = "dial"
	ShadowFrame = "frame"
	ShadowWrite = "write"
)

// ShadowError — тень не отправлена: этап и причина (errno)
type ShadowError struct {
	Stage string
	Err   error
}

func (e *ShadowError) Error() string { return "shadow " + e.Stage + ": " + e.Err.Error() }

func (e *ShadowError) Unwrap() error { return e.Err }

// cliPath — ~/bin/chooks; без абсолютного HOME — chooks рядом с прокси
func cliPath() string {
	if path := os.Getenv(EnvCLI); path != "" {
		return path
	}
	if home := os.Getenv("HOME"); strings.HasPrefix(home, "/") {
		return strings.TrimSuffix(home, "/") + "/bin/chooks"
	}
	if exe, err := os.Executable(); err == nil {
		if i := strings.LastIndexByte(exe, '/'); i >= 0 {
			return exe[:i+1] + "chooks"
		}
	}
	return "chooks"
}

func withEnv(env []string, key, value string) []string {
	out := make([]string, 0, len(env)+1)
	for _, e := range env {
		if !strings.HasPrefix(e, key+"=") {
			out = append(out, e)
		}
	}
	return append(out, key+"="+value)
}

// replaceStdin ставит на fd 0 memfd с data: CLI читает тот же stdin, что
// прочитал прокси, любого размера (pipe с буфером 64 КиБ без писателя
// повис бы)
func replaceStdin(data []byte) error {
	fd, err := unix.MemfdCreate("chooks-hook-stdin", unix.MFD_CLOEXEC)
	if err != nil {
		return err //nolint:error-wrap // ошибка выводится CLI пользователю без изменения исходного текста
	}
	defer syscall.Close(fd)
	for off := 0; off < len(data); {
		n, err := syscall.Write(fd, data[off:])
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return err //nolint:error-wrap // ошибка выводится CLI пользователю без изменения исходного текста
		}
		off += n
	}
	if _, err := syscall.Seek(fd, 0, 0); err != nil {
		return err //nolint:error-wrap // ошибка выводится CLI пользователю без изменения исходного текста
	}
	return syscall.Dup3(fd, 0, 0)
}

func readAll(fd int) ([]byte, error) {
	buf := make([]byte, 0, 16<<10)
	for {
		if len(buf) == cap(buf) {
			buf = append(buf, 0)[:len(buf)]
		}
		n, err := syscall.Read(fd, buf[len(buf):cap(buf)])
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return buf, fmt.Errorf("read hook stdin: %w", err)
		}
		if n == 0 {
			return buf, nil
		}
		buf = buf[:len(buf)+n]
	}
}

// dial — connect к unix-сокету со сроком: при полной очереди приёма connect
// ждёт не дольше SO_SNDTIMEO
func dial(path string, deadline time.Time) (int, error) {
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("create unix socket: %w", err)
	}
	if err := setTimeout(fd, syscall.SO_SNDTIMEO, time.Until(deadline)); err != nil {
		_ = syscall.Close(fd)
		return -1, err
	}
	for {
		err = syscall.Connect(fd, &syscall.SockaddrUnix{Name: path})
		if err != syscall.EINTR {
			break
		}
	}
	if err != nil {
		_ = syscall.Close(fd)
		return -1, err //nolint:error-wrap // текст connect-ошибки используется как причина CLI fallback
	}
	return fd, nil
}

// errTimeout — срок вышел
var errTimeout = syscall.ETIMEDOUT

// setTimeout ставит SO_RCVTIMEO или SO_SNDTIMEO; 0 — без срока. Срок уже
// вышел — errTimeout
func setTimeout(fd, opt int, d time.Duration) error {
	if d < 0 || (d == 0 && opt == syscall.SO_SNDTIMEO) {
		return errTimeout
	}
	if d > 0 && d < time.Microsecond {
		d = time.Microsecond
	}
	tv := syscall.NsecToTimeval(d.Nanoseconds())
	return syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, opt, &tv)
}

func remaining(deadline time.Time) (time.Duration, error) {
	if deadline.IsZero() {
		return 0, nil
	}
	d := time.Until(deadline)
	if d <= 0 {
		return 0, errTimeout
	}
	return d, nil
}

// writeAll пишет b целиком до срока; MSG_NOSIGNAL — закрытый демоном сокет
// даёт EPIPE, а не SIGPIPE
func writeAll(fd int, b []byte, deadline time.Time) error {
	for len(b) > 0 {
		d, err := remaining(deadline)
		if err != nil {
			return err
		}
		if err := setTimeout(fd, syscall.SO_SNDTIMEO, d); err != nil {
			return err
		}
		n, err := syscall.SendmsgN(fd, b, nil, nil, syscall.MSG_NOSIGNAL)
		switch {
		case err == syscall.EINTR:
			continue
		case err == syscall.EAGAIN:
			return errTimeout
		case err != nil:
			return err
		}
		b = b[n:]
	}
	return nil
}

// readFull читает len(b) байт до срока; нулевой срок — без срока
func readFull(fd int, b []byte, deadline time.Time) error {
	for len(b) > 0 {
		d, err := remaining(deadline)
		if err != nil {
			return err
		}
		if !deadline.IsZero() {
			if err := setTimeout(fd, syscall.SO_RCVTIMEO, d); err != nil {
				return err
			}
		}
		n, err := syscall.Read(fd, b)
		switch {
		case err == syscall.EINTR:
			continue
		case err == syscall.EAGAIN:
			return errTimeout
		case err != nil:
			return err
		case n == 0:
			return syscall.ECONNRESET
		}
		b = b[n:]
	}
	return nil
}

// deadlineReader — io.Reader поверх сокета со сроком для hookwire
type deadlineReader struct {
	fd       int
	deadline time.Time
}

func (r deadlineReader) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	for {
		d, err := remaining(r.deadline)
		if err != nil {
			return 0, err
		}
		if !r.deadline.IsZero() {
			if err := setTimeout(r.fd, syscall.SO_RCVTIMEO, d); err != nil {
				return 0, err
			}
		}
		n, err := syscall.Read(r.fd, b)
		switch {
		case err == syscall.EINTR:
			continue
		case err == syscall.EAGAIN:
			return 0, errTimeout
		case err != nil:
			return 0, err
		case n == 0:
			return 0, syscall.ECONNRESET
		}
		return n, nil
	}
}
