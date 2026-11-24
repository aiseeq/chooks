// Package hookctx describes the process state supplied with one hook request.
package hookctx

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/aiseeq/chooks/internal/procfs"
)

type contextKey struct{}

// WithRequest attaches the state of one hook invocation to its lifetime.
func WithRequest(ctx context.Context, request Context) context.Context {
	return context.WithValue(ctx, contextKey{}, request)
}

// Request retrieves the hook invocation state, when present.
func Request(ctx context.Context) (Context, bool) {
	request, ok := ctx.Value(contextKey{}).(Context)
	return request, ok
}

// UserHomeDir resolves HOME from the request when processing a hook.
func UserHomeDir(ctx context.Context) (string, error) {
	request, ok := Request(ctx)
	if !ok {
		return "", fmt.Errorf("hook request is missing")
	}
	home := request.Path(EnvHome)
	if home == "" {
		return "", fmt.Errorf("$HOME is not defined")
	}
	return home, nil
}

// PathKey — переменная env запроса, значение которой — путь к каталогу или
// файлу. Других путей из env хуки не читают: Path — единственный вход
type PathKey string

// Переменные-пути env запроса. XDG_CONFIG_HOME среди них нет: у неё своё
// правило относительного значения, его даёт ConfigDir
const (
	EnvHome             PathKey = "HOME"
	EnvRuntimeDir       PathKey = "XDG_RUNTIME_DIR"
	EnvTmpDir           PathKey = "TMPDIR"
	EnvStateHome        PathKey = "XDG_STATE_HOME"
	EnvClaudeConfigDir  PathKey = "CLAUDE_CONFIG_DIR"
	EnvModelsApproved   PathKey = "MODELS_APPROVED"
	EnvClaudeCodeTmpDir PathKey = "CLAUDE_CODE_TMPDIR"
)

// Path — значение key из env запроса как путь, который открывает из
// исполнителя то же, что ОС открыла бы из cwd запроса (Abs). Нет переменной
// или она пустая — "": вызывающий берёт своё умолчание, как база по пустому
// os.Getenv. У CLI значение как есть, у демона относительное — от cwd запроса
func (c Context) Path(key PathKey) string {
	value := c.Getenv(string(key))
	if value == "" {
		return ""
	}
	return c.Abs(value)
}

// RootPath — значение key как корень, с которым сравнивают абсолютные
// каталоги (лежит ли каталог под корнем), а не путь для открытия. У CLI
// значение как есть: относительный корень с абсолютным каталогом не
// совпадает никогда (filepath.Rel отказывает). У демона относительное
// значение — "": Path сделал бы его абсолютным, и совпадение, которого у CLI
// нет, появилось бы только у демона. Нет переменной или она пустая — ""
func (c Context) RootPath(key PathKey) string {
	value := c.Getenv(string(key))
	if c.process || filepath.IsAbs(value) {
		return value
	}
	return ""
}

// MatchPath — значение key для лексического сравнения с путями из ввода хука
// (имя проекта по каталогу, ~/ в команде): как есть у CLI и у демона. Файл по
// нему не открывается, и сравнение сырых строк даёт тот же исход, что у CLI;
// Path дал бы другой: относительный $HOME у демона стал бы абсолютным и
// совпал бы с абсолютным каталогом, с которым у CLI не совпадает
func (c Context) MatchPath(key PathKey) string {
	return c.Getenv(string(key))
}

// ChildDir — каталог запуска дочернего процесса запроса. У CLI ребёнок
// наследует cwd хука, даже удалённый, и стартует (Cwd пуст, если os.Getwd
// не смог). Демон в удалённый каталог chdir не сделает, и exec упал бы, хотя
// у CLI ребёнок стартует; поэтому у демона без каталога — "/". Относительный
// путь в env или аргументах ребёнка тогда ищется от "/", а у CLI не открылся
// бы: точно повторит CLI только cwd прокси, переданный дескриптором
func (c Context) ChildDir() string {
	if c.process {
		return c.Cwd
	}
	info, err := os.Stat(c.Cwd)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "/"
	case err != nil:
		// Иная ошибка stat: chdir ребёнка вернёт её сам при запуске
		return c.Cwd
	case !info.IsDir():
		// На месте каталога файл: каталога cwd больше нет
		return "/"
	}
	return c.Cwd
}

