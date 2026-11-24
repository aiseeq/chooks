package selflaunch

import (
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/aiseeq/chooks/internal/desktop"
)

// StartSkillReloadCleanup запускает скрытую подкоманду в отдельном процессе.
func StartSkillReloadCleanup(stateDir, textsDir, logFile string, now time.Time) error {
	if testing.Testing() {
		return errors.New("cleanup launch refused in test binary")
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	return desktop.StartDetached(skillReloadCleanupCommand(executable, stateDir, textsDir, logFile, now))
}

func skillReloadCleanupCommand(executable, stateDir, textsDir, logFile string, now time.Time) *exec.Cmd {
	return exec.Command(executable, "skill-reload-cleanup", "--state-dir", stateDir, "--texts-dir", textsDir, "--now", now.Format(time.RFC3339Nano), "--log-file", logFile)
}
