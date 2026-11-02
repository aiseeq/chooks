package understandcheck

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aiseeq/akit/transcript"

	"github.com/aiseeq/chooks/internal/questioncheck"
	"github.com/aiseeq/chooks/internal/tailscan"
)

// Until — конец пробы: после него хук ничего не делает. «Да» пользователя
// 02.11 15:15 CET в сессии лида (tl 2a879c2d:1734: «"Нужно твоё «да»" -
// ставь») на «Включаю на неделю пробой?» (tl 2a879c2d:1672). Итог — по
// журналу, тикет understand-check-probe
//
//nolint:magic-number // дата конца пробы — сама настройка, имя ей Until
var Until = time.Date(2026, time.November, 9, 15, 30, 0, 0, time.Local)

// Исходы в журнале
const (
	VerdictOK       = "ok"
	VerdictFix      = "fix"
	VerdictError    = "error"
	VerdictTimeout  = "timeout"
	VerdictOverride = "override"
)

// Asker — судья: модель, которой отдают промпт проверки и случай
type Asker interface {
	Model() string
	Ask(ctx context.Context, input string) (string, questioncheck.Usage, error)
}

// Call — вызов инструмента из входа PreToolUse
type Call struct {
	SessionID      string
	TranscriptPath string
	AgentID        string
	Tool           string
	Input          json.RawMessage
	// Name — имя сессии Claude Code (lead, work-admin), если известно
	Name string
}

// Hook — проверка понимания перед действием
type Hook struct {
	Judge       Asker
	JournalPath string
	// DeniedDir — ключи отклонённых вызовов по сессиям: повтор того же
	// вызова проходит без судьи
	DeniedDir string
	Budget    time.Duration
	Now       func() time.Time
	Warn      func(msg string, args ...any)
}

// Outcome — что хук отдаёт Claude Code
type Outcome struct {
	// Deny — отклонить вызов, Reason — что агент прочтёт
	Deny   bool
	Reason string
	// Skip — почему судью не звали (для лога хука)
	Skip string
	// Verdict — исход в журнале, если до судьи дошло
	Verdict string
}

// Entry — строка журнала: по нему итог пробы считает поправки, override,
// сбои и задержку
type Entry struct {
	Time       string `json:"time"`
	Session    string `json:"session,omitempty"`
	SessionID  string `json:"session_id"`
	Transcript string `json:"transcript"`
	Tool       string `json:"tool"`
	Kind       Kind   `json:"kind"`
	File       string `json:"file,omitempty"`
	// Key — ключ повтора: по нему итог находит, повторил ли агент вызов
	Key      string `json:"key"`
	Human    string `json:"human,omitempty"`
	HumanAt  string `json:"human_at,omitempty"`
	Prev     string `json:"prev,omitempty"`
	Action   string `json:"action"`
	Anchored bool   `json:"anchored"`
	Verdict  string `json:"verdict"`
	Fixes    []Fix  `json:"fixes,omitempty"`
	Error    string `json:"error,omitempty"`
	Model    string `json:"model,omitempty"`
	// LatencyMS — от начала хука до вердикта, с чтением транскрипта
	LatencyMS int64                `json:"latency_ms"`
	Usage     *questioncheck.Usage `json:"usage,omitempty"`
	// DenyRev — версия текста отказа у записи fix
	DenyRev int `json:"deny_rev,omitempty"`
}

// DefaultPaths — $XDG_STATE_HOME/chooks/understand-check.jsonl и каталог
// ключей рядом с журналом question-check
func DefaultPaths() (journal, denied string, err error) {
	path, err := questioncheck.DefaultJournalPath()
	if err != nil {
		return "", "", err
	}
	dir := filepath.Dir(path)
	return filepath.Join(dir, "understand-check.jsonl"), filepath.Join(dir, "understand-check"), nil
}

