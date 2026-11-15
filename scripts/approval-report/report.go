package main

import (
	"fmt"
	"io"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// listAll — печатать все попадания, не только примеры (флаг --list).
var listAll bool

var classNames = [5]string{"", "запись в инструкции и настройки", "коммит и push конфиг-репо", "выкатка graft deploy", "запись в Jira и Bitbucket"}

func report(w io.Writer, res result, from, to time.Time) {
	loc := berlin()
	fmt.Fprintf(w, "Окно: %s — %s\n", from.In(loc).Format("02.01.2006 15:04 MST"), to.In(loc).Format("02.01.2006 15:04 MST"))
	for _, pop := range []string{"sub", "worker"} {
		var agents []*Agent
		events := 0
		for _, a := range res.agents {
			if a.Pop == pop && a.Events > 0 {
				agents = append(agents, a)
				events += a.Events
			}
		}
		var recs []Record
		for _, r := range res.records {
			if r.Agent.Pop == pop {
				recs = append(recs, r)
			}
		}
		title := "Суб-агенты Claude (и форки), основная сессия не входит"
		if pop == "worker" {
			title = "Бойцы worker"
		}
		fmt.Fprintf(w, "\n== %s: %d агентов с вызовами в окне, файлов %d, вызовов инструментов %d\n", title, len(agents), res.files[pop], events)
		popReport(w, recs)
	}
	if len(res.errs) > 0 {
		fmt.Fprintf(w, "\nОшибки чтения: %d\n", len(res.errs))
		for _, e := range res.errs[:min(5, len(res.errs))] {
			fmt.Fprintln(w, "  ", e)
		}
	}
}

func popReport(w io.Writer, recs []Record) {
	totalCand, totalOut := 0, 0
	for c := 1; c <= 4; c++ {
		cand, out := classSection(w, recs, c)
		totalCand += cand
		totalOut += out
	}
	fmt.Fprintf(w, "\nВсего: кандидатов %d, отсеяно как чтение или копия вне ~/.claude %d (%s)\n", totalCand, totalOut, pct(totalOut, totalCand))
	popNotes(w, recs)
}

// classSection печатает строку «Класс N» и разбивку попаданий; возвращает кандидатов и отсеянных.
func classSection(w io.Writer, recs []Record, c int) (cand, out int) {
	var hits []Record
	agents := map[string]bool{}
	for _, r := range recs {
		if !r.V.Mentioned[c] {
			continue
		}
		cand++
		if r.V.HitClass(c) {
			hits = append(hits, r)
			agents[r.Agent.ID] = true
		}
	}
	out = cand - len(hits)
	fmt.Fprintf(w, "\nКласс %d (%s): %d событий у %d агентов; кандидатов по тексту %d, отсеяно %d (%s)\n",
		c, classNames[c], len(hits), len(agents), cand, out, pct(out, cand))
	if len(hits) > 0 {
		classDetails(w, hits, c)
	}
	return cand, out
}

func classDetails(w io.Writer, hits []Record, c int) {
	fmt.Fprintf(w, "  по мастеру: %s\n", byMaster(hits))
	verb := func(_ Record, h Hit) string { return h.Verb }
	switch c {
	case 1, 2:
		fmt.Fprintf(w, "  чем: %s\n", countHits(hits, c, verb))
	case 3:
		fmt.Fprintf(w, "  цель: %s\n", countHits(hits, c, func(_ Record, h Hit) string { return h.Prod }))
		fmt.Fprintf(w, "  действие: %s\n", countHits(hits, c, func(_ Record, h Hit) string { return h.Verb + " " + h.Prod }))
	case 4:
		fmt.Fprintf(w, "  вид: %s\n", countHits(hits, c, func(_ Record, h Hit) string { return h.Kind }))
		own := countHits(hits, c, func(r Record, h Hit) string {
			if (h.Kind == "comment" || h.Kind == "transition") && r.Agent.Keys[h.Key] {
				return h.Kind + " своей задачи"
			}
			return ""
		})
		fmt.Fprintf(w, "  итоговый комментарий о своей работе и закрытие своей задачи (ключ из задания агента): %s\n", own)
	}
	fmt.Fprintln(w, "  примеры:")
	for _, r := range examples(hits) {
		fmt.Fprintf(w, "    %s — %s\n", r.Ref(), describe(r, c))
	}
	if listAll {
		fmt.Fprintln(w, "  все:")
		for _, r := range hits {
			fmt.Fprintf(w, "    %s — %s\n", r.Ref(), describe(r, c))
		}
	}
}

// popNotes — то, что разбор не видит: неразобранная оболочка, `sub -F` без цели, запись из
// python/node, выкатка в пробе.
func popNotes(w io.Writer, recs []Record) {
	unparsed, varsUnparsed, specProt, specBody := 0, 0, 0, 0
	var spec, possible, probe []Record
	for _, r := range recs {
		if r.V.Unparsed {
			unparsed++
		}
		if r.V.VarsUnparsed {
			varsUnparsed++
		}
		if r.V.SpecFile {
			spec = append(spec, r)
		}
		if r.V.SpecFile && r.V.Mentioned[1] && !r.V.HitClass(1) {
			specProt++
		}
		if slices.ContainsFunc(r.V.Hits, func(h Hit) bool { return h.Verb == specBodyVerb }) {
			specBody++
		}
		switch ps := r.V.Unhit(); {
		case len(ps) == 0:
		case ps[0].Class == 3:
			probe = append(probe, r)
		default:
			possible = append(possible, r)
		}
	}
	fmt.Fprintf(w, "Не видно разбором: оболочка не разобрала %d кандидатов (проверены регулярками), переменные вложенного скрипта не разобраны у %d; `sub -F файл`, цели которого нет ни в выводе (не дошёл или обрезан), ни в файле спецификации (файла уже нет или путь в переменной) — %d, из них с путём класса 1 в тексте и без попадания %d\n", unparsed, varsUnparsed, len(spec), specProt)
	noteList(w, spec, func(r Record) string { return "файл " + specName(r.V.Spec) })
	if specBody > 0 {
		fmt.Fprintf(w, "`sub -F файл` с целями из файла спецификации (вывод их не показал): %d — цели по нынешнему содержимому файла, после вызова его могли переписать\n", specBody)
	}
	if len(possible) > 0 {
		fmt.Fprintf(w, "Возможные записи из кода python/node (эвристика, в числа классов не входят): %d\n", len(possible))
		noteList(w, possible, func(r Record) string {
			h := r.V.Unhit()[0]
			return fmt.Sprintf("класс %d?, %s, %s", h.Class, h.Verb, h.Target)
		})
	}
	if len(probe) > 0 {
		fmt.Fprintf(w, "Выкатка в пробе с временным XDG_CONFIG_HOME (в класс 3 не входит, в его «отсеяно» входит): %d\n", len(probe))
		noteList(w, probe, func(r Record) string {
			h := r.V.Unhit()[0]
			return h.Verb + " " + h.Target + ", " + h.Prod + ", мастер " + r.Agent.Master
		})
	}
}

// noteList печатает события примечания: 3 первых, с --list — все.
func noteList(w io.Writer, recs []Record, line func(Record) string) {
	if !listAll {
		recs = recs[:min(3, len(recs))]
	}
	for _, r := range recs {
		fmt.Fprintf(w, "    %s — %s\n", r.Ref(), line(r))
	}
}

func specName(s *SpecRef) string {
	if s == nil || s.Path == "" {
		return "в переменной"
	}
	return s.Path
}

func pct(a, b int) string {
	if b == 0 {
		return "—"
	}
	return strconv.FormatFloat(100*float64(a)/float64(b), 'f', 1, 64) + "%"
}

func byMaster(recs []Record) string {
	m := map[string]int{}
	for _, r := range recs {
		m[r.Agent.Master]++
	}
	return joinCounts(m)
}

// countHits считает события по ключу: событие с несколькими попаданиями одного ключа — одно.
func countHits(recs []Record, c int, key func(Record, Hit) string) string {
	m := map[string]int{}
	for _, r := range recs {
		seen := map[string]bool{}
		for _, h := range r.V.Hits {
			if h.Class != c {
				continue
			}
			k := key(r, h)
			if k == "" || seen[k] {
				continue
			}
			seen[k] = true
			m[k]++
		}
	}
	if len(m) == 0 {
		return "0"
	}
	return joinCounts(m)
}

func joinCounts(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s %d", k, m[k])
	}
	return strings.Join(parts, ", ")
}

