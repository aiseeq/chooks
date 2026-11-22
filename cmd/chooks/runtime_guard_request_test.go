package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aiseeq/chooks/internal/hookctx"
)

// runtimeGuardPID — pid хука в запросе: не pid процесса теста, его предки в
// runtimeProc ведут к claude агента
const runtimeGuardPID = 4242

// fixtureProc — дерево процессов запроса без claude: родитель хука — init, и
// проверка модели пропускает реплику. Считает обращения: тест с выдуманным pid
// требует, что проверка шла по фикстуре, а не по /proc машины, где такой pid
// может быть чужим процессом
type fixtureProc struct{ calls *atomic.Int64 }

func (p fixtureProc) Parent(int) (int, error) { p.calls.Add(1); return 1, nil }
func (p fixtureProc) Exe(int) (string, error) {
	p.calls.Add(1)
	return "", errFixtureProc
}
func (p fixtureProc) Comm(int) (string, error) {
	p.calls.Add(1)
	return "", errFixtureProc
}
func (p fixtureProc) Environ(int) ([]string, error) {
	p.calls.Add(1)
	return nil, errFixtureProc
}
func (p fixtureProc) Cmdline(int) ([]string, error) {
	p.calls.Add(1)
	return nil, errFixtureProc
}
func (p fixtureProc) Cwd(int) (string, error) {
	p.calls.Add(1)
	return "", errFixtureProc
}

var errFixtureProc = errors.New("fixture: no such process data")

// withFixtureProc ставит запросу дерево fixtureProc; счётчик — его обращения
func withFixtureProc(ctx context.Context) (context.Context, *atomic.Int64) {
	calls := new(atomic.Int64)
	return context.WithValue(ctx, runtimeGuardKey{}, runtimeGuardDeps{proc: fixtureProc{calls: calls}}), calls
}

// runtimeGuardRequest — запрос UserPromptSubmit со своими pid, HOME и cwd
func runtimeGuardRequest(t *testing.T, home string, env ...string) hookctx.Context {
	t.Helper()
	env = append([]string{"HOME=" + home, "XDG_RUNTIME_DIR=" + filepath.Join(home, "run")}, env...)
	return hookctx.New(strings.NewReader(""), io.Discard, io.Discard, env, home, runtimeGuardPID, runtimeGuardPID-1)
}

// Проверка модели claude в запросе идёт по pid и env запроса: у процесса
// теста другой pid (его предки claude агента не дают) и другой список
// одобренных моделей, где haiku одобрена
func TestRunUserPromptSubmit_RuntimeGuardUsesRequest(t *testing.T) {
	config, _ := silentConfig(t)
	write := func(path, contents string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	processHome := t.TempDir()
	processApproved := filepath.Join(processHome, "approved")
	write(processApproved, "haiku claude-haiku-4-5\n")
	t.Setenv("HOME", processHome)
	t.Setenv("MODELS_APPROVED", processApproved)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	home := t.TempDir()
	approved := filepath.Join(home, "approved")
	write(approved, "opus claude-opus-5-5\n")
	request := runtimeGuardRequest(t, home, "MODELS_APPROVED="+approved)

	logger, _ := fileLogger(t)
	// судья — настоящий model_guard, подменено только дерево процессов
	ctx := context.WithValue(hookctx.WithRequest(context.Background(), request), runtimeGuardKey{}, runtimeGuardDeps{proc: runtimeProc{hook: runtimeGuardPID}})
	data := []byte(`{"hook_event_name":"UserPromptSubmit","session_id":"request-guard","prompt":"x"}`)
	code, err := runUserPromptSubmit(ctx, hookUserPromptSubmit, data, config, logger, newHookTimerForRequest(hookUserPromptSubmit, request))
	if code != exitBlocked || err == nil || !strings.Contains(err.Error(), `модель "haiku" не одобрена`) {
		t.Fatalf("runUserPromptSubmit = %d, %v; want %d with haiku rejection by request state", code, err, exitBlocked)
	}
}
