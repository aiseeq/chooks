package nighthint

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// at — местное время 15.11 (или следующего дня) в часах и минутах
func at(day, hour, minute int) time.Time {
	return time.Date(2026, 11, day, hour, minute, 0, 0, time.Local)
}

// fakeDeps — зависимости с подменённым временем и признаком блокировки;
// calls считает вызовы признака
type fakeDeps struct {
	now    time.Time
	locked bool
	err    error
	calls  int
	store  Store
	// skill — признак вызова autonomous-work; nil — признака нет.
	// skillCalls считает обращения к нему
	skill      func(sessionID string) (bool, error)
	skillCalls int
}

func newFake(t *testing.T, now time.Time, locked bool) *fakeDeps {
	t.Helper()
	return &fakeDeps{now: now, locked: locked, store: Store{Dir: filepath.Join(t.TempDir(), "night-hint")}}
}

func (f *fakeDeps) deps() Deps {
	deps := Deps{
		Now: func() time.Time { return f.now },
		Locked: func(context.Context) (bool, error) {
			f.calls++
			return f.locked, f.err
		},
		Store: f.store,
	}
	if f.skill != nil {
		deps.SkillCalled = func(sessionID string) (bool, error) {
			f.skillCalls++
			return f.skill(sessionID)
		}
	}
	return deps
}

func (f *fakeDeps) run(t *testing.T, event Event) Result {
	t.Helper()
	result, err := Run(context.Background(), f.deps(), event)
	if err != nil {
		t.Fatalf("Run(%+v): %v", event, err)
	}
	return result
}

// eventOf — событие с тем, что включает подсказку у каждого из четырёх
func eventOf(name string) Event {
	event := Event{Name: name, SessionID: "s1"}
	switch name {
	case EventPreToolUse:
		event.ToolName = askTool
	case EventStop:
		event.LastMessage = "Сделано.\n❓ какой из двух вариантов берём?"
	}
	return event
}

var allEvents = []string{EventSessionStart, EventUserPromptSubmit, EventPreToolUse, EventStop}

// TestRunTable — заблокирован/нет × 21:59/22:30/08:59/09:01 для каждого из
// четырёх событий: подсказка только при блокировке в ночные часы
func TestRunTable(t *testing.T) {
	times := []struct {
		name  string
		now   time.Time
		night bool
	}{
		{"21:59", at(15, 21, 59), false},
		{"22:30", at(15, 22, 30), true},
		{"08:59", at(16, 8, 59), true},
		{"09:01", at(16, 9, 1), false},
	}
	for _, name := range allEvents {
		for _, tm := range times {
			for _, locked := range []bool{true, false} {
				t.Run(name+"/"+tm.name+"/locked="+map[bool]string{true: "да", false: "нет"}[locked], func(t *testing.T) {
					fake := newFake(t, tm.now, locked)
					result := fake.run(t, eventOf(name))
					want := locked && tm.night
					if got := result.Context != ""; got != want {
						t.Fatalf("подсказка = %t, ждали %t (skip %q)", got, want, result.Skip)
					}
					if result.Damaged != nil || result.LockErr != nil {
						t.Errorf("сбой на чистой записи: damaged %v, lock %v", result.Damaged, result.LockErr)
					}
					if want && result.Context != Text(tm.now) {
						t.Errorf("текст = %q", result.Context)
					}
					if want && !strings.Contains(result.Context, tm.name) {
						t.Errorf("в тексте нет времени %s: %q", tm.name, result.Context)
					}
					// Днём шина не спрашивается
					if !tm.night && fake.calls != 0 {
						t.Errorf("днём признак блокировки спрошен %d раз", fake.calls)
					}
					if tm.night && fake.calls != 1 {
						t.Errorf("ночью признак блокировки спрошен %d раз", fake.calls)
					}
				})
			}
		}
	}
}

