package main

import (
	"bufio"
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

	"github.com/aiseeq/chooks/internal/questioncheck"
)

// saidCase — пример прогона сверки: сообщение агента, реплика, начавшая ход,
// слова пользователя из разбора (у обычных ходов их нет) и метка
type saidCase struct {
	ID      string                    `json:"id"`
	Label   string                    `json:"label"`
	Time    time.Time                 `json:"ts"`
	Human   string                    `json:"human"`
	Message string                    `json:"message"`
	Quotes  []questioncheck.SaidQuote `json:"quotes"`
	// Turn — весь ход агента, Project — каталог проекта Claude Code
	// (-home-aisee-work): для --turn и --said
	Turn    string `json:"turn"`
	Project string `json:"project"`
}

// saidResult — вердикт по примеру. Found — поиск по журналу нашёл хотя бы
// одну цитату разбора: полнота поиска отдельно от точности судьи
type saidResult struct {
	ID        string                      `json:"id"`
	Label     string                      `json:"label"`
	Verdict   string                      `json:"verdict"`
	Findings  []questioncheck.SaidFinding `json:"findings,omitempty"`
	Error     string                      `json:"error,omitempty"`
	Given     int                         `json:"given"`
	Searched  int                         `json:"searched"`
	Found     bool                        `json:"found"`
	LatencyMS int64                       `json:"latency_ms"`
	Usage     questioncheck.Usage         `json:"usage"`
}

// saidBench — настройки прогона
type saidBench struct {
	judge    questioncheck.CLIJudge
	journals questioncheck.Journals
	// given — давать судье цитаты разбора; search — сколько реплик добавить поиском
	given   bool
	search  int
	timeout time.Duration
	// turn — судья видит весь ход; said — записи said.tsv, saidLimit — сколько
	// взять поиском сверх записей той же сессии
	turn      bool
	said      []questioncheck.SaidRecord
	saidLimit int
	home      string
}

