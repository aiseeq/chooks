package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/chooks/internal/core"
	"github.com/aiseeq/chooks/internal/hookctx"
)

// Срок чтения записи skill-reload в тексте ошибки — в поясе запроса, не
// исполнителя: у CLI это один пояс, у демона — разные
func TestNightHintDeadlineErrorInRequestLocation(t *testing.T) {
	zone, err := time.LoadLocation("Asia/Kathmandu")
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	req := hookctx.New(nil, nil, nil, []string{"HOME=" + home, "XDG_STATE_HOME=" + home + "/state", "TZ=Asia/Kathmandu"}, home, 1, 2)
	deadline := time.Date(2026, 11, 22, 1, 2, 3, 4e6, time.UTC)
	if deadline.In(time.Local).Format("15:04:05.000") == deadline.In(zone).Format("15:04:05.000") {
		t.Fatalf("executor zone %v shows the deadline as the request zone does", time.Local)
	}
	deps, err := nightHintDepsContext(context.Background(), core.DefaultConfig(), deadline, req)
	if err != nil {
		t.Fatal(err)
	}
	_, err = deps.SkillCalled("s1")
	want := deadline.In(zone).Format("15:04:05.000")
	if err == nil || !strings.Contains(err.Error(), "hook deadline "+want+" passed") {
		t.Fatalf("error=%v, want the deadline %s of the request zone", err, want)
	}
}
