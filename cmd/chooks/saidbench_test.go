package main

import (
	"fmt"
	"testing"
	"time"

	"github.com/aiseeq/chooks/internal/questioncheck"
)

func TestCoversAny(t *testing.T) {
	searched := []questioncheck.SaidQuote{{Text: "Это надо провернуть\nвообще для всех проектов. И периодически"}}
	if coversAny(searched, []questioncheck.SaidQuote{{Text: "это надо провернуть вообще для всех проектов"}}) {
		t.Fatal("регистр отличается — не совпадение")
	}
	if !coversAny(searched, []questioncheck.SaidQuote{{Text: "Это надо  провернуть вообще"}}) {
		t.Fatal("пробелы и перевод строки схлопываются")
	}
	if coversAny(searched, []questioncheck.SaidQuote{{Text: ""}, {Text: "другое"}}) {
		t.Fatal("пустая и чужая цитата не совпадают")
	}
}

func TestSaidBenchInForce(t *testing.T) {
	at := func(hour int) time.Time { return time.Date(2026, 10, 20, hour, 0, 0, 0, time.UTC) }
	bench := saidBench{home: "/home/u", saidLimit: 1, turn: true}
	// Своя сессия — по времени, хотя в файле не по порядку; чужая — поиском
	for hour := 12; hour >= 0; hour-- {
		bench.said = append(bench.said, questioncheck.SaidRecord{Time: at(hour), Scope: "global", Ref: fmt.Sprintf("8abab4f1:%d", hour), Quote: fmt.Sprintf("своё %d", hour)})
	}
	bench.said = append(bench.said,
		questioncheck.SaidRecord{Time: at(1), Scope: "global", Ref: "ffffffff:1", Words: "селинукс", Quote: "разберись с селинуксом"},
		questioncheck.SaidRecord{Time: at(1), Scope: "global", Ref: "ffffffff:2", Words: "отчёт", Quote: "чужое не по теме"},
	)
	// Запись из самой следующей реплики: время до секунды, ts примера — с долями
	bench.said = append(bench.said, questioncheck.SaidRecord{Time: at(13), Scope: "global", Ref: "8abab4f1:100", Quote: "поправка"})
	example := saidCase{ID: "-home-u/8abab4f1-9009.jsonl:100", Time: at(13).Add(101 * time.Millisecond), Project: "-home-u", Turn: "→ Bash: ausearch селинукс"}
	quotes := bench.inForce(example)
	if len(quotes) != sameSessionSaid+1 || quotes[0].Text != "своё 3" || quotes[sameSessionSaid-1].Text != "своё 12" || quotes[sameSessionSaid].Text != "разберись с селинуксом" {
		t.Fatalf("quotes = %+v", quotes)
	}
}
