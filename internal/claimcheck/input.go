// Package claimcheck builds the context shown to the claim-check judge from
// the Claude Code transcript as it existed at a particular event.
package claimcheck

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
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
	cropWords      []string
	outputLines    []string
	outputWords    [][]string
	outputLengths  []int
	outputRunes    int
	inputLines     []string
	inputWords     [][]string
	inputLengths   []int
	inputRunes     int
	matcher        *wordMatcher
	positions      map[int]map[string]int
	inputPositions map[int]map[string]int
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
	DefaultContextLimit        = 21000
	turnToolsCharLimit         = 20000
	shortAnswerTurnToolsLimit  = 11500
	longAnswerContextLimit     = 10000
	largeAnswerWordThreshold   = 250
	shortAnswerInputCharLimit  = 24300
	toolInputCharLimit         = 300
	callsOnlyInputCharLimit    = 1000
	callsOnlyMarkerCharReserve = 48
	toolOutputCharLimit        = 1600
	lastTurnToolsKept          = 5
	skippedToolName            = "[пропущено]"
	contextOutputCharLimit     = 4000
	contextRecentOutputLimit   = 300
	contextPreviousAnswerLimit = 1200
	maxContextToolCalls        = 2
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
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	start, promptStart, err := turnStart(rows, line)
	if err != nil {
		return nil, err
	}
	answerStart, err := legacyTurnStart(rows, line)
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
	if options.CallsOnly {
		words = append(words, "amend", "commit", "push")
		slices.Sort(words)
		words = slices.Compact(words)
	}
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
	if promptStart >= 0 {
		input.UserPrompt, _, err = userPrompt(&rows[promptStart])
		if err != nil {
			return nil, fmt.Errorf("read user prompt at line %d: %w", rows[promptStart].Line, err)
		}
		input.Attachments, err = attachments(&rows[promptStart])
		if err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if options.CallsOnly {
		return input, nil
	}
	if err := appendContext(ctx, input, rows, results, start, answer, words, matcher, limit, options.Unbounded); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !options.Unbounded && answerWordCount <= largeAnswerWordThreshold {
		for {
			size, err := inputJSONSize(input)
			if err != nil {
				return nil, err
			}
			if size <= shortAnswerInputCharLimit {
				break
			}
			reduced := max(limit-(size-shortAnswerInputCharLimit)-8, 0)
			if reduced >= limit {
				reduced = limit - 1
			}
			limit = reduced
			input.Context = Context{}
			if err := appendContext(ctx, input, rows, results, start, answer, words, matcher, limit, false); err != nil {
				return nil, err
			}
			if limit == 0 {
				return nil, fmt.Errorf("input size exceeds %d characters", shortAnswerInputCharLimit)
			}
		}
	}
	return input, nil
}

func inputJSONSize(input *Input) (int, error) {
	encoded, err := json.Marshal(input)
	if err != nil {
		return 0, fmt.Errorf("encode input size: %w", err)
	}
	return utf8.RuneCount(encoded), nil
}

