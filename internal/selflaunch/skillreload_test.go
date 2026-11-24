package selflaunch

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStartSkillReloadCleanupRefusesTestBinary(t *testing.T) {
	err := StartSkillReloadCleanup(t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "chooks.log"), time.Now())
	if err == nil || !strings.Contains(err.Error(), "cleanup launch refused in test binary") {
		t.Fatalf("StartSkillReloadCleanup error = %v", err)
	}
}

func TestSkillReloadCleanupCommandArguments(t *testing.T) {
	now := time.Date(2026, 11, 6, 7, 0, 0, 123456789, time.UTC)
	cmd := skillReloadCleanupCommand("/usr/bin/chooks", "/state", "/texts", "/logs/chooks.log", now)
	want := []string{"/usr/bin/chooks", "skill-reload-cleanup", "--state-dir", "/state", "--texts-dir", "/texts", "--now", now.Format(time.RFC3339Nano), "--log-file", "/logs/chooks.log"}
	if len(cmd.Args) != len(want) {
		t.Fatalf("cleanup command args: %#v, want %#v", cmd.Args, want)
	}
	for i := range want {
		if cmd.Args[i] != want[i] {
			t.Fatalf("cleanup command args: %#v, want %#v", cmd.Args, want)
		}
	}
	if cmd.Path != want[0] {
		t.Fatalf("cleanup executable: %q, want %q", cmd.Path, want[0])
	}
}
