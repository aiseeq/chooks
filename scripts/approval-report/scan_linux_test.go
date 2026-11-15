//go:build linux

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Время файла через настоящий statx: у нового файла время создания — время записи, и при записи в ту
// же наносекунду; файл с временем записи раньше создания (скопирован с прежним временем) — без
// времени создания. Временный каталог без времени создания (ramfs) — пропуск.
func TestFileTimes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "att1.jsonl")
	before := time.Now().Add(-time.Second)
	writeFile(t, path, "{}\n")
	after := time.Now().Add(time.Second)
	var st unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, path, 0, unix.STATX_BTIME, &st); err != nil {
		t.Fatal(err)
	}
	if st.Mask&unix.STATX_BTIME == 0 {
		t.Skipf("файловая система временного каталога %s не отдаёт время создания", filepath.Dir(path))
	}
	sc := scanner{}
	born, mod, err := sc.fileTimes(path)
	if err != nil {
		t.Fatal(err)
	}
	if born.Before(before) || born.After(after) || mod.Before(before) || mod.After(after) {
		t.Errorf("создание %v, запись %v, ждём между %v и %v", born, mod, before, after)
	}
	if err := os.Chtimes(path, born, born); err != nil {
		t.Fatal(err)
	}
	if b, m, err := sc.fileTimes(path); err != nil || !b.Equal(born) || !m.Equal(born) {
		t.Errorf("запись в миг создания: создание %v, запись %v, ошибка %v; ждём %v", b, m, err, born)
	}
	old := time.Date(2026, 11, 10, 10, 30, 0, 0, time.UTC)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	born, mod, err = sc.fileTimes(path)
	if err != nil || !born.IsZero() || !mod.Equal(old) {
		t.Errorf("копия: создание %v, запись %v, ошибка %v; ждём нулевое создание и запись %v", born, mod, err, old)
	}
}

// Файловая система без времени создания (procfs): создание нулевое, а не 1970 год.
func TestFileTimesNoBtime(t *testing.T) {
	var st unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, "/proc/self/status", 0, unix.STATX_BTIME, &st); err != nil {
		t.Fatal(err)
	}
	if st.Mask&unix.STATX_BTIME != 0 {
		t.Skip("procfs отдаёт время создания")
	}
	if born, _, err := fileTimes("/proc/self/status"); err != nil || !born.IsZero() {
		t.Errorf("procfs: создание %v, ошибка %v; ждём нулевое", born, err)
	}
}