// Run проверяет вызов. Всё, кроме поправки судьи, пропускает вызов: сбой
// проверки не повод держать агента
func (h Hook) Run(ctx context.Context, call Call) Outcome {
	started := h.Now()
	if !started.Before(Until) {
		return Outcome{Skip: "проба кончилась " + Until.Format("02.01 15:04")}
	}
	if call.AgentID != "" {
		return Outcome{Skip: "суб-агент"}
	}
	kind, file, action, ok := classify(call.Tool, call.Input)
	if !ok {
		return Outcome{Skip: "действие не проверяется"}
	}
	entry := Entry{
		Time:       started.Format(time.RFC3339),
		Session:    call.Name,
		SessionID:  call.SessionID,
		Transcript: call.TranscriptPath,
		Tool:       call.Tool,
		Kind:       kind,
		File:       file,
		Action:     clip(action, actionRunes),
	}
	if call.TranscriptPath == "" {
		return h.fail(entry, started, VerdictError, errors.New("no transcript path in hook input"))
	}
	turn, err := ReadTurn(call.TranscriptPath)
	if err != nil {
		return h.fail(entry, started, VerdictError, err)
	}
	if !turn.InTurn || strings.TrimSpace(turn.Human) == "" {
		return Outcome{Skip: "ход начат не репликой пользователя"}
	}
	entry.Human, entry.Prev, entry.Anchored = clip(turn.Human, humanRunes), clipHead(turn.Prev, prevRunes), turn.Anchored
	if !turn.HumanAt.IsZero() {
		entry.HumanAt = turn.HumanAt.Format(time.RFC3339)
	}
	key, err := callKey(call.Tool, call.Input, turn.Human)
	if err != nil {
		return h.fail(entry, started, VerdictError, err)
	}
	entry.Key = key
	denied, err := h.denied(call.SessionID, key)
	if err != nil {
		return h.fail(entry, started, VerdictError, err)
	}
	if denied {
		entry.Verdict = VerdictOverride
		entry.LatencyMS = h.Now().Sub(started).Milliseconds()
		h.write(entry)
		return Outcome{Verdict: VerdictOverride}
	}
	input := Case{Kind: kind, Human: turn.Human, HumanAt: turn.HumanAt, Prev: turn.Prev, Action: action}.Input()
	judgeCtx, cancel := context.WithTimeout(ctx, h.Budget)
	answer, usage, err := h.Judge.Ask(judgeCtx, input)
	cancel()
	entry.Model = h.Judge.Model()
	entry.Usage = &usage
	if err != nil {
		verdict := VerdictError
		if errors.Is(judgeCtx.Err(), context.DeadlineExceeded) {
			verdict = VerdictTimeout
		}
		return h.fail(entry, started, verdict, err)
	}
	verdict, err := ParseVerdict(answer)
	if err != nil {
		return h.fail(entry, started, VerdictError, err)
	}
	entry.Verdict, entry.Fixes = verdict.Verdict, verdict.Fixes
	entry.LatencyMS = h.Now().Sub(started).Milliseconds()
	if verdict.Verdict == VerdictOK {
		h.write(entry)
		return Outcome{Verdict: VerdictOK}
	}
	if err := h.remember(call.SessionID, key); err != nil {
		// Без ключа повтор тоже попал бы к судье и мог бы снова получить
		// отказ: агент застрял бы. Пропуск — явный failover с журналом
		return h.fail(entry, started, VerdictError, fmt.Errorf("fix not denied, key not stored: %w", err))
	}
	entry.DenyRev = DenyRev
	h.write(entry)
	return Outcome{Deny: true, Reason: DenyReason(verdict), Verdict: VerdictFix}
}

// fail пишет сбой в журнал и пропускает вызов
func (h Hook) fail(entry Entry, started time.Time, verdict string, err error) Outcome {
	entry.Verdict, entry.Error = verdict, err.Error()
	entry.LatencyMS = h.Now().Sub(started).Milliseconds()
	h.Warn("understand check failed, call let through", "verdict", verdict, "error", err)
	h.write(entry)
	return Outcome{Verdict: verdict}
}

func (h Hook) write(entry Entry) {
	if err := questioncheck.AppendJSONL(h.JournalPath, entry); err != nil {
		h.Warn("understand check journal not written", "path", h.JournalPath, "error", err)
	}
}

