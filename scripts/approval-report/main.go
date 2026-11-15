// approval-report считает действия классов 1–4 (инструкции, конфиг-репо, выкатка, запись в
// Jira/Bitbucket) у суб-агентов Claude и бойцов worker за окно времени. Только читает транскрипты
// и состояние worker, пишет только в stdout. Тикет subagent-approval-actions-report.
//
//	go run ./scripts/approval-report --since 2026-11-08T00:00:00+01:00 --until 2026-11-15T13:24:00+01:00
//	go run ./scripts/approval-report … --check a558a391:129 --check 1113-1209-step5-plan
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(s string) error { *m = append(*m, s); return nil }

func usageErr(msg string, err error) {
	fmt.Fprintln(os.Stderr, "approval-report:", msg, err)
	os.Exit(2)
}

func main() {
	home, err := os.UserHomeDir()
	if err != nil {
		usageErr("домашний каталог для путей по умолчанию:", err)
	}
	since := flag.String("since", "", "начало окна, RFC3339 (обязательно)")
	until := flag.String("until", "", "конец окна, RFC3339 (обязательно)")
	projects := flag.String("projects", filepath.Join(home, ".claude/projects"), "каталог транскриптов Claude Code")
	worker := flag.String("worker", filepath.Join(home, ".local/state/worker"), "каталог состояния worker")
	homeDir := flag.String("home", home, "домашний каталог для путей ~/.claude")
	jobs := flag.Int("j", 4, "файлов параллельно, от 1")
	flag.BoolVar(&listAll, "list", false, "печатать все попадания и возможные записи, не только по 3 примера")
	var checks multiFlag
	flag.Var(&checks, "check", "ссылка для сверки: id:строка суб-агента, задача/attN.jsonl:строка или задача бойца (повторяется)")
	flag.Parse()
	from, err1 := time.Parse(time.RFC3339, *since)
	to, err2 := time.Parse(time.RFC3339, *until)
	if err := errors.Join(err1, err2); err != nil {
		usageErr("--since и --until в RFC3339:", err)
	}
	if *jobs < 1 {
		usageErr("-j:", fmt.Errorf("нужно число от 1, дано %d", *jobs))
	}
	sc := scanner{home: *homeDir, from: from, to: to, jobs: *jobs}
	res, err := sc.run(*projects, *worker)
	if err != nil {
		fmt.Fprintln(os.Stderr, "approval-report:", err)
		os.Exit(1)
	}
	report(os.Stdout, res, from, to)
	if len(checks) > 0 {
		reportChecks(os.Stdout, res, checks)
	}
}

// Agent — суб-агент или боец.
type Agent struct {
	Pop     string // sub, worker
	ID      string // полный id агента или задача бойца
	Short   string // id в ссылке tl
	Master  string
	Keys    map[string]bool
	Events  int // вызовов инструментов в окне
	Untimed int // из них у бойца без времени в rollout codex: в окне по прежнему правилу
}

// Record — событие с признаком хотя бы одного класса.
type Record struct {
	Agent *Agent
	File  string // attN.jsonl у бойца
	Line  int
	Time  time.Time // у бойца без времени вызова в rollout codex — нулевое
	V     Verdict
}

// Ref — ссылка на событие в формате отчёта.
func (r Record) Ref() string {
	if r.Agent.Pop == "sub" {
		return fmt.Sprintf("tl %s:%d", r.Agent.Short, r.Line)
	}
	return fmt.Sprintf("%s/%s:%d", r.Agent.ID, r.File, r.Line)
}

type result struct {
	agents  []*Agent
	records []Record
	files   map[string]int // файлов с событиями в окне по популяции
	errs    []string
}
