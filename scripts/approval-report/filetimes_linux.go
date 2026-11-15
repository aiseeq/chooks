//go:build linux

package main

import (
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

// fileTimes — создание и последняя запись файла попытки: между ними — все её вызовы. Файловая
// система без времени создания (нет STATX_BTIME в маске ответа) — создание нулевое, граница ничего
// не отсекает.
func fileTimes(path string) (born, mod time.Time, err error) {
	var st unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, path, 0, unix.STATX_BTIME|unix.STATX_MTIME, &st); err != nil {
		return born, mod, fmt.Errorf("время файла: %w", err)
	}
	if st.Mask&unix.STATX_BTIME != 0 {
		born = time.Unix(st.Btime.Sec, int64(st.Btime.Nsec))
	}
	return born, time.Unix(st.Mtime.Sec, int64(st.Mtime.Nsec)), nil
}
