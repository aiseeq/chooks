// Command daemon сверяет пару прокси+демон chooks с CLI и меряет её время.
//
//	go run ./scripts/daemon equal --cli BIN --proxy BIN --inputs DIR --work DIR [--report FILE]
//	go run ./scripts/daemon timing --cli BIN --proxy BIN --work DIR [-n 200]
//	go run ./scripts/daemon timing --mode shadow --cli BIN --work DIR [-n 200]
//
// equal — равенство на наборе входов этапа 2а (equal.py и equal2.py из
// hd/s2a/review2/equality): код, stdout, stderr и файлы песочницы у CLI и у
// прокси к демону `chooks serve` на временном сокете. timing — N
// последовательных pre-tool-use `git status`: CLI против прокси+демона, p50 и
// p95 в мс, и цена старта прокси. timing --mode shadow — CLI с тенью и без
// неё (этап 2б-3а): по входу на каждую сравниваемую подкоманду, N вызовов
// каждой стороны вперемешку, p50 и p95, прирост p50 (порог 2 мс), исходы
// журнала тени демона; затем плохой демон — сокета нет и демон не читает
// запись больше буфера сокета (порог прироста 100 мс).
//
// Код выхода: 0 — расхождений нет, 1 — есть, 2 — сбой прогона.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/aiseeq/akit/proc"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: daemon equal|timing [flags]")
		os.Exit(2)
	}
	var code int
	var err error
	switch os.Args[1] {
	case "equal":
		code, err = runEqual(os.Args[2:])
	case "timing":
		code, err = runTiming(os.Args[2:])
	default:
		err = fmt.Errorf("unknown mode %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "daemon:", err)
		os.Exit(2)
	}
	os.Exit(code)
}

// binaries — общие флаги: бинари CLI и прокси, рабочий каталог
type binaries struct {
	cli, proxy, work string
}

func (b *binaries) register(fs *flag.FlagSet) {
	fs.StringVar(&b.cli, "cli", "", "бинарь chooks")
	fs.StringVar(&b.proxy, "proxy", "", "бинарь chooks-hook")
	fs.StringVar(&b.work, "work", "", "пустой каталог для песочниц")
}

func (b *binaries) resolve() error {
	if b.proxy == "" {
		return errors.New("--cli, --proxy and --work are required")
	}
	if err := b.resolveCLI(); err != nil {
		return err
	}
	return absPath(&b.proxy)
}

// resolveCLI — флаги режима без прокси: --cli и --work
func (b *binaries) resolveCLI() error {
	if b.cli == "" || b.work == "" {
		return errors.New("--cli and --work are required")
	}
	for _, p := range []*string{&b.cli, &b.work} {
		if err := absPath(p); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(b.work, 0o700); err != nil {
		return fmt.Errorf("work dir: %w", err)
	}
	return nil
}

func absPath(p *string) error {
	abs, err := filepath.Abs(*p)
	if err != nil {
		return fmt.Errorf("abs %s: %w", *p, err)
	}
	*p = abs
	return nil
}

// daemon — `chooks serve` на сокете во временном каталоге TMPDIR, а при
// длинном TMPDIR — /tmp (путь сокета не длиннее 107 байт); HOME, cwd и PATH у
// него свои, не песочницы
type daemon struct {
	cmd    *exec.Cmd
	socket string
	dir    string
}

// config — текст конфига демона ("" — без конфига, лог в stderr)
func startDaemon(cli string, logFile *os.File, config string) (*daemon, error) {
	base := os.TempDir()
	if len(base) > 60 {
		base = "/tmp"
	}
	dir, err := os.MkdirTemp(base, "chooks-d-")
	if err != nil {
		return nil, fmt.Errorf("socket dir: %w", err)
	}
	home := filepath.Join(dir, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		return nil, fmt.Errorf("daemon home: %w", err)
	}
	if config != "" {
		if err := os.MkdirAll(filepath.Join(home, ".config", "chooks"), 0o700); err != nil {
			return nil, fmt.Errorf("daemon config dir: %w", err)
		}
		if err := os.WriteFile(filepath.Join(home, ".config", "chooks", "config.yaml"), []byte(config), 0o600); err != nil {
			return nil, fmt.Errorf("daemon config: %w", err)
		}
	}
	d := &daemon{socket: filepath.Join(dir, "d.sock"), dir: dir}
	d.cmd = exec.Command(cli, "serve", "--socket", d.socket)
	d.cmd.Dir = home
	d.cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin", "LANG=C.UTF-8", "TZ=Asia/Tokyo"}
	d.cmd.Stdout, d.cmd.Stderr = logFile, logFile
	if err := d.cmd.Start(); err != nil {
		return nil, fmt.Errorf("start daemon: %w", err)
	}
	for range 100 {
		if _, err := os.Stat(d.socket); err == nil {
			return d, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	code := d.stop()
	return nil, fmt.Errorf("daemon socket did not appear (daemon exit %d)", code)
}

// stop — SIGTERM, ожидание выхода, код выхода демона
func (d *daemon) stop() int {
	if _, err := proc.StopChild(d.cmd.Process.Pid, 30*time.Second, func() error { return d.cmd.Process.Signal(syscall.SIGTERM) }); err != nil {
		fmt.Fprintln(os.Stderr, "daemon: stop:", err)
	}
	err := d.cmd.Wait()
	if rmErr := os.RemoveAll(d.dir); rmErr != nil {
		fmt.Fprintln(os.Stderr, "daemon: remove socket dir:", rmErr)
	}
	return exitCode(err)
}

func exitCode(err error) int {
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exitErr):
		return exitErr.ExitCode()
	default:
		return -1
	}
}

// outcome — наблюдаемое одного вызова
type outcome struct {
	code           int
	stdout, stderr []byte
	pid            int
	elapsed        time.Duration
}

func runOne(binary string, args, env []string, dir string, stdin []byte) (outcome, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env, cmd.Dir = slices.Clone(env), dir
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	start := time.Now()
	err := cmd.Run()
	o := outcome{code: exitCode(err), stdout: stdout.Bytes(), stderr: stderr.Bytes(), elapsed: time.Since(start)}
	if cmd.Process != nil {
		o.pid = cmd.Process.Pid
	}
	if o.code == -1 {
		return o, fmt.Errorf("run %s %v: %w", binary, args, err)
	}
	return o, nil
}
