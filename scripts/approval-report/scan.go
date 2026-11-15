package main

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

type scanner struct {
	home     string
	from, to time.Time
	jobs     int
	mu       sync.Mutex
	res      result
	// times — создание и последняя запись файла попытки; nil — fileTimes (тесты подменяют:
	// время создания файла не задать)
	times func(path string) (born, mod time.Time, err error)
}

func (s *scanner) fileTimes(path string) (born, mod time.Time, err error) {
	if s.times != nil {
		return s.times(path)
	}
	return fileTimes(path)
}

func (s *scanner) run(projects, worker string) (result, error) {
	short, err := shortIDs(projects)
	if err != nil {
		return result{}, err
	}
	subs, err := s.subTasks(projects, short)
	if err != nil {
		return result{}, err
	}
	workers, err := s.workerTasks(worker)
	if err != nil {
		return result{}, err
	}
	s.runTasks(append(subs, workers...))
	return s.collect(), nil
}

// subTasks — суб-агенты, чей файл менялся с начала окна: в окне ли сами записи, решает их время.
func (s *scanner) subTasks(projects string, short map[string]string) ([]func(), error) {
	subs, err := filepath.Glob(filepath.Join(projects, "*", "*", "subagents", "agent-*.jsonl"))
	if err != nil {
		return nil, fmt.Errorf("поиск транскриптов суб-агентов: %w", err)
	}
	names := &parentNames{m: map[string]string{}}
	var tasks []func()
	for _, f := range subs {
		if s.recent(f) {
			tasks = append(tasks, func() { s.scanSub(f, short, names) })
		}
	}
	return tasks, nil
}

// workerTasks — задачи бойцов, у которых есть попытка, менявшаяся с начала окна.
func (s *scanner) workerTasks(worker string) ([]func(), error) {
	dirs, err := os.ReadDir(worker)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("каталог worker: %w", err)
	}
	var tasks []func()
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		dir := filepath.Join(worker, d.Name())
		atts, err := filepath.Glob(filepath.Join(dir, "att*.jsonl"))
		if err != nil {
			return nil, fmt.Errorf("поиск попыток %s: %w", d.Name(), err)
		}
		recent := map[string]bool{}
		for _, a := range atts {
			if s.recent(a) {
				recent[a] = true
			}
		}
		if len(recent) > 0 {
			tasks = append(tasks, func() { s.scanWorker(dir, atts, recent) })
		}
	}
	return tasks, nil
}

func (s *scanner) runTasks(tasks []func()) {
	sem := make(chan struct{}, s.jobs)
	var wg sync.WaitGroup
	for _, t := range tasks {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			t()
		}()
	}
	wg.Wait()
}

// collect — итог под замком: записи по времени и ссылке, агенты по id.
func (s *scanner) collect() result {
	s.mu.Lock()
	defer s.mu.Unlock()
	res := s.res
	res.records = append([]Record(nil), s.res.records...)
	res.agents = append([]*Agent(nil), s.res.agents...)
	sort.Slice(res.records, func(i, j int) bool {
		a, b := res.records[i], res.records[j]
		if !a.Time.Equal(b.Time) {
			return a.Time.Before(b.Time)
		}
		return a.Ref() < b.Ref()
	})
	sort.Slice(res.agents, func(i, j int) bool { return res.agents[i].ID < res.agents[j].ID })
	if res.files == nil {
		res.files = map[string]int{}
	}
	return res
}

func (s *scanner) recent(f string) bool {
	st, err := os.Stat(f)
	return err == nil && !st.ModTime().Before(s.from)
}

func (s *scanner) add(a *Agent, recs []Record, pop string, files int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.res.files == nil {
		s.res.files = map[string]int{}
	}
	s.res.agents = append(s.res.agents, a)
	s.res.records = append(s.res.records, recs...)
	s.res.files[pop] += files
	if err != nil {
		s.res.errs = append(s.res.errs, err.Error())
	}
}

