package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Брошенный временный файл (писатель погиб до переименования) убирает
// следующая запись, свежий чужой — нет
func TestWriteSweepsStaleTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "limits.json")
	stale := filepath.Join(dir, ".limits.json.tmp-111")
	fresh := filepath.Join(dir, ".limits.json.tmp-222")
	other := filepath.Join(dir, ".accounts.json.tmp-333")
	for _, name := range []string{stale, fresh, other} {
		if err := os.WriteFile(name, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * staleAge)
	for _, name := range []string{stale, other} {
		if err := os.Chtimes(name, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if err := Write(path, []byte(`{"a":1}`), 0o640); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != `{"a":1}` {
		t.Fatalf("файл: %q, %v", got, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("права: %v, %v", info, err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("брошенный временный файл остался: %v", err)
	}
	for _, name := range []string{fresh, other} {
		if _, err := os.Stat(name); err != nil {
			t.Errorf("%s удалён: %v", name, err)
		}
	}
}
