package textstyle

import (
	"slices"
	"testing"
)

func descriptions(findings []Finding) []string {
	var names []string
	for _, f := range findings {
		names = append(names, f.Marker.Description)
	}
	return names
}

func TestCheckDraftFindsFormViolations(t *testing.T) {
	cases := map[string]string{
		"Готово — выкачено":                EmDash.Description,
		"Готово – выкачено":                EnDash.Description,
		"**Итог:** сделано":                Bold.Description,
		"## Итог":                          Heading.Description,
		"| Что | Кто |":                    MarkdownTable.Description,
		"Смотри [тикет](https://x/PROJ-1)": MarkdownLink.Description,
		"Статус «в работе»":                TypographicQuotes.Description,
		"Перезаписывает, а не дописывает":  ButNot.Description,
		"Это не баг кода, а данные":        ThisIsNotBut.Description,
		"Не из списка, а из ответа":        RepeatedPreposition.Description,
		"Нашлось три расхождения":          NumberWord.Description,
		"Быстрее в 2,5 раза":               DecimalComma.Description,
		"Выкатил 🚀":                        Emoji.Description,
		"Отличный вопрос! Лимит сброшен":   Preamble.Description,
		"Дай знать, если что":              Preamble.Description,
		"Нашёл. Лимит сбрасывается в 4:00": ReportOpening.Description,
	}
	for text, want := range cases {
		got := descriptions(CheckDraft(text))
		if !slices.Contains(got, want) {
			t.Errorf("%q: нет маркера %q, найдено %v", text, want, got)
		}
	}
}

// Его обычные реплики проходят: приветствие в личке, дефис, смайл, числа
// цифрами, точка в дроби, перечень строками "- "
func TestCheckDraftAllowsHisVoice(t *testing.T) {
	draft := "Привет. Выкатил в тест - глянь, когда будет минутка =)\n" +
		"- лимит 1 платёж в день\n" +
		"- комиссия 1.5%, было 3%\n" +
		"Ок, тогда оставляю. Завтра на встрече как раз и зафиксим\n" +
		"PROJ-12, PROJ-13 и PROJ-14 посмотрю до созвона"
	if findings := CheckDraft(draft); len(findings) > 0 {
		t.Fatalf("ложные срабатывания: %+v", findings)
	}
}

// Подсказка ключа — голый URL с самим ключом: адрес своего префикса, без
// адреса — шаблон; маркеры без своей подсказки — прежний Fix. Fix ключа —
// тот же голый URL с шаблоном: его читает тот, кто не зовёт Hint
func TestDraftKeyHint(t *testing.T) {
	urls := map[string]string{"PROJ": "https://jira.example/browse/"}
	var got []string
	for _, f := range CheckDraftKeys("PROJ-12 и OPS-3 — PROJ-12", []string{"PROJ", "OPS"}, urls) {
		got = append(got, f.Fragment+": "+f.Marker.Hint(f.Fragment))
	}
	want := []string{
		"OPS-3 — PROJ-12: " + EmDash.Fix,
		"PROJ-12: голый URL: 'https://jira.example/browse/PROJ-12'",
		"OPS-3: голый URL: 'https://…/browse/OPS-3'",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("подсказки %v, нужно %v", got, want)
	}
	m := DraftKey([]string{"PROJ"}, urls)
	if want := "голый URL: 'https://…/browse/<ключ>'"; m.Fix != want {
		t.Errorf("Fix %q, нужно %q", m.Fix, want)
	}
	m.FixFor = nil
	if got := m.Hint("PROJ-12"); got != m.Fix {
		t.Errorf("Hint без FixFor %q, нужно Fix %q", got, m.Fix)
	}
}

// Адрес — по префиксу ключа целиком, какой бы длины префикс ни был: у
// каждого префикса свой адрес
func TestDraftKeyHintPrefixLength(t *testing.T) {
	urls := map[string]string{
		"PROJ": "https://jira.example/browse/",
		"OPS":  "https://ops.jira.example/browse/",
	}
	var got []string
	for _, f := range CheckDraftKeys("OPS-3 и PROJ-12", []string{"PROJ", "OPS"}, urls) {
		got = append(got, f.Marker.Hint(f.Fragment))
	}
	want := []string{
		"голый URL: 'https://ops.jira.example/browse/OPS-3'",
		"голый URL: 'https://jira.example/browse/PROJ-12'",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("подсказки %v, нужно %v", got, want)
	}
}

func TestCheckDraftReportsLineNumbers(t *testing.T) {
	findings := CheckDraft("Ок\nГотово — выкачено\n\nНашлось три расхождения")
	var lines []int
	for _, f := range findings {
		lines = append(lines, f.Line)
	}
	if !slices.Equal(lines, []int{2, 4}) {
		t.Fatalf("строки нарушений %v, нужно [2 4]", lines)
	}
}
