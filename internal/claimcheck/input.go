// Package claimcheck builds the context shown to the claim-check judge from
// the Claude Code transcript as it existed at a particular event.
package claimcheck

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/aiseeq/akit/secretmask"
)

type Input struct {
	Event         string   `json:"event"`
	Answer        string   `json:"answer"`
	UserPrompt    *Prompt  `json:"userPrompt"`
	TurnTools     []Tool   `json:"turnTools"`
	Context       Context  `json:"context"`
	Attachments   []string `json:"attachments"`
	AnswerTime    string   `json:"answerTime"`
	LastEventTime string   `json:"lastEventTime"`
	// TurnLine — строка транскрипта, с которой начат ход: ключ хода у хука,
	// судье не идёт
	TurnLine  int  `json:"-"`
	CallsOnly bool `json:"-"`
}
type Prompt struct {
	Type string `json:"type"`
	Text string `json:"text"`
}
type Tool struct {
	Time         string `json:"time"`
	Name         string `json:"name"`
	Input        string `json:"input"`
	OutputLength int    `json:"outputLength"`
	Output       string `json:"output"`
	ExitCode     *int   `json:"exitCode,omitempty"`
	IsError      *bool  `json:"is_error,omitempty"`
	Skipped      int    `json:"skipped,omitempty"`
	callsOnly    bool
	relevant     bool
	// fullInput and fullOutput are the masked, uncropped texts: every refit crops
	// from them, so the crop marker counts the real length.
	fullInput      string
	fullOutput     string
	answerWords    []string
	evidenceScore  int
	evidenceScored bool
	cropWords      []string
	outputLines    []string
	outputWords    [][]string
	outputLengths  []int
	outputRunes    int
	inputLines     []string
	inputWords     [][]string
	inputLengths   []int
	inputRunes     int
	// inputRetainedRunes counts the runes of fullInput that Input keeps; it is
	// set wherever Input is cut, once inputExcerptMeasured.
	inputRetainedRunes   int
	inputExcerptMeasured bool
	matcher              *wordMatcher
	positions            map[int]map[string]int
	inputPositions       map[int]map[string]int
}

func (t Tool) MarshalJSON() ([]byte, error) {
	if t.Skipped > 0 {
		return json.Marshal(struct {
			Name    string `json:"name"`
			Skipped int    `json:"skipped"`
		}{Name: t.Name, Skipped: t.Skipped})
	}
	if t.callsOnly {
		return json.Marshal(struct {
			Time  string `json:"time"`
			Name  string `json:"name"`
			Input string `json:"input"`
		}{t.Time, t.Name, t.Input})
	}
	type toolAlias Tool
	return json.Marshal(toolAlias(t))
}

func (in Input) MarshalJSON() ([]byte, error) {
	if in.CallsOnly {
		return json.Marshal(struct {
			UserPrompt *Prompt `json:"userPrompt"`
			Answer     string  `json:"answer"`
			TurnTools  []Tool  `json:"turnTools"`
		}{in.UserPrompt, in.Answer, in.TurnTools})
	}
	type alias Input
	return json.Marshal(alias(in))
}

type Context struct {
	RecentTools    []Tool `json:"recentTools"`
	PreviousAnswer string `json:"previousAnswer,omitempty"`
	MatchingTools  []Tool `json:"matchingTools,omitempty"`
}
type Options struct {
	ContextLimit int
	Unbounded    bool
	CallsOnly    bool
}

const (
	transcriptContextCheckInterval = 255
	DefaultContextLimit            = 21000
	turnToolsCharLimit             = 20000
	shortAnswerTurnToolsLimit      = 11500
	longAnswerContextLimit         = 10000
	largeAnswerWordThreshold       = 250
	shortAnswerInputCharLimit      = 24300
	toolInputCharLimit             = 300
	callsOnlyBaselineCharLimit     = 150
	callsOnlyMinimumCharLimit      = 120
	callsOnlyMediumCharLimit       = 700
	callsOnlyInputCharLimit        = 1000
	toolOutputCharLimit            = 1600
	lastTurnToolsKept              = 5
	skippedToolName                = "[пропущено]"
	contextOutputCharLimit         = 4000
	contextRecentOutputLimit       = 300
	contextPreviousAnswerLimit     = 1200
	maxContextToolCalls            = 2
)

