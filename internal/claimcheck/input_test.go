package claimcheck

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func fixture(t *testing.T, events ...any) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "transcript.jsonl")
	f, e := os.Create(p)
	if e != nil {
		t.Fatal(e)
	}
	for _, v := range events {
		b, _ := json.Marshal(v)
		_, _ = f.Write(append(b, '\n'))
	}
	if e = f.Close(); e != nil {
		t.Fatal(e)
	}
	return p
}
func ev(typ, at string, content any) map[string]any {
	return map[string]any{"type": typ, "timestamp": at, "message": map[string]any{"content": content}}
}
func TestBuildInputFixtures(t *testing.T) {
	long := strings.Repeat("z", 2200)
	rows := []any{
		ev("user", "2026-01-01T00:00:00Z", "вопрос пользователя"),
		ev("assistant", "2026-01-01T00:00:01Z", []any{map[string]any{"type": "text", "text": "одна часть "}, map[string]any{"type": "text", "text": "полный ответ"}}),
		ev("user", "2026-01-01T00:01:00Z", "проверь"),
		ev("assistant", "2026-01-01T00:01:01Z", []any{map[string]any{"type": "tool_use", "id": "c1", "name": "Bash", "input": map[string]any{"command": "printf first"}}}),
		map[string]any{"type": "user", "timestamp": "2026-01-01T00:01:02Z", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "c1", "content": "first output"}}}},
		ev("assistant", "2026-01-01T00:01:03Z", []any{map[string]any{"type": "tool_use", "id": "c2", "name": "Read", "input": map[string]any{"file_path": "/tmp/a"}}}),
		map[string]any{"type": "user", "timestamp": "2026-01-01T00:01:04Z", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "c2", "content": long}}}},
		ev("assistant", "2026-01-01T00:01:05Z", []any{map[string]any{"type": "tool_use", "id": "c3", "name": "Grep", "input": map[string]any{"pattern": "needle"}}}),
		map[string]any{"type": "user", "timestamp": "2026-01-01T00:01:06Z", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "c3", "content": "needle found"}}}},
		ev("assistant", "2026-01-01T00:01:07Z", []any{map[string]any{"type": "text", "text": "Проверено, needle найден."}}),
		ev("user", "2026-01-01T00:02:00Z", []any{map[string]any{"type": "image", "source": map[string]any{"type": "base64", "data": "dont-copy-image"}}}),
		ev("assistant", "2026-01-01T00:02:01Z", []any{map[string]any{"type": "text", "text": "ответ с картинкой"}}),
		ev("assistant", "2026-01-01T00:02:02Z", []any{map[string]any{"type": "text", "text": "БУДУЩЕЕ секретное"}}),
	}
	p := fixture(t, rows...)
	t.Run("one response and all text blocks", func(t *testing.T) {
		got, e := BuildInput(p, 2, Options{})
		if e != nil {
			t.Fatal(e)
		}
		if got.Answer != "одна часть полный ответ" || got.Event != "stop" {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("decision time covers preceding timestamp jitter", func(t *testing.T) {
		p := fixture(t, ev("user", "2026-01-01T00:00:01.002Z", "вопрос"), ev("assistant", "2026-01-01T00:00:01.001Z", []any{map[string]any{"type": "text", "text": "ответ"}}))
		got, e := BuildInput(p, 2, Options{})
		if e != nil {
			t.Fatal(e)
		}
		if got.AnswerTime != "2026-01-01T00:00:01.002Z" || got.LastEventTime != got.AnswerTime {
			t.Fatalf("answer=%s last=%s", got.AnswerTime, got.LastEventTime)
		}
	})
	t.Run("tools output crop and no future", func(t *testing.T) {
		got, e := BuildInput(p, 10, Options{})
		if e != nil {
			t.Fatal(e)
		}
		if len(got.TurnTools) != 3 {
			t.Fatalf("tools=%d", len(got.TurnTools))
		}
		if !strings.Contains(got.TurnTools[1].Output, "[обрезано:") {
			t.Fatal("long output lacks crop marker")
		}
		b, _ := json.Marshal(got)
		if strings.Contains(string(b), "БУДУЩЕЕ") {
			t.Fatal("future text leaked")
		}
	})
	t.Run("user image marker", func(t *testing.T) {
		p := fixture(t,
			ev("user", "t1", []any{map[string]any{"type": "text", "text": "описание"}, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "data": "dont-copy-image"}}}),
			ev("assistant", "t2", []any{map[string]any{"type": "text", "text": "ответ с картинкой"}}),
		)
		got, e := BuildInput(p, 2, Options{})
		if e != nil {
			t.Fatal(e)
		}
		if len(got.Attachments) != 1 || got.Attachments[0] != "image" {
			t.Fatalf("attachments=%v", got.Attachments)
		}
	})
	t.Run("user file attachment includes path only", func(t *testing.T) {
		p := fixture(t,
			ev("user", "t1", []any{map[string]any{"type": "text", "text": "прочти файл"}, map[string]any{"type": "file", "file_path": "/home/user/report.txt", "content": "must not leak"}}),
			ev("assistant", "t2", []any{map[string]any{"type": "text", "text": "ответ"}}),
		)
		got, err := BuildInput(p, 2, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Attachments) != 1 || got.Attachments[0] != "file: /home/user/report.txt" {
			t.Fatalf("attachments=%v", got.Attachments)
		}
	})
	t.Run("service attachments do not replace turn start or become user attachments", func(t *testing.T) {
		p := fixture(t,
			ev("user", "t0", "<command-name>/clear</command-name><command-message>clear</command-message>"),
			ev("user", "t1", []any{map[string]any{"type": "text", "text": "исходный запрос"}, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "data": "private"}}}),
			map[string]any{"type": "attachment", "timestamp": "t2", "attachment": map[string]any{"type": "deferred_tools_record", "content": "служебное"}},
			map[string]any{"type": "user", "timestamp": "t3", "message": map[string]any{"content": ""}},
			ev("assistant", "t4", []any{map[string]any{"type": "text", "text": "ответ"}}),
		)
		got, err := BuildInput(p, 5, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if got.UserPrompt == nil || !strings.HasPrefix(got.UserPrompt.Text, "исходный запрос") || len(got.Attachments) != 1 || got.Attachments[0] != "image" {
			t.Fatalf("prompt=%+v attachments=%v", got.UserPrompt, got.Attachments)
		}
	})
	t.Run("stop hook rejection does not end turn", func(t *testing.T) {
		p := fixture(t,
			ev("user", "t1", "вопрос"),
			ev("assistant", "t2", []any{map[string]any{"type": "text", "text": "первый ответ"}}),
			map[string]any{"type": "system", "subtype": "stop_hook_summary", "timestamp": "t3"},
			ev("assistant", "t4", []any{map[string]any{"type": "tool_use", "id": "x", "name": "Bash", "input": map[string]any{"command": "check"}}}),
			map[string]any{"type": "user", "timestamp": "t5", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "x", "content": "checked"}}}},
			ev("assistant", "t6", []any{map[string]any{"type": "text", "text": "исправленный ответ"}}),
		)
		got, err := BuildInput(p, 6, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if got.UserPrompt == nil || got.UserPrompt.Text != "вопрос" || got.Answer != "первый ответисправленный ответ" {
			t.Fatalf("prompt=%+v answer=%q", got.UserPrompt, got.Answer)
		}
	})
	t.Run("tool screenshot is not a user attachment", func(t *testing.T) {
		p := fixture(t,
			ev("user", "t1", "сделай скриншот"),
			ev("assistant", "t2", []any{map[string]any{"type": "tool_use", "id": "x", "name": "Bash", "input": map[string]any{"command": "screenshot"}}}),
			map[string]any{"type": "user", "timestamp": "t3", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "x", "content": []any{map[string]any{"type": "image", "source": map[string]any{"type": "base64", "data": "private"}}}}}}},
			ev("assistant", "t4", []any{map[string]any{"type": "text", "text": "готово"}}),
		)
		got, err := BuildInput(p, 4, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Attachments) != 0 {
			t.Fatalf("attachments=%v", got.Attachments)
		}
	})
	t.Run("compact boundary begins continuation", func(t *testing.T) {
		p := fixture(t,
			ev("assistant", "t1", []any{map[string]any{"type": "text", "text": "старый ответ"}}),
			map[string]any{"type": "system", "subtype": "turn_duration", "timestamp": "t2"},
			map[string]any{"type": "system", "subtype": "compact_boundary", "timestamp": "t3"},
			ev("assistant", "t4", []any{map[string]any{"type": "text", "text": "ответ после компакции"}}),
		)
		got, err := BuildInput(p, 4, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if got.UserPrompt == nil || got.UserPrompt.Type != "compact" || got.Context.PreviousAnswer != "старый ответ" {
			t.Fatalf("prompt=%+v context=%+v", got.UserPrompt, got.Context)
		}
	})
	t.Run("earliest timestamp wins for reordered transcript events", func(t *testing.T) {
		p := fixture(t,
			map[string]any{"type": "system", "subtype": "turn_duration", "timestamp": "2026-01-01T00:00:00Z"},
			map[string]any{"type": "user", "origin": map[string]any{"kind": "task-notification"}, "timestamp": "2026-01-01T00:00:03Z", "message": map[string]any{"content": "late notification"}},
			map[string]any{"type": "user", "isMeta": true, "message": map[string]any{"content": "context compacted"}, "timestamp": "2026-01-01T00:00:01Z"},
			ev("assistant", "2026-01-01T00:00:04Z", []any{map[string]any{"type": "text", "text": "answer"}}),
		)
		got, err := BuildInput(p, 4, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if got.UserPrompt == nil || got.UserPrompt.Type != "compact" {
			t.Fatalf("prompt=%+v", got.UserPrompt)
		}
	})
	t.Run("send message", func(t *testing.T) {
		p := fixture(t, ev("user", "t1", "hi"), ev("assistant", "t2", []any{map[string]any{"type": "tool_use", "id": "s", "name": "SendMessage", "input": map[string]any{"message": "письмо"}}}))
		got, e := BuildInput(p, 2, Options{})
		if e != nil {
			t.Fatal(e)
		}
		if got.Event != "send" || got.Answer != "письмо" {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("peer message begins turn even when Claude marks it meta", func(t *testing.T) {
		p := fixture(t,
			ev("system", "t0", ""),
			map[string]any{"type": "system", "subtype": "turn_duration", "timestamp": "t1"},
			map[string]any{"type": "user", "isMeta": true, "origin": map[string]any{"kind": "peer"}, "timestamp": "t2", "message": map[string]any{"content": "<cross-session-message from-name=\"peer-name\">peer-name: письмо</cross-session-message>"}},
			ev("assistant", "t3", []any{map[string]any{"type": "tool_use", "id": "s", "name": "SendToProject", "input": map[string]any{"message": "ответ"}}}),
		)
		got, err := BuildInput(p, 4, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if got.UserPrompt == nil || got.UserPrompt.Type != "peer" || got.UserPrompt.Text != "peer-name: письмо" {
			t.Fatalf("prompt=%+v", got.UserPrompt)
		}
	})
	t.Run("task notification begins turn", func(t *testing.T) {
		p := fixture(t, ev("assistant", "t1", []any{map[string]any{"type": "text", "text": "старый"}}), map[string]any{"type": "attachment", "timestamp": "t2", "attachment": map[string]any{"type": "queued_command", "commandMode": "task-notification", "prompt": []any{map[string]any{"type": "text", "text": "задача "}, map[string]any{"type": "text", "text": "завершилась"}}}}, ev("assistant", "t3", []any{map[string]any{"type": "text", "text": "ответ задачи"}}))
		got, e := BuildInput(p, 3, Options{})
		if e != nil {
			t.Fatal(e)
		}
		if got.Answer != "ответ задачи" || got.UserPrompt == nil || strings.Join(strings.Fields(got.UserPrompt.Text), " ") != "задача завершилась" || got.Context.PreviousAnswer != "старый" {
			t.Fatalf("%+v", got)
		}
	})
}

func TestBoundToolsKeepsRelevantAndBoundaryCalls(t *testing.T) {
	const callCount = 200
	rows := []any{ev("user", "2026-01-01T00:00:00Z", "Проверь результат")}
	output := strings.Repeat("длинный нерелевантный вывод ", 100)
	for i := range callCount {
		id := fmt.Sprintf("call-%03d", i)
		path := fmt.Sprintf("/repo/%03d.txt", i)
		if i == callCount/2 {
			path = "/repo/evidence-file.txt"
		}
		at := fmt.Sprintf("2026-01-01T00:%02d:%02dZ", (i*2+1)/60, (i*2+1)%60)
		rows = append(rows,
			ev("assistant", at, []any{map[string]any{"type": "tool_use", "id": id, "name": "Read", "input": map[string]any{"file_path": path}}}),
			map[string]any{"type": "user", "timestamp": at, "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": id, "content": output}}}},
		)
	}
	rows = append(rows, ev("assistant", "2026-01-01T01:00:00Z", []any{map[string]any{"type": "text", "text": "Verified checksum for /repo/evidence-file.txt"}}))
	p := fixture(t, rows...)
	got, err := BuildInput(p, len(rows), Options{})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(got.TurnTools)
	if err != nil {
		t.Fatal(err)
	}
	if size := utf8.RuneCount(encoded); size > turnToolsCharLimit {
		t.Fatalf("turnTools JSON length=%d exceeds %d", size, turnToolsCharLimit)
	}
	kept := map[string]bool{}
	skipped := 0
	lastTime := ""
	for _, tool := range got.TurnTools {
		if tool.Skipped > 0 {
			skipped += tool.Skipped
			continue
		}
		if lastTime != "" && tool.Time < lastTime {
			t.Fatalf("tool order regressed: %s before %s", lastTime, tool.Time)
		}
		lastTime = tool.Time
		kept[tool.Input] = true
	}
	if !kept[`{"file_path":"/repo/evidence-file.txt"}`] {
		t.Fatal("answer-matching middle call was dropped")
	}
	if !kept[`{"file_path":"/repo/199.txt"}`] {
		t.Fatalf("last call was not retained at the second priority: %v", kept)
	}
	if skipped != callCount-len(kept) {
		t.Fatalf("skipped marker count=%d; omitted calls=%d", skipped, callCount-len(kept))
	}
}

func TestBoundToolsKeepsLastCallsBeforeAnswerAndCropsWriteInput(t *testing.T) {
	const callCount = 200
	rows := []any{ev("user", "2026-01-01T00:00:00Z", "Запиши отчёт и проверь бойцов")}
	content := strings.Repeat("report-r7 содержимое отчёта ", 2000)
	for i := range callCount {
		id := fmt.Sprintf("call-%03d", i)
		at := fmt.Sprintf("2026-01-01T00:%02d:%02dZ", (i*2+1)/60, (i*2+1)%60)
		call := map[string]any{"type": "tool_use", "id": id, "name": "Bash", "input": map[string]any{"command": fmt.Sprintf("pgrep -f worker-%03d", i)}}
		if i < 10 {
			call = map[string]any{"type": "tool_use", "id": id, "name": "Write", "input": map[string]any{"file_path": fmt.Sprintf("/work/report-r7-%d.md", i), "content": content}}
		}
		rows = append(rows,
			ev("assistant", at, []any{call}),
			map[string]any{"type": "user", "timestamp": at, "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": id, "content": strings.Repeat("нет процессов ", 200)}}}},
		)
	}
	rows = append(rows, ev("assistant", "2026-01-01T01:00:00Z", []any{map[string]any{"type": "text", "text": "Отчёт report-r7 записан, бойцов не идёт."}}))
	got, err := BuildInput(fixture(t, rows...), len(rows), Options{})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(got.TurnTools)
	if err != nil {
		t.Fatal(err)
	}
	if size := utf8.RuneCount(encoded); size > turnToolsCharLimit {
		t.Fatalf("turnTools JSON length=%d exceeds %d", size, turnToolsCharLimit)
	}
	n := len(got.TurnTools)
	if n < lastTurnToolsKept {
		t.Fatalf("turnTools=%d", n)
	}
	for k := range lastTurnToolsKept {
		tool := got.TurnTools[n-lastTurnToolsKept+k]
		want := fmt.Sprintf("pgrep -f worker-%03d", callCount-lastTurnToolsKept+k)
		if tool.Skipped > 0 || tool.Input != want || utf8.RuneCountInString(tool.Output) < 300 {
			t.Fatalf("call %d before answer: %+v, want input %q", lastTurnToolsKept-k, tool, want)
		}
	}
	write := got.TurnTools[0]
	if write.Name != "Write" || !strings.Contains(write.Input, "/work/report-r7-0.md") {
		t.Fatalf("first matching Write call was dropped: %+v", write)
	}
	full := utf8.RuneCountInString(fmt.Sprintf(`{"content":%q,"file_path":"/work/report-r7-0.md"}`, content))
	if marker := fmt.Sprintf("[обрезано: %d знаков из %d]", full-toolInputCharLimit, full); !strings.Contains(write.Input, marker) {
		t.Fatalf("Write input lacks %q: %q", marker, write.Input)
	}
	if size := utf8.RuneCountInString(write.Input); size > toolInputCharLimit+len("\n[обрезано: 000000 знаков из 000000]\n") {
		t.Fatalf("Write input length=%d", size)
	}
	skipped, real := 0, 0
	for _, tool := range got.TurnTools {
		if tool.Skipped > 0 {
			skipped += tool.Skipped
		} else {
			real++
		}
	}
	if skipped+real != callCount {
		t.Fatalf("kept %d + skipped %d != %d", real, skipped, callCount)
	}
}

