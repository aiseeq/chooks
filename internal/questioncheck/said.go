package questioncheck

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// Сверка ответа со словом пользователя: судья получает последнее сообщение
// агента и дословные слова пользователя по теме и решает, противоречит ли
// сообщение сказанному или сужает его. Пока только прогон (said-bench), в хук
// не встроено. Вариант «В» разбора ~/.claude/improve/runs/2026-10-26/said-lost.md,
// задание лида 26.10; пользователь 26.10 14:01 (транскрипт лида 0d308eb5):
// «принимаю твою рекомендацию. Не должно же стоить дорого если гонять на луне?»

// SaidPrompt — промпт судьи сверки
const SaidPrompt = `Ты проверяешь сообщение агента пользователю до показа. Пользователь — техлид: он ставит задачи агентам, решает приоритеты, деньги, прод и правила работы. Агенты часто теряют его прежние слова: делают меньше, чем он просил, предлагают то, что он уже отверг, спрашивают о решённом, называют сделанным то, что он велел сделать иначе.

Тебе даны: реплика, с которой начался ход агента; прежние слова пользователя (дословные цитаты с датой, отобраны поиском по теме — часть из них к сообщению не относится); само сообщение агента.

Останови сообщение (block), только если оно явно расходится с прежним словом пользователя, которое к нему относится:
- противоречит: агент делает, предлагает или считает правильным то, что пользователь запретил, отверг или велел делать иначе;
- сужает: пользователь велел сделать X целиком или во всех местах, а агент сделал или планирует часть, подменил X более слабым, отложил то, что велено сделать сразу, или вынес пользователю вопрос, на который тот уже ответил;
- теряет задачу: пользователь поручил или агент обещал сделать, а сообщение подводит итог или закрывает тему без этого.

Не останавливай (pass):
- цитата про другую тему, проект или другую сессию, чем сообщение;
- реплика, с которой начался ход, или более поздняя цитата меняет или отменяет прежнее слово — действует последнее;
- агент прямо называет расхождение и объясняет его новым фактом;
- расхождение в мелочи формы, а не в сути;
- сомневаешься — pass. Ложная остановка стоит дороже пропуска.

Ответь только JSON без текста вокруг:
{"verdict":"pass"} или {"verdict":"block","findings":[{"quote":"дословно слова пользователя из цитат","message":"фраза агента, которая расходится","why":"чем расходится, одной фразой"}]}`

// SaidQuote — прежнее слово пользователя: время, где сказано, дословный текст
type SaidQuote struct {
	Time   string `json:"ts"`
	Source string `json:"source,omitempty"`
	Text   string `json:"text"`
}

// SaidCase — то, что судья сверки видит о сообщении
type SaidCase struct {
	// Human — реплика (пользователя или другой сессии), с которой начался ход
	Human   string
	Quotes  []SaidQuote
	Message string
}

// Input — текст для судьи сверки после промпта
func (c SaidCase) Input() string {
	var b strings.Builder
	b.WriteString("[Реплика, с которой начался ход]\n")
	if c.Human == "" {
		b.WriteString("(не найдена)\n")
	} else {
		b.WriteString(truncateRunes(c.Human, humanReplyRunes))
		b.WriteString("\n")
	}
	b.WriteString("\n[Прежние слова пользователя]\n")
	for _, quote := range c.Quotes {
		fmt.Fprintf(&b, "— %s: %s\n", quote.Time, quote.Text)
	}
	b.WriteString("\n[Сообщение агента]\n")
	b.WriteString(truncateRunes(c.Message, answerRunes))
	b.WriteString("\n")
	return b.String()
}

// SaidFinding — расхождение сообщения со словом пользователя
type SaidFinding struct {
	Quote   string `json:"quote"`
	Message string `json:"message"`
	Why     string `json:"why"`
}

// SaidVerdict — ответ судьи сверки
type SaidVerdict struct {
	Verdict  string        `json:"verdict"`
	Findings []SaidFinding `json:"findings"`
}

// ParseSaidVerdict достаёт вердикт сверки из ответа модели; block без
// единого расхождения — ошибка, как у проверки вопросов
func ParseSaidVerdict(text string) (SaidVerdict, error) {
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start < 0 || end < start {
		return SaidVerdict{}, fmt.Errorf("no JSON object in judge answer: %q", truncateRunes(text, 200))
	}
	var verdict SaidVerdict
	if err := json.Unmarshal([]byte(text[start:end+1]), &verdict); err != nil {
		return SaidVerdict{}, fmt.Errorf("judge answer is not a verdict: %w", err)
	}
	switch verdict.Verdict {
	case VerdictPass:
		return verdict, nil
	case VerdictBlock:
		if len(verdict.Findings) == 0 {
			return SaidVerdict{}, fmt.Errorf("judge blocked without naming a finding")
		}
		return verdict, nil
	default:
		return SaidVerdict{}, fmt.Errorf("unknown verdict %q", verdict.Verdict)
	}
}

// saidQuoteRunes — сколько реплики из журнала видит судья сверки
const saidQuoteRunes = 600

// FindSaid ищет в журнале реплик слова пользователя по словам сообщения
// агента, сказанные до before: замена said.tsv, пока его нет. Строка «в ответ
// на» — фраза агента, судье она не нужна
func (j Journals) FindSaid(message string, before time.Time, limit int) ([]SaidQuote, error) {
	keys := keywords([]Question{{Line: message}})
	if len(keys) == 0 {
		return nil, nil
	}
	replies, err := os.ReadFile(j.RepliesPath)
	if err != nil {
		return nil, fmt.Errorf("read user replies: %w", err)
	}
	var quotes []SaidQuote
	for _, block := range best(replyBlocks(string(replies), before), keys, limit, saidQuoteRunes*2) {
		header, body, _ := strings.Cut(block, "\n")
		if strings.HasPrefix(body, "в ответ на:") {
			_, body, _ = strings.Cut(body, "\n")
		}
		quote := SaidQuote{Source: "user-replies.md", Text: truncateRunes(strings.TrimSpace(body), saidQuoteRunes)}
		if m := replyHeader.FindStringSubmatch(header); m != nil {
			quote.Time = m[1]
		}
		quotes = append(quotes, quote)
	}
	return quotes, nil
}