// specFiles — цели `sub -F файл`, не видные в выводе, из файла спецификации; файла уже нет —
// событие остаётся в «не видно разбором».
func (s *scanner) specFiles(recs []Record) error {
	var errs []error
	for i := range recs {
		v := &recs[i].V
		if !v.SpecFile || v.Spec == nil || v.Spec.Path == "" {
			continue
		}
		body, err := os.ReadFile(v.Spec.Path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("строка %d, файл спецификации: %w", recs[i].Line, err))
			continue
		}
		v.ResolveSpecFile(string(body), s.home)
	}
	return errors.Join(errs...)
}

// eachLine читает файл потоком: строки любой длины, номер с 1.
func eachLine(path string, fn func(n int, line []byte)) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("открыть: %w", err)
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)
	var buf []byte
	for n := 1; ; n++ {
		buf = buf[:0]
		for {
			part, err := r.ReadSlice('\n')
			buf = append(buf, part...)
			if errors.Is(err, bufio.ErrBufferFull) {
				continue
			}
			if err != nil {
				if len(buf) > 0 {
					fn(n, buf)
				}
				if errors.Is(err, io.EOF) {
					return nil
				}
				return fmt.Errorf("строка %d: %w", n, err)
			}
			break
		}
		fn(n, bytes.TrimRight(buf, "\n"))
	}
}

type ccLine struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	Cwd       string `json:"cwd"`
	Message   struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type ccPart struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	Name      string          `json:"name"`
	Text      string          `json:"text"`
	Input     struct {
		Command      string `json:"command"`
		FilePath     string `json:"file_path"`
		NotebookPath string `json:"notebook_path"`
	} `json:"input"`
}

var ccTools = map[string]bool{"Bash": true, "Edit": true, "Write": true, "MultiEdit": true, "NotebookEdit": true}

func (s *scanner) scanSub(path string, short map[string]string, names *parentNames) {
	id := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), "agent-"), ".jsonl")
	parentDir := filepath.Dir(filepath.Dir(path))
	parentID := filepath.Base(parentDir)
	x := &subScan{s: s, pending: map[string]int{}}
	master, err := names.get(parentDir + ".jsonl")
	if err != nil {
		x.errs = append(x.errs, err)
	}
	if master == "" {
		master = parentID[:min(8, len(parentID))]
	}
	x.a = &Agent{Pop: "sub", ID: id, Short: short[id], Master: master, Keys: map[string]bool{}}
	if x.a.Short == "" {
		x.a.Short = id[:min(8, len(id))]
	}
	if err := eachLine(path, x.line); err != nil {
		x.errs = append(x.errs, err)
	}
	if err := s.specFiles(x.recs); err != nil {
		x.errs = append(x.errs, err)
	}
	a := x.a
	if a.Events == 0 {
		a = &Agent{Pop: "sub-empty", ID: id}
	}
	err = errors.Join(x.errs...)
	if err != nil {
		err = fmt.Errorf("%s: %w", path, err)
	}
	files := 0
	if x.inWindow {
		files = 1
	}
	s.add(a, x.recs, "sub", files, err)
}

// subScan — разбор одного транскрипта суб-агента.
type subScan struct {
	s        *scanner
	a        *Agent
	recs     []Record
	pending  map[string]int // id вызова `sub -F файл` → индекс в recs: цели ждут вывода
	prompt   bool
	inWindow bool // есть запись со временем в окне: файл входит в число «файлов»
	errs     []error
}

func (x *subScan) line(n int, line []byte) {
	x.readTime(line)
	x.readPrompt(n, line)
	x.readResult(n, line)
	x.readToolUse(n, line)
}

// readTime — время записи, пока в окне не нашлось ни одной: файл, менявшийся после окна,
// без записей в нём в «файлов» не входит.
func (x *subScan) readTime(line []byte) {
	if x.inWindow || !bytes.Contains(line, []byte(`"timestamp"`)) {
		return
	}
	var l struct {
		Timestamp string `json:"timestamp"`
	}
	if json.Unmarshal(line, &l) != nil {
		return
	}
	ts, err := time.Parse(time.RFC3339Nano, l.Timestamp)
	x.inWindow = err == nil && !ts.Before(x.s.from) && !ts.After(x.s.to)
}

