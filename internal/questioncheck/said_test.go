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

const saidHeader = "time\tsession\tname\tproject\tscope\tkind\twords\tquote\tref\tsupersedes\n"

func writeSaid(t *testing.T, rows ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "said.tsv")
	if err := os.WriteFile(path, []byte(saidHeader+strings.Join(rows, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadSaid(t *testing.T) {
	path := writeSaid(t, "2026-10-20T16:01:16+02:00\t8abab4f1-x\tlead\t~\tglobal\trule\tбинарь chooks\tстрока 1\\nстрока 2\\tтаб \\\\ слэш\t8abab4f1:120\t")
	records, err := ReadSaid(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Quote != "строка 1\nстрока 2\tтаб \\ слэш" || records[0].Kind != "rule" || records[0].Time.Hour() != 16 {
		t.Fatalf("records = %+v", records)
	}
	for _, bad := range []string{"x\ty\n", saidHeader + "2026-10-20T16:01:16+02:00\tмало полей\n", saidHeader + "вчера\ts\tn\tp\tglobal\trule\tw\tq\tr\t\n"} {
		path := filepath.Join(t.TempDir(), "bad.tsv")
		if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadSaid(path); err == nil {
			t.Errorf("должна быть ошибка: %q", bad)
		}
	}
}

func TestInForce(t *testing.T) {
	at := func(day int) time.Time { return time.Date(2026, 10, day, 12, 0, 0, 0, time.UTC) }
	records := []SaidRecord{
		{Time: at(20), Scope: "global", Ref: "a:1", Quote: "общее"},
		{Time: at(20), Scope: "~/work", Ref: "a:2", Quote: "для work"},
		{Time: at(20), Scope: "/home/u/git", Ref: "a:3", Quote: "для git"},
		{Time: at(20), Scope: "~/work/projectB", Ref: "a:4", Quote: "старое projectB"},
		{Time: at(21), Scope: "~/work/projectB", Ref: "a:5", Supersedes: "a:4", Quote: "новое projectB"},
		{Time: at(23), Scope: "global", Ref: "a:6", Quote: "позже хода"},
		{Time: at(20), Scope: "~/work/projectBx", Ref: "a:7", Quote: "соседний проект"},
	}
	var got []string
	for _, record := range InForce(records, "-home-u-work-projectB", "/home/u", at(22)) {
		got = append(got, record.Quote)
	}
	if strings.Join(got, ",") != "общее,для work,новое projectB" {
		t.Fatalf("in force = %v", got)
	}
	// До отмены действует прежняя запись
	got = got[:0]
	for _, record := range InForce(records, "-home-u-work-projectB", "/home/u", at(21).Add(-time.Hour)) {
		got = append(got, record.Quote)
	}
	if strings.Join(got, ",") != "общее,для work,старое projectB" {
		t.Fatalf("in force before supersede = %v", got)
	}
}

func TestRankSaidAndTurnInput(t *testing.T) {
	records := []SaidRecord{
		{Time: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC), Kind: "rule", Words: "бинарь chooks", Quote: "переименовать бинарь полностью в chooks", Ref: "a:1"},
		{Kind: "promise", Words: "отчёт", Quote: "пришлю отчёт о прогоне", Ref: "a:2"},
	}
	ranked := RankSaid(records, "Переименовал бинарь в chooks", 5)
	if len(ranked) != 1 || ranked[0].Ref != "a:1" {
		t.Fatalf("ranked = %+v", ranked)
	}
	input := SaidCase{Quotes: []SaidQuote{ranked[0].AsQuote(), {Time: "t", Kind: "other", Text: "x"}}, Message: "итог", Turn: "→ Bash: mv a b\nитог"}.Input()
	for _, want := range []string{"— 2026-10-20T12:00:00Z, правило: переименовать", "— t, other: x", "[Ход агента]\n→ Bash: mv a b\nитог"} {
		if !strings.Contains(input, want) {
			t.Errorf("нет %q в\n%s", want, input)
		}
	}
	if strings.Contains(input, "[Сообщение агента]") {
		t.Error("с ходом последнее сообщение отдельно не нужно")
	}
}
