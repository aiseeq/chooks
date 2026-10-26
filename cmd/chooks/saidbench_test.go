package main

import (
	"testing"

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
