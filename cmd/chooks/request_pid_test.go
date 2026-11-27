package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/aiseeq/akit/proc"
	"github.com/aiseeq/chooks/internal/core"
	"github.com/aiseeq/chooks/internal/hookctx"
)

// processTree запускает оболочку с аргументами args и её дочерний sleep:
// настоящее дерево процессов запроса. Возвращает PID оболочки и sleep
func processTree(t *testing.T, args ...string) (shell, child int) {
	t.Helper()
	cmd := exec.Command("/bin/sh", append([]string{"-c", "sleep 120 & echo $!; wait"}, args...)...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Оболочка и её sleep останавливаются вместе
		if _, err := proc.StopChild(cmd.Process.Pid, 0, func() error { return cmd.Process.Kill() }); err != nil {
			t.Errorf("процесс %d не остановлен: %v", cmd.Process.Pid, err)
		}
		_ = cmd.Wait()
	})
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	child, err = strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid, child
}

// kill guard решает по дереву процессов от PID запроса: оболочка над своим
// процессом хука — предок сессии, оболочка другого запроса — нет. Запросы
// идут вместе, у каждого свой PID
func TestPreToolKillGuardRequestTreeParallel(t *testing.T) {
	var shells, children [2]int
	for i := range shells {
		shells[i], children[i] = processTree(t, "tree")
	}
	root := t.TempDir()
	config := filepath.Join(root, "config.yaml")
	text := "logger:\n  level: error\n  output: stderr\ntools:\n  kill_guard: {enabled: true}\n  wide_search_hint: {enabled: false}\n"
	if err := os.WriteFile(config, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	const count = 40
	type outcome struct {
		code        int
		out, stderr string
	}
	var got [count]outcome
	var wg sync.WaitGroup
	for i := range got {
		wg.Go(func() {
			own, target := i%2, i%2
			if (i/2)%2 == 1 {
				target = 1 - own
			}
			dir := filepath.Join(root, fmt.Sprint(i))
			data, err := json.Marshal(map[string]any{"tool_name": "Bash", "cwd": dir, "tool_input": map[string]any{"command": fmt.Sprintf("kill %d", shells[target])}})
			if err != nil {
				got[i] = outcome{code: -1, stderr: err.Error()}
				return
			}
			var out, stderr bytes.Buffer
			env := []string{"HOME=" + dir, "XDG_RUNTIME_DIR=" + filepath.Join(dir, "run"), "CLAUDE_CONFIG_DIR=" + filepath.Join(dir, "claude")}
			req := hookctx.New(bytes.NewReader(data), &out, &stderr, env, dir, children[own], shells[own])
			req.ConfigPath = config
			got[i] = outcome{code: executeHook(context.Background(), req, hookPreToolUse), out: out.String(), stderr: stderr.String()}
		})
	}
	wg.Wait()
	for i, r := range got {
		if (i/2)%2 == 0 {
			if r.code != exitBlocked || !strings.Contains(r.stderr, "предок этой сессии") {
				t.Errorf("request %d: kill of its own shell %d: %+v", i, shells[i%2], r)
			}
			continue
		}
		if r.code != exitAllowed || r.out != "" || r.stderr != "" {
			t.Errorf("request %d: kill of another request's shell %d: %+v", i, shells[1-i%2], r)
		}
	}
}

// Режим claude -p берётся из командной строки процесса CLAUDE_PID запроса:
// остановку неинтерактивной сессии человек не ждёт
func TestSessionEventRequestClaudePIDParallel(t *testing.T) {
	printShell, _ := processTree(t, "claude", "-p")
	chatShell, _ := processTree(t, "claude")
	config := &core.Config{Tools: map[string]core.ToolConfig{"notifier": {Enabled: false}}}
	root := t.TempDir()
	const count = 20
	var got [count]string
	var wg sync.WaitGroup
	for i := range got {
		wg.Go(func() {
			pid := chatShell
			if i%2 == 0 {
				pid = printShell
			}
			dir := filepath.Join(root, fmt.Sprint(i))
			req := hookctx.New(nil, nil, nil, []string{"HOME=" + dir, "XDG_RUNTIME_DIR=" + dir, fmt.Sprintf("CLAUDE_PID=%d", pid)}, dir, 100+i, 50+i)
			decision := announceSessionEvent(context.Background(), core.EventStop, &core.ToolInput{SessionID: "s"}, config, &captureLogger{}, &req)
			got[i] = fmt.Sprintf("state=%q reason=%q", decision.State, decision.Reason)
		})
	}
	wg.Wait()
	muted := fmt.Sprintf("state=%q reason=%q", "", "неинтерактивная сессия claude -p")
	for i, r := range got {
		if i%2 == 0 && r != muted {
			t.Errorf("request %d (claude -p): %s, want %s", i, r, muted)
		}
		if i%2 == 1 && (r == muted || !strings.HasPrefix(r, fmt.Sprintf("state=%q", core.StateDone))) {
			t.Errorf("request %d (interactive): %s", i, r)
		}
	}
}
