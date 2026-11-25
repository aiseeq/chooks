package claimcheck

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/aiseeq/chooks/internal/questioncheck"
)

type bodyJudge struct{ body string }

func (j *bodyJudge) Model() string { return "stub" }
func (j *bodyJudge) Ask(_ context.Context, _ string, body string) (string, questioncheck.Usage, error) {
	j.body = body
	return `{"final":"","sourced":false,"marked":false,"evidence":""}`, questioncheck.Usage{}, nil
}

func TestV6StopJudgeInput(t *testing.T) {
	j := &bodyJudge{}
	h := testHook(t, j)
	h.Prompt = "v6"
	event := stopEvent(t)
	if got := h.Stop(context.Background(), event); got.Result != OutcomePass {
		t.Fatalf("%+v", got)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal([]byte(j.body), &body); err != nil {
		t.Fatal(err)
	}
	var answer string
	var calls []map[string]json.RawMessage
	if err := json.Unmarshal(body["answer"], &answer); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body["turnTools"], &calls); err != nil {
		t.Fatal(err)
	}
	if len(body) != 3 || body["userPrompt"] == nil || answer != event.LastMessage || len(calls) != 1 || calls[0]["output"] != nil || calls[0]["input"] == nil {
		t.Fatalf("judge body: %s", j.body)
	}
}

func TestV6Stop(t *testing.T) {
	for _, tc := range []struct {
		name, answer, outcome string
		block                 bool
	}{
		{"empty", `{"final":"","sourced":false,"marked":false,"evidence":""}`, OutcomePass, false},
		{"sourced", `{"final":"X","sourced":true,"marked":false,"evidence":"docs"}`, OutcomePass, false},
		{"marked", `{"final":"X","sourced":false,"marked":true,"evidence":"docs"}`, OutcomePass, false},
		{"block", `{"final":"X","sourced":false,"marked":false,"evidence":"docs"}`, OutcomeBlock, true},
		{"missing", `{"final":"X","sourced":false}`, OutcomeError, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := &stubJudge{answer: func(string) (string, error) { return tc.answer, nil }}
			h := testHook(t, j)
			h.Prompt = "v6"
			got := h.Stop(context.Background(), stopEvent(t))
			if got.Result != tc.outcome || got.Block != tc.block {
				t.Fatalf("%+v", got)
			}
			if tc.block && (!strings.Contains(got.Reason, "X") || !strings.Contains(got.Reason, "docs")) {
				t.Fatal(got.Reason)
			}
			e := journal(t, h)
			if len(e) == 0 || e[len(e)-1].Prompt != "v6" {
				t.Fatalf("journal %+v", e)
			}
			if tc.outcome != OutcomeError && (e[len(e)-1].Final == nil || e[len(e)-1].Sourced == nil || e[len(e)-1].Marked == nil || e[len(e)-1].Evidence == nil) {
				t.Fatalf("journal %+v", e)
			}
		})
	}
}

func TestV6SendSkipped(t *testing.T) {
	j := &stubJudge{answer: answerOf(flagClaim)}
	h := testHook(t, j)
	h.Prompt = "v6"
	if got := h.Send(context.Background(), SendEvent{Text: "письмо"}); got.Result != OutcomeSkip || j.calls.Load() != 0 {
		t.Fatalf("%+v", got)
	}
}

