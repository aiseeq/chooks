package accounts

import (
	"slices"
	"time"
)

// Прогноз пула (пользователь 22.10: «с 3-мя подписками можно пересчитать
// темп?»): нынешний темп расхода прогоняется вперёд по правилу ротации —
// тратится учётка, которую выбрал бы pick, пустая сменяется следующей, у
// каждой учётки в её сброс доля обнуляется, неначатая неделя начинается с
// первым расходом
const (
	// PoolHorizon — горизонт прогноза: за неделю каждая учётка сбросится
	// хотя бы раз, дальше прогноз повторяет себя
	PoolHorizon = weekWindow
	// poolStep — шаг прогона: 10 минут — 1008 шагов на неделю, ошибка в
	// моменте опустения — не больше шага
	poolStep = 10 * time.Minute
	// poolMargin — запас: пул, который кончился бы при темпе на четверть
	// выше, — впритык. То же, что прежний порог одной учётки: прогноз на
	// сброс больше 80% (1/0.8 = 1.25)
	poolMargin = 1.25
)

// Pace — прогноз пула
type Pace int

const (
	// PaceSafe — пул не кончится за горизонт и при темпе с запасом
	PaceSafe Pace = iota
	// PaceNear — при нынешнем темпе хватит, при темпе с запасом — нет
	PaceNear
	// PaceOver — будет момент, когда все учётки пусты
	PaceOver
)

// String — имя для JSON и журнала
func (p Pace) String() string {
	switch p {
	case PaceNear:
		return "near"
	case PaceOver:
		return "over"
	default:
		return "safe"
	}
}

// Spell — отрезок прогноза: кто тратит. Email пуст — все учётки пусты
type Spell struct {
	Email string    `json:"email"`
	From  time.Time `json:"from"`
	To    time.Time `json:"to"`
}

// Point — доля недели учётки в момент
type Point struct {
	At   time.Time `json:"at"`
	Used float64   `json:"used"`
}

// Track — путь доли учётки в прогнозе: точки изломов (начало и конец
// расхода, сброс), между ними доля меняется линейно; Resets — сбросы недели
// на горизонте, включая недели, которые прогноз начал
type Track struct {
	Email  string      `json:"email"`
	Points []Point     `json:"points"`
	Resets []time.Time `json:"resets"`
}

// DryCause — почему пул пуст в DryAt
type DryCause string

const (
	// DrySpend — недели всех учёток с годным входом потрачены
	DrySpend DryCause = "spend"
	// DryLogin — у учётки запас недели есть, но её вход истёк (ExpiredLogins)
	DryLogin DryCause = "login"
)

// ExpiredLogin — учётка, которую прогон перестал брать с запасом недели:
// истёк вход
type ExpiredLogin struct {
	Email string `json:"email"`
	// LoginEnds — конец цепочки токенов; прогон не берёт учётку за
	// loginMargin до него
	LoginEnds time.Time `json:"loginEnds"`
	// Used — доля недели учётки в DryAt: остаток сгорит без нового входа
	Used float64 `json:"used"`
}

// LostReset — сброс недели, который без нового входа Claude не вернёт:
// вход учётки истекает раньше
type LostReset struct {
	Email     string    `json:"email"`
	ResetsAt  time.Time `json:"resetsAt"`
	LoginEnds time.Time `json:"loginEnds"`
}

// Forecast — прогноз пула при темпе Rate (% недели одной учётки в час)
type Forecast struct {
	Rate float64 `json:"ratePerHour"`
	Pace Pace    `json:"-"`
	// DryAt — первый момент, когда ни одну учётку взять нельзя: недели
	// пусты или входы истекли (DryCause); ноль — за горизонт нет
	DryAt time.Time `json:"dryAt,omitzero"`
	// DryCause — причина пустоты в DryAt; пусто, когда DryAt нет
	DryCause DryCause `json:"dryCause,omitempty"`
	// ExpiredLogins — учётки с запасом недели, чей вход истёк к DryAt (при
	// DryLogin), в порядке хранилища
	ExpiredLogins []ExpiredLogin `json:"expiredLogins,omitempty"`
	// RefillAt и RefillEmail — ближайший после DryAt сброс недели, который
	// вернёт Claude: вход учётки к сбросу годится; ноль — такого сброса нет
	RefillAt    time.Time `json:"refillAt,omitzero"`
	RefillEmail string    `json:"refillEmail,omitempty"`
	// LostResets — сбросы после DryAt и раньше RefillAt (нет RefillAt — все),
	// которые без нового входа Claude не вернут, по времени сброса
	LostResets []LostReset `json:"lostResets,omitempty"`
	Plan       []Spell     `json:"plan"`
	Tracks     []Track     `json:"tracks"`
	// MixedTiers — тарифы учёток разные (Max 5x и 20x): доли недели
	// несопоставимы, а прогноз считает их равными (22.10 у всех трёх 20x)
	MixedTiers bool `json:"mixedTiers"`
}

