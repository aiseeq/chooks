// Package atomicfile — запись файла через временный рядом и переименование:
// читатель видит либо прежний файл, либо новый целиком
package atomicfile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// staleAge — временный файл старше этого брошен: писатель погиб между
// созданием и переименованием (строку статуса Claude Code снимает сигналом,
// так за 11.11 в ~/.local/state/chooks набралось 658 `.limits-*.json`)
const staleAge = time.Minute

// Write пишет data в path атомарно
func Write(path string, data []byte, mode os.FileMode) error {
	return write(path, data, mode, false)
}

// WriteSync — как Write, но данные сбрасываются на диск до переименования:
// для файлов, потеря которых при сбое питания необратима (учётные данные)
func WriteSync(path string, data []byte, mode os.FileMode) error {
	return write(path, data, mode, true)
}

func tempPattern(path string) string {
	return "." + filepath.Base(path) + ".tmp-*"
}

func write(path string, data []byte, mode os.FileMode, sync bool) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), tempPattern(path))
	if err != nil {
		return fmt.Errorf("cannot create %s: %w", path, err)
	}
	fail := func(err error) error {
		tmp.Close()
		os.Remove(tmp.Name())
		return fmt.Errorf("cannot write %s: %w", path, err)
	}
	if err := tmp.Chmod(mode); err != nil {
		return fail(err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fail(err)
	}
	if sync {
		if err := tmp.Sync(); err != nil {
			return fail(err)
		}
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("cannot write %s: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("cannot replace %s: %w", path, err)
	}
	return sweep(path, time.Now())
}

// sweep удаляет брошенные временные файлы того же path
func sweep(path string, now time.Time) error {
	stale, err := filepath.Glob(filepath.Join(filepath.Dir(path), tempPattern(path)))
	if err != nil {
		return fmt.Errorf("cannot list temporary files of %s: %w", path, err)
	}
	var errs []error
	for _, name := range stale {
		info, err := os.Stat(name)
		if errors.Is(err, fs.ErrNotExist) {
			continue // соседний писатель как раз переименовал свой
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if now.Sub(info.ModTime()) < staleAge {
			continue
		}
		if err := os.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("cannot remove stale temporary files of %s: %w", path, err)
	}
	return nil
}
