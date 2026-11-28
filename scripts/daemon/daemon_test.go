package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/chooks/internal/skillreload"
)

func TestPrepareBinariesCanonicalNames(t *testing.T) {
	inputs := t.TempDir()
	cli := filepath.Join(inputs, "br-chooks")
	proxy := filepath.Join(inputs, "br-hook")
	for path, content := range map[string]string{cli: "cli", proxy: "proxy"} {
		if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	work := t.TempDir()
	gotCLI, gotProxy, err := prepareBinaries(work, cli, proxy)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(gotCLI) != "chooks" || filepath.Base(gotProxy) != "chooks-hook" {
		t.Fatalf("binary names %q, %q", filepath.Base(gotCLI), filepath.Base(gotProxy))
	}
	for path, want := range map[string]string{gotCLI: "cli", gotProxy: "proxy"} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("%s: got %q, want %q", path, got, want)
		}
	}
}

func TestNormalize(t *testing.T) {
	raw := []byte("time=\"2026-11-22 21:00:01\" elapsed_ms=12 pid=4242 other=142420 took=3.1ms\n" +
		"msg=\"hook via CLI fallback\" reason=x\nstages=read=3,run=10 ⏱  12.5 ms\n")
	want := "time=\"<time>\" elapsed_ms=<ms> pid=<hook-pid> other=142420 took=<d>\nstages=read=N,run=N ⏱  <ms> ms\n"
	if got := string(normalize(raw, 4242)); got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

func TestNormalizeSandboxPath(t *testing.T) {
	root := filepath.Join(t.TempDir(), "case-001")
	raw := []byte("path=" + filepath.Join(root, "home", "state") + "\n")
	if got, want := string(normalizeSandbox(raw, os.Getpid(), root)), "path=@SANDBOX@/home/state\n"; got != want {
		t.Fatalf("normalized path = %q, want %q", got, want)
	}
}

func TestInstallFixturesFixesMtimeAcrossMinuteBoundary(t *testing.T) {
	fixtures := map[string]string{"work/skill.md": "skill body"}
	var displayed []string
	for _, instant := range []time.Time{
		time.Date(2026, 11, 27, 12, 33, 59, 900_000_000, time.UTC),
		time.Date(2026, 11, 27, 12, 34, 0, 100_000_000, time.UTC),
	} {
		root := t.TempDir()
		s := sandbox{root: root, home: filepath.Join(root, "home"), work: filepath.Join(root, "work")}
		// Simulate fixture creation on either side of the minute transition.
		if err := os.Chtimes(root, instant, instant); err != nil {
			t.Fatal(err)
		}
		if err := installFixtures(s, fixtures); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(filepath.Join(s.work, "skill.md"))
		if err != nil {
			t.Fatal(err)
		}
		if !info.ModTime().Equal(fixtureMTime) {
			t.Fatalf("fixture mtime = %s, want fixed %s", info.ModTime(), fixtureMTime)
		}
		displayed = append(displayed, skillreload.FormatIn([]skillreload.Notice{{
			Kind: "skill", Name: "demo", LoadedAt: time.Date(2026, 11, 27, 11, 0, 0, 0, time.UTC),
			ChangedAt: info.ModTime(), Diff: "-old\n+new\n",
		}}, time.FixedZone("CET", 3600)))
	}
	if displayed[0] != displayed[1] {
		t.Fatalf("reminder dates differ across minute boundary: %q vs %q", displayed[0], displayed[1])
	}
}

func TestInstallFixturesUsesRequestedName(t *testing.T) {
	root := t.TempDir()
	s := sandbox{root: root, home: filepath.Join(root, "home"), work: filepath.Join(root, "work")}
	if err := installFixtures(s, map[string]string{"work/skill.md": "fixture"}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(s.work, "skill.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "fixture" {
		t.Fatalf("fixture = %q", got)
	}
}

func compareWithScripts(t *testing.T, cliScript, proxyScript string, c eqCase) ([]string, int) {
	t.Helper()
	root := t.TempDir()
	cli, proxy := filepath.Join(root, "cli"), filepath.Join(root, "proxy")
	for path, body := range map[string]string{cli: cliScript, proxy: proxyScript} {
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	fallback := filepath.Join(root, "fallbacks.log")
	if err := os.WriteFile(fallback, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	lines, mismatches, _, _, _, err := compareCases(&equalRun{sandbox: filepath.Join(root, "sandboxes"), fallbackLog: fallback}, cli, proxy, []eqCase{c})
	if err != nil {
		t.Fatal(err)
	}
	return lines, mismatches
}

func TestCompareCasesChecksExpectedBranchLabel(t *testing.T) {
	_, mismatches := compareWithScripts(t, "exit 0", "exit 0", eqCase{name: "wrong branch", suite: "equal", cmd: "understand-check", branch: "judge-deny"})
	if mismatches != 1 {
		t.Fatalf("mismatches = %d, want the incorrect branch label detected", mismatches)
	}
}

func TestCompareCasesUsesSameSandboxRootForBothSides(t *testing.T) {
	root := t.TempDir()
	cli, proxy := filepath.Join(root, "cli"), filepath.Join(root, "proxy")
	trace := filepath.Join(root, "roots.log")
	script := `sandbox=$(basename "$(dirname "$PWD")"); printf '%s\n' "$sandbox" >> "$TRACE"; printf '%s' "$sandbox"`
	for path := range map[string]bool{cli: true, proxy: true} {
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	fallback := filepath.Join(root, "fallbacks.log")
	if err := os.WriteFile(fallback, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []eqCase{
		{name: "first sandbox", suite: "equal", cmd: "test-hook", env: map[string]string{"TRACE": trace}},
		{name: "second sandbox", suite: "equal", cmd: "test-hook", env: map[string]string{"TRACE": trace}},
	}
	_, mismatches, _, _, _, err := compareCases(&equalRun{sandbox: filepath.Join(root, "sandboxes"), fallbackLog: fallback}, cli, proxy, cases)
	if err != nil {
		t.Fatal(err)
	}
	if mismatches != 0 {
		t.Fatalf("same-root CLI/proxy mismatch = %d", mismatches)
	}
	got, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Fields(string(got))
	if len(lines) != 4 || lines[0] != lines[1] || lines[2] != lines[3] || lines[0] == lines[2] {
		t.Fatalf("sandbox roots (CLI, proxy per case) = %q, want equal pairs and distinct cases", lines)
	}
}

func TestCompareCasesWrapsPromptBlockUnderClaude(t *testing.T) {
	script := `if [ "$CLAUDECODE" = 1 ]; then echo 'Запрос не одобрена' >&2; exit 2; fi; exit 0`
	lines, mismatches := compareWithScripts(t, script, script, eqCase{name: "blocked prompt", suite: "equal", cmd: "user-prompt-submit", branch: "block"})
	if mismatches != 0 {
		t.Fatalf("wrapped block mismatch = %d: %q", mismatches, lines)
	}
}

func TestTSVNorm(t *testing.T) {
	got := string(tsvNorm([]byte("2026-11-22T21:00:00\tpre-tool-use\tok\t12\t3\tx\nshort\tline\n")))
	if want := "<time>\tpre-tool-use\tok\tN\tN\tx\nshort\tline"; got != want {
		t.Fatalf("got %q", got)
	}
}

func TestSnapshotKeepsGitIndexAndUnderstandKey(t *testing.T) {
	root := t.TempDir()
	index := filepath.Join(root, "home", ".claude-config.git", "index")
	state := filepath.Join(root, "home", ".local", "state", "chooks", "skill-reload.json")
	understand := filepath.Join(root, "home", ".local", "state", "chooks", "understand-check.jsonl")
	if err := os.MkdirAll(filepath.Dir(index), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(state), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(index, []byte("volatile git index"), 0o600); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(root, "work", "transcript.jsonl")
	if err := os.WriteFile(state, []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(understand, []byte(`{"key":"0123456789abcdef0123456789abcdef","tool":"Write"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := snapshot(root, os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := snapshot[filepath.Join("home", ".claude-config.git", "index")]; !ok || got != "volatile git index" {
		t.Fatalf("git index snapshot = %q, present %v", got, ok)
	}
	if got, want := snapshot[filepath.Join("home", ".local", "state", "chooks", "skill-reload.json")], "@WORK@/transcript.jsonl"; got != want {
		t.Fatalf("transcript path = %q, want %q", got, want)
	}
	if got, want := snapshot[filepath.Join("home", ".local", "state", "chooks", "understand-check.jsonl")], `{"key":"0123456789abcdef0123456789abcdef","tool":"Write"}`; got != want {
		t.Fatalf("understand key = %q, want %q", got, want)
	}
}

func TestInstallTranscriptSubstitutesSandboxMarkers(t *testing.T) {
	s := sandbox{root: t.TempDir(), home: filepath.Join(t.TempDir(), "home"), work: filepath.Join(t.TempDir(), "work")}
	if err := os.MkdirAll(s.work, 0o700); err != nil {
		t.Fatal(err)
	}
	input := []byte(`{"type":"user","isMeta":true,"cwd":"@WORK@","text":"@HOME@"}` + "\n")
	if err := installTranscript(s, input); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(s.work, "transcript.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	want := strings.ReplaceAll(strings.ReplaceAll(string(input), "@HOME@", s.home), "@WORK@", s.work)
	if string(got) != want {
		t.Fatalf("transcript = %q, want %q", got, want)
	}
}

func TestPrepareEqual2InstallsRecordedJudgeStub(t *testing.T) {
	s, err := prepare(filepath.Join(t.TempDir(), "box"), "equal2")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(s.home, "bin", "claude"))
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"equal-stub-deny", `"verdict":"fix"`, `"verdict":"ok"`} {
		if !strings.Contains(string(data), marker) {
			t.Errorf("judge stub missing %q", marker)
		}
	}
	config, err := os.ReadFile(filepath.Join(s.home, ".config", "chooks", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(config), "skill_reload: {enabled: true}") {
		t.Fatal("equal2 config did not enable skill reload")
	}
	if _, err := os.Stat(filepath.Join(s.home, ".claude-config.git")); !os.IsNotExist(err) {
		t.Errorf("unexpected git fixture: %v", err)
	}
}

func TestCorpusCasesLoadsTranscriptSnapshot(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "understand-check"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "transcripts", "understand-check"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "understand-check", "case.json"), []byte(`{"session_id":"s","tool_name":"Write"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	want := []byte(`{"type":"user","isMeta":true}` + "\n")
	if err := os.WriteFile(filepath.Join(root, "transcripts", "understand-check", "case.jsonl"), want, 0o600); err != nil {
		t.Fatal(err)
	}
	cases, err := corpusCases(root, "equal2")
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 1 || string(cases[0].transcript) != string(want) {
		t.Fatalf("corpus snapshot = %q, cases %d; want %q", cases[0].transcript, len(cases), want)
	}
}

func TestRemoveSandbox(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sandbox")
	if err := os.MkdirAll(filepath.Join(root, "home"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "home", "state"), []byte("pending"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeSandbox(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("sandbox still exists after cleanup: %v", err)
	}
}

func TestRemoveSandboxRetriesTransientFailure(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sandbox")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	original := removeSandboxAttempt
	t.Cleanup(func() { removeSandboxAttempt = original })
	calls := 0
	removeSandboxAttempt = func(path string) error {
		calls++
		if calls == 1 {
			return os.ErrPermission
		}
		return original(path)
	}
	if err := removeSandbox(root); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("RemoveAll attempts %d, want 2", calls)
	}
}

func TestStats(t *testing.T) {
	var d []time.Duration
	for i := range 20 {
		d = append(d, time.Duration(20-i)*time.Millisecond)
	}
	if got, want := stats(d), "p50 10.5 ms, p95 19.0 ms, min 1.0, max 20.0"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// Добавочных входов equal2.py — 59, у каждого своё имя
func TestExtraCases(t *testing.T) {
	cases, err := extraCases()
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 59 {
		t.Fatalf("extra cases %d, want 59", len(cases))
	}
	seen := map[string]bool{}
	pids := 0
	for _, c := range cases {
		if c.env["CLAUDE_PID"] == strconv.Itoa(os.Getpid()) {
			pids++
		}
		if seen[c.name] {
			t.Fatalf("duplicate name %q", c.name)
		}
		seen[c.name] = true
	}
	if pids != 2 {
		t.Fatalf("CLAUDE_PID of this process in %d cases, want 2", pids)
	}
}

// Отличие кода, вывода или файла — строка отчёта; лишний файл у одной стороны тоже
func TestDiffObserved(t *testing.T) {
	a := observed{code: 0, stdout: "x", files: map[string]string{"f": "1"}}
	if d := diffObserved(a, a); len(d) != 0 {
		t.Fatalf("equal: %q", d)
	}
	for name, b := range map[string]observed{
		"code":       {code: 1, stdout: "x", files: map[string]string{"f": "1"}},
		"stderr":     {stdout: "x", stderr: "e", files: map[string]string{"f": "1"}},
		"file":       {stdout: "x", files: map[string]string{"f": "2"}},
		"extra file": {stdout: "x", files: map[string]string{"f": "1", "g": ""}},
		"no file":    {stdout: "x", files: map[string]string{}},
	} {
		if d := diffObserved(a, b); len(d) != 1 {
			t.Errorf("%s: %q", name, d)
		}
	}
}

func TestCompareCasesCountsRealDifferences(t *testing.T) {
	root := t.TempDir()
	cli := filepath.Join(root, "cli")
	proxy := filepath.Join(root, "proxy")
	for path, body := range map[string]string{cli: "#!/bin/sh\necho cli\n", proxy: "#!/bin/sh\necho proxy\n"} {
		if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	fallback := filepath.Join(root, "fallbacks.log")
	if err := os.WriteFile(fallback, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r := equalRun{sandbox: filepath.Join(root, "sandbox"), fallbackLog: fallback}
	_, mismatches, _, _, byHook, err := compareCases(&r, cli, proxy, []eqCase{{name: "mismatch", suite: "equal", cmd: "understand-check"}})
	if err != nil {
		t.Fatal(err)
	}
	if mismatches != 1 || byHook["understand-check"].mismatches != 1 {
		t.Fatalf("mismatches=%d stats=%+v, want one counted difference", mismatches, byHook)
	}
}

func TestDecisionBranchCountsOnlyActualPromptBlock(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  observed
		want string
	}{
		{"block", observed{code: 2, stderr: "Запрос не одобрена"}, "block"},
		{"unrelated error", observed{code: 2, stderr: "unexpected failure"}, "skip"},
		{"reminder", observed{stdout: "Напоминание"}, "reminder"},
		{"allowed", observed{code: 0}, "skip"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := decisionBranch("user-prompt-submit", tc.got); got != tc.want {
				t.Fatalf("decisionBranch() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLoadEqualCasesRejectsUnknownSuite(t *testing.T) {
	if _, err := loadEqualCases(t.TempDir(), "p4"); err == nil {
		t.Fatal("unknown suite was accepted")
	}
}

// Последняя строка журнала с пустой подробностью тоже считается
func TestJournalOutcomes(t *testing.T) {
	path := t.TempDir() + "/j.tsv"
	body := "t\tstop\ts\tmismatch\tstderr\nt\tstop\ts\tincomparable\tbusy\nt\tstop\ts\tmatch\t\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	counts, details, err := journalOutcomes(path)
	if err != nil {
		t.Fatal(err)
	}
	if counts["stop"] != [3]int{1, 1, 1} || len(details) != 2 {
		t.Fatalf("counts %v details %q", counts, details)
	}
}

// Относительная символьная ссылка на бинарь: подготовка кладёт сам бинарь,
// а не ссылку, цель которой в каталоге стенда не найдётся
func TestPrepareBinariesRelativeSymlink(t *testing.T) {
	inputs := t.TempDir()
	for name, content := range map[string]string{"real-cli": "cli", "real-hook": "proxy"} {
		if err := os.WriteFile(filepath.Join(inputs, name), []byte(content), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cli := filepath.Join(inputs, "br-chooks")
	proxy := filepath.Join(inputs, "br-hook")
	if err := os.Symlink("real-cli", cli); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real-hook", proxy); err != nil {
		t.Fatal(err)
	}
	gotCLI, gotProxy, err := prepareBinaries(t.TempDir(), cli, proxy)
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{gotCLI: "cli", gotProxy: "proxy"} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("%s: got %q, want %q", path, got, want)
		}
	}
}