// ConfigDir — каталог конфигурации запроса по правилу os.UserConfigDir:
// $XDG_CONFIG_HOME, без него $HOME/.config. Относительный $XDG_CONFIG_HOME —
// ошибка, как у os.UserConfigDir в CLI базы: от cwd он не разрешается ни у
// CLI, ни у демона. $HOME — по Path
func (c Context) ConfigDir() (string, error) {
	dir := c.Getenv("XDG_CONFIG_HOME")
	switch {
	case dir == "":
		home := c.Path(EnvHome)
		if home == "" {
			return "", fmt.Errorf("neither $XDG_CONFIG_HOME nor $HOME are defined")
		}
		return filepath.Join(home, ".config"), nil
	case !filepath.IsAbs(dir):
		return "", fmt.Errorf("path in $XDG_CONFIG_HOME is relative")
	}
	return dir, nil
}

// Context contains the state of the hook process, not that of the executor.
// Env is copied by New, so later changes to the caller's slice do not affect it.
type Context struct {
	Stdin      io.Reader
	Stdout     io.Writer
	Stderr     io.Writer
	Env        []string
	Cwd        string
	PID        int
	PPID       int
	ConfigPath string
	Verbose    bool
	Timeout    time.Duration
	// Time — время запроса: его берут хуки, сравниваемые тенью (Now), чтобы
	// CLI и демон решали при одном времени. Ноль — time.Now
	Time time.Time
	// Proc — чтение /proc запроса; nil — /proc машины (ProcFS)
	Proc procfs.FS
	// process marks the request of this very process (FromProcess): its Cwd
	// is the process cwd, so relative paths need no resolving
	process bool
	// runtimeBase и stateBase заменяют $XDG_RUNTIME_DIR и $XDG_STATE_HOME
	// у RuntimeDir и StateDir: тень демона пишет состояние в свой временный
	// каталог (WithStateRoots)
	runtimeBase, stateBase string
}

// Now — время запроса (Time), без него — time.Now
func (c Context) Now() time.Time {
	if c.Time.IsZero() {
		return time.Now()
	}
	return c.Time
}

// ProcFS — чтение /proc запроса: Proc или /proc машины
func (c Context) ProcFS() procfs.FS {
	if c.Proc == nil {
		return procfs.OS{}
	}
	return c.Proc
}

// WithStateRoots — запрос, у которого RuntimeDir и StateDir лежат под
// runtimeBase и stateHome вместо $XDG_RUNTIME_DIR и $XDG_STATE_HOME:
// теневое исполнение в демоне не трогает состояние пользователя
func (c Context) WithStateRoots(runtimeBase, stateHome string) Context {
	c.runtimeBase, c.stateBase = runtimeBase, stateHome
	return c
}

// New constructs a request context from values supplied by the caller.
func New(stdin io.Reader, stdout, stderr io.Writer, env []string, cwd string, pid, ppid int) Context {
	return Context{Stdin: stdin, Stdout: stdout, Stderr: stderr,
		Env: append([]string(nil), env...), Cwd: cwd, PID: pid, PPID: ppid, Timeout: 5 * time.Second}
}

// FromProcess captures the current process for the standalone CLI.
func FromProcess() (Context, error) {
	cwd, err := os.Getwd()
	request := New(os.Stdin, os.Stdout, os.Stderr, os.Environ(), cwd, os.Getpid(), os.Getppid())
	request.process = true
	return request, err
}

// LookupEnv distinguishes an absent variable from one set to an empty value.
func (c Context) LookupEnv(key string) (string, bool) {
	for _, entry := range c.Env {
		name, value, ok := strings.Cut(entry, "=")
		if ok && name == key {
			return value, true
		}
	}
	return "", false
}

// Getenv returns the value of a request environment variable.
func (c Context) Getenv(key string) string {
	value, _ := c.LookupEnv(key)
	return value
}

