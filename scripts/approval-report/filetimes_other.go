//go:build !linux

package main

import (
	"fmt"
	"os"
	"time"
)

// fileTimes — последняя запись файла попытки; времени создания здесь нет — нулевое.
func fileTimes(path string) (born, mod time.Time, err error) {
	st, err := os.Stat(path)
	if err != nil {
		return born, mod, fmt.Errorf("время файла: %w", err)
	}
	return born, st.ModTime(), nil
}
