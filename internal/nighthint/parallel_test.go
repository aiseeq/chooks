package nighthint

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Параллельные SessionStart/UserPromptSubmit одной сессии ночью —
// ровно одна подсказка
func TestParallelOncePerNight(t *testing.T) {
	for round := range 50 {
		store := Store{Dir: filepath.Join(t.TempDir(), "nh")}
		deps := Deps{
			Now:    func() time.Time { return time.Date(2026, 11, 15, 23, 0, 0, 0, time.Local) },
			Locked: func(context.Context) (bool, error) { return true, nil },
			Store:  store,
		}
		var given atomic.Int32
		var wg sync.WaitGroup
		for i := range 16 {
			wg.Go(func() {
				name := EventSessionStart
				if i%2 == 1 {
					name = EventUserPromptSubmit
				}
				res, err := Run(context.Background(), deps, Event{Name: name, SessionID: "s"})
				if err != nil {
					t.Errorf("round %d: %v", round, err)
					return
				}
				if res.Context != "" {
					given.Add(1)
				}
				if res.Damaged != nil {
					t.Errorf("round %d: damaged %v", round, res.Damaged)
				}
			})
		}
		wg.Wait()
		if got := given.Load(); got != 1 {
			t.Fatalf("round %d: подсказок %d, ждали 1", round, got)
		}
	}
}

// Параллельные Stop с ❓ одного хода — одна подсказка
func TestParallelStopOnce(t *testing.T) {
	store := Store{Dir: filepath.Join(t.TempDir(), "nh")}
	deps := Deps{
		Now:    func() time.Time { return time.Date(2026, 11, 16, 3, 0, 0, 0, time.Local) },
		Locked: func(context.Context) (bool, error) { return true, nil },
		Store:  store,
	}
	var given atomic.Int32
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			res, err := Run(context.Background(), deps, Event{Name: EventStop, SessionID: "s", LastMessage: "❓ x"})
			if err != nil {
				t.Error(err)
			}
			if res.Context != "" {
				given.Add(1)
			}
		})
	}
	wg.Wait()
	if given.Load() != 1 {
		t.Fatalf("Stop подсказок %d", given.Load())
	}
}

// Ночь, разделённая полночью и сменой даты: 23:59 и 00:01 — одна ночь
func TestMidnightOneNight(t *testing.T) {
	store := Store{Dir: filepath.Join(t.TempDir(), "nh")}
	now := time.Date(2026, 11, 15, 23, 59, 0, 0, time.Local)
	deps := Deps{
		Now:    func() time.Time { return now },
		Locked: func(context.Context) (bool, error) { return true, nil },
		Store:  store,
	}
	run := func(name, source string) Result {
		t.Helper()
		r, err := Run(context.Background(), deps, Event{Name: name, SessionID: "s", Source: source})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	r1 := run(EventUserPromptSubmit, "")
	now = time.Date(2026, 11, 16, 0, 1, 0, 0, time.Local)
	r2 := run(EventUserPromptSubmit, "")
	if r1.Context == "" || r2.Context != "" {
		t.Fatalf("r1 %q r2 %q", r1.Context, r2.Context)
	}
	// 09:00 — день, 22:00 того же дня — новая ночь
	now = time.Date(2026, 11, 16, 9, 0, 0, 0, time.Local)
	if run(EventUserPromptSubmit, "").Context != "" {
		t.Fatal("09:00 дал подсказку")
	}
	now = time.Date(2026, 11, 16, 22, 0, 0, 0, time.Local)
	if run(EventSessionStart, "startup").Context == "" {
		t.Fatal("22:00 следующей ночи без подсказки")
	}
	// Сжатие и /clear уносят подсказку из контекста — она даётся снова;
	// resume и обычный ход той же ночи — нет
	for _, c := range []struct {
		name, source string
		want         bool
	}{
		{EventSessionStart, "resume", false},
		{EventUserPromptSubmit, "", false},
		{EventSessionStart, "compact", true},
		{EventSessionStart, "clear", true},
		{EventUserPromptSubmit, "", false},
	} {
		if got := run(c.name, c.source).Context != ""; got != c.want {
			t.Errorf("%s %s: подсказка %v, нужно %v", c.name, c.source, got, c.want)
		}
	}
}