type event struct {
	Type             string `json:"type"`
	Subtype          string `json:"subtype"`
	IsCompactSummary bool   `json:"isCompactSummary"`
	IsMeta           bool   `json:"isMeta"`
	Timestamp        string `json:"timestamp"`
	Message          struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	Origin struct {
		Kind string `json:"kind"`
	} `json:"origin"`
	Attachment struct {
		Type        string `json:"type"`
		CommandMode string `json:"commandMode"`
		Origin      struct {
			Kind string `json:"kind"`
		} `json:"origin"`
		Prompt json.RawMessage `json:"prompt"`
	} `json:"attachment"`
	ToolUseResult json.RawMessage `json:"toolUseResult"`
	IsError       bool            `json:"is_error"`
}
type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	ToolUseID string          `json:"tool_use_id"`
	Name      string          `json:"name"`
	Path      string          `json:"path"`
	FilePath  string          `json:"file_path"`
	Filename  string          `json:"filename"`
	Input     json.RawMessage `json:"input"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}
type row struct {
	Line   int
	E      event
	Blocks []block
}

// BuildInput builds the judge input at the physical, one-based JSONL line.
func BuildInput(path string, line int, options Options) (*Input, error) {
	return BuildInputContext(context.Background(), path, line, options)
}

// BuildInputContext builds the judge input and stops when ctx is canceled.
func BuildInputContext(ctx context.Context, path string, line int, options Options) (*Input, error) {
	if line < 1 {
		return nil, fmt.Errorf("line must be positive")
	}
	rows, err := readRowsContext(ctx, path, line)
	if err != nil {
		return nil, err
	}
	return buildInputFromRows(ctx, rows, line, options)
}

func buildInputFromRows(ctx context.Context, rows []row, line int, options Options) (*Input, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	start, promptStart, err := turnStart(rows, line)
	if err != nil {
		return nil, err
	}
	answerStart, err := answerTurnStart(rows, line)
	if err != nil {
		return nil, err
	}
	kind, answer, target, err := decision(rows, line, answerStart)
	if err != nil {
		return nil, err
	}
	if options.CallsOnly && kind == "stop" {
		answer = messageText(target.Blocks)
	}
	input := newInput(kind, answer, target.E.Timestamp)
	input.CallsOnly = options.CallsOnly
	if answerStart < len(rows) {
		input.TurnLine = rows[answerStart].Line
	}
	words := answerWords(answer)
	answerWordCount := countAnswerWords(answer)
	matcher := newWordMatcher(words)
	results := indexToolResults(rows)
	input.AnswerTime = prefixTime(rows, line, input.AnswerTime)
	turnLimit := turnToolsCharLimit
	limit := options.ContextLimit
	if limit <= 0 {
		limit = DefaultContextLimit
	}
	if answerWordCount <= largeAnswerWordThreshold && !options.CallsOnly {
		turnLimit = shortAnswerTurnToolsLimit
	} else {
		limit = min(limit, longAnswerContextLimit)
	}
	if err := appendTurn(ctx, input, rows, results, start, line, answer, words, matcher, options.Unbounded, turnLimit); err != nil {
		return nil, err
	}
	if err := attachInputPrompt(input, rows, promptStart); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if options.CallsOnly {
		return input, nil
	}
	if err := appendInputContext(ctx, input, rows, results, start, answer, words, matcher, limit, options, answerWordCount); err != nil {
		return nil, err
	}
	return input, nil
}

func attachInputPrompt(input *Input, rows []row, promptStart int) error {
	if promptStart < 0 {
		return nil
	}
	promptRow := &rows[promptStart]
	prompt, _, err := userPrompt(promptRow)
	if err != nil {
		return fmt.Errorf("read user prompt at line %d: %w", promptRow.Line, err)
	}
	input.UserPrompt = prompt
	input.Attachments, err = attachments(promptRow)
	return err
}

func appendInputContext(ctx context.Context, input *Input, rows []row, results map[string]toolResultRef, start int, answer string, words []string, matcher *wordMatcher, limit int, options Options, answerWordCount int) error {
	if err := appendContext(ctx, input, rows, results, start, answer, words, matcher, limit, options.Unbounded); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if options.Unbounded || answerWordCount > largeAnswerWordThreshold {
		return nil
	}
	return fitShortAnswerContext(ctx, input, rows, results, start, answer, words, matcher, limit)
}

func fitShortAnswerContext(ctx context.Context, input *Input, rows []row, results map[string]toolResultRef, start int, answer string, words []string, matcher *wordMatcher, limit int) error {
	for {
		size, err := inputJSONSize(input)
		if err != nil {
			return err
		}
		if size <= shortAnswerInputCharLimit {
			return nil
		}
		reduced := max(limit-(size-shortAnswerInputCharLimit)-8, 0)
		if reduced >= limit {
			reduced = limit - 1
		}
		limit = reduced
		input.Context = Context{}
		if err := appendContext(ctx, input, rows, results, start, answer, words, matcher, limit, false); err != nil {
			return err
		}
		if limit == 0 {
			return fmt.Errorf("input size exceeds %d characters", shortAnswerInputCharLimit)
		}
	}
}

func inputJSONSize(input *Input) (int, error) {
	encoded, err := json.Marshal(input)
	if err != nil {
		return 0, fmt.Errorf("encode input size: %w", err)
	}
	return utf8.RuneCount(encoded), nil
}

func readRowsContext(ctx context.Context, path string, line int) ([]row, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open transcript: %w", err)
	}
	defer f.Close()
	var rows []row
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 32*1024*1024)
	for n := 1; scanner.Scan(); n++ {
		if n&transcriptContextCheckInterval == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if n > line {
			break
		}
		var e event
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			return nil, fmt.Errorf("decode transcript line %d: %w", n, err)
		}
		bs, err := parseBlocks(e.Message.Content)
		if err != nil {
			return nil, fmt.Errorf("parse transcript line %d content: %w", n, err)
		}
		rows = append(rows, row{Line: n, E: e, Blocks: bs})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read transcript: %w", err)
	}
	return rows, nil
}

func parseBlocks(content json.RawMessage) ([]block, error) {
	if len(content) == 0 || content[0] != '[' {
		return nil, nil
	}
	var blocks []block
	if err := json.Unmarshal(content, &blocks); err != nil {
		return nil, fmt.Errorf("decode content blocks: %w", err)
	}
	return blocks, nil
}

func decision(rows []row, line, start int) (string, string, *row, error) {
	var target *row
	for i := range rows {
		if rows[i].Line == line {
			target = &rows[i]
			break
		}
	}
	if target == nil {
		return "", "", nil, fmt.Errorf("no event at line %d", line)
	}
	answer, kind := "", "stop"
	var err error
	if target.E.Type != "assistant" {
		return "", "", nil, fmt.Errorf("line %d is not an assistant decision", line)
	}
	for i := range target.Blocks {
		b := &target.Blocks[i]
		if b.Type == "tool_use" && isSend(b.Name) {
			kind = "send"
			answer, err = stringField(b.Input, "message")
			if err != nil {
				return "", "", nil, err
			}
			if answer == "" {
				answer, err = stringField(b.Input, "text")
				if err != nil {
					return "", "", nil, err
				}
			}
			break
		}
	}
	if kind == "stop" {
		answer = stopAnswer(rows, line, start)
	}
	if answer == "" {
		return "", "", nil, fmt.Errorf("line %d has no answer text or send call", line)
	}
	return kind, answer, target, nil
}

func stopAnswer(rows []row, line, start int) string {
	var answer strings.Builder
	for i := start; i < len(rows); i++ {
		r := &rows[i]
		if r.Line > line {
			break
		}
		if r.E.Type == "assistant" {
			answer.WriteString(messageText(r.Blocks))
		}
	}
	return answer.String()
}

func messageText(blocks []block) string {
	var text strings.Builder
	for i := range blocks {
		b := &blocks[i]
		if b.Type == "text" {
			text.WriteString(b.Text)
		}
	}
	return text.String()
}

func isSend(name string) bool { return name == "SendMessage" || name == "SendToProject" }

// The prompt is the latest human or peer user message; a peer attachment can
// start a response only after a turn ends. Notifications never replace it.
// Consecutive queued human messages in one turn share the first one's tools.
// Compaction limits tools but does not replace the prompt. Answer and Send
// retry boundaries use the original per-turn rule independently.
func turnStart(rows []row, line int) (int, int, error) {
	state := turnStartState{prompt: -1, queued: -1, compact: -1, lastEnd: -1, lastHuman: -1}
	for i := range rows {
		if rows[i].Line >= line {
			break
		}
		if err := state.consider(&rows[i], i); err != nil {
			return 0, 0, err
		}
	}
	if state.compact > state.start {
		state.start = state.compact
	}
	if state.prompt < 0 && state.compact >= 0 {
		state.prompt = state.compact
	}
	return state.start, state.prompt, nil
}

type turnStartState struct {
	start, prompt, queued, compact, lastEnd, lastHuman int
}

func (s *turnStartState) consider(r *row, index int) error {
	if r.E.Type == "system" && r.E.Subtype == "turn_duration" {
		s.lastEnd, s.queued = index, -1
		return nil
	}
	isCompact, err := isCompactSummary(r)
	if err != nil {
		return err
	}
	if isCompact || r.E.Type == "system" && r.E.Subtype == "compact_boundary" {
		s.compact = index
		return nil
	}
	begins, err := beginsTurn(r)
	if err != nil {
		return fmt.Errorf("classify transcript line %d as turn start: %w", r.Line, err)
	}
	if begins || s.isPeerDelivery(r) {
		s.recordPrompt(r, index)
	}
	return nil
}

func (s *turnStartState) isPeerDelivery(r *row) bool {
	return r.E.Type == "attachment" && r.E.Attachment.Type == "queued_command" &&
		r.E.Attachment.Origin.Kind == "peer" && r.E.Attachment.CommandMode != "task-notification" &&
		s.lastEnd >= 0 && s.lastHuman <= s.lastEnd
}

func (s *turnStartState) recordPrompt(r *row, index int) {
	s.prompt = index
	humanAttachment := r.E.Type == "attachment" && r.E.Attachment.Origin.Kind == "human"
	if humanAttachment || r.E.Type == "user" && r.E.Origin.Kind != "peer" {
		s.lastHuman = index
	}
	if !humanAttachment {
		s.queued, s.start = -1, index
		return
	}
	if s.queued < 0 {
		s.queued = index
	}
	s.start = s.queued
}

func answerTurnStart(rows []row, line int) (int, error) {
	lastEnd := -1
	for i := range rows {
		r := &rows[i]
		if r.Line >= line {
			break
		}
		if r.E.Type == "system" && r.E.Subtype == "turn_duration" {
			lastEnd = i
		}
	}
	start := lastEnd + 1
	found := false
	for i := start; i < len(rows) && rows[i].Line < line; i++ {
		begins, err := beginsAnswerTurn(&rows[i])
		if err != nil {
			return 0, fmt.Errorf("classify transcript line %d as turn start: %w", rows[i].Line, err)
		}
		if !begins {
			continue
		}
		if !found || earlierEvent(rows[i], rows[start]) {
			start = i
			found = true
		}
	}
	return start, nil
}

func earlierEvent(a, b row) bool {
	ta, errA := time.Parse(time.RFC3339Nano, a.E.Timestamp)
	tb, errB := time.Parse(time.RFC3339Nano, b.E.Timestamp)
	if errA == nil && errB == nil {
		return ta.Before(tb)
	}
	return a.Line < b.Line
}

func beginsAnswerTurn(r *row) (bool, error) {
	compact, err := isCompactSummary(r)
	if err != nil {
		return false, err
	}
	if compact {
		return true, nil
	}
	if r.E.Type == "system" && r.E.Subtype == "compact_boundary" {
		return true, nil
	}
	if r.E.Type == "attachment" {
		return beginsAttachmentAnswerTurn(r), nil
	}
	if r.E.Type != "user" {
		return false, nil
	}
	if r.E.Origin.Kind == "task-notification" {
		return true, nil
	}
	return beginsUserTurn(r)
}

func beginsAttachmentAnswerTurn(r *row) bool {
	kind := r.E.Attachment.Origin.Kind
	return r.E.Attachment.CommandMode == "task-notification" || (r.E.Attachment.Type == "queued_command" && (kind == "human" || kind == "peer" || kind == "task-notification"))
}

func newInput(kind, answer, answerTime string) *Input {
	return &Input{Event: kind, Answer: mask(answer), AnswerTime: answerTime, TurnTools: []Tool{}, Attachments: []string{}, Context: Context{RecentTools: []Tool{}}}
}

func prefixTime(rows []row, line int, answerTime string) string {
	// Transcript line order is authoritative if adjacent timestamps wobble by milliseconds.
	for i := range rows {
		r := &rows[i]
		if r.Line < line && r.E.Timestamp != "" {
			answerTime = laterTimestamp(answerTime, r.E.Timestamp)
		}
	}
	return answerTime
}

func appendTurn(ctx context.Context, out *Input, rows []row, results map[string]toolResultRef, start, line int, answer string, words []string, matcher *wordMatcher, unbounded bool, limit int) error {
	if start >= 0 && start < len(rows) {
		p, hasPrompt, err := userPrompt(&rows[start])
		if err != nil {
			return fmt.Errorf("read user prompt at line %d: %w", rows[start].Line, err)
		}
		if hasPrompt {
			out.UserPrompt = p
		}
		attachments, err := attachments(&rows[start])
		if err != nil {
			return fmt.Errorf("read attachments at line %d: %w", rows[start].Line, err)
		}
		out.Attachments = append(out.Attachments, attachments...)
	}
	for i := start; i < len(rows) && rows[i].Line <= line; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		r := &rows[i]
		if r.Line < line && r.E.Timestamp != "" {
			out.LastEventTime = r.E.Timestamp
		}
		if r.E.Type == "assistant" {
			if err := appendCallTools(ctx, out, r, rows, results, i, line, words, matcher, unbounded); err != nil {
				return err
			}
		}
	}
	if !unbounded {
		tools, err := boundToolsContext(ctx, out.TurnTools, answer, limit)
		if err != nil {
			return err
		}
		out.TurnTools = tools
	}
	return nil
}

func appendCallTools(ctx context.Context, out *Input, r *row, rows []row, results map[string]toolResultRef, index, line int, words []string, matcher *wordMatcher, unbounded bool) error {
	for j := range r.Blocks {
		if err := ctx.Err(); err != nil {
			return err
		}
		b := &r.Blocks[j]
		if b.Type != "tool_use" {
			continue
		}
		t, err := toolFromCall(ctx, r, b, rows, results, index, line, words, matcher, unbounded, out.CallsOnly)
		if err != nil {
			return err
		}
		if out.CallsOnly {
			t.callsOnly = true
			if t.relevant {
				t.evidenceScore = answerWordOccurrences(t, words)
			}
			t.evidenceScored = true
			t.Output, t.fullOutput = "", ""
			t.outputWords = nil
			t.OutputLength, t.ExitCode, t.IsError = 0, nil, nil
		}
		out.TurnTools = append(out.TurnTools, t)
	}
	return nil
}

func appendContext(ctx context.Context, out *Input, rows []row, results map[string]toolResultRef, start int, answer string, words []string, matcher *wordMatcher, limit int, unbounded bool) error {
	priorCalls := make(map[string]*block)
	type recentCall struct{ rowIndex, blockIndex int }
	recent := make([]recentCall, 0, maxContextToolCalls)
	for i := range start {
		if err := ctx.Err(); err != nil {
			return err
		}
		r := &rows[i]
		if r.E.Type == "assistant" {
			text := messageText(r.Blocks)
			if text != "" {
				out.Context.PreviousAnswer = mask(text)
			}
			for j := range r.Blocks {
				if r.Blocks[j].Type != "tool_use" {
					continue
				}
				priorCalls[r.Blocks[j].ID] = &r.Blocks[j]
				recent = append(recent, recentCall{i, j})
				if len(recent) > maxContextToolCalls {
					recent = recent[1:]
				}
			}
		}
	}
	for _, call := range recent {
		r := &rows[call.rowIndex]
		tool, err := toolFromCall(ctx, r, &r.Blocks[call.blockIndex], rows, results, call.rowIndex, r.Line, words, matcher, unbounded, false)
		if err != nil {
			return err
		}
		if !unbounded {
			tool.Output = excerptWithWordsCached(tool.fullOutput, words, contextRecentOutputLimit, tool.matcher, tool.positions)
		}
		out.Context.RecentTools = append(out.Context.RecentTools, tool)
	}
	if !unbounded {
		out.Context.PreviousAnswer = excerpt(out.Context.PreviousAnswer, contextPreviousAnswerLimit)
	}
	if err := appendMatchingTools(ctx, out, rows, start, priorCalls, answer, limit, unbounded); err != nil {
		return err
	}
	if !unbounded {
		return boundContext(&out.Context, limit)
	}
	return nil
}

func appendMatchingTools(ctx context.Context, out *Input, rows []row, start int, calls map[string]*block, answer string, limit int, unbounded bool) error {
	words := answerWords(answer)
	matcher := newWordMatcher(words)
	var candidates []Tool
	for i := range start {
		if err := ctx.Err(); err != nil {
			return err
		}
		r := &rows[i]
		for j := range r.Blocks {
			b := &r.Blocks[j]
			tool, _, ok, err := matchingTool(ctx, r, b, calls, words, matcher, int(^uint(0)>>1), true)
			if err != nil {
				return err
			}
			if ok {
				candidates = append(candidates, tool)
			}
		}
	}
	if unbounded {
		out.Context.MatchingTools = append(out.Context.MatchingTools, candidates...)
		return nil
	}
	return selectMatchingTools(ctx, out, candidates, words, matcher, limit)
}

func selectMatchingTools(ctx context.Context, out *Input, candidates []Tool, words []string, matcher *wordMatcher, limit int) error {
	fullWords := make([][]string, len(candidates))
	minimumSizes := make([]int, len(candidates))
	for i := range candidates {
		tool := &candidates[i]
		fullWords[i] = wordsVisibleInTool(Tool{Input: tool.fullInput, Output: tool.fullOutput, matcher: matcher}, words)
		minimum, err := toolsJSONSize([]Tool{{Name: tool.Name}})
		if err != nil {
			return err
		}
		minimumSizes[i] = minimum
	}
	selected := make([]bool, len(candidates))
	remaining := make(map[string]bool)
	for _, visible := range fullWords {
		for _, word := range visible {
			remaining[word] = true
		}
	}
	for i := range out.Context.RecentTools {
		for _, word := range wordsVisibleInTool(out.Context.RecentTools[i], words) {
			delete(remaining, word)
		}
	}
	chosen, err := greedyMatchingTools(ctx, out, candidates, fullWords, minimumSizes, selected, remaining, words, limit)
	if err != nil {
		return err
	}
	for i := range candidates {
		if tool, ok := chosen[i]; ok {
			out.Context.MatchingTools = append(out.Context.MatchingTools, tool)
		}
	}
	return nil
}

func greedyMatchingTools(ctx context.Context, out *Input, candidates []Tool, fullWords [][]string, minimumSizes []int, selected []bool, remaining map[string]bool, words []string, limit int) (map[int]Tool, error) {
	chosen := make(map[int]Tool)
	for len(chosen) < len(candidates) {
		currentSize, err := contextJSONSize(out.Context)
		if err != nil {
			return nil, err
		}
		available := limit - currentSize - 32
		unseen := unseenWords(words, remaining)
		best, _, _, bestTool, err := bestMatchingTool(ctx, candidates, fullWords, minimumSizes, selected, remaining, words, unseen, available)
		if err != nil {
			return nil, err
		}
		if best < 0 {
			break
		}
		selected[best] = true
		chosen[best] = bestTool
		for _, word := range wordsVisibleInTool(bestTool, words) {
			delete(remaining, word)
		}
	}
	return chosen, nil
}

func bestMatchingTool(ctx context.Context, candidates []Tool, fullWords [][]string, minimumSizes []int, selected []bool, remaining map[string]bool, words, unseen []string, available int) (int, int, int, Tool, error) {
	best, bestScore, bestSize, bestTool := -1, 0, 0, Tool{}
	for i := range candidates {
		if err := ctx.Err(); err != nil {
			return -1, 0, 0, Tool{}, err
		}
		fit, score, size, eligible, err := matchingCandidate(ctx, candidates, fullWords, minimumSizes, selected, remaining, words, unseen, i, available)
		if err != nil {
			return -1, 0, 0, Tool{}, err
		}
		if !eligible {
			continue
		}
		if score*bestSize > bestScore*size || score*bestSize == bestScore*size && score > bestScore {
			best, bestScore, bestSize, bestTool = i, score, size, fit
		}
	}
	return best, bestScore, bestSize, bestTool, nil
}

func matchingCandidate(ctx context.Context, candidates []Tool, fullWords [][]string, minimumSizes []int, selected []bool, remaining map[string]bool, words, unseen []string, i, available int) (Tool, int, int, bool, error) {
	if selected[i] || !hasRemainingWord(fullWords[i], remaining) {
		selected[i] = true
		return Tool{}, 0, 0, false, nil
	}
	if available < minimumSizes[i] {
		selected[i] = true
		return Tool{}, 0, 0, false, nil
	}
	candidate := candidates[i]
	candidate.answerWords = unseen
	if candidate.outputLines == nil {
		if err := indexToolOutput(ctx, &candidate); err != nil {
			return Tool{}, 0, 0, false, err
		}
	}
	if candidate.inputLines == nil {
		if err := indexToolInput(ctx, &candidate); err != nil {
			return Tool{}, 0, 0, false, err
		}
	}
	candidates[i] = candidate
	fit, ok, err := fitContextTool(candidate, available)
	if err != nil {
		return Tool{}, 0, 0, false, err
	}
	if !ok {
		selected[i] = true
		return Tool{}, 0, 0, false, nil
	}
	score := countVisibleWords(wordsVisibleInTool(fit, words), remaining)
	size, err := toolsJSONSize([]Tool{fit})
	return fit, score, size, err == nil, err
}

func hasRemainingWord(words []string, remaining map[string]bool) bool {
	for _, word := range words {
		if remaining[word] {
			return true
		}
	}
	return false
}

func contextJSONSize(c Context) (int, error) {
	encoded, err := json.Marshal(c)
	if err != nil {
		return 0, fmt.Errorf("encode context size: %w", err)
	}
	return utf8.RuneCount(encoded), nil
}

func fitContextTool(tool Tool, budget int) (Tool, bool, error) {
	if budget <= 0 {
		return Tool{}, false, nil
	}
	if len(tool.cropWords) > 0 && sameWords(tool.answerWords, tool.cropWords) {
		size, err := toolsJSONSize([]Tool{tool})
		if err != nil {
			return Tool{}, false, err
		}
		if size <= budget {
			return tool, true, nil
		}
	}
	tool.cropWords = nil
	inputLimit, outputLimit := toolInputCharLimit, contextOutputCharLimit
	for {
		tool.Input = toolInputExcerpt(tool, inputLimit)
		tool.Output = toolOutputExcerpt(tool, outputLimit)
		size, err := toolsJSONSize([]Tool{tool})
		if err != nil {
			return Tool{}, false, err
		}
		if size <= budget {
			return tool, true, nil
		}
		over := size - budget
		switch {
		case outputLimit > 0 && outputLimit >= inputLimit:
			outputLimit = max(outputLimit-over, 0)
		case inputLimit > 0:
			inputLimit = max(inputLimit-over, 0)
		case outputLimit > 0:
			outputLimit = max(outputLimit-over, 0)
		default:
			return Tool{}, false, nil
		}
	}
}

// wholeInputPositions keys the answer word positions of a whole input in its
// positions cache, apart from the positions of its lines.
const wholeInputPositions = -1

func toolInputExcerpt(tool Tool, limit int) string {
	// A calls-only minimum window keeps the answer words of the whole input: the
	// line excerpt spends so small a limit on line markers and loses the words.
	if tool.callsOnly && limit > 0 && limit <= callsOnlyMinimumCharLimit && utf8.RuneCountInString(tool.fullInput) > limit {
		return excerptAroundWords(tool.fullInput, tool.answerWords, limit, tool.matcher, tool.inputPositions, wholeInputPositions)
	}
	if tool.inputLines == nil {
		return excerptWithWordsCached(tool.fullInput, tool.answerWords, limit, tool.matcher, tool.inputPositions)
	}
	return excerptWithWordsIndexed(tool.fullInput, tool.answerWords, limit, tool.inputLines, tool.inputWords, tool.inputLengths, tool.inputRunes, tool.matcher, tool.inputPositions)
}

func toolOutputExcerpt(tool Tool, limit int) string {
	if tool.outputLines == nil {
		return excerptWithWordsCached(tool.fullOutput, tool.answerWords, limit, tool.matcher, tool.positions)
	}
	return excerptWithWordsIndexed(tool.fullOutput, tool.answerWords, limit, tool.outputLines, tool.outputWords, tool.outputLengths, tool.outputRunes, tool.matcher, tool.positions)
}

func matchingTool(ctx context.Context, r *row, b *block, calls map[string]*block, words []string, matcher *wordMatcher, budget int, unbounded bool) (Tool, string, bool, error) {
	if b.Type != "tool_result" || budget <= 0 {
		return Tool{}, "", false, nil
	}
	text, err := plain(b.Content)
	if err != nil {
		return Tool{}, "", false, err
	}
	matched, err := matcher.containsAny(ctx, text)
	if err != nil {
		return Tool{}, "", false, err
	}
	if !matched {
		return Tool{}, "", false, nil
	}
	name, input := "tool", ""
	if call, ok := calls[b.ToolUseID]; ok {
		name = call.Name
		input = mask(string(call.Input))
	}
	outputLimit := min(contextOutputCharLimit, budget)
	if unbounded {
		outputLimit = utf8.RuneCountInString(text)
	}
	maskedOutput := mask(text)
	outputLines, outputWords, outputLengths := []string(nil), [][]string(nil), []int(nil)
	if !unbounded {
		outputLines = splitExcerptLines(maskedOutput)
		outputWords, err = answerLineWords(ctx, outputLines, matcher)
		if err != nil {
			return Tool{}, "", false, err
		}
		outputLengths = make([]int, len(outputLines))
		for i, line := range outputLines {
			outputLengths[i] = utf8.RuneCountInString(line)
		}
	}
	positions := make(map[int]map[string]int)
	inputPositions := make(map[int]map[string]int)
	outputRunes := utf8.RuneCountInString(maskedOutput)
	tool := Tool{Time: r.E.Timestamp, Name: name, Input: input, OutputLength: utf8.RuneCountInString(text), Output: excerptWithWordsIndexed(maskedOutput, words, outputLimit, outputLines, outputWords, outputLengths, outputRunes, matcher, positions), fullInput: input, fullOutput: maskedOutput, answerWords: words, outputLines: outputLines, outputWords: outputWords, outputLengths: outputLengths, outputRunes: outputRunes, matcher: matcher, positions: positions, inputPositions: inputPositions}
	return tool, tool.Output, true, nil
}

func indexToolOutput(ctx context.Context, tool *Tool) error {
	tool.outputLines = splitExcerptLines(tool.fullOutput)
	indexed, err := answerLineWords(ctx, tool.outputLines, tool.matcher)
	if err != nil {
		return err
	}
	tool.outputWords = indexed
	tool.outputLengths = make([]int, len(tool.outputLines))
	for i, line := range tool.outputLines {
		tool.outputLengths[i] = utf8.RuneCountInString(line)
	}
	tool.outputRunes = utf8.RuneCountInString(tool.fullOutput)
	return nil
}

func indexToolInput(ctx context.Context, tool *Tool) error {
	tool.inputLines = splitExcerptLines(tool.fullInput)
	indexed, err := answerLineWords(ctx, tool.inputLines, tool.matcher)
	if err != nil {
		return err
	}
	tool.inputWords = indexed
	tool.inputLengths = make([]int, len(tool.inputLines))
	for i, line := range tool.inputLines {
		tool.inputLengths[i] = utf8.RuneCountInString(line)
	}
	tool.inputRunes = utf8.RuneCountInString(tool.fullInput)
	return nil
}

func laterTimestamp(a, b string) string {
	ta, errA := time.Parse(time.RFC3339Nano, a)
	tb, errB := time.Parse(time.RFC3339Nano, b)
	if errA == nil && errB == nil {
		if tb.After(ta) {
			return b
		}
		return a
	}
	if b > a {
		return b
	}
	return a
}

func beginsTurn(r *row) (bool, error) {
	compact, err := isCompactSummary(r)
	if err != nil {
		return false, err
	}
	if compact {
		return true, nil
	}
	if r.E.Type == "system" && r.E.Subtype == "compact_boundary" {
		return true, nil
	}
	if r.E.Type == "attachment" {
		return r.E.Attachment.Type == "queued_command" && r.E.Attachment.Origin.Kind == "human", nil
	}
	if r.E.Type != "user" {
		return false, nil
	}
	return beginsUserTurn(r)
}

func beginsUserTurn(r *row) (bool, error) {
	if r.E.Origin.Kind == "peer" {
		return true, nil
	}
	if r.E.Origin.Kind == "task-notification" {
		return false, nil
	}
	if r.E.IsMeta || (r.E.Origin.Kind != "" && r.E.Origin.Kind != "human") {
		return false, nil
	}
	for i := range r.Blocks {
		b := &r.Blocks[i]
		if b.Type == "tool_result" {
			return false, nil
		}
	}
	text, err := plain(r.E.Message.Content)
	if err != nil {
		return false, err
	}
	text = strings.TrimSpace(systemReminderRE.ReplaceAllString(text, ""))
	if strings.Contains(text, "<command-name>") || strings.Contains(text, "<local-command-caveat>") || strings.Contains(text, "<local-command-stdout>") {
		return false, nil
	}
	attachments, err := attachments(r)
	if err != nil {
		return false, err
	}
	return text != "" || len(attachments) > 0, nil
}

var systemReminderRE = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`)
var peerEnvelopeRE = regexp.MustCompile(`(?s)^\s*(?:Another Claude session sent a message:\s*)?<cross-session-message[^>]*>\s*`)