// examples — до 3 событий, сначала от разных агентов, по порядку времени.
func examples(recs []Record) []Record {
	var out []Record
	seen := map[string]bool{}
	taken := map[int]bool{}
	for i, r := range recs {
		if len(out) == 3 {
			break
		}
		if !seen[r.Agent.ID] {
			seen[r.Agent.ID] = true
			taken[i] = true
			out = append(out, r)
		}
	}
	for i, r := range recs {
		if len(out) == 3 {
			break
		}
		if !taken[i] {
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out
}

func describe(r Record, c int) string {
	var parts []string
	for _, h := range r.V.Hits {
		if h.Class != c {
			continue
		}
		s := h.Verb + " " + h.Target
		if c == 3 {
			s += " (" + h.Prod + ")"
		}
		if c == 4 {
			s += " [" + h.Kind
			if h.Key != "" && r.Agent.Keys[h.Key] {
				s += ", своя задача"
			}
			s += "]"
		}
		parts = append(parts, s)
		if len(parts) == 2 {
			break
		}
	}
	when := ""
	if r.Agent.Pop == "sub" {
		when = r.Time.In(berlin()).Format("02.01 15:04") + ", "
	}
	return when + "мастер " + r.Agent.Master + ": " + strings.Join(parts, "; ")
}

func berlin() *time.Location {
	if loc, err := time.LoadLocation("Europe/Berlin"); err == nil {
		return loc
	}
	return time.Local
}

// checkRef — ссылка для сверки: id агента (префикс у суб-агента), попытка бойца и строка.
type checkRef struct {
	id, file string
	line     int
	hasLine  bool
}

var reRefLine = regexp.MustCompile(`^(.*):(\d+)$`)

// parseRef: `id:N`, `задача/attN.jsonl:N`; без `:N` — вся ссылка id агента или задачи.
func parseRef(ref string) (checkRef, error) {
	m := reRefLine.FindStringSubmatch(ref)
	if m == nil {
		return checkRef{id: ref}, nil
	}
	line, err := strconv.Atoi(m[2])
	if err != nil {
		return checkRef{}, fmt.Errorf("номер строки %q: %w", m[2], err)
	}
	c := checkRef{id: m[1], line: line, hasLine: true}
	if id, file, ok := strings.Cut(c.id, "/"); ok {
		c.id, c.file = id, file
	}
	return c, nil
}

func (c checkRef) agent(a *Agent) bool {
	return a.ID == c.id || (a.Pop == "sub" && strings.HasPrefix(a.ID, c.id))
}

func (c checkRef) record(r Record) bool {
	if !c.agent(r.Agent) {
		return false
	}
	return !c.hasLine || (r.Line == c.line && (c.file == "" || r.File == c.file))
}

// reportChecks — сверка ссылок из приёмки: какие классы найдены у события или задачи.
func reportChecks(w io.Writer, res result, checks []string) {
	fmt.Fprintln(w, "\n== Сверка ссылок")
	for _, raw := range checks {
		ref := strings.TrimPrefix(strings.TrimSpace(raw), "tl ")
		c, err := parseRef(ref)
		if err != nil {
			fmt.Fprintf(w, "%s: ссылка не разобрана: %v\n", ref, err)
			continue
		}
		printCheck(w, res, ref, c)
	}
}

func printCheck(w io.Writer, res result, ref string, c checkRef) {
	var found []Record
	for _, r := range res.records {
		if c.record(r) {
			found = append(found, r)
		}
	}
	switch {
	case !slices.ContainsFunc(res.agents, c.agent):
		fmt.Fprintf(w, "%s: не суб-агент и не боец в окне (основная сессия или вне окна) — не считается\n", ref)
	case len(found) == 0:
		fmt.Fprintf(w, "%s: без признаков классов — не считается\n", ref)
	default:
		printFound(w, ref, found)
	}
}

func printFound(w io.Writer, ref string, found []Record) {
	var hit [5]bool
	var refs []string
	for _, r := range found {
		for _, h := range r.V.Hits {
			hit[h.Class] = true
		}
		if len(r.V.Hits) > 0 && len(refs) < 3 {
			refs = append(refs, r.Ref()+" "+describe(r, r.V.Hits[0].Class))
		}
	}
	if len(refs) == 0 {
		fmt.Fprintf(w, "%s: кандидат класса %s, отсеян — не считается\n", ref, classList(found[0].V.Mentioned))
		return
	}
	fmt.Fprintf(w, "%s: попадание, класс %s\n", ref, classList(hit))
	for _, s := range refs {
		fmt.Fprintf(w, "    %s\n", s)
	}
}

// classList — номера классов 1–4, отмеченных в наборе, через запятую.
func classList(set [5]bool) string {
	var cs []string
	for c := 1; c <= 4; c++ {
		if set[c] {
			cs = append(cs, strconv.Itoa(c))
		}
	}
	return strings.Join(cs, ",")
}
