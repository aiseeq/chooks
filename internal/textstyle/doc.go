package textstyle

import (
	"regexp"
	"strings"
	"unicode"
)

// Маркеры документа markdown (сводка встречи перед импортом в Google Doc):
// разметка в нём разрешена, зато ключ задачи без ссылки и относительная
// дата читателю документа ничего не скажут. Заменяют ручной чек-лист из
// grep в скилле meeting-summary (import.md, «Чек-лист перед импортом»)
var (
	// RelativeDate относительная дата: документ читают в другой день
	RelativeDate = Marker{
		Description: "относительная дата",
		Fix:         "число: '14.11', 'пт 14.11'",
		Find:        findRelativeDate,
	}
	// NotXButY оборот «не X, а Y» без запятой перед «не»: чек-лист сводки
	// ловил его отдельно от «X, а не Y»
	NotXButY = Marker{
		Description: "оборот 'не X, а Y'",
		Fix:         "утверждение прямо, противопоставление при нужде отдельным предложением",
		Find:        RegexpFinder(regexp.MustCompile(`(?i)` + wordBoundary + `(не\s+[^,.!?\n]{1,40},\s+а\s+\p{L}+)`)),
	}
)

// UnlinkedKeyDescription — имя маркера ключа задачи вне ссылки
const UnlinkedKeyDescription = "ключ задачи без ссылки"

// UnlinkedKey ключ задачи вне ссылки markdown и вне URL: в документе его не
// открыть. prefixes — ключи проектов («PROJ»); пусто — любой ключ вида
// ABC-12 (тогда находкой будут и UTF-8, SHA-256)
func UnlinkedKey(prefixes []string) Marker {
	project := `[A-Z][A-Z0-9]{1,9}`
	if len(prefixes) > 0 {
		quoted := make([]string, len(prefixes))
		for i, p := range prefixes {
			quoted[i] = regexp.QuoteMeta(p)
		}
		project = `(?:` + strings.Join(quoted, "|") + `)`
	}
	key := regexp.MustCompile(`(?:^|[^\p{L}\p{N}_-])(` + project + `-\d+)`)
	return Marker{
		Description: UnlinkedKeyDescription,
		Fix:         "ссылка markdown: '[KEY-12](https://…/browse/KEY-12)'",
		Find:        func(line string) string { return findUnlinkedKey(key, line) },
	}
}

// DocMarkers маркеры документа: тире, кавычки, обороты противопоставления,
// десятичная запятая и маркеры документа. Markdown (заголовки, списки,
// ссылки, таблицы, выделение) не нарушение. prefixes — как у UnlinkedKey
func DocMarkers(prefixes []string) []Marker {
	return []Marker{
		EmDash, EnDash, TypographicQuotes, ButNot, ThisIsNotBut, RepeatedPreposition, NotXButY,
		DecimalComma, UnlinkedKey(prefixes), RelativeDate,
	}
}

// CheckDoc проверяет документ markdown построчно
func CheckDoc(text string, prefixes []string) []Finding {
	return check(text, DocMarkers(prefixes))
}

var (
	// linkedPattern ссылка markdown целиком и голый URL: ключ внутри них
	// открывается
	linkedPattern = regexp.MustCompile(`\[[^\]\n]*\]\([^)\s]*\)|https?://\S+`)
)

// findUnlinkedKey ищет ключ задачи вне ссылок: ссылки и URL заменяются
// пробелами той же длины, чтобы ключ рядом со ссылкой не склеился с ней
func findUnlinkedKey(key *regexp.Regexp, line string) string {
	bare := linkedPattern.ReplaceAllStringFunc(line, func(s string) string {
		return strings.Repeat(" ", len(s))
	})
	if m := key.FindStringSubmatch(bare); m != nil {
		return m[1]
	}
	return ""
}

var (
	relativeWordPattern = regexp.MustCompile(`(?i)` + wordBoundary +
		`((?:сегодня|завтра|вчера|послезавтра|позавчера|(?:сегодняшн|завтрашн|вчерашн)\p{L}+|` +
		`на\s+(?:этой|следующей|прошлой)\s+неделе|через\s+неделю)(?:[^\p{L}\p{N}]|$))`)
	// weekdayPattern день недели с предлогом. «Среда» — только «в среду»:
	// «среда», «в среде» в тексте обычно окружение, а не день
	weekdayPattern = regexp.MustCompile(`(?i)` + wordBoundary +
		`((?:в|во|к|до|с|со|по|после)\s+(?:понедельник\p{L}*|вторник\p{L}*|четверг\p{L}*|пятниц\p{L}*|` +
		`суббот\p{L}*|воскресень\p{L}*)|в\s+среду)(?:[^\p{L}\p{N}]|$)`)
)

// findRelativeDate ищет относительную дату; день недели с числом после него
// («в пятницу 14.11», «в пятницу, 14.11», «в пятницу (14.11)») — не находка
func findRelativeDate(line string) string {
	if m := relativeWordPattern.FindStringSubmatch(line); m != nil {
		return strings.TrimRightFunc(m[1], func(r rune) bool { return !unicode.IsLetter(r) })
	}
	for _, loc := range weekdayPattern.FindAllStringSubmatchIndex(line, -1) {
		if !datedAfter(line[loc[3]:]) {
			return line[loc[2]:loc[3]]
		}
	}
	return ""
}

// datedAfter — после дня недели сразу идёт число: через пробелы, запятую или
// открывающую скобку
func datedAfter(rest string) bool {
	rest = strings.TrimLeft(rest, " \t,(")
	return rest != "" && rest[0] >= '0' && rest[0] <= '9'
}