// readPrompt — ключи задач из первого сообщения user (задание суб-агенту).
func (x *subScan) readPrompt(n int, line []byte) {
	if x.prompt || !bytes.Contains(line, []byte(`"type":"user"`)) {
		return
	}
	var l ccLine
	if json.Unmarshal(line, &l) != nil || l.Type != "user" {
		return
	}
	x.prompt = true
	t, err := contentText(l.Message.Content)
	if err != nil {
		x.errs = append(x.errs, fmt.Errorf("строка %d, задание: %w", n, err))
	}
	x.a.Keys = issueKeys(t)
}

// readResult — вывод `sub -F файл`: по нему определяются цели правки.
func (x *subScan) readResult(n int, line []byte) {
	if len(x.pending) == 0 || !bytes.Contains(line, []byte(`"tool_result"`)) {
		return
	}
	var l ccLine
	var parts []ccPart
	if json.Unmarshal(line, &l) != nil || l.Type != "user" || json.Unmarshal(l.Message.Content, &parts) != nil {
		return
	}
	for i := range parts {
		p := &parts[i]
		idx, ok := x.pending[p.ToolUseID]
		if !ok || p.Type != "tool_result" {
			continue
		}
		t, err := contentText(p.Content)
		if err != nil {
			x.errs = append(x.errs, fmt.Errorf("строка %d, вывод: %w", n, err))
		}
		x.recs[idx].V.ResolveSpec(t, x.s.home, l.Cwd)
		delete(x.pending, p.ToolUseID)
	}
}

func (x *subScan) readToolUse(n int, line []byte) {
	if !bytes.Contains(line, []byte(`"tool_use"`)) {
		return
	}
	var l ccLine
	if json.Unmarshal(line, &l) != nil || l.Type != "assistant" {
		return
	}
	ts, err := time.Parse(time.RFC3339Nano, l.Timestamp)
	if err != nil || ts.Before(x.s.from) || ts.After(x.s.to) {
		return
	}
	var parts []ccPart
	if json.Unmarshal(l.Message.Content, &parts) != nil {
		return
	}
	for i := range parts {
		x.toolUse(n, ts, l.Cwd, &parts[i])
	}
}

func (x *subScan) toolUse(n int, ts time.Time, cwd string, p *ccPart) {
	if p.Type != "tool_use" || !ccTools[p.Name] {
		return
	}
	x.a.Events++
	ev := Event{Tool: p.Name, Command: p.Input.Command, Cwd: cwd, Home: x.s.home}
	for _, fp := range []string{p.Input.FilePath, p.Input.NotebookPath} {
		if fp != "" {
			ev.Paths = append(ev.Paths, fp)
		}
	}
	v := Classify(ev)
	if !interesting(v) {
		return
	}
	if v.SpecFile {
		x.pending[p.ID] = len(x.recs)
	}
	x.recs = append(x.recs, Record{Agent: x.a, Line: n, Time: ts, V: v})
}

func interesting(v Verdict) bool {
	return len(v.Hits) > 0 || len(v.Possible) > 0 || v.SpecFile || v.Mentioned[1] || v.Mentioned[2] || v.Mentioned[3] || v.Mentioned[4]
}

// contentText — текст content: строка или список частей с полем text; пустой content — пустой текст.
func contentText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil
	}
	var parts []ccPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", fmt.Errorf("content не строка и не список частей: %w", err)
	}
	var b strings.Builder
	for i := range parts {
		b.WriteString(parts[i].Text)
	}
	return b.String(), nil
}

type cxLine struct {
	Type string `json:"type"`
	Item struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		Command string `json:"command"`
		Output  string `json:"aggregated_output"`
		Changes []struct {
			Path string `json:"path"`
		} `json:"changes"`
	} `json:"item"`
}

type job struct {
	Master  string    `json:"master"`
	Dir     string    `json:"dir"`
	Task    string    `json:"task"`
	Created time.Time `json:"created"`
	Owner   struct {
		Name string `json:"name"`
	} `json:"owner"`
}

