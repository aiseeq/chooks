package tailscan

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Каталог вместо транскрипта — ошибка на любой файловой системе: на btrfs
// размер пустого каталога 0, на tmpfs — 40 (Сон 01.11, обход tests)
func TestScan_DirectoryIsError(t *testing.T) {
	called := false
	err := Scan(t.TempDir(), func([]byte) bool { called = true; return true })
	if !errors.Is(err, ErrNotRegular) || called {
		t.Fatalf("err = %v, called = %v; want ErrNotRegular before reading", err, called)
	}
}

func TestScan_MissingIsNotExist(t *testing.T) {
	err := Scan(filepath.Join(t.TempDir(), "missing.jsonl"), func([]byte) bool { return true })
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v, want os.ErrNotExist", err)
	}
}

// Окно растёт вдвое, пока fn не найдёт своё; первая строка окна не с начала
// файла — целая
func TestScan_GrowsUntilFound(t *testing.T) {
	var b strings.Builder
	b.WriteString("MARK\n")
	line := strings.Repeat("x", 1023) + "\n"
	for b.Len() < 3*Start {
		b.WriteString(line)
	}
	path := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	var windows []int
	err := Scan(path, func(buf []byte) bool {
		windows = append(windows, len(buf))
		if len(buf) > 0 && buf[0] != 'x' && buf[0] != 'M' {
			t.Errorf("window starts mid-line: %q", buf[:10])
		}
		return strings.Contains(string(buf), "MARK")
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(windows) != 3 || windows[2] != b.Len() {
		t.Fatalf("windows = %v, want 3 with the last = whole file %d", windows, b.Len())
	}
}

func TestClip(t *testing.T) {
	short := []byte("a\nb\n")
	if got := Clip(short); string(got) != string(short) {
		t.Fatalf("Clip(short) = %q", got)
	}
	// Срез Max байт с конца приходится на «z»: обрезанная строка снимается
	long := []byte("h\n" + strings.Repeat("yz\n", Max/3+1))
	got := Clip(long)
	if len(got) > Max || got[0] != 'y' {
		t.Fatalf("Clip(long): len %d, first %q", len(got), got[0])
	}
}
