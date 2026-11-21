// Package tailscan читает хвост большого файла окнами, растущими вдвое:
// транскрипт сессии бывает в сотни МБ и целиком в таймаут хука не
// укладывается, а нужное — последние записи
package tailscan

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

const (
	// Start — первое окно
	Start = 1 << 20
	// Max — предел окна: у work-admin между репликами человека бывают сотни
	// сообщений сессий
	Max = 32 << 20
)

// ErrNotRegular — путь транскрипта ведёт не к обычному файлу (каталог,
// устройство). Своя ошибка, а не ошибка чтения: на tmpfs чтение каталога
// падает EISDIR, на btrfs пустой каталог читается без ошибки
var ErrNotRegular = errors.New("not a regular file")

// Scan отдаёт fn хвосты файла path по возрастанию окна, от Start до Max или
// всего файла, пока fn не скажет, что нужное найдено. Окно не с начала файла
// — без обрезанной первой строки. Не обычный файл — ошибка: у каталога на
// btrfs размер 0, и чтение пустого окна ошибки не дало бы
func Scan(path string, fn func(buf []byte) (done bool)) error {
	return ScanFrom("", path, fn)
}

// ScanFrom — Scan с относительным path от каталога dir (cwd запроса), а не
// от cwd процесса. В текстах ошибок path остаётся таким, как его дали
func ScanFrom(dir, path string, fn func(buf []byte) (done bool)) error {
	open := path
	if dir != "" && !filepath.IsAbs(path) {
		open = filepath.Join(dir, path)
	}
	file, err := os.Open(open)
	if err != nil {
		var pathErr *fs.PathError
		if errors.As(err, &pathErr) {
			pathErr.Path = path
		}
		return fmt.Errorf("open transcript: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat transcript: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("transcript %s (%s): %w", path, info.Mode().Type(), ErrNotRegular)
	}
	size := info.Size()
	for window := int64(Start); ; window *= 2 {
		window = min(window, size)
		buf := make([]byte, window)
		if _, err := file.ReadAt(buf, size-window); err != nil && err != io.EOF {
			return fmt.Errorf("read transcript tail: %w", err)
		}
		if window < size {
			buf = dropPartialLine(buf)
		}
		if fn(buf) || window >= size || window >= Max {
			return nil
		}
	}
}

func dropPartialLine(buf []byte) []byte {
	if _, after, ok := bytes.Cut(buf, []byte{'\n'}); ok {
		return after
	}
	return buf
}