// scanWorker — задача бойца: попытки по номеру (att2 раньше att10); попытки вне окна только
// считают повторы команд — все попытки пишут в один rollout codex.
func (s *scanner) scanWorker(dir string, atts []string, recent map[string]bool) {
	task := filepath.Base(dir)
	j, ok, err := s.loadJob(dir)
	if err != nil {
		s.add(&Agent{Pop: "worker-skip", ID: task}, nil, "worker", 0, err)
		return
	}
	if !ok {
		return
	}
	master := j.Master
	if master == "" {
		master = j.Owner.Name
	}
	w := &workerScan{s: s, a: &Agent{Pop: "worker", ID: task, Short: task, Master: master}, dir: j.Dir,
		next: map[string]map[string]int{}, rollouts: map[string]*rollout{}}
	keys, err := briefKeys(dir)
	var errs []error
	if err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", task, err))
	}
	w.a.Keys = keys
	num := map[string]int{}
	for _, a := range atts {
		n, err := attNum(a)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", task, err))
			n = math.MaxInt // без номера — в конец: порядок повторов в rollout для неё неизвестен
		}
		num[a] = n
	}
	slices.SortStableFunc(atts, func(a, b string) int { return cmp.Compare(num[a], num[b]) })
	for _, att := range atts {
		if err := w.att(att, !recent[att]); err != nil {
			errs = append(errs, fmt.Errorf("%s/%s: %w", task, filepath.Base(att), err))
		}
	}
	if err := s.specFiles(w.recs); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", task, err))
	}
	s.add(w.a, w.recs, "worker", w.files, errors.Join(errs...))
}

// loadJob — job.json задачи; ok=false — задачи нет (нет job.json) или она создана после окна.
// Задача, созданная после окна, вызовов в нём не имеет: время вызова не нужно.
func (s *scanner) loadJob(dir string) (j job, ok bool, err error) {
	raw, err := os.ReadFile(filepath.Join(dir, "job.json"))
	if os.IsNotExist(err) {
		return j, false, nil
	}
	if err != nil {
		return j, false, fmt.Errorf("%s/job.json: %w", filepath.Base(dir), err)
	}
	if err := json.Unmarshal(raw, &j); err != nil {
		return j, false, fmt.Errorf("%s/job.json: %w", filepath.Base(dir), err)
	}
	return j, j.Created.IsZero() || !j.Created.After(s.to), nil
}

// briefKeys — ключи задач из брифов бойца.
func briefKeys(dir string) (map[string]bool, error) {
	briefs, err := filepath.Glob(filepath.Join(dir, "brief*.md"))
	if err != nil {
		return issueKeys(""), fmt.Errorf("поиск брифов: %w", err)
	}
	var brief strings.Builder
	var errs []error
	for _, b := range briefs {
		t, err := os.ReadFile(b)
		if err != nil {
			errs = append(errs, fmt.Errorf("бриф: %w", err))
			continue
		}
		brief.Write(t)
	}
	return issueKeys(brief.String()), errors.Join(errs...)
}

// workerScan — разбор попыток бойца; записи копятся по всем попыткам задачи.
type workerScan struct {
	s      *scanner
	a      *Agent
	dir    string
	recs   []Record
	files  int // попыток с вызовами в окне; попытка без вызовов — по времени файла
	file   string
	path   string         // att*.jsonl
	items  []attItem      // вызовы попытки по порядку
	seen   map[string]int // id item → индекс в items
	thread string         // thread_id попытки: имя её rollout codex
	// next — по thread: ключ вызова → первый ещё не сопоставленный вызов rollout с этим ключом
	next     map[string]map[string]int
	rollouts map[string]*rollout // по thread
	callErr  error               // rollout не прочитан
}

// attItem — вызов попытки: первая строка о нём и вывод из item.completed.
type attItem struct {
	n      int
	ev     Event
	done   bool // есть item.completed: только такой вызов есть в rollout
	output string
	thread string
}

// call — вызов из rollout codex: каталог (у команды) и время начала; нет времени — нулевое.
type call struct {
	cwd string
	at  time.Time
}

