package hookctx

import (
	"os"
	"path/filepath"
	"testing"
)

// Каждый ключ-путь env: CLI (FromProcess из cwd хука) и запрос демона (cwd
// процесса — другой каталог) по Path открывают один файл. Пустое и
// отсутствующее значение — "" у обоих
func TestPathEnvCLIAndRequest(t *testing.T) {
	keys := []PathKey{EnvHome, EnvRuntimeDir, EnvTmpDir, EnvStateHome, EnvClaudeConfigDir, EnvModelsApproved, EnvClaudeCodeTmpDir}
	root := symlinkTree(t)
	cwd := filepath.Join(root, "link")
	executor := t.TempDir()
	if err := os.WriteFile(filepath.Join(executor, "config"), []byte("wrong"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, key := range keys {
		t.Run(string(key), func(t *testing.T) {
			for _, value := range []string{"../config", root + "/link/../config"} {
				env := []string{string(key) + "=" + value}
				t.Chdir(cwd)
				cli, err := FromProcess()
				if err != nil {
					t.Fatal(err)
				}
				cli.Env = env
				cliText := readText(t, cli.Path(key))
				t.Chdir(executor)
				daemon := New(nil, nil, nil, env, cwd, 1, 2)
				if got := readText(t, daemon.Path(key)); cliText != "right" || got != cliText {
					t.Errorf("%s=%s: CLI read %q from %q, request %q from %q, want right", key, value, cliText, cli.Path(key), got, daemon.Path(key))
				}
				if got := cli.Path(key); got != value {
					t.Errorf("CLI %s=%s: Path %q, want as given", key, value, got)
				}
			}
			for _, env := range [][]string{nil, {string(key) + "="}} {
				for _, request := range []Context{New(nil, nil, nil, env, cwd, 1, 2), {Env: env, Cwd: cwd, process: true}} {
					if got := request.Path(key); got != "" {
						t.Errorf("env %q: Path %q, want empty", env, got)
					}
				}
			}
		})
	}
}

// XDG_CONFIG_HOME: относительное значение — ошибка у CLI и у демона, как у
// os.UserConfigDir; без него — $HOME/.config по Path
func TestConfigDirCLIAndRequest(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	cli, err := FromProcess()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		env        []string
		cli, other string
		fails      bool
	}{
		{env: []string{"XDG_CONFIG_HOME=cfg", "HOME=/h"}, fails: true},
		{env: []string{"XDG_CONFIG_HOME=/abs", "HOME=h"}, cli: "/abs", other: "/abs"},
		{env: []string{"HOME=h"}, cli: "h/.config", other: filepath.Join(cwd, "h", ".config")},
		{env: []string{"XDG_CONFIG_HOME=", "HOME=/h"}, cli: "/h/.config", other: "/h/.config"},
		{env: nil, fails: true},
	} {
		cli.Env = tc.env
		daemon := New(nil, nil, nil, tc.env, cwd, 1, 2)
		cliDir, cliErr := cli.ConfigDir()
		daemonDir, daemonErr := daemon.ConfigDir()
		if tc.fails {
			if cliErr == nil || daemonErr == nil || cliErr.Error() != daemonErr.Error() {
				t.Errorf("env %q: CLI %q %v, request %q %v, want one error", tc.env, cliDir, cliErr, daemonDir, daemonErr)
			}
			continue
		}
		if cliErr != nil || daemonErr != nil || cliDir != tc.cli || daemonDir != tc.other {
			t.Errorf("env %q: CLI %q %v, request %q %v, want %q and %q", tc.env, cliDir, cliErr, daemonDir, daemonErr, tc.cli, tc.other)
		}
	}
}

// RootPath — корень для сравнения: у CLI как есть, у демона относительный
// не берётся, абсолютный — как есть
func TestRootPathCLIAndRequest(t *testing.T) {
	cli := Context{Cwd: "/cwd", process: true}
	for _, tc := range []struct {
		value, cli, daemon string
	}{{"rel", "rel", ""}, {"/abs", "/abs", "/abs"}, {"", "", ""}} {
		env := []string{"TMPDIR=" + tc.value}
		cli.Env = env
		daemon := New(nil, nil, nil, env, "/cwd", 1, 2)
		if got := cli.RootPath(EnvTmpDir); got != tc.cli {
			t.Errorf("CLI %q: %q, want %q", tc.value, got, tc.cli)
		}
		if got := daemon.RootPath(EnvTmpDir); got != tc.daemon {
			t.Errorf("request %q: %q, want %q", tc.value, got, tc.daemon)
		}
	}
}

// TempDir — $TMPDIR по Path, без него /tmp; RuntimeDir без
// $XDG_RUNTIME_DIR — под TempDir
func TestTempDirRequest(t *testing.T) {
	for _, tc := range []struct {
		env              []string
		temp, runtimeDir string
	}{
		{[]string{"TMPDIR=t"}, "/cwd/t", "/cwd/t/chooks"},
		{nil, "/tmp", "/tmp/chooks"},
		{[]string{"TMPDIR=/x", "XDG_RUNTIME_DIR=r"}, "/x", "/cwd/r/chooks"},
	} {
		request := New(nil, nil, nil, tc.env, "/cwd", 1, 2)
		if got := request.TempDir(); got != tc.temp {
			t.Errorf("env %q: TempDir %q, want %q", tc.env, got, tc.temp)
		}
		if got := request.RuntimeDir(); got != tc.runtimeDir {
			t.Errorf("env %q: RuntimeDir %q, want %q", tc.env, got, tc.runtimeDir)
		}
	}
}

// MatchPath — значение как есть у CLI и у демона: сравнение строк с путями
// ввода не зависит от cwd
func TestMatchPathCLIAndRequest(t *testing.T) {
	for _, value := range []string{"home", "/abs/home", ""} {
		env := []string{"HOME=" + value}
		cli := Context{Env: env, Cwd: "/cwd", process: true}
		daemon := New(nil, nil, nil, env, "/cwd", 1, 2)
		if got, want := daemon.MatchPath(EnvHome), cli.MatchPath(EnvHome); got != value || want != value {
			t.Errorf("HOME=%q: request %q, CLI %q, want as given", value, got, want)
		}
	}
}

// ChildDir: у CLI — cwd как есть (ребёнок наследует и удалённый), у демона —
// cwd запроса, а удалённый — "/", чтобы ребёнок стартовал, как у CLI
func TestChildDirDeletedCwd(t *testing.T) {
	cwd := filepath.Join(t.TempDir(), "gone")
	if err := os.Mkdir(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	daemon := New(nil, nil, nil, nil, cwd, 1, 2)
	if got := daemon.ChildDir(); got != cwd {
		t.Errorf("live cwd: %q, want %q", got, cwd)
	}
	if err := os.Remove(cwd); err != nil {
		t.Fatal(err)
	}
	if got := daemon.ChildDir(); got != "/" {
		t.Errorf("deleted cwd: %q, want /", got)
	}
	// На месте каталога файл: каталога cwd тоже нет
	if err := os.WriteFile(cwd, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := daemon.ChildDir(); got != "/" {
		t.Errorf("file in place of cwd: %q, want /", got)
	}
	cli := Context{Cwd: cwd, process: true}
	if got := cli.ChildDir(); got != cwd {
		t.Errorf("CLI: %q, want %q as given", got, cwd)
	}
}
