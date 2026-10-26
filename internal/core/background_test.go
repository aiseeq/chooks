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
	active, err := RecordBackground("s1", tasks, 0, now)
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
	if _, err := RecordBackground("s1", []BackgroundTask{server}, 0, start); err != nil {
		t.Fatalf("учёт задач: %v", err)
	}

	later := start.Add(abandonedTaskAge + time.Minute)
	agent := BackgroundTask{ID: "agent", Type: "subagent"}
	active, err := RecordBackground("s1", []BackgroundTask{server, agent}, 0, later)
	if err != nil {
		t.Fatalf("учёт задач: %v", err)
	}
	if len(active) != 1 || active[0].ID != "agent" {
		t.Errorf("живой должна остаться только новая задача: %+v", active)
	}
}

// Задача, пропавшая из списка, забывается: тот же идентификатор позже —
// уже новая задача
func TestRecordBackground_ForgetsFinishedTasks(t *testing.T) {
	runtimeDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	start := time.Now()

	task := BackgroundTask{ID: "a"}
	if _, err := RecordBackground("s1", []BackgroundTask{task}, 0, start); err != nil {
		t.Fatalf("учёт задач: %v", err)
	}
	if _, err := RecordBackground("s1", nil, 0, start.Add(time.Minute)); err != nil {
		t.Fatalf("учёт задач: %v", err)
	}
	if _, err := os.Stat(filepath.Join(runtimeDir, "chooks", "sessions", "s1"+tasksFileSuffix)); !os.IsNotExist(err) {
		t.Error("без задач файл учёта должен удаляться")
	}

	active, err := RecordBackground("s1", []BackgroundTask{task}, 0, start.Add(abandonedTaskAge+time.Hour))
	if err != nil {
		t.Fatalf("учёт задач: %v", err)
	}
	if len(active) != 1 {
		t.Errorf("задача, увиденная заново, должна считаться свежей: %+v", active)
	}
}

// Будильник без задач тоже держит сессию: учёт его помнит
func TestRecordBackground_KeepsCrons(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	if _, err := RecordBackground("s1", nil, 2, time.Now()); err != nil {
		t.Fatalf("учёт задач: %v", err)
	}
	background, err := LoadBackground("s1")
	if err != nil || background.Crons != 2 {
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
	active, err := RecordBackground("s1", tasks, 0, time.Now())
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
