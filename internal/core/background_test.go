package core

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRecordBackground_FreshTasksAreActive(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	now := time.Now()

	tasks := []BackgroundTask{{ID: "a", Type: "shell", Description: "сборка"}, {ID: "b", Type: "subagent"}}
	active, err := RecordBackground("s1", tasks, nil, now)
	if err != nil {
		t.Fatalf("учёт задач: %v", err)
	}
	if len(active) != 2 {
		t.Errorf("свежие задачи должны считаться живыми: %+v", active)
	}

	background, err := LoadBackground("s1")
	if err != nil {
		t.Fatalf("чтение учёта: %v", err)
	}
	if len(background.Tasks) != 2 || background.Tasks[0].Description != "сборка" || !background.Tasks[0].Since.Equal(now) {
		t.Errorf("учёт должен хранить описание и время появления: %+v", background)
	}
}

// Dev-сервер в фоне не отчитывается никогда: иначе он глушил бы уведомления
// до конца сессии
func TestRecordBackground_AbandonedTaskExpires(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	start := time.Now()

	server := BackgroundTask{ID: "server", Type: "shell"}
	if _, err := RecordBackground("s1", []BackgroundTask{server}, nil, start); err != nil {
		t.Fatalf("учёт задач: %v", err)
	}

	later := start.Add(abandonedTaskAge + time.Minute)
	agent := BackgroundTask{ID: "agent", Type: "subagent"}
	active, err := RecordBackground("s1", []BackgroundTask{server, agent}, nil, later)
	if err != nil {
		t.Fatalf("учёт задач: %v", err)
	}
	if len(active) != 1 || active[0].ID != "agent" {
		t.Errorf("живой должна остаться только новая задача: %+v", active)
	}
}

// Слежение за правками Artifact Claude Code ставит сам при публикации и не
// снимает даже после /clear: агент его не ждёт, паузу оно не держит и в
// строке статуса не видно (27.10 work-admin стоял на паузе без уведомления о
// конце хода)
func TestRecordBackground_ArtifactWatchIsNotWaited(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	watch := BackgroundTask{ID: "m1", Type: "monitor", Description: "live updates for artifact https://claude.ai/artifact/HPY63WfM3BQkbnf5dv7eaV (auto-armed on publish)"}
	monitor := BackgroundTask{ID: "m2", Type: "monitor", Description: "live updates for deploy log"}
	active, err := RecordBackground("s1", []BackgroundTask{watch, monitor}, nil, time.Now())
	if err != nil {
		t.Fatalf("учёт задач: %v", err)
	}
	if len(active) != 1 || active[0].ID != "m2" {
		t.Errorf("живым должен остаться только свой монитор: %+v", active)
	}
	background, err := LoadBackground("s1")
	if err != nil || len(background.Tasks) != 1 || background.Tasks[0].ID != "m2" {
		t.Errorf("в учёте не должно быть слежения за Artifact: %+v, %v", background, err)
	}

	if _, err := RecordBackground("s1", []BackgroundTask{watch}, nil, time.Now()); err != nil {
		t.Fatalf("учёт задач: %v", err)
	}
	if background, err := LoadBackground("s1"); err != nil || len(background.Tasks) != 0 {
		t.Errorf("одно слежение за Artifact - фоновой работы нет: %+v, %v", background, err)
	}
}

// Задача, пропавшая из списка, забывается: тот же идентификатор позже —
// уже новая задача
func TestRecordBackground_ForgetsFinishedTasks(t *testing.T) {
	runtimeDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	start := time.Now()

	task := BackgroundTask{ID: "a"}
	if _, err := RecordBackground("s1", []BackgroundTask{task}, nil, start); err != nil {
		t.Fatalf("учёт задач: %v", err)
	}
	if _, err := RecordBackground("s1", nil, nil, start.Add(time.Minute)); err != nil {
		t.Fatalf("учёт задач: %v", err)
	}
	if _, err := os.Stat(filepath.Join(runtimeDir, "chooks", "sessions", "s1"+tasksFileSuffix)); !os.IsNotExist(err) {
		t.Error("без задач файл учёта должен удаляться")
	}

	active, err := RecordBackground("s1", []BackgroundTask{task}, nil, start.Add(abandonedTaskAge+time.Hour))
	if err != nil {
		t.Fatalf("учёт задач: %v", err)
	}
	if len(active) != 1 {
		t.Errorf("задача, увиденная заново, должна считаться свежей: %+v", active)
	}
}

// Будильники без задач учёт тоже помнит: разовые отдельно от расписаний
func TestRecordBackground_KeepsCrons(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	crons := []SessionCron{{ID: "w1"}, {ID: "w2"}, {ID: "r1", Recurring: true}}
	if _, err := RecordBackground("s1", nil, crons, time.Now()); err != nil {
		t.Fatalf("учёт задач: %v", err)
	}
	background, err := LoadBackground("s1")
	if err != nil || background.Crons != 2 || background.Schedules != 1 {
		t.Errorf("будильники: %+v, %v", background, err)
	}
}

func TestRecordBackground_CorruptFileIsAnError(t *testing.T) {
	runtimeDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	dir := filepath.Join(runtimeDir, "chooks", "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "s1"+tasksFileSuffix), []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}

	tasks := []BackgroundTask{{ID: "a"}}
	active, err := RecordBackground("s1", tasks, nil, time.Now())
	if err == nil {
		t.Error("испорченный файл учёта должен давать ошибку")
	}
	if active != nil {
		t.Errorf("при сбое учёта список не выдумывается: %+v", active)
	}
}

func TestBackgroundAbandoned(t *testing.T) {
	now := time.Now()
	old := TrackedTask{Since: now.Add(-abandonedTaskAge - time.Minute)}
	fresh := TrackedTask{Since: now.Add(-time.Minute)}

	tests := []struct {
		name       string
		background Background
		expected   bool
	}{
		{name: "работы нет", background: Background{}, expected: false},
		{name: "все задачи старше срока", background: Background{Tasks: []TrackedTask{old, old}}, expected: true},
		{name: "одна свежая", background: Background{Tasks: []TrackedTask{old, fresh}}, expected: false},
		{name: "будильник держит", background: Background{Tasks: []TrackedTask{old}, Crons: 1}, expected: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.background.Abandoned(now); got != tt.expected {
				t.Errorf("Abandoned = %v, ожидалось %v", got, tt.expected)
			}
		})
	}
}
