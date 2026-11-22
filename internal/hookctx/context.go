// Package hookctx describes the process state supplied with one hook request.
package hookctx

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
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
	home := request.Getenv("HOME")
	if home == "" {
		return "", fmt.Errorf("$HOME is not defined")
	}
	return request.Abs(home), nil
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
	// process marks the request of this very process (FromProcess): its Cwd
	// is the process cwd, so relative paths need no resolving
	process bool
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
	base := c.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		base = c.Getenv("TMPDIR")
		if base == "" {
			base = "/tmp"
		}
	}
	return filepath.Join(c.Abs(base), "chooks")
}

// StateDir resolves the persistent chooks state directory for the request.
func (c Context) StateDir() (string, error) {
	base := c.Getenv("XDG_STATE_HOME")
	if base == "" {
		home := c.Getenv("HOME")
		if home == "" {
			return "", fmt.Errorf("cannot resolve home dir: $HOME is not defined")
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(c.Abs(base), "chooks"), nil
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
