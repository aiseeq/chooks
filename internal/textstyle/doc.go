package textstyle

import (
	"regexp"
	"strings"
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
	// linkedPattern ссылка markdown целиком, ссылка по сноске ([x][ref]) и
	// её определение ([ref]: url), голый URL: ключ внутри них
	// открывается
	linkedPattern = regexp.MustCompile(`\[[^\]\n]*\]\([^)\s]*\)|\[[^\]\n]*\]\[[^\]\n]*\]|^\s*\[[^\]\n]*\]:\s*\S+|https?://\S+`)
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

// Числа и единицы срока: «через 2 недели», «3 дня назад», «ближайшие 2 недели»
const (
	relCount = `(?:два|две|три|четыре|пять|пару|несколько|полтора|полторы|\d+(?:-\d+)?)`
	relUnit  = `(?:недел|дн|день|месяц|год|лет|квартал)\p{L}*`
	// relPrep необязательный предлог перед «этой неделей», «следующим
	// месяцем»: находка — оборот целиком
	relPrep = `(?:(?:на|в|во|к|до|с|со|по)\s+)?`
)

// relativeForms — формы относительной даты, по ветке на форму
var relativeForms = []string{
	`сегодня|завтра|вчера|послезавтра|позавчера`,
	`(?:сегодняшн|завтрашн|вчерашн)\p{L}+`,
	// «эта неделя», «на следующей неделе», «к следующей неделе», «до конца
	// прошлой недели» — в ячейке таблицы и с любым предлогом. «На той же
	// неделе» — та, о которой уже сказано: не находка
	relPrep + `(?:эт(?:а|ой|у|ом)|следующ(?:ая|ей|ую|ем)|текущ(?:ая|ей|ую|ем)|прошл(?:ая|ой|ую|ом)|` +
		`будущ(?:ая|ей|ую|ем)|нынешн(?:яя|ей|юю|ем)|той)\s+недел(?:я|и|е|ю)`,
	// «в следующем месяце», «со следующего квартала», «в этом году»
	relPrep + `(?:эт(?:от|ом|ого)|следующ(?:ий|ем|его)|текущ(?:ий|ем|его)|прошл(?:ый|ом|ого)|` +
		`будущ(?:ий|ем|его)|нынешн(?:ий|ем|его))\s+(?:месяц|квартал|год)\p{L}*`,
	// «к концу недели», «в начале месяца», «до конца года»
	`(?:к|в|до)\s+(?:конц|начал)[уеа]\s+(?:недели|месяца|квартала|года)`,
	`на\s+днях|на\s+неделе`,
	`в\s+ближайш(?:ие\s+дни|ее\s+время)`,
	relCount + `\s+` + relUnit + `\s+назад|(?:неделю|месяц|год)\s+назад`,
	`ближайш\p{L}+\s+` + relCount + `\s+` + relUnit,
	`(?:в|на)\s+(?:эти|этих|следующие|следующих|ближайшие|ближайших|прошлые|прошлых)\s+выходн\p{L}+`,
}

// spanForms — срок: «через неделю», «спустя 2 дня», «неделю спустя», «в
// течение недели». С «после» («через неделю после релиза») срок идёт от
// события, а не от дня встречи: не находка
var spanForms = []string{
	`(?:через|спустя)\s+(?:неделю|день|месяц|полгода|год)`,
	`(?:через|спустя)\s+` + relCount + `\s+` + relUnit,
	`(?:` + relCount + `\s+` + relUnit + `|неделю|месяц|год)\s+спустя`,
	`в\s+течение\s+недели`,
}

// weekdayName день недели полным именем или сокращением (пн, вт)
const weekdayName = `(?:(?:понедельник|вторник|сред|четверг|пятниц|суббот|воскресень)\p{L}*|пн|вт|ср|чт|пт|сб|вс)`

var (
	relativeWordPattern = regexp.MustCompile(`(?i)` + wordBoundary + `(` + strings.Join(relativeForms, "|") + `)(?:[^\p{L}\p{N}]|$)`)
	spanPattern         = regexp.MustCompile(`(?i)` + wordBoundary + `(` + strings.Join(spanForms, "|") + `)(?:[^\p{L}\p{N}]|$)`)
	// eventAfter — после срока событие: «через неделю после релиза»
	eventAfter = regexp.MustCompile(`^\s+после(?:[^\p{L}\p{N}]|$)`)
	// weekendPattern «к выходным», «до выходных» — срок, он всегда
	// конкретный. «В выходные», «на выходных» без определителя — категория
	// дней («начисления в выходные», «просадку на выходных поймают алерты»):
	// не находки
	weekendPattern = regexp.MustCompile(`(?i)` + wordBoundary + `(к\s+выходным|до\s+выходных)(?:[^\p{L}\p{N}]|$)`)
	// nounAfter — после «выходным», «выходных» существительное во
	// множественном числе: «к выходным данным», «до выходных полей» —
	// прилагательное, не дни. Четыре буквы и больше: «к выходным нам» —
	// местоимение, срок
	nounAfter = regexp.MustCompile(`^\s+\p{L}{2,}(?:ам|ям|ым|ов|ей|ых)(?:[^\p{L}\p{N}]|$)`)
	// weekdayPattern день недели с предлогом, полным именем или сокращением
	// (пн, вт). Без предлога день в сводке — расписание («понедельник и
	// четверг, 15:00»), «по четвергам» — повтор: не находки. «Среда» — только
	// в формах дня («в среду», «к среде», «до среды»): «среда», «в среде» в
	// тексте обычно окружение, а с «этот», «следующий» («в следующую среду»)
	// — день. «Выходные» — только с «эти», «следующие»: «начисления в
	// выходные» — категория дней
	weekdayPattern = regexp.MustCompile(`(?i)` + wordBoundary +
		`((?:в|во|к|ко|до|с|со|по|после|на)\s+(?:` + weekdayWhich + `\s+)?(?:понедельник\p{L}*|вторник\p{L}*|четверг\p{L}*|пятниц\p{L}*|` +
		`суббот\p{L}*|воскресень\p{L}*)|(?:в|на)\s+среду|к\s+среде|(?:до|с|со|после)\s+среды|(?:в|к|до|с|со|после|на)\s+` + weekdayWhich + `\s+(?:среду|среде|среды)|` +
		`(?:в|во|к|ко|до|с|со|по|после|на)\s+(?:пн|вт|ср|чт|пт|сб|вс))(?:[^\p{L}\p{N}]|$)`)
	// weekdayRange «с понедельника по пятницу», «с пн до пт» без
	// определителя — будни, расписание, как «в выходные»: не находка. С
	// определителем («со следующего понедельника по пятницу») — конкретная
	// неделя, находка
	weekdayRange = regexp.MustCompile(`(?i)` + wordBoundary + `(?:с|со)\s+` + weekdayName + `\s+(?:по|до)\s+` + weekdayName)
	// weekdayDated — после дня недели дата: «в пятницу 14.11», «в пятницу,
	// 14.11», «в пятницу (14.11)», «в среду 9 декабря». Число без даты («3
	// задачи», «11:00») и дата с «с» («в понедельник, с 20 сентября
	// начинает» — другой срок) — нет
	weekdayDated = regexp.MustCompile(`^[ \t,(]*` + dateHead)
	// periodDated — после оборота дата через пробел: «в конце недели 16 -
	// 20.11», «на неделе с 09.11». Дата в скобках или после запятой у
	// оборота — пояснение, оборот остаётся находкой, как в чек-листе
	// meeting-summary («на этой неделе (02.11 - 08.11)»)
	periodDated = regexp.MustCompile(`^\s+(?:с\s+)?` + dateHead)
)

// dateHead — начало даты: «14.11», «16 - 20.11», «9 декабря», «2026-11-20»
const dateHead = `(?:\d{4}-\d{2}-\d{2}|(?:\d{1,2}\s*[-–]\s*)?\d{1,2}(?:\.\d{1,2}|` +
	`\s+(?:январ|феврал|март|апрел|ма|июн|июл|август|сентябр|октябр|ноябр|декабр)[яа]))`

// weekdayWhich — «этот», «следующий» перед днём недели во всех нужных формах
const weekdayWhich = `(?:этот|эту|этого|этой|этому|следующий|следующую|следующего|следующей|следующему|` +
	`прошлый|прошлую|прошлого|прошлой|прошлому|ближайший|ближайшую|ближайшего|ближайшей|ближайшему)`

// findRelativeDate ищет относительную дату, одну на строку. Оборот с датой
// после него («в пятницу 14.11», «в конце недели 16 - 20.11») — не находка
func findRelativeDate(line string) string {
	if word := firstUndated(relativeWordPattern, periodDated, line, nil); word != "" {
		return word
	}
	if span := firstUndated(spanPattern, periodDated, line, func(_, rest string) bool { return eventAfter.MatchString(rest) }); span != "" {
		return span
	}
	if weekend := firstUndated(weekendPattern, periodDated, line, func(_, rest string) bool { return nounAfter.MatchString(rest) }); weekend != "" {
		return weekend
	}
	days := weekdayRange.ReplaceAllStringFunc(line, func(s string) string { return strings.Repeat(" ", len(s)) })
	return firstUndated(weekdayPattern, weekdayDated, days, func(day, _ string) bool {
		return strings.HasSuffix(day, "ам") || strings.HasSuffix(day, "ям")
	})
}

// firstUndated — первый оборот группы 1 pattern, после которого нет даты по
// dated и который excused (если задан) не оправдывает по самому обороту и
// остатку строки
func firstUndated(pattern, dated *regexp.Regexp, line string, excused func(turn, rest string) bool) string {
	for _, loc := range pattern.FindAllStringSubmatchIndex(line, -1) {
		turn, rest := line[loc[2]:loc[3]], line[loc[3]:]
		if !dated.MatchString(rest) && (excused == nil || !excused(turn, rest)) {
			return turn
		}
	}
	return ""
}
