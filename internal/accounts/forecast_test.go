package accounts

import (
	"slices"
	"testing"
	"time"
)

// Три учётки: активная почти пуста, личная с поздним сбросом, третья не
// начата. При 3% в час пул кончается: 3.5 ч активной, по 33 ч личной и
// третьей; первым вернётся сброс активной
func TestForecastPoolRunsDry(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	s := &Store{Active: "a", Accounts: []Account{account("a", 20*day, now), account("b", 20*day, now), account("c", 20*day, now)}}
	usage := map[string]Usage{
		"a@example.com": {Week: Window{Used: 96, ResetsAt: now.Add(4 * day)}},
		"b@example.com": {Week: Window{Used: 0, ResetsAt: now.Add(6 * day)}},
	}
	f := s.Forecast(usage, 3, now)
	if f.Pace != PaceOver {
		t.Fatalf("прогноз %v, ожидался over", f.Pace)
	}
	// 3/3 + 99/3 + 99/3 = 67 ч
	if want := now.Add(67 * time.Hour); f.DryAt.Sub(want).Abs() > 20*time.Minute {
		t.Errorf("пул пуст %v, ожидалось около %v", f.DryAt, want)
	}
	if f.RefillEmail != "a@example.com" || !f.RefillAt.Equal(now.Add(4*day)) {
		t.Errorf("первый сброс: %s %v", f.RefillEmail, f.RefillAt)
	}
	if f.DryCause != DrySpend || len(f.ExpiredLogins) > 0 || len(f.LostResets) > 0 {
		t.Errorf("причина %q, входы %v, сбросы без входа %v, ожидался расход", f.DryCause, f.ExpiredLogins, f.LostResets)
	}
	var order []string
	for _, sp := range f.Plan {
		order = append(order, sp.Email)
	}
	if want := []string{"a@example.com", "b@example.com", "c@example.com", "", "a@example.com"}; len(order) < len(want) || !slices.Equal(order[:len(want)], want) {
		t.Errorf("план: %v, ожидалось начало %v", order, want)
	}
}

// Одна неначатая учётка: 0.5% в час — 84% за неделю, хватает; с запасом в
// четверть — 105%, не хватает: впритык
func TestForecastPoolNear(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	s := &Store{Active: "a", Accounts: []Account{account("a", 20*day, now)}}
	if f := s.Forecast(nil, 0.5, now); f.Pace != PaceNear || !f.DryAt.IsZero() {
		t.Errorf("0.5%%/ч: %v, пуст %v", f.Pace, f.DryAt)
	}
	if f := s.Forecast(nil, 0.3, now); f.Pace != PaceSafe {
		t.Errorf("0.3%%/ч: %v", f.Pace)
	}
}

// Разные тарифы помечаются: доли недели Max 5x и 20x несопоставимы
func TestForecastMixedTiers(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	s := &Store{Active: "a", Accounts: []Account{tiered(t, "a", "default_claude_max_20x", now), tiered(t, "b", "default_claude_max_20x", now)}}
	if s.Forecast(nil, 1, now).MixedTiers {
		t.Error("одинаковые тарифы помечены разными")
	}
	s.Accounts = append(s.Accounts, tiered(t, "c", "default_claude_max_5x", now))
	if !s.Forecast(nil, 1, now).MixedTiers {
		t.Error("разные тарифы не помечены")
	}
}

// Путь доли учётки: с нынешней доли, до пустой, сброс в ноль, новая неделя
func TestForecastTracks(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	s := &Store{Active: "a", Accounts: []Account{account("a", 20*day, now), account("b", 20*day, now)}}
	resets := now.Add(2 * day)
	f := s.Forecast(map[string]Usage{"a@example.com": {Week: Window{Used: 90, ResetsAt: resets}}}, 1, now)
	if len(f.Tracks) != 2 || f.Tracks[0].Email != "a@example.com" {
		t.Fatalf("пути: %+v", f.Tracks)
	}
	a := f.Tracks[0]
	if p := a.Points[0]; !p.At.Equal(now) || p.Used != 90 {
		t.Errorf("начало пути: %+v", p)
	}
	full, zero := false, false
	for _, p := range a.Points {
		full = full || p.Used >= WeekEmptyPercent
		zero = zero || p.At.Equal(resets) && p.Used == 0
	}
	if !full || !zero {
		t.Errorf("путь без опустения или сброса: %+v", a.Points)
	}
	if len(a.Resets) < 2 || !a.Resets[0].Equal(resets) {
		t.Errorf("сбросы a: %v", a.Resets)
	}
	if b := f.Tracks[1]; len(b.Resets) == 0 || b.Points[0].Used != 0 {
		t.Errorf("b начинает неделю, когда a пуста: %+v", b)
	}
}

