package hookctx

import (
	"os"
	"path/filepath"
	"testing"
)

// symlinkTree: root/link → root/other/inside. От root путь link/../config ОС
// разрешает через цель симлинка в root/other/config («right»), а лексически
// очищенный — в root/config («wrong»)
func symlinkTree(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "other", "inside"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "other", "inside"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{"other/config": "right", "config": "wrong"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func readText(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// Abs открывает тот же файл, что ОС от cwd запроса: у CLI путь как есть, у
// запроса демона — от его cwd, в обоих случаях без чистки .. мимо симлинка
func TestAbsOpensWhatOSOpens(t *testing.T) {
	root := symlinkTree(t)
	t.Chdir(root)
	request, err := FromProcess()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"link/../config", root + "/link/../config"} {
		if got := request.Abs(path); got != path {
			t.Errorf("CLI Abs(%q) = %q, want as given", path, got)
		}
		if got := readText(t, request.Abs(path)); got != "right" {
			t.Errorf("CLI %s: %q, want right", path, got)
		}
	}
	// Демон: cwd процесса — другой каталог, где тоже лежит config «wrong»
	elsewhere := t.TempDir()
	t.Chdir(elsewhere)
	if err := os.WriteFile(filepath.Join(elsewhere, "config"), []byte("wrong"), 0o600); err != nil {
		t.Fatal(err)
	}
	daemon := New(nil, nil, nil, nil, root, 1, 2)
	for _, path := range []string{"link/../config", root + "/link/../config"} {
		if got := readText(t, daemon.Abs(path)); got != "right" {
			t.Errorf("daemon %s: %q from %q, want right", path, got, daemon.Abs(path))
		}
	}
}

// cwd запроса — путь через симлинк (так его даёт $PWD), а вызывающий
// склеивает и чистит дальше: filepath.Join(Abs(".."), "config"). ОС из этого
// cwd под .. понимает родителя цели симлинка; так же должен понять и демон
func TestAbsSymlinkCwdThenJoin(t *testing.T) {
	root := symlinkTree(t)
	cwd := filepath.Join(root, "link")
	t.Chdir(cwd)
	process := readText(t, filepath.Join("..", "config"))
	daemon := New(nil, nil, nil, nil, cwd, 1, 2)
	t.Chdir(root)
	got := readText(t, filepath.Join(daemon.Abs(".."), "config"))
	if process != "right" || got != process {
		t.Errorf("daemon read %q, the process from the same cwd %q, want right", got, process)
	}
}

// Каталоги состояния собираются из env как у CLI и открываются от cwd запроса
func TestStateDirsFromRequestCwd(t *testing.T) {
	request, err := FromProcess()
	if err != nil {
		t.Fatal(err)
	}
	request.Env = []string{"XDG_RUNTIME_DIR=rf", "XDG_STATE_HOME=sf"}
	if got := request.RuntimeDir(); got != filepath.Join("rf", "chooks") {
		t.Errorf("process runtime dir = %q", got)
	}
	daemon := New(nil, nil, nil, request.Env, "/request", 1, 2)
	if got, err := daemon.StateDir(); err != nil || got != "/request/sf/chooks" {
		t.Errorf("daemon state dir = %q, %v", got, err)
	}
}