func TestCallsOnlyInput(t *testing.T) {
	p := fixture(t, ev("user", "t1", "вопрос"), ev("assistant", "t2", []any{map[string]any{"type": "text", "text": "черновик"}, map[string]any{"type": "tool_use", "id": "a", "name": "Bash", "input": map[string]any{"command": "check docs"}}}), map[string]any{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "a", "content": "secret output"}}}}, ev("assistant", "t4", []any{map[string]any{"type": "text", "text": "последний ответ"}}))
	in, err := BuildInput(p, 4, Options{CallsOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err = json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if len(m) != 3 || in.Answer != "последний ответ" || strings.Contains(string(b), "output") || !strings.Contains(string(b), "check docs") {
		t.Fatalf("%s", b)
	}
}

func TestTurnBoundaryDeliveries(t *testing.T) {
	call := func(id string) any {
		return ev("assistant", "t", []any{map[string]any{"type": "tool_use", "id": id, "name": "Bash", "input": map[string]any{"command": id}}})
	}
	final := ev("assistant", "t", []any{map[string]any{"type": "text", "text": "ответ"}})
	notification := map[string]any{"type": "user", "origin": map[string]any{"kind": "task-notification"}, "message": map[string]any{"content": "уведомление"}}
	peerAttachment := map[string]any{"type": "attachment", "attachment": map[string]any{"type": "queued_command", "origin": map[string]any{"kind": "peer"}, "prompt": "письмо"}}
	notificationAttachment := map[string]any{"type": "attachment", "attachment": map[string]any{"type": "queued_command", "commandMode": "task-notification", "origin": map[string]any{"kind": "task-notification"}, "prompt": "уведомление"}}
	humanAttachment := map[string]any{"type": "attachment", "attachment": map[string]any{"type": "queued_command", "origin": map[string]any{"kind": "human"}, "prompt": "новый вопрос"}}
	duration := map[string]any{"type": "system", "subtype": "turn_duration"}
	for _, tc := range []struct {
		name   string
		events []any
		prompt string
		tools  []string
	}{
		{"notification", []any{ev("user", "t", "вопрос"), call("before"), duration, notification, call("after"), final}, "вопрос", []string{"before", "after"}},
		{"notification attachment", []any{ev("user", "t", "вопрос"), call("before"), notificationAttachment, call("after"), final}, "вопрос", []string{"before", "after"}},
		{"peer attachment during turn", []any{ev("user", "t", "вопрос"), call("before"), peerAttachment, call("after"), final}, "вопрос", []string{"before", "after"}},
		{"peer attachment after human reply during turn", []any{duration, ev("user", "t", "человеческая реплика"), call("before"), peerAttachment, call("after"), final}, "человеческая реплика", []string{"before", "after"}},
		{"peer attachment after turn", []any{ev("user", "t", "вопрос"), duration, peerAttachment, call("after"), final}, "письмо", []string{"after"}},
		{"peer attachment after peer user", []any{ev("user", "t", "вопрос"), duration, map[string]any{"type": "user", "origin": map[string]any{"kind": "peer"}, "message": map[string]any{"content": "первое письмо"}}, peerAttachment, call("after"), final}, "письмо", []string{"after"}},
		{"multiple queued human", []any{ev("user", "t", "вопрос"), call("before"), humanAttachment, call("middle"), map[string]any{"type": "attachment", "attachment": map[string]any{"type": "queued_command", "origin": map[string]any{"kind": "human"}, "prompt": "последний вопрос"}}, call("after"), final}, "последний вопрос", []string{"middle", "after"}},
		{"compact after prompt", []any{ev("user", "t", "вопрос"), call("before"), map[string]any{"type": "system", "subtype": "compact_boundary"}, call("after"), final}, "вопрос", []string{"after"}},
		{"queued human", []any{ev("system", "t", ""), ev("user", "t", "вопрос"), call("before"), peerAttachment, humanAttachment, call("after"), final}, "новый вопрос", []string{"after"}},
		{"peer user", []any{ev("user", "t", "вопрос"), call("before"), map[string]any{"type": "user", "origin": map[string]any{"kind": "peer"}, "message": map[string]any{"content": "письмо"}}, call("after"), final}, "письмо", []string{"after"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := fixture(t, tc.events...)
			for _, opt := range []Options{{}, {CallsOnly: true}} {
				in, err := BuildInput(p, len(tc.events), opt)
				if err != nil {
					t.Fatal(err)
				}
				if in.UserPrompt == nil || in.UserPrompt.Text != tc.prompt || len(in.TurnTools) != len(tc.tools) {
					t.Fatalf("%+v", in)
				}
				for i, x := range tc.tools {
					if !strings.Contains(in.TurnTools[i].Input, x) {
						t.Fatalf("%+v", in.TurnTools)
					}
				}
			}
		})
	}
}

func TestQueuedPromptDoesNotCrossTurnEnd(t *testing.T) {
	duration := map[string]any{"type": "system", "subtype": "turn_duration"}
	queued := func(prompt string) any {
		return map[string]any{"type": "attachment", "attachment": map[string]any{"type": "queued_command", "origin": map[string]any{"kind": "human"}, "prompt": prompt}}
	}
	call := func(id string) any {
		return ev("assistant", "t", []any{map[string]any{"type": "tool_use", "id": id, "name": "Bash", "input": map[string]any{"command": id}}})
	}
	notice := map[string]any{"type": "user", "origin": map[string]any{"kind": "task-notification"}, "message": map[string]any{"content": "уведомление"}}
	final := ev("assistant", "t", []any{map[string]any{"type": "text", "text": "ответ"}})
	events := []any{queued("первый ход"), call("old-turn-call"), duration, notice, queued("новый ход"), call("new-turn-call"), final}
	in, err := BuildInput(fixture(t, events...), len(events), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if in.UserPrompt == nil || in.UserPrompt.Text != "новый ход" || len(in.TurnTools) != 1 || !strings.Contains(in.TurnTools[0].Input, "new-turn-call") {
		t.Fatalf("prompt=%+v calls=%+v", in.UserPrompt, in.TurnTools)
	}
}

func TestQueuedPromptResetsWhenUserRecordOpensTurn(t *testing.T) {
	queued := func(prompt string) any {
		return map[string]any{"type": "attachment", "attachment": map[string]any{"type": "queued_command", "origin": map[string]any{"kind": "human"}, "prompt": prompt}}
	}
	call := func(id string) any {
		return ev("assistant", "t", []any{map[string]any{"type": "tool_use", "id": id, "name": "Bash", "input": map[string]any{"command": id}}})
	}
	final := ev("assistant", "t", []any{map[string]any{"type": "text", "text": "ответ"}})
	events := []any{queued("устаревший prompt"), ev("user", "t", "реплика-запись"), call("before-new-prompt"), queued("новый prompt"), call("after-new-prompt"), final}
	in, err := BuildInput(fixture(t, events...), len(events), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if in.UserPrompt == nil || in.UserPrompt.Text != "новый prompt" || len(in.TurnTools) != 1 || !strings.Contains(in.TurnTools[0].Input, "after-new-prompt") {
		t.Fatalf("prompt=%+v calls=%+v", in.UserPrompt, in.TurnTools)
	}
}

func TestAnswerAndSendTurnEnd(t *testing.T) {
	duration := map[string]any{"type": "system", "subtype": "turn_duration"}
	text := func(s string) any { return ev("assistant", "t", []any{map[string]any{"type": "text", "text": s}}) }
	notice := map[string]any{"type": "user", "origin": map[string]any{"kind": "task-notification"}, "message": map[string]any{"content": "уведомление"}}
	p := fixture(t, ev("user", "t", "вопрос"), text("старый"), duration, notice, text("новый"))
	in, err := BuildInput(p, 5, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if in.Answer != "новый" || in.TurnLine != 4 || in.UserPrompt.Text != "вопрос" {
		t.Fatalf("%+v", in)
	}
}

func TestCallsOnlyKeepsCallsWhenOutputsExhaustOldBudget(t *testing.T) {
	events := []any{ev("user", "t", "проверка")}
	for i := range 60 {
		id := fmt.Sprintf("call-%d", i)
		events = append(events, ev("assistant", "t", []any{map[string]any{"type": "tool_use", "id": id, "name": "Bash", "input": map[string]any{"command": id + strings.Repeat("x", 180)}}}))
		events = append(events, map[string]any{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": id, "content": strings.Repeat("result", 1000)}}}})
	}
	events = append(events, ev("assistant", "t", []any{map[string]any{"type": "text", "text": "готово"}}))
	in, err := BuildInput(fixture(t, events...), len(events), Options{CallsOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(in.TurnTools) != 60 {
		t.Fatalf("calls=%d", len(in.TurnTools))
	}
	for _, call := range in.TurnTools {
		if call.Skipped > 0 {
			t.Fatalf("skipped %+v", call)
		}
	}
}

func TestCallsOnlyKeepsLongCommandAction(t *testing.T) {
	command := strings.Repeat("x", 640) + " git commit -m done\n" + strings.Repeat("y", 250)
	p := fixture(t, ev("user", "t", "сделай"), ev("assistant", "t", []any{map[string]any{"type": "tool_use", "id": "a", "name": "Bash", "input": map[string]any{"command": command}}}), ev("assistant", "t", []any{map[string]any{"type": "text", "text": "готово"}}))
	in, err := BuildInput(p, 3, Options{CallsOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(in.TurnTools) != 1 || !strings.Contains(in.TurnTools[0].Input, "git commit") {
		t.Fatalf("calls: %+v", in.TurnTools)
	}
}

func TestCallsOnlyCropsLongInputAndKeepsLaterCalls(t *testing.T) {
	events := []any{ev("user", "t", "проверь вызовы")}
	for i := range 24 {
		id := fmt.Sprintf("call-%02d", i)
		command := id + " " + strings.Repeat("x", 1200)
		events = append(events, ev("assistant", "t", []any{map[string]any{"type": "tool_use", "id": id, "name": "Bash", "input": map[string]any{"command": command}}}))
	}
	events = append(events, ev("assistant", "t", []any{map[string]any{"type": "text", "text": "готово"}}))
	in, err := BuildInput(fixture(t, events...), len(events), Options{CallsOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	var lastSeen bool
	for i := range in.TurnTools {
		call := &in.TurnTools[i]
		if strings.Contains(call.Input, "call-23") {
			lastSeen = true
		}
	}
	if !lastSeen {
		t.Fatalf("last call was lost after cropping an earlier call: %+v", in.TurnTools)
	}

	bounded, err := boundTools([]Tool{
		{Name: "Bash", callsOnly: true, fullInput: strings.Repeat("a", 9000)},
		{Name: "Bash", callsOnly: true, fullInput: strings.Repeat("b", 9000)},
	}, "", 2500)
	if err != nil {
		t.Fatal(err)
	}
	if len(bounded) != 2 {
		t.Fatalf("budget dropped a later call: %+v", bounded)
	}
	for i, call := range bounded {
		if !strings.Contains(call.Input, "[обрезано:") || len([]rune(call.Input)) <= toolInputCharLimit+100 || len([]rune(call.Input)) >= 1000+100 {
			t.Fatalf("call %d was not cropped within the calls-only limit: %+v", i, call)
		}
	}
}

func TestCallsOnlyInputLimitEvenWithAvailableBudget(t *testing.T) {
	command := strings.Repeat("x", 5000)
	events := []any{
		ev("user", "t", "проверь вызов"),
		ev("assistant", "t", []any{map[string]any{"type": "tool_use", "id": "long-call", "name": "Bash", "input": map[string]any{"command": command}}}),
		ev("assistant", "t", []any{map[string]any{"type": "text", "text": "готово"}}),
	}
	in, err := BuildInput(fixture(t, events...), len(events), Options{CallsOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(in.TurnTools) != 1 {
		t.Fatalf("calls=%+v", in.TurnTools)
	}
	input := in.TurnTools[0].Input
	marker := strings.Index(input, "[обрезано:")
	if marker < 0 {
		t.Fatalf("long calls-only input has no crop marker: %q", input)
	}
	var omitted, total int
	if _, err := fmt.Sscanf(input[marker:], "[обрезано: %d знаков из %d]", &omitted, &total); err != nil {
		t.Fatalf("cannot parse crop marker in %q: %v", input, err)
	}
	if total-omitted != 1000 {
		t.Fatalf("calls-only input retained %d source characters, want 1000: %q", total-omitted, input)
	}

	fitted, kept, err := fitTool(Tool{Name: "Bash", callsOnly: true, fullInput: strings.Repeat("x", 5000)}, 10000)
	if err != nil || !kept {
		t.Fatalf("fit long calls-only input with available budget: kept=%v err=%v", kept, err)
	}
	marker = strings.Index(fitted.Input, "[обрезано:")
	if marker < 0 {
		t.Fatalf("fitTool did not enforce the 1000 rune calls-only limit: %q", fitted.Input)
	}
	if _, err := fmt.Sscanf(fitted.Input[marker:], "[обрезано: %d знаков из %d]", &omitted, &total); err != nil {
		t.Fatalf("cannot parse fitTool crop marker in %q: %v", fitted.Input, err)
	}
	if total-omitted != 1000 {
		t.Fatalf("fitTool retained %d source characters, want 1000: %q", total-omitted, fitted.Input)
	}

	buildCall := func(command string) Tool {
		events := []any{
			ev("user", "t", "проверь точную границу"),
			ev("assistant", "t", []any{map[string]any{"type": "tool_use", "id": "boundary-call", "name": "Bash", "input": map[string]any{"command": command}}}),
			ev("assistant", "t", []any{map[string]any{"type": "text", "text": "готово"}}),
		}
		input, err := BuildInput(fixture(t, events...), len(events), Options{CallsOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(input.TurnTools) != 1 {
			t.Fatalf("boundary call missing: %+v", input.TurnTools)
		}
		return input.TurnTools[0]
	}

	exact := buildCall(strings.Repeat("x", 1000))
	if exact.Input != strings.Repeat("x", 1000) || strings.Contains(exact.Input, "[обрезано:") || exact.inputLines != nil {
		t.Fatalf("input at the 1000 rune limit was cropped or prepared for cropping: %+v", exact)
	}

	over := buildCall(strings.Repeat("x", 1001))
	if !strings.Contains(over.Input, "[обрезано:") || len([]rune(over.Input)) <= 1000 || over.inputLines == nil {
		t.Fatalf("input at 1001 runes was not prepared and cropped: %+v", over)
	}

	unicodeExactInput := strings.Repeat("я", 1000)
	unicodeExact := buildCall(unicodeExactInput)
	if unicodeExact.Input != unicodeExactInput || retainedInputRunes(unicodeExact) != 1000 {
		t.Fatalf("1000 Cyrillic runes were cropped: retained=%d input=%q", retainedInputRunes(unicodeExact), unicodeExact.Input)
	}
	unicodeOver := buildCall(strings.Repeat("я", 1001))
	if !strings.Contains(unicodeOver.Input, "[обрезано:") || retainedInputRunes(unicodeOver) != 1000 {
		t.Fatalf("1001 Cyrillic runes retained %d, want 1000: %q", retainedInputRunes(unicodeOver), unicodeOver.Input)
	}
	refit, ok, err := fitToolWithInputLimit(Tool{Name: "Bash", callsOnly: true, fullInput: strings.Repeat("я", 1001)}, 5000, callsOnlyInputCharLimit)
	if err != nil || !ok || retainedInputRunes(refit) != callsOnlyInputCharLimit {
		t.Fatalf("refit retained %d runes, want %d: ok=%v err=%v", retainedInputRunes(refit), callsOnlyInputCharLimit, ok, err)
	}
}

func TestCallsOnlyCropsToOneRuneWhenBudgetIsTight(t *testing.T) {
	tool := Tool{Name: "Bash", callsOnly: true, fullInput: strings.Repeat("x", 2000)}
	tool.Input = toolInputExcerpt(tool, 1)
	budget, err := toolsJSONSize([]Tool{tool})
	if err != nil {
		t.Fatal(err)
	}
	fit, kept, err := fitTool(Tool{Name: "Bash", callsOnly: true, fullInput: strings.Repeat("x", 2000)}, budget)
	if err != nil || !kept {
		t.Fatalf("fit one-rune calls-only input: kept=%v err=%v", kept, err)
	}
	if fit.Input != tool.Input || !strings.Contains(fit.Input, "[обрезано: 1999 знаков из 2000]") || !strings.HasSuffix(fit.Input, "x") {
		t.Fatalf("budget did not retain one input rune with a crop marker: %q", fit.Input)
	}
}

// Предел 1 не влезает (число в маркере теряет разряд при пределе 0), а один
// маркер влезает: вызов остаётся с маркером обрезки, а не выпадает
func TestCallsOnlyKeepsMarkerWhenOneRuneDoesNotFit(t *testing.T) {
	for _, n := range []int{1200, 1500} {
		markerOnly := Tool{Name: "Bash", callsOnly: true, fullInput: strings.Repeat("x", n)}
		markerOnly.Input = toolInputExcerpt(markerOnly, 0)
		budget, err := toolsJSONSize([]Tool{markerOnly})
		if err != nil {
			t.Fatal(err)
		}
		fit, kept, err := fitTool(Tool{Name: "Bash", callsOnly: true, fullInput: strings.Repeat("x", n)}, budget)
		if err != nil || !kept {
			t.Fatalf("n=%d budget=%d: kept=%v err=%v, want the call kept with a crop marker", n, budget, kept, err)
		}
		if fit.Input != markerOnly.Input || !strings.Contains(fit.Input, "[обрезано:") {
			t.Fatalf("n=%d: input %q, want marker only %q", n, fit.Input, markerOnly.Input)
		}
	}
}

func TestCallsOnlyInputExcerptBudgetDependsOnRelevance(t *testing.T) {
	for _, tc := range []struct {
		name     string
		input    string
		relevant bool
		limit    int
	}{
		{name: "relevant", input: strings.Repeat("x", 500) + " needle " + strings.Repeat("y", 500), relevant: true, limit: 1000},
		{name: "irrelevant", input: strings.Repeat("x", 1000), limit: 1000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool := Tool{
				Name: "Bash", Input: tc.input, fullInput: tc.input,
				callsOnly: true, relevant: tc.relevant,
				answerWords: []string{"needle"}, cropWords: []string{"needle"},
			}
			got, ok, err := fitTool(tool, 5000)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				t.Fatal("fitTool rejected call")
			}
			n := utf8.RuneCountInString(got.Input)
			if n > tc.limit {
				t.Fatalf("input excerpt has %d runes, limit %d", n, tc.limit)
			}
			if tc.relevant && (n <= 300 || !strings.Contains(got.Input, "needle")) {
				t.Fatalf("relevant excerpt lost its larger budget or answer word: %d runes, %q", n, got.Input)
			}
		})
	}
}

func TestCallsOnlyPreservesGitActionCalls(t *testing.T) {
	p := fixture(t,
		ev("user", "t1", "сделай"),
		ev("assistant", "t2", []any{map[string]any{"type": "tool_use", "id": "a", "name": "Bash", "input": map[string]any{"command": "git commit -q -m done && git commit --amend && git push"}}}),
		ev("assistant", "t3", []any{map[string]any{"type": "text", "text": "готово"}}),
	)
	in, err := BuildInput(p, 3, Options{CallsOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(in.TurnTools) != 1 || !strings.Contains(in.TurnTools[0].Input, "git commit -q") || !strings.Contains(in.TurnTools[0].Input, "git push") {
		t.Fatalf("short git action call was not preserved: %+v", in.TurnTools)
	}
}

func TestCallsOnlyBoundToolsPrioritizesRelevantExcerptAndKeepsShortInputWhole(t *testing.T) {
	answerWords := []string{"needle", "result"}
	longInput := strings.Repeat("x", 288) + strings.Repeat("y", 312)
	shortInput := strings.Repeat("short", 58)
	tools := []Tool{
		{Name: "Bash", callsOnly: true, fullInput: longInput, fullOutput: "needle result", answerWords: answerWords},
		{Name: "Bash", callsOnly: true, fullInput: shortInput, fullOutput: "result", answerWords: answerWords},
		{Name: "Bash", callsOnly: true, fullInput: strings.Repeat("z", 2000), answerWords: answerWords},
		{Name: "Bash", callsOnly: true, fullInput: strings.Repeat("a", 2000), answerWords: answerWords},
		{Name: "Bash", callsOnly: true, fullInput: strings.Repeat("b", 2000), answerWords: answerWords},
		{Name: "Bash", callsOnly: true, fullInput: strings.Repeat("c", 2000), answerWords: answerWords},
		{Name: "Bash", callsOnly: true, fullInput: strings.Repeat("d", 2000), answerWords: answerWords},
	}
	longFit, _, err := fitTool(tools[0], 10000)
	if err != nil {
		t.Fatal(err)
	}
	shortFit, _, err := fitTool(tools[1], 10000)
	if err != nil {
		t.Fatal(err)
	}
	minimumTail, err := toolsJSONSize([]Tool{{Name: "Bash"}, {Name: "Bash"}, {Name: "Bash"}, {Name: "Bash"}, {Name: "Bash"}})
	if err != nil {
		t.Fatal(err)
	}
	longSize, _ := toolsJSONSize([]Tool{longFit})
	shortSize, _ := toolsJSONSize([]Tool{shortFit})
	budget := longSize + shortSize + minimumTail + 700
	bounded, err := boundTools(tools, "needle result", budget)
	if err != nil {
		t.Fatal(err)
	}
	if bounded[0].Input != longInput {
		t.Fatalf("relevant call was cropped under tight budget: %q", bounded[0].Input)
	}
	if !bounded[0].relevant {
		t.Fatal("call relevant through its output lost its relevant flag")
	}
	if bounded[1].Input != shortInput || strings.Contains(bounded[1].Input, "[обрезано:") {
		t.Fatalf("short call was cropped: %q", bounded[1].Input)
	}
}

func TestCallsOnlyBoundToolsExpandsSelectedLongInput(t *testing.T) {
	tools := make([]Tool, 6)
	for i := range tools {
		tools[i] = Tool{Name: "Bash", callsOnly: true, fullInput: strings.Repeat(string(rune('a'+i)), 2000)}
	}
	bounded, err := boundTools(tools, "ответ", 8000)
	if err != nil {
		t.Fatal(err)
	}
	expanded := false
	for i := 1; i < len(bounded); i++ {
		if retainedInputRunes(bounded[i]) > 800 {
			expanded = true
		}
	}
	if !expanded {
		t.Fatalf("no selected call grew beyond the initial reserve share: %+v", bounded[1:])
	}
}

// The reviewer's probe: an early call with the answer word and 80 short calls
// after it, built through BuildInput like claim-check-input does.
func TestCallsOnlyPrioritizesRelevantLongCallOverManyShortCalls(t *testing.T) {
	for _, shortRelevant := range []bool{false, true} {
		t.Run(fmt.Sprintf("short_relevant_%v", shortRelevant), func(t *testing.T) {
			bash := func(id, command string) map[string]any {
				return ev("assistant", "2026-01-01T00:00:01Z", []any{map[string]any{"type": "tool_use", "id": id, "name": "Bash", "input": map[string]any{"command": command}}})
			}
			events := []any{ev("user", "2026-01-01T00:00:01Z", "проверь"), bash("source", "verify needle "+strings.Repeat("x", 1200))}
			for i := range 80 {
				command := "echo " + strings.Repeat("z", 270) + fmt.Sprint(i)
				if shortRelevant {
					command = "echo needle " + strings.Repeat("z", 263) + fmt.Sprint(i)
				}
				events = append(events, bash(fmt.Sprint(i), command))
			}
			events = append(events, ev("assistant", "2026-01-01T00:00:01Z", []any{map[string]any{"type": "text", "text": "needle найден"}}))
			in, err := BuildInput(fixture(t, events...), len(events), Options{CallsOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			var sources []Tool
			for _, call := range in.TurnTools {
				if strings.Contains(call.Input, "verify") {
					sources = append(sources, call)
				}
			}
			if len(sources) != 1 {
				t.Fatalf("relevant source calls in input: %d, want 1", len(sources))
			}
			source := sources[0]
			if !strings.Contains(source.Input, "verify needle") {
				t.Fatalf("source excerpt lost the answer word: %q", source.Input)
			}
			if !shortRelevant && retainedInputRunes(source) < callsOnlyInputCharLimit {
				t.Fatalf("source excerpt shorter than main's %d runes: retained=%d", callsOnlyInputCharLimit, retainedInputRunes(source))
			}
		})
	}
}

func TestCallsOnlyCountsOutputMatchesBeforeClearingOutput(t *testing.T) {
	blocks := make([]any, 0, 2)
	results := make([]any, 0, 2)
	for i := range 2 {
		id := fmt.Sprintf("call-%02d", i)
		blocks = append(blocks, map[string]any{
			"type": "tool_use", "id": id, "name": "Bash",
			"input": map[string]any{"command": id + " " + strings.Repeat("x", 1200)},
		})
		output := "needle"
		if i == 0 {
			output = strings.Repeat("needle ", 20)
		}
		results = append(results, map[string]any{"type": "tool_result", "tool_use_id": id, "content": output})
	}
	p := fixture(t,
		ev("user", "t1", "проверь needle"),
		ev("assistant", "t2", blocks),
		map[string]any{"type": "user", "message": map[string]any{"content": results}},
		ev("assistant", "t4", []any{map[string]any{"type": "text", "text": "needle подтверждён"}}),
	)
	in, err := BuildInput(p, 4, Options{CallsOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	first, second := -1, -1
	for i := range in.TurnTools {
		call := &in.TurnTools[i]
		if strings.Contains(call.Input, "call-00") {
			first = i
			if !call.evidenceScored || call.evidenceScore < 20 || call.fullOutput != "" {
				t.Fatalf("output score was not retained before clearing output: %+v", call)
			}
		}
		if strings.Contains(call.Input, "call-01") {
			second = i
		}
	}
	if first < 0 || second < 0 {
		t.Fatalf("priority calls missing: first=%d second=%d", first, second)
	}
	order := callsOnlyPriorityOrder(in.TurnTools)
	firstRank, secondRank := -1, -1
	for rank, index := range order {
		if index == first {
			firstRank = rank
		}
		if index == second {
			secondRank = rank
		}
	}
	if firstRank < 0 || secondRank < 0 || firstRank >= secondRank {
		t.Fatalf("call with more answer matches ranked after its peer: order=%v first=%d second=%d", order, firstRank, secondRank)
	}
}

func TestCallsOnlyExpansionKeepsLargerExcerptWhenFitIsSmaller(t *testing.T) {
	current := Tool{Name: "Bash", Input: strings.Repeat("x", 120)}
	fit := Tool{Name: "Bash", Input: strings.Repeat("я", 100)}
	got, replaced := keepLargerToolFit(current, fit)
	if replaced || got.Input != current.Input {
		t.Fatalf("smaller fit replaced the existing excerpt: replaced=%v input=%q", replaced, got.Input)
	}
}

// A literal crop marker inside the call's own input must not count as the
// marker of its excerpt: the larger excerpt below shows it, the smaller does not.
func TestCallsOnlyLiteralCropMarkerDoesNotChangeExcerptChoice(t *testing.T) {
	literal := "[обрезано: 0 знаков из 1]"
	call := Tool{Name: "Bash", callsOnly: true, fullInput: strings.Repeat("a", 120) + literal + strings.Repeat("b", 2000)}
	current, ok, err := fitToolWithInputLimit(call, 5000, 400)
	if err != nil || !ok || !strings.Contains(current.Input, literal) {
		t.Fatalf("larger excerpt lacks the literal marker: ok=%v err=%v input=%q", ok, err, current.Input)
	}
	smaller, ok, err := fitToolWithInputLimit(call, 5000, 200)
	if err != nil || !ok || strings.Contains(smaller.Input, literal) {
		t.Fatalf("smaller excerpt shows the literal marker: ok=%v err=%v input=%q", ok, err, smaller.Input)
	}
	if retainedInputRunes(current) != 400 || retainedInputRunes(smaller) != 200 {
		t.Fatalf("retained runes %d and %d, want 400 and 200", retainedInputRunes(current), retainedInputRunes(smaller))
	}
	got, replaced := keepLargerToolFit(current, smaller)
	if replaced || got.Input != current.Input {
		t.Fatalf("smaller excerpt replaced the larger one: retained=%d input=%q", retainedInputRunes(got), got.Input)
	}
	larger, ok, err := fitToolWithInputLimit(call, 5000, 600)
	if err != nil || !ok {
		t.Fatalf("refit failed: ok=%v err=%v", ok, err)
	}
	if got, replaced := keepLargerToolFit(current, larger); !replaced || got.Input != larger.Input {
		t.Fatalf("larger excerpt did not replace the smaller one: retained=%d", retainedInputRunes(got))
	}
}

// randomCallInput builds a call input of about length runes: one line or
// several, Latin or Cyrillic filler, with the answer word somewhere if relevant.
func randomCallInput(rng *rand.Rand, length int, relevant bool) string {
	filler := []rune("xyz")
	if rng.Intn(2) == 0 {
		filler = []rune("эюя")
	}
	runes := make([]rune, length)
	lineWidth := length + 1
	if rng.Intn(2) == 0 {
		lineWidth = rng.Intn(120) + 20
	}
	for i := range runes {
		runes[i] = filler[rng.Intn(len(filler))]
		if i%lineWidth == lineWidth-1 {
			runes[i] = '\n'
		}
	}
	input := string(runes)
	if relevant {
		at := rng.Intn(len(runes) + 1)
		input = string(runes[:at]) + " needle " + string(runes[at:])
	}
	return input
}

// Invariant: a relevant call is never dropped from the calls-only input while
// another call keeps more of its input than its minimum window.
func TestCallsOnlyRelevantCallsSurviveRandomTurns(t *testing.T) {
	rng := rand.New(rand.NewSource(510134))
	dropped, kept := 0, 0
	for trial := range 80 {
		count := rng.Intn(80) + 1
		budget := turnToolsCharLimit
		if rng.Intn(2) == 0 {
			budget = rng.Intn(turnToolsCharLimit-2000) + 2000
		}
		relevantShare := rng.Intn(4) + 1
		tools := make([]Tool, count)
		relevant := make(map[string]bool, count)
		for i := range tools {
			length := rng.Intn(2500) + 1
			if rng.Intn(3) == 0 {
				length = rng.Intn(300) + 1
			}
			id := fmt.Sprintf("%03d-%03d", trial, i)
			relevant[id] = rng.Intn(relevantShare+1) != 0
			tools[i] = Tool{Time: id, Name: "Bash", callsOnly: true, fullInput: randomCallInput(rng, length, relevant[id])}
		}
		bounded, err := boundTools(tools, "needle найден", budget)
		if err != nil {
			t.Fatalf("trial %d: %v", trial, err)
		}
		size, err := toolsJSONSize(bounded)
		if err != nil {
			t.Fatal(err)
		}
		if size > budget {
			t.Fatalf("trial %d: turn tools take %d characters, budget %d", trial, size, budget)
		}
		present := make(map[string]Tool, len(bounded))
		for _, call := range bounded {
			if call.Skipped == 0 {
				present[call.Time] = call
			}
			// A cropped relevant call keeps at least its minimum window, and
			// that window shows the answer word.
			if call.Skipped == 0 && relevant[call.Time] && retainedInputRunes(call) == callsOnlyMinimumCharLimit && utf8.RuneCountInString(call.fullInput) > callsOnlyMinimumCharLimit && !strings.Contains(call.Input, "needle") {
				t.Fatalf("trial %d: minimum window of %s lost the answer word: %q", trial, call.Time, call.Input)
			}
		}
		var missing []string
		for id, isRelevant := range relevant {
			if _, ok := present[id]; isRelevant && !ok {
				missing = append(missing, id)
			}
		}
		if len(missing) == 0 {
			kept++
			continue
		}
		dropped++
		for id, call := range present {
			if retainedInputRunes(call) > callsOnlyMinimumRunes(call) {
				t.Fatalf("trial %d (%d calls, budget %d): relevant calls %v dropped while %s keeps %d runes, minimum %d", trial, count, budget, missing, id, retainedInputRunes(call), callsOnlyMinimumRunes(call))
			}
		}
	}
	if dropped == 0 || kept == 0 {
		t.Fatalf("random turns do not cover both cases: %d with a relevant call dropped, %d with all kept", dropped, kept)
	}
}

// The minimum window of a relevant call shows the answer word even inside a
// long line in the middle of a multi-line input.
func TestCallsOnlyMinimumWindowKeepsAnswerWord(t *testing.T) {
	input := strings.Repeat("x", 300) + "\n" + strings.Repeat("y", 200) + " needle " + strings.Repeat("y", 200) + "\n" + strings.Repeat("z", 300)
	call := Tool{Name: "Bash", callsOnly: true, fullInput: input, answerWords: answerWords("needle найден")}
	fit, ok, err := fitToolWithInputLimit(call, turnToolsCharLimit, callsOnlyMinimumCharLimit)
	if err != nil || !ok {
		t.Fatalf("minimum window did not fit: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(fit.Input, "needle") || retainedInputRunes(fit) != callsOnlyMinimumCharLimit || utf8.RuneCountInString(fit.Input) > callsOnlyMinimumCharLimit {
		t.Fatalf("minimum window lost the answer word or its size: retained=%d input=%q", retainedInputRunes(fit), fit.Input)
	}
}

// A relevant call among the last ones does not keep its reserve share ahead of
// an earlier relevant call: it starts from its minimum window and grows in the
// same priority order, so the earlier call of a lower length class is whole.
func TestCallsOnlyLastRelevantCallsGrowInPriorityOrder(t *testing.T) {
	early := "verify needle " + strings.Repeat("x", 586)
	tools := []Tool{{Time: "early", Name: "Bash", callsOnly: true, fullInput: early}}
	for i := range 10 {
		tools = append(tools, Tool{Time: fmt.Sprintf("short-%02d", i), Name: "Bash", callsOnly: true, fullInput: "echo needle " + strings.Repeat("z", 276) + fmt.Sprint(i)})
	}
	for i := range lastTurnToolsKept {
		tools = append(tools, Tool{Time: fmt.Sprintf("last-%d", i), Name: "Bash", callsOnly: true, fullInput: "cat needle " + strings.Repeat(string(rune('a'+i)), 2000)})
	}
	bounded, err := boundTools(tools, "needle найден", 6000)
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range bounded {
		if call.Time == "early" {
			if call.Input != early {
				t.Fatalf("earlier relevant call was cropped behind the last calls: retained=%d input=%q", retainedInputRunes(call), call.Input)
			}
			return
		}
	}
	t.Fatal("earlier relevant call was dropped")
}

// A last call without room for any input keeps an empty excerpt and counts as
// retaining nothing, not the input it had before.
func TestCallsOnlyLastCallWithoutRoomRetainsNothing(t *testing.T) {
	tools := []Tool{{Time: "last", Name: "Bash", callsOnly: true, fullInput: strings.Repeat("x", 500), Input: strings.Repeat("x", 500), inputRetainedRunes: 500, inputExcerptMeasured: true}}
	selected := make([]bool, len(tools))
	if err := keepLastTools(tools, selected, 0, 10); err != nil {
		t.Fatal(err)
	}
	if !selected[0] || tools[0].Input != "" || retainedInputRunes(tools[0]) != 0 {
		t.Fatalf("last call without room: selected=%v retained=%d input=%q", selected[0], retainedInputRunes(tools[0]), tools[0].Input)
	}
}
