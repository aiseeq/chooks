package claimcheck

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
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/aiseeq/chooks/internal/questioncheck"
)

// Хук claim-check: судья по ходу агента ищет утверждения о фактах без
// проверки. На ответе в конце хода (Stop) флаги не дают ходу закончиться, на
// письме другой сессии (PreToolUse SendMessage) — отклоняют письмо. Вариант
// «а» карточки №38 git-admin: «на флаг агент проверяет утверждение или коротко
// отвечает, что оно проверено». Сбой судьи ход и письмо не держит: исход —
// в журнале, сбои подряд — уведомлением

// Исходы в журнале claim-check.jsonl
const (
	OutcomeBlock          = "block"
	OutcomeDeny           = "deny"
	OutcomePass           = "pass"
	OutcomeSkip           = "skip"
	OutcomeError          = "error"
	OutcomeUnknownVerdict = "unknown_verdict"
	OutcomeRetrySame      = "retry_same"
	OutcomeRetryLimit     = "retry_limit"
	OutcomeQueueTimeout   = "queue_timeout"
	OutcomeSecondAnswer   = "second_answer"
	OutcomeRecovered      = "recovered"
)

// Причины сбоя (поле reason у error). Сбоем судьи для счётчика подряд
// считаются все, кроме input и state: те — о транскрипте и состоянии хука
const (
	FailTimeout = "timeout"
	FailNoCodex = "no_codex"
	FailAuth    = "auth"
	FailLimit   = "limit"
	FailParse   = "parse"
	FailJudge   = "judge"
	FailInput   = "input"
	FailState   = "state"
)

// События хука
const (
	EventStop = "stop"
	EventSend = "send"
)

// Asker — судья: account — каталог учётки (CODEX_HOME), пусто — без выбора
type Asker interface {
	Model() string
	Ask(ctx context.Context, account, input string) (string, questioncheck.Usage, error)
}

// Hook — проверка утверждений по ходу
type Hook struct {
	Judge  Asker
	Prompt string
	// Accounts — каталоги учёток codex по порядку: учётка без кредитов
	// метится на час, вызов идёт на следующую. Пусто — один вызов без учётки
	Accounts []string
	// StateDir — каталог состояния chooks: журнал claim-check.jsonl и
	// claim-check/ (метки учёток, счётчик сбоев, отказы писем)
	StateDir string
	// WorkerMarks — файл метки worker по каталогу учётки
	// (~/.local/state/worker/accounts/<имя>.json): учётка с until в будущем
	// пропускается, как по своей метке. Файлы worker хук только читает
	WorkerMarks map[string]string
	// SlotDir — места судей на машине ($XDG_RUNTIME_DIR/chooks/claimcheck)
	SlotDir string
	// Slots — сколько судей одновременно; 0 — DefaultSlots
	Slots int
	// SlotPoll — шаг ожидания места; 0 — 100 мс
	SlotPoll time.Duration
	// Budget — срок судьи вместе с ожиданием места
	Budget time.Duration
	// Build overrides input construction when provided.
	Build func(context.Context, string, int, Options) (*Input, error)
	Now   func() time.Time
	Warn  func(msg string, args ...any)
	// Notify — уведомление на рабочий стол о сбоях подряд; nil — без него
	Notify func(title, message string) error
}

