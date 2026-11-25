package main

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
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

func TestTSVNorm(t *testing.T) {
	got := string(tsvNorm([]byte("2026-11-22T21:00:00\tpre-tool-use\tok\t12\t3\tx\nshort\tline\n")))
	if want := "<time>\tpre-tool-use\tok\tN\tN\tx\nshort\tline"; got != want {
		t.Fatalf("got %q", got)
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