// deniedPath — файл ключей сессии; id сессии от Claude Code — uuid, но в
// имя файла идёт только его безопасная часть
func (h Hook) deniedPath(session string) (string, error) {
	if session == "" || strings.ContainsAny(session, `/\`) || session == "." || session == ".." {
		return "", fmt.Errorf("bad session id %q", session)
	}
	return filepath.Join(h.DeniedDir, session), nil
}

// denied — вызов с этим ключом уже получал отказ в сессии
func (h Hook) denied(session, key string) (bool, error) {
	path, err := h.deniedPath(session)
	if err != nil {
		return false, err
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("open denied keys: %w", err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if scanner.Text() == key {
			return true, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return false, fmt.Errorf("read denied keys: %w", err)
	}
	return false, nil
}

func (h Hook) remember(session, key string) error {
	path, err := h.deniedPath(session)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(h.DeniedDir, 0o700); err != nil {
		return fmt.Errorf("create denied dir: %w", err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open denied keys: %w", err)
	}
	if _, err := file.WriteString(key + "\n"); err != nil {
		file.Close()
		return fmt.Errorf("store denied key: %w", err)
	}
	return file.Close()
}

// callKey — ключ повтора: инструмент, его ввод в каноническом JSON (ключи
// объекта по порядку) и реплики хода. Реплики в ключе — отказ прошлого хода
// не пропускает тот же вызов после новой реплики (противник Terra 02.11,
// сценарий 3). У письма адресат не в ключе: тот же текст разным мастерам —
// одно решение, и повтор после неверной поправки не ловит её у каждого
// адресата заново (сценарий 4)
func callKey(tool string, input json.RawMessage, human string) (string, error) {
	var value any
	if err := json.Unmarshal(input, &value); err != nil {
		return "", fmt.Errorf("parse tool input: %w", err)
	}
	if fields, ok := value.(map[string]any); ok && tool == "SendMessage" {
		delete(fields, "to")
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode tool input: %w", err)
	}
	hash := sha256.New()
	for _, part := range [][]byte{[]byte(tool), canonical, []byte(human)} {
		hash.Write(part)
		hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil)[:16]), nil
}

// DenyRev — версия текста отказа: итог пробы делит записи по ней, если
// текст поменяется
const DenyRev = 1

// DenyReason — текст, который агент получит вместо вызова
func DenyReason(verdict Verdict) string {
	var b strings.Builder
	b.WriteString("Проверка понимания (проба до 09.11): судья без контекста беседы видит, что действие расходится с репликой пользователя.\n")
	for _, fix := range verdict.Fixes {
		fmt.Fprintf(&b, "- Реплика: «%s»\n  Действие: «%s»\n  Расхождение: %s\n  Как исправить: %s\n",
			fix.Quote, fix.Action, strings.TrimSuffix(fix.Why, "."), strings.TrimSuffix(fix.Fix, "."))
	}
	b.WriteString("Исправь действие и вызови его заново. Если поправка неверна (судья не видит беседы, " +
		"а ты знаешь, почему прочтение верно) — повтори тот же вызов без изменений: повтор пропускается.")
	return b.String()
}

// ReadTurn читает ход из хвоста транскрипта: окно растёт, пока не ясно,
// начат ли ход репликой пользователя и после какого ответа агента она идёт
func ReadTurn(path string) (Turn, error) {
	var turn Turn
	err := tailscan.Scan(path, func(buf []byte) bool {
		turn = scanTurn(buf)
		return turn.Decided
	})
	if err != nil {
		return Turn{}, err
	}
	return turn, nil
}

// scanTurn проходит события окна по порядку тем же Collector, что сухой прогон
func scanTurn(buf []byte) Turn {
	var c Collector
	parser := transcript.NewParser()
	for line := range bytes.SplitSeq(buf, []byte("\n")) {
		// Выводы инструментов большие и ходу не нужны, кроме ответов в окне
		// вопроса; служебные записи отбрасывает сам Parser
		if !transcript.WithoutResults(line) && !bytes.Contains(line, []byte(`"answers":`)) {
			continue
		}
		// Нечитаемая строка — последняя, которую ещё дописывают, — без событий
		if events, err := parser.Parse(line); err == nil {
			for i := range events {
				c.Add(events[i])
			}
		}
	}
	return c.Turn()
}
