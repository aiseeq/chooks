package questioncheck

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseSaidVerdict(t *testing.T) {
	verdict, err := ParseSaidVerdict("```json\n{\"verdict\":\"block\",\"findings\":[{\"quote\":\"во всех проектах\",\"message\":\"сделал в одном\",\"why\":\"сужает\"}]}\n```")
	if err != nil || verdict.Verdict != VerdictBlock || verdict.Findings[0].Quote != "во всех проектах" {
		t.Fatalf("verdict = %+v, err = %v", verdict, err)
	}
	if verdict, err := ParseSaidVerdict(`{"verdict":"pass"}`); err != nil || verdict.Verdict != VerdictPass {
		t.Fatalf("verdict = %+v, err = %v", verdict, err)
	}
	for _, bad := range []string{`{"verdict":"block","findings":[]}`, `{"verdict":"maybe"}`, `не знаю`, `{"verdict":`} {
		if _, err := ParseSaidVerdict(bad); err == nil {
			t.Errorf("должна быть ошибка: %s", bad)
		}
	}
}

func TestSaidCaseInput(t *testing.T) {
	input := SaidCase{
		Quotes:  []SaidQuote{{Time: "2026-10-20T14:01:16.007Z", Text: "переименовать полностью"}},
		Message: "Переименовал только бинарь.",
	}.Input()
	for _, want := range []string{"(не найдена)", "— 2026-10-20T14:01:16.007Z: переименовать полностью", "[Сообщение агента]\nПереименовал только бинарь."} {
		if !strings.Contains(input, want) {
			t.Errorf("нет %q в\n%s", want, input)
		}
	}
}

func TestFindSaid(t *testing.T) {
	replies := filepath.Join(t.TempDir(), "user-replies.md")
	text := "# Реплики\n\n" +
		"### 2026-10-20T14:01:16.007Z (20.10 16:01 CEST) · ~/git · claude x\nв ответ на: Переименовать бинарь?\nМожно полностью переименовать бинарь в chooks\n\n" +
		"### 2026-10-21T09:00:00.000Z (21.10 11:00 CEST) · ~/git · claude x\nбинарь chooks ставь сразу\n"
	if err := os.WriteFile(replies, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	journals := Journals{RepliesPath: replies}
	quotes, err := journals.FindSaid("Переименовал бинарь в chooks", time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC), 5)
	if err != nil {
		t.Fatal(err)
	}
	// Реплика позже before не видна, строка «в ответ на» отрезана
	if len(quotes) != 1 || quotes[0].Time != "2026-10-20T14:01:16.007Z" || quotes[0].Text != "Можно полностью переименовать бинарь в chooks" {
		t.Fatalf("quotes = %+v", quotes)
	}
}