func userPrompt(r *row) (*Prompt, bool, error) {
	compact, err := isCompactSummary(r)
	if err != nil {
		return nil, false, err
	}
	if compact {
		return &Prompt{Type: "compact", Text: "context compacted"}, true, nil
	}
	if r.E.Type == "system" && r.E.Subtype == "compact_boundary" {
		return &Prompt{Type: "compact", Text: "context compacted"}, true, nil
	}
	if r.E.Type == "user" {
		return userRecordPrompt(r)
	}
	if r.E.Type == "attachment" {
		return attachmentPrompt(r)
	}
	return nil, false, nil
}

func userRecordPrompt(r *row) (*Prompt, bool, error) {
	if r.E.Origin.Kind == "peer" || r.E.Origin.Kind == "task-notification" {
		text, err := plain(r.E.Message.Content)
		if err != nil {
			return nil, false, err
		}
		if r.E.Origin.Kind == "peer" {
			text = cleanPeerText(text)
		}
		return &Prompt{Type: r.E.Origin.Kind, Text: mask(text)}, true, nil
	}
	if r.E.Origin.Kind != "" && r.E.Origin.Kind != "human" || r.E.IsMeta {
		return nil, false, nil
	}
	text, err := humanText(r)
	if err != nil {
		return nil, false, err
	}
	attached, err := hasAttachment(r)
	if err != nil {
		return nil, false, err
	}
	if text != "" || attached {
		return &Prompt{Type: "human", Text: mask(text)}, true, nil
	}
	return nil, false, nil
}

func attachmentPrompt(r *row) (*Prompt, bool, error) {
	if r.E.Attachment.CommandMode == "" && r.E.Attachment.Type == "" {
		return nil, false, nil
	}
	typ := r.E.Attachment.Origin.Kind
	if typ == "" {
		typ = r.E.Attachment.CommandMode
	}
	if typ == "" {
		typ = r.E.Attachment.Type
	}
	if r.E.Attachment.CommandMode == "task-notification" {
		typ = "task-notification"
	}
	text, err := plain(r.E.Attachment.Prompt)
	if err != nil {
		return nil, false, fmt.Errorf("decode attachment prompt: %w", err)
	}
	if typ == "peer" {
		text = cleanPeerText(text)
	}
	return &Prompt{Type: typ, Text: mask(text)}, true, nil
}

func humanText(r *row) (string, error) {
	var s strings.Builder
	for i := range r.Blocks {
		if r.Blocks[i].Type == "text" {
			s.WriteString(r.Blocks[i].Text)
		}
	}
	for i := range r.Blocks {
		if r.Blocks[i].Type == "image" {
			s.WriteString("\n[image]")
		}
	}
	text := s.String()
	if text == "" && len(r.E.Message.Content) > 0 && r.E.Message.Content[0] == '"' {
		if err := json.Unmarshal(r.E.Message.Content, &text); err != nil {
			return "", fmt.Errorf("decode text prompt: %w", err)
		}
	}
	return text, nil
}

