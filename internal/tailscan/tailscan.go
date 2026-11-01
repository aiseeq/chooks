// Package tailscan читает хвост большого файла окнами, растущими вдвое:
// транскрипт сессии бывает в сотни МБ и целиком в таймаут хука не
// укладывается, а нужное — последние записи
package tailscan

import (
	"bytes"
	"fmt"
	"io"
	"os"
)

const (
	// Start — первое окно
	Start = 1 << 20
	// Max — предел окна: у work-admin между репликами человека бывают сотни
	// сообщений сессий
	Max = 32 << 20
)

// Scan отдаёт fn хвосты файла path по возрастанию окна, от Start до Max или
// всего файла, пока fn не скажет, что нужное найдено. Окно не с начала файла
// — без обрезанной первой строки. Не обычный файл — ошибка: у каталога на
// btrfs размер 0, и чтение пустого окна ошибки не дало бы
func Scan(path string, fn func(buf []byte) (done bool)) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open transcript: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat transcript: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("transcript %s is not a regular file (%s)", path, info.Mode().Type())
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

// Clip — последние Max байт buf без обрезанной первой строки
func Clip(buf []byte) []byte {
	if len(buf) <= Max {
		return buf
	}
	return dropPartialLine(buf[len(buf)-Max:])
}

func dropPartialLine(buf []byte) []byte {
	if _, after, ok := bytes.Cut(buf, []byte{'\n'}); ok {
		return after
	}
	return buf
}