// Запас есть, вход кончается раньше: a пуста за час, b берётся с нуля и
// через 9 ч (вход 10 ч минус час запаса) брать её нельзя, при доле около 24.
// Причина — вход b и вход c, негодный уже в начале прогона (запас у c
// полный); возврат — сброс a, вход которой годится, сброс c раньше него — без
// входа. d пуста — её истёкший вход пустоты не делает
func TestForecastPoolLoginExpires(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	s := &Store{Active: "a", Accounts: []Account{account("c", 30*time.Minute, now), account("d", 5*time.Hour, now), account("a", 20*day, now), account("b", 10*time.Hour, now)}}
	usage := map[string]Usage{
		"a@example.com": {Week: Window{Used: 96, ResetsAt: now.Add(4 * day)}},
		"b@example.com": {Week: Window{Used: 0, ResetsAt: now.Add(6 * day)}},
		"c@example.com": {Week: Window{Used: 0, ResetsAt: now.Add(day)}},
		"d@example.com": {Week: Window{Used: WeekEmptyPercent, ResetsAt: now.Add(5 * day)}},
	}
	f := s.Forecast(usage, 3, now)
	if f.Pace != PaceOver || f.DryCause != DryLogin {
		t.Fatalf("прогноз %v, причина %q, ожидались over и вход", f.Pace, f.DryCause)
	}
	if want := now.Add(9 * time.Hour); f.DryAt.Sub(want).Abs() > 20*time.Minute {
		t.Errorf("пул пуст %v, ожидалось около %v", f.DryAt, want)
	}
	if len(f.ExpiredLogins) != 2 {
		t.Fatalf("истёкшие входы: %+v", f.ExpiredLogins)
	}
	if c := f.ExpiredLogins[0]; c.Email != "c@example.com" || !c.LoginEnds.Equal(now.Add(30*time.Minute)) || c.Used != 0 {
		t.Errorf("негодный с начала вход: %+v, ожидался c, вход до %v, доля 0", c, now.Add(30*time.Minute))
	}
	e := f.ExpiredLogins[1]
	if e.Email != "b@example.com" || !e.LoginEnds.Equal(now.Add(10*time.Hour)) || e.Used < 20 || e.Used > 28 {
		t.Errorf("истёкший вход: %+v, ожидался b, вход до %v, доля около 24", e, now.Add(10*time.Hour))
	}
	if f.RefillEmail != "a@example.com" || !f.RefillAt.Equal(now.Add(4*day)) {
		t.Errorf("возврат %s %v, ожидался сброс a", f.RefillEmail, f.RefillAt)
	}
	if len(f.LostResets) != 1 || f.LostResets[0].Email != "c@example.com" || !f.LostResets[0].ResetsAt.Equal(now.Add(day)) {
		t.Errorf("сбросы без входа %+v, ожидался сброс c", f.LostResets)
	}
}

// Сброс после истечения входа — не возврат: у a вход кончается за сутки до
// её сброса, у b вход живой, сброс позже. Пул пуст расходом за 20 минут (b
// почти пуста, a пуста), возврат — b, сброс a — без нового входа. Сброс g
// без входа — в один момент с возвратом b: его не называют
func TestForecastPoolResetAfterLogin(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	s := &Store{Active: "b", Accounts: []Account{account("a", day, now), account("g", day, now), account("b", 20*day, now)}}
	usage := map[string]Usage{
		"a@example.com": {Week: Window{Used: 99, ResetsAt: now.Add(2 * day)}},
		"b@example.com": {Week: Window{Used: 98, ResetsAt: now.Add(3 * day)}},
		"g@example.com": {Week: Window{Used: 99, ResetsAt: now.Add(3 * day)}},
	}
	f := s.Forecast(usage, 3, now)
	if f.DryCause != DrySpend || len(f.ExpiredLogins) > 0 {
		t.Errorf("причина %q, входы %+v, ожидался расход", f.DryCause, f.ExpiredLogins)
	}
	if f.RefillEmail != "b@example.com" || !f.RefillAt.Equal(now.Add(3*day)) {
		t.Errorf("возврат %s %v, ожидался сброс b", f.RefillEmail, f.RefillAt)
	}
	want := []LostReset{{Email: "a@example.com", ResetsAt: now.Add(2 * day), LoginEnds: now.Add(day)}}
	if !slices.EqualFunc(f.LostResets, want, func(x, y LostReset) bool {
		return x.Email == y.Email && x.ResetsAt.Equal(y.ResetsAt) && x.LoginEnds.Equal(y.LoginEnds)
	}) {
		t.Errorf("сбросы без входа %+v, ожидалось %+v", f.LostResets, want)
	}
	// Без b сброса с живым входом нет: возврата нет, сбросы a и e — без
	// входа, по времени сброса
	s = &Store{Active: "a", Accounts: []Account{account("e", day, now), account("a", day, now)}}
	usage["e@example.com"] = Usage{Week: Window{Used: 99, ResetsAt: now.Add(60 * time.Hour)}}
	f = s.Forecast(usage, 3, now)
	var lost []string
	for _, r := range f.LostResets {
		lost = append(lost, r.Email)
	}
	if !f.RefillAt.IsZero() || f.RefillEmail != "" || !slices.Equal(lost, []string{"a@example.com", "e@example.com"}) {
		t.Errorf("a и e: возврат %s %v, сбросы без входа %+v", f.RefillEmail, f.RefillAt, f.LostResets)
	}
}