func TestOncePerNight(t *testing.T) {
	for _, name := range []string{EventSessionStart, EventUserPromptSubmit} {
		t.Run(name, func(t *testing.T) {
			fake := newFake(t, at(15, 22, 30), true)
			if fake.run(t, eventOf(name)).Context == "" {
				t.Fatal("первый ход ночи без подсказки")
			}
			// Тот же вечер и утро той же ночи — без повтора, и для другого события
			for _, now := range []time.Time{at(15, 23, 50), at(16, 0, 10), at(16, 8, 59)} {
				fake.now = now
				for _, other := range []string{EventSessionStart, EventUserPromptSubmit} {
					if result := fake.run(t, eventOf(other)); result.Context != "" {
						t.Errorf("%s в %s: повтор подсказки той же ночи", other, now.Format("02.01 15:04"))
					}
				}
			}
			// Другая сессия той же ночи — своя подсказка
			event := eventOf(name)
			event.SessionID = "s2"
			if fake.run(t, event).Context == "" {
				t.Error("другая сессия той же ночи без подсказки")
			}
			// Следующая ночь — снова
			fake.now = at(16, 22, 0)
			if fake.run(t, eventOf(name)).Context == "" {
				t.Error("следующая ночь без подсказки")
			}
		})
	}
}

func TestUnlockedNightNotMarked(t *testing.T) {
	// Ночью при разблокированном экране подсказки нет и ночь не отмечается:
	// заблокировал позже — подсказка придёт
	fake := newFake(t, at(15, 22, 30), false)
	if fake.run(t, eventOf(EventUserPromptSubmit)).Context != "" {
		t.Fatal("подсказка при разблокированном экране")
	}
	fake.locked = true
	fake.now = at(15, 23, 10)
	if fake.run(t, eventOf(EventUserPromptSubmit)).Context == "" {
		t.Error("после блокировки той же ночью подсказки нет")
	}
}

func TestAskEveryCall(t *testing.T) {
	fake := newFake(t, at(15, 23, 0), true)
	for i := range 3 {
		if fake.run(t, eventOf(EventPreToolUse)).Context == "" {
			t.Fatalf("вызов %d AskUserQuestion без подсказки", i+1)
		}
	}
	if result := fake.run(t, Event{Name: EventPreToolUse, SessionID: "s1", ToolName: "Bash"}); result.Context != "" || fake.calls != 3 {
		t.Errorf("другой инструмент: подсказка %q, вызовов признака %d", result.Context, fake.calls)
	}
}

func TestStop(t *testing.T) {
	fake := newFake(t, at(15, 23, 0), true)
	plain := Event{Name: EventStop, SessionID: "s1", LastMessage: "Сделано, тесты зелёные."}
	if result := fake.run(t, plain); result.Context != "" || fake.calls != 0 {
		t.Errorf("ответ без ❓: подсказка %q, вызовов признака %d", result.Context, fake.calls)
	}
	question := eventOf(EventStop)
	if fake.run(t, question).Context == "" {
		t.Fatal("ответ с ❓ ночью без подсказки")
	}
	// Продолжение после подсказки снова с ❓ — без повтора: нет петли
	if fake.run(t, question).Context != "" {
		t.Error("второй Stop того же хода получил подсказку")
	}
	active := question
	active.StopHookActive = true
	if fake.run(t, active).Context != "" {
		t.Error("stop_hook_active получил подсказку")
	}
	// Новый ход снимает отметку
	fake.run(t, eventOf(EventUserPromptSubmit))
	if fake.run(t, question).Context == "" {
		t.Error("Stop с ❓ после новой реплики без подсказки")
	}
}

func TestStopFreshAfterActiveSkip(t *testing.T) {
	// stop_hook_active пропускается до отметки: следующий обычный Stop с ❓
	// подсказку получает
	fake := newFake(t, at(15, 23, 0), true)
	active := eventOf(EventStop)
	active.StopHookActive = true
	if fake.run(t, active).Context != "" || fake.calls != 0 {
		t.Fatal("stop_hook_active получил подсказку или спросил признак")
	}
	if fake.run(t, eventOf(EventStop)).Context == "" {
		t.Error("Stop с ❓ после пропуска stop_hook_active без подсказки")
	}
}