func TestBoundToolsLastCallsFillHalfBudget(t *testing.T) {
	tools := make([]Tool, 40)
	for i := range tools {
		in := fmt.Sprintf("ls %02d", i)
		tools[i] = Tool{Time: "t", Name: "Bash", Input: in, fullInput: in}
	}
	got, err := boundTools(tools, "ответ", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Skipped != 40-(len(got)-1) {
		t.Fatalf("expected one leading skip marker: %+v", got)
	}
	size, err := toolsJSONSize(got)
	if err != nil {
		t.Fatal(err)
	}
	if real := len(got) - 1; real <= lastTurnToolsKept || size > 1000 {
		t.Fatalf("recent calls kept=%d size=%d", real, size)
	}
}

func TestBoundToolsKeepsLastCallsEvenWhenBudgetIsTooSmall(t *testing.T) {
	tools := make([]Tool, 8)
	for i := range tools {
		out := strings.Repeat("вывод ", 50)
		tools[i] = Tool{Time: "t", Name: "Bash", Input: "ls", fullInput: "ls", Output: out, fullOutput: out}
	}
	got, err := boundTools(tools, "ответ", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != lastTurnToolsKept+1 || got[0].Skipped != len(tools)-lastTurnToolsKept {
		t.Fatalf("got %+v", got)
	}
	for _, tool := range got[1:] {
		if tool.Name != "Bash" || tool.Output != "" {
			t.Fatalf("last call not kept bare: %+v", tool)
		}
	}
}

func TestContextRecentToolsCropWriteInput(t *testing.T) {
	content := strings.Repeat("x", 5000)
	p := fixture(t,
		ev("user", "t1", "запиши"),
		ev("assistant", "t2", []any{map[string]any{"type": "tool_use", "id": "w", "name": "Write", "input": map[string]any{"file_path": "/work/a.md", "content": content}}}),
		map[string]any{"type": "user", "timestamp": "t3", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "w", "content": "written"}}}},
		ev("assistant", "t4", []any{map[string]any{"type": "text", "text": "записано"}}),
		map[string]any{"type": "system", "subtype": "turn_duration", "timestamp": "t5"},
		ev("user", "t6", "дальше"),
		ev("assistant", "t7", []any{map[string]any{"type": "text", "text": "готово"}}),
	)
	got, err := BuildInput(p, 7, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Context.RecentTools) != 1 || !strings.Contains(got.Context.RecentTools[0].Input, "/work/a.md") || !strings.Contains(got.Context.RecentTools[0].Input, "[обрезано: ") {
		t.Fatalf("recent tools=%+v", got.Context.RecentTools)
	}
}