func readRows(path string, line int) ([]row, error) {
	return readRowsContext(context.Background(), path, line)
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
		if n&255 == 0 {
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
	start, prompt, queued, compact, lastEnd, lastHuman := 0, -1, -1, -1, -1, -1
	for i := range rows {
		r := &rows[i]
		if r.Line >= line {
			break
		}
		if r.E.Type == "system" && r.E.Subtype == "turn_duration" {
			lastEnd, queued = i, -1
			continue
		}
		isCompact, err := isCompactSummary(r)
		if err != nil {
			return 0, 0, err
		}
		if isCompact || (r.E.Type == "system" && r.E.Subtype == "compact_boundary") {
			compact = i
			continue
		}
		begins, err := beginsTurn(r)
		if err != nil {
			return 0, 0, fmt.Errorf("classify transcript line %d as turn start: %w", r.Line, err)
		}
		peerDelivery := r.E.Type == "attachment" && r.E.Attachment.Type == "queued_command" && r.E.Attachment.Origin.Kind == "peer" && r.E.Attachment.CommandMode != "task-notification" && lastEnd >= 0 && lastHuman <= lastEnd
		begins = begins || peerDelivery
		if begins {
			prompt = i
			if r.E.Type == "attachment" && r.E.Attachment.Origin.Kind == "human" || r.E.Type == "user" && r.E.Origin.Kind != "peer" {
				lastHuman = i
			}
			if r.E.Type == "attachment" && r.E.Attachment.Origin.Kind == "human" {
				if queued < 0 {
					queued = i
				}
				start = queued
			} else {
				queued, start = -1, i
			}
		}
	}
	if compact > start {
		start = compact
	}
	if prompt < 0 && compact >= 0 {
		prompt = compact
	}
	return start, prompt, nil
}

func legacyTurnStart(rows []row, line int) (int, error) {
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
		begins, err := legacyBeginsTurn(&rows[i])
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

func legacyBeginsTurn(r *row) (bool, error) {
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
		return legacyBeginsAttachmentTurn(r), nil
	}
	if r.E.Type != "user" {
		return false, nil
	}
	if r.E.Origin.Kind == "task-notification" {
		return true, nil
	}
	return beginsUserTurn(r)
}

func legacyBeginsAttachmentTurn(r *row) bool {
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
			t.Output, t.fullOutput = "", ""
			t.outputWords = nil
			t.OutputLength, t.ExitCode, t.IsError = 0, nil, nil
			t.relevant = matches(t.fullInput, words)
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
	fullWords := make([][]string, len(candidates))
	minimumSizes := make([]int, len(candidates))
	for i, tool := range candidates {
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
	for _, tool := range out.Context.RecentTools {
		for _, word := range wordsVisibleInTool(tool, words) {
			delete(remaining, word)
		}
	}
	chosen := make(map[int]Tool)
	for len(chosen) < len(candidates) {
		currentSize, err := contextJSONSize(out.Context)
		if err != nil {
			return err
		}
		available := limit - currentSize - 32
		best, bestScore, bestSize := -1, 0, 0
		var bestTool Tool
		unseen := unseenWords(words, remaining)
		for i := range candidates {
			if err := ctx.Err(); err != nil {
				return err
			}
			if selected[i] {
				continue
			}
			canAdd := false
			for _, word := range fullWords[i] {
				if remaining[word] {
					canAdd = true
					break
				}
			}
			if !canAdd {
				selected[i] = true
				continue
			}
			candidate := candidates[i]
			candidate.answerWords = unseen
			if available < minimumSizes[i] {
				selected[i] = true
				continue
			}
			if candidate.outputLines == nil {
				if err := indexToolOutput(ctx, &candidate); err != nil {
					return err
				}
			}
			if candidate.inputLines == nil {
				if err := indexToolInput(ctx, &candidate); err != nil {
					return err
				}
			}
			candidates[i] = candidate
			fit, ok, err := fitContextTool(candidate, available)
			if err != nil {
				return err
			}
			if !ok {
				selected[i] = true
				continue
			}
			score := 0
			for _, word := range wordsVisibleInTool(fit, words) {
				if remaining[word] {
					score++
				}
			}
			size, err := toolsJSONSize([]Tool{fit})
			if err != nil {
				return err
			}
			if score*bestSize > bestScore*size || score*bestSize == bestScore*size && score > bestScore {
				best, bestScore, bestSize, bestTool = i, score, size, fit
			}
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
	for i := range candidates {
		if tool, ok := chosen[i]; ok {
			out.Context.MatchingTools = append(out.Context.MatchingTools, tool)
		}
	}
	return nil
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

func toolInputExcerpt(tool Tool, limit int) string {
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
	var err error
	input := string(b.Input)
	if b.Name == "Bash" {
		var err error
		input, err = stringField(b.Input, "command")
		if err != nil {
			return Tool{}, fmt.Errorf("decode Bash command: %w", err)
		}
	}
	out := ""
	length := 0
	var code *int
	var iserr *bool
	result, ok := results[b.ID]
	if b.ID != "" && ok && result.row.Line <= end && result.row.Line > r.Line {
		out, err = plain(result.block.Content)
		if err != nil {
			return Tool{}, fmt.Errorf("decode %s output: %w", b.Name, err)
		}
		length = utf8.RuneCountInString(out)
		e := result.block.IsError || result.row.E.IsError
		iserr = &e
		v, ok, err := nestedInt(result.row.E.ToolUseResult, "exit_code")
		if err != nil {
			return Tool{}, fmt.Errorf("decode %s exit code: %w", b.Name, err)
		}
		if ok {
			code = &v
		}
	} else {
	findResult:
		for j := index + 1; j < len(rows) && rows[j].Line <= end; j++ {
			for k := range rows[j].Blocks {
				rb := &rows[j].Blocks[k]
				if rb.Type == "tool_result" && (rb.ToolUseID == b.ID || b.ID == "") {
					out, err := plain(rb.Content)
					if err != nil {
						return Tool{}, fmt.Errorf("decode %s output: %w", b.Name, err)
					}
					length = utf8.RuneCountInString(out)
					e := rb.IsError || rows[j].E.IsError
					iserr = &e
					v, ok, err := nestedInt(rows[j].E.ToolUseResult, "exit_code")
					if err != nil {
						return Tool{}, fmt.Errorf("decode %s exit code: %w", b.Name, err)
					}
					if ok {
						code = &v
					}
					break findResult
				}
			}
		}
	}
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
	return Tool{Time: r.E.Timestamp, Name: b.Name, Input: inputExcerpt, OutputLength: length, Output: excerptWithWordsIndexed(maskedOutput, words, outputLimit, outputLines, outputWords, outputLengths, outputRunes, matcher, positions), ExitCode: code, IsError: iserr, relevant: matches(input, words) || hasLineWord(outputWords), fullInput: maskedInput, fullOutput: maskedOutput, answerWords: words, cropWords: cropWords, inputLines: inputLines, inputWords: inputWords, inputLengths: inputLengths, inputRunes: inputRunes, outputLines: outputLines, outputWords: outputWords, outputLengths: outputLengths, outputRunes: outputRunes, matcher: matcher, positions: positions, inputPositions: inputPositions}, nil
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

// boundTools keeps turnTools within budget characters of JSON. The last calls
// before the answer always stay, sharing half the budget. Matching calls then
// spend the remainder in order of how many not-yet-visible answer words they add.
func boundTools(tools []Tool, answer string, budget int) ([]Tool, error) {
	return boundToolsContext(context.Background(), tools, answer, budget)
}

func boundToolsContext(ctx context.Context, tools []Tool, answer string, budget int) ([]Tool, error) {
	if len(tools) == 0 {
		return []Tool{}, nil
	}
	words := answerWords(answer)
	bounded := make([]Tool, len(tools))
	copy(bounded, tools)
	for i := range bounded {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		t := &bounded[i]
		t.relevant = t.relevant || matches(t.Input, words) || matches(t.Output, words)
		if t.callsOnly {
			t.relevant = t.relevant || matches(t.fullInput, words)
		}
		if !t.callsOnly || len(t.answerWords) == 0 {
			t.answerWords = words
		}
	}
	fullWords := make([][]string, len(bounded))
	minimumSizes := make([]int, len(bounded))
	for i, tool := range bounded {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		fullWords[i] = append(fullWords[i], cachedOutputWords(tool.outputWords)...)
		fullWords[i] = append(fullWords[i], lineAnswerWordsWithMatcher(tool.fullInput, words, tool.matcher)...)
		minimum, err := toolsJSONSize([]Tool{{Name: tool.Name}})
		if err != nil {
			return nil, err
		}
		minimumSizes[i] = minimum
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
	keep := func(i, limit int) error {
		if selected[i] {
			return nil
		}
		// Keeping a call inside a gap at worst splits one skip marker into two:
		// the call, one more marker and two commas. Counts only shrink otherwise.
		marker, err := skippedMarkerSize(len(bounded))
		if err != nil {
			return err
		}
		available := limit - currentSize - 2 - marker
		if available < minimumSizes[i] {
			return nil
		}
		candidate := bounded[i]
		if candidate.callsOnly {
			candidate.relevant = false
		}
		fit, ok, err := fitTool(candidate, available)
		if err != nil || !ok {
			return err
		}
		fit.relevant = bounded[i].relevant
		bounded[i], selected[i] = fit, true
		currentSize, err = toolsJSONSize(toolsWithSkipped(bounded, selected))
		if err != nil {
			return err
		}
		return nil
	}
	if bounded[0].callsOnly {
		for i := range bounded {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if utf8.RuneCountInString(bounded[i].fullInput) <= toolInputCharLimit {
				if err := keep(i, budget); err != nil {
					return nil, err
				}
			}
		}
	}
	remaining := make(map[string]bool)
	for _, visible := range fullWords {
		for _, word := range visible {
			remaining[word] = true
		}
	}
	for i := range bounded {
		if selected[i] {
			for _, word := range wordsVisibleInTool(bounded[i], words) {
				delete(remaining, word)
			}
		}
	}
	attempted := make([]bool, len(bounded))
	for {
		if bounded[0].callsOnly {
			break
		}
		currentSize, err := toolsJSONSize(toolsWithSkipped(bounded, selected))
		if err != nil {
			return nil, err
		}
		marker, err := skippedMarkerSize(len(bounded))
		if err != nil {
			return nil, err
		}
		available := budget - currentSize - 2 - marker
		best, bestScore, bestSize := -1, 0, 0
		var bestFit Tool
		unseen := unseenWords(words, remaining)
		for i := range bounded {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if selected[i] || attempted[i] || !bounded[i].relevant {
				continue
			}
			canAdd := false
			for _, word := range fullWords[i] {
				if remaining[word] {
					canAdd = true
					break
				}
			}
			if !canAdd {
				attempted[i] = true
				continue
			}
			candidate := bounded[i]
			candidate.answerWords = unseen
			if available < minimumSizes[i] {
				attempted[i] = true
				continue
			}
			fit, ok, err := fitTool(candidate, available)
			if err != nil {
				return nil, err
			}
			if !ok {
				attempted[i] = true
				continue
			}
			score := 0
			for _, word := range wordsVisibleInTool(fit, words) {
				if remaining[word] {
					score++
				}
			}
			size, err := toolsJSONSize([]Tool{fit})
			if err != nil {
				return nil, err
			}
			if score*bestSize > bestScore*size || score*bestSize == bestScore*size && score > bestScore {
				best, bestScore, bestSize, bestFit = i, score, size, fit
			}
		}
		if best < 0 || bestScore == 0 {
			break
		}
		bounded[best], selected[best] = bestFit, true
		for _, word := range wordsVisibleInTool(bestFit, words) {
			delete(remaining, word)
		}
	}
	currentSize, err = toolsJSONSize(toolsWithSkipped(bounded, selected))
	if err != nil {
		return nil, err
	}
	// Keep any remaining answer-matching calls while there is room, then fill
	// with earlier calls. Selection order does not affect transcript order.
	for i := range bounded {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if bounded[i].relevant {
			if err := keep(i, budget); err != nil {
				return nil, err
			}
		}
	}
	for i := last - 1; i >= 0; i-- {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := keep(i, budget); err != nil {
			return nil, err
		}
	}
	for i := range bounded {
		if err := keep(i, budget); err != nil {
			return nil, err
		}
	}
	if bounded[0].callsOnly {
		remaining := 0
		for i := range bounded {
			if selected[i] && bounded[i].relevant {
				remaining++
			}
		}
		for i := range bounded {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if !selected[i] || !bounded[i].relevant || remaining == 0 {
				continue
			}
			remaining--
			currentSize, err := toolsJSONSize(toolsWithSkipped(bounded, selected))
			if err != nil {
				return nil, err
			}
			callSize, err := toolsJSONSize([]Tool{bounded[i]})
			if err != nil {
				return nil, err
			}
			share := (budget - currentSize + callSize) / (remaining + 1)
			if share <= callSize {
				continue
			}
			fit, ok, err := fitTool(bounded[i], share)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			fitSize, err := toolsJSONSize([]Tool{fit})
			if err != nil {
				return nil, err
			}
			if fitSize > callSize {
				bounded[i] = fit
			}
		}
	}
	return toolsWithSkipped(bounded, selected), nil
}

func matchingWords(tool Tool, words []string) []string {
	input, output := strings.ToLower(tool.fullInput), strings.ToLower(tool.fullOutput)
	found := make([]string, 0, len(words))
	seen := make(map[string]bool, len(words))
	for _, word := range words {
		if !seen[word] && (strings.Contains(input, word) || strings.Contains(output, word)) {
			found = append(found, word)
			seen[word] = true
		}
	}
	return found
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
	found, _ := matcher.wordsInLine(context.Background(), input+"\n"+output)
	for _, word := range found {
		seen[word] = struct{}{}
	}
	visible := make([]string, 0, len(seen))
	for word := range seen {
		visible = append(visible, word)
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
		inputLimit = max(toolInputCharLimit-callsOnlyMarkerCharReserve, 0)
		if tool.relevant {
			inputLimit = max(callsOnlyInputCharLimit-callsOnlyMarkerCharReserve, 0)
		}
	}
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
	if totalRunes < 0 {
		totalRunes = utf8.RuneCountInString(s)
	}
	if totalRunes <= limit {
		return s
	}
	if matcher == nil {
		matcher = newWordMatcher(words)
	}
	lineWords := make([][]string, len(lines))
	for i, line := range lines {
		if i < len(indexed) {
			lineWords[i] = intersectWords(indexed[i], words)
		} else {
			lineWords[i], _ = matcher.wordsInLine(context.Background(), line)
		}
		if utf8.RuneCountInString(line) > limit && len(lineWords[i]) > 0 {
			lines[i] = excerptAroundWords(line, words, max(limit-80, 1), matcher, positions, i)
			lineWords[i] = lineAnswerWordsWithMatcher(lines[i], words, matcher)
		}
	}
	evidence := make([]bool, len(lines))
	for i := range lines {
		if len(lineWords[i]) > 0 {
			for j := max(0, i-1); j <= min(len(lines)-1, i+1); j++ {
				evidence[j] = true
			}
		}
	}
	selected := make([]bool, len(lines))
	for i := range evidence {
		selected[i] = evidence[i]
	}
	lineLengths := append([]int(nil), cachedLengths...)
	if len(lineLengths) != len(lines) {
		lineLengths = make([]int, len(lines))
	}
	for i := range lines {
		if len(cachedLengths) != len(lines) || i >= len(cachedLines) || lines[i] != cachedLines[i] {
			lineLengths[i] = utf8.RuneCountInString(lines[i])
		}
	}
	render := func() string {
		var out []string
		for i := 0; i < len(lines); {
			if selected[i] {
				out = append(out, lines[i])
				i++
				continue
			}
			start := i
			for i < len(lines) && !selected[i] {
				i++
			}
			out = append(out, fmt.Sprintf("[… %d строк …]", i-start))
		}
		return strings.Join(out, "\n")
	}
	renderLength := func() int {
		length := 0
		for i := 0; i < len(lines); {
			if selected[i] {
				length += lineLengths[i]
				i++
				if i < len(lines) {
					length++
				}
				continue
			}
			start := i
			for i < len(lines) && !selected[i] {
				i++
			}
			count := i - start
			digits := 1
			for n := count; n >= 10; n /= 10 {
				digits++
			}
			length += utf8.RuneCountInString("[…  строк …]") + digits
			if i < len(lines) {
				length++
			}
		}
		return length
	}
	// If evidence alone exceeds the cap, keep the line windows that add the most
	// not-yet-visible answer words. This avoids spending the whole excerpt on an
	// early cluster when later lines contain different evidence.
	if renderLength() > limit {
		for i := range selected {
			selected[i] = false
		}
		remaining := make(map[string]bool)
		for _, lineWords := range lineWords {
			for _, word := range lineWords {
				remaining[word] = true
			}
		}
		windowWords := make([][]string, len(lines))
		for i := range lines {
			seen := make(map[string]struct{})
			for j := max(0, i-1); j <= min(len(lines)-1, i+1); j++ {
				for _, word := range lineWords[j] {
					if _, ok := seen[word]; !ok {
						seen[word] = struct{}{}
						windowWords[i] = append(windowWords[i], word)
					}
				}
			}
		}
		type window struct{ index, score, cost int }
		windows := make([]window, 0, len(lines))
		for i := range lines {
			if len(lineWords[i]) == 0 {
				continue
			}
			score, cost := 0, 0
			for _, word := range windowWords[i] {
				if remaining[word] {
					score++
				}
			}
			for j := max(0, i-1); j <= min(len(lines)-1, i+1); j++ {
				cost += lineLengths[j] + 1
			}
			if cost < 1 {
				cost = 1
			}
			windows = append(windows, window{i, score, cost})
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
		selectedCount, selectedRunes, omittedRuns := 0, 0, 1
		setSelected := func(i int, value bool) {
			if selected[i] == value {
				return
			}
			leftOmitted := i > 0 && !selected[i-1]
			rightOmitted := i+1 < len(selected) && !selected[i+1]
			if value {
				selectedCount++
				selectedRunes += lineLengths[i]
				if leftOmitted && rightOmitted {
					omittedRuns++
				} else if leftOmitted || rightOmitted { /* same run */
				} else {
					omittedRuns--
				}
			} else {
				selectedCount--
				selectedRunes -= lineLengths[i]
				if leftOmitted && rightOmitted {
					omittedRuns--
				} else if !leftOmitted && !rightOmitted {
					omittedRuns++
				}
			}
			selected[i] = value
		}
		fits := func() bool {
			// An omitted run marker is under 32 runes; this conservative bound
			// keeps fit checks constant-time while leaving room for separators.
			segments := selectedCount + omittedRuns
			return selectedRunes+32*omittedRuns+max(segments-1, 0) <= limit
		}
		for _, candidate := range windows {
			best := candidate.index
			score := 0
			for _, word := range windowWords[best] {
				if remaining[word] {
					score++
				}
			}
			if score == 0 {
				continue
			}
			from, to := max(0, best-1), min(len(lines)-1, best+1)
			var added []int
			for j := from; j <= to; j++ {
				if !selected[j] {
					setSelected(j, true)
					added = append(added, j)
				}
			}
			if !fits() {
				for i := len(added) - 1; i >= 0; i-- {
					setSelected(added[i], false)
				}
				if !selected[best] {
					setSelected(best, true)
					if !fits() {
						setSelected(best, false)
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
	// Boundary context is lower priority than matching evidence.
	for _, i := range []int{0, len(lines) - 1} {
		if !selected[i] {
			selected[i] = true
			if renderLength() > limit {
				selected[i] = false
			}
		}
	}
	result := render()
	if utf8.RuneCountInString(result) <= limit {
		return result
	}
	// A single exceptionally long evidence line can exceed the budget.
	return excerpt(result, limit)
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
	type span struct {
		start, end int
		words      []string
	}
	var spans []span
	for _, word := range words {
		if position, ok := positions[word]; ok {
			spans = append(spans, span{
				start: max(0, position-20),
				end:   min(len(runes), position+utf8.RuneCountInString(word)+20),
				words: []string{word},
			})
		}
	}
	if len(spans) == 0 {
		return excerpt(s, limit)
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	renderPicked := func(picked []int, left, right int) string {
		var out strings.Builder
		cursor := 0
		writeGap := func(end int) {
			if end <= cursor {
				return
			}
			if out.Len() > 0 {
				out.WriteByte('\n')
			}
			out.WriteString(fmt.Sprintf("[обрезано: %d знаков из %d]", len(runes)-limit, len(runes)))
			out.WriteByte('\n')
			cursor = end
		}
		if left > 0 {
			out.WriteString(string(runes[:left]))
			cursor = left
		}
		for _, i := range picked {
			item := spans[i]
			start, end := item.start, item.end
			if start < left {
				start = left
			}
			if end > right {
				end = right
			}
			if end <= start || end <= cursor {
				continue
			}
			if start > cursor {
				writeGap(start)
			}
			if cursor > 0 && out.Len() > 0 && !strings.HasSuffix(out.String(), "\n") {
				out.WriteByte('\n')
			}
			out.WriteString(string(runes[max(start, cursor):end]))
			cursor = end
		}
		if cursor < right {
			writeGap(right)
		}
		if right < len(runes) {
			if out.Len() > 0 {
				out.WriteByte('\n')
			}
			out.WriteString(string(runes[right:]))
		}
		return out.String()
	}
	render := func(selected []bool, left, right int) string {
		picked := make([]int, 0)
		for i, keep := range selected {
			if keep {
				picked = append(picked, i)
			}
		}
		return renderPicked(picked, left, right)
	}
	selected := make([]bool, len(spans))
	remaining := make(map[string]bool, len(spans))
	positionWords := make([]string, 0, len(positions))
	for _, item := range spans {
		remaining[item.words[0]] = true
		positionWords = append(positionWords, item.words[0])
	}
	sort.Slice(positionWords, func(i, j int) bool { return positions[positionWords[i]] < positions[positionWords[j]] })
	current := ""
	picked := make([]int, 0)
	for {
		best, bestScore, bestText, bestCost := -1, 0, "", 0
		currentLen := utf8.RuneCountInString(current)
		for i, item := range spans {
			if selected[i] || !remaining[item.words[0]] {
				continue
			}
			selected[i] = true
			trialPicked := append(append([]int(nil), picked...), i)
			sort.Ints(trialPicked)
			trial := renderPicked(trialPicked, 0, len(runes))
			selected[i] = false
			cost := utf8.RuneCountInString(trial) - currentLen
			if cost <= 0 {
				cost = 1
			}
			if utf8.RuneCountInString(trial) > limit {
				continue
			}
			score := 0
			first := sort.Search(len(positionWords), func(j int) bool { return positions[positionWords[j]] >= item.start })
			for j := first; j < len(positionWords) && positions[positionWords[j]] < item.end; j++ {
				if remaining[positionWords[j]] {
					score++
				}
			}
			if score*bestCost > bestScore*cost || score*bestCost == bestScore*cost && score > 0 && (best < 0 || i < best) {
				best, bestScore, bestText, bestCost = i, score, trial, cost
			}
		}
		if best < 0 {
			break
		}
		selected[best] = true
		picked = append(picked, best)
		sort.Ints(picked)
		current = bestText
		first := sort.Search(len(positionWords), func(j int) bool { return positions[positionWords[j]] >= spans[best].start })
		for j := first; j < len(positionWords) && positions[positionWords[j]] < spans[best].end; j++ {
			delete(remaining, positionWords[j])
		}
	}
	// Once evidence snippets have priority, use spare space for the edges.
	edgeStart, edgeEnd := 0, len(runes)
	for _, fromStart := range []bool{true, false} {
		low, high, best := 0, limit, 0
		if fromStart {
			for i, item := range spans {
				if selected[i] {
					high = min(high, item.start)
				}
			}
		} else {
			for i, item := range spans {
				if selected[i] {
					high = min(high, len(runes)-item.end)
				}
			}
		}
		for low <= high {
			middle := (low + high) / 2
			left, right := edgeStart, edgeEnd
			if fromStart {
				left = middle
			} else {
				right = len(runes) - middle
			}
			trial := render(selected, left, right)
			if utf8.RuneCountInString(trial) <= limit {
				best = middle
				low = middle + 1
			} else {
				high = middle - 1
			}
		}
		if fromStart {
			edgeStart = best
		} else {
			edgeEnd = len(runes) - best
		}
		current = render(selected, edgeStart, edgeEnd)
	}
	selectedAny := false
	for _, keep := range selected {
		selectedAny = selectedAny || keep
	}
	if !selectedAny {
		return excerpt(s, limit)
	}
	if utf8.RuneCountInString(current) > limit {
		return excerpt(current, limit)
	}
	return current
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
			found, _ := matcher.wordsInLine(context.Background(), line)
			return intersectWords(found, words)
		}
		matcher = newWordMatcher(words)
	}
	found, _ := matcher.wordsInLine(context.Background(), line)
	return found
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
	for word, position := range bytePositions {
		positions[word] = utf8.RuneCountInString(lower[:position])
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
	words := make([]string, 0, len(seen))
	for word := range seen {
		words = append(words, word)
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
