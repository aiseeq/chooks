package textstyle

import (
	"slices"
	"testing"
)

// Разметка сводки — заголовок, таблица, ссылка на ключ, список, выделение —
// в документе не нарушение
func TestCheckDocAllowsMarkdown(t *testing.T) {
	doc := "## Итоги встречи 14.11\n" +
		"\n" +
		"| Задача | Кто | Срок |\n" +
		"|---|---|---|\n" +
		"| [PROJ-12](https://jira.example/browse/PROJ-12) | Егор | пт 21.11 |\n" +
		"\n" +
		"- **Решение:** выкатка в пятницу 21.11, откат по [PROJ-13](https://jira.example/browse/PROJ-13)\n" +
		"- проверка во вторник, 18.11, отчёт в среду (19.11)\n" +
		"- лог: https://jira.example/browse/PROJ-14?focus=1\n" +
		"- тестовая среда, в среде prod не трогаем\n" +
		"- комиссия 1.5%"
	if findings := CheckDoc(doc, projKeys); len(findings) > 0 {
		t.Fatalf("ложные срабатывания: %+v", findings)
	}
}

func TestCheckDocFindsMarkers(t *testing.T) {
	cases := []struct {
		line, want, fragment string
	}{
		{"Срок по PROJ-34 сдвинут", UnlinkedKeyDescription, "PROJ-34"},
		{"[PROJ-12](https://x/PROJ-12) и PROJ-34", UnlinkedKeyDescription, "PROJ-34"},
		{"Созвон в пятницу", RelativeDate.Description, "в пятницу"},
		{"Созвон в пятницу в 11:00", RelativeDate.Description, "в пятницу"},
		{"До понедельника решим", RelativeDate.Description, "До понедельника"},
		{"Отчёт в среду", RelativeDate.Description, "в среду"},
		{"Завтра выкатка", RelativeDate.Description, "Завтра"},
		{"Как решили на этой неделе", RelativeDate.Description, "на этой неделе"},
		{"Вчерашний релиз", RelativeDate.Description, "Вчерашний"},
		{"Быстрее в 3,5 раза", DecimalComma.Description, "3,5"},
		{"Готово — выкачено", EmDash.Description, "Готово — выкачено"},
		{"Статус «в работе»", TypographicQuotes.Description, "«в работе»"},
		{"Перезаписывает, а не дописывает", ButNot.Description, "Перезаписывает, а не дописывает"},
		{"Ошибка не в коде, а данные", NotXButY.Description, "не в коде, а данные"},
		{"Не баг, а фича", NotXButY.Description, "Не баг, а фича"},
		{"**Итог** — выкатка", EmDash.Description, "**Итог** — выкатка"},
		{"`api` — сервис", EmDash.Description, "`api` — сервис"},
		{"делим 10% — пополам", EmDash.Description, "10% — пополам"},
		{"| — |", EmDash.Description, "| — |"},
		{"срок 2–3 дня", EnDash.Description, "2–3"},
		{"ответ «it's done» пришёл", TypographicQuotes.Description, "«it's done»"},
		{"хвост цитаты»", TypographicQuotes.Description, "»"},
		{"Готово к среде", RelativeDate.Description, "к среде"},
		{"до среды решим", RelativeDate.Description, "до среды"},
		{"созвон в пн", RelativeDate.Description, "в пн"},
		{"вернёмся через две недели", RelativeDate.Description, "через две недели"},
		{"доделаем в эти выходные", RelativeDate.Description, "в эти выходные"},
		{"сдаём по пятницу", RelativeDate.Description, "по пятницу"},
		{"созвон в следующий понедельник", RelativeDate.Description, "в следующий понедельник"},
		{"выкатка в эту пятницу", RelativeDate.Description, "в эту пятницу"},
		{"встреча в следующую среду", RelativeDate.Description, "в следующую среду"},
		{"готово к пятнице", RelativeDate.Description, "к пятнице"},
		{"как и на прошлой неделе", RelativeDate.Description, "на прошлой неделе"},
		{"вернёмся через день", RelativeDate.Description, "через день"},
		{"вернёмся через месяц", RelativeDate.Description, "через месяц"},
		{"вернёмся через 2 недели", RelativeDate.Description, "через 2 недели"},
		{"сдадим к концу недели", RelativeDate.Description, "к концу недели"},
		{"со следующей недели новый график", RelativeDate.Description, "со следующей недели"},
		{"обсуждали на днях", RelativeDate.Description, "на днях"},
		{"в следующем месяце", RelativeDate.Description, "в следующем месяце"},
		{"цитата „в работе“", TypographicQuotes.Description, "„в работе“"},
		{"план – факт", EnDash.Description, "план – факт"},
		{"| Эта неделя | релиз |", RelativeDate.Description, "Эта неделя"},
		{"| следующая неделя | тест |", RelativeDate.Description, "следующая неделя"},
		{"план к следующей неделе", RelativeDate.Description, "к следующей неделе"},
		{"до конца следующей недели", RelativeDate.Description, "следующей недели"},
		{"на текущей неделе", RelativeDate.Description, "на текущей неделе"},
		{"в этом месяце", RelativeDate.Description, "в этом месяце"},
		{"в прошлом месяце", RelativeDate.Description, "в прошлом месяце"},
		{"со следующего квартала", RelativeDate.Description, "со следующего квартала"},
		{"в следующем году", RelativeDate.Description, "в следующем году"},
		{"в начале месяца", RelativeDate.Description, "в начале месяца"},
		{"в конце недели", RelativeDate.Description, "в конце недели"},
		{"до конца года", RelativeDate.Description, "до конца года"},
		{"к концу квартала", RelativeDate.Description, "к концу квартала"},
		{"вернёмся через полгода", RelativeDate.Description, "через полгода"},
		{"вернёмся через год", RelativeDate.Description, "через год"},
		{"Через два дня", RelativeDate.Description, "Через два дня"},
		{"через три месяца", RelativeDate.Description, "через три месяца"},
		{"через пару лет", RelativeDate.Description, "через пару лет"},
		{"через полтора месяца", RelativeDate.Description, "через полтора месяца"},
		{"через 2-3 недели", RelativeDate.Description, "через 2-3 недели"},
		{"через 23 дня", RelativeDate.Description, "через 23 дня"},
		{"2-3 недели назад", RelativeDate.Description, "2-3 недели назад"},
		{"несколько дней назад", RelativeDate.Description, "несколько дней назад"},
		{"месяц назад", RelativeDate.Description, "месяц назад"},
		{"ближайшие 2 недели", RelativeDate.Description, "ближайшие 2 недели"},
		{"позавчера обсудили", RelativeDate.Description, "позавчера"},
		{"сегодняшний созвон", RelativeDate.Description, "сегодняшний"},
		{"завтрашняя выкатка", RelativeDate.Description, "завтрашняя"},
		{"на этих выходных", RelativeDate.Description, "на этих выходных"},
		{"в ближайшие выходные", RelativeDate.Description, "в ближайшие выходные"},
		{"в прошлые выходные", RelativeDate.Description, "в прошлые выходные"},
		{"перенесли на понедельник", RelativeDate.Description, "на понедельник"},
		{"перенесли на среду", RelativeDate.Description, "на среду"},
		{"созвон во вт", RelativeDate.Description, "во вт"},
		{"созвон в чт", RelativeDate.Description, "в чт"},
		{"созвон в пятницу / zoom", RelativeDate.Description, "в пятницу"},
		{"созвон в пятницу утром", RelativeDate.Description, "в пятницу"},
		{"Завтра, выкатка", RelativeDate.Description, "Завтра"},
		{"в пятницу 14.11 созвон, итоги в понедельник", RelativeDate.Description, "в понедельник"},
		{"задача _PROJ-19_ в работе", UnlinkedKeyDescription, "PROJ-19"},
		{"x[PROJ-1](https://x/PROJ-1)PROJ-2", UnlinkedKeyDescription, "PROJ-2"},
	}
	for _, c := range cases {
		findings := CheckDoc(c.line, projKeys)
		i := slices.IndexFunc(findings, func(f Finding) bool { return f.Marker.Description == c.want })
		if i < 0 {
			t.Errorf("%q: нет маркера %q, найдено %v", c.line, c.want, descriptions(findings))
			continue
		}
		if findings[i].Fragment != c.fragment {
			t.Errorf("%q: фрагмент %q, нужно %q", c.line, findings[i].Fragment, c.fragment)
		}
	}
}

