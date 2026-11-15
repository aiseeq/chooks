package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
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

// subTasks — суб-агенты, чей файл менялся с начала окна.
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
		var recent []string
		for _, a := range atts {
			if s.recent(a) {
				recent = append(recent, a)
			}
		}
		if len(recent) > 0 {
			tasks = append(tasks, func() { s.scanWorker(dir, recent) })
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
	s.add(a, x.recs, "sub", 1, err)
}

// subScan — разбор одного транскрипта суб-агента.
type subScan struct {
	s       *scanner
	a       *Agent
	recs    []Record
	pending map[string]int // id вызова `sub -F файл` → индекс в recs: цели ждут вывода
	prompt  bool
	errs    []error
}

func (x *subScan) line(n int, line []byte) {
	x.readPrompt(n, line)
	x.readResult(n, line)
	x.readToolUse(n, line)
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

func (s *scanner) scanWorker(dir string, atts []string) {
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
	w := &workerScan{s: s, a: &Agent{Pop: "worker", ID: task, Short: task, Master: master}, dir: j.Dir, created: j.Created}
	keys, err := briefKeys(dir)
	var errs []error
	if err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", task, err))
	}
	w.a.Keys = keys
	sort.Strings(atts)
	for _, att := range atts {
		if err := w.att(att); err != nil {
			errs = append(errs, fmt.Errorf("%s/%s: %w", task, filepath.Base(att), err))
		}
	}
	if err := s.specFiles(w.recs); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", task, err))
	}
	s.add(w.a, w.recs, "worker", len(atts), errors.Join(errs...))
}

// loadJob — job.json задачи; ok=false — задачи нет (нет job.json) или она создана после окна.
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
	s       *scanner
	a       *Agent
	dir     string
	created time.Time
	recs    []Record
	file    string
	seen    map[string]bool
	pending map[string]int // id `sub -F файл` → индекс в recs: цели — в выводе item.completed
	path    string         // att*.jsonl
	thread  string         // thread_id попытки: имя её rollout codex
	occ     map[string]int // сколько раз команда с этим ключом уже встретилась в попытке
	cwds    map[string][]string
	cwdErr  error // rollout не прочитан
}

func (w *workerScan) att(path string) error {
	w.file = filepath.Base(path)
	w.path = path
	w.seen = map[string]bool{}
	w.pending = map[string]int{}
	w.thread, w.occ, w.cwds, w.cwdErr = "", map[string]int{}, nil, nil
	return errors.Join(eachLine(path, w.line), w.cwdErr)
}

// callCwd — каталог вызова из rollout codex попытки: att*.jsonl его не пишет. Вызов в rollout
// находится по тексту команды и номеру её повтора; rollout читается при первой нужде.
func (w *workerScan) callCwd(key string, occ int) (string, bool) {
	if w.cwds == nil && w.cwdErr == nil {
		w.cwds, w.cwdErr = rolloutCwds(filepath.Dir(w.path), w.thread)
		if w.cwds == nil {
			w.cwds = map[string][]string{}
		}
	}
	if list := w.cwds[key]; occ < len(list) {
		return list[occ], true
	}
	return "", false
}

// rolloutCwds — каталоги вызовов оболочки из rollout codex: ключ команды → каталоги по порядку.
// Нет thread_id или файла rollout (старые задачи) — пусто, без ошибки.
func rolloutCwds(taskDir, thread string) (map[string][]string, error) {
	if thread == "" {
		return nil, nil
	}
	files, err := filepath.Glob(filepath.Join(taskDir, "codex", "sessions", "*", "*", "*", "rollout-*-"+thread+".jsonl"))
	if err != nil || len(files) == 0 {
		return nil, err
	}
	out := map[string][]string{}
	err = eachLine(files[0], func(_ int, line []byte) {
		if !bytes.Contains(line, []byte(`"CommandExecution"`)) {
			return
		}
		var r struct {
			Payload struct {
				Type string `json:"type"`
				Item struct {
					Type    string   `json:"type"`
					Command []string `json:"command"`
					Cwd     string   `json:"cwd"`
				} `json:"item"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &r) != nil || r.Payload.Type != "item_completed" || r.Payload.Item.Type != "CommandExecution" {
			return
		}
		key := commandKey(strings.Join(r.Payload.Item.Command, " "))
		out[key] = append(out[key], strings.TrimPrefix(r.Payload.Item.Cwd, "file://"))
	})
	if err != nil {
		return nil, fmt.Errorf("rollout %s: %w", filepath.Base(files[0]), err)
	}
	return out, nil
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

func (w *workerScan) line(n int, line []byte) {
	if bytes.HasPrefix(line, []byte(`{"type":"thread.started"`)) {
		var t struct {
			ID string `json:"thread_id"`
		}
		if json.Unmarshal(line, &t) == nil {
			w.thread = t.ID
		}
		return
	}
	if !bytes.Contains(line, []byte(`"command_execution"`)) && !bytes.Contains(line, []byte(`"file_change"`)) {
		return
	}
	var l cxLine
	if json.Unmarshal(line, &l) != nil || (l.Type != "item.started" && l.Type != "item.completed") {
		return
	}
	if w.seen[l.Item.ID] {
		if i, ok := w.pending[l.Item.ID]; ok && l.Type == "item.completed" {
			w.recs[i].V.ResolveSpec(l.Item.Output, w.s.home, w.dir)
			delete(w.pending, l.Item.ID)
		}
		return
	}
	w.seen[l.Item.ID] = true
	ev, ok := w.event(&l)
	if !ok {
		return
	}
	w.a.Events++
	key, occ := "", 0
	if ev.Tool == codexCommand {
		key = commandKey(ev.Command)
		occ = w.occ[key]
		w.occ[key]++
	}
	v := Classify(ev)
	if !interesting(v) {
		return
	}
	if ev.Tool == codexCommand {
		if cwd, ok := w.callCwd(key, occ); ok {
			ev.Cwd, ev.CallCwd = cwd, true
			v = Classify(ev)
			if !interesting(v) {
				return
			}
		}
	}
	switch {
	case v.SpecFile && l.Type == "item.completed":
		v.ResolveSpec(l.Item.Output, w.s.home, w.dir)
	case v.SpecFile:
		w.pending[l.Item.ID] = len(w.recs)
	}
	w.recs = append(w.recs, Record{Agent: w.a, File: w.file, Line: n, Time: w.created, V: v})
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
