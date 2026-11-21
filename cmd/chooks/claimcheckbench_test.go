package main

import (
	"bytes"
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWilson(t *testing.T) {
	cases := []struct {
		k, n   int
		lo, hi float64
	}{
		{0, 10, 0, 0.2775},
		{8, 10, 0.4902, 0.9433},
		{10, 10, 0.7225, 1},
		{0, 0, 0, 1},
	}
	for _, c := range cases {
		lo, hi := wilson(c.k, c.n)
		if math.Abs(lo-c.lo) > 1e-4 || math.Abs(hi-c.hi) > 1e-4 {
			t.Errorf("wilson(%d, %d) = %.4f–%.4f, want %.4f–%.4f", c.k, c.n, lo, hi, c.lo, c.hi)
		}
	}
}

func TestPercentile(t *testing.T) {
	v := []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20}
	if percentile(v, 0.5) != 10 || percentile(v, 0.95) != 19 || percentile(nil, 0.5) != 0 || percentile(v[:1], 0.95) != 1 {
		t.Fatalf("p50 %d p95 %d", percentile(v, 0.5), percentile(v, 0.95))
	}
}

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestClaimSplit(t *testing.T) {
	dir := t.TempDir()
	split := filepath.Join(dir, "split.tsv")
	writeFile(t, split, "ref\tpart\na:1\tdev\nb:2\thold\nextra:3\thold\n")
	parts, err := readClaimSplit(split)
	if err != nil || len(parts) != 3 || parts["b:2"] != "hold" {
		t.Fatalf("parts %v, %v", parts, err)
	}
	rows, err := assignClaimParts([]claimCase{{Ref: "a:1"}, {Ref: "b:2"}}, parts)
	if err != nil || len(rows) != 2 || rows[0].Part != "dev" || rows[1].Part != "hold" {
		t.Fatalf("rows %+v, %v", rows, err)
	}
	if _, err := assignClaimParts([]claimCase{{Ref: "a:1"}, {Ref: "c:9"}}, parts); err == nil || !strings.Contains(err.Error(), "c:9") {
		t.Fatalf("строка без части — ошибка с её ref, got %v", err)
	}
	for name, text := range map[string]string{
		"чужая часть": "a:1\ttest\n",
		"повтор":      "a:1\tdev\na:1\thold\n",
		"одно поле":   "a:1\n",
	} {
		writeFile(t, split, text)
		if _, err := readClaimSplit(split); err == nil {
			t.Errorf("%s: ждали ошибку", name)
		}
	}
}

// TestClaimBenchStub — стенд на заглушке: TSV по строкам и прогонам, сбой
// разбора — не flag, предфильтр не зовёт судью, сводка по большинству
func TestClaimBenchStub(t *testing.T) {
	dir := t.TempDir()
	inputs, answers := filepath.Join(dir, "inputs"), filepath.Join(dir, "answers")
	for _, d := range []string{inputs, answers} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(dir, "cases.jsonl"), strings.Join([]string{
		`{"ref":"aa:1","class":"unmarked","refuted":true,"text":"в 3.4.2 исправлено"}`,
		`{"ref":"aa:2","class":"ok","refuted":false,"text":"тест прошёл"}`,
		`{"ref":"27d21ddd:3","class":"unmarked","refuted":false,"text":"файл на месте"}`,
		`{"ref":"bb:4","class":"marked","refuted":false,"text":"не проверял"}`,
		`{"ref":"bb:5","class":"ok","refuted":false,"text":"ничего"}`,
	}, "\n")+"\n")
	writeFile(t, filepath.Join(dir, "split.tsv"), "ref\tpart\naa:1\tdev\naa:2\tdev\n27d21ddd:3\thold\nbb:4\thold\nbb:5\tdev\n")
	for ref, answer := range map[string]string{
		"aa:1":       "в 3.4.2 исправлено",
		"aa:2":       "тест прошёл",
		"27d21ddd:3": "файл есть",
		"bb:4":       "не проверял 5 штук",
		"bb:5":       "Хорошо, так и сделаю.",
	} {
		writeFile(t, filepath.Join(inputs, ref+".json"), `{"event":"stop","answer":"`+answer+`"}`)
	}
	writeFile(t, filepath.Join(answers, "aa:1.json"), `{"claims":[{"text":"в 3.4.2 исправлено","verdict":"unchecked"}]}`)
	writeFile(t, filepath.Join(answers, "aa:2.json"), `{"claims":[{"text":"другое утверждение совсем","verdict":"unchecked"},{"text":"тест прошёл","verdict":"checked"}]}`)
	writeFile(t, filepath.Join(answers, "27d21ddd:3.json"), `{"claims":[{"text":"файл на месте","verdict":"wrong-time"`)
	writeFile(t, filepath.Join(answers, "bb:4.json"), `{"claims":[{"text":"не проверял","verdict":"unchecked-marked"}]}`)

	rows, err := loadClaimRows(filepath.Join(dir, "cases.jsonl"), filepath.Join(dir, "split.tsv"), inputs, "all")
	if err != nil || len(rows) != 5 {
		t.Fatalf("rows %d, %v", len(rows), err)
	}
	dev, err := loadClaimRows(filepath.Join(dir, "cases.jsonl"), filepath.Join(dir, "split.tsv"), inputs, "dev")
	if err != nil || len(dev) != 3 {
		t.Fatalf("dev rows %d, %v", len(dev), err)
	}
	bench := claimBench{judge: recordedAsker{dir: answers}, runs: 2, jobs: 3, timeout: time.Second, prefilter: true}
	var out, raw bytes.Buffer
	if err := bench.run(context.Background(), rows, &out, &raw); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 1+5*2 || lines[0] != strings.Join(claimTSVHeader, "\t") {
		t.Fatalf("tsv:\n%s", out.String())
	}
	got := map[string]claimRun{}
	for _, line := range lines[1:] {
		r, err := parseClaimRun(strings.Split(line, "\t"))
		if err != nil {
			t.Fatal(err)
		}
		got[r.Ref] = r
	}
	want := map[string][4]bool{ // flag, hit, сбой, предфильтр
		"aa:1":       {true, true, false, false},
		"aa:2":       {true, false, false, false},
		"27d21ddd:3": {false, false, true, false},
		"bb:4":       {false, false, false, false},
		"bb:5":       {false, false, false, true},
	}
	for ref, w := range want {
		r := got[ref]
		if r.Flag != w[0] || r.Hit != w[1] || (r.Error != "") != w[2] || r.Prefiltered != w[3] || r.Model != "stub" {
			t.Errorf("%s: %+v, want flag/hit/err/prefilter %v", ref, r, w)
		}
	}
	if got["aa:2"].Claims != 2 || got["aa:2"].Class != "ok" || got["aa:1"].Part != "dev" || !got["aa:1"].Refuted {
		t.Errorf("поля строки: %+v %+v", got["aa:2"], got["aa:1"])
	}
	if strings.Count(raw.String(), "\n") != 10 || !strings.Contains(raw.String(), `"prefiltered":true`) {
		t.Errorf("raw:\n%s", raw.String())
	}

	summary := summarizeClaimRuns(parseRunsForTest(t, lines[1:]))
	for _, line := range []string{
		"model stub, part dev: строк 3, вызовов судьи 4, отсеяно предфильтром 2",
		"unmarked опровергнутые     flag 1/1 1.00 [0.21–1.00]; попадание 1/1 1.00 [0.21–1.00]",
		"ok                         flag 1/2 0.50 [0.09–0.91]; попадание 0/2 0.00 [0.00–0.66]",
		"model stub, part hold: строк 2, вызовов судьи 4, отсеяно предфильтром 0",
		"unmarked без опровержения  flag 0/1 0.00 [0.00–0.79]; попадание 0/1 0.00 [0.00–0.79]",
		"сбои: 2 из 4 вызовов",
		"расхождение прогонов по flag: 0/3 0.00",
	} {
		if !strings.Contains(summary, line) {
			t.Errorf("в сводке нет %q:\n%s", line, summary)
		}
	}
	// Без 27d21ddd в отложенной части остаётся только marked
	hold := summary[strings.Index(summary, "part hold"):]
	without := hold[strings.Index(hold, "без 27d21ddd"):]
	if strings.Contains(without, "unmarked") || !strings.Contains(without, "marked ") {
		t.Errorf("без 27d21ddd:\n%s", without)
	}
}

