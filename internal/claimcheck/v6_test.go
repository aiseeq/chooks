package claimcheck

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

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
		{"peer attachment after human reply during turn", []any{duration, ev("user", "t", "человеческая реплика"), peerAttachment, call("after"), final}, "человеческая реплика", []string{"after"}},
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
	var cropped *Tool
	var lastSeen bool
	for i := range in.TurnTools {
		call := &in.TurnTools[i]
		if strings.Contains(call.Input, "call-05") {
			cropped = call
		}
		if strings.Contains(call.Input, "call-23") {
			lastSeen = true
		}
	}
	if cropped == nil || !strings.Contains(cropped.Input, "[обрезано:") || len([]rune(cropped.Input)) <= toolInputCharLimit || len([]rune(cropped.Input)) >= callsOnlyInputCharLimit {
		t.Fatalf("call input was not cropped to fit the calls-only budget: %+v", cropped)
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
		if !strings.Contains(call.Input, "[обрезано:") || len([]rune(call.Input)) <= toolInputCharLimit+100 || len([]rune(call.Input)) >= callsOnlyInputCharLimit {
			t.Fatalf("call %d was not cropped within the calls-only limit: %+v", i, call)
		}
	}
}
