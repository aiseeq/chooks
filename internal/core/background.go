package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// abandonedTaskAge — сколько фоновая задача может висеть без отчёта, прежде
// чем Claude перестанет её «ждать». Намеренно вечная задача (dev-сервер,
// watcher) иначе глушила бы уведомления до конца сессии; честно-долгая
// задача сверх срока в худшем случае даёт один лишний звонок
const abandonedTaskAge = 2 * time.Hour

// tasksFileSuffix отличает файл учёта задач от файла состояния той же сессии
const tasksFileSuffix = ".tasks"

// TrackedTask — фоновая задача сессии, как её застал конец хода
type TrackedTask struct {
	ID          string    `json:"id"`
	Type        string    `json:"type"`
	Description string    `json:"description"`
	Since       time.Time `json:"since"`
}

// Abandoned — задача висит дольше abandonedTaskAge, Claude её больше не ждёт
func (t TrackedTask) Abandoned(now time.Time) bool {
	return now.Sub(t.Since) >= abandonedTaskAge
}

// Background — фоновая работа сессии на конец последнего хода. Её читает
// строка статуса: по ней человек видит, что держит сессию на паузе и можно
// ли её сбрасывать (пользователь 26.10: «С этими висящими консолями не
// понятно, можно ли сбрасывать?»)
type Background struct {
	Tasks []TrackedTask `json:"tasks"`
	// Crons — разовые будильники: держат паузу. Schedules — расписания:
	// паузу не держат, но пропадут при перезапуске сессии
	Crons     int `json:"crons"`
	Schedules int `json:"schedules,omitempty"`
}

// agentTypes — типы фоновых задач, где работают свои агенты, а не ожидание
// внешнего (shell, монитор)
var agentTypes = map[string]bool{"subagent": true, "workflow": true}

// Agent — в задаче работают свои агенты
func (t TrackedTask) Agent() bool {
	return agentTypes[t.Type]
}

// AgentsWorking — среди задач есть не брошенная задача агентов
func (b Background) AgentsWorking(now time.Time) bool {
	for _, task := range b.Tasks {
		if task.Agent() && !task.Abandoned(now) {
			return true
		}
	}
	return false
}

// Abandoned — вся работа брошена: разовых будильников нет, а каждая задача висит
// дольше срока. Пустая работа брошенной не считается: её просто нет
func (b Background) Abandoned(now time.Time) bool {
	if b.Crons > 0 || len(b.Tasks) == 0 {
		return false
	}
	for _, task := range b.Tasks {
		if !task.Abandoned(now) {
			return false
		}
	}
	return true
}

// RecordBackground запоминает фоновую работу сессии на конец хода и
// отбирает задачи, которых Claude ещё ждёт: всё из списка Claude Code, кроме
// висящих дольше abandonedTaskAge. Время первого появления задачи хранится
// рядом с состоянием сессии, исчезнувшие из списка задачи забываются. При
// сбое учёта возвращается только ошибка: брошенные задачи не отличить от
// живых, и чем заменить отбор, решает вызывающий
func RecordBackground(sessionID string, tasks []BackgroundTask, crons []SessionCron, now time.Time) ([]BackgroundTask, error) {
	if sessionID == "" {
		return tasks, nil
	}

	statePath, err := sessionStatePath(sessionID)
	if err != nil {
		return nil, err
	}
	path := statePath + tasksFileSuffix

	if len(tasks) == 0 && len(crons) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("cannot forget background tasks: %w", err)
		}
		return tasks, nil
	}

	previous, err := loadBackground(path)
	if err != nil {
		return nil, err
	}
	since := make(map[string]time.Time, len(previous.Tasks))
	for _, task := range previous.Tasks {
		since[task.ID] = task.Since
	}

	wakeups, schedules := CountCrons(crons)
	current := Background{Crons: wakeups, Schedules: schedules, Tasks: make([]TrackedTask, 0, len(tasks))}
	active := make([]BackgroundTask, 0, len(tasks))
	for _, task := range tasks {
		first, known := since[task.ID]
		if !known {
			first = now
		}
		tracked := TrackedTask{ID: task.ID, Type: task.Type, Description: task.Description, Since: first}
		current.Tasks = append(current.Tasks, tracked)
		if !tracked.Abandoned(now) {
			active = append(active, task)
		}
	}

	if err := saveBackground(path, current); err != nil {
		return nil, err
	}
	return active, nil
}

// LoadBackground читает фоновую работу сессии на конец последнего хода.
// Нет записи — работы нет
func LoadBackground(sessionID string) (Background, error) {
	statePath, err := sessionStatePath(sessionID)
	if err != nil {
		return Background{}, err
	}
	return loadBackground(statePath + tasksFileSuffix)
}

// loadBackground читает файл учёта. Отсутствующий файл — задач ещё не видели
func loadBackground(path string) (Background, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Background{}, nil
	}
	if err != nil {
		return Background{}, fmt.Errorf("cannot read background tasks: %w", err)
	}

	var background Background
	if err := json.Unmarshal(data, &background); err != nil {
		return Background{}, fmt.Errorf("cannot parse background tasks %s: %w", path, err)
	}
	return background, nil
}

// saveBackground запоминает фоновую работу сессии
func saveBackground(path string, background Background) error {
	data, err := json.Marshal(background)
	if err != nil {
		return fmt.Errorf("cannot encode background tasks: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("cannot create session state dir: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("cannot write background tasks: %w", err)
	}
	return nil
}
