package questioncheck

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeBody — тело поддельного ключа: его начала нет нигде, кроме ключа
const fakeBody = "Qz7Xw2Rt9Lm4Vb6Nc1Pk8Jh3Gf5Ds0Ay"

// fakeKey собран из частей: целиком в исходнике его не должно быть
func fakeKey() string { return "sk_live" + "_" + fakeBody }

// assertNoSecret — в тексте нет ни ключа, ни его начала: срез посреди ключа
// оставил бы начало короче образца
func assertNoSecret(t *testing.T, where, text string) {
	t.Helper()
	for _, part := range []string{fakeBody[:4], "sk_live" + "_"} {
		if strings.Contains(text, part) {
			t.Errorf("%s: %q passed: %s", where, part, Excerpt(text, 300))
		}
	}
}

// straddling — ключ стоит так, что срез на limit приходится на его начало
func straddling(limit int) string {
	return strings.Repeat("я", limit-len("sk_live_Qz")) + fakeKey() + " хвост"
}

func TestCaseInputHidesSecrets(t *testing.T) {
	tests := map[string]Case{
		"реплика":          {HumanReply: "вот ключ " + fakeKey()},
		"прошлый ответ":    {PrevAnswer: "вот ключ " + fakeKey()},
		"ответ":            {Answer: "вот ключ " + fakeKey()},
		"срез реплики":     {HumanReply: straddling(humanReplyRunes)},
		"срез прошлого":    {PrevAnswer: straddling(prevAnswerRunes)},
		"срез ответа":      {Answer: straddling(answerRunes)},
		"выдержка реплики": {Excerpts: Excerpts{Replies: []string{"вот ключ " + fakeKey()}}},
		"выдержка решения": {Excerpts: Excerpts{Decisions: []string{"вот ключ " + fakeKey()}}},
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			got, _ := (&recordingCLI{}).capture(t, "opus", input.Input())
			assertNoSecret(t, name, got)
		})
	}
	// Срез сам по себе, без двери к модели: журнал пишет выдержки напрямую
	for name, text := range map[string]string{
		"реплика": (Case{HumanReply: straddling(humanReplyRunes)}).Input(),
		"ответ":   (Case{Answer: straddling(answerRunes)}).Input(),
	} {
		assertNoSecret(t, "вход без двери: "+name, text)
	}
}

func TestSaidInputHidesSecrets(t *testing.T) {
	quotes := []SaidQuote{{Time: "t", Text: Excerpt(straddling(saidQuoteRunes), saidQuoteRunes)}}
	for name, input := range map[string]SaidCase{
		"реплика":   {Human: straddling(humanReplyRunes), Quotes: quotes},
		"сообщение": {Message: straddling(answerRunes), Quotes: quotes},
		"ход":       {Turn: straddling(turnRunes), Quotes: quotes},
	} {
		assertNoSecret(t, name, input.Input())
	}
	record := SaidRecord{Time: time.Now(), Ref: "r", Kind: "rule", Quote: straddling(saidQuoteRunes)}
	assertNoSecret(t, "запись said", record.AsQuote().Text)
}

