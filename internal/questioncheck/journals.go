package questioncheck

import (
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/aiseeq/akit/secretmask"
)

// Поиск по словам: сколько основ слов берётся и сколько записей считаются
// редкими (best)
const (
	maxKeywords = 10
	rareEntries = 3
)

// Journals — журнал реплик пользователя во всех переписках
type Journals struct {
	RepliesPath string
}

// DefaultJournals — пути журналов в ~/.claude
func DefaultJournals(claudeDir string) Journals {
	return Journals{
		RepliesPath: claudeDir + "/user-replies.md",
	}
}

var (
	wordPattern = regexp.MustCompile(`[\p{L}\p{N}][\p{L}\p{N}_-]*`)
	// replyHeader — заголовок блока журнала реплик: «### 2026-10-22T14:33:15.508Z (…) · ~ · claude …»
	replyHeader = regexp.MustCompile(`^### (\d{4}-\d{2}-\d{2}T[0-9:.]+Z)`)
)

// stopWords — частые слова вопросов, которые ничего не находят
var stopWords = map[string]bool{
	"сейчас": true, "делать": true, "делаем": true, "давай": true, "можно": true, "нужно": true,
	"нужен": true, "нужна": true, "нужны": true, "твоего": true, "твоё": true, "твое": true,
	"вашего": true, "решение": true, "ответа": true, "ждёт": true, "ждет": true, "который": true,
	"которая": true, "которые": true, "чтобы": true, "потом": true, "после": true, "сначала": true,
	"тогда": true, "только": true, "ещё": true, "если": true, "когда": true, "какой": true,
	"какую": true, "какие": true, "этого": true, "этому": true, "этот": true, "этой": true,
	"будет": true, "будем": true, "сделать": true, "оставить": true, "правильно": true,
	"вопрос": true, "вопросы": true, "открытый": true, "открытых": true, "скажи": true,
}

// keywords — основы значимых слов текста: без окончания, чтобы
// «артефакт» находил «артефакта». Длинные и редкие слова полезнее, вес — по
// редкости в самих журналах (best)
func keywords(text string) []string {
	seen := map[string]bool{}
	var keys []string
	for _, word := range wordPattern.FindAllString(strings.ToLower(text), -1) {
		if utf8.RuneCountInString(word) < 4 || stopWords[word] {
			continue
		}
		key := stem(word)
		if seen[key] {
			continue
		}
		seen[key] = true
		keys = append(keys, key)
	}
	// Длинные слова вперёд: короткие чаще общие
	sort.SliceStable(keys, func(a, b int) bool {
		return utf8.RuneCountInString(keys[a]) > utf8.RuneCountInString(keys[b])
	})
	if len(keys) > maxKeywords {
		keys = keys[:maxKeywords]
	}
	return keys
}

// stem отрезает от русского слова до двух последних букв (окончание),
// оставляя не меньше четырёх. Латиницу и идентификаторы (atlas, ABC-12) не
// трогает: это имена
func stem(word string) string {
	runes := []rune(word)
	for _, r := range runes {
		if !unicode.Is(unicode.Cyrillic, r) && r != '-' {
			return word
		}
	}
	keep := len(runes) - 2
	if keep < 4 {
		keep = len(runes)
	}
	return string(runes[:keep])
}

// replyBlocks режет журнал реплик на блоки «### время …» не позже before
func replyBlocks(text string, before time.Time) []string {
	var blocks []string
	var current strings.Builder
	keep := false
	flush := func() {
		if keep && current.Len() > 0 {
			blocks = append(blocks, strings.TrimSpace(current.String()))
		}
		current.Reset()
	}
	for line := range strings.SplitSeq(text, "\n") {
		if m := replyHeader.FindStringSubmatch(line); m != nil {
			flush()
			keep = true
			if !before.IsZero() {
				at, err := time.Parse(time.RFC3339Nano, m[1])
				keep = err == nil && at.Before(before)
			}
		}
		if keep {
			current.WriteString(line)
			current.WriteByte('\n')
		}
	}
	flush()
	return blocks
}

// best выбирает записи по BM25 с бинарной частотой: вес основы —
// log(1 + (N − n + 0.5) / (n + 0.5)), где n — число записей с ней, так редкое
// слово весит больше частого; длинная запись (вставленный лог) не выигрывает
// числом слов. Нужно совпадение хотя бы двух основ, если основ больше одной,
// или одной редкой (id артефакта, номер тикета — не больше rareEntries записей);
// при равном весе новее та, что ниже в журнале
func best(entries, keys []string, limit, runes int) []string {
	indexes := bestIndexes(entries, keys, limit)
	result := make([]string, 0, len(indexes))
	for _, index := range indexes {
		result = append(result, Excerpt(entries[index], runes))
	}
	return result
}

// bestIndexes — номера записей, которые выбрал бы best, лучшие первыми
func bestIndexes(entries, keys []string, limit int) []int {
	if len(entries) == 0 {
		return nil
	}
	const k1, b = 1.2, 0.75
	lower := make([]string, len(entries))
	freq := make(map[string]int, len(keys))
	total := 0
	for i, entry := range entries {
		lower[i] = strings.ToLower(entry)
		total += utf8.RuneCountInString(entry)
		for _, key := range keys {
			if strings.Contains(lower[i], key) {
				freq[key]++
			}
		}
	}
	avg := float64(total) / float64(len(entries))
	need := 2
	if len(keys) == 1 {
		need = 1
	}
	type scored struct {
		index int
		score float64
	}
	var hits []scored
	for i := range entries {
		norm := (k1 + 1) / (1 + k1*(1-b+b*float64(utf8.RuneCountInString(entries[i]))/avg))
		matched := 0
		rare := false
		score := 0.0
		for _, key := range keys {
			n := float64(freq[key])
			if n > 0 && strings.Contains(lower[i], key) {
				matched++
				rare = rare || freq[key] <= rareEntries
				score += math.Log(1+(float64(len(entries))-n+0.5)/(n+0.5)) * norm
			}
		}
		if matched >= need || rare {
			hits = append(hits, scored{index: i, score: score})
		}
	}
	sort.SliceStable(hits, func(a, b int) bool {
		if hits[a].score != hits[b].score {
			return hits[a].score > hits[b].score
		}
		return hits[a].index > hits[b].index
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	indexes := make([]int, 0, len(hits))
	for _, hit := range hits {
		indexes = append(indexes, hit.index)
	}
	return indexes
}

// Excerpt — выдержка для судьи и журнала: без значений секретов, до limit
// знаков с пометкой «…[обрезано]», судья должен знать, что видит не всё.
// Значения скрываются до среза: начало ключа после него короче образца и
// прошло бы и дверь Ask
func Excerpt(text string, limit int) string {
	text, _ = secretmask.Mask(text)
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + " …[обрезано]"
}
