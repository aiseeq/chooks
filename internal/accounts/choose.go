package accounts

import (
	"fmt"
	"time"
)

// Пороги «пустой» учётки. Неделя: на 100% запросы отклоняются, а строка
// статуса долю из ответа 429 не получает (последняя удачная доля так и
// висит), поэтому порог должен сработать на последней удачной доле. Строка
// статуса отдаёт долю целым числом: на пороге 99.5 переключение срабатывало
// только на целом 100, когда запросы уже отклонялись (22.10: 13:52:52 доля
// 99, 14:00:11 — 100 и 429), порог 99 срабатывает на целом 99 (пользователь
// 22.10: «Сместим на пол-процента планку?»)
const WeekEmptyPercent = 99.0

// weekWindow — недельное окно: неначатая неделя кончится не раньше
const weekWindow = 168 * time.Hour

// loginMargin — вход, чья цепочка токенов истекает раньше, не выбирается:
// переключение на него оборвётся сразу после
const loginMargin = time.Hour

// Usage — расход учётки по снимку строки статуса (по почте учётки)
type Usage struct {
	Email string
	Week  Window
}

// state — что ротация знает об учётке сейчас
type state struct {
	account *Account
	week    Window
	// reason — общий диагноз годности входа и недельного лимита.
	reason      accountReason
	detail      string
	loginEnds   time.Time
	loginUsable bool
	// weekKnown — сброс недели известен и ещё не прошёл
	weekKnown bool
	// resets — сброс для сравнения: неизвестная неделя считается
	// неначатой и кончится через полное окно
	resets time.Time
	empty  bool
}

type accountReason uint8

const (
	accountAvailable accountReason = iota
	accountDead
	accountBroken
	accountCleared
	accountLoginExpiring
	accountWeekUnknown
	accountWeekFull
)

// inspectAccount вычисляет состояние входа и недели единым правилом для
// выбора, замеров и объяснения отказа переключения.
func inspectAccount(a *Account, usage map[string]Usage, now time.Time) state {
	st := state{account: a, resets: now.Add(weekWindow)}
	if a.Dead != "" {
		st.reason = accountDead
		st.detail = a.Dead
	} else if o, err := a.oauth(); err != nil {
		st.reason = accountBroken
		st.detail = err.Error()
	} else if o.cleared() {
		st.reason = accountCleared
	} else {
		st.loginUsable = true
		if family := o.family(); family > 0 {
			st.loginEnds = time.UnixMilli(family)
			if !st.loginEnds.After(now.Add(loginMargin)) {
				st.loginUsable = false
				st.reason = accountLoginExpiring
			}
		}
	}
	if u := usage[a.Email].Week; u.ResetsAt.After(now) {
		st.week, st.weekKnown, st.resets = u, true, u.ResetsAt
		if u.Used >= WeekEmptyPercent {
			st.empty = true
			if st.loginUsable {
				st.reason = accountWeekFull
			}
		} else if st.loginUsable {
			st.reason = accountAvailable
		}
	} else if st.loginUsable {
		st.reason = accountWeekUnknown
	}
	return st
}

// eligible — вход учётки жив и не истекает в ближайший час
func eligible(a *Account, now time.Time) bool {
	return inspectAccount(a, nil, now).loginUsable
}

// states собирает состояние живых учёток в порядке хранилища
func states(s *Store, usage map[string]Usage, now time.Time) []state {
	return statesWith(s, usage, now, nil)
}

// statesWith — states с проверкой входа от вызывающего
func statesWith(s *Store, usage map[string]Usage, now time.Time, usable func(*Account) bool) []state {
	var out []state
	for i := range s.Accounts {
		a := &s.Accounts[i]
		st := inspectAccount(a, usage, now)
		if usable == nil {
			if !st.loginUsable {
				continue
			}
		} else if !usable(a) {
			continue
		}
		out = append(out, st)
	}
	return out
}

// choose выбирает учётку по правилу пользователя (22.10): из непустых —
// с ближайшим сбросом недели, чтобы её остаток не сгорел. Равные сбросы —
// остаётся активная. Все пусты — остаётся активная: лучше нет.
// Возвращает выбранную учётку (nil — выбрать не из чего) и причину
func choose(s *Store, usage map[string]Usage, now time.Time) (*Account, string) {
	result := chooseResult(s, usage, now)
	return result.account, result.reason
}