// Forecast прогоняет темп rate вперёд по правилу ротации от расхода usage
func (s *Store) Forecast(usage map[string]Usage, rate float64, now time.Time) Forecast {
	f := Forecast{Rate: rate, MixedTiers: s.MixedTiers()}
	run := s.simulate(usage, rate, now)
	f.DryAt, f.RefillAt, f.RefillEmail, f.Plan, f.Tracks = run.dryAt, run.refillAt, run.refillEmail, run.plan, run.tracks
	f.ExpiredLogins, f.LostResets = run.expired, run.lost
	switch {
	case len(f.ExpiredLogins) > 0:
		f.DryCause = DryLogin
	case !f.DryAt.IsZero():
		f.DryCause = DrySpend
	}
	switch {
	case !f.DryAt.IsZero():
		f.Pace = PaceOver
	case !s.simulate(usage, rate*poolMargin, now).dryAt.IsZero():
		f.Pace = PaceNear
	}
	return f
}

// poolRun — итог одного прогона
type poolRun struct {
	dryAt, refillAt time.Time
	refillEmail     string
	expired         []ExpiredLogin
	lost            []LostReset
	plan            []Spell
	tracks          []Track
}

// simulate — один прогон пула шагами poolStep
func (s *Store) simulate(usage map[string]Usage, rate float64, now time.Time) poolRun {
	// Копия: прогон меняет активную, хранилище — нет
	sim := &Store{Active: s.Active, Accounts: s.Accounts}
	logins := make(map[string]login, len(s.Accounts))
	for i := range s.Accounts {
		logins[s.Accounts[i].UUID] = loginOf(&s.Accounts[i])
	}
	weeks := map[string]Window{}
	for email, u := range usage {
		if u.Week.ResetsAt.After(now) {
			weeks[email] = u.Week
		}
	}
	var run poolRun
	tracks := make(map[string]*Track, len(s.Accounts))
	for i := range s.Accounts {
		a := &s.Accounts[i]
		if !logins[a.UUID].usableAt(now) {
			continue
		}
		tr := &Track{Email: a.Email, Points: []Point{{At: now, Used: weeks[a.Email].Used}}}
		if w, ok := weeks[a.Email]; ok {
			tr.Resets = append(tr.Resets, w.ResetsAt)
		}
		tracks[a.Email] = tr
	}
	mark := func(email string, at time.Time, used float64) {
		if tr := tracks[email]; tr != nil {
			tr.Points = append(tr.Points, Point{At: at, Used: used})
		}
	}
	spender := ""
	end := now.Add(PoolHorizon)
	for t := now; t.Before(end); t = t.Add(poolStep) {
		cur := make(map[string]Usage, len(weeks))
		for email, w := range weeks {
			if !w.ResetsAt.After(t) {
				mark(email, w.ResetsAt, w.Used)
				mark(email, w.ResetsAt, 0)
				delete(weeks, email)
				continue
			}
			cur[email] = Usage{Email: email, Week: w}
		}
		at := t
		best, _ := pickFrom(sim, statesWith(sim, cur, t, func(a *Account) bool { return logins[a.UUID].usableAt(at) }))
		if best == nil {
			if spender != "" {
				mark(spender, t, weeks[spender].Used)
				spender = ""
			}
			if run.dryAt.IsZero() {
				run.dryAt = t
				run.expired = s.expiredLogins(logins, weeks, now)
				run.refillAt, run.refillEmail, run.lost = s.refill(logins, weeks, now)
			}
			run.plan = extend(run.plan, "", t)
			continue
		}
		a := best.account
		sim.Active = a.UUID
		w, ok := weeks[a.Email]
		if !ok {
			w = Window{ResetsAt: t.Add(weekWindow)}
			if tr := tracks[a.Email]; tr != nil {
				tr.Resets = append(tr.Resets, w.ResetsAt)
			}
		}
		if spender != a.Email {
			if spender != "" {
				mark(spender, t, weeks[spender].Used)
			}
			mark(a.Email, t, w.Used)
			spender = a.Email
		}
		w.Used += rate * poolStep.Hours()
		weeks[a.Email] = w
		run.plan = extend(run.plan, a.Email, t)
	}
	for i := range s.Accounts {
		if tr := tracks[s.Accounts[i].Email]; tr != nil {
			tr.Points = append(tr.Points, Point{At: end, Used: weeks[tr.Email].Used})
			run.tracks = append(run.tracks, *tr)
		}
	}
	return run
}