// attNum — номер попытки из имени attN.jsonl.
func attNum(path string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), "att"), ".jsonl"))
	if err != nil {
		return 0, fmt.Errorf("номер попытки %s: %w", filepath.Base(path), err)
	}
	return n, nil
}

// att — попытка: сначала все её вызовы, потом время каждого — вызову без времени нужны соседи.
func (w *workerScan) att(path string, countOnly bool) error {
	w.file = filepath.Base(path)
	w.path = path
	w.items, w.seen = nil, map[string]int{}
	w.thread, w.callErr = "", nil
	err := eachLine(path, w.line)
	born, mod, terr := w.s.fileTimes(path)
	calls, found := w.lookup(born, mod)
	if countOnly {
		return errors.Join(err, w.callErr, terr)
	}
	in := w.inWindow(calls, born, mod)
	// попытка без вызовов времени не имеет: в окне, если менялась с его начала (прежнее правило)
	if slices.Contains(in, true) || len(w.items) == 0 {
		w.files++
	}
	for i := range w.items {
		if in[i] {
			w.record(&w.items[i], calls[i], found[i])
		}
	}
	return errors.Join(err, w.callErr, terr)
}

// fileSlack — запас на то, что время файла попытки и время записи rollout пишутся не разом.
const fileSlack = 5 * time.Second

// lookup — вызовы попытки в rollout: по ключу и порядку в thread, начатые не раньше создания файла
// попытки и не позже последней записи в него. Вызов rollout, начатый до создания файла, —
// вызов прежней попытки, которого её att*.jsonl не записал, — пропускается. Незаконченный в att
// вызов (item.started без item.completed) rollout то пишет, то нет (у оборванной попытки и посреди
// неё бывает и так, и так): он берёт вызов rollout, только если таких вызовов во времени попытки
// больше, чем законченных вызовов с тем же ключом после него, — иначе вызов достаётся им.
func (w *workerScan) lookup(born, mod time.Time) ([]call, []bool) {
	n := len(w.items)
	calls, found := make([]call, n), make([]bool, n)
	keys, later := make([]string, n), make([]int, n)
	done := map[[2]string]int{} // thread и ключ → законченных вызовов попытки дальше по файлу
	for i := n - 1; i >= 0; i-- {
		it := &w.items[i]
		keys[i] = eventKey(it.ev)
		k := [2]string{it.thread, keys[i]}
		later[i] = done[k]
		if it.done {
			done[k]++
		}
	}
	for i := range w.items {
		it := &w.items[i]
		list, p := w.candidates(it.thread, keys[i], born, mod)
		if len(list) == 0 || (!it.done && len(list) <= later[i]) {
			continue
		}
		calls[i], found[i] = list[0], true
		w.next[it.thread][keys[i]] = p + 1
	}
	return calls, found
}

// inWindow — в окне ли вызовы попытки. Вызов со временем из rollout — по нему. Вызов без времени
// ограничивают соседи по попытке: начат не раньше предыдущего вызова со временем и создания файла
// попытки, не позже следующего вызова со временем, а без него — последней записи в файл. Границы не
// решают — в окне по прежнему правилу (задача создана до конца окна, попытка менялась с его начала).
func (w *workerScan) inWindow(calls []call, born, mod time.Time) []bool {
	from, to := w.s.from, w.s.to
	in := make([]bool, len(calls))
	next := make([]time.Time, len(calls))
	hi := mod
	for i := len(calls) - 1; i >= 0; i-- {
		next[i] = hi
		if !calls[i].at.IsZero() {
			hi = calls[i].at
		}
	}
	lo := born
	for i, c := range calls {
		if !c.at.IsZero() {
			in[i] = !c.at.Before(from) && !c.at.After(to)
			if c.at.After(lo) {
				lo = c.at
			}
			continue
		}
		in[i] = !lo.After(to) && !next[i].Before(from)
		if in[i] {
			w.a.Untimed++
		}
	}
	return in
}