func cleanPeerText(text string) string {
	text = peerEnvelopeRE.ReplaceAllString(text, "")
	return strings.TrimSpace(strings.TrimSuffix(text, "</cross-session-message>"))
}

func isCompactSummary(r *row) (bool, error) {
	if r.E.Type != "user" {
		return false, nil
	}
	if r.E.IsCompactSummary {
		return true, nil
	}
	if !r.E.IsMeta {
		return false, nil
	}
	text, err := plain(r.E.Message.Content)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(text) == "context compacted", nil
}

func attachments(r *row) ([]string, error) {
	if r.E.IsMeta {
		return nil, nil
	}
	var blocks []block
	if r.E.Type == "user" && (r.E.Origin.Kind == "" || r.E.Origin.Kind == "human") {
		blocks = r.Blocks
	} else if r.E.Type == "attachment" && r.E.Attachment.Type == "queued_command" && r.E.Attachment.Origin.Kind == "human" {
		parsed, err := parseBlocks(r.E.Attachment.Prompt)
		if err != nil {
			return nil, err
		}
		blocks = parsed
	} else {
		return nil, nil
	}
	var out []string
	for i := range blocks {
		b := &blocks[i]
		switch b.Type {
		case "image":
			out = append(out, "image")
		case "document", "file":
			path, err := blockPath(b)
			if err != nil {
				return nil, err
			}
			if path == "" {
				path = "(path unavailable)"
			}
			out = append(out, "file: "+mask(path))
		}
	}
	return out, nil
}

func blockPath(b *block) (string, error) {
	for _, value := range []string{b.FilePath, b.Path, b.Filename, b.Name} {
		if value != "" {
			return value, nil
		}
	}
	for _, key := range []string{"file_path", "path", "filename", "name"} {
		if s, err := fieldString(b.Content, key); err != nil {
			return "", err
		} else if s != "" {
			return s, nil
		}
		if s, err := fieldString(b.Input, key); err != nil {
			return "", err
		} else if s != "" {
			return s, nil
		}
	}
	return "", nil
}

func fieldString(raw json.RawMessage, key string) (string, error) {
	if len(raw) == 0 || raw[0] != '{' {
		return "", nil
	}
	s, err := stringField(raw, key)
	if err != nil {
		return "", err
	}
	return s, nil
}

func hasAttachment(r *row) (bool, error) {
	attachments, err := attachments(r)
	return len(attachments) > 0, err
}

type toolResultRef struct {
	row   *row
	block *block
}

func indexToolResults(rows []row) map[string]toolResultRef {
	results := make(map[string]toolResultRef)
	for i := range rows {
		for j := range rows[i].Blocks {
			b := &rows[i].Blocks[j]
			if b.Type == "tool_result" {
				if _, exists := results[b.ToolUseID]; !exists {
					results[b.ToolUseID] = toolResultRef{row: &rows[i], block: b}
				}
			}
		}
	}
	return results
}

func toolFromCall(ctx context.Context, r *row, b *block, rows []row, results map[string]toolResultRef, index, end int, words []string, matcher *wordMatcher, unbounded, callsOnly bool) (Tool, error) {
	input := string(b.Input)
	if b.Name == "Bash" {
		command, err := stringField(b.Input, "command")
		if err != nil {
			return Tool{}, fmt.Errorf("decode Bash command: %w", err)
		}
		input = command
	}
	out, length, code, iserr, err := callOutput(r, b, rows, results, index, end)
	if err != nil {
		return Tool{}, err
	}
	return makeCallTool(ctx, r, b, input, out, length, code, iserr, words, matcher, unbounded, callsOnly)
}

func callOutput(r *row, b *block, rows []row, results map[string]toolResultRef, index, end int) (string, int, *int, *bool, error) {
	out := ""
	length := 0
	var code *int
	var iserr *bool
	result, ok := results[b.ID]
	if b.ID != "" && ok && result.row.Line <= end && result.row.Line > r.Line {
		var err error
		out, err = plain(result.block.Content)
		if err != nil {
			return "", 0, nil, nil, fmt.Errorf("decode %s output: %w", b.Name, err)
		}
		length = utf8.RuneCountInString(out)
		e := result.block.IsError || result.row.E.IsError
		iserr = &e
		v, ok, err := nestedInt(result.row.E.ToolUseResult, "exit_code")
		if err != nil {
			return "", 0, nil, nil, fmt.Errorf("decode %s exit code: %w", b.Name, err)
		}
		if ok {
			code = &v
		}
	} else {
	findResult:
		for j := index + 1; j < len(rows) && rows[j].Line <= end; j++ {
			for k := range rows[j].Blocks {
				rb := &rows[j].Blocks[k]
				if rb.Type != "tool_result" || rb.ToolUseID != b.ID && b.ID != "" {
					continue
				}
				var err error
				out, err = plain(rb.Content)
				if err != nil {
					return "", 0, nil, nil, fmt.Errorf("decode %s output: %w", b.Name, err)
				}
				length = utf8.RuneCountInString(out)
				e := rb.IsError || rows[j].E.IsError
				iserr = &e
				v, ok, err := nestedInt(rows[j].E.ToolUseResult, "exit_code")
				if err != nil {
					return "", 0, nil, nil, fmt.Errorf("decode %s exit code: %w", b.Name, err)
				}
				if ok {
					code = &v
				}
				break findResult
			}
		}
	}
	return out, length, code, iserr, nil
}

func makeCallTool(ctx context.Context, r *row, b *block, input, out string, length int, code *int, iserr *bool, words []string, matcher *wordMatcher, unbounded, callsOnly bool) (Tool, error) {
	var err error
	// Mask before cropping: a crop could split a secret so the mask no longer recognises it.
	maskedInput, maskedOutput := mask(input), mask(out)
	inputLimit, outputLimit := toolInputCharLimit, toolOutputCharLimit
	if callsOnly {
		inputLimit = callsOnlyInputCharLimit
	}
	if unbounded {
		inputLimit, outputLimit = len([]rune(maskedInput)), len([]rune(maskedOutput))
	}
	outputLines := splitExcerptLines(maskedOutput)
	outputWords, err := answerLineWords(ctx, outputLines, matcher)
	if err != nil {
		return Tool{}, err
	}
	outputLengths := make([]int, len(outputLines))
	for i, line := range outputLines {
		outputLengths[i] = utf8.RuneCountInString(line)
	}
	outputRunes := utf8.RuneCountInString(maskedOutput)
	inputRunes := utf8.RuneCountInString(maskedInput)
	var inputLines []string
	var inputWords [][]string
	var inputLengths []int
	if !unbounded && inputRunes > inputLimit {
		inputLines = splitExcerptLines(maskedInput)
		inputWords, err = answerLineWords(ctx, inputLines, matcher)
		if err != nil {
			return Tool{}, err
		}
		inputLengths = make([]int, len(inputLines))
		for i, line := range inputLines {
			inputLengths[i] = utf8.RuneCountInString(line)
		}
	}
	positions := make(map[int]map[string]int)
	inputPositions := make(map[int]map[string]int)
	var inputExcerpt string
	var cropWords []string
	if !unbounded {
		cropWords = words
	}
	if inputLines != nil {
		inputExcerpt = excerptWithWordsIndexed(maskedInput, words, inputLimit, inputLines, inputWords, inputLengths, inputRunes, matcher, inputPositions)
	} else {
		inputExcerpt = excerptWithWordsCached(maskedInput, words, inputLimit, matcher, inputPositions)
	}
	return Tool{Time: r.E.Timestamp, Name: b.Name, Input: inputExcerpt, OutputLength: length, Output: excerptWithWordsIndexed(maskedOutput, words, outputLimit, outputLines, outputWords, outputLengths, outputRunes, matcher, positions), ExitCode: code, IsError: iserr, relevant: matches(input, words) || hasLineWord(outputWords), fullInput: maskedInput, fullOutput: maskedOutput, answerWords: words, cropWords: cropWords, inputLines: inputLines, inputWords: inputWords, inputLengths: inputLengths, inputRunes: inputRunes, inputRetainedRunes: min(inputLimit, inputRunes), inputExcerptMeasured: true, outputLines: outputLines, outputWords: outputWords, outputLengths: outputLengths, outputRunes: outputRunes, matcher: matcher, positions: positions, inputPositions: inputPositions}, nil
}
func plain(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", fmt.Errorf("decode text: %w", err)
		}
		return s, nil
	}
	if raw[0] == '[' {
		var bs []block
		if err := json.Unmarshal(raw, &bs); err != nil {
			return "", fmt.Errorf("decode content blocks: %w", err)
		}
		var a []string
		for i := range bs {
			b := &bs[i]
			if b.Text != "" {
				a = append(a, b.Text)
			}
		}
		return strings.Join(a, "\n"), nil
	}
	return "", fmt.Errorf("unexpected tool result content shape")
}
func stringField(raw json.RawMessage, key string) (string, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", fmt.Errorf("decode tool input: %w", err)
	}
	field, exists := m[key]
	if !exists {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(field, &s); err != nil {
		return "", fmt.Errorf("decode tool input field %s: %w", key, err)
	}
	return s, nil
}
func nestedInt(raw json.RawMessage, key string) (int, bool, error) {
	if len(raw) == 0 || raw[0] != '{' {
		return 0, false, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return 0, false, fmt.Errorf("decode result object: %w", err)
	}
	for _, k := range []string{key, "exitCode", "exit_code"} {
		if x, ok := m[k].(float64); ok {
			return int(x), true, nil
		}
	}
	return 0, false, nil
}

// boundToolsContext keeps turnTools within budget characters of JSON. The last calls
// before the answer always stay, sharing half the budget. Matching calls then
// spend the remainder in order of how many not-yet-visible answer words they add.
// In calls-only mode no call with answer words is dropped while another call
// keeps more input than its minimum window.
type toolBoundState struct {
	tools        []Tool
	selected     []bool
	minimumSizes []int
	budget       int
	currentSize  int
}

func (s *toolBoundState) fitAt(i, limit, inputLimit int) (Tool, bool, error) {
	marker, err := skippedMarkerSize(len(s.tools))
	if err != nil {
		return Tool{}, false, err
	}
	available := limit - s.currentSize - 2 - marker
	if available < s.minimumSizes[i] {
		return Tool{}, false, nil
	}
	candidate := s.tools[i]
	if inputLimit == 0 {
		inputLimit = callsOnlyClassCharLimit(candidate)
	}
	return fitToolWithInputLimit(candidate, available, inputLimit)
}

func (s *toolBoundState) commit(i int, fit Tool) error {
	fit.relevant = s.tools[i].relevant
	if !fit.relevant {
		fit.evidenceScore = 0
	}
	s.tools[i], s.selected[i] = fit, true
	var err error
	s.currentSize, err = toolsJSONSize(toolsWithSkipped(s.tools, s.selected))
	return err
}

func (s *toolBoundState) keep(i, limit int) error {
	if s.selected[i] {
		return nil
	}
	fit, ok, err := s.fitAt(i, limit, 0)
	if err != nil || !ok {
		return err
	}
	return s.commit(i, fit)
}

func (s *toolBoundState) grow(i, available, inputLimit int) error {
	fit, ok, err := fitToolWithInputLimit(s.tools[i], available, inputLimit)
	if err != nil || !ok {
		return err
	}
	if larger, ok := keepLargerToolFit(s.tools[i], fit); ok {
		s.tools[i] = larger
		s.currentSize, err = toolsJSONSize(toolsWithSkipped(s.tools, s.selected))
	}
	return err
}

func (s *toolBoundState) cropToMinimum(i int) (bool, error) {
	fit, ok, err := fitToolWithInputLimit(s.tools[i], s.budget, callsOnlyMinimumCharLimit)
	if err != nil || !ok || retainedInputRunes(fit) >= retainedInputRunes(s.tools[i]) {
		return false, err
	}
	s.tools[i] = fit
	s.currentSize, err = toolsJSONSize(toolsWithSkipped(s.tools, s.selected))
	return err == nil, err
}

func (s *toolBoundState) shrinkLongest() (bool, error) {
	longest, longestRunes := -1, 0
	for i := range s.tools {
		retained := retainedInputRunes(s.tools[i])
		if s.selected[i] && retained > callsOnlyMinimumRunes(s.tools[i]) && retained > longestRunes {
			longest, longestRunes = i, retained
		}
	}
	if longest < 0 {
		return false, nil
	}
	return s.cropToMinimum(longest)
}

