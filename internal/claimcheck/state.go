package claimcheck

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/aiseeq/chooks/internal/atomicfile"
)

// Состояние хука в каталоге состояния chooks (~/.local/state/chooks):
// журнал claim-check.jsonl и каталог claim-check/ с метками учёток codex,
// счётчиком сбоев судьи и отказами писем по сессиям

// accountRetry — срок метки учётки, которой отказано по кредитам: через час
// вызов снова пробует её первой (как accountRetry у worker)
const accountRetry = time.Hour

// accountOutRe — отказ по кредитам учётки; rateRe — темп запросов: он учётку
// не метит (worker codexaccounts.go, противник замысла worker, сценарий 5)
var (
	accountOutRe = regexp.MustCompile(`(?i)out of credits|usage limit|quota`)
	rateRe       = regexp.MustCompile(`(?i)rate limit|\b429\b|too many requests`)
)

// AccountOut — отказ судьи msg значит, что у учётки кончились кредиты
func AccountOut(msg string) bool { return accountOutRe.MatchString(msg) && !rateRe.MatchString(msg) }

// accountMark — учётка без кредитов до Until
type accountMark struct {
	Until time.Time `json:"until"`
	Why   string    `json:"why"`
	At    time.Time `json:"at"`
}

func (h Hook) stateDir() string { return filepath.Join(h.StateDir, "claim-check") }

// accountMarkPath — файл метки учётки: имя каталога и хэш полного пути,
// чтобы ~/.codex и /other/.codex не делили метку
func (h Hook) accountMarkPath(home string) string {
	sum := sha256.Sum256([]byte(home))
	name := strings.TrimPrefix(filepath.Base(home), ".")
	return filepath.Join(h.stateDir(), "accounts", name+"-"+hex.EncodeToString(sum[:4])+".json")
}

// accountOutUntil — учётка помечена без кредитов на now: до какого времени
func (h Hook) accountOutUntil(home string, now time.Time) (time.Time, bool, error) {
	var mark accountMark
	err := readJSON(h.accountMarkPath(home), &mark)
	if errors.Is(err, fs.ErrNotExist) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	return mark.Until, mark.Until.After(now), nil
}

// workerOutUntil — метка worker у учётки на now. Нет файла или он не
// разбирается — метки нет: это пропуск метки, а не отказ учётке
func (h Hook) workerOutUntil(home string, now time.Time) (time.Time, bool) {
	path := h.WorkerMarks[home]
	if path == "" {
		return time.Time{}, false
	}
	var mark accountMark
	if readJSON(path, &mark) != nil {
		return time.Time{}, false
	}
	return mark.Until, mark.Until.After(now)
}

func (h Hook) markAccountOut(home, why string, now time.Time) error {
	mark := accountMark{Until: now.Add(accountRetry), Why: excerpt(why, 300), At: now}
	return writeJSON(h.accountMarkPath(home), mark)
}

// Сбои судьи подряд: на notifyAfter-м подряд — уведомление на рабочий стол,
// не чаще раза в notifyEvery (противник замысла, сценарий 6)
const (
	notifyAfter = 5
	notifyEvery = time.Hour
)

// health — счётчик подряд идущих сбоев судьи
type health struct {
	Failures   int       `json:"failures"`
	Reason     string    `json:"reason,omitempty"`
	Since      time.Time `json:"since,omitzero"`
	NotifiedAt time.Time `json:"notified_at,omitzero"`
}

func (h Hook) healthPath() string { return filepath.Join(h.stateDir(), "health.json") }

// judgeFailed считает сбой судьи; на пятом подряд и не чаще раза в час —
// уведомление. Ошибка уведомления — в лог хука: сбой судьи уже в журнале
func (h Hook) judgeFailed(reason string, now time.Time) {
	err := withFileLock(h.healthPath()+".lock", func() error {
		var state health
		if err := readJSON(h.healthPath(), &state); err != nil && !errors.Is(err, fs.ErrNotExist) {
			h.warn("claim check health unreadable, counter restarted", "error", err)
		}
		if state.Failures == 0 {
			state.Since = now
		}
		state.Failures++
		state.Reason = reason
		notify := state.Failures >= notifyAfter && (state.NotifiedAt.IsZero() || now.Sub(state.NotifiedAt) >= notifyEvery)
		if notify {
			state.NotifiedAt = now
		}
		if err := writeJSON(h.healthPath(), state); err != nil {
			return err
		}
		if notify && h.Notify != nil {
			message := fmt.Sprintf("claim-check: судья недоступен %d раз подряд (%s)", state.Failures, reason)
			if err := h.Notify("claim-check", message); err != nil {
				h.warn("claim check failure alert not delivered", "error", err)
			}
		}
		return nil
	})
	if err != nil {
		h.warn("claim check health not stored", "error", err)
	}
}

// judgeAnswered сбрасывает счётчик: после сбоев — строка «восстановлен» в
// журнале с числом сбоев до неё
func (h Hook) judgeAnswered(entry Entry, now time.Time) {
	err := withFileLock(h.healthPath()+".lock", func() error {
		var state health
		if err := readJSON(h.healthPath(), &state); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if state.Failures == 0 {
			return nil
		}
		recovered := Entry{Time: now.Format(time.RFC3339), Session: entry.Session, SessionID: entry.SessionID,
			Event: entry.Event, Outcome: OutcomeRecovered, Note: "восстановлен", AfterFailures: state.Failures,
			Reason: state.Reason}
		if !state.Since.IsZero() {
			recovered.FailingSince = state.Since.Format(time.RFC3339)
		}
		h.write(recovered)
		state.Failures, state.Reason, state.Since = 0, "", time.Time{}
		return writeJSON(h.healthPath(), state)
	})
	if err != nil {
		h.warn("claim check health not reset", "error", err)
	}
}

// sendState — отказы писем claim-check в ходе сессии: ключи отказанных
// текстов (их повтор проходит) и число отказов (не больше sendDenialLimit)
type sendState struct {
	Turn    string   `json:"turn"`
	Denials int      `json:"denials"`
	Keys    []string `json:"keys"`
}

// sendDenialLimit — сколько отказов письмам claim-check даёт за ход сессии:
// дальше письмо проходит с записью retry_limit (противник замысла, сценарий 2)
const sendDenialLimit = 2

func (h Hook) sendStatePath(session string) (string, error) {
	if session == "" || strings.ContainsAny(session, `/\`) || session == "." || session == ".." {
		return "", fmt.Errorf("bad session id %q", session)
	}
	return filepath.Join(h.stateDir(), "send", session+".json"), nil
}

// readSendState — отказы хода turn: состояние прошлого хода не считается
func (h Hook) readSendState(path, turn string) (sendState, error) {
	var state sendState
	err := readJSON(path, &state)
	if errors.Is(err, fs.ErrNotExist) || (err == nil && state.Turn != turn) {
		return sendState{Turn: turn}, nil
	}
	return state, err
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read state: %w", err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	return atomicfile.Write(path, data, 0o600)
}