// Entry — строка журнала
type Entry struct {
	Prompt     string  `json:"prompt,omitempty"`
	Final      *string `json:"final,omitempty"`
	Sourced    *bool   `json:"sourced,omitempty"`
	Marked     *bool   `json:"marked,omitempty"`
	Evidence   *string `json:"evidence,omitempty"`
	Time       string  `json:"time"`
	Session    string  `json:"session,omitempty"`
	SessionID  string  `json:"session_id"`
	Transcript string  `json:"transcript,omitempty"`
	Event      string  `json:"event"`
	Outcome    string  `json:"outcome"`
	Reason     string  `json:"reason,omitempty"`
	Error      string  `json:"error,omitempty"`
	// LatencyMS — от начала хука до исхода; SlotWaitMS — из них ожидание места
	LatencyMS  int64 `json:"latency_ms"`
	SlotWaitMS int64 `json:"slot_wait_ms"`
	Claims     int   `json:"claims"`
	Flags      int   `json:"flags"`
	Unknown    int   `json:"unknown,omitempty"`
	// Flagged — утверждения с флагом: по ним меряют реакцию агента
	Flagged       []Claim        `json:"flagged,omitempty"`
	UnknownClaims []UnknownClaim `json:"unknown_claims,omitempty"`
	// Key — ключ текста письма: повтор того же текста проходит
	Key     string               `json:"key,omitempty"`
	Account string               `json:"account,omitempty"`
	Model   string               `json:"model,omitempty"`
	Usage   *questioncheck.Usage `json:"usage,omitempty"`
	// Answer — начало второго ответа после блока (second_answer): его
	// ошибки меряют отдельно, судья его не видит (противник, сценарий 1)
	Answer        string `json:"answer,omitempty"`
	Note          string `json:"note,omitempty"`
	AfterFailures int    `json:"after_failures,omitempty"`
	FailingSince  string `json:"failing_since,omitempty"`
}

// Outcome — что хук отдаёт Claude Code
type Outcome struct {
	// Block — не дать ходу закончиться (Stop) или отклонить письмо (send)
	Block  bool
	Reason string
	// Result — исход в журнале
	Result string
}

// StopEvent — вход Stop
type StopEvent struct {
	SessionID      string
	Name           string
	TranscriptPath string
	StopHookActive bool
	LastMessage    string
}

// SendEvent — письмо из входа PreToolUse SendMessage
type SendEvent struct {
	SessionID      string
	Name           string
	TranscriptPath string
	ToolUseID      string
	Text           string
}

// JournalPath — журнал хука в каталоге состояния
func JournalPath(stateDir string) string { return filepath.Join(stateDir, "claim-check.jsonl") }

// answerExcerpt — сколько знаков второго ответа идёт в журнал, errorExcerpt —
// текста ошибки
const (
	answerExcerpt = 2000
	errorExcerpt  = 600
)

// transcriptWait — сколько ждать, пока транскрипт догонит вход хука:
// последний ответ или вызов письма ещё могут быть не дописаны
const transcriptWait = 15 * time.Second

// Stop судит ответ в конце хода. Второй ответ после блока (stop_hook_active)
// судье не идёт: один круг на ход, факт второго ответа — в журнал
func (h Hook) Stop(ctx context.Context, event StopEvent) Outcome {
	started := h.now()
	entry := Entry{Time: started.Format(time.RFC3339), Session: event.Name, SessionID: event.SessionID,
		Transcript: event.TranscriptPath, Event: EventStop}
	if h.Prompt == "v6" {
		entry.Prompt = "v6"
	}
	if event.StopHookActive {
		entry.Outcome, entry.Answer = OutcomeSecondAnswer, questioncheck.Excerpt(event.LastMessage, answerExcerpt)
		h.write(entry)
		return Outcome{Result: OutcomeSecondAnswer}
	}
	if strings.TrimSpace(event.LastMessage) == "" {
		return Outcome{Result: OutcomeSkip}
	}
	ctx, cancel := context.WithTimeout(ctx, h.Budget)
	defer cancel()
	input, err := h.stopInput(ctx, event)
	if err != nil {
		return h.fail(entry, started, FailInput, err)
	}
	if h.Prompt == "v6" {
		input.Answer = mask(event.LastMessage)
		input.CallsOnly = true
	}
	verdict, err := h.verdict(ctx, &entry, input, started)
	if err != nil {
		return Outcome{Result: entry.Outcome}
	}
	if h.Prompt == "v6" {
		if strings.TrimSpace(*entry.Final) == "" || *entry.Sourced || *entry.Marked {
			entry.Outcome = OutcomePass
			h.finish(entry, started)
			return Outcome{Result: OutcomePass}
		}
		entry.Outcome = OutcomeBlock
		h.finish(entry, started)
		return Outcome{Block: true, Reason: fmt.Sprintf("Итоговое утверждение: «%s». Чем проверить: %s. Проверь утверждение или пометь, что оно не проверено.", *entry.Final, *entry.Evidence), Result: OutcomeBlock}
	}
	flagged := verdict.FlaggedClaims()
	if len(flagged) == 0 {
		entry.Outcome = OutcomePass
		h.finish(entry, started)
		return Outcome{Result: OutcomePass}
	}
	entry.Outcome = OutcomeBlock
	h.finish(entry, started)
	return Outcome{Block: true, Reason: StopReason(flagged), Result: OutcomeBlock}
}

