// Package claimcheck builds the context shown to the claim-check judge from
// the Claude Code transcript as it existed at a particular event.
package claimcheck

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
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
	relevant     bool
	// fullInput and fullOutput are the masked, uncropped texts: every refit crops
	// from them, so the crop marker counts the real length.
	fullInput   string
	fullOutput  string
	answerWords []string
}

func (t Tool) MarshalJSON() ([]byte, error) {
	if t.Skipped > 0 {
		return json.Marshal(struct {
			Name    string `json:"name"`
			Skipped int    `json:"skipped"`
		}{Name: t.Name, Skipped: t.Skipped})
	}
	type toolAlias Tool
	return json.Marshal(toolAlias(t))
}

type Context struct {
	RecentTools    []Tool `json:"recentTools"`
	PreviousAnswer string `json:"previousAnswer,omitempty"`
	MatchingTools  []Tool `json:"matchingTools,omitempty"`
}
type Options struct {
	ContextLimit int
	Unbounded    bool
}

const (
	DefaultContextLimit        = 21000
	turnToolsCharLimit         = 20000
	shortAnswerTurnToolsLimit  = 11500
	longAnswerContextLimit     = 10000
	largeAnswerWordThreshold   = 250
	shortAnswerInputCharLimit  = 24500
	toolInputCharLimit         = 300
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
	if line < 1 {
		return nil, fmt.Errorf("line must be positive")
	}
	rows, err := readRows(path, line)
	if err != nil {
		return nil, err
	}
	start, err := turnStart(rows, line)
	if err != nil {
		return nil, err
	}
	kind, answer, target, err := decision(rows, line, start)
	if err != nil {
		return nil, err
	}
	input := newInput(kind, answer, target.E.Timestamp)
	words := answerWords(answer)
	input.AnswerTime = prefixTime(rows, line, input.AnswerTime)
	turnLimit := turnToolsCharLimit
	limit := options.ContextLimit
	if limit <= 0 {
		limit = DefaultContextLimit
	}
	if len(words) <= largeAnswerWordThreshold {
		turnLimit = shortAnswerTurnToolsLimit
	} else {
		limit = min(limit, longAnswerContextLimit)
	}
	if err := appendTurn(input, rows, start, line, answer, options.Unbounded, turnLimit); err != nil {
		return nil, err
	}
	if err := appendContext(input, rows, start, answer, limit, options.Unbounded); err != nil {
		return nil, err
	}
	if !options.Unbounded && len(words) <= largeAnswerWordThreshold {
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
			if err := appendContext(input, rows, start, answer, limit, false); err != nil {
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
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open transcript: %w", err)
	}
	defer f.Close()
	var rows []row
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 32*1024*1024)
	for n := 1; scanner.Scan(); n++ {
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

func turnStart(rows []row, line int) (int, error) {
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
		begins, err := beginsTurn(&rows[i])
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

func appendTurn(out *Input, rows []row, start, line int, answer string, unbounded bool, limit int) error {
	words := answerWords(answer)
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
		r := &rows[i]
		if r.Line < line && r.E.Timestamp != "" {
			out.LastEventTime = r.E.Timestamp
		}
		if r.E.Type == "assistant" {
			if err := appendCallTools(out, r, rows, i, line, words, unbounded); err != nil {
				return err
			}
		}
	}
	if !unbounded {
		tools, err := boundTools(out.TurnTools, answer, limit)
		if err != nil {
			return err
		}
		out.TurnTools = tools
	}
	return nil
}

func appendCallTools(out *Input, r *row, rows []row, index, line int, words []string, unbounded bool) error {
	for j := range r.Blocks {
		b := &r.Blocks[j]
		if b.Type != "tool_use" {
			continue
		}
		t, err := toolFromCall(r, b, rows, index, line, words, unbounded)
		if err != nil {
			return err
		}
		out.TurnTools = append(out.TurnTools, t)
	}
	return nil
}

func appendContext(out *Input, rows []row, start int, answer string, limit int, unbounded bool) error {
	priorCalls := make(map[string]*block)
	words := answerWords(answer)
	for i := range start {
		r := &rows[i]
		if r.E.Type == "assistant" {
			text := messageText(r.Blocks)
			if text != "" {
				out.Context.PreviousAnswer = mask(text)
			}
			if err := addPriorCalls(out, r, rows, i, priorCalls, words, unbounded); err != nil {
				return err
			}
		}
	}
	if len(out.Context.RecentTools) > maxContextToolCalls {
		out.Context.RecentTools = out.Context.RecentTools[len(out.Context.RecentTools)-maxContextToolCalls:]
	}
	if !unbounded {
		out.Context.PreviousAnswer = excerpt(out.Context.PreviousAnswer, contextPreviousAnswerLimit)
	}
	if err := appendMatchingTools(out, rows, start, priorCalls, answer, limit, unbounded); err != nil {
		return err
	}
	if !unbounded {
		return boundContext(&out.Context, limit)
	}
	return nil
}

func addPriorCalls(out *Input, r *row, rows []row, index int, calls map[string]*block, words []string, unbounded bool) error {
	for j := range r.Blocks {
		b := &r.Blocks[j]
		if b.Type != "tool_use" {
			continue
		}
		calls[b.ID] = b
		tool, err := toolFromCall(r, b, rows, index, r.Line, words, unbounded)
		if err != nil {
			return err
		}
		if !unbounded {
			tool.Output = excerptWithWords(tool.fullOutput, words, contextRecentOutputLimit)
		}
		out.Context.RecentTools = append(out.Context.RecentTools, tool)
	}
	return nil
}

func appendMatchingTools(out *Input, rows []row, start int, calls map[string]*block, answer string, limit int, unbounded bool) error {
	words := answerWords(answer)
	var candidates []Tool
	for i := range start {
		r := &rows[i]
		for j := range r.Blocks {
			b := &r.Blocks[j]
			tool, _, ok, err := matchingTool(r, b, calls, words, int(^uint(0)>>1), true)
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
	selected := make([]bool, len(candidates))
	remaining := make(map[string]bool, len(words))
	for _, word := range words {
		remaining[word] = true
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
		for i := range candidates {
			if selected[i] {
				continue
			}
			candidate := candidates[i]
			candidate.answerWords = unseenWords(words, remaining)
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
	inputLimit, outputLimit := toolInputCharLimit, contextOutputCharLimit
	for {
		tool.Input = excerptWithWords(tool.fullInput, tool.answerWords, inputLimit)
		tool.Output = excerptWithWords(tool.fullOutput, tool.answerWords, outputLimit)
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

func matchingTool(r *row, b *block, calls map[string]*block, words []string, budget int, unbounded bool) (Tool, string, bool, error) {
	if b.Type != "tool_result" || budget <= 0 {
		return Tool{}, "", false, nil
	}
	text, err := plain(b.Content)
	if err != nil {
		return Tool{}, "", false, err
	}
	if !matches(text, words) {
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
	tool := Tool{Time: r.E.Timestamp, Name: name, Input: input, OutputLength: utf8.RuneCountInString(text), Output: mask(excerptWithWords(text, words, outputLimit)), fullInput: input, fullOutput: mask(text), answerWords: words}
	return tool, tool.Output, true, nil
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
		return beginsAttachmentTurn(r), nil
	}
	if r.E.Type != "user" {
		return false, nil
	}
	return beginsUserTurn(r)
}

func beginsAttachmentTurn(r *row) bool {
	kind := r.E.Attachment.Origin.Kind
	return r.E.Attachment.CommandMode == "task-notification" || (r.E.Attachment.Type == "queued_command" && (kind == "human" || kind == "peer" || kind == "task-notification"))
}

func beginsUserTurn(r *row) (bool, error) {
	if r.E.Origin.Kind == "peer" || r.E.Origin.Kind == "task-notification" {
		return true, nil
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
func toolFromCall(r *row, b *block, rows []row, index, end int, words []string, unbounded bool) (Tool, error) {
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
	for j := index + 1; j < len(rows) && rows[j].Line <= end; j++ {
		for k := range rows[j].Blocks {
			rb := &rows[j].Blocks[k]
			if rb.Type == "tool_result" && (rb.ToolUseID == b.ID || b.ID == "") {
				var err error
				out, err = plain(rb.Content)
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
				break
			}
		}
	}
	// Mask before cropping: a crop could split a secret so the mask no longer recognises it.
	maskedInput, maskedOutput := mask(input), mask(out)
	inputLimit, outputLimit := toolInputCharLimit, toolOutputCharLimit
	if unbounded {
		inputLimit, outputLimit = len([]rune(maskedInput)), len([]rune(maskedOutput))
	}
	return Tool{Time: r.E.Timestamp, Name: b.Name, Input: excerptWithWords(maskedInput, words, inputLimit), OutputLength: length, Output: excerptWithWords(maskedOutput, words, outputLimit), ExitCode: code, IsError: iserr, relevant: matches(input, words) || matches(out, words), fullInput: maskedInput, fullOutput: maskedOutput, answerWords: words}, nil
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
	if len(tools) == 0 {
		return []Tool{}, nil
	}
	words := answerWords(answer)
	bounded := make([]Tool, len(tools))
	copy(bounded, tools)
	for i := range bounded {
		t := &bounded[i]
		t.relevant = t.relevant || matches(t.Input, words) || matches(t.Output, words)
		t.answerWords = words
	}
	selected := make([]bool, len(bounded))
	reserve := budget / 2
	last := max(len(bounded)-lastTurnToolsKept, 0)
	if err := keepLastTools(bounded, selected, last, reserve); err != nil {
		return nil, err
	}
	keep := func(i, limit int) error {
		if selected[i] {
			return nil
		}
		size, err := toolsJSONSize(toolsWithSkipped(bounded, selected))
		if err != nil {
			return err
		}
		// Keeping a call inside a gap at worst splits one skip marker into two:
		// the call, one more marker and two commas. Counts only shrink otherwise.
		marker, err := skippedMarkerSize(len(bounded))
		if err != nil {
			return err
		}
		fit, ok, err := fitTool(bounded[i], limit-size-2-marker)
		if err != nil || !ok {
			return err
		}
		bounded[i], selected[i] = fit, true
		return nil
	}
	remaining := make(map[string]bool, len(words))
	for _, word := range words {
		remaining[word] = true
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
		for i := range bounded {
			if selected[i] || attempted[i] || !bounded[i].relevant {
				continue
			}
			candidate := bounded[i]
			candidate.answerWords = unseenWords(words, remaining)
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
	// Keep any remaining answer-matching calls while there is room, then fill
	// with earlier calls. Selection order does not affect transcript order.
	for i := range bounded {
		if bounded[i].relevant {
			if err := keep(i, budget); err != nil {
				return nil, err
			}
		}
	}
	for i := last - 1; i >= 0; i-- {
		if err := keep(i, budget); err != nil {
			return nil, err
		}
	}
	for i := range bounded {
		if err := keep(i, budget); err != nil {
			return nil, err
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
	var found []string
	for _, word := range words {
		if strings.Contains(input, word) || strings.Contains(output, word) {
			found = append(found, word)
		}
	}
	return found
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
	inputLimit, outputLimit := toolInputCharLimit, toolOutputCharLimit
	for {
		tool.Input = excerptWithWords(tool.fullInput, tool.answerWords, inputLimit)
		tool.Output = excerptWithWords(tool.fullOutput, tool.answerWords, outputLimit)
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
		if c.PreviousAnswer != "" {
			r := []rune(c.PreviousAnswer)
			c.PreviousAnswer = string(r[:len(r)/2])
		} else if n := len(c.MatchingTools); n > 0 && c.MatchingTools[n-1].Output != "" {
			r := []rune(c.MatchingTools[n-1].Output)
			c.MatchingTools[n-1].Output = string(r[:len(r)/2])
		} else if n := len(c.MatchingTools); n > 0 && c.MatchingTools[n-1].Input != "" {
			r := []rune(c.MatchingTools[n-1].Input)
			c.MatchingTools[n-1].Input = string(r[:len(r)/2])
		} else if n := len(c.RecentTools); n > 0 && c.RecentTools[n-1].Input != "" {
			r := []rune(c.RecentTools[n-1].Input)
			c.RecentTools[n-1].Input = string(r[:len(r)/2])
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
	lines := strings.Split(s, "\n")
	// Tool inputs are JSON. Preserve line-aware evidence inside JSON strings
	// whose newlines are represented by the two literal characters \\ and n.
	if len(lines) == 1 && strings.Contains(s, `\n`) {
		lines = strings.Split(s, `\n`)
	}
	if len(lines) <= 1 || limit <= 0 {
		if len(lines) == 1 && limit > 0 && utf8.RuneCountInString(s) > limit && matches(s, words) {
			return excerptAroundWords(s, words, limit)
		}
		return excerpt(s, limit)
	}
	for i, line := range lines {
		if utf8.RuneCountInString(line) > limit && matches(line, words) {
			lines[i] = excerptAroundWords(line, words, max(limit-80, 1))
		}
	}
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	evidence := make([]bool, len(lines))
	for i, line := range lines {
		if matches(line, words) {
			for j := max(0, i-1); j <= min(len(lines)-1, i+1); j++ {
				evidence[j] = true
			}
		}
	}
	selected := make([]bool, len(lines))
	for i := range evidence {
		selected[i] = evidence[i]
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
	// If evidence alone exceeds the cap, keep the line windows that add the most
	// not-yet-visible answer words. This avoids spending the whole excerpt on an
	// early cluster when later lines contain different evidence.
	if utf8.RuneCountInString(render()) > limit {
		for i := range selected {
			selected[i] = false
		}
		remaining := make(map[string]bool, len(words))
		for _, word := range words {
			remaining[word] = true
		}
		lineWords := make([][]string, len(lines))
		for i, line := range lines {
			lower := strings.ToLower(line)
			for _, word := range words {
				if strings.Contains(lower, word) {
					lineWords[i] = append(lineWords[i], word)
				}
			}
		}
		windowWords := make([][]string, len(lines))
		lineLengths := make([]int, len(lines))
		for i := range lines {
			lineLengths[i] = utf8.RuneCountInString(lines[i])
			seen := make(map[string]bool)
			for j := max(0, i-1); j <= min(len(lines)-1, i+1); j++ {
				for _, word := range lineWords[j] {
					if !seen[word] {
						seen[word] = true
						windowWords[i] = append(windowWords[i], word)
					}
				}
			}
		}
		attempted := make([]bool, len(lines))
		for {
			best, bestScore, bestCost := -1, 0, 0
			for i := range lines {
				if attempted[i] || len(lineWords[i]) == 0 {
					continue
				}
				score := 0
				for _, word := range windowWords[i] {
					if remaining[word] {
						score++
					}
				}
				cost := 0
				for j := max(0, i-1); j <= min(len(lines)-1, i+1); j++ {
					if !selected[j] {
						cost += lineLengths[j] + 1
					}
				}
				if cost < 1 {
					cost = 1
				}
				if score*bestCost > bestScore*cost || score*bestCost == bestScore*cost && score > bestScore {
					best, bestScore, bestCost = i, score, cost
				}
			}
			if best < 0 {
				break
			}
			attempted[best] = true
			from, to := max(0, best-1), min(len(lines)-1, best+1)
			var added []int
			for j := from; j <= to; j++ {
				if !selected[j] {
					selected[j] = true
					added = append(added, j)
				}
			}
			if utf8.RuneCountInString(render()) > limit {
				for _, j := range added {
					selected[j] = false
				}
				if !selected[best] {
					selected[best] = true
					if utf8.RuneCountInString(render()) > limit {
						selected[best] = false
						continue
					}
				}
			}
			for word := range remaining {
				for j := from; j <= to; j++ {
					if selected[j] && strings.Contains(strings.ToLower(lines[j]), word) {
						delete(remaining, word)
						break
					}
				}
			}
		}
	}
	// Boundary context is lower priority than matching evidence.
	for _, i := range []int{0, len(lines) - 1} {
		if !selected[i] {
			selected[i] = true
			if utf8.RuneCountInString(render()) > limit {
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

func excerptAroundWords(s string, words []string, limit int) string {
	runes := []rune(s)
	lower := strings.ToLower(s)
	type span struct {
		start, end int
		words      []string
	}
	var spans []span
	positions := make(map[string]int, len(words))
	for _, word := range words {
		if _, exists := positions[word]; exists {
			continue
		}
		if before, _, ok := strings.Cut(lower, word); ok {
			position := utf8.RuneCountInString(before)
			positions[word] = position
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
	render := func(selected []bool, left, right int) string {
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
		for i, item := range spans {
			if !selected[i] {
				continue
			}
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
	selected := make([]bool, len(spans))
	remaining := make(map[string]bool, len(spans))
	for _, item := range spans {
		remaining[item.words[0]] = true
	}
	current := ""
	for {
		best, bestScore, bestText, bestCost := -1, 0, "", 0
		currentLen := utf8.RuneCountInString(current)
		for i, item := range spans {
			if selected[i] || !remaining[item.words[0]] {
				continue
			}
			selected[i] = true
			trial := render(selected, 0, len(runes))
			selected[i] = false
			cost := utf8.RuneCountInString(trial) - currentLen
			if cost <= 0 {
				cost = 1
			}
			if utf8.RuneCountInString(trial) > limit {
				continue
			}
			score := 0
			for word, position := range positions {
				if remaining[word] && position >= item.start && position < item.end {
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
		current = bestText
		for word, position := range positions {
			if position >= spans[best].start && position < spans[best].end {
				delete(remaining, word)
			}
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
	for _, w := range wordRE.FindAllString(strings.ToLower(s), -1) {
		if len([]rune(w)) >= 3 {
			a = append(a, w)
		}
	}
	return a
}
func matches(s string, words []string) bool {
	v := strings.ToLower(s)
	for _, w := range words {
		if strings.Contains(v, w) {
			return true
		}
	}
	return false
}