func (s *toolBoundState) keepMinimum(i int) (bool, error) {
	if s.selected[i] {
		return true, nil
	}
	for {
		fit, ok, err := s.fitAt(i, s.budget, callsOnlyMinimumCharLimit)
		if err != nil {
			return false, err
		}
		if ok && retainedInputRunes(fit) >= callsOnlyMinimumRunes(s.tools[i]) {
			return true, s.commit(i, fit)
		}
		shrunk, err := s.shrinkLongest()
		if err != nil {
			return false, err
		}
		if !shrunk {
			if !ok {
				return false, nil
			}
			return true, s.commit(i, fit)
		}
	}
}

func boundToolsContext(ctx context.Context, tools []Tool, answer string, budget int) ([]Tool, error) {
	if len(tools) == 0 {
		return []Tool{}, nil
	}
	words := answerWords(answer)
	bounded, fullWords, minimumSizes, err := prepareBoundTools(ctx, tools, words)
	if err != nil {
		return nil, err
	}
	selected := make([]bool, len(bounded))
	reserve := budget / 2
	last := max(len(bounded)-lastTurnToolsKept, 0)
	if err := keepLastTools(bounded, selected, last, reserve); err != nil {
		return nil, err
	}
	currentSize, err := toolsJSONSize(toolsWithSkipped(bounded, selected))
	if err != nil {
		return nil, err
	}
	state := &toolBoundState{tools: bounded, selected: selected, minimumSizes: minimumSizes, budget: budget, currentSize: currentSize}
	if err := selectBoundedMatchingTools(ctx, state, words, fullWords, budget); err != nil {
		return nil, err
	}
	state.currentSize, err = toolsJSONSize(toolsWithSkipped(bounded, selected))
	if err != nil {
		return nil, err
	}
	if err := boundPriorityPasses(ctx, state, last, budget); err != nil {
		return nil, err
	}
	for i := last - 1; i >= 0; i-- {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := state.keep(i, budget); err != nil {
			return nil, err
		}
	}
	for i := range bounded {
		if err := state.keep(i, budget); err != nil {
			return nil, err
		}
	}
	return toolsWithSkipped(bounded, selected), nil
}

func prepareBoundTools(ctx context.Context, tools []Tool, words []string) ([]Tool, [][]string, []int, error) {
	bounded := make([]Tool, len(tools))
	copy(bounded, tools)
	for i := range bounded {
		if err := ctx.Err(); err != nil {
			return nil, nil, nil, err
		}
		t := &bounded[i]
		t.relevant = t.relevant || matches(t.Input, words) || matches(t.Output, words)
		if t.callsOnly {
			t.relevant = t.relevant || matches(t.fullInput, words) || matches(t.fullOutput, words)
		}
		if !t.callsOnly || len(t.answerWords) == 0 {
			t.answerWords = words
		}
	}
	if bounded[0].callsOnly {
		for i := range bounded {
			if !bounded[i].evidenceScored {
				if bounded[i].relevant {
					bounded[i].evidenceScore = answerWordOccurrences(bounded[i], words)
				}
				bounded[i].evidenceScored = true
			}
			if !bounded[i].relevant {
				bounded[i].evidenceScore = 0
			}
		}
	}
	fullWords := make([][]string, len(bounded))
	minimumSizes := make([]int, len(bounded))
	for i := range bounded {
		tool := &bounded[i]
		if err := ctx.Err(); err != nil {
			return nil, nil, nil, err
		}
		fullWords[i] = append(fullWords[i], cachedOutputWords(tool.outputWords)...)
		fullWords[i] = append(fullWords[i], lineAnswerWordsWithMatcher(tool.fullInput, words, tool.matcher)...)
		minimum, err := toolsJSONSize([]Tool{{Name: tool.Name}})
		if err != nil {
			return nil, nil, nil, err
		}
		minimumSizes[i] = minimum
	}
	return bounded, fullWords, minimumSizes, nil
}

func selectBoundedMatchingTools(ctx context.Context, state *toolBoundState, words []string, fullWords [][]string, budget int) error {
	remaining := remainingAnswerWords(state, words, fullWords)
	attempted := make([]bool, len(state.tools))
	for !state.tools[0].callsOnly {
		matchedSize, err := toolsJSONSize(toolsWithSkipped(state.tools, state.selected))
		if err != nil {
			return err
		}
		marker, err := skippedMarkerSize(len(state.tools))
		if err != nil {
			return err
		}
		available := budget - matchedSize - 2 - marker
		best, score, fit, err := findBestBoundedTool(ctx, state, words, fullWords, remaining, attempted, available)
		if err != nil {
			return err
		}
		if best < 0 || score == 0 {
			break
		}
		state.tools[best], state.selected[best] = fit, true
		for _, word := range wordsVisibleInTool(fit, words) {
			delete(remaining, word)
		}
	}
	return nil
}

func remainingAnswerWords(state *toolBoundState, words []string, fullWords [][]string) map[string]bool {
	remaining := make(map[string]bool)
	for _, visible := range fullWords {
		for _, word := range visible {
			remaining[word] = true
		}
	}
	for i := range state.tools {
		if state.selected[i] {
			for _, word := range wordsVisibleInTool(state.tools[i], words) {
				delete(remaining, word)
			}
		}
	}
	return remaining
}

func findBestBoundedTool(ctx context.Context, state *toolBoundState, words []string, fullWords [][]string, remaining map[string]bool, attempted []bool, available int) (int, int, Tool, error) {
	best, bestScore, bestSize, bestFit := -1, 0, 0, Tool{}
	unseen := unseenWords(words, remaining)
	for i := range state.tools {
		if err := ctx.Err(); err != nil {
			return -1, 0, Tool{}, err
		}
		if state.selected[i] || attempted[i] || !state.tools[i].relevant {
			continue
		}
		fit, score, size, eligible, err := scoreBoundedCandidate(state, words, fullWords, remaining, unseen, i, available)
		if err != nil {
			return -1, 0, Tool{}, err
		}
		if !eligible {
			attempted[i] = true
			continue
		}
		if score*bestSize > bestScore*size || score*bestSize == bestScore*size && score > bestScore {
			best, bestScore, bestSize, bestFit = i, score, size, fit
		}
	}
	return best, bestScore, bestFit, nil
}

func scoreBoundedCandidate(state *toolBoundState, words []string, fullWords [][]string, remaining map[string]bool, unseen []string, i, available int) (Tool, int, int, bool, error) {
	canAdd := false
	for _, word := range fullWords[i] {
		if remaining[word] {
			canAdd = true
			break
		}
	}
	if !canAdd || available < state.minimumSizes[i] {
		return Tool{}, 0, 0, false, nil
	}
	candidate := state.tools[i]
	candidate.answerWords = unseen
	fit, ok, err := fitTool(candidate, available)
	if err != nil || !ok {
		return Tool{}, 0, 0, false, err
	}
	score := 0
	for _, word := range wordsVisibleInTool(fit, words) {
		if remaining[word] {
			score++
		}
	}
	size, err := toolsJSONSize([]Tool{fit})
	return fit, score, size, err == nil, err
}

func boundPriorityPasses(ctx context.Context, state *toolBoundState, last, budget int) error {
	if !state.tools[0].callsOnly {
		return keepRelevantTools(state, budget)
	}
	order := callsOnlyPriorityOrder(state.tools)
	minimumOnly, err := keepCallsOnlyMinimums(ctx, state, order, last)
	if err != nil {
		return err
	}
	if !minimumOnly {
		if err := growCallsOnlyRelevant(ctx, state, order, budget); err != nil {
			return err
		}
		if err := keepCallsOnlyOther(state, order, budget); err != nil {
			return err
		}
	}
	return keepShortIrrelevantCalls(ctx, state, budget)
}

func keepCallsOnlyMinimums(ctx context.Context, state *toolBoundState, order []int, last int) (bool, error) {
	for i := last; i < len(state.tools); i++ {
		if state.tools[i].relevant {
			if _, err := state.cropToMinimum(i); err != nil {
				return false, err
			}
		}
	}
	minimumOnly := false
	for _, i := range order {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if !state.tools[i].relevant {
			continue
		}
		kept, err := state.keepMinimum(i)
		if err != nil {
			return false, err
		}
		minimumOnly = minimumOnly || !kept
	}
	return minimumOnly, nil
}

func growCallsOnlyRelevant(ctx context.Context, state *toolBoundState, order []int, budget int) error {
	for _, i := range order {
		if err := ctx.Err(); err != nil {
			return err
		}
		tool := state.tools[i]
		if !tool.relevant || !state.selected[i] || retainedInputRunes(tool) >= callsOnlyLimitRunes(tool) {
			continue
		}
		callSize, err := toolsJSONSize([]Tool{tool})
		if err != nil {
			return err
		}
		if err := state.grow(i, budget-state.currentSize+callSize, callsOnlyInputCharLimit); err != nil {
			return err
		}
	}
	return nil
}

func keepCallsOnlyOther(state *toolBoundState, order []int, budget int) error {
	for _, i := range order {
		if !state.tools[i].relevant {
			if err := state.keep(i, budget); err != nil {
				return err
			}
		}
	}
	return shareCallsOnlyBudget(state, order, budget)
}

func shareCallsOnlyBudget(state *toolBoundState, order []int, budget int) error {
	cropped := 0
	for _, i := range order {
		if state.selected[i] && retainedInputRunes(state.tools[i]) < callsOnlyLimitRunes(state.tools[i]) {
			cropped++
		}
	}
	for _, i := range order {
		if !state.selected[i] || retainedInputRunes(state.tools[i]) >= callsOnlyLimitRunes(state.tools[i]) {
			continue
		}
		cropped--
		callSize, err := toolsJSONSize([]Tool{state.tools[i]})
		if err != nil {
			return err
		}
		share := (budget - state.currentSize + callSize) / (cropped + 1)
		if share <= callSize {
			continue
		}
		if err := state.grow(i, share, callsOnlyInputCharLimit); err != nil {
			return err
		}
	}
	return nil
}

func keepShortIrrelevantCalls(ctx context.Context, state *toolBoundState, budget int) error {
	if !state.tools[0].callsOnly {
		return nil
	}
	for _, i := range callsOnlyPriorityOrder(state.tools) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !state.tools[i].relevant && utf8.RuneCountInString(state.tools[i].fullInput) <= toolInputCharLimit {
			if err := state.keep(i, budget); err != nil {
				return err
			}
		}
	}
	return nil
}

func keepRelevantTools(state *toolBoundState, budget int) error {
	for i := range state.tools {
		if state.tools[i].relevant {
			if err := state.keep(i, budget); err != nil {
				return err
			}
		}
	}
	return nil
}

// callsOnlyPriorityOrder puts calls tied to the answer before length classes,
// then prefers more answer-word matches and later calls within each class.
func callsOnlyPriorityOrder(tools []Tool) []int {
	type priority struct {
		index     int
		inputSize int
		score     int
		relevant  bool
	}
	items := make([]priority, len(tools))
	for i := range tools {
		items[i] = priority{
			index:     i,
			inputSize: utf8.RuneCountInString(tools[i].fullInput),
			score:     tools[i].evidenceScore,
			relevant:  tools[i].relevant,
		}
	}
	sort.SliceStable(items, func(a, b int) bool {
		i, j := items[a], items[b]
		if i.relevant != j.relevant {
			return i.relevant
		}
		iGroup, jGroup := callsOnlyPriorityGroup(i.inputSize), callsOnlyPriorityGroup(j.inputSize)
		if iGroup != jGroup {
			return iGroup < jGroup
		}
		if i.score != j.score {
			return i.score > j.score
		}
		return i.index > j.index
	})
	order := make([]int, len(items))
	for i := range items {
		order[i] = items[i].index
	}
	return order
}

func callsOnlyPriorityGroup(inputLength int) int {
	if inputLength <= toolInputCharLimit {
		return 0
	}
	if inputLength <= callsOnlyInputCharLimit {
		return 1
	}
	return 2
}

// callsOnlyClassCharLimit is the input limit a call gets when it is first
// selected: relevant and short calls up to the full limit, longer calls
// without answer words less the longer they are.
func callsOnlyClassCharLimit(tool Tool) int {
	if !tool.callsOnly {
		return toolInputCharLimit
	}
	length := utf8.RuneCountInString(tool.fullInput)
	switch {
	case tool.relevant || length <= toolInputCharLimit:
		return callsOnlyInputCharLimit
	case length > callsOnlyInputCharLimit:
		return callsOnlyBaselineCharLimit
	default:
		return callsOnlyMediumCharLimit
	}
}

