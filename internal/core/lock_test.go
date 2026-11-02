package core

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Занятая блокировка не держит дольше срока: раньше состояние сессий
// ждало её без срока и могло съесть весь таймаут хука
func TestLockFile_BusyUntilDeadline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	holder, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}

	waiter, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer waiter.Close()
	started := time.Now()
	err = LockFile(waiter, started.Add(150*time.Millisecond))
	waited := time.Since(started)
	if !errors.Is(err, ErrLockBusy) {
		t.Fatalf("ожидалась ErrLockBusy, получено %v", err)
	}
	if waited < 150*time.Millisecond || waited > time.Second {
		t.Errorf("ждали %s, а срок 150ms", waited)
	}

	// Освобождённую блокировку берёт сразу
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if err := LockFile(waiter, time.Now().Add(time.Second)); err != nil {
		t.Fatalf("свободная блокировка: %v", err)
	}
}

func TestSettingsHooks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	settings := `{"hooks":{
		"UserPromptSubmit":[{"hooks":[{"type":"command","command":"$HOME/bin/chooks user-prompt-submit","timeout":3}]}],
		"Stop":[{"hooks":[{"command":"$HOME/bin/chooks stop","timeout":3},{"command":"/x/chooks question-check","timeout":60}]}],
		"PermissionRequest":[{"hooks":[{"command":"chooks no-prompt","timeout":0.5},{"command":"single"}]}]
	}}`
	if err := os.WriteFile(path, []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	hooks, err := SettingsHooks(path)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]SettingsHook{}
	for _, hook := range hooks {
		got[hook.Subcommand] = hook
	}
	if len(hooks) != 4 {
		t.Errorf("команда из одного слова не хук chooks, ожидалось 4 записи: %+v", hooks)
	}
	if h := got["user-prompt-submit"]; h.Event != "UserPromptSubmit" || h.Timeout != 3*time.Second {
		t.Errorf("user-prompt-submit: %+v", h)
	}
	if h := got["question-check"]; h.Event != "Stop" || h.Timeout != time.Minute {
		t.Errorf("question-check: %+v", h)
	}
	if h := got["no-prompt"]; h.Timeout != 500*time.Millisecond {
		t.Errorf("дробный таймаут: %+v", h)
	}

	missing, err := SettingsHooks(filepath.Join(t.TempDir(), "none.json"))
	if err != nil || missing != nil {
		t.Errorf("нет файла — нет хуков без ошибки, получено %v, %v", missing, err)
	}
}
