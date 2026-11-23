package fallbackwatch

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)

func record(t *testing.T, dir string, threshold int, at time.Time, reason string) Decision {
	t.Helper()
	d, err := Record(dir, threshold, Event{At: at, Hook: "pre-tool-use", Reason: reason})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// Ровно при N уведомления нет, на N+1 — одно, дальше в том же часе — нет;
// вызовы старше часа выходят из окна, и уведомление снова возможно не
// раньше часа после прошлого
func TestThresholdAndWindow(t *testing.T) {
	const n = 10
	dir := t.TempDir()
	for i := range n {
		if d := record(t, dir, n, t0.Add(time.Duration(i)*time.Second), "connect: refused"); d.Alert || d.Count != i+1 {
			t.Fatalf("call %d: %+v", i+1, d)
		}
	}
	d := record(t, dir, n, t0.Add(n*time.Second), "busy")
	if !d.Alert || d.Count != n+1 {
		t.Fatalf("call N+1: %+v", d)
	}
	if got, want := d.Summary(), "connect: refused ×10; busy ×1"; got != want {
		t.Fatalf("summary %q, want %q", got, want)
	}
	for i := range 5 {
		if d := record(t, dir, n, t0.Add(time.Duration(20+i)*time.Second), "busy"); d.Alert {
			t.Fatalf("second alert within the hour at call %d: %+v", n+2+i, d)
		}
	}
	// Через час с четвертью все прежние вызовы вне окна: счёт заново
	later := t0.Add(75 * time.Minute)
	for i := range n {
		if d := record(t, dir, n, later.Add(time.Duration(i)*time.Second), "ack: timeout"); d.Alert || d.Count != i+1 {
			t.Fatalf("after window, call %d: %+v", i+1, d)
		}
	}
	if d := record(t, dir, n, later.Add(n*time.Second), "ack: timeout"); !d.Alert {
		t.Fatalf("after window, call N+1: %+v", d)
	}
}

// Вызов за 59 минут до текущего ещё в окне, за 60 — уже нет
func TestWindowEdge(t *testing.T) {
	dir := t.TempDir()
	record(t, dir, 1, t0, "a")
	if d := record(t, dir, 1, t0.Add(59*time.Minute), "b"); !d.Alert || d.Count != 2 {
		t.Fatalf("59 min: %+v", d)
	}
	dir = t.TempDir()
	record(t, dir, 1, t0, "a")
	if d := record(t, dir, 1, t0.Add(time.Hour), "b"); d.Alert || d.Count != 1 {
		t.Fatalf("60 min: %+v", d)
	}
}

// Порог 0 — счёт без уведомления
func TestDisabled(t *testing.T) {
	dir := t.TempDir()
	for i := range 30 {
		if d := record(t, dir, 0, t0.Add(time.Duration(i)*time.Second), "x"); d.Alert {
			t.Fatalf("alert with threshold 0: %+v", d)
		}
	}
}

// Параллельные хуки: все вызовы учтены, уведомление одно
func TestConcurrentRecords(t *testing.T) {
	dir := t.TempDir()
	var alerts atomic.Int64
	var wg sync.WaitGroup
	for i := range 40 {
		wg.Go(func() {
			d, err := Record(dir, 10, Event{At: t0.Add(time.Duration(i) * time.Millisecond), Hook: "stop", Reason: fmt.Sprint("r", i%3)})
			if err != nil {
				t.Error(err)
			}
			if d.Alert {
				alerts.Add(1)
			}
		})
	}
	wg.Wait()
	if n := alerts.Load(); n != 1 {
		t.Fatalf("%d alerts, want 1", n)
	}
	if d := record(t, dir, 10, t0.Add(time.Second), "r0"); d.Count != 41 {
		t.Fatalf("count %d, want 41", d.Count)
	}
}

// Нечитаемый файл состояния заменяется: вызов записан, причина — в
// StateReset
func TestUnreadableState(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := Record(dir, 10, Event{At: t0, Hook: "stop", Reason: "x"})
	if err != nil || d.StateReset == nil || d.Count != 1 {
		t.Fatalf("decision %+v err %v", d, err)
	}
	if d := record(t, dir, 10, t0.Add(time.Second), "x"); d.Count != 2 || d.StateReset != nil {
		t.Fatalf("after repair: %+v", d)
	}
}
