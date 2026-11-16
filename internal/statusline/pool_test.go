package statusline

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/chooks/internal/accounts"
)

// Темп — сумма шагов за окно на часы истории. Неделя, начатая до журнала,
// даёт только базу; начатая после — считается от нуля
func TestPoolRate(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	old := now.Add(4 * 24 * time.Hour)             // неделя началась 3 дня назад, до журнала
	fresh := now.Add(7*24*time.Hour - 2*time.Hour) // началась 2 ч назад
	steps := []limitStep{
		{At: now.Add(-20 * time.Hour), ResetsAt: old, SevenDay: 60},
		{At: now.Add(-10 * time.Hour), ResetsAt: old, SevenDay: 70},
		{At: now.Add(-time.Hour), ResetsAt: old, SevenDay: 80},
		{At: now.Add(-30 * time.Minute), ResetsAt: fresh, SevenDay: 4},
	}
	rate, span, err := poolRate(steps, now)
	if err != nil {
		t.Fatal(err)
	}
	if span != 20*time.Hour || rate != 24.0/20 {
		t.Errorf("темп %v%%/ч за %v, ожидалось 1.2%%/ч за 20 ч", rate, span)
	}
	if _, _, err := poolRate(steps[2:], now); !errors.Is(err, errShortHistory) {
		t.Errorf("короткая история: %v", err)
	}
}

// Хвост большого журнала читается с первой целой строки
func TestReadRecentStepsTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "limits-history.jsonl")
	line := `{"at":"2026-10-22T10:00:00Z","sevenDayResetsAt":"2026-10-26T10:00:00Z","sevenDayUsedPercentage":5}` + "\n"
	if err := os.WriteFile(path, []byte(strings.Repeat(line, historyTail/len(line)+50)), 0o644); err != nil {
		t.Fatal(err)
	}
	steps, err := readRecentSteps(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(steps); n == 0 || n > historyTail/len(line) {
		t.Errorf("шагов в хвосте: %d", n)
	}
}

// Частичный сброс недели — шаг вниз: расход считается от него, а не от
// прежнего максимума
func TestPoolRateAfterPartialReset(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	resets := now.Add(4 * 24 * time.Hour)
	steps := []limitStep{
		{At: now.Add(-10 * time.Hour), ResetsAt: resets, SevenDay: 100},
		{At: now.Add(-5 * time.Hour), ResetsAt: resets, SevenDay: 50},
		{At: now.Add(-time.Hour), ResetsAt: resets, SevenDay: 60},
	}
	rate, span, err := poolRate(steps, now)
	if err != nil {
		t.Fatal(err)
	}
	if span != 10*time.Hour || rate != 1 {
		t.Errorf("темп %v%%/ч за %v, ожидалось 1%%/ч за 10 ч", rate, span)
	}
}