// Ключ в журналах: выдержка реплики, запись решения, поиск сверки
func TestJournalExcerptsHideSecrets(t *testing.T) {
	dir := t.TempDir()
	replies := filepath.Join(dir, "user-replies.md")
	decisions := filepath.Join(dir, "decisions.md")
	writeFile(t, replies, "# Реплики\n\n### 2026-10-20T14:01:16.007Z (20.10 16:01 CEST) · ~/work · claude x\n"+
		"в ответ на: Какой артефакт удалить?\nудали артефакт Xq7pTz3mKd9LbWv2, ключ "+fakeKey()+"\n\n")
	writeFile(t, decisions, "# Журнал решений\n\n## Записи\n\n## artifact-Xq7pTz3mKd9LbWv2\n- Решение: удалить, ключ "+fakeKey()+"\n")
	journals := Journals{RepliesPath: replies, DecisionsPath: decisions}

	excerpts, err := journals.Find([]Question{{Line: "Артефакт Xq7pTz3mKd9LbWv2 удалить?"}}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(excerpts.Replies) != 1 || len(excerpts.Decisions) != 1 {
		t.Fatalf("journals not found: %+v", excerpts)
	}
	assertNoSecret(t, "реплика", excerpts.Replies[0])
	assertNoSecret(t, "решение", excerpts.Decisions[0])

	quotes, err := journals.FindSaid("Артефакт Xq7pTz3mKd9LbWv2 удалить?", time.Now(), 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(quotes) != 1 {
		t.Fatalf("said not found: %+v", quotes)
	}
	assertNoSecret(t, "сверка", quotes[0].Text)
}

// recordingCLI подменяет claude и codex в PATH: скрипт пишет stdin в файл и
// отвечает как настоящий CLI
type recordingCLI struct{}

func (*recordingCLI) capture(t *testing.T, model, input string) (string, string) {
	t.Helper()
	bin := t.TempDir()
	stdin := filepath.Join(bin, "stdin")
	script := func(name, output string) {
		body := "#!/bin/sh\ncat > '" + stdin + "'\nprintf '%s\\n' '" + output + "'\n"
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	script("claude", `{"result":"{\"verdict\":\"pass\"}"}`)
	script("codex", `{"type":"item.completed","item":{"type":"agent_message","text":"{\"verdict\":\"pass\"}"}}`)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	judge := CLIJudge{Name: model, ID: "id-" + model, Effort: "low", Dir: bin}
	answer, _, err := judge.Ask(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	sent, err := os.ReadFile(stdin)
	if err != nil {
		t.Fatal(err)
	}
	return string(sent), answer
}

// Вход, собранный мимо выдержек (цитаты бенча идут в судью как есть), всё
// равно проходит дверь: обоим поставщикам значение не уходит
func TestAskHidesSecretFromEveryProvider(t *testing.T) {
	for _, model := range []string{"opus", "luna"} {
		t.Run(model, func(t *testing.T) {
			input := SaidCase{Quotes: []SaidQuote{{Time: "t", Text: "ключ " + fakeKey()}}, Message: "хвост входа"}.Input()
			sent, answer := (&recordingCLI{}).capture(t, model, input)
			assertNoSecret(t, "stdin "+model, sent)
			if !strings.Contains(sent, "хвост входа") {
				t.Errorf("input lost: %q", sent)
			}
			if !strings.Contains(answer, "pass") {
				t.Errorf("answer lost: %q", answer)
			}
		})
	}
}

// Обычный текст проходит без изменений: маскировка не портит вход судьи
func TestExcerptKeepsOrdinaryText(t *testing.T) {
	text := "sk-learn и ключ-значение, token 123, BEGIN transaction"
	if got := Excerpt(text, 100); got != text {
		t.Errorf("ordinary text changed: %q", got)
	}
	if got := Excerpt("абвгд", 2); got != "аб …[обрезано]" {
		t.Errorf("clip: %q", got)
	}
}

// В журнале question-check нет значения, ни в вопросах, ни в тексте хода после
// остановки; судья его тоже не видит
func TestCheckerJournalHidesSecrets(t *testing.T) {
	f := newFixture(t, "lead", "~")
	message := "Ключ " + fakeKey() + " удалить?"
	result := f.checker.Run(context.Background(), f.event(message))
	if result.Skip != "" {
		t.Fatalf("skipped: %+v", result)
	}
	assertNoSecret(t, "судья", f.judge.input)
	entries := f.journal(t)
	if len(entries) != 1 {
		t.Fatalf("journal: %+v", entries)
	}
	assertNoSecret(t, "журнал", strings.Join(entries[0].Questions, "\n")+entries[0].Message)

	// Текст хода после остановки пишется в журнал через тот же срез
	if got := Excerpt(straddling(afterBlockRunes), afterBlockRunes); strings.Contains(got, fakeBody[:4]) {
		t.Errorf("after block text: %q", got)
	}
}

// Ключ, приклеенный к символу слова: литеральный \n в строке оболочки или
// JSON, «_», буква, %20 перед ключом. Граница слова перед образцом его не видит
func TestGluedKeyIsHidden(t *testing.T) {
	for _, prefix := range []string{`\n`, `\t`, "_", "x", "%20", "KEY="} {
		t.Run(prefix, func(t *testing.T) {
			text := Excerpt("ключ "+prefix+fakeKey()+" хвост", 200)
			assertNoSecret(t, "выдержка", text)
			if !strings.Contains(text, "ключ "+prefix) || !strings.Contains(text, " хвост") {
				t.Errorf("text around the key lost: %q", text)
			}
		})
	}
}

// Вопросы режутся по строкам и теряют цитаты и код: маска после этого не узнала
// бы тело закрытого ключа без BEGIN и ключ, приклеенный к замене цитаты
func TestQuestionsHideSecretsBeforeSplit(t *testing.T) {
	key := "sk-" + "proj-" + fakeBody
	body := "MIIEvQIBADANBgkqhkiG9w0BAQEFAASC"
	armor := "-----" + "BEGIN PRIVATE KEY" + "-----\n" + body + "?\n-----" + "END PRIVATE KEY" + "-----\n"
	for name, message := range map[string]string{
		"кавычки":         `Ключ "а"` + key + " удалить?",
		"вставка кода":    "Ключ `а`" + key + " удалить?",
		"ёлочки":          "Ключ «а»" + key + " удалить?",
		"тело ключа":      armor + "Удалить?",
		"просьба с телом": armor + "ждёт твоего «да»",
	} {
		t.Run(name, func(t *testing.T) {
			questions := Questions(message)
			if len(questions) == 0 {
				t.Fatalf("no questions in %q", message)
			}
			for _, part := range []string{fakeBody[:4], body[:8]} {
				for _, question := range questions {
					if strings.Contains(question.Text, part) || strings.Contains(question.Line, part) {
						t.Errorf("%q passed: %+v", part, question)
					}
				}
				if joined := strings.Join(Texts(questions), "\n"); strings.Contains(joined, part) {
					t.Errorf("%q in journal: %s", part, joined)
				}
			}
		})
	}
}
