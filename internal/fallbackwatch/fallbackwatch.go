// Package fallbackwatch — счёт вызовов хуков, которые прокси chooks-hook
// отдал CLI мимо демона chooks serve, и решение об уведомлении. Счёт ведёт
// CLI запасного пути: при остановленном, убитом или зависшем демоне считать
// больше некому (противник этапа 2б, сценарий 7). Состояние — файл в каталоге
// состояния chooks под flock: параллельные хуки не теряют вызовов и не шлют
// двух уведомлений.
package fallbackwatch

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/aiseeq/chooks/internal/atomicfile"
	"github.com/aiseeq/chooks/internal/core"
)

// Window — скользящее окно счёта и наименьший промежуток между уведомлениями
const Window = time.Hour

// Имена файлов в каталоге состояния chooks
const (
	FileName = "fallbacks.json"
	LockName = ".fallbacks.lock"
)

// lockWait — сколько ждать блокировку: её держат на чтение и запись файла
// в несколько КБ
const lockWait = time.Second

// minKept — сколько последних вызовов хранится, если порог меньше: из них
// собираются причины уведомления
const minKept = 100

// Event — один вызов, исполненный CLI мимо демона
type Event struct {
	At     time.Time `json:"at"`
	Hook   string    `json:"hook"`
	Reason string    `json:"reason"`
}

type state struct {
	Events    []Event   `json:"events"`
	AlertedAt time.Time `json:"alerted_at,omitzero"`
}

// ReasonCount — причина ухода в CLI и число вызовов с ней в окне
type ReasonCount struct {
	Reason string
	Count  int
}

// Decision — итог записи вызова: Alert — пора уведомить; Count — вызовов
// в окне (не больше хранимых), Reasons — их причины по убыванию числа.
// StateReset — прежний файл состояния был нечитаем и заменён новым: счёт
// начат заново
type Decision struct {
	Alert      bool
	Count      int
	Reasons    []ReasonCount
	StateReset error
}

// Record записывает ev в каталог dir и решает, уведомлять ли: вызовов за
// Window стало больше threshold, а прошлое уведомление было не позже Window
// назад. threshold 0 — вызов записывается, уведомления нет. Ошибка — вызов
// не записан, решения нет
func Record(dir string, threshold int, ev Event) (Decision, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Decision{}, fmt.Errorf("fallback state dir: %w", err)
	}
	lock, err := os.OpenFile(filepath.Join(dir, LockName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return Decision{}, fmt.Errorf("fallback lock: %w", err)
	}
	defer lock.Close()
	if err := core.LockFile(lock, time.Now().Add(lockWait)); err != nil {
		return Decision{}, fmt.Errorf("fallback lock: %w", err)
	}
	path := filepath.Join(dir, FileName)
	st, readErr := load(path)
	decision := st.add(ev, threshold)
	decision.StateReset = readErr
	data, err := json.Marshal(st)
	if err != nil {
		return Decision{}, fmt.Errorf("fallback state: %w", err)
	}
	if err := atomicfile.Write(path, data, 0o600); err != nil {
		// Без записи уведомление ушло бы снова на следующем вызове: не шлём
		return Decision{}, err
	}
	return decision, nil
}

// load читает состояние; файла нет — пустое, нечитаемое — пустое с ошибкой:
// счёт начинается заново, причина уходит в Decision.StateReset
func load(path string) (state, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return state{}, nil
	}
	if err != nil {
		return state{}, fmt.Errorf("fallback state: %w", err)
	}
	var st state
	if err := json.Unmarshal(data, &st); err != nil {
		return state{}, fmt.Errorf("fallback state %s unreadable, started anew: %w", path, err)
	}
	return st, nil
}

// add добавляет вызов, отбрасывает вышедшие из окна и лишние и решает об
// уведомлении
func (st *state) add(ev Event, threshold int) Decision {
	since := ev.At.Add(-Window)
	st.Events = slices.DeleteFunc(st.Events, func(e Event) bool { return !e.At.After(since) })
	st.Events = append(st.Events, ev)
	if keep := max(threshold+1, minKept); len(st.Events) > keep {
		st.Events = slices.Delete(st.Events, 0, len(st.Events)-keep)
	}
	d := Decision{Count: len(st.Events), Reasons: reasons(st.Events)}
	if threshold > 0 && d.Count > threshold && (st.AlertedAt.IsZero() || !ev.At.Before(st.AlertedAt.Add(Window))) {
		d.Alert = true
		st.AlertedAt = ev.At
	}
	return d
}

func reasons(events []Event) []ReasonCount {
	counts := map[string]int{}
	for _, e := range events {
		counts[e.Reason]++
	}
	out := make([]ReasonCount, 0, len(counts))
	for reason, n := range counts {
		out = append(out, ReasonCount{Reason: reason, Count: n})
	}
	slices.SortFunc(out, func(a, b ReasonCount) int {
		return cmp.Or(cmp.Compare(b.Count, a.Count), strings.Compare(a.Reason, b.Reason))
	})
	return out
}

// Summary — причины одной строкой: «connect: … ×6; busy ×5»
func (d Decision) Summary() string {
	parts := make([]string, 0, len(d.Reasons))
	for _, r := range d.Reasons {
		parts = append(parts, fmt.Sprintf("%s ×%d", r.Reason, r.Count))
	}
	return strings.Join(parts, "; ")
}
