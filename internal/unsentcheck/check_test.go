package unsentcheck

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Ответы сухого прогона work-admin 29.10 (hits.txt), остановленные бы им
func TestClaim_DryRunStops(t *testing.T) {
	stops := []string{
		// 26.10, админ ~/work, четырежды
		"**Ещё не отправлено:** ответ Пете про шаблоны, текст выше.",
		"**Ещё не отправлено:** ⏎ - Пете ответ про шаблоны;",
		// 27.10
		"- **Оля.** Ответ ей ты ещё не отправил, текст выше.",
		// 29.10, Ане
		"Ещё не отправлен ответ Ане в чат «Клиент - задачи», текст выше без изменений.",
		"Ответ Ане ещё не отправлен, текст в моём прошлом сообщении.",
		// Формы, названные в описании пользователю
		"Черновик Оле ещё не ушёл.",
		"Текст для Вани не ушло",
	}
	for _, answer := range stops {
		if Claim(answer) == "" {
			t.Errorf("фраза не найдена: %q", answer)
		}
	}
}

func TestClaim_FalseStops(t *testing.T) {
	// 24.10 у лида: описание механизма, полная форма причастия — не статус
	if got := Claim("Набранный, но не отправленный текст защищён этими 10 минутами без ввода."); got != "" {
		t.Errorf("описание механизма принято за статус: %q", got)
	}
	for _, answer := range []string{"Список не отправленных черновиков пуст.", "Счёт ещё не отправлен провайдеру.", "Ответ Ане отправлен в 10:05."} {
		if got := Claim(answer); got != "" {
			t.Errorf("%q: найдено %q", answer, got)
		}
	}
	// glint 27.10: «ответ не отправлен» про письмо сессии. Регэксп его не
	// отличает: агент получает подсказку и заканчивает ход как есть
	if Claim("Сообщение пришло от proj, но сессии с таким именем нет, так что ответ не отправлен.") == "" {
		t.Error("ожидалась находка: регэксп не различает адресата")
	}
}

const (
	human      = `{"type":"user","uuid":"u1","timestamp":"2026-10-29T10:00:00Z","origin":{"kind":"human"},"message":{"content":"что с ответом Ане?"}}`
	peer       = `{"type":"attachment","uuid":"a1","timestamp":"2026-10-29T10:00:00Z","attachment":{"type":"queued_command","prompt":"письмо сессии","origin":{"kind":"peer"}}}`
	history    = `{"type":"assistant","uuid":"m1","timestamp":"2026-10-29T10:01:00Z","message":{"stop_reason":"tool_use","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"reddy history 'Клиент - задачи' | tail -20"}}]}}`
	otherBash  = `{"type":"assistant","uuid":"m2","timestamp":"2026-10-29T10:02:00Z","message":{"stop_reason":"tool_use","content":[{"type":"tool_use","id":"t2","name":"Bash","input":{"command":"reddy unread"}}]}}`
	toolResult = `{"type":"user","uuid":"r2","timestamp":"2026-10-29T10:02:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"t2","content":"history пуста"}]}}`
	notice     = `{"type":"user","uuid":"n1","timestamp":"2026-10-29T10:03:00Z","origin":{"kind":"task-notification"},"message":{"content":"<task-notification>…"}}`
	// Запрос claude -p и SDK пишется без origin
	program = `{"type":"user","uuid":"p1","timestamp":"2026-10-29T10:00:00Z","entrypoint":"sdk-cli","message":{"content":"что с ответом?"}}`
	// Правка тикета, где слова reddy и history — текст, а не вызов
	ticketEdit = `{"type":"assistant","uuid":"m3","timestamp":"2026-10-29T10:02:00Z","message":{"stop_reason":"tool_use","content":[{"type":"tool_use","id":"t3","name":"Bash","input":{"command":"cat >> docs/tasks/t.md <<'EOF'\n- ход без reddy history — блок\nEOF"}}]}}`
)