func TestStopDay(t *testing.T) {
	for _, locked := range []bool{true, false} {
		fake := newFake(t, at(15, 14, 0), locked)
		if fake.run(t, eventOf(EventStop)).Context != "" {
			t.Errorf("днём (заблокирован=%t) ❓ дал подсказку", locked)
		}
	}
}

func TestLockError(t *testing.T) {
	for _, name := range allEvents {
		fake := newFake(t, at(15, 23, 0), true)
		fake.err = errors.New("no system bus")
		result := fake.run(t, eventOf(name))
		if result.Context != "" || result.LockErr == nil {
			t.Errorf("%s: при ошибке признака подсказка %q, ошибка %v", name, result.Context, result.LockErr)
		}
		// Ошибка не отмечает ночь: признак вернулся — подсказка придёт
		fake.err = nil
		if fake.run(t, eventOf(name)).Context == "" {
			t.Errorf("%s: после ошибки признака подсказки нет", name)
		}
	}
}

func TestUnsupportedEvent(t *testing.T) {
	fake := newFake(t, at(15, 23, 0), true)
	if _, err := Run(context.Background(), fake.deps(), Event{Name: "PostToolUse", SessionID: "s1"}); !errors.Is(err, ErrUnsupportedEvent) {
		t.Errorf("ошибка = %v", err)
	}
}

func TestStoreErrors(t *testing.T) {
	fake := newFake(t, at(15, 23, 0), true)
	for _, name := range []string{EventSessionStart, EventUserPromptSubmit, EventStop} {
		event := eventOf(name)
		event.SessionID = ""
		if _, err := Run(context.Background(), fake.deps(), event); err == nil {
			t.Errorf("%s без сессии: нет ошибки", name)
		}
	}
	// AskUserQuestion записи не трогает: сессия ему не нужна
	event := eventOf(EventPreToolUse)
	event.SessionID = ""
	if result := fake.run(t, event); result.Context == "" {
		t.Error("AskUserQuestion без сессии без подсказки")
	}
}

func TestIsNightAndNightOf(t *testing.T) {
	cases := []struct {
		now   time.Time
		night bool
		of    string
	}{
		{at(15, 21, 59), false, "2026-11-15"},
		{at(15, 22, 0), true, "2026-11-15"},
		{at(15, 23, 59), true, "2026-11-15"},
		{at(16, 0, 0), true, "2026-11-15"},
		{at(16, 8, 59), true, "2026-11-15"},
		{at(16, 9, 0), false, "2026-11-16"},
		{at(1, 3, 0), true, "2026-10-31"},
	}
	for _, c := range cases {
		if got := IsNight(c.now); got != c.night {
			t.Errorf("IsNight(%s) = %t", c.now.Format("02.01 15:04"), got)
		}
		if got := NightOf(c.now); got != c.of {
			t.Errorf("NightOf(%s) = %s, ждали %s", c.now.Format("02.01 15:04"), got, c.of)
		}
	}
}

func TestHasQuestion(t *testing.T) {
	if HasQuestion("Готово.\n✅ сделано") {
		t.Error("ответ без ❓")
	}
	if !HasQuestion("Готово.\n- ❓ нужен доступ к pc") {
		t.Error("строка списка с ❓")
	}
}