// callsOnlyMinimumRunes is how much input the minimum window of a call keeps.
// callsOnlyMinimumCharLimit is the least limit that holds the window of an
// answer word, 20 runes on each side, between two crop markers of an input
// under a million runes; see excerptAroundWords.
func callsOnlyMinimumRunes(tool Tool) int {
	return min(utf8.RuneCountInString(tool.fullInput), callsOnlyMinimumCharLimit)
}

// callsOnlyLimitRunes is how much input a call keeps at its full limit.
func callsOnlyLimitRunes(tool Tool) int {
	return min(utf8.RuneCountInString(tool.fullInput), callsOnlyInputCharLimit)
}

func keepLargerToolFit(current, fit Tool) (Tool, bool) {
	if retainedInputRunes(fit) > retainedInputRunes(current) {
		return fit, true
	}
	return current, false
}

// retainedInputRunes is how many runes of fullInput the excerpt keeps. The
// count is recorded where the excerpt is cut, never parsed from its text: the
// input itself may contain a literal crop marker. A Tool built without a cut
// keeps its Input whole.
func retainedInputRunes(tool Tool) int {
	if tool.inputExcerptMeasured {
		return tool.inputRetainedRunes
	}
	return utf8.RuneCountInString(tool.Input)
}

func answerWordOccurrences(tool Tool, words []string) int {
	wanted := make(map[string]struct{}, len(words))
	for _, word := range words {
		wanted[strings.ToLower(word)] = struct{}{}
	}
	count := 0
	visit := func(text string) {
		visitWordTokens(text, func(token string) {
			if _, ok := wanted[strings.ToLower(token)]; ok {
				count++
			}
		})
	}
	visit(tool.fullInput)
	visit(tool.fullOutput)
	return count
}

func wordsVisibleInTool(tool Tool, words []string) []string {
	input, output := strings.ToLower(tool.Input), strings.ToLower(tool.Output)
	seen := make(map[string]struct{})
	visitWordTokens(input+"\n"+output, func(token string) {
		if i := sort.SearchStrings(words, token); i < len(words) && words[i] == token {
			seen[token] = struct{}{}
		}
	})
	matcher := tool.matcher
	if matcher == nil || !slices.Equal(matcher.words, words) {
		matcher = newWordMatcher(words)
	}
	positions := matcher.wordPositions(input + "\n" + output)
	for _, word := range words {
		if _, found := positions[word]; found {
			seen[word] = struct{}{}
		}
	}
	visible := slices.Sorted(maps.Keys(seen))
	if visible == nil {
		return []string{}
	}
	return visible
}

func unseenWords(words []string, remaining map[string]bool) []string {
	var unseen []string
	for _, word := range words {
		if remaining[word] {
			unseen = append(unseen, word)
		}
	}
	return unseen
}

// keepLastTools selects tools[from:] and crops them to share reserve: the
// smallest take what they need first, the larger split what is left.
func keepLastTools(tools []Tool, selected []bool, from, reserve int) error {
	order := make([]int, 0, len(tools)-from)
	sizes := make(map[int]int, len(tools)-from)
	for i := from; i < len(tools); i++ {
		size, err := toolsJSONSize([]Tool{tools[i]})
		if err != nil {
			return err
		}
		order = append(order, i)
		sizes[i] = size
	}
	sort.SliceStable(order, func(a, b int) bool { return sizes[order[a]] < sizes[order[b]] })
	remaining := reserve - len("[]")
	if from > 0 {
		marker, err := skippedMarkerSize(from)
		if err != nil {
			return err
		}
		remaining -= marker + 1
	}
	for n, i := range order {
		share := remaining/(len(order)-n) - 1
		fit, ok, err := fitTool(tools[i], share)
		if err != nil {
			return err
		}
		if !ok {
			fit = tools[i]
			fit.Input, fit.Output = "", ""
			fit.inputRetainedRunes, fit.inputExcerptMeasured = 0, true
		}
		tools[i], selected[i] = fit, true
		size, err := toolsJSONSize([]Tool{fit})
		if err != nil {
			return err
		}
		remaining -= size + 1
	}
	return nil
}

func skippedMarkerSize(skipped int) (int, error) {
	size, err := toolsJSONSize([]Tool{{Name: skippedToolName, Skipped: skipped}})
	return size - len("[]"), err
}

// fitTool crops the call's input and output from their full texts, cutting the
// longer allowance by the overflow until the call fits budget characters of JSON.
func fitTool(tool Tool, budget int) (Tool, bool, error) {
	return fitToolWithInputLimit(tool, budget, 0)
}

func fitToolWithInputLimit(tool Tool, budget, callsOnlyInputLimit int) (Tool, bool, error) {
	if !tool.callsOnly && len(tool.cropWords) > 0 && sameWords(tool.answerWords, tool.cropWords) {
		size, err := toolsJSONSize([]Tool{tool})
		if err != nil {
			return Tool{}, false, err
		}
		if size <= budget {
			return tool, true, nil
		}
	}
	tool.cropWords = nil
	inputLimit, outputLimit := toolInputCharLimit, toolOutputCharLimit
	if tool.callsOnly {
		inputLimit = callsOnlyInputCharLimit
		if callsOnlyInputLimit > 0 {
			inputLimit = min(inputLimit, callsOnlyInputLimit)
		}
	}
	for {
		tool.Input = toolInputExcerpt(tool, inputLimit)
		tool.inputRetainedRunes = min(inputLimit, utf8.RuneCountInString(tool.fullInput))
		tool.inputExcerptMeasured = true
		tool.Output = toolOutputExcerpt(tool, outputLimit)
		size, err := toolsJSONSize([]Tool{tool})
		if err != nil {
			return Tool{}, false, err
		}
		if size <= budget {
			return tool, true, nil
		}
		over := size - budget
		switch {
		case outputLimit > 0 && outputLimit >= inputLimit:
			outputLimit = max(outputLimit-over, 0)
		case inputLimit > 0:
			inputLimit = max(inputLimit-over, 0)
		default:
			return Tool{}, false, nil
		}
	}
}

func sameWords(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func toolsWithSkipped(tools []Tool, selected []bool) []Tool {
	out := make([]Tool, 0, len(tools))
	for i := 0; i < len(tools); {
		if selected[i] {
			out = append(out, tools[i])
			i++
			continue
		}
		start := i
		for i < len(tools) && !selected[i] {
			i++
		}
		out = append(out, Tool{Name: skippedToolName, Skipped: i - start})
	}
	return out
}

func toolsJSONSize(tools []Tool) (int, error) {
	encoded, err := json.Marshal(tools)
	if err != nil {
		return 0, fmt.Errorf("encode turn tools: %w", err)
	}
	return utf8.RuneCount(encoded), nil
}

func boundContext(c *Context, budget int) error {
	encoded, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("encode context size: %w", err)
	}
	for utf8.RuneCount(encoded) > budget {
		excess := utf8.RuneCount(encoded) - budget
		shorten := func(s string) string {
			r := []rune(s)
			keep := max(len(r)-excess, 0)
			if keep == len(r) && keep > 0 {
				keep--
			}
			return string(r[:keep])
		}
		if c.PreviousAnswer != "" {
			c.PreviousAnswer = shorten(c.PreviousAnswer)
		} else if n := len(c.MatchingTools); n > 0 && c.MatchingTools[n-1].Output != "" {
			c.MatchingTools[n-1].Output = shorten(c.MatchingTools[n-1].Output)
		} else if n := len(c.MatchingTools); n > 0 && c.MatchingTools[n-1].Input != "" {
			c.MatchingTools[n-1].Input = shorten(c.MatchingTools[n-1].Input)
		} else if n := len(c.RecentTools); n > 0 && c.RecentTools[n-1].Input != "" {
			c.RecentTools[n-1].Input = shorten(c.RecentTools[n-1].Input)
		} else if n := len(c.MatchingTools); n > 0 {
			c.MatchingTools = c.MatchingTools[:n-1]
		} else if n := len(c.RecentTools); n > 0 {
			c.RecentTools = c.RecentTools[:n-1]
		} else {
			break
		}
		encoded, err = json.Marshal(c)
		if err != nil {
			return fmt.Errorf("encode context size: %w", err)
		}
	}
	return nil
}
func excerpt(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	a := (n) / 2
	z := n - a
	return string(rs[:a]) + fmt.Sprintf("\n[обрезано: %d знаков из %d]\n", len(rs)-n, len(rs)) + string(rs[len(rs)-z:])
}

// excerptWithWords prioritizes complete lines containing answer words, with one
// neighboring line on either side, then fills the remaining space with edges.
// Omitted runs are explicit so the judge can tell a crop from absent evidence.
func excerptWithWords(s string, words []string, limit int) string {
	return excerptWithWordsCached(s, words, limit, nil, nil)
}

func excerptWithWordsCached(s string, words []string, limit int, matcher *wordMatcher, positions map[int]map[string]int) string {
	return excerptWithWordsIndexed(s, words, limit, nil, nil, nil, -1, matcher, positions)
}

func excerptWithWordsIndexed(s string, words []string, limit int, cachedLines []string, indexed [][]string, cachedLengths []int, totalRunes int, matcher *wordMatcher, positions map[int]map[string]int) string {
	lines := cachedLines
	if lines != nil {
		lines = append([]string(nil), lines...)
	} else {
		lines = splitExcerptLines(s)
	}
	if len(lines) <= 1 || limit <= 0 {
		return excerptShortInput(s, lines, words, limit, totalRunes, matcher, positions)
	}
	if totalRunes < 0 {
		totalRunes = utf8.RuneCountInString(s)
	}
	if totalRunes <= limit {
		return s
	}
	if matcher == nil {
		matcher = newWordMatcher(words)
	}
	lineWords := prepareExcerptLines(lines, indexed, words, limit, matcher, positions)
	lineLengths := excerptLineLengths(lines, cachedLines, cachedLengths)
	selected := selectExcerptEvidence(lines, lineWords, lineLengths, limit)
	state := excerptRenderer{lines: lines, lengths: lineLengths, selected: selected}
	// Boundary context is lower priority than matching evidence.
	for _, i := range []int{0, len(lines) - 1} {
		if !selected[i] {
			selected[i] = true
			if state.length() > limit {
				selected[i] = false
			}
		}
	}
	result := state.render()
	if utf8.RuneCountInString(result) <= limit {
		return result
	}
	// A single exceptionally long evidence line can exceed the budget.
	return excerpt(result, limit)
}

func excerptShortInput(s string, lines, words []string, limit, totalRunes int, matcher *wordMatcher, positions map[int]map[string]int) string {
	if totalRunes < 0 {
		totalRunes = utf8.RuneCountInString(s)
	}
	if len(lines) == 1 && limit > 0 && totalRunes > limit {
		if matcher == nil {
			matcher = newWordMatcher(words)
		}
		linePositions := positions[0]
		if linePositions == nil {
			linePositions = matcher.wordPositions(s)
			if positions != nil {
				positions[0] = linePositions
			}
		}
		if len(linePositions) > 0 {
			return excerptAroundWords(s, words, limit, matcher, positions, 0)
		}
	}
	return excerpt(s, limit)
}

func prepareExcerptLines(lines []string, indexed [][]string, words []string, limit int, matcher *wordMatcher, positions map[int]map[string]int) [][]string {
	lineWords := make([][]string, len(lines))
	for i, line := range lines {
		if i < len(indexed) {
			lineWords[i] = intersectWords(indexed[i], words)
		} else {
			lineWords[i] = lineAnswerWordsWithMatcher(line, words, matcher)
		}
		if utf8.RuneCountInString(line) <= limit || len(lineWords[i]) == 0 {
			continue
		}
		lines[i] = excerptAroundWords(line, words, max(limit-80, 1), matcher, positions, i)
		lineWords[i] = lineAnswerWordsWithMatcher(lines[i], words, matcher)
	}
	return lineWords
}

type excerptRenderer struct {
	lines    []string
	lengths  []int
	selected []bool
}

func (r excerptRenderer) render() string {
	var out []string
	for i := 0; i < len(r.lines); {
		if r.selected[i] {
			out = append(out, r.lines[i])
			i++
			continue
		}
		start := i
		for i < len(r.lines) && !r.selected[i] {
			i++
		}
		out = append(out, fmt.Sprintf("[… %d строк …]", i-start))
	}
	return strings.Join(out, "\n")
}

