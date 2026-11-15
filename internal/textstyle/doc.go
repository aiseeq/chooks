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
	// «_» не граница: ключ в курсиве markdown (_KEY-1_) — тоже ключ
	key := regexp.MustCompile(`(?:^|[^\p{L}\p{N}-])(` + project + `-\d+)`)
	return Marker{
		Description: UnlinkedKeyDescription,
		Fix:         "ссылка markdown: '[KEY-12](https://…/browse/KEY-12)'",
		Find:        func(line string) string { return findUnlinkedKey(key, line) },
	}
}

// Тире и кавычки документа — любой такой знак, как у чек-листа сводки: в
// markdown рядом с ними обычно разметка (**итог** — …, `api` — …, 10% — …),
// и маркеры хука, ждущие букву по обе стороны, их пропускают
var (
	DocEmDash = Marker{
		Description: EmDash.Description,
		Fix:         EmDash.Fix,
		Find:        RegexpFinder(regexp.MustCompile(`\S*[ \t]*—[ \t]*\S*`)),
	}
	DocEnDash = Marker{
		Description: EnDash.Description,
		Fix:         EnDash.Fix,
		Find:        RegexpFinder(regexp.MustCompile(`\S*[ \t]*–[ \t]*\S*`)),
	}
	// DocQuotes пара кавычек целиком, иначе одиночная кавычка
	DocQuotes = Marker{
		Description: TypographicQuotes.Description,
		Fix:         TypographicQuotes.Fix,
		Find:        RegexpFinder(regexp.MustCompile(`[«“„][^«»“”„\n]*[»”“]|[«»“”„]`)),
	}
)

// DocMarkers маркеры документа: тире, кавычки, обороты противопоставления,
// десятичная запятая и маркеры документа. Markdown (заголовки, списки,
// ссылки, таблицы, выделение) не нарушение. Оборот «не X, а Y» один на все
// его формы («это не X, а Y», «не из X, а из Y»): одна находка на оборот.
// prefixes — как у UnlinkedKey
func DocMarkers(prefixes []string) []Marker {
	return []Marker{
		DocEmDash, DocEnDash, DocQuotes, ButNot, NotXButY,
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
		`на\s+(?:этой|следующей|прошлой)\s+неделе|(?:с|со)\s+(?:этой|следующей|прошлой)\s+недели|` +
		`(?:к|в)\s+конц[уе]\s+(?:недели|месяца)|в\s+(?:этом|следующем|прошлом)\s+месяце|на\s+днях|` +
		`через\s+(?:неделю|день|месяц|полгода|год)|` +
		`через\s+(?:две|три|четыре|пять|пару|несколько|\d+)\s+(?:недел|дн|день|месяц|год|лет)\p{L}*|` +
		`(?:в|на)\s+(?:эти|этих|следующие|следующих|ближайшие|ближайших|прошлые|прошлых)\s+выходн\p{L}+)(?:[^\p{L}\p{N}]|$))`)
	// weekdayPattern день недели с предлогом, полным именем или сокращением
	// (пн, вт). Без предлога день в сводке — расписание («понедельник и
	// четверг, 15:00»), «по четвергам» — повтор: не находки. «Среда» — только
	// в формах дня («в среду», «к среде», «до среды»): «среда», «в среде» в
	// тексте обычно окружение, а с «этот», «следующий» («в следующую среду»)
	// — день. «Выходные» — только с «эти», «следующие»: «начисления в
	// выходные» — категория дней
	weekdayPattern = regexp.MustCompile(`(?i)` + wordBoundary +
		`((?:в|во|к|до|с|со|по|после)\s+(?:` + weekdayWhich + `\s+)?(?:понедельник\p{L}*|вторник\p{L}*|четверг\p{L}*|пятниц\p{L}*|` +
		`суббот\p{L}*|воскресень\p{L}*)|в\s+среду|к\s+среде|(?:до|с|со|после)\s+среды|(?:в|к|до|с|со|после)\s+` + weekdayWhich + `\s+(?:среду|среде|среды)|` +
		`(?:в|во|к|до|с|со|по|после)\s+(?:пн|вт|ср|чт|пт|сб|вс))(?:[^\p{L}\p{N}]|$)`)
)

// weekdayWhich — «этот», «следующий» перед днём недели во всех нужных формах
const weekdayWhich = `(?:этот|эту|этого|этой|следующий|следующую|следующего|следующей|прошлый|прошлую|прошлого|прошлой|ближайший|ближайшую|ближайшего|ближайшей)`

// findRelativeDate ищет относительную дату; день недели с числом после него
// («в пятницу 14.11», «в пятницу, 14.11», «в пятницу (14.11)») — не находка
func findRelativeDate(line string) string {
	if m := relativeWordPattern.FindStringSubmatch(line); m != nil {
		return strings.TrimRightFunc(m[1], func(r rune) bool { return !unicode.IsLetter(r) })
	}
	for _, loc := range weekdayPattern.FindAllStringSubmatchIndex(line, -1) {
		day := line[loc[2]:loc[3]]
		if !datedAfter(line[loc[3]:]) && !strings.HasSuffix(day, "ам") && !strings.HasSuffix(day, "ям") {
			return day
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
