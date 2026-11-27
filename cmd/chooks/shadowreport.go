package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/aiseeq/chooks/internal/core"
)

// shadowTimeLayout — время строк журнала тени и файла длительностей
const shadowTimeLayout = "2006-01-02T15:04:05.000Z07:00"

func newShadowReportCmd(exitCode *int) *cobra.Command {
	var since time.Duration
	cmd := &cobra.Command{
		Use:   "shadow-report [--since 24h]",
		Short: "Сводка тени демона: охват, сравнения, расхождения, несравнимые и пропущенные вызовы за период",
		Long: `Читает журнал тени chooks-shadow.tsv и файл длительностей chooks-hooks.tsv
рядом с chooks.log и печатает за период по каждой сравниваемой подкоманде:
вызовы CLI (строки chooks-hooks.tsv без столбцов прокси), сравнения (совпало и
расхождения), несравнимые и пропущенные — вызовы CLI без строки тени (запись не
ушла, демона не было, хук убит до отправки). Ниже — расхождения и причины
несравнимых по счёту и подкоманды вне тени с причиной.

Код выхода: 0 — сравнения есть и расхождений нет; 1 — есть расхождения или
нет ни одного сравнения (ноль сравнений — не «без расхождений»); 2 — журналы
не прочитаны.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			request, err := commandRequest(cmd)
			if err != nil {
				*exitCode = 2
				return err
			}
			config, err := core.LoadConfigForRequest(commandConfigPath(cmd), request)
			if err != nil {
				*exitCode = 2
				return fmt.Errorf("failed to load config: %w", err)
			}
			if config.Logger.Output != "file" || config.Logger.LogFile == "" {
				*exitCode = 2
				return fmt.Errorf("журнал тени лежит рядом с файлом лога, а logger.output не file")
			}
			dir := filepath.Dir(config.Logger.LogFile)
			to := request.Now()
			report, err := buildShadowReport(filepath.Join(dir, shadowJournalFile), filepath.Join(dir, durationsFile), to.Add(-since), to)
			if err != nil {
				*exitCode = 2
				return err
			}
			report.write(cmd.OutOrStdout(), request.Location())
			if report.compared() == 0 || report.mismatches() > 0 {
				*exitCode = 1
			}
			return nil
		},
	}
	cmd.Flags().DurationVar(&since, "since", 24*time.Hour, "период до текущего момента")
	return cmd
}

// shadowCount — счёт одной подкоманды за период
type shadowCount struct {
	calls, paired, pairedMatch, pairedMismatch int
	match, mismatch, incomparable              int
}

type shadowReport struct {
	from, to          time.Time
	journal           string
	counts            map[string]*shadowCount
	mismatchDetails   map[string]int
	incomparableWhy   map[string]int
	journalMissing    bool
	durationsMissing  bool
	shadowWithoutCall int
	// coveredFrom — первая строка файла длительностей (с ротированным .1)
	// позже начала периода: вызовы CLI до неё не видны, и период считается
	// с неё; нулевое — период покрыт
	coveredFrom time.Time
}

func (r *shadowReport) count(hook string) *shadowCount {
	c := r.counts[hook]
	if c == nil {
		c = &shadowCount{}
		r.counts[hook] = c
	}
	return c
}

func (r *shadowReport) compared() int {
	n := 0
	for _, c := range r.counts {
		n += c.pairedMatch + c.pairedMismatch
	}
	return n
}

func (r *shadowReport) mismatches() int {
	n := 0
	for _, c := range r.counts {
		n += c.mismatch
	}
	return n
}

// shadowCLICall — прямой вызов CLI сравниваемой подкоманды из файла
// длительностей
type shadowCLICall struct {
	hook    string
	session string
	at      time.Time
}

type shadowCallKey struct {
	hook, session string
}

// buildShadowReport считает журнал тени и вызовы CLI из файла длительностей
// за [from, to]; оба файла читаются с ротированным .1. Нет файла — ноль
// строк, отмечено в сводке. Файл длительностей начинается позже from —
// период сужается до его начала, чтобы ротация не прятала пропуски
func buildShadowReport(journal, durations string, from, to time.Time) (*shadowReport, error) {
	r := &shadowReport{from: from, to: to, journal: journal, counts: map[string]*shadowCount{},
		mismatchDetails: map[string]int{}, incomparableWhy: map[string]int{}}
	for _, hook := range shadowCovered() {
		r.count(hook)
	}
	// Вызов CLI напрямую — строка из шести столбцов (recordDuration);
	// строки прокси (восемь столбцов) тень не шлют, но начало файла
	// показывают
	var calls []shadowCLICall
	var first time.Time
	missing, err := scanRotated(durations, func(fields []string) {
		at, ok := shadowLineTime(fields)
		if !ok {
			return
		}
		if first.IsZero() || at.Before(first) {
			first = at
		}
		if _, covered := shadowHooks[fields[1]]; covered && len(fields) == 6 {
			calls = append(calls, shadowCLICall{hook: fields[1], session: fields[2], at: at})
		}
	})
	if err != nil {
		return nil, err
	}
	r.durationsMissing = missing
	if first.After(from) {
		r.coveredFrom, from = first, first
	}
	inPeriod := func(at time.Time) bool { return !at.Before(from) && !at.After(to) }
	for _, c := range calls {
		if inPeriod(c.at) {
			r.count(c.hook).calls++
		}
	}
	// Строки обоих журналов имеют разные временные метки: длительность
	// записывается при завершении CLI, а тень — со временем запроса. Поэтому
	// связываем их по хуку и сессии и учитываем не более одной тени на вызов
	// CLI, попавший в период.
	remaining := make(map[shadowCallKey]int)
	for _, c := range calls {
		if inPeriod(c.at) {
			remaining[shadowCallKey{hook: c.hook, session: c.session}]++
		}
	}
	missing, err = scanRotated(journal, func(fields []string) {
		r.countJournalRow(fields, inPeriod, remaining)
	})
	if err != nil {
		return nil, err
	}
	r.journalMissing = missing
	return r, nil
}

func (r *shadowReport) countJournalRow(fields []string, inPeriod func(time.Time) bool, remaining map[shadowCallKey]int) {
	at, ok := shadowLineTime(fields)
	if !ok || len(fields) < 5 || !inPeriod(at) {
		return
	}
	hook := fields[1]
	key := shadowCallKey{hook: hook, session: fields[2]}
	switch fields[3] {
	case shadowMatch, shadowMismatch, shadowIncomparable:
	default:
		return
	}
	c := r.count(hook)
	paired := remaining[key] > 0
	if paired {
		remaining[key]--
		c.paired++
	} else {
		r.shadowWithoutCall++
	}
	switch fields[3] {
	case shadowMatch:
		c.match++
		if paired {
			c.pairedMatch++
		}
	case shadowMismatch:
		c.mismatch++
		if paired {
			c.pairedMismatch++
		}
		r.mismatchDetails[hook+": "+fields[4]]++
	case shadowIncomparable:
		c.incomparable++
		r.incomparableWhy[hook+": "+fields[4]]++
	}
}

// shadowLineTime — время строки журнала (первый столбец); false — строка
// короче двух столбцов или время не разобрано
func shadowLineTime(fields []string) (time.Time, bool) {
	if len(fields) < 2 {
		return time.Time{}, false
	}
	at, err := time.Parse(shadowTimeLayout, fields[0])
	return at, err == nil
}

// scanRotated — scanTSV по ротированному path.1, затем по path; true —
// нет ни того, ни другого
func scanRotated(path string, row func(fields []string)) (bool, error) {
	oldMissing, err := scanTSV(path+".1", row)
	if err != nil {
		return false, err
	}
	missing, err := scanTSV(path, row)
	if err != nil {
		return false, err
	}
	return oldMissing && missing, nil
}

// scanTSV зовёт row на каждую строку файла; true — файла нет
func scanTSV(path string, row func(fields []string)) (bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		row(strings.Split(scanner.Text(), "\t"))
	}
	if err := scanner.Err(); err != nil {
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	return false, nil
}

func (r *shadowReport) write(w io.Writer, loc *time.Location) {
	fmt.Fprintf(w, "Тень за %s — %s\n", r.from.In(loc).Format("2006-01-02 15:04"), r.to.In(loc).Format("2006-01-02 15:04 MST"))
	if r.journalMissing {
		fmt.Fprintf(w, "журнала тени нет: %s\n", r.journal)
	}
	if r.durationsMissing {
		fmt.Fprintln(w, "файла длительностей нет: вызовы CLI не посчитаны")
	}
	if !r.coveredFrom.IsZero() {
		fmt.Fprintf(w, "период не покрыт файлом длительностей до %s: счёт с этого момента\n", r.coveredFrom.In(loc).Format("2006-01-02 15:04:05"))
	}
	var total shadowCount
	missed := 0
	for _, hook := range slices.Sorted(maps.Keys(r.counts)) {
		c := r.counts[hook]
		// Пар не больше вызовов: remaining считает только вызовы периода
		skipped := c.calls - c.paired
		missed += skipped
		total.calls += c.calls
		total.match += c.pairedMatch
		total.mismatch += c.mismatch
		total.incomparable += c.incomparable
		// Расхождение видно всегда, даже у строки тени без вызова CLI
		unpaired := ""
		if extra := c.mismatch - c.pairedMismatch; extra > 0 {
			unpaired = fmt.Sprintf(", расхождений без вызова CLI %d", extra)
		}
		fmt.Fprintf(w, "%s: вызовов CLI %d, сравнено %d (совпало %d, расхождений %d)%s, не сравнимо %d, пропущено %d\n",
			hook, c.calls, c.pairedMatch+c.pairedMismatch, c.pairedMatch, c.pairedMismatch, unpaired, c.incomparable, skipped)
	}
	writeCounts(w, "Расхождения (подкоманда: поля):", r.mismatchDetails)
	writeCounts(w, "Не сравнимо (подкоманда: причина):", r.incomparableWhy)
	fmt.Fprintln(w, "Вне тени:")
	for _, hook := range slices.Sorted(maps.Keys(shadowOutside)) {
		fmt.Fprintf(w, "  %s — %s\n", hook, shadowOutside[hook])
	}
	fmt.Fprintf(w, "Итог: вызовов CLI %d, сравнено %d, расхождений %d, не сравнимо %d\n",
		total.calls, r.compared(), total.mismatch, total.incomparable)
	fmt.Fprintf(w, "Пропущено (вызовы CLI без строки тени): %d\n", missed)
	fmt.Fprintf(w, "Строк тени без вызова CLI: %d\n", r.shadowWithoutCall)
	if r.compared() == 0 {
		fmt.Fprintln(w, "Сравнений нет: это не «без расхождений».")
	}
}

func writeCounts(w io.Writer, title string, counts map[string]int) {
	if len(counts) == 0 {
		return
	}
	fmt.Fprintln(w, title)
	for _, key := range slices.Sorted(maps.Keys(counts)) {
		fmt.Fprintf(w, "  %s — %d\n", key, counts[key])
	}
}