func (r excerptRenderer) length() int {
	length := 0
	for i := 0; i < len(r.lines); {
		if r.selected[i] {
			length += r.lengths[i]
			i++
			if i < len(r.lines) {
				length++
			}
			continue
		}
		start := i
		for i < len(r.lines) && !r.selected[i] {
			i++
		}
		count := i - start
		digits := 1
		for n := count; n >= 10; n /= 10 {
			digits++
		}
		length += utf8.RuneCountInString("[…  строк …]") + digits
		if i < len(r.lines) {
			length++
		}
	}
	return length
}

func excerptLineLengths(lines, cachedLines []string, cachedLengths []int) []int {
	lengths := append([]int(nil), cachedLengths...)
	if len(lengths) != len(lines) {
		lengths = make([]int, len(lines))
	}
	for i, line := range lines {
		if len(cachedLengths) != len(lines) || i >= len(cachedLines) || line != cachedLines[i] {
			lengths[i] = utf8.RuneCountInString(line)
		}
	}
	return lengths
}

func selectExcerptEvidence(lines []string, lineWords [][]string, lengths []int, limit int) []bool {
	selected := make([]bool, len(lines))
	for i, words := range lineWords {
		if len(words) == 0 {
			continue
		}
		for j := max(0, i-1); j <= min(len(lines)-1, i+1); j++ {
			selected[j] = true
		}
	}
	renderer := excerptRenderer{lines: lines, lengths: lengths, selected: selected}
	if renderer.length() <= limit {
		return selected
	}
	clear(selected)
	remaining := excerptRemainingWords(lineWords)
	windows := excerptWindows(lines, lineWords, lengths, remaining)
	fitExcerptWindows(windows, lines, lineWords, lengths, remaining, selected, limit)
	return selected
}

func excerptRemainingWords(lineWords [][]string) map[string]bool {
	remaining := make(map[string]bool)
	for _, words := range lineWords {
		for _, word := range words {
			remaining[word] = true
		}
	}
	return remaining
}

type excerptWindow struct{ index, score, cost int }

func excerptWindows(lines []string, lineWords [][]string, lengths []int, remaining map[string]bool) []excerptWindow {
	windows := make([]excerptWindow, 0, len(lines))
	for i := range lines {
		if len(lineWords[i]) == 0 {
			continue
		}
		score, cost := 0, 0
		for _, word := range neighboringAnswerWords(lineWords, i) {
			if remaining[word] {
				score++
			}
		}
		for j := max(0, i-1); j <= min(len(lines)-1, i+1); j++ {
			cost += lengths[j] + 1
		}
		if cost < 1 {
			cost = 1
		}
		windows = append(windows, excerptWindow{i, score, cost})
	}
	sort.Slice(windows, func(i, j int) bool {
		a, b := windows[i], windows[j]
		if a.score*b.cost != b.score*a.cost {
			return a.score*b.cost > b.score*a.cost
		}
		if a.score != b.score {
			return a.score > b.score
		}
		return a.index < b.index
	})
	return windows
}

func fitExcerptWindows(windows []excerptWindow, lines []string, lineWords [][]string, lengths []int, remaining map[string]bool, selected []bool, limit int) {
	selectedCount, selectedRunes, omittedRuns := 0, 0, 1
	for _, candidate := range windows {
		best := candidate.index
		if countVisibleWords(neighboringAnswerWords(lineWords, best), remaining) == 0 {
			continue
		}
		from, to := max(0, best-1), min(len(lines)-1, best+1)
		added := selectWindow(selected, lengths, from, to, &selectedCount, &selectedRunes, &omittedRuns)
		if !excerptSelectionFits(selectedCount, selectedRunes, omittedRuns, limit) {
			undoExcerptWindow(selected, lengths, added, &selectedCount, &selectedRunes, &omittedRuns)
			if !selected[best] {
				added = selectWindow(selected, lengths, best, best, &selectedCount, &selectedRunes, &omittedRuns)
				if !excerptSelectionFits(selectedCount, selectedRunes, omittedRuns, limit) {
					undoExcerptWindow(selected, lengths, added, &selectedCount, &selectedRunes, &omittedRuns)
					continue
				}
			}
		}
		for j := from; j <= to; j++ {
			if selected[j] {
				for _, word := range lineWords[j] {
					delete(remaining, word)
				}
			}
		}
	}
}

func countVisibleWords(words []string, remaining map[string]bool) int {
	count := 0
	for _, word := range words {
		if remaining[word] {
			count++
		}
	}
	return count
}

func selectWindow(selected []bool, lengths []int, from, to int, count, runes, omitted *int) []int {
	var added []int
	for i := from; i <= to; i++ {
		if selected[i] {
			continue
		}
		leftOmitted := i > 0 && !selected[i-1]
		rightOmitted := i+1 < len(selected) && !selected[i+1]
		*count++
		*runes += lengths[i]
		if leftOmitted && rightOmitted {
			*omitted++
		} else if !leftOmitted && !rightOmitted {
			*omitted--
		}
		selected[i] = true
		added = append(added, i)
	}
	return added
}

func undoExcerptWindow(selected []bool, lengths []int, added []int, count, runes, omitted *int) {
	for i := len(added) - 1; i >= 0; i-- {
		index := added[i]
		leftOmitted := index > 0 && !selected[index-1]
		rightOmitted := index+1 < len(selected) && !selected[index+1]
		*count--
		*runes -= lengths[index]
		if leftOmitted && rightOmitted {
			*omitted--
		} else if !leftOmitted && !rightOmitted {
			*omitted++
		}
		selected[index] = false
	}
}

func excerptSelectionFits(count, runes, omitted, limit int) bool {
	segments := count + omitted
	return runes+32*omitted+max(segments-1, 0) <= limit
}

type wordSpan struct {
	start, end int
	words      []string
}

type wordExcerptRenderer struct {
	runes []rune
	spans []wordSpan
	limit int
}

func excerptAroundWords(s string, words []string, limit int, matcher *wordMatcher, positionsCache map[int]map[string]int, lineIndex int) string {
	if matcher == nil {
		matcher = newWordMatcher(words)
	}
	runes := []rune(s)
	positions := positionsCache[lineIndex]
	if positions == nil {
		positions = matcher.wordPositions(s)
		if positionsCache != nil {
			positionsCache[lineIndex] = positions
		}
	}
	spans := answerWordSpans(words, positions, len(runes))
	if len(spans) == 0 {
		return excerpt(s, limit)
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	renderer := wordExcerptRenderer{runes: runes, spans: spans, limit: limit}
	selected, _ := chooseWordSpans(renderer, positions)
	edgeStart, edgeEnd := fillWordExcerptEdges(renderer, selected)
	current := renderer.render(selected, edgeStart, edgeEnd)
	if !hasSelectedSpan(selected) {
		return excerpt(s, limit)
	}
	if utf8.RuneCountInString(current) > limit {
		return excerpt(current, limit)
	}
	return current
}

func answerWordSpans(words []string, positions map[string]int, runeCount int) []wordSpan {
	var spans []wordSpan
	for _, word := range matchedWordsFromPositions(words, positions) {
		position := positions[word]
		spans = append(spans, wordSpan{
			start: max(0, position-20),
			end:   min(runeCount, position+utf8.RuneCountInString(word)+20),
			words: []string{word},
		})
	}
	return spans
}

func matchedWordsFromPositions(words []string, positions map[string]int) []string {
	matched := make([]string, 0, len(positions))
	for word := range positions {
		if i := sort.SearchStrings(words, word); i < len(words) && words[i] == word {
			matched = append(matched, word)
		}
	}
	sort.Strings(matched)
	return matched
}

func matchedWordsInLine(line string, words []string, matcher *wordMatcher) []string {
	positions := matcher.wordPositions(line)
	if len(positions) == 0 {
		return nil
	}
	return matchedWordsFromPositions(words, positions)
}

func (r wordExcerptRenderer) renderPicked(picked []int, left, right int) string {
	var out strings.Builder
	cursor := 0
	writeGap := func(end int) {
		if end <= cursor {
			return
		}
		if out.Len() > 0 {
			out.WriteByte('\n')
		}
		out.WriteString(fmt.Sprintf("[обрезано: %d знаков из %d]", len(r.runes)-r.limit, len(r.runes)))
		out.WriteByte('\n')
		cursor = end
	}
	if left > 0 {
		out.WriteString(string(r.runes[:left]))
		cursor = left
	}
	for _, i := range picked {
		item := r.spans[i]
		start, end := max(item.start, left), min(item.end, right)
		if end <= start || end <= cursor {
			continue
		}
		if start > cursor {
			writeGap(start)
		}
		if cursor > 0 && out.Len() > 0 && !strings.HasSuffix(out.String(), "\n") {
			out.WriteByte('\n')
		}
		out.WriteString(string(r.runes[max(start, cursor):end]))
		cursor = end
	}
	if cursor < right {
		writeGap(right)
	}
	if right < len(r.runes) {
		if out.Len() > 0 {
			out.WriteByte('\n')
		}
		out.WriteString(string(r.runes[right:]))
	}
	return out.String()
}

// renderPickedRuneCount returns the exact rune count of renderPicked without
// materializing every candidate string considered by bestWordSpan.
func (r wordExcerptRenderer) renderPickedRuneCount(picked []int, left, right, markerRunes int) int {
	count, cursor := 0, 0
	hasOutput, endsWithNewline := false, false
	writeGap := func(end int) {
		if end <= cursor {
			return
		}
		if hasOutput {
			count++
		}
		count += markerRunes + 1
		hasOutput, endsWithNewline = true, true
		cursor = end
	}
	if left > 0 {
		count = left
		hasOutput = true
		endsWithNewline = r.runes[left-1] == '\n'
		cursor = left
	}
	for _, i := range picked {
		item := r.spans[i]
		start, end := max(item.start, left), min(item.end, right)
		if end <= start || end <= cursor {
			continue
		}
		if start > cursor {
			writeGap(start)
		}
		if cursor > 0 && hasOutput && !endsWithNewline {
			count++
		}
		start = max(start, cursor)
		if end > start {
			count += end - start
			hasOutput = true
			endsWithNewline = r.runes[end-1] == '\n'
		}
		cursor = end
	}
	if cursor < right {
		writeGap(right)
	}
	if right < len(r.runes) {
		if hasOutput {
			count++
		}
		count += len(r.runes) - right
	}
	return count
}

func (r wordExcerptRenderer) render(selected []bool, left, right int) string {
	picked := make([]int, 0)
	for i, keep := range selected {
		if keep {
			picked = append(picked, i)
		}
	}
	return r.renderPicked(picked, left, right)
}

func chooseWordSpans(renderer wordExcerptRenderer, positions map[string]int) ([]bool, []int) {
	selected := make([]bool, len(renderer.spans))
	remaining, positionWords := wordPositionSet(renderer.spans, positions)
	current := ""
	picked := make([]int, 0)
	for {
		best, bestText := bestWordSpan(renderer, selected, picked, current, remaining, positionWords, positions)
		if best < 0 {
			return selected, picked
		}
		selected[best] = true
		picked = append(picked, best)
		sort.Ints(picked)
		current = bestText
		clearCoveredWords(renderer.spans[best], positionWords, positions, remaining)
	}
}

func wordPositionSet(spans []wordSpan, positions map[string]int) (map[string]bool, []string) {
	remaining := make(map[string]bool, len(spans))
	positionWords := make([]string, 0, len(positions))
	for _, span := range spans {
		word := span.words[0]
		remaining[word] = true
		positionWords = append(positionWords, word)
	}
	sort.Slice(positionWords, func(i, j int) bool { return positions[positionWords[i]] < positions[positionWords[j]] })
	return remaining, positionWords
}

func bestWordSpan(renderer wordExcerptRenderer, selected []bool, picked []int, current string, remaining map[string]bool, positionWords []string, positions map[string]int) (int, string) {
	best, bestScore, bestText, bestCost := -1, 0, "", 0
	currentLen := utf8.RuneCountInString(current)
	markerRunes := utf8.RuneCountInString(fmt.Sprintf("[обрезано: %d знаков из %d]", len(renderer.runes)-renderer.limit, len(renderer.runes)))
	for i, item := range renderer.spans {
		if selected[i] || !remaining[item.words[0]] {
			continue
		}
		trialPicked := append(append([]int(nil), picked...), i)
		sort.Ints(trialPicked)
		trialRunes := renderer.renderPickedRuneCount(trialPicked, 0, len(renderer.runes), markerRunes)
		cost := max(trialRunes-currentLen, 1)
		if trialRunes > renderer.limit {
			continue
		}
		score := visibleSpanWords(item, positionWords, positions, remaining)
		if betterWordSpan(i, score, cost, best, bestScore, bestCost) {
			best, bestScore, bestCost = i, score, cost
		}
	}
	if best >= 0 {
		trialPicked := append(append([]int(nil), picked...), best)
		sort.Ints(trialPicked)
		bestText = renderer.renderPicked(trialPicked, 0, len(renderer.runes))
	}
	return best, bestText
}

func visibleSpanWords(item wordSpan, positionWords []string, positions map[string]int, remaining map[string]bool) int {
	first := sort.Search(len(positionWords), func(j int) bool { return positions[positionWords[j]] >= item.start })
	score := 0
	for j := first; j < len(positionWords) && positions[positionWords[j]] < item.end; j++ {
		if remaining[positionWords[j]] {
			score++
		}
	}
	return score
}

func betterWordSpan(index, score, cost, best, bestScore, bestCost int) bool {
	return score*bestCost > bestScore*cost || score*bestCost == bestScore*cost && score > 0 && (best < 0 || index < best)
}

func clearCoveredWords(item wordSpan, positionWords []string, positions map[string]int, remaining map[string]bool) {
	first := sort.Search(len(positionWords), func(j int) bool { return positions[positionWords[j]] >= item.start })
	for j := first; j < len(positionWords) && positions[positionWords[j]] < item.end; j++ {
		delete(remaining, positionWords[j])
	}
}

func fillWordExcerptEdges(renderer wordExcerptRenderer, selected []bool) (int, int) {
	edgeStart, edgeEnd := 0, len(renderer.runes)
	for _, fromStart := range []bool{true, false} {
		low, best := 0, 0
		var high int
		if fromStart {
			high = wordSpanBoundary(selected, renderer.spans, true, len(renderer.runes))
		} else {
			high = wordSpanBoundary(selected, renderer.spans, false, len(renderer.runes))
		}
		for low <= high {
			middle := (low + high) / 2
			left, right := edgeStart, edgeEnd
			if fromStart {
				left = middle
			} else {
				right = len(renderer.runes) - middle
			}
			if utf8.RuneCountInString(renderer.render(selected, left, right)) <= renderer.limit {
				best = middle
				low = middle + 1
			} else {
				high = middle - 1
			}
		}
		if fromStart {
			edgeStart = best
		} else {
			edgeEnd = len(renderer.runes) - best
		}
	}
	return edgeStart, edgeEnd
}

func wordSpanBoundary(selected []bool, spans []wordSpan, fromStart bool, runeCount int) int {
	boundary := runeCount
	for i, item := range spans {
		if !selected[i] {
			continue
		}
		value := item.start
		if !fromStart {
			value = runeCount - item.end
		}
		boundary = min(boundary, value)
	}
	return boundary
}

func hasSelectedSpan(selected []bool) bool {
	for _, keep := range selected {
		if keep {
			return true
		}
	}
	return false
}

func mask(s string) string { x, _ := secretmask.Mask(s); return x }

var wordRE = regexp.MustCompile(`[\pL\pN_./-]{2,}`)

func answerWords(s string) []string {
	var a []string
	seen := make(map[string]struct{})
	for _, w := range wordRE.FindAllString(strings.ToLower(s), -1) {
		if len([]rune(w)) >= 3 {
			if _, ok := seen[w]; ok {
				continue
			}
			seen[w] = struct{}{}
			a = append(a, w)
		}
	}
	sort.Strings(a)
	return a
}

func countAnswerWords(s string) int {
	count := 0
	for _, word := range wordRE.FindAllString(strings.ToLower(s), -1) {
		if len([]rune(word)) >= 3 {
			count++
		}
	}
	return count
}

// lineAnswerWords retains substring matches across token boundaries such as
// paths and inflected words. Callers with an existing matcher reuse it below.
func lineAnswerWords(line string, words []string) []string {
	return lineAnswerWordsWithMatcher(line, words, nil)
}

func lineAnswerWordsWithMatcher(line string, words []string, matcher *wordMatcher) []string {
	if matcher == nil {
		matcher = newWordMatcher(words)
	} else if !sameWordSlice(matcher.words, words) {
		// A line may only need words that have not appeared in earlier lines.
		// Keep using the turn-wide automaton and filter its matches instead of
		// rebuilding an automaton for every long line.
		if len(words) <= len(matcher.words) && sortedWordsContain(matcher.words, words) {
			return matchedWordsInLine(line, words, matcher)
		}
		matcher = newWordMatcher(words)
	}
	return matchedWordsInLine(line, words, matcher)
}

func sameWordSlice(a, b []string) bool {
	return len(a) == len(b) && (len(a) == 0 || &a[0] == &b[0])
}

func exactLineAnswerWords(line string, words []string) []string {
	var found []string
	visitWordTokens(line, func(token string) {
		if i := sort.SearchStrings(words, token); i < len(words) && words[i] == token {
			if !slices.Contains(found, token) {
				found = append(found, token)
			}
		}
	})
	if len(found) == 0 {
		return nil
	}
	sort.Strings(found)
	return found
}

func visitWordTokens(s string, visit func(string)) {
	start, count := -1, 0
	for i, r := range s {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_' || r == '.' || r == '/' || r == '-' {
			if start < 0 {
				start = i
			}
			count++
			continue
		}
		if start >= 0 && count >= 3 {
			visit(s[start:i])
		}
		start, count = -1, 0
	}
	if start >= 0 && count >= 3 {
		visit(s[start:])
	}
}