// Send судит письмо другой сессии. Тот же текст после отказа проходит
// (retry_same); изменённый судится заново, но отказов за ход сессии не больше
// sendDenialLimit — дальше пропуск (retry_limit)
func (h Hook) Send(ctx context.Context, event SendEvent) Outcome {
	if h.Prompt == "v6" {
		return Outcome{Result: OutcomeSkip}
	}
	started := h.now()
	entry := Entry{Time: started.Format(time.RFC3339), Session: event.Name, SessionID: event.SessionID,
		Transcript: event.TranscriptPath, Event: EventSend, Key: textKey(event.Text)}
	if strings.TrimSpace(event.Text) == "" {
		return Outcome{Result: OutcomeSkip}
	}
	ctx, cancel := context.WithTimeout(ctx, h.Budget)
	defer cancel()
	statePath, err := h.sendStatePath(event.SessionID)
	if err != nil {
		return h.fail(entry, started, FailState, err)
	}
	input, err := h.sendInput(ctx, event)
	if err != nil {
		return h.fail(entry, started, FailInput, err)
	}
	// Одно письмо судится одним судьёй: тот же текст, пришедший параллельно,
	// ждёт здесь записи отказа и уходит retry_same (ревью Terra 5ec62fa, п. 3)
	unlock, err := lockWithin(ctx, filepath.Join(h.SlotDir, "send", event.SessionID+"-"+entry.Key+".lock"), h.SlotPoll)
	if errors.Is(err, errQueueTimeout) {
		entry.Outcome, entry.Error = OutcomeQueueTimeout, "same letter is being judged: "+err.Error()
		h.finish(entry, started)
		return Outcome{Result: OutcomeQueueTimeout}
	}
	if err != nil {
		return h.fail(entry, started, FailState, err)
	}
	defer unlock()
	turn := fmt.Sprintf("%s:%d", event.TranscriptPath, input.TurnLine)
	state, err := h.readSendState(statePath, turn)
	if err != nil {
		return h.fail(entry, started, FailState, err)
	}
	if slices.Contains(state.Keys, entry.Key) {
		entry.Outcome = OutcomeRetrySame
		h.finish(entry, started)
		return Outcome{Result: OutcomeRetrySame}
	}
	if state.Denials >= sendDenialLimit {
		entry.Outcome = OutcomeRetryLimit
		h.finish(entry, started)
		return Outcome{Result: OutcomeRetryLimit}
	}
	verdict, err := h.verdict(ctx, &entry, input, started)
	if err != nil {
		return Outcome{Result: entry.Outcome}
	}
	flagged := verdict.FlaggedClaims()
	if len(flagged) == 0 {
		entry.Outcome = OutcomePass
		h.finish(entry, started)
		return Outcome{Result: OutcomePass}
	}
	// Отказ — только с записанным ключом: иначе повтор того же текста снова
	// попал бы к судье. Предел проверяется под блокировкой: параллельные
	// письма хода не превышают его. Свой ключ здесь не перепроверяется: его
	// пишет только держатель блокировки письма, взятой выше
	outcome := OutcomeDeny
	err = withFileLock(statePath+".lock", func() error {
		state, err := h.readSendState(statePath, turn)
		if err != nil {
			return err
		}
		if state.Denials >= sendDenialLimit {
			outcome = OutcomeRetryLimit
			return nil
		}
		state.Denials++
		state.Keys = append(state.Keys, entry.Key)
		return writeJSON(statePath, state)
	})
	if err != nil {
		return h.fail(entry, started, FailState, fmt.Errorf("deny not stored, letter let through: %w", err))
	}
	entry.Outcome = outcome
	h.finish(entry, started)
	if outcome != OutcomeDeny {
		return Outcome{Result: outcome}
	}
	return Outcome{Block: true, Reason: SendReason(flagged), Result: OutcomeDeny}
}