func TestStoreRecords(t *testing.T) {
	store := Store{Dir: filepath.Join(t.TempDir(), "night-hint")}
	// Записи нет — снимать нечего, каталог не создаётся
	if err := store.ResetStop("s1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(store.Dir); !os.IsNotExist(err) {
		t.Errorf("ResetStop без записи создал каталог: %v", err)
	}
	if err := store.Sweep(time.Now()); err != nil {
		t.Errorf("Sweep без каталога: %v", err)
	}
	// Испорченная запись — пустая
	if err := os.MkdirAll(store.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.Dir, "s1.json")
	if err := os.WriteFile(path, []byte(`{"night":"2026-11-15","stop_hin`), 0o600); err != nil {
		t.Fatal(err)
	}
	if fresh, err := store.MarkNight("s1", "2026-11-15"); !errors.Is(err, ErrRecordDamaged) || !fresh {
		t.Errorf("испорченная запись: fresh %t, %v", fresh, err)
	}
	if fresh, err := store.MarkStop("s1"); err != nil || !fresh {
		t.Errorf("MarkStop: fresh %t, %v", fresh, err)
	}
	// Отметка Stop не стирает ночь
	if fresh, err := store.MarkNight("s1", "2026-11-15"); err != nil || fresh {
		t.Errorf("ночь после MarkStop: fresh %t, %v", fresh, err)
	}
	if err := store.ResetStop("s1"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"night":"2026-11-15"}` {
		t.Errorf("запись после ResetStop = %s", data)
	}
	// Имя сессии в путь не попадает
	if _, err := store.MarkNight("../s1", "2026-11-15"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(store.Dir), "s1.json")); !os.IsNotExist(err) {
		t.Errorf("запись вне каталога: %v", err)
	}
	for _, bad := range []string{"", " ", "/", "."} {
		if _, err := store.MarkStop(bad); err == nil {
			t.Errorf("сессия %q: нет ошибки", bad)
		}
	}
}

func TestStoreSweep(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	now := time.Now()
	old := filepath.Join(store.Dir, "old.json")
	fresh := filepath.Join(store.Dir, "fresh.json")
	other := filepath.Join(store.Dir, "keep.txt")
	for _, path := range []string{old, fresh, other} {
		if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	stale := now.Add(-recordTTL - time.Minute)
	for _, path := range []string{old, other} {
		if err := os.Chtimes(path, stale, stale); err != nil {
			t.Fatal(err)
		}
	}
	almost := now.Add(-recordTTL + time.Minute)
	if err := os.Chtimes(fresh, almost, almost); err != nil {
		t.Fatal(err)
	}
	if err := store.Sweep(now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("старая запись осталась: %v", err)
	}
	for _, path := range []string{fresh, other} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s убран: %v", filepath.Base(path), err)
		}
	}
}

func TestDamagedRecord(t *testing.T) {
	write := func(t *testing.T, fake *fakeDeps, body string) string {
		t.Helper()
		if err := os.MkdirAll(fake.store.Dir, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(fake.store.Dir, "s1.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	for _, name := range []string{EventSessionStart, EventUserPromptSubmit, EventStop} {
		t.Run(name, func(t *testing.T) {
			// Испорченная запись: подсказка дана, сбой — рядом с результатом
			fake := newFake(t, at(15, 23, 0), true)
			write(t, fake, `{"night":`)
			result := fake.run(t, eventOf(name))
			if result.Context == "" || !errors.Is(result.Damaged, ErrRecordDamaged) {
				t.Errorf("подсказка %q, damaged %v", result.Context, result.Damaged)
			}
			// Запись заменена целой: второй ход без повтора и без сбоя
			result = fake.run(t, eventOf(name))
			if result.Context != "" || result.Damaged != nil {
				t.Errorf("второй ход: подсказка %q, damaged %v", result.Context, result.Damaged)
			}
		})
	}
	t.Run("UserPromptSubmit day", func(t *testing.T) {
		// Днём испорченную запись чинит снятие отметки Stop, решение — день
		fake := newFake(t, at(15, 14, 0), true)
		path := write(t, fake, `{"stop_hinted":tr`)
		result := fake.run(t, eventOf(EventUserPromptSubmit))
		if result.Context != "" || result.Skip != "день" || !errors.Is(result.Damaged, ErrRecordDamaged) {
			t.Errorf("результат %+v", result)
		}
		data, err := os.ReadFile(path)
		if err != nil || string(data) != `{}` {
			t.Errorf("запись после ResetStop = %q, %v", data, err)
		}
	})
	for _, name := range []string{EventSessionStart, EventStop} {
		t.Run(name+" unreadable", func(t *testing.T) {
			// Запись не открыть — сбой решения, подсказки нет
			fake := newFake(t, at(15, 23, 0), true)
			if err := os.MkdirAll(filepath.Join(fake.store.Dir, "s1.json"), 0o700); err != nil {
				t.Fatal(err)
			}
			if _, err := Run(context.Background(), fake.deps(), eventOf(name)); err == nil || errors.Is(err, ErrRecordDamaged) {
				t.Errorf("ошибка = %v", err)
			}
		})
	}
}

// Адрес ночного вопроса — админ пространства (autonomous-work п.3 с 16.11,
// тикет night-hint-question-to-admin), не прежний «блоком в Сон»
func TestTextAddressesSpaceAdmin(t *testing.T) {
	text := Text(time.Date(2026, 11, 15, 23, 5, 0, 0, time.Local))
	if !strings.Contains(text, "админу пространства") {
		t.Errorf("в подсказке нет адреса «админу пространства»: %q", text)
	}
	for _, old := range []string{"блоком в Сон", "проект Сна"} {
		if strings.Contains(text, old) {
			t.Errorf("в подсказке прежний адрес %q: %q", old, text)
		}
	}
	if !strings.Contains(text, "23:05") {
		t.Errorf("в подсказке нет времени: %q", text)
	}
}

// Сессия после /clear получила подсказку на SessionStart, первый ход будит
// письмо другой сессии: пока autonomous-work не вызван, подсказка на каждом
// UserPromptSubmit, после вызова — раз за ночь (тикет
// night-hint-after-clear-first-turn)
func TestPromptRepeatsUntilSkillCalled(t *testing.T) {
	fake := newFake(t, at(16, 5, 6), true)
	called := false
	fake.skill = func(sessionID string) (bool, error) {
		if sessionID != "s1" {
			t.Errorf("признак спрошен для сессии %q", sessionID)
		}
		return called, nil
	}
	start := eventOf(EventSessionStart)
	start.Source = "clear"
	if fake.run(t, start).Context == "" {
		t.Fatal("SessionStart(clear) ночью без подсказки")
	}
	for i := range 2 {
		fake.now = fake.now.Add(time.Minute)
		result := fake.run(t, eventOf(EventUserPromptSubmit))
		if result.Context != Text(fake.now) || result.SkillErr != nil {
			t.Fatalf("ход %d без вызова скилла: подсказка %q, skip %q, сбой %v", i+1, result.Context, result.Skip, result.SkillErr)
		}
	}
	called = true
	for i := range 2 {
		fake.now = fake.now.Add(time.Minute)
		if result := fake.run(t, eventOf(EventUserPromptSubmit)); result.Context != "" || result.SkillErr != nil {
			t.Fatalf("ход %d после вызова скилла: подсказка %q, сбой %v", i+1, result.Context, result.SkillErr)
		}
	}
	// SessionStart по-прежнему раз за ночь: resume без вызова скилла — без
	// повтора, признак не спрашивается
	called = false
	asked := fake.skillCalls
	resume := eventOf(EventSessionStart)
	resume.Source = "resume"
	if fake.run(t, resume).Context != "" || fake.skillCalls != asked {
		t.Errorf("SessionStart(resume) той же ночи: повтор или признак спрошен (%d → %d)", asked, fake.skillCalls)
	}
}

// Первый ход ночи даёт подсказку и без признака: ночь ещё не отмечена,
// признак не спрашивается
func TestPromptFirstOfNightSkipsSkillCheck(t *testing.T) {
	fake := newFake(t, at(15, 23, 0), true)
	fake.skill = func(string) (bool, error) { return true, nil }
	if fake.run(t, eventOf(EventUserPromptSubmit)).Context == "" {
		t.Fatal("первый ход ночи без подсказки")
	}
	if fake.skillCalls != 0 {
		t.Errorf("признак спрошен %d раз на первом ходе ночи", fake.skillCalls)
	}
}

// Признака нет (нет записи skill-reload, выключен, сбой чтения) — прежнее
// правило «раз за ночь», причина — в SkillErr
func TestPromptOnceWithoutSkillState(t *testing.T) {
	noRecord := errors.New("no record")
	for name, skill := range map[string]func(string) (bool, error){
		"нет признака":  nil,
		"сбой признака": func(string) (bool, error) { return false, noRecord },
	} {
		t.Run(name, func(t *testing.T) {
			fake := newFake(t, at(15, 23, 0), true)
			fake.skill = skill
			if fake.run(t, eventOf(EventSessionStart)).Context == "" {
				t.Fatal("SessionStart ночью без подсказки")
			}
			for i := range 2 {
				result := fake.run(t, eventOf(EventUserPromptSubmit))
				if result.Context != "" {
					t.Fatalf("ход %d: повтор подсказки без признака", i+1)
				}
				want := ErrSkillUnknown
				if skill != nil {
					want = noRecord
				}
				if !errors.Is(result.SkillErr, want) {
					t.Errorf("ход %d: SkillErr = %v, ждали %v", i+1, result.SkillErr, want)
				}
			}
		})
	}
}

// Днём и при разблокированном экране подсказки нет и признак не спрашивается
func TestPromptSkillCheckOnlyAtLockedNight(t *testing.T) {
	for name, fake := range map[string]*fakeDeps{
		"день":       newFake(t, at(16, 14, 0), true),
		"не заблок.": newFake(t, at(16, 2, 0), false),
	} {
		fake.skill = func(string) (bool, error) { return false, nil }
		for i := range 2 {
			if result := fake.run(t, eventOf(EventUserPromptSubmit)); result.Context != "" {
				t.Errorf("%s, ход %d: подсказка %q", name, i+1, result.Context)
			}
		}
		if fake.skillCalls != 0 {
			t.Errorf("%s: признак спрошен %d раз", name, fake.skillCalls)
		}
	}
}

// Подсказка велит вызвать скилл, а не называет его: «скилл autonomous-work»
// без глагола на свежем старте ночи 17→18.11 не вызвали 4 из 8 сессий без
// текста скилла в контексте (тикет night-hint-skill-share-1118). Сессия, у
// которой скилл уже в контексте (вызван или в invoked_skills после
// компакции), вызывать его заново не обязана
func TestTextOrdersSkillCall(t *testing.T) {
	text := Text(time.Date(2026, 11, 18, 4, 1, 0, 0, time.Local))
	for _, want := range []string{"вызови Skill autonomous-work", "первым действием", "если его текста нет в контексте"} {
		if !strings.Contains(text, want) {
			t.Errorf("в подсказке нет %q: %q", want, text)
		}
	}
}

func TestLidAndPresenceSkip(t *testing.T) {
	fake := newFake(t, at(19, 23, 0), false)
	deps := fake.deps()
	closed := false
	var lidErr error
	calls := 0
	deps.LidClosed = func(context.Context) (bool, error) { calls++; return closed, lidErr }
	event := eventOf(EventSessionStart)
	run := func() Result {
		t.Helper()
		result, err := Run(context.Background(), deps, event)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	if r := run(); r.Context != "" || !r.PresenceSkipInfo {
		t.Fatalf("first open: %+v", r)
	}
	if r := run(); r.Context != "" || r.PresenceSkipInfo {
		t.Fatalf("second open: %+v", r)
	}
	closed = true
	if r := run(); r.Context == "" {
		t.Fatalf("closed: %+v", r)
	}
	fake.locked = true
	callsBefore := calls
	if r := run(); r.Context != "" || calls != callsBefore {
		t.Fatalf("locked: %+v, lid calls %d", r, calls)
	}
	fake.locked = false
	closed = false
	lidErr = errors.New("lid unavailable")
	if r := run(); r.Context != "" || !errors.Is(r.LockErr, lidErr) {
		t.Fatalf("unknown: %+v", r)
	}
	lidErr = nil
	fake.now = at(20, 23, 0)
	if r := run(); !r.PresenceSkipInfo {
		t.Fatalf("next night: %+v", r)
	}
	event.SessionID = "s2"
	if r := run(); !r.PresenceSkipInfo {
		t.Fatalf("different session: %+v", r)
	}
}
