package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/aiseeq/chooks/internal/hookctx"
)

// TestMain даёт процессу тестов временный HOME до первого теста. Тесты
// подменяют env процесса частично (t.Setenv XDG_STATE_HOME, XDG_RUNTIME_DIR)
// или env запроса, а HOME процесса оставался настоящим: при сбое подмены
// (мутант m1 ревью 2 — env первого запроса у всех) хуки сессии s1 писали
// skill-reload, night-hint и limits в состояние пользователя. Каталоги XDG и
// CLAUDE_CONFIG_DIR сняты: они выводятся из временного HOME. После тестов
// настоящие каталоги состояния, кэша akit и runtime сверяются со снимком до
// них (realDirsUnchanged)
func TestMain(m *testing.M) {
	os.Exit(runIsolated(m))
}

// testHome — временный HOME процесса тестов; realHome — HOME до подмены
var testHome, realHome string

func runIsolated(m *testing.M) int {
	home, err := os.MkdirTemp("", "chooks-test-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "test home:", err)
		return 1
	}
	defer os.RemoveAll(home)
	testHome, realHome = home, os.Getenv("HOME")
	watched := realDirs()
	before := listDirs(watched)
	for _, name := range []string{"XDG_STATE_HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "CLAUDE_CONFIG_DIR", "MODELS_APPROVED"} {
		if err := os.Unsetenv(name); err != nil {
			fmt.Fprintln(os.Stderr, "unset", name+":", err)
			return 1
		}
	}
	for name, value := range map[string]string{"HOME": home, "XDG_RUNTIME_DIR": filepath.Join(home, "run")} {
		if err := os.Setenv(name, value); err != nil {
			fmt.Fprintln(os.Stderr, "set", name+":", err)
			return 1
		}
	}
	code := m.Run()
	if leaked := newFakeEntries(before, listDirs(watched)); len(leaked) > 0 {
		fmt.Fprintf(os.Stderr, "tests wrote into the real HOME of the process (%s): %s\n", realHome, strings.Join(leaked, ", "))
		return 1
	}
	return code
}

// realDirs — каталоги, куда хук пишет по env процесса до подмены: состояние
// chooks, кэш akit (webtext), runtime chooks
func realDirs() []string {
	var dirs []string
	if state := xdgDir("XDG_STATE_HOME", ".local/state"); state != "" {
		dirs = append(dirs, filepath.Join(state, "chooks"))
	}
	if cache := xdgDir("XDG_CACHE_HOME", ".cache"); cache != "" {
		dirs = append(dirs, filepath.Join(cache, "akit"))
	}
	if runtime := os.Getenv("XDG_RUNTIME_DIR"); runtime != "" {
		dirs = append(dirs, filepath.Join(runtime, "chooks"))
	}
	return dirs
}

// xdgDir — каталог XDG процесса до подмены: переменная или HOME/rel
func xdgDir(name, rel string) string {
	if dir := os.Getenv(name); dir != "" {
		return dir
	}
	if realHome == "" {
		return ""
	}
	return filepath.Join(realHome, rel)
}

// listDirs — записи каталогов на два уровня вглубь (chooks/night-hint/s1.json)
func listDirs(dirs []string) map[string]bool {
	seen := map[string]bool{}
	for _, dir := range dirs {
		top, _ := filepath.Glob(filepath.Join(dir, "*"))
		nested, _ := filepath.Glob(filepath.Join(dir, "*", "*"))
		for _, path := range slices.Concat(top, nested) {
			seen[path] = true
		}
	}
	return seen
}

// liveSession — имя файла живой сессии Claude Code (UUID, у агента —
// UUID.a<hex>) или кэша akit (hex-хэш): их во время тестов пишут настоящие
// сессии пользователя, тесты же берут короткие имена (s1, shared)
var liveSession = regexp.MustCompile(`^([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}|[0-9a-f]{16,})([.-]|$)`)

func newFakeEntries(before, after map[string]bool) []string {
	var leaked []string
	for path := range after {
		if !before[path] && !liveSession.MatchString(filepath.Base(path)) {
			leaked = append(leaked, path)
		}
	}
	slices.Sort(leaked)
	return leaked
}

// Хук запроса процесса пишет по env процесса: каталоги состояния, runtime и
// кэша у тестов — во временном HOME, а не в настоящем
func TestProcessDirsIsolated(t *testing.T) {
	request, err := hookctx.FromProcess()
	if err != nil {
		t.Fatal(err)
	}
	state, err := request.StateDir()
	if err != nil {
		t.Fatal(err)
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	for name, dir := range map[string]string{"state": state, "runtime": request.RuntimeDir(), "cache": cache} {
		if rel, err := filepath.Rel(testHome, dir); err != nil || strings.HasPrefix(rel, "..") {
			t.Errorf("%s dir %s is outside the test HOME %s (real HOME %s)", name, dir, testHome, realHome)
		}
	}
}