// verdict зовёт судью на свободном месте. Ошибка — исхода нет: очередь, сбой
// или ошибка разбора; она уже в журнале, исход — в entry.Outcome, и вызывающий
// пропускает ход или письмо
func (h Hook) verdict(ctx context.Context, entry *Entry, input *Input, started time.Time) (Verdict, error) {
	body, err := encodeInput(input)
	if err != nil {
		h.fail(*entry, started, FailInput, err)
		entry.Outcome = OutcomeError
		return Verdict{}, err
	}
	waitStart := h.now()
	release, err := acquireSlot(ctx, h.SlotDir, h.slots(), h.SlotPoll)
	entry.SlotWaitMS = h.now().Sub(waitStart).Milliseconds()
	if errors.Is(err, errQueueTimeout) {
		entry.Outcome, entry.Error = OutcomeQueueTimeout, err.Error()
		h.finish(*entry, started)
		return Verdict{}, err
	}
	if err != nil {
		h.fail(*entry, started, FailState, err)
		entry.Outcome = OutcomeError
		return Verdict{}, err
	}
	answer, usage, account, err := h.askInSlot(ctx, body, release)
	entry.Model, entry.Account = h.Judge.Model(), account
	if usage != (questioncheck.Usage{}) {
		entry.Usage = &usage
	}
	if err != nil {
		reason := failReason(ctx, err)
		h.fail(*entry, started, reason, err)
		h.judgeFailed(reason, h.now())
		entry.Outcome = OutcomeError
		return Verdict{}, err
	}
	if h.Prompt == "v6" {
		v, parseErr := parseV6(answer)
		if parseErr != nil {
			h.fail(*entry, started, FailParse, parseErr)
			h.judgeFailed(FailParse, h.now())
			entry.Outcome = OutcomeError
			return Verdict{}, parseErr
		}
		entry.Final, entry.Sourced, entry.Marked, entry.Evidence = v.Final, v.Sourced, v.Marked, v.Evidence
		h.judgeAnswered(*entry, h.now())
		return Verdict{}, nil
	}
	verdict, unknown, err := ParseClaims(answer)
	if err != nil {
		h.fail(*entry, started, FailParse, err)
		h.judgeFailed(FailParse, h.now())
		entry.Outcome = OutcomeError
		return Verdict{}, err
	}
	h.judgeAnswered(*entry, h.now())
	flagged := verdict.FlaggedClaims()
	entry.Claims, entry.Flags, entry.Flagged = len(verdict.Claims)+len(unknown), len(flagged), flagged
	if len(unknown) > 0 {
		entry.Unknown = len(unknown)
		// Отдельная строка — метрика сбоя: исход хода по верным пунктам
		// пишется своей строкой
		bad := *entry
		bad.Outcome, bad.UnknownClaims, bad.Flagged = OutcomeUnknownVerdict, unknown, nil
		h.finish(bad, started)
	}
	return verdict, nil
}

// askInSlot зовёт судью и освобождает место и при панике: в живом демоне
// восстановленная паника иначе держала бы место до queue_timeout у следующих
// (ревью Terra 5ec62fa, п. 2)
func (h Hook) askInSlot(ctx context.Context, body string, release func()) (string, questioncheck.Usage, string, error) {
	defer release()
	return h.ask(ctx, body)
}