func parseRunsForTest(t *testing.T, lines []string) []claimRun {
	t.Helper()
	var runs []claimRun
	for _, line := range lines {
		r, err := parseClaimRun(strings.Split(line, "\t"))
		if err != nil {
			t.Fatal(err)
		}
		runs = append(runs, r)
	}
	return runs
}

// TestClaimSummaryMajority — итог строки по большинству прогонов, расхождение
// и перцентили
func TestClaimSummaryMajority(t *testing.T) {
	run := func(ref string, n int, flag, hit bool, ms int64) claimRun {
		return claimRun{Ref: ref, Part: "hold", Class: "unmarked", Refuted: true, Model: "luna/medium", Run: n, Flag: flag, Hit: hit, MS: ms, TokensIn: int(ms) * 10, TokensOut: 7}
	}
	runs := []claimRun{
		run("a:1", 1, true, true, 100), run("a:1", 2, true, false, 200), run("a:1", 3, false, false, 300),
		run("b:1", 1, false, false, 400), run("b:1", 2, true, true, 500), run("b:1", 3, false, false, 600),
		run("c:1", 1, true, true, 700), run("c:1", 2, true, true, 800), run("c:1", 3, true, true, 900),
	}
	summary := summarizeClaimRuns(runs)
	for _, line := range []string{
		"unmarked опровергнутые     flag 2/3 0.67 [0.21–0.94]; попадание 1/3 0.33 [0.06–0.79]",
		"расхождение прогонов по flag: 2/3 0.67",
		"сбои: 0 из 9 вызовов",
		"токены вход p50 5000 p95 9000; выход p50 7 p95 7; мс p50 500 p95 900",
	} {
		if !strings.Contains(summary, line) {
			t.Errorf("в сводке нет %q:\n%s", line, summary)
		}
	}
}

func TestClaimRunFields(t *testing.T) {
	r := claimRun{Ref: "a:1", Part: "dev", Class: "ok", Model: "stub", Run: 2, Flag: true, Claims: 3, TokensIn: 10, TokensOut: 4, MS: 55, Error: "bad\tjson\nline"}
	back, err := parseClaimRun(r.fields())
	if err != nil {
		t.Fatal(err)
	}
	r.Error = "bad json line"
	if back != r {
		t.Fatalf("%+v != %+v", back, r)
	}
	if _, err := parseClaimRun([]string{"a:1"}); err == nil {
		t.Fatal("короткая строка — ошибка")
	}
	bad := r.fields()
	bad[6] = "yes"
	if _, err := parseClaimRun(bad); err == nil {
		t.Fatal("флаг не 0/1 — ошибка")
	}
}
