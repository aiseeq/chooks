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