// ask проходит учётки по порядку: отказ по кредитам метит учётку на час и
// отдаёт вызов следующей в том же бюджете
func (h Hook) ask(ctx context.Context, body string) (string, questioncheck.Usage, string, error) {
	if len(h.Accounts) == 0 {
		answer, usage, err := h.Judge.Ask(ctx, "", body)
		return answer, usage, "", err
	}
	var total questioncheck.Usage
	var out []string
	for _, account := range h.Accounts {
		now := h.now()
		until, marked, err := h.accountOutUntil(account, now)
		if err != nil {
			h.warn("claim check account mark unreadable, account tried", "account", account, "error", err)
		}
		if !marked {
			until, marked = h.workerOutUntil(account, now)
		}
		if marked {
			out = append(out, fmt.Sprintf("%s до %s", account, until.Format(time.RFC3339)))
			continue
		}
		answer, usage, err := h.Judge.Ask(ctx, account, body)
		total = total.Add(usage)
		if err != nil && ctx.Err() == nil && AccountOut(err.Error()) {
			if markErr := h.markAccountOut(account, err.Error(), h.now()); markErr != nil {
				h.warn("claim check account mark not stored", "account", account, "error", markErr)
			}
			out = append(out, account+": "+excerpt(err.Error(), 200))
			continue
		}
		return answer, total, account, err
	}
	return "", total, "", fmt.Errorf("%w: %s", errAccountsOut, strings.Join(out, "; "))
}

var errAccountsOut = errors.New("all codex accounts out of credits")

var authRe = regexp.MustCompile(`(?i)\b401\b|unauthori[sz]ed|not logged in|log ?in again|authentication|refresh token|codex login`)

// failReason — класс сбоя судьи для журнала и уведомления
func failReason(ctx context.Context, err error) string {
	msg := err.Error()
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return FailTimeout
	case errors.Is(err, errAccountsOut), AccountOut(msg), rateRe.MatchString(msg):
		return FailLimit
	case errors.Is(err, exec.ErrNotFound):
		return FailNoCodex
	case authRe.MatchString(msg):
		return FailAuth
	}
	return FailJudge
}

// stopInput — вход судьи на последнем ответе. Транскрипт может отставать от
// входа Stop: ждём, пока последний ответ в нём совпадёт с last_assistant_message
func (h Hook) stopInput(ctx context.Context, event StopEvent) (*Input, error) {
	want := normalizeSpace(event.LastMessage)
	return h.waitLine(ctx, event.TranscriptPath, true, func(r *row) bool {
		got := normalizeSpace(messageText(r.Blocks))
		return got != "" && (strings.Contains(want, got) || strings.Contains(got, want))
	})
}

// sendInput — вход судьи на вызове письма: строка с tool_use этого вызова
func (h Hook) sendInput(ctx context.Context, event SendEvent) (*Input, error) {
	return h.waitLine(ctx, event.TranscriptPath, false, func(r *row) bool {
		for i := range r.Blocks {
			b := &r.Blocks[i]
			if b.Type != "tool_use" || !isSend(b.Name) {
				continue
			}
			if event.ToolUseID != "" && b.ID == event.ToolUseID {
				return true
			}
			if event.ToolUseID == "" {
				if message, err := stringField(b.Input, "message"); err == nil && message == event.Text {
					return true
				}
			}
		}
		return false
	})
}

