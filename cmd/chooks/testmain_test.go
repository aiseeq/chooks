package main

import (
	"bytes"
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
// CLAUDE_CONFIG_DIR сняты: они выводятся из временного HOME. TMPDIR — тоже
// внутри корня прогона: пути t.TempDir несут его имя. После тестов настоящие
// каталоги состояния, кэша akit и runtime сверяются со снимком до них
// (newFakeEntries)
func TestMain(m *testing.M) {
	for _, arg := range os.Args[1:] {
		if arg == "skill-reload-cleanup" {
			fmt.Fprintln(os.Stderr, "chooks test binary refuses skill-reload-cleanup")
			os.Exit(2)
		}
	}
	// Бинарь тестов как CLI chooks для TestRelativeEnvCLIAndRequest: до
	// подмены HOME и до m.Run, env и cwd — запуска
	if os.Getenv(relativeEnvHelper) == "1" {
		os.Exit(runRelativeEnvCLI())
	}
	// Бинарь тестов как CLI chooks с argv запуска, как прокси хука или как
	// CLI, упавший паникой (serve_test.go)
	if code, ok := runServeHelper(); ok {
		os.Exit(code)
	}
	for _, arg := range os.Args[1:] {
		if !strings.HasPrefix(arg, "-test.") {
			fmt.Fprintf(os.Stderr, "chooks test binary refuses positional argument %q\n", arg)
			os.Exit(2)
		}
	}
	os.Exit(runIsolated(m.Run))
}

// testRoot — корень прогона со случайным именем: в нём временный HOME
// (testHome) и TMPDIR процесса тестов; realHome — HOME до подмены
var testRoot, testHome, realHome string

// runIsolated готовит временный HOME, запускает run (у TestMain — m.Run) и
// сверяет настоящие каталоги со снимком до него: запись тестов туда — код 1
func runIsolated(run func() int) int {
	root, err := os.MkdirTemp("", "chooks-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "test root:", err)
		return 1
	}
	defer os.RemoveAll(root)
	home, tmp := filepath.Join(root, "home"), filepath.Join(root, "tmp")
	for _, dir := range []string{home, tmp} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			fmt.Fprintln(os.Stderr, "test dir:", err)
			return 1
		}
	}
	testRoot, testHome, realHome = root, home, os.Getenv("HOME")
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
	if err := os.Setenv("TMPDIR", tmp); err != nil {
		fmt.Fprintln(os.Stderr, "set TMPDIR:", err)
		return 1
	}
	code := run()
	if leaked := newFakeEntries(before, listDirs(watched), runMarkers(root)); len(leaked) > 0 {
		fmt.Fprintf(os.Stderr, "tests wrote into the real HOME of the process (%s): %s\n", realHome, strings.Join(leaked, ", "))
		return 1
	}
	return code
}

// watchDir — настоящий каталог под сверкой. У hashed имена записей — хэши
// содержимого и у живых сессий, и у тестов: по имени их не различить
type watchDir struct {
	path   string
	depth  int
	hashed bool
}

