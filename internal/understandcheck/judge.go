// Package understandcheck — проверка понимания: свежий судья без контекста
// сверяет действие агента, которое уносит слово пользователя дальше (письмо
// другой сессии, бриф суб-агенту, правка инструкций), с дословной репликой
// пользователя. Идея пользователя 02.11 (сессия лида, tl 2a879c2d:987):
// «агент читает мои слова будучи в контексте… я понял эту фразу так то, буду
// делать то-то — этот текст поступает агенту без контекста и он проводит чек»
package understandcheck

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aiseeq/akit/secretmask"
	"github.com/aiseeq/chooks/internal/questioncheck"
)

// Prompt — системный промпт судьи
const Prompt = `Ты проверяешь, верно ли агент понял реплику пользователя, до того как его действие унесёт её смысл дальше: письмо другой сессии, задание суб-агенту или правка инструкций агентов (правила, скиллы, память, журнал решений). Пользователь — техлид, руководит командой агентов. Контекста беседы у тебя нет, и это намеренно: ты видишь только реплику, конец сообщения агента, на которое пользователь отвечал, и само действие.

Ищи расхождения смысла действия с репликой, видимые из этих текстов:
- охват шире сказанного: «только тут / в этом проекте / этот раз» → «всем», «везде», «по умолчанию», «навсегда»;
- сила сильнее сказанного: «осторожнее» → «запрещено», «пауза» → «остановить сразу», «подумай» → «построй механизм», «можно» → «обязательно»;
- вопрос, мнение или размышление пользователя («а ты как считаешь?», «может, …?», «проверь, …?») исполнено как указание и разослано или записано правилом;
- к однозначному ответу добавлен свой вариант («по таймеру» → «по таймеру или сразу»), выбран не тот вариант из предложенных в конце сообщения агента;
- пользователю приписано то, чего в реплике нет: «решение пользователя: …», «пользователь сказал …» с добавленными утверждениями, причинами, сроками, адресатами;
- реплика отменяет или сужает что-то, а действие это сохраняет или расширяет.

Не придирайся:
- детали исполнения, имена, пути, команды, знание проекта, которых нет в реплике, — нормальная работа агента, если они не выданы за слово пользователя и не меняют его смысл;
- действие может быть частью работы агента, не связанной с этой репликой прямо (реплика — «да», «продолжай», вопрос о другом), — тогда ок;
- двусмысленная реплика, которую агент прочитал одним из разумных способов, — ок; поправка только если прочтение агента расходится с буквой реплики;
- если для вывода нужен контекст, которого нет в трёх текстах, — ок.

Ответ — только JSON, без текста вокруг:
{"verdict":"ok"}
или
{"verdict":"fix","fixes":[{"quote":"<дословный кусок реплики>","action":"<дословный кусок действия, который расходится>","why":"<в чём расхождение, одной фразой>","fix":"<как исправить действие>"}]}`

// Fix — поправка судьи: где действие расходится с репликой
type Fix struct {
	Quote  string `json:"quote"`
	Action string `json:"action"`
	Why    string `json:"why"`
	Fix    string `json:"fix"`
}

// Verdict — ответ судьи: ok или fix с поправками
type Verdict struct {
	Verdict string `json:"verdict"`
	Fixes   []Fix  `json:"fixes,omitempty"`
}

// ParseVerdict достаёт вердикт из ответа модели: JSON-объект, возможно в
// блоке кода или с текстом вокруг. Вердикт вне ok/fix и fix без поправки —
// ошибка: такой ответ не повод останавливать агента
func ParseVerdict(text string) (Verdict, error) {
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start < 0 || end < start {
		return Verdict{}, fmt.Errorf("no JSON object in judge answer: %q", clip(text, 200))
	}
	var verdict Verdict
	if err := json.Unmarshal([]byte(text[start:end+1]), &verdict); err != nil {
		return Verdict{}, fmt.Errorf("judge answer is not a verdict: %w", err)
	}
	switch verdict.Verdict {
	case "ok":
		verdict.Fixes = nil
		return verdict, nil
	case "fix":
		if len(verdict.Fixes) == 0 {
			return Verdict{}, fmt.Errorf("judge answered fix without a correction")
		}
		return verdict, nil
	default:
		return Verdict{}, fmt.Errorf("unknown verdict %q", verdict.Verdict)
	}
}

// Пределы частей входа судьи в рунах: длинная вставка пользователя или
// SKILL.md целиком не должны раздувать вызов
const (
	humanRunes  = 4000
	prevRunes   = 1500
	actionRunes = 6000
)

// Input — текст случая для судьи
func (c Case) Input() string {
	var b strings.Builder
	b.WriteString("== Конец сообщения агента, на которое отвечал пользователь\n")
	if c.Prev == "" {
		b.WriteString("(нет: реплика начала беседу)\n")
	} else {
		b.WriteString(clipHead(c.Prev, prevRunes))
		b.WriteString("\n")
	}
	b.WriteString("\n== Реплика пользователя дословно\n")
	b.WriteString(clip(c.Human, humanRunes))
	b.WriteString("\n\n== Действие агента: ")
	b.WriteString(c.Kind.Title())
	b.WriteString("\n")
	b.WriteString(clip(c.Action, actionRunes))
	b.WriteString("\n")
	return b.String()
}

// clip оставляет начало текста
var clip = questioncheck.Excerpt

// clipHead оставляет конец текста: судье нужен конец сообщения агента —
// вопрос или предложение, на которое ответил пользователь. Значения секретов
// скрываются до среза, как в questioncheck.Excerpt: иначе остался бы хвост ключа
func clipHead(text string, limit int) string {
	text, _ = secretmask.Mask(text)
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return "[обрезано]… " + string(runes[len(runes)-limit:])
}
