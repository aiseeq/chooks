package claimcheck

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestV7Stop(t *testing.T) {
	const final = `"final":"","sourced":false,"marked":false,"evidence":""`
	for _, tc := range []struct {
		name, answer, outcome string
		block                 bool
		want                  []string
	}{
		{"no ask", `{` + final + `,"ask":"","ask_kind":"","ask_mark":false,"checked_line":false}`, OutcomePass, false, nil},
		{"question checked", `{` + final + `,"ask":"Q","ask_kind":"question","ask_mark":true,"checked_line":true}`, OutcomePass, false, nil},
		{"question no check", `{` + final + `,"ask":"Q","ask_kind":"question","ask_mark":true,"checked_line":false}`, OutcomeBlock, true, []string{"«Q»", "Нет строки «Проверил: <id>»", "Без проведённой проверки"}},
		{"question no mark", `{` + final + `,"ask":"Q","ask_kind":"question","ask_mark":false,"checked_line":true}`, OutcomeBlock, true, []string{"Нет строки ❓:"}},
		{"question neither", `{` + final + `,"ask":"Q","ask_kind":"question","ask_mark":false,"checked_line":false}`, OutcomeBlock, true, []string{"Нет строки ❓:", "Нет строки «Проверил: <id>»", "вопрос не задавай"}},
		{"draft checked", `{` + final + `,"ask":"D","ask_kind":"draft","ask_mark":false,"checked_line":true}`, OutcomePass, false, nil},
		{"draft no check", `{` + final + `,"ask":"D","ask_kind":"draft","ask_mark":true,"checked_line":false}`, OutcomeBlock, true, []string{"Черновик людям: «D»", "исправленный черновик"}},
		{"final and question", `{"final":"X","sourced":false,"marked":false,"evidence":"docs","ask":"Q","ask_kind":"question","ask_mark":false,"checked_line":false}`, OutcomeBlock, true, []string{"«X»", "docs", "«Q»"}},
		{"v6 answer", `{` + final + `}`, OutcomeError, false, nil},
		{"unknown kind", `{` + final + `,"ask":"Q","ask_kind":"other","ask_mark":false,"checked_line":false}`, OutcomeError, false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := &stubJudge{answer: func(string) (string, error) { return tc.answer, nil }}
			h := testHook(t, j)
			h.Prompt = "v7"
			got := h.Stop(context.Background(), stopEvent(t))
			if got.Result != tc.outcome || got.Block != tc.block {
				t.Fatalf("%+v", got)
			}
			for _, w := range tc.want {
				if !strings.Contains(got.Reason, w) {
					t.Fatalf("reason %q lacks %q", got.Reason, w)
				}
			}
			e := journal(t, h)
			if len(e) == 0 || e[len(e)-1].Prompt != "v7" {
				t.Fatalf("journal %+v", e)
			}
			last := e[len(e)-1]
			if tc.outcome != OutcomeError && (last.AskKind == nil || last.AskMark == nil || last.CheckedLine == nil || last.Ask == nil) {
				t.Fatalf("journal %+v", last)
			}
		})
	}
}

// v6 не пишет полей второй задачи, даже если судья их вернул
func TestV6IgnoresAskFields(t *testing.T) {
	j := &stubJudge{answer: func(string) (string, error) {
		return `{"final":"","sourced":false,"marked":false,"evidence":"","ask":"Q","ask_kind":"question","ask_mark":false,"checked_line":false}`, nil
	}}
	h := testHook(t, j)
	h.Prompt = "v6"
	if got := h.Stop(context.Background(), stopEvent(t)); got.Result != OutcomePass {
		t.Fatalf("%+v", got)
	}
	e := journal(t, h)
	if last := e[len(e)-1]; last.AskKind != nil || last.Ask != nil {
		t.Fatalf("journal %+v", last)
	}
}

func TestV7SendSkipped(t *testing.T) {
	j := &stubJudge{answer: answerOf(flagClaim)}
	h := testHook(t, j)
	h.Prompt = "v7"
	if got := h.Send(context.Background(), SendEvent{Text: "письмо"}); got.Result != OutcomeSkip || j.calls.Load() != 0 {
		t.Fatalf("%+v", got)
	}
}

func TestV7StopJudgeInput(t *testing.T) {
	j := &bodyJudge{}
	h := testHook(t, j)
	h.Prompt = "v7"
	event := stopEvent(t)
	h.Stop(context.Background(), event)
	var body map[string]json.RawMessage
	if err := json.Unmarshal([]byte(j.body), &body); err != nil {
		t.Fatal(err)
	}
	var calls []map[string]json.RawMessage
	if err := json.Unmarshal(body["turnTools"], &calls); err != nil {
		t.Fatal(err)
	}
	if len(body) != 3 || body["userPrompt"] == nil || len(calls) != 1 || calls[0]["output"] != nil || calls[0]["input"] == nil {
		t.Fatalf("judge body: %s", j.body)
	}
}
