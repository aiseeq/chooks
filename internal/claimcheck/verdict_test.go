package claimcheck

import "testing"

func TestParseVerdict(t *testing.T) {
	const valid = `{"claims":[{"text":"тест прошёл","verdict":"checked","basis":"Bash 10:00Z","could_check":""},{"text":"в 3.4.2 исправлено","verdict":"unchecked","basis":"нет","could_check":"список изменений"}]}`
	v, err := ParseVerdict(valid)
	if err != nil || len(v.Claims) != 2 || v.Claims[1].CouldCheck != "список изменений" || v.Claims[0].Basis != "Bash 10:00Z" {
		t.Fatalf("valid: %+v, %v", v, err)
	}
	if !v.Flagged() {
		t.Fatal("unchecked даёт flag")
	}

	// Лишний текст и markdown вокруг, фигурная скобка в тексте до JSON
	wrapped := "Разбор {черновик}:\n```json\n" + `{"claims":[{"text":"a","verdict":"wrong-time","basis":"","could_check":""}]}` + "\n```\nИтог {flag}."
	v, err = ParseVerdict(wrapped)
	if err != nil || len(v.Claims) != 1 || v.Claims[0].Verdict != VerdictWrongTime || !v.Flagged() {
		t.Fatalf("wrapped: %+v, %v", v, err)
	}

	// Два объекта с claims — берётся первый
	v, err = ParseVerdict(`{"claims":[{"text":"a","verdict":"unchecked"}]} или {"claims":[]}`)
	if err != nil || len(v.Claims) != 1 || !v.Flagged() {
		t.Fatalf("первый объект: %+v, %v", v, err)
	}

	v, err = ParseVerdict(`{"claims":[]}`)
	if err != nil || len(v.Claims) != 0 || v.Flagged() {
		t.Fatalf("пустой список — верный ответ без flag: %+v, %v", v, err)
	}

	v, err = ParseVerdict(`{"claims":[{"text":"a","verdict":"checked"},{"text":"b","verdict":"unchecked-marked"}]}`)
	if err != nil || v.Flagged() {
		t.Fatalf("checked и unchecked-marked не дают flag: %+v, %v", v, err)
	}

	for name, bad := range map[string]string{
		"обрыв":            `{"claims":[{"text":"a","verdict":"unchecked"},`,
		"без JSON":         "утверждений нет",
		"без claims":       `{"verdict":"unchecked"}`,
		"чужой вердикт":    `{"claims":[{"text":"a","verdict":"refuted"}]}`,
		"claims не список": `{"claims":"unchecked"}`,
	} {
		if v, err := ParseVerdict(bad); err == nil {
			t.Errorf("%s: ждали сбой разбора, got %+v", name, v)
		}
	}
}

func TestHits(t *testing.T) {
	const target = "Исправлено ли это в `3.4.2`, я не проверял."
	v := Verdict{Claims: []Claim{
		{Text: "Исправлено ли это в `3.4.2`", Verdict: VerdictUncheckedMarked},
		{Text: "Батарею сажал Immich", Verdict: VerdictUnchecked},
	}}
	if !v.Flagged() || v.Hits(target) {
		t.Fatal("flag на другое утверждение — не попадание; помеченное совпадение не флаг")
	}
	v.Claims[0].Verdict = VerdictUnchecked
	if !v.Hits(target) {
		t.Fatal("flag на совпадающее утверждение — попадание")
	}
	v.Claims[0].Verdict = VerdictWrongTime
	if !v.Hits(target) {
		t.Fatal("wrong-time на совпадающее — попадание")
	}
}

func TestSameClaim(t *testing.T) {
	const target = "В четырёх папках лежали единственные копии исходников страниц. В 03:05 я скопировал их HTML в постоянную папку."
	cases := []struct {
		name, claim string
		want        bool
	}{
		{"подстрока, пробелы и регистр", "в 03:05 я  скопировал\nих html", true},
		{"тезис внутри утверждения", "Ответ: " + target + " Дальше ничего.", true},
		// 5 слов, общих 3 (единственные, копии, исходников): ровно 60%
		{"60% слов", "единственные копии исходников лежат отдельно", true},
		// 5 слов, общих 2: 40%
		{"мимо", "копии исходников удалены ночью навсегда", false},
		{"короткое не по словам", "папках лежали", true},
		{"регистр и перевод строки, не по словам", "В четырёх\nПАПКАХ", true},
		{"два слова мимо подстроки", "лежали папках", false},
		{"пустое", "  ", false},
	}
	for _, c := range cases {
		if got := SameClaim(c.claim, target); got != c.want {
			t.Errorf("%s: SameClaim(%q) = %v, want %v", c.name, c.claim, got, c.want)
		}
	}
	if !SameClaim("Ответ: тест прошёл, дальше", "тест прошёл") {
		t.Error("короткий тезис внутри утверждения — подстрока")
	}
	// Тезис короче утверждения: доля — от слов тезиса (4 из 4), а не от 9 слов утверждения
	if !SameClaim("На 06:37 очередь `tk next` была пуста, проверил", "очередь tk next пуста") {
		t.Error("доля считается от меньшего набора")
	}
	if SameClaim("что угодно", "") {
		t.Error("пустой тезис ни с чем не совпадает")
	}
	// Ниже порога в 3 слова: два общих слова из двух — не совпадение
	if SameClaim("копии исходников", "исходников копии нет") {
		t.Error("меньше трёх слов в меньшем наборе — не по словам")
	}
}