// realDirs — каталоги, куда хук пишет по env процесса до подмены: состояние
// chooks и runtime chooks (записи сессий на двух уровнях: night-hint/s1.json),
// кэш akit (transcript-replies/<код>/<хэш>.gob — на третьем)
func realDirs() []watchDir {
	var dirs []watchDir
	if state := xdgDir("XDG_STATE_HOME", ".local/state"); state != "" {
		dirs = append(dirs, watchDir{path: filepath.Join(state, "chooks"), depth: 2})
	}
	if cache := xdgDir("XDG_CACHE_HOME", ".cache"); cache != "" {
		dirs = append(dirs, watchDir{path: filepath.Join(cache, "akit"), depth: 3, hashed: true})
	}
	if runtime := os.Getenv("XDG_RUNTIME_DIR"); runtime != "" {
		dirs = append(dirs, watchDir{path: filepath.Join(runtime, "chooks"), depth: 2})
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

// sharedEntries — общие записи состояния и runtime chooks, которые живые хуки
// пишут вне сессии (путь от каталога под сверкой, шаблон filepath.Match). Их
// пишет любой процесс пользователя во время прогона, по имени тест от живого
// хука не отличить: такая запись — тестовая, только если в ней корень прогона
var sharedEntries = []string{
	// statusline всех сессий: снимок лимитов (atomicfile.Write — временный
	// .limits.json.tmp-*), блокировка снимка, журнал шагов недели
	"limits.json", ".limits.json.tmp-*", ".limits.lock", "limits-history.jsonl",
	// statusline: ротация учёток (accounts): хранилище (atomicfile.WriteSync),
	// его блокировка, журнал переключений, отметка запуска ротации
	"accounts.json", ".accounts.json.tmp-*", ".accounts.lock", "account-switches.jsonl", ".rotate-spawn",
	// CLI запасного пути хуков (fallbackwatch): счёт уходов мимо демона, его
	// временный файл и блокировка
	"fallbacks.json", ".fallbacks.json.tmp-*", ".fallbacks.lock",
	// understand-check всех сессий: общий журнал решений
	"understand-check.jsonl",
	// skill-reload: отметка суточной чистки и каталог текстов скиллов
	"skill-reload/.last-cleanup", "skill-reload/texts",
	// каталоги записей сессий: живой хук создаёт их при первой записи
	"night-hint", "skill-reload", "understand-check", "sessions", "question-judge",
}

// sharedEntry — общая ли запись rel (путь от каталога под сверкой)
func sharedEntry(rel string) bool {
	return slices.ContainsFunc(sharedEntries, func(pattern string) bool {
		ok, err := filepath.Match(pattern, rel)
		return err == nil && ok
	})
}

// listDirs — записи каталогов до их глубины; значение — запись судится только
// по содержимому: каталог hashed или общая запись живых хуков (sharedEntries)
func listDirs(dirs []watchDir) map[string]bool {
	seen := map[string]bool{}
	for _, dir := range dirs {
		pattern := dir.path
		for range dir.depth {
			pattern = filepath.Join(pattern, "*")
			found, _ := filepath.Glob(pattern)
			for _, path := range found {
				rel, err := filepath.Rel(dir.path, path)
				seen[path] = dir.hashed || err == nil && sharedEntry(rel)
			}
		}
	}
	return seen
}

// liveSession — имя записи живой сессии Claude Code: UUID ровно по длине и
// дефисам 8-4-4-4-12, дальше только суффиксы через точку (UUID.json,
// UUID.<агент>.json, UUID.tasks). Тесты пакета берут не-UUID имена (s1,
// shared, sess-1): их запись в настоящий HOME под шаблон не подходит
var liveSession = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}(\.[0-9a-z]+)*$`)

// runMarkers — имена корня прогона как есть и без симлинков: случайный
// корень есть в путях t.TempDir и временного HOME, живая сессия его не знает
func runMarkers(root string) [][]byte {
	markers := [][]byte{[]byte(root)}
	if real, err := filepath.EvalSymlinks(root); err == nil && real != root {
		markers = append(markers, []byte(real))
	}
	return markers
}

// newFakeEntries — новые записи, которые оставили тесты. Имя не по шаблону
// живой сессии — тестовое. Запись с именем живой сессии, хэш кэша akit и общая
// запись живых хуков (sharedEntries) — тестовые, если в файле есть корень
// прогона: путь транскрипта t.TempDir в записи skill-reload или в кэше
// транскрипта akit
func newFakeEntries(before, after map[string]bool, markers [][]byte) []string {
	var leaked []string
	for path, byContent := range after {
		if _, existed := before[path]; existed {
			continue
		}
		if !byContent && !liveSession.MatchString(filepath.Base(path)) || mentions(path, markers) {
			leaked = append(leaked, path)
		}
	}
	slices.Sort(leaked)
	return leaked
}

// mentions — есть ли в обычном файле один из markers
func mentions(path string, markers [][]byte) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return slices.ContainsFunc(markers, func(marker []byte) bool { return bytes.Contains(data, marker) })
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

// Сверка ловит всё, кроме живых UUID-записей и общих записей живых хуков
// (снимок лимитов и его временный файл, каталоги сессий) без корня прогона
// внутри: hex-имя без дефисов (1234567890abcdef.json) и limits.json.bak —
// тестовые
func TestFakeEntriesFilter(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "run-root")
	dirs := []watchDir{{path: filepath.Join(root, "state"), depth: 2}, {path: filepath.Join(root, "akit"), depth: 3, hashed: true}}
	before := listDirs(dirs)
	uuid := "12345678-1234-1234-1234-123456789abc"
	files := map[string]string{
		"state/night-hint/s1.json":                      "{}",
		"state/night-hint/1234567890abcdef.json":        "{}",
		"state/night-hint/" + uuid + "x.json":           "{}",
		"state/night-hint/" + uuid + ".json":            "{}",
		"state/skill-reload/" + uuid + ".a1b2.json":     "{}",
		"state/skill-reload/" + uuid + ".tasks":         `{"transcript":"` + marker + `/t.jsonl"}`,
		"akit/webtext/1234567890abcdef.txt":             "page",
		"akit/transcript-replies/code/" + uuid + ".gob": "x" + marker + "/t.jsonl",
		"state/limits.json":                             "{}",
		"state/.limits.json.tmp-3795369489":             "{}",
		"state/.accounts.json.tmp-1":                    "{}",
		"state/skill-reload/.last-cleanup":              "",
		"state/limits-history.jsonl":                    `{"cwd":"` + marker + `"}`,
		"state/limits.json.bak":                         "{}",
	}
	for rel, text := range files {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	for _, path := range newFakeEntries(before, listDirs(dirs), runMarkers(marker)) {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, rel)
	}
	want := []string{
		"akit/transcript-replies/code/" + uuid + ".gob",
		"state/limits-history.jsonl",
		"state/limits.json.bak",
		"state/night-hint/" + uuid + "x.json",
		"state/night-hint/1234567890abcdef.json",
		"state/night-hint/s1.json",
		"state/skill-reload/" + uuid + ".tasks",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("reported %q, want %q", got, want)
	}
}

// Сверка срабатывает сквозь runIsolated: фиктивная запись прогона в
// «настоящее» состояние даёт код 1, живая UUID-запись и чистый прогон — код
// прогона. «Настоящий» HOME здесь — песочница теста: TestMain уже подменил
// HOME процесса, а t.Setenv вернёт env и после runIsolated
func TestRunIsolatedReportsLeak(t *testing.T) {
	savedRoot, savedHome, savedReal := testRoot, testHome, realHome
	t.Cleanup(func() { testRoot, testHome, realHome = savedRoot, savedHome, savedReal })
	for _, name := range []string{"HOME", "XDG_RUNTIME_DIR", "TMPDIR", "XDG_STATE_HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "CLAUDE_CONFIG_DIR", "MODELS_APPROVED"} {
		t.Setenv(name, os.Getenv(name))
	}
	uuid := "12345678-1234-1234-1234-123456789abc"
	cases := []struct {
		name, file string
		code, want int
		liveWriter bool
	}{
		{"fake session", "night-hint/s1.json", 0, 1, false},
		{"fake hex name", "night-hint/1234567890abcdef.json", 0, 1, false},
		{"live session", "night-hint/" + uuid + ".json", 0, 0, false},
		{"clean run keeps its code", "", 3, 3, false},
		// statusline живой сессии пишет снимок лимитов во время прогона
		{"live limits writer", "", 0, 0, true},
		{"live limits writer and fake session", "night-hint/s1.json", 0, 1, true},
	}
	for _, tc := range cases {
		sandbox := t.TempDir()
		state := filepath.Join(sandbox, ".local", "state", "chooks")
		if err := os.MkdirAll(filepath.Join(state, "night-hint"), 0o700); err != nil {
			t.Fatal(err)
		}
		for name, value := range map[string]string{"HOME": sandbox, "XDG_RUNTIME_DIR": filepath.Join(sandbox, "run"), "TMPDIR": sandbox} {
			if err := os.Setenv(name, value); err != nil {
				t.Fatal(err)
			}
		}
		stop, done := make(chan struct{}), make(chan error, 1)
		got := runIsolated(func() int {
			if tc.liveWriter {
				started := make(chan struct{})
				go func() { done <- liveLimitsWriter(state, started, stop) }()
				<-started
			}
			if tc.file != "" {
				if err := os.WriteFile(filepath.Join(state, tc.file), []byte("{}"), 0o600); err != nil {
					t.Error(err)
				}
			}
			return tc.code
		})
		close(stop)
		if tc.liveWriter {
			if err := <-done; err != nil {
				t.Error(err)
			}
		}
		if got != tc.want {
			t.Errorf("%s: runIsolated = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// liveLimitsWriter пишет снимок лимитов, как statusline живой сессии
// (atomicfile.Write): limits.json и временный .limits.json.tmp-*, который
// держит до stop — сверка runIsolated видит оба. started — оба на диске
func liveLimitsWriter(state string, started chan<- struct{}, stop <-chan struct{}) error {
	if err := os.WriteFile(filepath.Join(state, "limits.json"), []byte(`{"accounts":{}}`), 0o644); err != nil {
		close(started)
		return fmt.Errorf("live writer: limits.json: %w", err)
	}
	tmp, err := os.CreateTemp(state, ".limits.json.tmp-*")
	close(started)
	if err != nil {
		return fmt.Errorf("live writer: temporary snapshot: %w", err)
	}
	<-stop
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("live writer: close %s: %w", tmp.Name(), err)
	}
	if err := os.Rename(tmp.Name(), filepath.Join(state, "limits.json")); err != nil {
		return fmt.Errorf("live writer: replace limits.json: %w", err)
	}
	return nil
}
