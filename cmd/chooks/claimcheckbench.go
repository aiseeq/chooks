package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/aiseeq/chooks/internal/claimcheck"
	"github.com/aiseeq/chooks/internal/questioncheck"
)

// claimCase — строка набора с метками (cases-v4.jsonl): судье из неё не идёт
// ничего, метки нужны только для счёта
type claimCase struct {
	Ref     string `json:"ref"`
	Class   string `json:"class"`
	Refuted bool   `json:"refuted"`
	Text    string `json:"text"`
}

// claimRow — строка прогона: метки, часть разбиения и вход судьи
type claimRow struct {
	claimCase
	Part   string
	Input  string
	Answer string
}

// claimRun — итог строки в одном прогоне; одна строка TSV
type claimRun struct {
	Ref         string
	Part        string
	Class       string
	Refuted     bool
	Model       string
	Run         int
	Flag        bool
	Hit         bool
	Claims      int
	TokensIn    int
	TokensOut   int
	MS          int64
	Error       string
	Prefiltered bool
}

// claimRaw — сырой ответ судьи для разбора глазами
type claimRaw struct {
	Ref         string              `json:"ref"`
	Part        string              `json:"part"`
	Model       string              `json:"model"`
	Run         int                 `json:"run"`
	Answer      string              `json:"answer"`
	Error       string              `json:"error,omitempty"`
	Usage       questioncheck.Usage `json:"usage"`
	MS          int64               `json:"ms"`
	Prefiltered bool                `json:"prefiltered,omitempty"`
}

// claimAsker — судья стенда: модель через CLI или заглушка с записанными ответами
type claimAsker interface {
	ask(ctx context.Context, ref, input string) (string, questioncheck.Usage, error)
	model() string
}

type cliAsker struct{ judge questioncheck.CLIJudge }

func (a cliAsker) ask(ctx context.Context, _, input string) (string, questioncheck.Usage, error) {
	return a.judge.Ask(ctx, input)
}
func (a cliAsker) model() string { return a.judge.Model() }

// recordedAsker отдаёт ответ из файла <ref>.json каталога (--judge-stub):
// мера стенда без модели; модель в TSV — stub
type recordedAsker struct{ dir string }

func (a recordedAsker) ask(_ context.Context, ref, _ string) (string, questioncheck.Usage, error) {
	data, err := os.ReadFile(filepath.Join(a.dir, ref+".json"))
	if err != nil {
		return "", questioncheck.Usage{}, fmt.Errorf("recorded answer: %w", err)
	}
	return string(data), questioncheck.Usage{}, nil
}
func (a recordedAsker) model() string { return "stub" }

var claimTSVHeader = []string{"ref", "part", "class", "refuted", "model", "run", "flag", "hit", "claims", "tokens_in", "tokens_out", "ms", "error", "prefiltered"}

// claimBench — настройки прогона
type claimBench struct {
	judge     claimAsker
	runs      int
	jobs      int
	timeout   time.Duration
	prefilter bool
}

