package questioncheck

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aiseeq/chooks/internal/tailscan"

	"github.com/aiseeq/akit/transcript"
)

// Tail — то, что проверке нужно из транскрипта
type Tail struct {
	// AssistantUUID — uuid последней записи ассистента с текстом: сообщение,
	// на котором сработал хук. По нему итог недели находит, что было дальше
	AssistantUUID string
	// HumanReply — последняя реплика человека (не сообщение другой сессии)
	HumanReply string
	// HumanFound — реплика найдена в пределах окна
	HumanFound bool
	// PrevAnswer — предыдущий ответ агента: последний текст, которым кончился
	// его ход (stop_reason end_turn), кроме проверяемого. Проверяемого ответа в
	// транскрипте к Stop-хуку ещё нет, а рекомендация к вопросу «жду твоего
	// ответа» бывает в прошлом ответе (пользователь 25.10, случай 19:01)
	PrevAnswer string
	// PrevFound — предыдущий ответ найден в пределах окна
	PrevFound bool
	// PrevAfterHuman — предыдущий ответ написан после реплики человека: она на
	// него не отвечала (lead 27.10 23:31, «"Делаем так?" - да» на другой вопрос)
	PrevAfterHuman bool
}

// ReadTail читает хвост транскрипта: uuid последнего текста ассистента,
// последнюю реплику человека — набранную в приглашении (type user,
// origin human) или поставленную в очередь посреди хода (queued_command) — и
// предыдущий ответ агента, отличный от проверяемого answer. Окно растёт, пока
// не найдены прошлая реплика человека и предыдущий ответ агента
func ReadTail(path, answer string) (Tail, error) {
	var tail Tail
	err := tailscan.Scan(path, func(buf []byte) bool {
		tail = scanTail(buf, answer)
		return tail.HumanFound && tail.PrevFound
	})
	if err != nil {
		return Tail{}, err
	}
	return tail, nil
}

// scanTail идёт по строкам с конца. uuid текста ассистента берётся только
// после реплики человека, предыдущий ответ — и до неё
func scanTail(buf []byte, answer string) Tail {
	var tail Tail
	answer = strings.TrimSpace(answer)
	lines := bytes.Split(buf, []byte("\n"))
	for i := len(lines) - 1; i >= 0 && !(tail.HumanFound && tail.PrevFound); i-- {
		line := lines[i]
		// Разбор JSON только у строк-кандидатов: в хвосте мегабайты служебных
		// записей (api-request-blob), их разбор съел бы бюджет хука
		wantUUID := !tail.HumanFound && tail.AssistantUUID == ""
		wantAssistant := (wantUUID || !tail.PrevFound) && bytes.Contains(line, []byte(`"type":"assistant"`))
		wantHuman := !tail.HumanFound && bytes.Contains(line, []byte(`"kind":"human"`))
		if !wantAssistant && !wantHuman {
			continue
		}
		// Строки разбираются по одной: состояние разбора (имена инструментов
		// по id вызова) реплике и тексту ответа не нужно. Нечитаемая строка —
		// обрезанная первая в окне или последняя, которую ещё дописывают, — без событий
		if events, err := transcript.NewParser().Parse(line); err == nil {
			tail.take(events, wantUUID, wantHuman, answer)
		}
	}
	// Реплики нет в окне — порядок неизвестен
	tail.PrevAfterHuman = tail.PrevAfterHuman && tail.HumanFound
	return tail
}

// take берёт из событий одной записи текст ответа ассистента и реплику человека
func (tail *Tail) take(events []transcript.Event, wantUUID, wantHuman bool, answer string) {
	var texts []string
	var reply *transcript.Reply
	for i := range events {
		event := &events[i]
		switch {
		case event.Sidechain:
		case event.Role == transcript.Agent:
			texts = append(texts, event.Text)
			reply = event.Reply
		case event.Role == transcript.User && wantHuman:
			tail.HumanReply = event.Text
			tail.HumanFound = true
		}
	}
	if len(texts) > 0 {
		tail.takeAssistant(strings.Join(texts, "\n"), events[0].UUID, reply.StopReason, wantUUID, answer)
	}
}

// takeAssistant берёт из ответа ассистента uuid текста (если ещё нужен) и
// предыдущий ответ. Проверяемый ответ, успевший попасть в транскрипт, — не
// предыдущий
func (tail *Tail) takeAssistant(text, uuid, stopReason string, wantUUID bool, answer string) {
	if wantUUID {
		tail.AssistantUUID = uuid
	}
	if !tail.PrevFound && stopReason == "end_turn" && strings.TrimSpace(text) != answer {
		tail.PrevAnswer = text
		tail.PrevFound = true
		// Хвост читается с конца: реплика ещё не найдена — значит, она раньше
		tail.PrevAfterHuman = !tail.HumanFound
	}
}

// replaySlack — допуск ReplayTail: время журнала — начало хука до секунды, а
// запись ответа в транскрипте бывает в той же секунде позже
const replaySlack = 2 * time.Second

// ReplayTail восстанавливает по транскрипту то, что видел хук в момент at
// (время записи журнала): проверяемый ответ — последний текст хода (end_turn)
// не позже at с допуском replaySlack, хвост —
// как у ReadTail, если бы транскрипт кончался этим ответом. Нужен стенду,
// чтобы прогнать нового судью по записям журнала
func ReplayTail(path string, at time.Time) (Tail, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Tail{}, "", fmt.Errorf("read transcript: %w", err)
	}
	end, answer := -1, ""
	at = at.Add(replaySlack)
	for start := 0; start < len(data); {
		next := bytes.IndexByte(data[start:], '\n')
		if next < 0 {
			next = len(data) - start
		}
		line := data[start : start+next]
		start += next + 1
		if !bytes.Contains(line, []byte(`"type":"assistant"`)) || !bytes.Contains(line, []byte(`"end_turn"`)) {
			continue
		}
		events, err := transcript.NewParser().Parse(line)
		if err != nil || len(events) == 0 || events[0].Time.After(at) {
			continue
		}
		var texts []string
		for i := range events {
			event := &events[i]
			if event.Role == transcript.Agent && !event.Sidechain && event.Reply != nil && event.Reply.StopReason == "end_turn" && strings.TrimSpace(event.Text) != "" {
				texts = append(texts, event.Text)
			}
		}
		if len(texts) > 0 {
			end, answer = start, strings.Join(texts, "\n")
		}
	}
	if end < 0 {
		return Tail{}, "", fmt.Errorf("no finished turn before %s in %s", at.Format(time.RFC3339), path)
	}
	buf := tailscan.Clip(data[:min(end, len(data))])
	return scanTail(buf, strings.TrimSpace(answer)), strings.TrimSpace(answer), nil
}