func TestBoundToolsLastCallsLeaveSmallCallsShareToLargeOne(t *testing.T) {
	tools := make([]Tool, lastTurnToolsKept)
	for i := range tools {
		tools[i] = Tool{Time: "t", Name: "Bash", Input: "ls", fullInput: "ls"}
	}
	large := strings.Repeat("y", 5000)
	tools[0].Output, tools[0].fullOutput = large, large
	got, err := boundTools(tools, "ответ", 3000)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != lastTurnToolsKept || utf8.RuneCountInString(got[0].Output) < 1000 {
		t.Fatalf("large last call output=%d of %d tools", utf8.RuneCountInString(got[0].Output), len(got))
	}
}

func TestBoundToolsStaysWithinBudgetWhenSplittingGaps(t *testing.T) {
	tools := make([]Tool, 60)
	for i := range tools {
		out := strings.Repeat("в", 40+i*37%900)
		if i%7 == 3 {
			out += " ответ"
		}
		in := fmt.Sprintf("cmd %d", i)
		tools[i] = Tool{Time: "2026-01-01T00:00:00Z", Name: "Bash", Input: in, fullInput: in, Output: out, fullOutput: out}
	}
	for budget := 700; budget <= 6000; budget += 37 {
		got, err := boundTools(tools, "ответ", budget)
		if err != nil {
			t.Fatal(err)
		}
		size, err := toolsJSONSize(got)
		if err != nil {
			t.Fatal(err)
		}
		if size > budget {
			t.Fatalf("budget %d: turnTools JSON length=%d", budget, size)
		}
	}
}