// eventKey — ключ вызова: текст команды или пути правки файлов.
func eventKey(ev Event) string {
	if ev.Tool == codexCommand {
		return "c" + commandKey(ev.Command)
	}
	paths := slices.Sorted(slices.Values(ev.Paths))
	return "f" + strings.Join(paths, "\n")
}

// candidates — ещё не сопоставленные вызовы rollout с ключом, начатые во время попытки (lookup),
// и номер первого из них: att*.jsonl не пишет ни каталога, ни времени вызова. Rollout читается при
// первой нужде; без времени в rollout — все по порядку.
func (w *workerScan) candidates(thread, key string, born, mod time.Time) ([]call, int) {
	r, ok := w.rollouts[thread]
	if !ok {
		var err error
		r, err = rolloutCalls(filepath.Dir(w.path), thread)
		w.callErr = errors.Join(w.callErr, err)
		w.rollouts[thread] = r
		w.next[thread] = map[string]int{}
	}
	list := r.list(key)
	p := w.next[thread][key]
	for p < len(list) && !list[p].at.IsZero() && list[p].at.Before(born.Add(-fileSlack)) {
		p++
	}
	q := p
	for q < len(list) && !list[q].at.After(mod.Add(fileSlack)) {
		q++
	}
	return list[p:q], p
}

// rollout — вызовы rollout codex одного thread.
type rollout struct {
	byKey map[string][]call // ключ вызова → вызовы по порядку
	order []keyedCall       // все вызовы по порядку: для команд с маской секрета
}

type keyedCall struct {
	key string
	c   call
}

// redactMark — чем codex заменяет в att*.jsonl текст, похожий на секрет; в rollout текст открыт.
const redactMark = "[REDACTED_SECRET]"

// list — вызовы с ключом; ключ с маской секрета подходит к ключам rollout, где на месте маски
// любой текст.
func (r *rollout) list(key string) []call {
	if !strings.Contains(key, redactMark) {
		return r.byKey[key]
	}
	parts := strings.Split(key, redactMark)
	var l []call
	for _, kc := range r.order {
		if maskMatch(parts, kc.key) {
			l = append(l, kc.c)
		}
	}
	return l
}

// maskMatch — s из частей parts по порядку с любым текстом между ними.
func maskMatch(parts []string, s string) bool {
	first, last := parts[0], parts[len(parts)-1]
	if len(s) < len(first)+len(last) || !strings.HasPrefix(s, first) || !strings.HasSuffix(s, last) {
		return false
	}
	s = s[len(first) : len(s)-len(last)]
	for _, p := range parts[1 : len(parts)-1] {
		i := strings.Index(s, p)
		if i < 0 {
			return false
		}
		s = s[i+len(p):]
	}
	return true
}

// rolloutCalls — вызовы оболочки и правки файлов из rollout codex по порядку.
// Нет файла rollout (старые задачи, попытка без thread_id) — пусто, без ошибки.
func rolloutCalls(taskDir, thread string) (*rollout, error) {
	r := &rollout{byKey: map[string][]call{}}
	files, err := filepath.Glob(filepath.Join(taskDir, "codex", "sessions", "*", "*", "*", "rollout-*-"+thread+".jsonl"))
	if err != nil {
		return r, fmt.Errorf("поиск rollout: %w", err)
	}
	if len(files) == 0 {
		return r, nil
	}
	var errs []error
	err = eachLine(files[0], func(n int, line []byte) {
		key, c, ok, err := rolloutCall(line)
		if err != nil {
			errs = append(errs, fmt.Errorf("строка %d: %w", n, err))
		}
		if ok {
			r.byKey[key] = append(r.byKey[key], c)
			r.order = append(r.order, keyedCall{key, c})
		}
	})
	if err := errors.Join(append(errs, err)...); err != nil {
		return r, fmt.Errorf("rollout %s: %w", filepath.Base(files[0]), err)
	}
	return r, nil
}

