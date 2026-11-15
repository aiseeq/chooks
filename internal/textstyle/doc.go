package textstyle

import (
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
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
// ABC-12 (тогда находкой будут и UTF-8, SHA-256). Каждый ключ строки —
// своя находка; за номером не буква и не цифра (PROJ-1a не ключ), дефис
// можно: ветка feature/PROJ-12-fix тоже ведёт к задаче
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
		Find: func(line string) string {
			if keys := findUnlinkedKeys(key, line); len(keys) > 0 {
				return keys[0]
			}
			return ""
		},
		FindAll: func(line string) []string { return findUnlinkedKeys(key, line) },
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
	// её определение ([ref]: url): ключ внутри них открывается
	linkedPattern = regexp.MustCompile(`\[[^\]\n]*\]\([^)\s]*\)|\[[^\]\n]*\]\[[^\]\n]*\]|^\s*\[[^\]\n]*\]:\s*\S+`)
	// urlPattern голый URL: ключ внутри него тоже открывается
	urlPattern = regexp.MustCompile(`https?://\S+`)
)

// findUnlinkedKeys ищет ключи задачи вне ссылок: ссылки и URL заменяются
// пробелами той же длины, чтобы ключ рядом со ссылкой не склеился с ней.
// Правая граница — проверкой знака после номера: в RE2 нет lookahead, а
// съеденный знак отнял бы левую границу у ключа сразу за ним. Знак после
// номера — из исходной строки: в PROJ-1https://… за номером буква, хотя URL
// уже заменён пробелами. Повтор ключа в строке — одна находка
func findUnlinkedKeys(key *regexp.Regexp, line string) []string {
	bare := blankURLs(linkedPattern.ReplaceAllStringFunc(line, blank))
	var keys []string
	for _, m := range key.FindAllStringSubmatchIndex(bare, -1) {
		next, _ := utf8.DecodeRuneInString(line[m[3]:])
		found := bare[m[2]:m[3]]
		if unicode.IsLetter(next) || unicode.IsNumber(next) || slices.Contains(keys, found) {
			continue
		}
		keys = append(keys, found)
	}
	return keys
}

// blank — пробелы на месте s той же длины
func blank(s string) string { return strings.Repeat(" ", len(s)) }