func splitExcerptLines(s string) []string {
	lines := strings.Split(s, "\n")
	// JSON tool inputs can encode line breaks as two literal characters.
	if len(lines) == 1 && strings.Contains(s, `\n`) {
		lines = strings.Split(s, `\n`)
	}
	return lines
}

func answerLineWords(ctx context.Context, lines []string, matcher *wordMatcher) ([][]string, error) {
	indexed := make([][]string, len(lines))
	for i, line := range lines {
		indexed[i] = exactLineAnswerWords(strings.ToLower(line), matcher.words)
		found, err := matcher.wordsInLine(ctx, line)
		if err != nil {
			return nil, err
		}
		seen := make(map[string]struct{}, len(indexed[i])+len(found))
		for _, word := range indexed[i] {
			seen[word] = struct{}{}
		}
		for _, word := range found {
			seen[word] = struct{}{}
		}
		indexed[i] = indexed[i][:0]
		for word := range seen {
			indexed[i] = append(indexed[i], word)
		}
		sort.Strings(indexed[i])
	}
	return indexed, nil
}

type wordNode struct {
	next    map[byte]int
	fail    int
	outputs []string
}

type wordMatcher struct {
	nodes []wordNode
	words []string
}

func newWordMatcher(words []string) *wordMatcher {
	m := &wordMatcher{nodes: []wordNode{{next: make(map[byte]int)}}, words: words}
	for _, word := range words {
		state := 0
		for i := 0; i < len(word); i++ {
			b := word[i]
			next, ok := m.nodes[state].next[b]
			if !ok {
				next = len(m.nodes)
				m.nodes[state].next[b] = next
				m.nodes = append(m.nodes, wordNode{next: make(map[byte]int)})
			}
			state = next
		}
		m.nodes[state].outputs = append(m.nodes[state].outputs, word)
	}
	queue := make([]int, 0, len(m.nodes))
	for _, child := range m.nodes[0].next {
		queue = append(queue, child)
	}
	for head := 0; head < len(queue); head++ {
		state := queue[head]
		for b, child := range m.nodes[state].next {
			fallback := m.nodes[state].fail
			for fallback != 0 {
				if _, ok := m.nodes[fallback].next[b]; ok {
					break
				}
				fallback = m.nodes[fallback].fail
			}
			if next, ok := m.nodes[fallback].next[b]; ok && next != child {
				m.nodes[child].fail = next
			}
			m.nodes[child].outputs = append(m.nodes[child].outputs, m.nodes[m.nodes[child].fail].outputs...)
			queue = append(queue, child)
		}
	}
	return m
}

func (m *wordMatcher) wordsInLine(ctx context.Context, line string) ([]string, error) {
	lower := strings.ToLower(line)
	var found []string
	var seen map[string]struct{}
	state := 0
	for i := 0; i < len(lower); i++ {
		if i&0xffff == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		b := lower[i]
		for state != 0 {
			if _, ok := m.nodes[state].next[b]; ok {
				break
			}
			state = m.nodes[state].fail
		}
		if next, ok := m.nodes[state].next[b]; ok {
			state = next
		}
		for _, word := range m.nodes[state].outputs {
			if seen == nil {
				seen = make(map[string]struct{})
			}
			if _, ok := seen[word]; !ok {
				seen[word] = struct{}{}
				found = append(found, word)
			}
		}
	}
	sort.Strings(found)
	return found, nil
}

func (m *wordMatcher) containsAny(ctx context.Context, line string) (bool, error) {
	lower := strings.ToLower(line)
	state := 0
	for i := 0; i < len(lower); i++ {
		if i&0xffff == 0 {
			if err := ctx.Err(); err != nil {
				return false, err
			}
		}
		b := lower[i]
		for state != 0 {
			if _, ok := m.nodes[state].next[b]; ok {
				break
			}
			state = m.nodes[state].fail
		}
		if next, ok := m.nodes[state].next[b]; ok {
			state = next
		}
		if len(m.nodes[state].outputs) > 0 {
			return true, nil
		}
	}
	return false, nil
}

func (m *wordMatcher) wordPositions(line string) map[string]int {
	lower := strings.ToLower(line)
	bytePositions := make(map[string]int)
	state := 0
	for i := 0; i < len(lower); i++ {
		b := lower[i]
		for state != 0 {
			if _, ok := m.nodes[state].next[b]; ok {
				break
			}
			state = m.nodes[state].fail
		}
		if next, ok := m.nodes[state].next[b]; ok {
			state = next
		}
		for _, word := range m.nodes[state].outputs {
			start := i + 1 - len(word)
			if old, ok := bytePositions[word]; !ok || start < old {
				bytePositions[word] = start
			}
		}
	}
	positions := make(map[string]int, len(bytePositions))
	type wordBytePosition struct {
		word     string
		position int
	}
	ordered := make([]wordBytePosition, 0, len(bytePositions))
	for word, position := range bytePositions {
		ordered = append(ordered, wordBytePosition{word: word, position: position})
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].position < ordered[j].position })
	lastByte, runePosition := 0, 0
	for _, match := range ordered {
		runePosition += utf8.RuneCountInString(lower[lastByte:match.position])
		positions[match.word] = runePosition
		lastByte = match.position
	}
	return positions
}

func hasLineWord(lines [][]string) bool {
	for _, line := range lines {
		if len(line) > 0 {
			return true
		}
	}
	return false
}

func cachedOutputWords(lines [][]string) []string {
	seen := make(map[string]struct{})
	for _, line := range lines {
		for _, word := range line {
			seen[word] = struct{}{}
		}
	}
	words := slices.Sorted(maps.Keys(seen))
	if words == nil {
		return []string{}
	}
	return words
}

func neighboringAnswerWords(lineWords [][]string, index int) []string {
	seen := make(map[string]struct{})
	var words []string
	for line := max(0, index-1); line <= min(len(lineWords)-1, index+1); line++ {
		for _, word := range lineWords[line] {
			if _, ok := seen[word]; ok {
				continue
			}
			seen[word] = struct{}{}
			words = append(words, word)
		}
	}
	return words
}

func intersectWords(lineWords, words []string) []string {
	var out []string
	for _, word := range lineWords {
		if i := sort.SearchStrings(words, word); i < len(words) && words[i] == word {
			out = append(out, word)
		}
	}
	return out
}

func sortedWordsContain(words, subset []string) bool {
	i, j := 0, 0
	for i < len(subset) && j < len(words) {
		switch {
		case subset[i] == words[j]:
			i++
			j++
		case subset[i] > words[j]:
			j++
		default:
			return false
		}
	}
	return i == len(subset)
}

func matches(s string, words []string) bool {
	v := strings.ToLower(s)
	found := false
	visitWordTokens(v, func(token string) {
		if i := sort.SearchStrings(words, token); i < len(words) && words[i] == token {
			found = true
		}
	})
	if found {
		return true
	}
	for _, w := range words {
		if strings.Contains(v, w) {
			return true
		}
	}
	return false
}
