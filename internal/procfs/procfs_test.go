package procfs

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// Запись и повтор: ответы (данные, имена, «нет файла» и прочие ошибки)
// повторяются по порядку, путь без записи — ErrNotRecorded и Missing
func TestRecordReplay(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte("1 (x) S 0"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "42"), 0o700); err != nil {
		t.Fatal(err)
	}
	rec := NewRecorder(OS{})
	read := func(fsys FS) []any {
		var got []any
		data, err := fsys.ReadFile(filepath.Join(dir, "stat"))
		got = append(got, string(data), err == nil)
		_, err = fsys.ReadFile(filepath.Join(dir, "gone"))
		got = append(got, errors.Is(err, fs.ErrNotExist))
		names, err := fsys.ReadDir(dir)
		got = append(got, names, err == nil)
		return got
	}
	live := read(rec)
	if live[0] != "1 (x) S 0" || live[2] != true {
		t.Fatalf("live reads %v", live)
	}
	replay := NewReplay(rec.Reads())
	if got := read(replay); !reflect.DeepEqual(got, live) {
		t.Fatalf("replay %v, live %v", got, live)
	}
	if len(replay.Missing()) != 0 {
		t.Fatalf("missing %v", replay.Missing())
	}
	_, err := replay.ReadFile(filepath.Join(dir, "other"))
	if !errors.Is(err, ErrNotRecorded) || errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("unrecorded path: %v", err)
	}
	if len(replay.Missing()) != 1 {
		t.Fatalf("missing %v, want the unrecorded path", replay.Missing())
	}
}
