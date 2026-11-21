package nighthint

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aiseeq/chooks/internal/core"
)

// lockWait — сколько ждать запись сессии: SessionStart и UserPromptSubmit
// новой сессии приходят почти одновременно, и без блокировки подсказка ушла
// бы дважды. Оба держат её миллисекунды
const lockWait = time.Second

// recordTTL — запись сессии старше этого убирается при записи другой:
// сессия, не тронутая неделю, следующей ночью получит подсказку заново
const recordTTL = 7 * 24 * time.Hour

// ErrRecordDamaged — запись сессии была испорчена и заменена целой. Это не
// сбой решения: худшее последствие — повтор подсказки; причина — в лог
var ErrRecordDamaged = errors.New("night-hint record damaged, replaced")

// record — что подсказка уже дала сессии
type record struct {
	// Night — ночь (NightOf), в которую дана подсказка SessionStart или
	// UserPromptSubmit
	Night             string `json:"night,omitempty"`
	PresenceSkipNight string `json:"presence_skip_night,omitempty"`
	// StopHinted — подсказка на Stop дана в этом ходе; снимает её
	// следующий UserPromptSubmit
	StopHinted bool `json:"stop_hinted,omitempty"`
}

// Store — записи сессий: файл `<сессия>.json` в Dir
type Store struct {
	Dir string
}

// DefaultDir — $XDG_STATE_HOME/chooks/night-hint, рядом с skill-reload:
// сессию продолжают и после перезагрузки
func DefaultDir() (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot resolve home dir: %w", err)
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "chooks", "night-hint"), nil
}

// MarkNight отмечает подсказку ночи night; false — она этой ночью уже дана
func (s Store) MarkNight(sessionID, night string) (bool, error) {
	return s.update(sessionID, func(rec *record) bool {
		if rec.Night == night {
			return false
		}
		rec.Night = night
		return true
	})
}

// MarkPresenceSkip отмечает первый за ночь пропуск по присутствию.
func (s Store) MarkPresenceSkip(sessionID, night string) (bool, error) {
	return s.update(sessionID, func(rec *record) bool {
		if rec.PresenceSkipNight == night {
			return false
		}
		rec.PresenceSkipNight = night
		return true
	})
}

// MarkStop отмечает подсказку на Stop; false — она в этом ходе уже дана
func (s Store) MarkStop(sessionID string) (bool, error) {
	return s.update(sessionID, func(rec *record) bool {
		if rec.StopHinted {
			return false
		}
		rec.StopHinted = true
		return true
	})
}

// ResetStop снимает отметку подсказки на Stop. Записи нет или отметки нет —
// файл не открывается на запись: UserPromptSubmit приходит на каждый ход
func (s Store) ResetStop(sessionID string) error {
	path, err := s.path(sessionID)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read record: %w", err)
	}
	if rec, err := parseRecord(data); err == nil && !rec.StopHinted {
		return nil
	}
	_, err = s.update(sessionID, func(rec *record) bool {
		if !rec.StopHinted {
			return false
		}
		rec.StopHinted = false
		return true
	})
	return err
}

// update меняет запись сессии под блокировкой; change возвращает, изменил ли
// он запись. Испорченная запись (оборванная запись) считается пустой и
// перезаписывается целой, ошибка — ErrRecordDamaged рядом с результатом
func (s Store) update(sessionID string, change func(*record) bool) (changed bool, err error) {
	path, err := s.path(sessionID)
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return false, fmt.Errorf("create state dir: %w", err)
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return false, fmt.Errorf("open record: %w", err)
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil && err == nil {
			changed, err = false, fmt.Errorf("close record: %w", closeErr)
		}
	}()
	if err := core.LockFile(file, time.Now().Add(lockWait)); err != nil {
		return false, err
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return false, fmt.Errorf("read record: %w", err)
	}
	rec, damaged := parseRecord(data)
	changed = change(&rec)
	if !changed && damaged == nil {
		return false, nil
	}
	out, err := json.Marshal(rec)
	if err != nil {
		return false, fmt.Errorf("encode record: %w", err)
	}
	if err := file.Truncate(0); err != nil {
		return false, fmt.Errorf("truncate record: %w", err)
	}
	if _, err := file.WriteAt(out, 0); err != nil {
		return false, fmt.Errorf("write record: %w", err)
	}
	if damaged != nil {
		return changed, fmt.Errorf("%w: %w", ErrRecordDamaged, damaged)
	}
	return changed, nil
}

// parseRecord разбирает запись сессии; пустой файл — пустая запись
func parseRecord(data []byte) (record, error) {
	var rec record
	if len(data) == 0 {
		return rec, nil
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return record{}, fmt.Errorf("parse record: %w", err)
	}
	return rec, nil
}

// Sweep убирает записи сессий, не тронутые recordTTL. Зовётся после
// подсказки; отметка пропуска по присутствию тоже создаёт запись
func (s Store) Sweep(now time.Time) error {
	entries, err := os.ReadDir(s.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("list records: %w", err)
	}
	var errs []error
	for _, entry := range entries {
		path := filepath.Join(s.Dir, entry.Name())
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		info, err := entry.Info()
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if now.Sub(info.ModTime()) < recordTTL {
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// path — файл записи сессии. Имя сессии приходит извне и в путь попадать не
// должно
func (s Store) path(sessionID string) (string, error) {
	// Base пустой строки — «.», пустого имени после неё не бывает
	name := filepath.Base(strings.TrimSpace(sessionID))
	if name == "." || name == string(filepath.Separator) {
		return "", fmt.Errorf("invalid session id %q: %w", sessionID, os.ErrInvalid)
	}
	return filepath.Join(s.Dir, name+".json"), nil
}