// Пустота пула словами: расход — когда и до какого сброса; вход — чьи входы
// истекают с остатком недели; сброс без входа не выдаётся за возврат
func TestDryText(t *testing.T) {
	now := time.Date(2026, 11, 15, 22, 47, 0, 0, time.UTC)
	store := &accounts.Store{Accounts: []accounts.Account{{Email: "x@work.example"}, {Email: "y@protonmail.com"}, {Email: "z@gmail.com"}}}
	unit := accounts.Unit{Scale: 2}
	dry, refill, ends, resets := now.Add(90*time.Hour), now.Add(190*time.Hour), now.Add(91*time.Hour), now.Add(130*time.Hour)
	st := accounts.ShortTime
	spend := accounts.Forecast{DryAt: dry, DryCause: accounts.DrySpend, RefillAt: refill, RefillEmail: "x@work.example"}
	if got, want := dryText(store, spend, unit), "пул кончится "+st(dry)+", до сброса work.example 100 ч"; got != want {
		t.Errorf("расход:\n%s\nожидалось\n%s", got, want)
	}
	spend.LostResets = []accounts.LostReset{{Email: "z@gmail.com", ResetsAt: resets, LoginEnds: dry.Add(time.Hour)}}
	want := "пул кончится " + st(dry) + "; сброс gmail.com " + st(resets) + " без нового входа не вернёт Claude (вход до " + st(dry.Add(time.Hour)) + "); до сброса work.example 100 ч"
	if got := dryText(store, spend, unit); got != want {
		t.Errorf("расход, сброс без входа:\n%s\nожидалось\n%s", got, want)
	}
	login := accounts.Forecast{
		DryAt: dry, DryCause: accounts.DryLogin, RefillAt: refill, RefillEmail: "x@work.example",
		ExpiredLogins: []accounts.ExpiredLogin{{Email: "y@protonmail.com", LoginEnds: ends, Used: 81}},
		LostResets:    []accounts.LostReset{{Email: "z@gmail.com", ResetsAt: resets, LoginEnds: dry.Add(time.Hour)}},
	}
	want = "пул кончится " + st(dry) + ": без нового входа не взять protonmail.com (вход до " + st(ends) + ", остаток недели 36%)" +
		"; сброс gmail.com " + st(resets) + " без нового входа не вернёт Claude (вход до " + st(dry.Add(time.Hour)) + ")" +
		"; до сброса work.example 100 ч"
	if got := dryText(store, login, unit); got != want {
		t.Errorf("вход:\n%s\nожидалось\n%s", got, want)
	}
	login.LostResets = nil
	want = "пул кончится " + st(dry) + ": без нового входа не взять protonmail.com (вход до " + st(ends) + ", остаток недели 36%); до сброса work.example 100 ч"
	if got := dryText(store, login, unit); got != want {
		t.Errorf("вход без сбросов без входа:\n%s\nожидалось\n%s", got, want)
	}
	login.LostResets = []accounts.LostReset{{Email: "z@gmail.com", ResetsAt: resets, LoginEnds: dry.Add(time.Hour)}}
	login.ExpiredLogins = append(login.ExpiredLogins, accounts.ExpiredLogin{Email: "x@work.example", Used: 0})
	login.RefillAt, login.RefillEmail = time.Time{}, ""
	want = "пул кончится " + st(dry) + ": без нового входа не взять protonmail.com (вход до " + st(ends) + ", остаток недели 36%), work.example (вход негоден, остаток недели 198%)" +
		"; сброс gmail.com " + st(resets) + " без нового входа не вернёт Claude (вход до " + st(dry.Add(time.Hour)) + ")" +
		"; без нового входа сброса нет"
	if got := dryText(store, login, unit); got != want {
		t.Errorf("два входа, возврата нет:\n%s\nожидалось\n%s", got, want)
	}
	none := accounts.Forecast{DryAt: dry, DryCause: accounts.DryLogin, LostResets: []accounts.LostReset{{Email: "z@gmail.com", ResetsAt: resets}}}
	want = "пул кончится " + st(dry) + ": годного входа нет ни у одной учётки; сброс gmail.com " + st(resets) + " без нового входа не вернёт Claude (вход негоден); без нового входа сброса нет"
	if got := dryText(store, none, unit); got != want {
		t.Errorf("годных входов нет:\n%s\nожидалось\n%s", got, want)
	}
}

// Строка «Пул учёток» при пустоте из-за входа называет вход, а не только
// момент и сброс
func TestPoolTextLogin(t *testing.T) {
	now := time.Date(2026, 11, 15, 22, 47, 0, 0, time.UTC)
	store := &accounts.Store{Accounts: []accounts.Account{{Email: "y@protonmail.com"}}}
	f := PoolForecast{Forecast: accounts.Forecast{
		Rate: 1, Pace: accounts.PaceOver, DryAt: now.Add(time.Hour), DryCause: accounts.DryLogin,
		ExpiredLogins: []accounts.ExpiredLogin{{Email: "y@protonmail.com", LoginEnds: now.Add(2 * time.Hour), Used: 50}},
	}, RateSpanHours: 72}
	got := poolText(store, f, accounts.Unit{Scale: 2}, now)
	if want := "Пул учёток: темп 48% в сутки (за 72 ч); пул кончится " + accounts.ShortTime(now.Add(time.Hour)) + ": без нового входа не взять protonmail.com"; !strings.HasPrefix(got, want) {
		t.Errorf("строка пула:\n%s\nожидалось начало\n%s", got, want)
	}
}
