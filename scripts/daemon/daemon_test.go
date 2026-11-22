package main

import (
	"os"
	"strconv"
	"testing"
	"time"
)

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
