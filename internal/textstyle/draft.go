package textstyle

import (
	"regexp"
	"slices"
	"strings"
)

// Маркеры только для черновика: команду style запускает сам агент на голом
// тексте, кода рядом нет, поэтому проверки здесь строже, чем у хука
var (
	EnDash = Marker{
		Description: "короткое тире",
		Fix:         "обычный дефис с пробелами: ' - '",
		Find:        RegexpFinder(regexp.MustCompile(`\S*[ \t]*–[ \t]*\S*`)),
	}
	// DecimalComma в черновике десятичная запятая почти всегда ошибка: хук
	// Jira её не ловит, там она цитирует формат сумм в интерфейсе ("1.000,00")
	DecimalComma = Marker{
		Description: "десятичная запятая",
		Fix:         "десятичная точка: '2.5'",
		Find:        RegexpFinder(regexp.MustCompile(`\p{Nd}+,\p{Nd}+`)),
	}
	MarkdownTable = Marker{
		Description: "markdown-таблица",
		Fix:         "строки '- ' или текст без таблицы",
		Find:        RegexpFinder(regexp.MustCompile(`^\s*\|.*\|\s*$`)),
	}
	MarkdownLink = Marker{
		Description: "markdown-ссылка",
		Fix:         "голая ссылка или ключ тикета",
		Find:        RegexpFinder(regexp.MustCompile(`\[[^\]\n]+\]\([^)\s]+\)`)),
	}
	// Emoji иконки: эмоция в его текстах - =), ;), %), =(
	Emoji = Marker{
		Description: "эмодзи",
		Fix:         "без иконок, эмоция - =) ;) %) =(",
		Find:        RegexpFinder(regexp.MustCompile(`[\x{1F300}-\x{1FAFF}\x{2600}-\x{27BF}\x{2B50}\x{2B55}\x{2705}\x{274C}]`)),
	}
	Preamble = Marker{
		Description: "предисловие или мета-фраза",
		Fix:         "сразу суть, без вступления и прощания",
		Find:        RegexpFinder(regexp.MustCompile(`(?i)` + wordBoundary + `(отличный вопрос|хороший вопрос|давай разбер[её]мся|скажу честно|надеюсь,? (?:это )?помож\p{L}*|дай знать,? если)`)),
	}
	// ReportOpening первое слово - отчёт о своём действии вместо сути
	ReportOpening = Marker{
		Description: "отчёт о своём действии в начале",
		Fix:         "сразу суть: что найдено или что делать",
		Find:        RegexpFinder(regexp.MustCompile(`^\s*(?:Нашёл|Нашел|Разобрался|Посмотрел|Понял)\.`)),
	}
)

// DraftMarkers маркеры черновика: все маркеры хука Jira, кроме приветствия
// (в личке "Привет." первой репликой дня - правило), и маркеры черновика
var DraftMarkers = []Marker{
	EmDash, EnDash, Bold, Heading, MarkdownTable, MarkdownLink, TypographicQuotes,
	ButNot, ThisIsNotBut, RepeatedPreposition, NumberWord, DecimalComma,
	Emoji, Preamble, ReportOpening,
}

// Finding нарушение в строке черновика
type Finding struct {
	Line     int
	Marker   Marker
	Fragment string
}

// anyIssueURL адрес задачи для префикса без адреса: чужой адрес в
// подсказке увёл бы ссылку в другой Jira
const anyIssueURL = "https://…/browse/"

// DraftKey ключ задачи вне URL в черновике чата или письма: людям ключ —
// ссылкой, а markdown в черновике запрещён (MarkdownLink), поэтому голым URL
// с самим ключом. Поиск — тот же, что у UnlinkedKey; issueURLs — адрес
// задачи без ключа по префиксу, префикс без адреса — шаблон адреса
func DraftKey(prefixes []string, issueURLs map[string]string) Marker {
	m := UnlinkedKey(prefixes)
	m.Fix = "голый URL: '" + anyIssueURL + "<ключ>'"
	m.FixFor = func(key string) string {
		prefix, _, _ := strings.Cut(key, "-")
		issueURL := issueURLs[prefix]
		if issueURL == "" {
			issueURL = anyIssueURL
		}
		return "голый URL: '" + issueURL + key + "'"
	}
	return m
}

// CheckDraftKeys проверяет черновик маркерами черновика и ключом задачи.
// prefixes пусто — ключ не ищется: в чате любой «ABC-12» (UTF-8, SHA-256)
// был бы ложной находкой
func CheckDraftKeys(text string, prefixes []string, issueURLs map[string]string) []Finding {
	if len(prefixes) == 0 {
		return CheckDraft(text)
	}
	return check(text, append(slices.Clip(DraftMarkers), DraftKey(prefixes, issueURLs)))
}

// CheckDraft проверяет черновик построчно: номер строки ведёт к месту правки,
// а оборот через перенос строки в коротком сообщении почти не встречается
func CheckDraft(text string) []Finding {
	return check(text, DraftMarkers)
}

// check ищет маркеры построчно
func check(text string, markers []Marker) []Finding {
	var findings []Finding
	for i, line := range strings.Split(text, "\n") {
		for _, marker := range markers {
			for _, fragment := range marker.fragments(line) {
				findings = append(findings, Finding{Line: i + 1, Marker: marker, Fragment: strings.TrimSpace(fragment)})
			}
		}
	}
	return findings
}
