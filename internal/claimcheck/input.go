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
	fullInput  string
	fullOutput string
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
type Options struct{ ContextLimit int }

const (
	DefaultContextLimit    = 6000
	turnToolsCharLimit     = 6000
	toolInputCharLimit     = 300
	toolOutputCharLimit    = 1600
	lastTurnToolsKept      = 5
	skippedToolName        = "[пропущено]"
	contextOutputCharLimit = 1000
	maxContextToolCalls    = 2
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
	input.AnswerTime = prefixTime(rows, line, input.AnswerTime)
	if err := appendTurn(input, rows, start, line, answer); err != nil {
		return nil, err
	}
	limit := options.ContextLimit
	if limit <= 0 {
		limit = DefaultContextLimit
	}
	if err := appendContext(input, rows, start, answer, limit); err != nil {
		return nil, err
	}
	return input, nil
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

func appendTurn(out *Input, rows []row, start, line int, answer string) error {
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
			if err := appendCallTools(out, r, rows, i, line, words); err != nil {
				return err
			}
		}
	}
	tools, err := boundTools(out.TurnTools, answer, turnToolsCharLimit)
	if err != nil {
		return err
	}
	out.TurnTools = tools
	return nil
}

func appendCallTools(out *Input, r *row, rows []row, index, line int, words []string) error {
	for j := range r.Blocks {
		b := &r.Blocks[j]
		if b.Type != "tool_use" {
			continue
		}
		t, err := toolFromCall(r, b, rows, index, line, words)
		if err != nil {
			return err
		}
		out.TurnTools = append(out.TurnTools, t)
	}
	return nil
}

func appendContext(out *Input, rows []row, start int, answer string, limit int) error {
	priorCalls := make(map[string]*block)
	for i := range start {
		r := &rows[i]
		if r.E.Type == "assistant" {
			text := messageText(r.Blocks)
			if text != "" {
				out.Context.PreviousAnswer = mask(text)
			}
			if err := addPriorCalls(out, r, rows, i, priorCalls); err != nil {
				return err
			}
		}
	}
	if len(out.Context.RecentTools) > maxContextToolCalls {
		out.Context.RecentTools = out.Context.RecentTools[len(out.Context.RecentTools)-maxContextToolCalls:]
	}
	if err := appendMatchingTools(out, rows, start, priorCalls, answer, limit); err != nil {
		return err
	}
	return boundContext(&out.Context, limit)
}

func addPriorCalls(out *Input, r *row, rows []row, index int, calls map[string]*block) error {
	for j := range r.Blocks {
		b := &r.Blocks[j]
		if b.Type != "tool_use" {
			continue
		}
		calls[b.ID] = b
		tool, err := toolFromCall(r, b, rows, index, r.Line, nil)
		if err != nil {
			return err
		}
		out.Context.RecentTools = append(out.Context.RecentTools, tool)
	}
	return nil
}

func appendMatchingTools(out *Input, rows []row, start int, calls map[string]*block, answer string, limit int) error {
	words := answerWords(answer)
	contextChars := utf8.RuneCountInString(out.Context.PreviousAnswer)
	for i := range start {
		r := &rows[i]
		for j := range r.Blocks {
			b := &r.Blocks[j]
			tool, text, ok, err := matchingTool(r, b, calls, words, limit-contextChars)
			if err != nil {
				return err
			}
			if ok {
				out.Context.MatchingTools = append(out.Context.MatchingTools, tool)
				contextChars += utf8.RuneCountInString(text)
			}
		}
	}
	return nil
}

func matchingTool(r *row, b *block, calls map[string]*block, words []string, budget int) (Tool, string, bool, error) {
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
	tool := Tool{Time: r.E.Timestamp, Name: name, Input: input, OutputLength: utf8.RuneCountInString(text), Output: mask(excerpt(text, min(contextOutputCharLimit, budget)))}
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
func toolFromCall(r *row, b *block, rows []row, index, end int, words []string) (Tool, error) {
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
	return Tool{Time: r.E.Timestamp, Name: b.Name, Input: excerpt(maskedInput, toolInputCharLimit), OutputLength: length, Output: excerpt(maskedOutput, toolOutputCharLimit), ExitCode: code, IsError: iserr, relevant: matches(input, words) || matches(out, words), fullInput: maskedInput, fullOutput: maskedOutput}, nil
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
// before the answer are its freshest evidence, so at least lastTurnToolsKept of
// them always stay, cropped into half the budget. The rest of the budget goes to
// calls matching the answer, then to earlier recent calls, then to the first ones.
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
	for i := last - 1; i >= 0; i-- {
		if err := keep(i, reserve); err != nil {
			return nil, err
		}
	}
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
		tool.Input = excerpt(tool.fullInput, inputLimit)
		tool.Output = excerpt(tool.fullOutput, outputLimit)
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