// Location resolves the request's TZ without changing the executor's timezone.
func (c Context) Location() *time.Location {
	tz, ok := c.LookupEnv("TZ")
	if !ok {
		if c.process {
			return time.Local
		}
		tz = "/etc/localtime"
	}
	tz = strings.TrimPrefix(tz, ":")
	if tz == "" {
		return time.UTC
	}
	if filepath.IsAbs(tz) {
		data, err := os.ReadFile(tz)
		if err == nil {
			if loc, err := time.LoadLocationFromTZData(tz, data); err == nil {
				return loc
			}
		}
		return time.UTC
	}
	if !c.process {
		loc, err := c.requestZone(tz)
		if err == nil {
			return loc
		}
		// Свой источник запроса пояса не дал или его нет: дальше системная
		// база, как в time.LoadLocation после $ZONEINFO. time.LoadLocation
		// сам смотрит $ZONEINFO исполнителя, а он запросу не принадлежит
		if loc, err := platformZone(tz); err == nil {
			return loc
		}
		if _, set := os.LookupEnv("ZONEINFO"); set {
			return time.UTC
		}
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return time.UTC
	}
	return loc
}

// platformZoneSources — каталоги базы поясов, где их ищет пакет time на unix
// (zoneinfo_unix.go)
var platformZoneSources = []string{"/usr/share/zoneinfo/", "/usr/share/lib/zoneinfo/", "/usr/lib/locale/TZ/", "/etc/zoneinfo"}

// requestZone — то, что в процессе хука time.LoadLocation взял бы из его env,
// а у демона взял бы из env демона. «Local» — пояс процесса: у хука с
// TZ=Local это файл Local из системных каталогов (time.initLocal), иначе UTC.
// $ZONEINFO запроса (каталог или несжатый zip, относительный — от cwd
// запроса) смотрится до системной базы, как в time.LoadLocation.
// errNoRequestZone — решает time.LoadLocation: имя без $ZONEINFO и без
// «Local»; иная ошибка — $ZONEINFO пояса не дал, и time.LoadLocation тоже
// идёт дальше, в системную базу
func (c Context) requestZone(name string) (*time.Location, error) {
	if name == "Local" {
		for _, dir := range platformZoneSources {
			if data, err := os.ReadFile(filepath.Join(dir, name)); err == nil {
				if loc, err := time.LoadLocationFromTZData(name, data); err == nil {
					return loc, nil
				}
			}
		}
		return time.UTC, nil
	}
	zoneinfo := c.Getenv("ZONEINFO")
	if zoneinfo == "" || name == "UTC" || strings.Contains(name, "..") || strings.HasPrefix(name, "\\") {
		return nil, errNoRequestZone
	}
	data, err := zoneData(c.Abs(zoneinfo), name)
	if err != nil {
		return nil, fmt.Errorf("zone %s from $ZONEINFO: %w", name, err)
	}
	loc, err := time.LoadLocationFromTZData(name, data)
	if err != nil {
		return nil, fmt.Errorf("zone %s from $ZONEINFO: %w", name, err)
	}
	return loc, nil
}

// platformZone — пояс name из системных каталогов, как loadLocation пакета
// time после $ZONEINFO. UTC и имена, которые time.LoadLocation отвергает,
// решает он сам (errNoRequestZone)
func platformZone(name string) (*time.Location, error) {
	if name == "UTC" || name == "Local" || strings.Contains(name, "..") || strings.HasPrefix(name, "\\") {
		return nil, errNoRequestZone
	}
	last := fmt.Errorf("zone %s not in system zoneinfo", name)
	for _, dir := range platformZoneSources {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			last = err
			continue
		}
		loc, err := time.LoadLocationFromTZData(name, data)
		if err != nil {
			last = err
			continue
		}
		return loc, nil
	}
	return nil, last
}

// errNoRequestZone — у запроса нет своего источника пояса
var errNoRequestZone = errors.New("no request zone source")

// zoneData читает пояс name из каталога или zip-архива source, как
// loadTzinfoFromDirOrZip пакета time
func zoneData(source, name string) ([]byte, error) {
	if !strings.HasSuffix(source, ".zip") {
		return os.ReadFile(source + "/" + name)
	}
	archive, err := zip.OpenReader(source)
	if err != nil {
		return nil, fmt.Errorf("open zone archive: %w", err)
	}
	defer archive.Close()
	file, err := archive.Open(name)
	if err != nil {
		return nil, fmt.Errorf("zone %s in archive %s: %w", name, source, err)
	}
	defer file.Close()
	return io.ReadAll(file)
}

