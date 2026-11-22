package skillreload

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aiseeq/chooks/internal/hookctx"
)

// Запись с относительным путём скилла: файл открывается по пути, разрешённому
// от cwd запроса, а ${CLAUDE_SKILL_DIR} рендерится по пути из записи, как в
// базе. Неизменённый скилл не даёт уведомления ни у CLI, ни у демона
func TestReviewRelativeSkill(t *testing.T) {
	f := newFixture(t)
	t.Chdir(f.root)
	f.skillDir = filepath.Join("skills", "demo")
	base := time.Now().Add(-time.Hour)
	f.writeSkill("Папка ${CLAUDE_SKILL_DIR}.", base)
	if err := os.WriteFile(f.transcript, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	seed := func(dir string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		hash, err := newDirStore(dir).put("Папка skills/demo.")
		if err != nil {
			t.Fatal(err)
		}
		state := newSessionState()
		state.Transcript = f.transcript
		path := "skills/demo/SKILL.md"
		state.Files[path] = &watched{Kind: kindSkill, Path: path, Dir: "skills/demo", Name: "demo", Base: hash, LoadedAt: base}
		raw, err := json.Marshal(state)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "sess-1.json"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	cli, err := hookctx.FromProcess()
	if err != nil {
		t.Fatal(err)
	}
	seed(f.checker.StateDir)
	f.checker.Abs = cli.Abs
	cliNotices := f.check(Event{})

	f.checker.StateDir = filepath.Join(f.root, "state-daemon")
	seed(f.checker.StateDir)
	request := hookctx.New(nil, nil, nil, []string{"HOME=" + f.root}, f.root, os.Getpid(), os.Getppid())
	f.checker.Abs = request.Abs
	daemonNotices := f.check(Event{})

	for _, n := range daemonNotices {
		t.Logf("daemon diff=%s", n.Diff)
	}
	if len(cliNotices) != 0 || len(daemonNotices) != 0 {
		t.Fatalf("unchanged skill: cli notices=%d daemon notices=%d, want 0 and 0", len(cliNotices), len(daemonNotices))
	}
}

// Демон с cwd, отличным от cwd процесса, открывает относительный путь записи
// от cwd запроса: правка там видна, а одноимённый файл у процесса не мешает
func TestRelativeSkillOpensFromRequestCwd(t *testing.T) {
	f := newFixture(t)
	daemonCwd := t.TempDir()
	t.Chdir(daemonCwd)
	base := time.Now().Add(-time.Hour)
	f.writeSkill("Папка ${CLAUDE_SKILL_DIR}. Правка.", base)
	decoy := filepath.Join(daemonCwd, "skills", "demo")
	if err := os.MkdirAll(decoy, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(decoy, skillFileName), []byte(demoFrontmatter+"Папка ${CLAUDE_SKILL_DIR}.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.transcript, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	hash, err := newDirStore(f.checker.StateDir).put("Папка skills/demo.")
	if err != nil {
		t.Fatal(err)
	}
	state := newSessionState()
	state.Transcript = f.transcript
	path := "skills/demo/SKILL.md"
	state.Files[path] = &watched{Kind: kindSkill, Path: path, Dir: "skills/demo", Name: "demo", Base: hash, LoadedAt: base}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.checker.StateDir, "sess-1.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	request := hookctx.New(nil, nil, nil, []string{"HOME=" + f.root}, f.root, os.Getpid(), os.Getppid())
	f.checker.Abs = request.Abs
	wantDiff(t, f.check(Event{}), []string{"+Папка skills/demo. Правка."}, []string{f.root})
}