// Слова, похожие на относительную дату, — не находка
func TestCheckDocRelativeDateLookalikes(t *testing.T) {
	for _, line := range []string{
		"Обсудили завтрак команды",
		"Пятничный релиз отменён",
		"Среда разработки готова",
		"в пятницу 14.11 созвон",
		"PROJ-nn - шаблон ключа",
		"APROJ-12 не наш ключ", "OTHER-5 чужой проект",
		"Мне сказали, а потом передумали",
		"созвон в пт 14.11",
		"в среде стейджинга",
		"созвоны по средам и по четвергам",
		"релиз: понедельник и четверг, 15:00",
		"Пятница - день релиза",
		"Начисления в выходные",
		"дежурства по воскресеньям",
		"созвон в пятницу\t14.11",
		"Задача не была закрыта в срок из-за долгого согласования с заказчиком, а затем ушла в отпуск",
		"ключ SUB-PROJ-1 составной",
		"этапы квартала согласованы",
		"[PROJ-25][ref] и сноска",
		"[PROJ-6]: https://x/PROJ-6",
		"на понедельник 17.11 перенесли",
		"в пятницу 09.11 созвон",
		"в среду 9 декабря",
		"конкурс поэта недели",
	} {
		if findings := CheckDoc(line, projKeys); len(findings) > 0 {
			t.Errorf("%q: ложное срабатывание %v", line, descriptions(findings))
		}
	}
}

