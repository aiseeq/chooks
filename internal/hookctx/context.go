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

// Abs resolves a relative path using the hook's working directory. A request
// of this very process (FromProcess) keeps the path relative: the OS resolves
// it from the same cwd, and error texts name it as the env or config gave it,
// as the CLI always did. Any other request (a daemon serving a hook) joins it
// to the request cwd, since the executor's cwd belongs to nobody's hook.
func (c Context) Abs(path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	if c.process {
		return path
	}
	return filepath.Join(c.Cwd, path)
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

// LookPath searches the request's PATH without consulting the executor's cwd.
func (c Context) LookPath(name string) (string, error) {
	for _, dir := range filepath.SplitList(c.Getenv("PATH")) {
		path := filepath.Join(c.Abs(dir), name)
		if executable(path) {
			if !filepath.IsAbs(dir) {
				return "", &exec.Error{Name: name, Err: exec.ErrDot}
			}
			return path, nil
		}
	}
	return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
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