// blankURLs заменяет голые URL пробелами. URL, открытый внутри скобки,
// кончается перед закрывающей «)»: в «(https://x/PROJ-2)PROJ-3» ключ за
// скобкой — не часть адреса
func blankURLs(line string) string {
	var b strings.Builder
	end := 0
	for {
		m := urlPattern.FindStringIndex(line[end:])
		if m == nil {
			break
		}
		start, stop := end+m[0], end+m[1]
		if strings.Count(line[:start], "(") > strings.Count(line[:start], ")") {
			if i := strings.IndexByte(line[start:stop], ')'); i >= 0 {
				stop = start + i
			}
		}
		b.WriteString(line[end:start])
		b.WriteString(blank(line[start:stop]))
		end = stop
	}
	b.WriteString(line[end:])
	return b.String()
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
	// прошлой недели» — в ячейке таблицы и с любым предлогом
	relPrep + `(?:эт(?:а|ой|у|ом)|следующ(?:ая|ей|ую|ем)|текущ(?:ая|ей|ую|ем)|прошл(?:ая|ой|ую|ом)|` +
		`будущ(?:ая|ей|ую|ем)|нынешн(?:яя|ей|юю|ем)|ближайш(?:ая|ей|ую))\s+недел(?:я|и|е|ю)`,
	// «на той неделе» — следующая или прошлая. «К концу той недели»,
	// «в течение той недели» после даты («релиз 16.11; к концу той недели»)
	// — отсылка к ней, «на той же неделе» — к уже сказанной: не находки
	`на\s+той\s+неделе`,
	// «в следующем месяце», «со следующего квартала», «в этом году»
	relPrep + `(?:эт(?:от|ом|ого)|следующ(?:ий|ем|его)|текущ(?:ий|ем|его)|прошл(?:ый|ом|ого)|` +
		`будущ(?:ий|ем|его)|нынешн(?:ий|ем|его)|ближайш(?:ий|ем|его))\s+(?:месяц|квартал|год)\p{L}*`,
	// «к концу недели», «в начале месяца», «до конца года»
	`(?:к|в|до)\s+(?:конц|начал)[уеа]\s+(?:недели|месяца|квартала|года)`,
	`на\s+днях|на\s+неделе`,
	`в\s+ближайш(?:ие\s+(?:дни|недели|месяцы)|ее\s+время)`,
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
	// outputAfter — «выходным», «выходных» перед существительным из
	// короткого списка — прилагательное, не дни: «к выходным данным», «до
	// выходных полей». Список закрытый, как у genitiveAfter: другое слово
	// после оборота («к выходным клиентам отдадим», «к выходным дням», «к
	// выходным нам») оборот не оправдывает, это срок
	outputAfter = regexp.MustCompile(`(?i)^\s+(?:данн(?:ым|ых)|параметр(?:ам|ов)|пол(?:ям|ей)|файл(?:ам|ов)|` +
		`ос(?:ям|ей)|значени(?:ям|й)|сигнал(?:ам|ов))(?:[^\p{L}\p{N}]|$)`)
	// genitiveAfter — после «среду», «год» существительное в родительном из
	// короткого списка: «в среду окружения», «на среду разработки» — среда
	// как окружение, «ближайший год выпуска» — год как признак, не срок.
	// Остальные слова в родительном («на неделе моды») оборот не оправдывают:
	// лишняя находка смотрится глазами, а пропуск оставит дату в сводке
	genitiveAfter = regexp.MustCompile(`(?i)^\s+(?:окружения|разработки|исполнения|выполнения|тестирования|сборки|` +
		`выпуска|рождения|основания)(?:[^\p{L}\p{N}]|$)`)
	// negationAfter — «в этом году выпуска не будет», «в среду сборки нет»:
	// родительный при отрицании, оборот остаётся сроком
	negationAfter = regexp.MustCompile(`(?i)^\s+\p{L}+\s+(?:не|нет)(?:[^\p{L}\p{N}]|$)`)
	// nextElided «на следующей» без «неделе» после уже названной недели:
	// «на этой неделе тесты, на следующей релиз». Слово на «е», «и» после
	// него — своё существительное («на следующей встрече», «строке»)
	nextElided = regexp.MustCompile(`(?i)` + wordBoundary + `(на\s+следующей)(?:[^\p{L}\p{N}]|$)`)
	weekNamed  = regexp.MustCompile(`(?i)недел`)
	nounNext   = regexp.MustCompile(`(?i)^\s+\p{L}*[еи](?:[^\p{L}\p{N}]|$)`)
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
	// weekdayRange «с понедельника по пятницу», «с пн до пт» — будни
	// целиком, устойчивое имя рабочей недели в расписании, как «в
	// выходные»: не находка. Другие пары дней («со среды по пятницу», «с
	// понедельника по воскресенье») — отрезок конкретной недели («в отпуске
	// со среды по пятницу»), как и будни с определителем («со следующего
	// понедельника по пятницу»): находки
	weekdayRange = regexp.MustCompile(`(?i)` + wordBoundary + `с\s+(?:понедельник\p{L}*|пн)\s+(?:по|до)\s+(?:пятниц\p{L}*|пт)(?:[^\p{L}\p{N}]|$)`)
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

// dateHead — начало даты: «14.11», «16 - 20.11», «9 декабря», «2026-11-20».
// После точки — месяц двумя цифрами, за ним не цифра и не «%»: «1.5 часа»,
// «2.5%», «1.25» — не дата
const dateHead = `(?:\d{4}-\d{2}-\d{2}|(?:\d{1,2}\s*[-–]\s*)?\d{1,2}(?:\.(?:0[1-9]|1[0-2])(?:[^\p{N}%]|$)|` +
	`\s+(?:январ|феврал|март|апрел|ма|июн|июл|август|сентябр|октябр|ноябр|декабр)[яа](?:[^\p{L}\p{N}]|$)))`

// weekdayWhich — «этот», «следующий» перед днём недели во всех нужных формах
const weekdayWhich = `(?:этот|эту|этого|этой|этому|следующий|следующую|следующего|следующей|следующему|` +
	`прошлый|прошлую|прошлого|прошлой|прошлому|ближайший|ближайшую|ближайшего|ближайшей|ближайшему)`

// findRelativeDate ищет относительную дату, одну на строку: самый левый
// оборот. Оборот с датой после него («в пятницу 14.11», «в конце недели 16 -
// 20.11») — не находка
func findRelativeDate(line string) string {
	days := weekdayRange.ReplaceAllStringFunc(line, blank)
	turn, at := "", len(line)
	for _, c := range []struct {
		pattern, dated *regexp.Regexp
		text           string
		excused        func(before, turn, rest string) bool
	}{
		{relativeWordPattern, periodDated, line, func(_, turn, rest string) bool {
			return lastWordIs(turn, "год") && genitiveTerm(rest)
		}},
		{spanPattern, periodDated, line, func(_, _, rest string) bool { return eventAfter.MatchString(rest) }},
		{weekendPattern, periodDated, line, func(_, _, rest string) bool { return outputAfter.MatchString(rest) }},
		{nextElided, periodDated, line, func(before, _, rest string) bool {
			return !weekNamed.MatchString(before) || nounNext.MatchString(rest)
		}},
		{weekdayPattern, weekdayDated, days, func(_, day, rest string) bool {
			return strings.HasSuffix(day, "ам") || strings.HasSuffix(day, "ям") ||
				lastWordIs(day, "сред") && genitiveTerm(rest)
		}},
	} {
		if found, start := firstUndated(c.pattern, c.dated, c.text, c.excused); start < at {
			turn, at = found, start
		}
	}
	return turn
}

// genitiveTerm — после оборота слово из genitiveAfter не при отрицании
func genitiveTerm(rest string) bool {
	return genitiveAfter.MatchString(rest) && !negationAfter.MatchString(rest)
}

// lastWordIs — последнее слово оборота начинается со stem: «ближайший год»,
// «в среду»
func lastWordIs(turn, stem string) bool {
	words := strings.Fields(strings.ToLower(turn))
	return strings.HasPrefix(words[len(words)-1], stem)
}

// firstUndated — первый оборот группы 1 pattern и его начало, после
// которого нет даты по dated и который excused не оправдывает по тексту до
// оборота, самому обороту и остатку строки. Нет такого — "" и len(line)
func firstUndated(pattern, dated *regexp.Regexp, line string, excused func(before, turn, rest string) bool) (string, int) {
	for _, loc := range pattern.FindAllStringSubmatchIndex(line, -1) {
		before, turn, rest := line[:loc[2]], line[loc[2]:loc[3]], line[loc[3]:]
		if !dated.MatchString(rest) && !excused(before, turn, rest) {
			return turn, loc[2]
		}
	}
	return "", len(line)
}
