package questioncheck

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aiseeq/akit/proc"
	"github.com/aiseeq/chooks/internal/hookctx"
)

// A local shell stub stands in for the model CLI; no model is invoked.
func TestJudgeRequestEnvironment(t *testing.T) {
	root := t.TempDir()
	bin, work := filepath.Join(root, "bin"), filepath.Join(root, "work")
	for _, dir := range []string{bin, work} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	stub := `#!/bin/sh
printf '{"result":"%s|%s|%s","is_error":false}' "$REQUEST_MARK" "$PWD" "${CLAUDE_PID-unset}"
`
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(stub), 0700); err != nil {
		t.Fatal(err)
	}
	req := hookctx.New(nil, nil, nil, []string{"PATH=" + bin, "REQUEST_MARK=one", "CLAUDE_PID=123"}, root, 456, 123)
	judge := CLIJudge{Name: "opus", Effort: "high", Dir: work, Prompt: "test"}
	got, _, err := judge.Ask(hookctx.WithRequest(context.Background(), req), "test")
	if err != nil || got != strings.Join([]string{"one", work, "unset"}, "|") {
		t.Fatalf("result=%q err=%v", got, err)
	}
}

// codex с ненулевым кодом и отказом хода в JSON stdout: причина отказа — в
// ошибке, по ней хук метит учётку (ревью Terra 5ec62fa, п. 4)
func TestCodexFailedTurnOnNonZeroExit(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	stub := `#!/bin/sh
cat > /dev/null
echo '{"type":"turn.started"}'
echo '{"type":"turn.failed","error":{"message":"Your workspace is out of credits."}}'
echo 'stderr tail' >&2
exit 1
`
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte(stub), 0700); err != nil {
		t.Fatal(err)
	}
	req := hookctx.New(nil, nil, nil, []string{"PATH=" + bin + ":/usr/bin:/bin"}, root, 456, 123)
	judge := CLIJudge{Name: "luna", ID: "gpt-6-luna", Effort: "high", Dir: root, Prompt: "test"}
	_, _, err := judge.Ask(hookctx.WithRequest(context.Background(), req), "test")
	if err == nil || !strings.Contains(err.Error(), "out of credits") || !strings.Contains(err.Error(), "exit status 1") {
		t.Fatalf("err = %v", err)
	}
}

// KillJudgeGroups убивает всю группу идущего судьи, и потомка тоже, и не
// даёт запустить нового (ревью Terra 5ec62fa, п. 1)
func TestKillJudgeGroups(t *testing.T) {
	if err := proc.BecomeChildSubreaper(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		judgeGroups.Lock()
		judgeGroups.terminating = false
		judgeGroups.Unlock()
	})
	root := t.TempDir()
	pidFile := filepath.Join(root, "child")
	cmd := exec.Command("/bin/sh", "-c", `sleep 60 & echo $! > "$1"; wait`, "sh", pidFile)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	done := make(chan error, 1)
	go func() {
		_, err := runTracked(cmd)
		done <- err
	}()
	var child int
	for deadline := time.Now().Add(10 * time.Second); child == 0 && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		raw, _ := os.ReadFile(pidFile)
		child, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
	}
	if child == 0 {
		t.Fatal("judge child not started")
	}
	reaped := false
	t.Cleanup(func() {
		if reaped {
			return
		}
		stat, err := proc.ReadStat("/proc", child)
		if err == nil && stat.State != "Z" && stat.State != "X" {
			_ = proc.SignalDescendant(child, syscall.SIGKILL)
		}
		var status syscall.WaitStatus
		_, _ = syscall.Wait4(child, &status, 0, nil)
	})
	if n := KillJudgeGroups(nil); n != 1 {
		t.Errorf("killed groups %d, want 1", n)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		// При мутанте shell и sleep остаются нашими потомками. Сначала
		// останавливаем sleep по проверенному владению, затем сам cmd.
		_ = proc.SignalDescendant(child, syscall.SIGKILL)
		_ = proc.SignalCmd(cmd, syscall.SIGKILL)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
		t.Fatal("judge not killed")
	}
	// Потомок без wait родителя становится зомби у subreaper: проверяем это,
	// затем забираем его статус, чтобы зомби не жил до конца тестового бинаря.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", child))
		if err != nil || strings.Contains(string(stat), ") Z ") {
			break
		}
		if time.Now().After(deadline) {
			_ = proc.SignalDescendant(child, syscall.SIGKILL)
			t.Fatalf("judge child %d survived", child)
		}
	}
	var status syscall.WaitStatus
	waited, err := syscall.Wait4(child, &status, 0, nil)
	if err != nil && err != syscall.ECHILD || err == nil && waited != child {
		t.Fatalf("reap judge child %d: waited=%d err=%v", child, waited, err)
	}
	reaped = true
	if _, err := proc.ReadStat("/proc", child); !proc.Gone(err) {
		t.Fatalf("judge child %d remains after Wait4: %v", child, err)
	}
	if _, err := runTracked(exec.Command("/bin/true")); err == nil {
		t.Error("judge started after KillJudgeGroups")
	}
}
