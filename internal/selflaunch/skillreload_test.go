package selflaunch

import (
	"strings"
	"testing"
	"time"
)

func TestStartSkillReloadCleanupRefusesTestBinary(t *testing.T) {
	err := StartSkillReloadCleanup(t.TempDir(), t.TempDir(), time.Now())
	if err == nil || !strings.Contains(err.Error(), "cleanup launch refused in test binary") {
		t.Fatalf("StartSkillReloadCleanup error = %v", err)
	}
}