// Причина пустоты и возврат на краях: вход с большим остатком истекает до
// пустоты (как 15.11: доля 91.6), крошечный остаток не причина, годного
// входа нет ни у кого, возврат — ближайший из годных сбросов, сброс не на
// сетке шага — по правилу плана
func TestForecastPoolLoginEdges(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	week := func(used float64, in time.Duration) Usage {
		return Usage{Week: Window{Used: used, ResetsAt: now.Add(in)}}
	}
	// planned — учётка тратит в плане с момента from
	planned := func(f Forecast, email string, from time.Time) bool {
		return slices.ContainsFunc(f.Plan, func(sp Spell) bool { return sp.Email == email && !sp.From.Before(from) })
	}
	t.Run("большой остаток", func(t *testing.T) {
		// a тратит 1%/ч 4 ч до 94 и теряет вход; b и c пусты, c сбросится раньше b
		s := &Store{Active: "a", Accounts: []Account{account("a", 5*time.Hour, now), account("b", 20*day, now), account("c", 20*day, now)}}
		usage := map[string]Usage{"a@example.com": week(90, 2*day), "b@example.com": week(99, 3*day), "c@example.com": week(99, 30*time.Hour)}
		f := s.Forecast(usage, 1, now)
		if f.DryCause != DryLogin || len(f.ExpiredLogins) != 1 || f.ExpiredLogins[0].Email != "a@example.com" || f.ExpiredLogins[0].Used < 93 {
			t.Errorf("причина %q, входы %+v, ожидался вход a с долей около 94", f.DryCause, f.ExpiredLogins)
		}
		if f.RefillEmail != "c@example.com" || !f.RefillAt.Equal(now.Add(30*time.Hour)) || len(f.LostResets) > 0 {
			t.Errorf("возврат %s %v, сбросы без входа %+v, ожидался ближайший — c", f.RefillEmail, f.RefillAt, f.LostResets)
		}
	})
	t.Run("крошечный остаток", func(t *testing.T) {
		// a теряет вход через 2 ч, b тратит до пустоты 6 ч. Остаток a не
		// больше расхода за шаг (3%/ч — 0.5 за 10 минут) — не причина,
		// больше — причина
		for _, tc := range []struct {
			used  float64
			cause DryCause
		}{{98.9, DrySpend}, {98.5, DrySpend}, {98, DryLogin}} {
			s := &Store{Active: "b", Accounts: []Account{account("a", 3*time.Hour, now), account("b", 20*day, now)}}
			f := s.Forecast(map[string]Usage{"a@example.com": week(tc.used, 3*day), "b@example.com": week(80, 2*day)}, 3, now)
			if f.DryCause != tc.cause || len(f.ExpiredLogins) != map[DryCause]int{DrySpend: 0, DryLogin: 1}[tc.cause] {
				t.Errorf("доля a %v: причина %q, входы %+v, ожидалась причина %q", tc.used, f.DryCause, f.ExpiredLogins, tc.cause)
			}
		}
	})
	t.Run("годных входов нет", func(t *testing.T) {
		dead := account("b", 20*day, now)
		dead.Dead = "нужен /login"
		s := &Store{Active: "a", Accounts: []Account{account("a", 30*time.Minute, now), dead}}
		f := s.Forecast(map[string]Usage{"a@example.com": week(10, 2*day)}, 3, now)
		if f.DryCause != DryLogin || !f.DryAt.Equal(now) || len(f.ExpiredLogins) != 2 || !f.ExpiredLogins[1].LoginEnds.IsZero() {
			t.Errorf("пуст %v, причина %q, входы %+v, ожидались сейчас, вход, a и b без срока", f.DryAt, f.DryCause, f.ExpiredLogins)
		}
		if len(f.LostResets) != 1 || f.LostResets[0].Email != "a@example.com" || !f.RefillAt.IsZero() {
			t.Errorf("возврат %v, сбросы без входа %+v, ожидался только сброс a без входа", f.RefillAt, f.LostResets)
		}
		if f = (&Store{}).Forecast(nil, 3, now); f.DryCause != DryLogin || len(f.ExpiredLogins) > 0 {
			t.Errorf("пустое хранилище: причина %q, входы %+v", f.DryCause, f.ExpiredLogins)
		}
	})
	t.Run("сброс не на сетке", func(t *testing.T) {
		// Вход кончается через час и минуту после сброса: на сетке сброс —
		// возврат, через 5 минут после шага — учётку на следующем шаге уже
		// не взять, и это не возврат
		for _, off := range []time.Duration{0, 5 * time.Minute} {
			reset := 2*day + off
			s := &Store{Active: "a", Accounts: []Account{account("a", reset+time.Hour+time.Minute, now)}}
			f := s.Forecast(map[string]Usage{"a@example.com": week(96, reset)}, 3, now)
			if back := !f.RefillAt.IsZero(); back != planned(f, "a@example.com", now.Add(reset)) || back != (off == 0) || back == (len(f.LostResets) == 1) {
				t.Errorf("сброс +%v: возврат %v, сбросы без входа %+v, план %+v", off, f.RefillAt, f.LostResets, f.Plan)
			}
		}
	})
}