type chooseOutcome uint8

const (
	chooseOrdinary chooseOutcome = iota
	chooseAllEmpty
	chooseNoLiveAccounts
)

type chooseResultValue struct {
	account *Account
	reason  string
	outcome chooseOutcome
}

func chooseResult(s *Store, usage map[string]Usage, now time.Time) chooseResultValue {
	best, all := pick(s, usage, now)
	if best == nil {
		if active := s.byUUID(s.Active); active != nil {
			return chooseResultValue{account: active, reason: "все учётки пусты", outcome: chooseAllEmpty}
		}
		return chooseResultValue{reason: "нет живых учёток", outcome: chooseNoLiveAccounts}
	}
	if best.account.UUID == s.Active {
		return chooseResultValue{account: best.account, reason: "активная — ближайший сброс"}
	}
	// Доли в причине — в % недели подписки за $100, как в строке статуса и
	// chooks limits (лид 12.11 17:39 по слову пользователя «вот прям везде»)
	unit := s.PlanUnit(now)
	reason := "неделя не начата"
	if best.weekKnown {
		reason = fmt.Sprintf("сброс %s, неделя %s", ShortTime(best.resets), unit.Percent(best.week.Used))
	}
	if active := s.byUUID(s.Active); active != nil {
		var activeState *state
		for i := range all {
			if all[i].account == active {
				activeState = &all[i]
				break
			}
		}
		if activeState == nil {
			return chooseResultValue{account: best.account, reason: fmt.Sprintf("вход %s не годится; %s", active.Email, reason)}
		}
		if activeState.empty {
			return chooseResultValue{account: best.account, reason: fmt.Sprintf("%s пуста (неделя %s); %s", active.Email, unit.Percent(activeState.week.Used), reason)}
		}
	}
	return chooseResultValue{account: best.account, reason: "ближе сброс: " + reason}
}

// pick — правило выбора без причины: из непустых живых учёток — с ближайшим
// сбросом недели, равные сбросы — активная. nil — непустых нет. Вторым —
// все живые учётки. Его же прогоняет прогноз пула
func pick(s *Store, usage map[string]Usage, now time.Time) (*state, []state) {
	return pickFrom(s, states(s, usage, now))
}

// pickFrom — pick по уже собранным состояниям
func pickFrom(s *Store, all []state) (*state, []state) {
	var best *state
	for i := range all {
		st := &all[i]
		if st.empty {
			continue
		}
		if best == nil || st.resets.Before(best.resets) ||
			st.resets.Equal(best.resets) && st.account.UUID == s.Active && best.account.UUID != s.Active {
			best = st
		}
	}
	return best, all
}

// usageStale — расход неактивной учётки с неизвестной или пустой неделей не
// спрашивался дольше этого: неделя могла начаться или сброситься
const usageStale = 30 * time.Minute

// needsUsage — живые учётки, чья неделя неизвестна или пуста: без их доли и
// сброса правило выбора слепо, а частичный сброс недели (12.11) у неактивной
// учётки виден только замером API. Активная тоже: после /login сессии рисуют
// её неделю первой долей, а первая доля запись только поднимает (13.11 сброс
// с 99% до 0%, после /login запись так и стояла на 99%)
func needsUsage(s *Store, usage map[string]Usage, now time.Time) []*Account {
	var out []*Account
	for _, st := range states(s, usage, now) {
		a := st.account
		if (st.weekKnown && !st.empty) || now.Sub(a.UsageCheckedAt) < usageStale {
			continue
		}
		out = append(out, a)
	}
	return out
}

// weekdays — короткие имена дней недели
var weekdays = [...]string{"вс", "пн", "вт", "ср", "чт", "пт", "сб"}

// ShortTime пишет время местным: «пн 26.10 11:00»
func ShortTime(at time.Time) string {
	at = at.Local()
	return weekdays[at.Weekday()] + " " + at.Format("02.01 15:04")
}