func newSaidBenchCmd() *cobra.Command {
	var model, effort, saidPath string
	bench := saidBench{}
	jobs := 4
	cmd := &cobra.Command{
		Use:   "said-bench [--model luna] [--search 5] [--given=false] <примеры.jsonl>",
		Short: "Прогнать судью сверки ответа со словом пользователя по размеченным примерам",
		Long: `Читает JSONL примеров {id, label, ts, human, message, quotes} и спрашивает
судью сверки, расходится ли сообщение агента с прежними словами пользователя.
Слова — цитаты примера (--given) и реплики из журнала, найденные по словам
сообщения раньше ts (--search N). Нет ни одной цитаты — pass без вызова.
Печатает JSONL вердиктов. В хук не встроено: это прогон до решения.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			if bench.judge, err = newJudge(home, model, effort); err != nil {
				return err
			}
			bench.judge.Prompt = questioncheck.SaidPrompt
			if bench.turn {
				bench.judge.Prompt = questioncheck.SaidTurnPrompt
			}
			bench.journals = questioncheck.DefaultJournals(filepath.Join(home, ".claude"))
			bench.home = home
			if saidPath != "" {
				if bench.said, err = questioncheck.ReadSaid(saidPath); err != nil {
					return err
				}
			}
			return bench.run(cmd.Context(), args[0], jobs, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&model, "model", "luna", "модель судьи из ~/.config/models/approved")
	cmd.Flags().StringVar(&effort, "effort", "medium", "усилие рассуждения: low, medium, high")
	cmd.Flags().BoolVar(&bench.given, "given", true, "давать судье цитаты примера")
	cmd.Flags().IntVar(&bench.search, "search", 5, "сколько реплик журнала добавить поиском по словам сообщения")
	cmd.Flags().IntVar(&jobs, "jobs", 4, "сколько судей звать одновременно")
	cmd.Flags().DurationVar(&bench.timeout, "timeout", 3*time.Minute, "предел на один вызов судьи")
	cmd.Flags().BoolVar(&bench.turn, "turn", false, "давать судье весь ход (поле turn), а не последнее сообщение")
	cmd.Flags().StringVar(&saidPath, "said", "", "said.tsv: давать судье действующие записи вместо поиска по журналу реплик")
	cmd.Flags().IntVar(&bench.saidLimit, "said-limit", 10, "сколько записей said.tsv взять поиском сверх записей той же сессии")
	return cmd
}

func (b saidBench) run(ctx context.Context, path string, jobs int, out io.Writer) error {
	examples, err := readSaidCases(path)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(out)
	var mu sync.Mutex
	var firstErr error
	queue := make(chan saidCase)
	var wg sync.WaitGroup
	for i := 0; i < max(jobs, 1); i++ {
		wg.Go(func() {
			for example := range queue {
				result, err := b.one(ctx, example)
				mu.Lock()
				if err == nil {
					err = encoder.Encode(result)
				}
				if err != nil && firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
			}
		})
	}
	for i := range examples {
		queue <- examples[i]
	}
	close(queue)
	wg.Wait()
	return firstErr
}

func readSaidCases(path string) ([]saidCase, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open examples: %w", err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1<<20), 16<<20)
	var examples []saidCase
	for scanner.Scan() {
		var example saidCase
		if err := json.Unmarshal(scanner.Bytes(), &example); err != nil {
			return nil, fmt.Errorf("parse example: %w", err)
		}
		examples = append(examples, example)
	}
	return examples, scanner.Err()
}

// one — вердикт по одному примеру; ошибка — только сбой чтения журнала,
// сбой судьи пишется в результат
func (b saidBench) one(ctx context.Context, example saidCase) (saidResult, error) {
	result := saidResult{ID: example.ID, Label: example.Label}
	var quotes []questioncheck.SaidQuote
	if b.given {
		quotes = append(quotes, example.Quotes...)
		result.Given = len(example.Quotes)
	}
	if b.said != nil {
		said := b.inForce(example)
		result.Searched = len(said)
		result.Found = coversAny(said, example.Quotes)
		quotes = append(quotes, said...)
	} else if b.search > 0 {
		searched, err := b.journals.FindSaid(example.Message, example.Time, b.search)
		if err != nil {
			return result, err
		}
		result.Searched = len(searched)
		result.Found = coversAny(searched, example.Quotes)
		quotes = append(quotes, searched...)
	}
	if len(quotes) == 0 {
		result.Verdict = questioncheck.VerdictPass
		return result, nil
	}
	saidCase := questioncheck.SaidCase{Human: example.Human, Quotes: quotes, Message: example.Message}
	if b.turn {
		saidCase.Turn = example.Turn
	}
	input := saidCase.Input()
	judgeCtx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	started := time.Now()
	answer, usage, err := b.judge.Ask(judgeCtx, input)
	result.LatencyMS = time.Since(started).Milliseconds()
	result.Usage = usage
	var verdict questioncheck.SaidVerdict
	if err == nil {
		verdict, err = questioncheck.ParseSaidVerdict(answer)
	}
	if err != nil {
		result.Verdict = questioncheck.VerdictError
		result.Error = err.Error()
		return result, nil
	}
	result.Verdict, result.Findings = verdict.Verdict, verdict.Findings
	return result, nil
}

// sameSessionSaid — сколько последних записей той же сессии судья видит всегда:
// обещания и поручения этой беседы поиск по словам хода может не найти
const sameSessionSaid = 10

// inForce — действующие записи said.tsv для примера: последние записи той же
// сессии и лучшие по словам реплики и хода из остальных
func (b saidBench) inForce(example saidCase) []questioncheck.SaidQuote {
	transcript, _, _ := strings.Cut(example.ID, ".jsonl:")
	session := filepath.Base(transcript)
	var own, other []questioncheck.SaidRecord
	// ts примера — время следующей реплики пользователя (поправки), а время
	// записи said.tsv — до секунды: без среза по секунде судья видел бы
	// запись из самой поправки (26.10: 32 из 42 положительных, 16 из 150
	// отрицательных). Живой хук срабатывает до реплики, ему это не нужно
	cutoff := example.Time.Truncate(time.Second)
	inForce := questioncheck.InForce(b.said, example.Project, b.home, cutoff)
	for i := range inForce {
		id, _, _ := strings.Cut(inForce[i].Ref, ":")
		if id != "" && strings.HasPrefix(session, id) {
			own = append(own, inForce[i])
		} else {
			other = append(other, inForce[i])
		}
	}
	// said-eval.tsv собирается не по порядку дат
	sort.SliceStable(own, func(i, j int) bool { return own[i].Time.Before(own[j].Time) })
	own = own[max(len(own)-sameSessionSaid, 0):]
	text := example.Message
	if b.turn {
		text = example.Turn
	}
	var quotes []questioncheck.SaidQuote
	chosen := append(own, questioncheck.RankSaid(other, example.Human+"\n"+text, b.saidLimit)...)
	for i := range chosen {
		quotes = append(quotes, chosen[i].AsQuote())
	}
	return quotes
}

// coversAny — среди найденных поиском есть реплика, содержащая начало одной
// из цитат разбора (цитата — дословный кусок реплики). Пробелы схлопываются:
// журнал реплик пишет их так
func coversAny(searched, wanted []questioncheck.SaidQuote) bool {
	for _, want := range wanted {
		head := []rune(strings.Join(strings.Fields(want.Text), " "))
		head = head[:min(len(head), 60)]
		if len(head) == 0 {
			continue
		}
		for _, found := range searched {
			if strings.Contains(strings.Join(strings.Fields(found.Text), " "), string(head)) {
				return true
			}
		}
	}
	return false
}
