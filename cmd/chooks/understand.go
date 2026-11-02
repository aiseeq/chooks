package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/aiseeq/akit/transcript"

	"github.com/aiseeq/chooks/internal/questioncheck"
	"github.com/aiseeq/chooks/internal/understandcheck"
)

func newUnderstandCasesCmd() *cobra.Command {
	var days int
	var project string
	cmd := &cobra.Command{
		Use:   "understand-cases [--days 7] [--project S]",
		Short: "Выбрать из транскриптов действия, которые уносят слово пользователя дальше",
		Long: `Читает транскрипты сессий (~/.claude/projects/*/*.jsonl, без суб-агентов),
изменённые за --days дней, и печатает JSONL случаев проверки понимания: письмо
другой сессии (SendMessage), бриф суб-агенту (Agent) и правка файла инструкций
(Edit, Write, MultiEdit, правка из Bash) в ходе после реплики пользователя — с
этой репликой, концом сообщения агента перед ней и следующей репликой
пользователя для разметки. Тяжёлое чтение: запускать через graft exec --heavy.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			since := time.Now().AddDate(0, 0, -days)
			cases, err := collectUnderstandCases(filepath.Join(home, ".claude", "projects"), project, since)
			if err != nil {
				return err
			}
			encoder := json.NewEncoder(cmd.OutOrStdout())
			for i := range cases {
				if err := encoder.Encode(cases[i]); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&days, "days", 7, "сколько дней назад")
	cmd.Flags().StringVar(&project, "project", "", "только папки проектов, содержащие эту строку")
	return cmd
}

// collectUnderstandCases — случаи из транскриптов папки projects, изменённых
// после since; события раньше since не дают случаев, но задают реплику хода
func collectUnderstandCases(projects, project string, since time.Time) ([]understandcheck.Case, error) {
	paths, err := filepath.Glob(filepath.Join(projects, "*", "*.jsonl"))
	if err != nil {
		return nil, fmt.Errorf("list transcripts: %w", err)
	}
	var cases []understandcheck.Case
	for _, path := range paths {
		if project != "" && !bytes.Contains([]byte(filepath.Base(filepath.Dir(path))), []byte(project)) {
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("stat %s: %w", path, err)
		}
		if info.ModTime().Before(since) {
			continue
		}
		collector := understandcheck.Collector{Session: sessionPrefix(path)}
		meta, err := transcript.Read(path, keepUnderstandLine, collector.Add)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		found := collector.Cases()
		for i := range found {
			c := found[i]
			if c.Time.Before(since) {
				continue
			}
			c.Name = meta.Name
			cases = append(cases, c)
		}
	}
	sort.SliceStable(cases, func(i, j int) bool { return cases[i].Time.Before(cases[j].Time) })
	return cases, nil
}

// keepUnderstandLine отбрасывает выводы инструментов до разбора JSON: они
// большие и случаям не нужны, кроме ответов в окне вопроса (поле answers)
func keepUnderstandLine(raw []byte) bool {
	return transcript.WithoutResults(raw) || bytes.Contains(raw, []byte(`"answers":`))
}

// understandResult — вердикт судьи по случаю
type understandResult struct {
	ID        string                `json:"id"`
	Kind      understandcheck.Kind  `json:"kind"`
	Verdict   string                `json:"verdict"`
	Fixes     []understandcheck.Fix `json:"fixes,omitempty"`
	Error     string                `json:"error,omitempty"`
	InputLen  int                   `json:"input_runes"`
	LatencyMS int64                 `json:"latency_ms"`
	Usage     questioncheck.Usage   `json:"usage"`
}

func newUnderstandBenchCmd() *cobra.Command {
	var model, effort string
	var jobs int
	var limit time.Duration
	cmd := &cobra.Command{
		Use:   "understand-bench [--model opus] [--effort high] <случаи.jsonl>",
		Short: "Прогнать судью проверки понимания по случаям understand-cases",
		Long: `Читает JSONL случаев (understand-cases или размеченные вручную: human, prev,
kind, action) и спрашивает судью, расходится ли действие с репликой
пользователя. Печатает JSONL вердиктов с токенами и задержкой. В хук не
встроено: это сухой прогон до решения. Тяжёлое: через graft exec --heavy.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			judge, err := newJudge(home, model, effort)
			if err != nil {
				return err
			}
			judge.Prompt = understandcheck.Prompt
			cases, err := readUnderstandCases(args[0])
			if err != nil {
				return err
			}
			return runUnderstandBench(cmd.Context(), judge, cases, jobs, limit, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&model, "model", judgeModel, "модель судьи из ~/.config/models/approved")
	cmd.Flags().StringVar(&effort, "effort", judgeEffort, "усилие рассуждения: low, medium, high")
	cmd.Flags().IntVar(&jobs, "jobs", 2, "сколько судей звать одновременно")
	cmd.Flags().DurationVar(&limit, "timeout", 3*time.Minute, "предел на один вызов судьи")
	return cmd
}

func readUnderstandCases(path string) ([]understandcheck.Case, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open cases: %w", err)
	}
	defer file.Close()
	var cases []understandcheck.Case
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1<<20), 64<<20)
	for line := 1; scanner.Scan(); line++ {
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			continue
		}
		var c understandcheck.Case
		if err := json.Unmarshal(scanner.Bytes(), &c); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, line, err)
		}
		if c.Human == "" || c.Action == "" {
			return nil, fmt.Errorf("%s:%d: case %q without human or action", path, line, c.ID)
		}
		cases = append(cases, c)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read cases: %w", err)
	}
	return cases, nil
}

// runUnderstandBench зовёт судью по случаям в jobs потоков; сбой вызова —
// строка с error, прогон идёт дальше; ошибка — только запись вывода
func runUnderstandBench(ctx context.Context, judge questioncheck.CLIJudge, cases []understandcheck.Case, jobs int, limit time.Duration, out io.Writer) error {
	encoder := json.NewEncoder(out)
	var mu sync.Mutex
	var firstErr error
	queue := make(chan understandcheck.Case)
	var wg sync.WaitGroup
	for range max(jobs, 1) {
		wg.Go(func() {
			for c := range queue {
				result := judgeUnderstandCase(ctx, judge, c, limit)
				mu.Lock()
				if err := encoder.Encode(result); err != nil && firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
			}
		})
	}
	for i := range cases {
		queue <- cases[i]
	}
	close(queue)
	wg.Wait()
	return firstErr
}

func judgeUnderstandCase(ctx context.Context, judge questioncheck.CLIJudge, c understandcheck.Case, limit time.Duration) understandResult {
	input := c.Input()
	result := understandResult{ID: c.ID, Kind: c.Kind, InputLen: len([]rune(input))}
	callCtx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	start := time.Now()
	answer, usage, err := judge.Ask(callCtx, input)
	result.LatencyMS = time.Since(start).Milliseconds()
	result.Usage = usage
	if err != nil {
		result.Verdict, result.Error = "error", err.Error()
		return result
	}
	verdict, err := understandcheck.ParseVerdict(answer)
	if err != nil {
		result.Verdict, result.Error = "error", err.Error()
		return result
	}
	result.Verdict, result.Fixes = verdict.Verdict, verdict.Fixes
	return result
}

// sessionPrefix — первые 8 знаков id сессии из имени транскрипта, как у tl
// (ссылки вида tl 49856745:5774)
func sessionPrefix(path string) string {
	base := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if len(base) > 8 {
		return base[:8]
	}
	return base
}
