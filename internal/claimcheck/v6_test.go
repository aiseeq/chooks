package claimcheck

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

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
	humanAttachment := map[string]any{"type": "attachment", "attachment": map[string]any{"type": "queued_command", "origin": map[string]any{"kind": "human"}, "prompt": "новый вопрос"}}
	duration := map[string]any{"type": "system", "subtype": "turn_duration"}
	for _, tc := range []struct {
		name   string
		events []any
		prompt string
		tools  []string
	}{
		{"notification", []any{ev("user", "t", "вопрос"), call("before"), duration, notification, call("after"), final}, "вопрос", []string{"before", "after"}},
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