// Abs gives the path that opens, from the executor, the file the OS would open
// for this path from the hook's cwd. Nothing is cleaned: with a symlink
// link/../x is not x. An absolute path and any path of this very process's
// request (FromProcess, the CLI) stay exactly as given: the OS resolves a
// relative one from the same cwd, and error texts name it as the env or
// config gave it. A relative path of another request (a daemon serving a
// hook) is appended to the request cwd with the symlinks of the cwd itself
// resolved: callers join and clean further (filepath.Join(home, ".claude")),
// and a leading .. then must step out of the directory the OS stands in, not
// out of a symlink's name. A cwd that does not resolve is used as given.
func (c Context) Abs(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	if c.process {
		return path
	}
	if c.Cwd == "" {
		return path
	}
	dir, err := filepath.EvalSymlinks(c.Cwd)
	if err != nil {
		//nolint:fallback-return // cwd без каталога или прав берётся как дал запрос: ОС от него откроет путь или вернёт ошибку открытия, симлинки cwd нужны лишь для чистки у вызывающих
		dir = c.Cwd
	}
	return strings.TrimSuffix(dir, "/") + "/" + path
}

// RuntimeDir resolves the short-lived state directory from this request.
func (c Context) RuntimeDir() string {
	if c.runtimeBase != "" {
		return filepath.Join(c.runtimeBase, "chooks")
	}
	base := c.Path(EnvRuntimeDir)
	if base == "" {
		base = c.TempDir()
	}
	return filepath.Join(base, "chooks")
}

// TempDir — временный каталог запроса по правилу os.TempDir: $TMPDIR, без
// него /tmp
func (c Context) TempDir() string {
	if dir := c.Path(EnvTmpDir); dir != "" {
		return dir
	}
	return "/tmp"
}

// StateDir resolves the persistent chooks state directory for the request.
func (c Context) StateDir() (string, error) {
	if c.stateBase != "" {
		return filepath.Join(c.stateBase, "chooks"), nil
	}
	base := c.Path(EnvStateHome)
	if base == "" {
		home := c.Path(EnvHome)
		if home == "" {
			return "", fmt.Errorf("cannot resolve home dir: $HOME is not defined")
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "chooks"), nil
}

// LookPath ищет name так, как LookPath из os/exec в процессе запроса. У CLI
// это он сам по env и cwd процесса, как в базе. У демона — его повтор
// (lp_unix.go lookPath) по PATH и GODEBUG из env запроса: пустой элемент PATH —
// «.», относительный элемент разрешается от cwd запроса и без
// GODEBUG=execerrdot=0 даёт ErrDot. Найденный путь у демона абсолютный:
// filepath.Join после Abs чистит его так же, как Join(dir, name) базы чистит
// путь от cwd процесса, а cwd в Abs уже без симлинков
func (c Context) LookPath(name string) (string, error) {
	if c.process {
		return exec.LookPath(name)
	}
	switch name {
	case "", ".", "..":
		return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
	}
	if strings.Contains(name, "/") {
		path := c.Abs(name)
		if !executable(path) {
			return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
		}
		return path, nil
	}
	for _, dir := range filepath.SplitList(c.Getenv("PATH")) {
		if dir == "" {
			dir = "."
		}
		path := filepath.Join(c.Abs(dir), name)
		if executable(path) {
			if !filepath.IsAbs(dir) && c.godebug("execerrdot") != "0" {
				return "", &exec.Error{Name: name, Err: exec.ErrDot}
			}
			return path, nil
		}
	}
	return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
}

// godebug — значение настройки name в GODEBUG запроса, как его читает
// internal/godebug: пары через «,», последняя пара имени главнее, хвост
// после «#» — настройка bisect, не значение. Своей директивы godebug в go.mod
// нет, поэтому без пары в env действует умолчание Go
func (c Context) godebug(name string) string {
	pairs := strings.Split(c.Getenv("GODEBUG"), ",")
	for i := len(pairs) - 1; i >= 0; i-- {
		key, value, ok := strings.Cut(pairs[i], "=")
		if !ok || key != name {
			continue
		}
		value, _, _ = strings.Cut(value, "#")
		return value
	}
	return ""
}

func executable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	err = unix.Faccessat(unix.AT_FDCWD, path, unix.X_OK, unix.AT_EACCESS)
	if err == nil {
		return true
	}
	return (err == syscall.ENOSYS || err == syscall.EPERM) && info.Mode()&0o111 != 0
}
