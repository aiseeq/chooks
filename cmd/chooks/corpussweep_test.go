package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestRunCorpusSweep(t *testing.T) {
	input := strings.Join([]string{
		`{"id":1,"command":"rg foo ~/.claude/projects"}`,
		`{"id":2,"command":"grep x ~/.codex/config.toml"}`,
		`{"id":3,"command":"ls internal/"}`,
	}, "\n")
	var out, summary bytes.Buffer
	if err := runCorpusSweep(&out, &summary, strings.NewReader(input), "command", "/home/aisee"); err != nil {
		t.Fatal(err)
	}
	var results []struct {
		ID          int `json:"id"`
		CorpusSweep struct {
			Blocked bool     `json:"blocked"`
			Sweeps  []string `json:"sweeps"`
			Points  []string `json:"points"`
		} `json:"corpus_sweep"`
	}
	for line := range strings.SplitSeq(strings.TrimSpace(out.String()), "\n") {
		var result struct {
			ID          int `json:"id"`
			CorpusSweep struct {
				Blocked bool     `json:"blocked"`
				Sweeps  []string `json:"sweeps"`
				Points  []string `json:"points"`
			} `json:"corpus_sweep"`
		}
		if err := json.Unmarshal([]byte(line), &result); err != nil {
			t.Fatalf("разбор строки результата: %v", err)
		}
		results = append(results, result)
	}
	if len(results) != 3 {
		t.Fatalf("получено строк: %d, ожидалось 3", len(results))
	}
	if results[0].ID != 1 || !results[0].CorpusSweep.Blocked || len(results[0].CorpusSweep.Sweeps) != 1 {
		t.Fatalf("первая строка: %+v", results[0])
	}
	if results[1].ID != 2 || results[1].CorpusSweep.Blocked || len(results[1].CorpusSweep.Points) != 1 {
		t.Fatalf("вторая строка: %+v", results[1])
	}
	if results[2].ID != 3 || results[2].CorpusSweep.Blocked || len(results[2].CorpusSweep.Sweeps) != 0 || len(results[2].CorpusSweep.Points) != 0 {
		t.Fatalf("третья строка: %+v", results[2])
	}
	if got, want := summary.String(), "Всего: 3; остановлено: 1; точечных среди пропущенных: 1; без корпуса: 1\n"; got != want {
		t.Fatalf("сводка %q, ожидалось %q", got, want)
	}
}

func TestRunCorpusSweepRejectsBadRows(t *testing.T) {
	cases := []struct {
		name, input, want string
	}{
		{"нет поля", `{"id":1}`, "строка 1"},
		{"не объект", `[]`, "строка 1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out, summary bytes.Buffer
			err := runCorpusSweep(&out, &summary, strings.NewReader(tc.input), "command", "/home/aisee")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ошибка = %v, нужна ошибка с %q", err, tc.want)
			}
		})
	}
}
