package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// CLI уведомления держит отсоединённый deliver-alert до очистки теста: она
// отпускает писателя и ждёт его выхода раньше, чем TempDir удалит его каталог
func TestShadowNotificationWriterCleanup(t *testing.T) {
	var writerPID int
	t.Run("notification writer is held through the hook", func(t *testing.T) {
		socket, _ := captureFrames(t)
		home := shadowHome(t, true)
		runShadowCLI(t, home, hookNotification, shadowInputs(home)[hookNotification], socket)
		pidLogs := filepath.Join(home, "alert-pid-*")
		if !waitFor(t, 10*time.Second, func() bool {
			logs, _ := filepath.Glob(pidLogs)
			for _, log := range logs {
				writerPID = completeAlertWriterPID(readFile(t, log))
				if writerPID != 0 {
					return true
				}
			}
			return writerPID != 0
		}) {
			t.Fatalf("notification writer was not held; pid logs %q", pidLogs)
		}
	})
	if writerPID == 0 {
		return
	}
	if !waitFor(t, 10*time.Second, func() bool {
		_, err := os.Stat(fmt.Sprintf("/proc/%d", writerPID))
		return os.IsNotExist(err)
	}) {
		t.Errorf("notification writer pid %d survived test cleanup", writerPID)
	}
}

// completeAlertWriterPID — pid из завершённой строкой «pid N\n» записи:
// писатель может дописывать её в момент первого чтения
func completeAlertWriterPID(log string) int {
	lastNewline := strings.LastIndexByte(log, '\n')
	if lastNewline < 0 {
		return 0
	}
	for line := range strings.SplitSeq(log[:lastNewline], "\n") {
		id, ok := strings.CutPrefix(line, "pid ")
		if !ok || id == "" || strings.Trim(id, "0123456789") != "" {
			continue
		}
		pid, err := strconv.Atoi(id)
		if err == nil && pid > 0 {
			return pid
		}
	}
	return 0
}
