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
	judge := CLIJudge{Name: model, ID: "id-" + model, Effort: "low", Dir: bin, Prompt: SaidPrompt}
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

// Ключ формы sk- с длинным отрезком букв и цифр скрыт при этих соседях:
// экранирования и управляющие последовательности из грамматики границ secretmask,
// голая буква или цифра. Короткие отрезки после голой буквы или цифры, бесцифровые
// общие sk, экранирования вне грамматики - объявленная цена, doc comment пакета
func TestGluedDashKeyIsHidden(t *testing.T) {
	neighbours := []string{
		`\n`, `\t`, `\f`, `\v`, `\x20`, ` `, "_", "x", "0", "KEY=", "Bearer%20",
		"%20", "%3D", "%2520", `=`, "=3D", "&#61;", "&#x3D;",
		"\x1b[32m", "\x1b[2K", `\e[32m`, `\E[1m`, `\033[0m`, `\33[0m`, `\x1B[32m`, `\u001b[32m`,
		`\u{1b}[32m`, `\U0000003d`, `^[[32m`, "␛[32m", "\x1b(B", "[32m", "[0;32m",
	}
	for _, kind := range []string{"sk-" + "proj-", "sk-" + "ant-", "sk-"} {
		key := kind + fakeBody
		for _, prefix := range neighbours {
			t.Run(kind+prefix, func(t *testing.T) {
				text := Excerpt("ключ "+prefix+key+" хвост", 200)
				if strings.Contains(text, fakeBody[:4]) {
					t.Errorf("value passed: %q", text)
				}
				if !strings.Contains(text, "ключ "+prefix) || !strings.Contains(text, " хвост") {
					t.Errorf("text around the key lost: %q", text)
				}
			})
		}
	}
}

// Настоящий ключ стоит вплотную за словом с тем же началом («task-» + ключ):
// совпадение от начала слова отклоняется и не должно съесть ключ вместе с собой
func TestKeyAfterRejectedMatchIsHidden(t *testing.T) {
	body := "aB3-xY9_kL2-mN4_pQ7-rS1_tU5"
	for _, word := range []string{
		"ta" + "sk-", "ta" + "sk_", "ri" + "sk-", "de" + "sk-note_", "ta" + "sk-letters-",
		"ta" + "sk-" + strings.Repeat("letters", 4) + "-",
	} {
		for _, kind := range []string{"sk-", "sk_"} {
			t.Run(word+kind, func(t *testing.T) {
				text := Excerpt("начало "+word+kind+body+" хвост", 300)
				if strings.Contains(text, body[:4]) {
					t.Errorf("value passed: %q", text)
				}
				if !strings.Contains(text, "начало ") || !strings.Contains(text, " хвост") {
					t.Errorf("text around the key lost: %q", text)
				}
			})
		}
	}
}

// Ключ настоящей формы с телом без цифры скрыт, когда стоит сам по себе. Рядом с
// буквой, цифрой, «-» или «_» такой не скрывается: объявленная цена, doc comment пакета
func TestDigitFreeKeyIsHidden(t *testing.T) {
	letters := strings.Repeat("QzXwRtLmVbNcPkJhGfDsAy", 2)
	for _, form := range []struct {
		prefix string
		length int
	}{
		{"sk_" + "live_", 24}, {"sk_" + "test_", 24}, {"rk_" + "live_", 24}, {"rk_" + "test_", 24},
		{"pk_" + "live_", 24}, {"pk_" + "test_", 24},
		{"gh" + "p_", 36}, {"gh" + "o_", 36}, {"gh" + "u_", 36}, {"gh" + "s_", 36}, {"gh" + "r_", 36},
		{"AI" + "za", 35},
	} {
		body := letters[:form.length]
		for _, prefix := range []string{
			"", " ", "\n", `\n`, `"`, "=", "KEY=", "Bearer ", "%20", "\x1b[32m",
		} {
			t.Run(form.prefix+prefix, func(t *testing.T) {
				text := Excerpt("ключ\n"+prefix+form.prefix+body+" хвост", 200)
				if strings.Contains(text, body[:8]) {
					t.Errorf("value passed: %q", text)
				}
				if !strings.Contains(text, "ключ\n"+prefix) || !strings.Contains(text, " хвост") {
					t.Errorf("text around the key lost: %q", text)
				}
			})
		}
	}
}

// Имена, похожие на префикс ключа, но без случайного тела, остаются текстом:
// судья и журнал видят их такими, как написал автор
func TestNamesLikeKeyPrefixAreKept(t *testing.T) {
	for _, name := range []string{
		"network_test" + "_ConnectionTimeoutCase",
		"framework_test" + "_HandlesEmptyInput",
		"xoxo" + "-hugs-and-kisses",
		"xoxb" + "-hugs-and-kisses",
		"task-" + "proj-overview-page",
		"ask-" + "ant-colony-question",
		"task_" + "ThisIsLongDescriptiveName",
	} {
		t.Run(name, func(t *testing.T) {
			text := "имя " + name + " хвост"
			if got := Excerpt(text, 200); got != text {
				t.Errorf("name changed: %q", got)
			}
		})
	}
}