func newClaimCheckBenchCmd() *cobra.Command {
	var inputs, cases, split, part, model, effort, promptPath, recorded, outPath, rawPath string
	var summary bool
	bench := claimBench{}
	cmd := &cobra.Command{
		Use:   "claim-check-bench --inputs <каталог> --cases <cases.jsonl> --split <split.tsv> [--model luna] [--prompt <файл>] | --summary <прогон.tsv>...",
		Short: "Прогнать судью claim-check по набору с метками или свести прогон",
		Long: `Для каждой строки набора нужной части (--part) отдаёт судье вход <ref>.json
из --inputs (от chooks claim-check-input) с промптом --prompt и пишет TSV: строка
на строку набора и прогон. Итог строки — flag, если судья дал unchecked или
wrong-time; попадание — flag на утверждение, совпадающее с text строки. Ответ,
который не разобрался, — сбой, не flag. --raw — сырые ответы в JSONL.
--judge-stub <каталог> — вместо модели ответы из файлов <ref>.json.
--prefilter — строку без признаков факта судья не видит, итог — не flag.
--summary — свести готовые TSV: доли flag и попадания с интервалом Вильсона по
части и классу, расхождение прогонов, сбои, токены и мс. В хук не встроено.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if summary {
				if len(args) == 0 {
					return fmt.Errorf("--summary needs TSV files")
				}
				return summarizeClaimFiles(args, cmd.OutOrStdout())
			}
			if len(args) > 0 {
				return fmt.Errorf("unexpected arguments: %v", args)
			}
			if inputs == "" || cases == "" || split == "" {
				return fmt.Errorf("--inputs, --cases and --split are required")
			}
			if part != "dev" && part != "hold" && part != "all" {
				return fmt.Errorf("--part must be dev, hold or all, got %q", part)
			}
			if recorded != "" {
				bench.judge = recordedAsker{dir: recorded}
			} else {
				if promptPath == "" {
					return fmt.Errorf("--prompt is required without --judge-stub")
				}
				prompt, err := os.ReadFile(promptPath)
				if err != nil {
					return fmt.Errorf("read prompt: %w", err)
				}
				judge, err := newJudge(model, effort)
				if err != nil {
					return err
				}
				judge.Prompt = string(prompt)
				bench.judge = cliAsker{judge: judge}
			}
			rows, err := loadClaimRows(cases, split, inputs, part)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if outPath != "" {
				file, err := os.Create(outPath)
				if err != nil {
					return err
				}
				defer file.Close()
				out = file
			}
			var raw io.Writer = io.Discard
			if rawPath != "" {
				file, err := os.Create(rawPath)
				if err != nil {
					return err
				}
				defer file.Close()
				raw = file
			}
			return bench.run(cmd.Context(), rows, out, raw)
		},
	}
	cmd.Flags().StringVar(&inputs, "inputs", "", "каталог входов судьи <ref>.json")
	cmd.Flags().StringVar(&cases, "cases", "", "набор с метками (JSONL: ref, class, refuted, text)")
	cmd.Flags().StringVar(&split, "split", "", "разбиение TSV: ref, часть (dev или hold)")
	cmd.Flags().StringVar(&part, "part", "dev", "какую часть гнать: dev, hold, all")
	cmd.Flags().StringVar(&model, "model", "luna", "модель судьи из ~/.config/models/approved")
	cmd.Flags().StringVar(&effort, "effort", "medium", "усилие рассуждения: low, medium, high")
	cmd.Flags().StringVar(&promptPath, "prompt", "", "файл промпта судьи")
	cmd.Flags().IntVar(&bench.runs, "runs", 1, "сколько прогонов на строку")
	cmd.Flags().IntVar(&bench.jobs, "jobs", 4, "сколько судей звать одновременно")
	cmd.Flags().DurationVar(&bench.timeout, "timeout", 3*time.Minute, "предел на один вызов судьи")
	cmd.Flags().BoolVar(&bench.prefilter, "prefilter", false, "строки без признаков факта не отдавать судье")
	cmd.Flags().StringVar(&recorded, "judge-stub", "", "каталог записанных ответов <ref>.json вместо модели")
	cmd.Flags().StringVar(&outPath, "out", "", "файл TSV (по умолчанию stdout)")
	cmd.Flags().StringVar(&rawPath, "raw", "", "файл JSONL сырых ответов судьи")
	cmd.Flags().BoolVar(&summary, "summary", false, "свести готовые TSV прогонов из аргументов")
	return cmd
}

// readClaimSplit читает разбиение: ref и часть dev или hold; строка заголовка
// с ref пропускается
func readClaimSplit(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open split: %w", err)
	}
	defer file.Close()
	parts := map[string]string{}
	scanner := bufio.NewScanner(file)
	for n := 1; scanner.Scan(); n++ {
		line := strings.TrimRight(scanner.Text(), "\r")
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 2 {
			return nil, fmt.Errorf("split line %d: want 2 fields, got %d", n, len(fields))
		}
		if n == 1 && fields[0] == "ref" {
			continue
		}
		if fields[1] != "dev" && fields[1] != "hold" {
			return nil, fmt.Errorf("split line %d: part %q is not dev or hold", n, fields[1])
		}
		if _, dup := parts[fields[0]]; dup {
			return nil, fmt.Errorf("split line %d: ref %s repeats", n, fields[0])
		}
		parts[fields[0]] = fields[1]
	}
	return parts, scanner.Err()
}

func readClaimCases(path string) ([]claimCase, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open cases: %w", err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1<<20), 16<<20)
	var cases []claimCase
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		var c claimCase
		if err := json.Unmarshal(scanner.Bytes(), &c); err != nil {
			return nil, fmt.Errorf("parse case: %w", err)
		}
		if c.Ref == "" {
			return nil, fmt.Errorf("case without ref: %s", questioncheck.Excerpt(scanner.Text(), 100))
		}
		cases = append(cases, c)
	}
	return cases, scanner.Err()
}

// assignClaimParts — строки набора с частью; строка без части — ошибка:
// иначе она молча выпала бы из обеих частей
func assignClaimParts(cases []claimCase, parts map[string]string) ([]claimRow, error) {
	var rows []claimRow
	var missing []string
	for _, c := range cases {
		part, ok := parts[c.Ref]
		if !ok {
			missing = append(missing, c.Ref)
			continue
		}
		rows = append(rows, claimRow{claimCase: c, Part: part})
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%d cases have no part in split: %s", len(missing), strings.Join(missing, ", "))
	}
	return rows, nil
}

func loadClaimRows(casesPath, splitPath, inputs, part string) ([]claimRow, error) {
	cases, err := readClaimCases(casesPath)
	if err != nil {
		return nil, err
	}
	parts, err := readClaimSplit(splitPath)
	if err != nil {
		return nil, err
	}
	all, err := assignClaimParts(cases, parts)
	if err != nil {
		return nil, err
	}
	var rows []claimRow
	for _, row := range all {
		if part != "all" && row.Part != part {
			continue
		}
		data, err := os.ReadFile(filepath.Join(inputs, row.Ref+".json"))
		if err != nil {
			return nil, fmt.Errorf("judge input: %w", err)
		}
		var input claimcheck.Input
		if err := json.Unmarshal(data, &input); err != nil {
			return nil, fmt.Errorf("judge input %s: %w", row.Ref, err)
		}
		row.Input, row.Answer = string(data), input.Answer
		rows = append(rows, row)
	}
	return rows, nil
}

func (b claimBench) run(ctx context.Context, rows []claimRow, out, raw io.Writer) error {
	tsv := bufio.NewWriter(out)
	defer tsv.Flush()
	if _, err := fmt.Fprintln(tsv, strings.Join(claimTSVHeader, "\t")); err != nil {
		return fmt.Errorf("write TSV header: %w", err)
	}
	rawEnc := json.NewEncoder(raw)
	rawEnc.SetEscapeHTML(false)
	type job struct {
		row claimRow
		run int
	}
	var mu sync.Mutex
	var firstErr error
	queue := make(chan job)
	var wg sync.WaitGroup
	for range max(b.jobs, 1) {
		wg.Go(func() {
			for j := range queue {
				result, rawAnswer := b.one(ctx, j.row, j.run)
				mu.Lock()
				_, err := fmt.Fprintln(tsv, strings.Join(result.fields(), "\t"))
				if err == nil {
					err = tsv.Flush()
				}
				if err == nil {
					err = rawEnc.Encode(rawAnswer)
				}
				if err != nil && firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
			}
		})
	}
	for run := 1; run <= max(b.runs, 1); run++ {
		for _, row := range rows {
			queue <- job{row: row, run: run}
		}
	}
	close(queue)
	wg.Wait()
	return firstErr
}

// one — итог строки в одном прогоне. Сбой судьи и разбора — в поле Error, не flag
func (b claimBench) one(ctx context.Context, row claimRow, run int) (claimRun, claimRaw) {
	result := claimRun{Ref: row.Ref, Part: row.Part, Class: row.Class, Refuted: row.Refuted, Model: b.judge.model(), Run: run}
	raw := claimRaw{Ref: row.Ref, Part: row.Part, Model: result.Model, Run: run}
	if b.prefilter && !claimcheck.HasFactSignal(row.Answer) {
		result.Prefiltered, raw.Prefiltered = true, true
		return result, raw
	}
	judgeCtx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	started := time.Now()
	answer, usage, err := b.judge.ask(judgeCtx, row.Ref, row.Input)
	result.MS = time.Since(started).Milliseconds()
	result.TokensIn = usage.InputTokens + usage.CachedInputTokens + usage.CacheWriteTokens
	result.TokensOut = usage.OutputTokens
	raw.Answer, raw.Usage, raw.MS = answer, usage, result.MS
	var verdict claimcheck.Verdict
	if err == nil {
		verdict, err = claimcheck.ParseVerdict(answer)
	}
	if err != nil {
		result.Error, raw.Error = err.Error(), err.Error()
		return result, raw
	}
	result.Claims = len(verdict.Claims)
	result.Flag = verdict.Flagged()
	result.Hit = verdict.Hits(row.Text)
	return result, raw
}

func boolField(v bool) string {
	if v {
		return "1"
	}
	return "0"
}

func (r claimRun) fields() []string {
	clean := strings.NewReplacer("\t", " ", "\n", " ", "\r", " ").Replace(r.Error)
	return []string{r.Ref, r.Part, r.Class, boolField(r.Refuted), r.Model, strconv.Itoa(r.Run),
		boolField(r.Flag), boolField(r.Hit), strconv.Itoa(r.Claims), strconv.Itoa(r.TokensIn),
		strconv.Itoa(r.TokensOut), strconv.FormatInt(r.MS, 10), clean, boolField(r.Prefiltered)}
}

func parseClaimRun(fields []string) (claimRun, error) {
	if len(fields) != len(claimTSVHeader) {
		return claimRun{}, fmt.Errorf("want %d fields, got %d", len(claimTSVHeader), len(fields))
	}
	var r claimRun
	var errs []error
	flag := func(s string) bool {
		if s != "0" && s != "1" {
			errs = append(errs, fmt.Errorf("bad flag %q", s))
		}
		return s == "1"
	}
	number := func(s string) int {
		n, err := strconv.Atoi(s)
		if err != nil {
			errs = append(errs, err)
		}
		return n
	}
	r.Ref, r.Part, r.Class, r.Refuted, r.Model = fields[0], fields[1], fields[2], flag(fields[3]), fields[4]
	r.Run, r.Flag, r.Hit, r.Claims = number(fields[5]), flag(fields[6]), flag(fields[7]), number(fields[8])
	r.TokensIn, r.TokensOut, r.MS = number(fields[9]), number(fields[10]), int64(number(fields[11]))
	r.Error, r.Prefiltered = fields[12], flag(fields[13])
	return r, errors.Join(errs...)
}

func readClaimRuns(path string) ([]claimRun, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open runs: %w", err)
	}
	defer file.Close()
	var runs []claimRun
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1<<20), 16<<20)
	for n := 1; scanner.Scan(); n++ {
		fields := strings.Split(scanner.Text(), "\t")
		if fields[0] == "ref" || scanner.Text() == "" {
			continue
		}
		r, err := parseClaimRun(fields)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, n, err)
		}
		runs = append(runs, r)
	}
	return runs, scanner.Err()
}

func summarizeClaimFiles(paths []string, out io.Writer) error {
	var runs []claimRun
	for _, path := range paths {
		r, err := readClaimRuns(path)
		if err != nil {
			return err
		}
		runs = append(runs, r...)
	}
	_, err := io.WriteString(out, summarizeClaimRuns(runs))
	return err
}

// wilson — интервал Вильсона 95% для k из n; при n = 0 — весь отрезок
func wilson(k, n int) (float64, float64) {
	if n == 0 {
		return 0, 1
	}
	const z = 1.959963984540054
	p, nf := float64(k)/float64(n), float64(n)
	center := p + z*z/(2*nf)
	spread := z * math.Sqrt(p*(1-p)/nf+z*z/(4*nf*nf))
	denom := 1 + z*z/nf
	return math.Max(0, (center-spread)/denom), math.Min(1, (center+spread)/denom)
}

// percentile — значение ранга p (ближайший ранг) среди отсортированных
func percentile(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(p * float64(len(sorted))))
	return sorted[min(max(rank, 1), len(sorted))-1]
}

// claimRowResult — итог строки набора по всем прогонам: большинство
type claimRowResult struct {
	ref, part, class  string
	refuted           bool
	runs, flags, hits int
}

func (r claimRowResult) flag() bool    { return 2*r.flags > r.runs }
func (r claimRowResult) hit() bool     { return 2*r.hits > r.runs }
func (r claimRowResult) diverge() bool { return r.runs > 1 && r.flags > 0 && r.flags < r.runs }

// claimGroup — класс в сводке: unmarked делится по опровержению
func claimGroup(class string, refuted bool) string {
	if class == "unmarked" {
		if refuted {
			return "unmarked опровергнутые"
		}
		return "unmarked без опровержения"
	}
	return class
}

var claimGroupOrder = []string{"unmarked опровергнутые", "unmarked без опровержения", "ok", "marked", "checked-wrong-source"}

// claimExcludedSession — сессия, о которой план требует итог с ней и без неё
const claimExcludedSession = "27d21ddd"

func share(k, n int) string {
	lo, hi := wilson(k, n)
	if n == 0 {
		return fmt.Sprintf("%d/%d", k, n)
	}
	return fmt.Sprintf("%d/%d %.2f [%.2f–%.2f]", k, n, float64(k)/float64(n), lo, hi)
}

// claimGroupStats — строки одной модели и части: итоги по строкам в порядке
// TSV, сбои, предфильтр и замеры вызовов судьи
type claimGroupStats struct {
	rows                  map[string]*claimRowResult
	order                 []string
	failures, prefiltered int
	ms, in, out           []int64
}

func collectClaimGroup(group []claimRun) claimGroupStats {
	s := claimGroupStats{rows: map[string]*claimRowResult{}}
	for i := range group {
		r := &group[i]
		row := s.rows[r.Ref]
		if row == nil {
			row = &claimRowResult{ref: r.Ref, part: r.Part, class: r.Class, refuted: r.Refuted}
			s.rows[r.Ref] = row
			s.order = append(s.order, r.Ref)
		}
		row.runs++
		if r.Flag {
			row.flags++
		}
		if r.Hit {
			row.hits++
		}
		if r.Error != "" {
			s.failures++
		}
		if r.Prefiltered {
			s.prefiltered++
			continue
		}
		s.ms = append(s.ms, r.MS)
		s.in = append(s.in, int64(r.TokensIn))
		s.out = append(s.out, int64(r.TokensOut))
	}
	for _, v := range [][]int64{s.ms, s.in, s.out} {
		slices.Sort(v)
	}
	return s
}

// writeScope — доли flag и попадания по классам; without — сессия,
// строки которой не считаются (пусто — все строки)
func (s claimGroupStats) writeScope(b *strings.Builder, without string) {
	counts := map[string]*[3]int{}
	var groups []string
	for _, ref := range s.order {
		row := s.rows[ref]
		if without != "" && strings.HasPrefix(ref, without+":") {
			continue
		}
		g := claimGroup(row.class, row.refuted)
		c := counts[g]
		if c == nil {
			c = &[3]int{}
			counts[g] = c
			groups = append(groups, g)
		}
		c[0]++
		if row.flag() {
			c[1]++
		}
		if row.hit() {
			c[2]++
		}
	}
	for _, g := range orderedGroups(groups) {
		c := counts[g]
		fmt.Fprintf(b, "    %-26s flag %s; попадание %s\n", g, share(c[1], c[0]), share(c[2], c[0]))
	}
}

// divergence — строки с несколькими прогонами и те из них, где flag разный
func (s claimGroupStats) divergence() (int, int) {
	multi, diverge := 0, 0
	for _, ref := range s.order {
		if s.rows[ref].runs > 1 {
			multi++
			if s.rows[ref].diverge() {
				diverge++
			}
		}
	}
	return diverge, multi
}

// summarizeClaimRuns — сводка по модели и части: доли flag и попадания по
// классам (итог строки — большинство прогонов; сбой — не flag) с 27d21ddd и
// без неё, расхождение прогонов, сбои, предфильтр, токены и мс
func summarizeClaimRuns(runs []claimRun) string {
	type key struct{ model, part string }
	byKey := map[key][]claimRun{}
	var keys []key
	for i := range runs {
		k := key{runs[i].Model, runs[i].Part}
		if _, seen := byKey[k]; !seen {
			keys = append(keys, k)
		}
		byKey[k] = append(byKey[k], runs[i])
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].model != keys[j].model {
			return keys[i].model < keys[j].model
		}
		return keys[i].part < keys[j].part
	})
	var b strings.Builder
	for _, k := range keys {
		s := collectClaimGroup(byKey[k])
		fmt.Fprintf(&b, "model %s, part %s: строк %d, вызовов судьи %d, отсеяно предфильтром %d\n", k.model, k.part, len(s.order), len(s.ms), s.prefiltered)
		b.WriteString("  все\n")
		s.writeScope(&b, "")
		fmt.Fprintf(&b, "  без %s\n", claimExcludedSession)
		s.writeScope(&b, claimExcludedSession)
		fmt.Fprintf(&b, "  расхождение прогонов по flag: %s\n", share(s.divergence()))
		fmt.Fprintf(&b, "  сбои: %d из %d вызовов\n", s.failures, len(s.ms))
		fmt.Fprintf(&b, "  токены вход p50 %d p95 %d; выход p50 %d p95 %d; мс p50 %d p95 %d\n",
			percentile(s.in, 0.5), percentile(s.in, 0.95), percentile(s.out, 0.5), percentile(s.out, 0.95), percentile(s.ms, 0.5), percentile(s.ms, 0.95))
	}
	return b.String()
}

// orderedGroups — классы в порядке мер плана, незнакомые — после, по алфавиту
func orderedGroups(groups []string) []string {
	present := map[string]bool{}
	for _, g := range groups {
		present[g] = true
	}
	var out []string
	for _, g := range claimGroupOrder {
		if present[g] {
			out = append(out, g)
			delete(present, g)
		}
	}
	return append(out, slices.Sorted(maps.Keys(present))...)
}