// rolloutCall — вызов из строки rollout. Время начала — время записи о завершении минус
// длительность команды; у записи без времени (старый формат) — нулевое, время не разобрано —
// нулевое и ошибка.
func rolloutCall(line []byte) (key string, c call, ok bool, err error) {
	if !bytes.Contains(line, []byte(`"CommandExecution"`)) && !bytes.Contains(line, []byte(`"FileChange"`)) {
		return "", call{}, false, nil
	}
	var r struct {
		Timestamp string `json:"timestamp"`
		Payload   struct {
			Type string `json:"type"`
			Item struct {
				Type     string                     `json:"type"`
				Command  []string                   `json:"command"`
				Cwd      string                     `json:"cwd"`
				Changes  map[string]json.RawMessage `json:"changes"`
				Duration struct {
					Secs  int64 `json:"secs"`
					Nanos int64 `json:"nanos"`
				} `json:"duration"`
			} `json:"item"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &r) != nil || r.Payload.Type != "item_completed" {
		return "", call{}, false, nil
	}
	it := &r.Payload.Item
	switch it.Type {
	case "CommandExecution":
		c.cwd = strings.TrimPrefix(it.Cwd, "file://")
		if dec, err := url.PathUnescape(c.cwd); err == nil {
			c.cwd = dec
		}
		key = eventKey(Event{Tool: codexCommand, Command: strings.Join(it.Command, " ")})
	case "FileChange":
		key = eventKey(Event{Tool: "file_change", Paths: slices.Collect(maps.Keys(it.Changes))})
	default:
		return "", call{}, false, nil
	}
	if r.Timestamp == "" {
		return key, c, true, nil
	}
	ts, err := time.Parse(time.RFC3339Nano, r.Timestamp)
	if err != nil {
		return key, c, true, fmt.Errorf("время вызова: %w", err)
	}
	c.at = ts.Add(-time.Duration(it.Duration.Secs)*time.Second - time.Duration(it.Duration.Nanos))
	return key, c, true, nil
}

// commandKey — текст команды без кавычек, обратных косых и пробелов: att*.jsonl хранит argv
// строкой в кавычках codex, rollout — массивом.
func commandKey(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\'' || r == '"' || r == '\\' || unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}

// line — строка попытки: thread_id или вызов; повтор того же item отмечает завершение и вывод.
func (w *workerScan) line(n int, line []byte) {
	if w.readThread(line) {
		return
	}
	if !bytes.Contains(line, []byte(`"command_execution"`)) && !bytes.Contains(line, []byte(`"file_change"`)) {
		return
	}
	var l cxLine
	if json.Unmarshal(line, &l) != nil || (l.Type != "item.started" && l.Type != "item.completed") {
		return
	}
	done := l.Type == "item.completed"
	if i, ok := w.seen[l.Item.ID]; ok {
		if done {
			w.items[i].done, w.items[i].output = true, l.Item.Output
		}
		return
	}
	ev, ok := w.event(&l)
	if !ok {
		return
	}
	w.seen[l.Item.ID] = len(w.items)
	w.items = append(w.items, attItem{n: n, ev: ev, done: done, output: l.Item.Output, thread: w.thread})
}

// readThread — thread_id попытки из thread.started; true — строка разобрана.
func (w *workerScan) readThread(line []byte) bool {
	if !bytes.HasPrefix(line, []byte(`{"type":"thread.started"`)) {
		return false
	}
	var t struct {
		ID string `json:"thread_id"`
	}
	if json.Unmarshal(line, &t) == nil {
		w.thread = t.ID
	}
	return true
}

// record — вызов в окне: в счёт вызовов, с признаком класса — запись. Каталог вызова — из
// rollout; цели `sub -F файл` — из вывода item.completed.
func (w *workerScan) record(it *attItem, c call, found bool) {
	w.a.Events++
	ev := it.ev
	v := Classify(ev)
	if !interesting(v) {
		return
	}
	if ev.Tool == codexCommand && found {
		ev.Cwd, ev.CallCwd = c.cwd, true
		v = Classify(ev)
		if !interesting(v) {
			return
		}
	}
	v.ResolveSpec(it.output, w.s.home, w.dir)
	w.recs = append(w.recs, Record{Agent: w.a, File: w.file, Line: it.n, Time: c.at, V: v})
}

// event — команда или правка файлов; прочие item не считаются вызовами.
func (w *workerScan) event(l *cxLine) (Event, bool) {
	ev := Event{Tool: l.Item.Type, Command: l.Item.Command, Cwd: w.dir, Home: w.s.home}
	switch l.Item.Type {
	case codexCommand:
	case "file_change":
		for _, c := range l.Item.Changes {
			ev.Paths = append(ev.Paths, c.Path)
		}
	default:
		return ev, false
	}
	return ev, true
}

// parentNames — имя мастера родительской сессии, как его показывает tl: последняя запись agent-name.
type parentNames struct {
	mu sync.Mutex
	m  map[string]string
}

// get — имя по транскрипту родителя; "" — имени нет. Ошибка чтения возвращается один раз, дальше — "" из кэша.
func (p *parentNames) get(path string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n, ok := p.m[path]; ok {
		return n, nil
	}
	n, err := readAgentName(path)
	p.m[path] = n
	return n, err
}

// readAgentName — имя из хвоста 16 МБ, нет там — из всего файла; файла нет — "" без ошибки.
func readAgentName(path string) (string, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("транскрипт родителя: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("транскрипт родителя %s: %w", path, err)
	}
	const tail = 16 << 20
	if st.Size() > tail {
		name, err := lastName(io.NewSectionReader(f, st.Size()-tail, tail))
		if err != nil || name != "" {
			return name, err
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return "", fmt.Errorf("транскрипт родителя %s: %w", path, err)
		}
	}
	return lastName(f)
}

// lastName — последнее agent-name, без него — последнее custom-title.
func lastName(r io.Reader) (string, error) {
	br := bufio.NewReaderSize(r, 1<<20)
	name, title := "", ""
	for {
		line, err := br.ReadSlice('\n')
		nameLine(line, &name, &title)
		for errors.Is(err, bufio.ErrBufferFull) {
			_, err = br.ReadSlice('\n')
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("чтение имени мастера: %w", err)
		}
	}
	if name != "" {
		return name, nil
	}
	return title, nil
}

func nameLine(line []byte, name, title *string) {
	if !bytes.HasPrefix(line, []byte(`{"type":"agent-name"`)) && !bytes.HasPrefix(line, []byte(`{"type":"custom-title"`)) {
		return
	}
	var l struct {
		AgentName   string `json:"agentName"`
		CustomTitle string `json:"customTitle"`
	}
	if json.Unmarshal(line, &l) != nil {
		return
	}
	if l.AgentName != "" {
		*name = l.AgentName
	}
	if l.CustomTitle != "" {
		*title = l.CustomTitle
	}
}

// shortIDs — id в ссылке tl: первые 8 знаков, длиннее, если префикс не однозначен среди всех транскриптов.
func shortIDs(projects string) (map[string]string, error) {
	mains, err := filepath.Glob(filepath.Join(projects, "*", "*.jsonl"))
	if err != nil {
		return nil, fmt.Errorf("поиск транскриптов сессий: %w", err)
	}
	subs, err := filepath.Glob(filepath.Join(projects, "*", "*", "subagents", "agent-*.jsonl"))
	if err != nil {
		return nil, fmt.Errorf("поиск транскриптов суб-агентов: %w", err)
	}
	ids := make([]string, 0, len(mains)+len(subs))
	for _, m := range mains {
		ids = append(ids, strings.TrimSuffix(filepath.Base(m), ".jsonl"))
	}
	for _, s := range subs {
		ids = append(ids, strings.TrimSuffix(strings.TrimPrefix(filepath.Base(s), "agent-"), ".jsonl"))
	}
	sort.Strings(ids)
	out := map[string]string{}
	for i, id := range ids {
		l := 8
		for _, nb := range []int{i - 1, i + 1} {
			if nb >= 0 && nb < len(ids) && ids[nb] != id {
				l = max(l, commonPrefix(id, ids[nb])+1)
			}
		}
		out[id] = id[:min(l, len(id))]
	}
	return out, nil
}

func commonPrefix(a, b string) int {
	c := 0
	for c < len(a) && c < len(b) && a[c] == b[c] {
		c++
	}
	return c
}