// Строки находок — свои: номер ведёт к месту правки
func TestCheckDocReportsLineNumbers(t *testing.T) {
	doc := "## Итог\n" +
		"PROJ-34 без ссылки\n" +
		"созвон в пятницу\n" +
		"завтра выкатка\n" +
		"в 3,5 раза\n" +
		"готово — выкачено\n" +
		"в пятницу 14.11 — не находка даты"
	var got []string
	for _, f := range CheckDoc(doc, projKeys) {
		got = append(got, f.Marker.Description+"@"+string(rune('0'+f.Line)))
	}
	want := []string{
		UnlinkedKeyDescription + "@2",
		RelativeDate.Description + "@3",
		RelativeDate.Description + "@4",
		DecimalComma.Description + "@5",
		EmDash.Description + "@6",
		EmDash.Description + "@7",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("находки %v, нужно %v", got, want)
	}
}

// projKeys — префиксы ключей проекта в тестах
var projKeys = []string{"PROJ"}

// Без префиксов находка — любой ключ вида ABC-12, внутри ссылки — нет
func TestCheckDocAnyKey(t *testing.T) {
	findings := CheckDoc("OTHER-5 и [PROJ-1](https://x/PROJ-1)", nil)
	if len(findings) != 1 || findings[0].Fragment != "OTHER-5" {
		t.Fatalf("находки %+v, нужно одна OTHER-5", findings)
	}
}

// Оборот — одна находка, в какой бы форме он ни был
func TestCheckDocOneFindingPerTurn(t *testing.T) {
	for _, line := range []string{"Это не баг кода, а данные", "Не из списка, а из ответа"} {
		if findings := CheckDoc(line, projKeys); len(findings) != 1 {
			t.Errorf("%q: находки %v, нужна одна", line, descriptions(findings))
		}
	}
}

// Ключ по умолчанию — заглавные буквы, дефис, число: одна буква не ключ
func TestCheckDocDefaultKeyShape(t *testing.T) {
	if findings := CheckDoc("пункт A-1 и B2-3", nil); len(findings) != 1 || findings[0].Fragment != "B2-3" {
		t.Fatalf("находки %+v, нужна одна B2-3", findings)
	}
}

// Префикс проекта — буквально: «.» в префиксе не любой символ
func TestCheckDocKeyPrefixLiteral(t *testing.T) {
	if findings := CheckDoc("ключ AXB-1", []string{"A.B"}); len(findings) > 0 {
		t.Fatalf("префикс как шаблон: %+v", findings)
	}
}