func (h Hook) waitLine(ctx context.Context, path string, latest bool, find func(*row) bool) (*Input, error) {
	if path == "" {
		return nil, errors.New("no transcript path in hook input")
	}
	wait := transcriptWait
	if deadline, ok := ctx.Deadline(); ok {
		if third := time.Until(deadline) / 3; third < wait {
			wait = third
		}
	}
	if wait <= 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, context.DeadlineExceeded
	}
	deadline := time.Now().Add(wait)
	cursor := transcriptCursor{}
	lastLine := 0
	found := 0
	for {
		err := scanTranscriptTail(path, &cursor, func(r *row) {
			if latest && messageText(r.Blocks) != "" {
				found = 0
			}
			if find(r) {
				found = r.Line
			}
		})
		lastLine = cursor.line
		if err != nil {
			return nil, err
		}
		if found > 0 {
			build := h.Build
			if build == nil {
				build = BuildInputContext
			}
			type result struct {
				input *Input
				err   error
			}
			done := make(chan result, 1)
			go func() {
				input, err := build(ctx, path, found, Options{CallsOnly: h.Prompt == "v6"})
				done <- result{input, err}
			}()
			select {
			case got := <-done:
				return got.input, got.err
			case <-ctx.Done():
				return nil, fmt.Errorf("build claim-check input: %w", ctx.Err())
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("transcript %s has no matching event yet (last candidate line %d)", path, lastLine)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("transcript wait: %w", ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

type transcriptCursor struct {
	offset int64
	line   int
}

// scanTranscriptTail consumes only complete JSONL records appended since the
// prior pass. The offset stays at the start of an incomplete trailing record.
func scanTranscriptTail(path string, cursor *transcriptCursor, fn func(*row)) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open transcript: %w", err)
	}
	defer f.Close()
	if _, err := f.Seek(cursor.offset, 0); err != nil {
		return fmt.Errorf("seek transcript: %w", err)
	}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 32*1024*1024)
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat transcript: %w", err)
	}
	var consumed int64
	for scanner.Scan() {
		data := scanner.Bytes()
		lineBytes := int64(len(data)) + 1
		if cursor.offset+consumed+lineBytes > info.Size() {
			break
		}
		consumed += lineBytes
		cursor.line++
		if !bytes.Contains(data, []byte(`"assistant"`)) {
			continue
		}
		var e event
		if json.Unmarshal(data, &e) != nil || e.Type != "assistant" {
			continue
		}
		blocks, err := parseBlocks(e.Message.Content)
		if err == nil {
			fn(&row{Line: cursor.line, E: e, Blocks: blocks})
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read transcript: %w", err)
	}
	cursor.offset += consumed
	return nil
}

// lastAnswerLine — номер и текст последней строки ответа агента с текстом
func lastAnswerLine(path string) (int, string, error) {
	line, text := 0, ""
	err := scanTranscript(path, []byte(`"assistant"`), func(n int, r *row) {
		if t := messageText(r.Blocks); t != "" {
			line, text = n, t
		}
	})
	return line, text, err
}

// sendCallLine — строка ответа агента с вызовом письма: по tool_use_id, без
// него — последний вызов письма с тем же текстом
func sendCallLine(path, toolUseID, text string) (int, error) {
	line := 0
	err := scanTranscript(path, []byte(`"tool_use"`), func(n int, r *row) {
		for i := range r.Blocks {
			b := &r.Blocks[i]
			if b.Type != "tool_use" || !isSend(b.Name) {
				continue
			}
			if toolUseID != "" {
				if b.ID == toolUseID {
					line = n
				}
				continue
			}
			if message, err := stringField(b.Input, "message"); err == nil && message == text {
				line = n
			}
		}
	})
	return line, err
}

// scanTranscript разбирает только строки ответа агента с needle: транскрипт
// бывает в сотни МБ
func scanTranscript(path string, needle []byte, fn func(int, *row)) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open transcript: %w", err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 32*1024*1024)
	for n := 1; scanner.Scan(); n++ {
		data := scanner.Bytes()
		if !bytes.Contains(data, needle) || !bytes.Contains(data, []byte(`"assistant"`)) {
			continue
		}
		var e event
		// Недописанная последняя строка не читается: её ждёт waitLine
		if json.Unmarshal(data, &e) != nil || e.Type != "assistant" {
			continue
		}
		// Строку с нечитаемыми блоками пропускаем: строку решения потом
		// разбирает BuildInput и вернёт её ошибку сам
		if blocks, err := parseBlocks(e.Message.Content); err == nil {
			fn(n, &row{Line: n, E: e, Blocks: blocks})
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read transcript: %w", err)
	}
	return nil
}

func encodeInput(input *Input) (string, error) {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(input); err != nil {
		return "", fmt.Errorf("encode judge input: %w", err)
	}
	return b.String(), nil
}

// textKey — ключ текста письма: адресат не в ключе, как у understand-check
func textKey(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:16])
}