func TestChatCall(t *testing.T) {
	calls := []string{
		// Формы из транскриптов за 7 дней (независимая проверка 29.10)
		"R=/home/aisee/work/reddy/bin/reddy; $R history 2783119 --since '2026-10-29'",
		`R="/home/aisee/work/reddy/bin/reddy" && "${R}" history Оля`,
		"which reddy && timeout 60 reddy history Оля",
		"for c in 1 2; do reddy history $c; done",
		`"$HOME/bin/reddy" history Оля`,
		"for c in 1 2; do ./bin/reddy history $c; done",
		"cd ~/work\nreddy history Оля | tail",
		`R=/home/aisee/work/reddy/bin/reddy; f(){ $R history $1 --since "$2"; }; f 1 2`,
		"reddy history 'Клиент - задачи'",
		"reddy history --since 1d Оля | tail -20",
		"cd ~ && reddy history Оля",
		"reddy --json history Оля",
		"/home/aisee/bin/reddy history Оля",
		`echo "$(reddy history Оля)"`,
	}
	for _, command := range calls {
		if !chatCall(command) {
			t.Errorf("вызов не узнан: %q", command)
		}
	}
	texts := []string{
		"reddy unread",
		"sub 'reddy history' 'x' check.go",
		"cat >> t.md <<'EOF'\nход без reddy history — блок\nEOF",
		`claude -p "Сначала выполни Bash: reddy history Оля"`,
		"git commit -m 'unsent-check: reddy history в ходе'",
		"R=/home/aisee/work/reddy/bin/reddy; $R chats; echo history",
		"R=/home/aisee/work/reddy/bin/reddy; $RX history",
		// Строка heredoc с примером вызова в правке теста
		"sub <<'EOF'\ncalls := []string{\n\t\"reddy history 'Клиент - задачи'\",\n}\nEOF",
	}
	for _, command := range texts {
		if chatCall(command) {
			t.Errorf("текст принят за вызов: %q", command)
		}
	}
}

func writeTranscript(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRun(t *testing.T) {
	const answer = "Ответ Ане ещё не отправлен, текст выше."
	cases := []struct {
		name      string
		event     Event
		lines     []string
		wantBlock bool
		wantSkip  string
	}{
		{name: "чат открыт в ходе", event: Event{LastMessage: answer}, lines: []string{human, history, otherBash, toolResult, notice}},
		{name: "чат не открыт", event: Event{LastMessage: answer}, lines: []string{human, otherBash, toolResult, notice}, wantBlock: true},
		{name: "чат открыт в прошлом ходе", event: Event{LastMessage: answer}, lines: []string{history, human, otherBash}, wantBlock: true},
		{name: "письмо сессии начинает ход", event: Event{LastMessage: answer}, lines: []string{human, history, peer, otherBash}, wantBlock: true},
		{name: "начала хода нет", event: Event{LastMessage: answer}, lines: []string{history, otherBash}},
		{name: "запрос программы начинает ход", event: Event{LastMessage: answer}, lines: []string{program, history, program, otherBash}, wantBlock: true},
		{name: "чат открыт в ходе программы", event: Event{LastMessage: answer}, lines: []string{program, history, otherBash}},
		{name: "reddy history в тексте правки", event: Event{LastMessage: answer}, lines: []string{human, ticketEdit}, wantBlock: true},
		{name: "фразы нет", event: Event{LastMessage: "Ответ Ане отправлен."}, lines: []string{human}},
		{name: "повторный Stop", event: Event{LastMessage: answer, StopHookActive: true}, lines: []string{human}, wantSkip: "stop hook active"},
		{name: "субагент", event: Event{LastMessage: answer, AgentID: "a1"}, lines: []string{human}, wantSkip: "subagent"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.event.TranscriptPath = writeTranscript(t, tc.lines...)
			got, err := Run(tc.event)
			if err != nil {
				t.Fatal(err)
			}
			if got.Block != tc.wantBlock || got.Skip != tc.wantSkip {
				t.Fatalf("Block = %v, Skip = %q; want %v, %q", got.Block, got.Skip, tc.wantBlock, tc.wantSkip)
			}
			if got.Block && !strings.Contains(got.Reason, "reddy history") {
				t.Errorf("подсказка без reddy history: %q", got.Reason)
			}
		})
	}
}

func TestRun_MissingTranscript(t *testing.T) {
	_, err := Run(Event{LastMessage: "Ещё не отправлено: ответ Оле", TranscriptPath: filepath.Join(t.TempDir(), "нет.jsonl")})
	if err == nil {
		t.Fatal("ожидалась ошибка чтения транскрипта")
	}
}

func TestChatOpened_BeyondFirstWindow(t *testing.T) {
	var b strings.Builder
	b.WriteString(human + "\n" + history + "\n")
	filler := `{"type":"assistant","uuid":"f","timestamp":"2026-10-29T10:01:00Z","message":{"content":[{"type":"text","text":"` + strings.Repeat("ж", 1000) + `"}]}}` + "\n"
	for b.Len() < 3*tailWindowStart {
		b.WriteString(filler)
	}
	path := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	checked, err := ChatOpened(path)
	if err != nil {
		t.Fatal(err)
	}
	if !checked {
		t.Error("reddy history за первым окном не найден")
	}
}