// expiredLogins — учётки, годные в начале прогона now, чья неделя в момент
// опустения не пуста: взять их не дал истёкший вход, пул пуст из-за входа, не
// расхода (учётку с годным входом и запасом прогон бы взял). weeks — недели
// в момент опустения, учётки без недели в weeks начинают её с нуля
func (s *Store) expiredLogins(logins map[string]login, weeks map[string]Window, now time.Time) []ExpiredLogin {
	var out []ExpiredLogin
	for i := range s.Accounts {
		a := &s.Accounts[i]
		l := logins[a.UUID]
		if !l.usableAt(now) {
			continue
		}
		if used := weeks[a.Email].Used; used < WeekEmptyPercent {
			out = append(out, ExpiredLogin{Email: a.Email, LoginEnds: l.ends, Used: used})
		}
	}
	return out
}

// refill — ближайший сброс недели, после которого учётку можно взять: её
// вход к сбросу годится. Сбросы раньше него у учёток, годных в начале
// прогона now, но с мёртвым к сбросу входом, — в lost. weeks — недели в
// момент опустения, все сбросы в них позже него
func (s *Store) refill(logins map[string]login, weeks map[string]Window, now time.Time) (time.Time, string, []LostReset) {
	var at time.Time
	email := ""
	var lost []LostReset
	for i := range s.Accounts {
		a := &s.Accounts[i]
		l := logins[a.UUID]
		w, ok := weeks[a.Email]
		if !ok || !l.usableAt(now) {
			continue
		}
		switch {
		case !l.usableAt(w.ResetsAt):
			lost = append(lost, LostReset{Email: a.Email, ResetsAt: w.ResetsAt, LoginEnds: l.ends})
		case at.IsZero() || w.ResetsAt.Before(at):
			at, email = w.ResetsAt, a.Email
		}
	}
	lost = slices.DeleteFunc(lost, func(r LostReset) bool { return !at.IsZero() && !r.ResetsAt.Before(at) })
	slices.SortStableFunc(lost, func(x, y LostReset) int { return x.ResetsAt.Compare(y.ResetsAt) })
	return at, email, lost
}

// extend продлевает последний отрезок плана или начинает новый
func extend(plan []Spell, email string, t time.Time) []Spell {
	if n := len(plan); n > 0 && plan[n-1].Email == email {
		plan[n-1].To = t.Add(poolStep)
		return plan
	}
	return append(plan, Spell{Email: email, From: t, To: t.Add(poolStep)})
}

// MixedTiers — у живых учёток разные тарифы (среди тех, чей тариф известен)
func (s *Store) MixedTiers() bool {
	return len(s.liveTiers().known) > 1
}

// Tier — общий тариф живых учёток (rateLimitTier). Пусто, когда тарифы
// разные или тариф хоть одной живой учётки не известен (токены не
// разобрать, тарифа в них нет): подтвердить им цену плана нельзя
func (s *Store) Tier() string {
	if t := s.liveTiers(); len(t.known) == 1 && !t.unknown {
		return t.known[0]
	}
	return ""
}

// tierSet — разные известные тарифы живых учёток в порядке учёток; unknown —
// у какой-то живой учётки тариф не известен
type tierSet struct {
	known   []string
	unknown bool
}

// liveTiers — тарифы живых учёток
func (s *Store) liveTiers() tierSet {
	var t tierSet
	if s == nil {
		return t
	}
	for i := range s.Accounts {
		if s.Accounts[i].Dead != "" {
			continue
		}
		o, err := s.Accounts[i].oauth()
		switch {
		case err != nil:
			// токены не разобрать — причина при сверке хранилища (store.go)
			t.unknown = true
		case o.RateLimitTier == "":
			t.unknown = true
		case !slices.Contains(t.known, o.RateLimitTier):
			t.known = append(t.known, o.RateLimitTier)
		}
	}
	return t
}