func (h Hook) slots() int {
	if h.Slots > 0 {
		return h.Slots
	}
	return DefaultSlots
}

func (h Hook) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func (h Hook) warn(msg string, args ...any) {
	if h.Warn != nil {
		h.Warn(msg, args...)
	}
}

// fail пишет сбой и пропускает ход или письмо
func (h Hook) fail(entry Entry, started time.Time, reason string, err error) Outcome {
	entry.Outcome, entry.Reason, entry.Error = OutcomeError, reason, questioncheck.Excerpt(err.Error(), errorExcerpt)
	h.warn("claim check failed, let through", "event", entry.Event, "reason", reason, "error", err)
	h.finish(entry, started)
	return Outcome{Result: OutcomeError}
}

func (h Hook) finish(entry Entry, started time.Time) {
	entry.LatencyMS = h.now().Sub(started).Milliseconds()
	h.write(entry)
}

func (h Hook) write(entry Entry) {
	if err := questioncheck.AppendJSONL(JournalPath(h.StateDir), entry); err != nil {
		h.warn("claim check journal not written", "error", err)
	}
}

// reasonTail — что агент делает с флагом (вариант «а» карточки №38)
const reasonTail = "Проверь каждое утверждение или коротко ответь, чем оно уже проверено (вызов и его время). Чего проверить нечем — пометь как непроверенное."

// StopReason — текст блока Stop: флаги судьи как есть
func StopReason(flagged []Claim) string {
	var b strings.Builder
	b.WriteString("Проверка утверждений (claim-check): судья по ходу не нашёл проверки этих утверждений ответа.\n")
	writeFlags(&b, flagged)
	b.WriteString(reasonTail)
	return b.String()
}

// SendReason — текст отказа письму
func SendReason(flagged []Claim) string {
	var b strings.Builder
	b.WriteString("Проверка утверждений (claim-check): судья по ходу не нашёл проверки этих утверждений письма.\n")
	writeFlags(&b, flagged)
	b.WriteString(reasonTail)
	b.WriteString(" Исправь письмо и отправь заново; если флаг неверен, тот же текст без изменений пройдёт без повторной проверки.")
	return b.String()
}

func writeFlags(b *strings.Builder, flagged []Claim) {
	for _, c := range flagged {
		fmt.Fprintf(b, "- «%s» — %s: %s\n", c.Text, c.Verdict, c.Basis)
		if c.CouldCheck != "" {
			fmt.Fprintf(b, "  Чем проверить: %s\n", c.CouldCheck)
		}
	}
}

// v6 uses pointers to reject missing or null fields instead of guessing.
type finalVerdict struct {
	Final    *string `json:"final"`
	Sourced  *bool   `json:"sourced"`
	Marked   *bool   `json:"marked"`
	Evidence *string `json:"evidence"`
}

func parseV6(answer string) (finalVerdict, error) {
	for i := strings.Index(answer, "{"); i >= 0; {
		var v finalVerdict
		if err := json.NewDecoder(strings.NewReader(answer[i:])).Decode(&v); err == nil && v.Final != nil && v.Sourced != nil && v.Marked != nil && v.Evidence != nil {
			return v, nil
		}
		next := strings.Index(answer[i+1:], "{")
		if next < 0 {
			break
		}
		i += next + 1
	}
	return finalVerdict{}, fmt.Errorf("invalid v6 judge answer")
}
