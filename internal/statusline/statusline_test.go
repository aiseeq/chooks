package statusline

import (
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/chooks/internal/core"
)

// ansiPattern убирает оформление: проверяется содержимое, а не escape-коды
var ansiPattern = regexp.MustCompile(`\033\[[0-9;]*m`)

func plain(s string) string { return ansiPattern.ReplaceAllString(s, "") }

func TestProjectBadgeShowsState(t *testing.T) {
	tests := []struct {
		name     string
		state    core.SessionState
		expected string
	}{
		{name: "работа", state: core.StateWorking, expected: badgeBlue},
		{name: "ждёт свои задачи", state: core.StatePaused, expected: badgeSlate},
		{name: "ожидание ответа", state: core.StateWaiting, expected: badgeAmber},
		{name: "новое, не смотрел", state: core.StateDone, expected: badgeGreen},
		{name: "посмотрел", state: core.StateIdle, expected: badgeGray},
		{name: "ошибка API", state: core.StateError, expected: badgeRed},
		{name: "состояние не прочиталось", state: core.StateUnknown, expected: badgeBlue},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			badge := projectBadge("/home/user/work/demo", tt.state)
			if !strings.HasPrefix(badge, tt.expected) {
				t.Errorf("ожидался цвет %q, получено %q", tt.expected, badge)
			}
			if got := plain(badge); got != " DEMO " {
				t.Errorf("ожидалось %q, получено %q", " DEMO ", got)
			}
		})
	}
}

func TestContextBar(t *testing.T) {
	tests := []struct {
		used     float64
		expected string
		shade    string
	}{
		{used: 0, expected: "ctx ░░░░░░░░░░ 0%", shade: "153"},
		{used: 47, expected: "ctx ▓▓▓▓░░░░░░ 47%", shade: "111"},
		{used: 74, expected: "ctx ▓▓▓▓▓▓▓░░░ 74%", shade: "69"},
		{used: 100, expected: "ctx ▓▓▓▓▓▓▓▓▓▓ 100%", shade: "33"},
		// Округлённые до 100% проценты — полная полоса, а не 9 клеток
		{used: 99.6, expected: "ctx ▓▓▓▓▓▓▓▓▓▓ 100%", shade: "33"},
		{used: 29.6, expected: "ctx ▓▓▓░░░░░░░ 30%", shade: "111"},
		// Заявленное переполнение не должно рисовать полосу длиннее шкалы
		{used: 140, expected: "ctx ▓▓▓▓▓▓▓▓▓▓ 100%", shade: "33"},
	}

	for _, tt := range tests {
		got := contextBar(tt.used)
		if plain(got) != tt.expected {
			t.Errorf("contextBar(%v) = %q, ожидалось %q", tt.used, plain(got), tt.expected)
		}
		// Чем полнее, тем гуще оттенок голубого, без светофора
		if !strings.Contains(got, "\033[38;5;"+tt.shade+"m▓") && !strings.Contains(got, "\033[38;5;"+tt.shade+"m░") {
			t.Errorf("contextBar(%v) = %q, ожидался оттенок %s", tt.used, got, tt.shade)
		}
	}
}

func TestTerminalTitle(t *testing.T) {
	tests := []struct {
		name     string
		state    core.SessionState
		expected string
	}{
		{name: "работа", state: core.StateWorking, expected: "🔄 demo"},
		{name: "ждёт свои задачи", state: core.StatePaused, expected: "⏸️ demo"},
		{name: "ожидание ответа", state: core.StateWaiting, expected: "✴️ demo"},
		{name: "новое, не смотрел", state: core.StateDone, expected: "❇️ demo"},
		{name: "посмотрел", state: core.StateIdle, expected: "✅ demo"},
		{name: "ошибка API", state: core.StateError, expected: "🅰️ demo"},
		{name: "состояние не прочиталось: как работа", state: core.StateUnknown, expected: "🔄 demo"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := terminalTitle("/home/user/work/demo", tt.state)
			if got != tt.expected {
				t.Errorf("ожидалось %q, получено %q", tt.expected, got)
			}
		})
	}
}

func TestDetails(t *testing.T) {
	input := Input{}
	input.Model.DisplayName = "Opus 5"
	input.Effort.Level = "high"
	input.Cost.LinesAdded = 412
	input.Cost.LinesRemoved = 87

	parts := details(input, 47, weekBar{label: "нед"}, time.Now())

	expected := []string{"Opus 5·high", "ctx ▓▓▓▓░░░░░░ 47%", "+412 −87"}
	if len(parts) != len(expected) {
		t.Fatalf("ожидалось %d элементов, получено %d: %q", len(expected), len(parts), parts)
	}
	for i, want := range expected {
		if got := plain(parts[i]); got != want {
			t.Errorf("элемент %d: ожидалось %q, получено %q", i, want, got)
		}
	}
}

func TestShortModel(t *testing.T) {
	tests := map[string]string{
		"Opus 5 (1M context)": "Opus 5",
		"Opus 5":              "Opus 5",
		"":                    "",
	}

	for model, expected := range tests {
		if got := shortModel(model); got != expected {
			t.Errorf("shortModel(%q) = %q, ожидалось %q", model, got, expected)
		}
	}
}

func TestBuildFitsSingleLine(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	input := Input{CWD: t.TempDir()}
	input.Model.DisplayName = "Opus 5 (1M context)"

	line, title := build(input, 47, weekBar{label: "нед"}, time.Now(), hidden, testLogger(t))

	if strings.Contains(line, "\n") {
		t.Errorf("строка статуса должна умещаться в одну строку: %q", line)
	}
	if strings.Contains(plain(line), "1M context") {
		t.Errorf("уточнение модели должно убираться: %q", plain(line))
	}
	if !strings.HasPrefix(title, "🔄 ") {
		t.Errorf("заголовок должен начинаться со значка состояния: %q", title)
	}
}

func hidden() (bool, error)  { return false, nil }
func visible() (bool, error) { return true, nil }

// Законченный ход становится «посмотрел», только когда вкладку видно
func TestBuildMarksTurnSeen(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	input := Input{CWD: "/home/user/work/demo", SessionID: "s1"}
	if err := core.SaveSessionState("s1", core.StateDone); err != nil {
		t.Fatal(err)
	}

	if _, title := build(input, 10, weekBar{}, time.Now(), hidden, testLogger(t)); title != "❇️ demo" {
		t.Errorf("вкладку не видно — новое остаётся: %q", title)
	}
	if _, title := build(input, 10, weekBar{}, time.Now(), func() (bool, error) { return false, errors.New("dbus") }, testLogger(t)); title != "❇️ demo" {
		t.Errorf("сбой проверки — новое остаётся: %q", title)
	}
	if _, title := build(input, 10, weekBar{}, time.Now(), visible, testLogger(t)); title != "✅ demo" {
		t.Errorf("вкладку видно — посмотрел: %q", title)
	}
	if state, err := core.LoadSessionState("s1"); err != nil || state != core.StateIdle {
		t.Errorf("состояние = %q, %v", state, err)
	}
	// Работу вид вкладки не трогает
	if err := core.SaveSessionState("s1", core.StateWorking); err != nil {
		t.Fatal(err)
	}
	if _, title := build(input, 10, weekBar{}, time.Now(), visible, testLogger(t)); title != "🔄 demo" {
		t.Errorf("работа остаётся работой: %q", title)
	}
}

// Сессия без записи — новая или после /clear — стоит пустой, а не работает;
// показ записи не создаёт, первый запрос по-прежнему делает её работой
func TestBuildFreshSessionIdle(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	input := Input{CWD: "/home/user/work/demo", SessionID: "s1"}

	if _, title := build(input, 10, weekBar{}, time.Now(), hidden, testLogger(t)); title != "✅ demo" {
		t.Errorf("сессия без записи: %q", title)
	}
	if _, recorded, err := core.LookupSessionState("s1"); err != nil || recorded {
		t.Errorf("строка статуса записала состояние: %v, %v", recorded, err)
	}
	if state, err := core.LoadSessionState("s1"); err != nil || state != core.StateWorking {
		t.Errorf("хукам сессия без записи — работа: %q, %v", state, err)
	}
}

// Пауза показывает, что держит сессию; задачи старше срока — уже «готово»
func TestBuildShowsBackground(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	input := Input{CWD: "/home/user/work/demo", SessionID: "s1"}
	start := time.Now()
	tasks := []core.BackgroundTask{{ID: "b1", Type: "shell", Description: "Luna на честном наборе"}}
	if _, err := core.RecordBackground("s1", tasks, 0, start); err != nil {
		t.Fatal(err)
	}
	if err := core.SaveSessionState("s1", core.StatePaused); err != nil {
		t.Fatal(err)
	}

	line, title := build(input, 10, weekBar{}, start.Add(14*time.Minute), hidden, testLogger(t))
	if title != "⏸️ demo" || !strings.Contains(plain(line), "фон: Luna на честном наборе 14 мин") {
		t.Errorf("пауза с задачей: %q, %q", title, plain(line))
	}

	line, title = build(input, 10, weekBar{}, start.Add(3*time.Hour+28*time.Minute), hidden, testLogger(t))
	if title != "❇️ demo" || !strings.Contains(plain(line), "Luna на честном наборе 3 ч 28 мин брошена") {
		t.Errorf("брошенная задача — готово: %q, %q", title, plain(line))
	}
	if state, err := core.LoadSessionState("s1"); err != nil || state != core.StateDone {
		t.Errorf("состояние = %q, %v", state, err)
	}

	// Пока ход идёт, список задач устарел и не показывается
	if err := core.SaveSessionState("s1", core.StateWorking); err != nil {
		t.Fatal(err)
	}
	if line, _ := build(input, 10, weekBar{}, start, hidden, testLogger(t)); strings.Contains(plain(line), "фон:") {
		t.Errorf("во время хода фон не показывается: %q", plain(line))
	}
}

// Пауза, где работают свои агенты, — ▶️ на синей плашке, агент первым в
// «фон:»; брошенный агент паузу внешнего ожидания не меняет
func TestBuildShowsAgentsWorking(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	input := Input{CWD: "/home/user/work/demo", SessionID: "s1"}
	start := time.Now()
	tasks := []core.BackgroundTask{
		{ID: "m1", Type: "monitor", Description: "live updates"},
		{ID: "a1", Type: "subagent", Description: "Sleep page"},
	}
	if _, err := core.RecordBackground("s1", tasks, 0, start); err != nil {
		t.Fatal(err)
	}
	if err := core.SaveSessionState("s1", core.StatePaused); err != nil {
		t.Fatal(err)
	}

	line, title := build(input, 10, weekBar{}, start.Add(5*time.Minute), hidden, testLogger(t))
	if title != "▶️ demo" || !strings.HasPrefix(line, badgeBlue) || !strings.Contains(plain(line), "фон: Sleep page 5 мин +1") {
		t.Errorf("агенты работают: %q, %q", title, plain(line))
	}
	if state, err := core.LoadSessionState("s1"); err != nil || state != core.StatePaused {
		t.Errorf("показ агентов не пишется в состояние: %q, %v", state, err)
	}

	if err := core.SaveSessionState("s1", core.StatePaused); err != nil {
		t.Fatal(err)
	}
	if _, err := core.RecordBackground("s1", []core.BackgroundTask{{ID: "a1", Type: "subagent"}, {ID: "b1", Type: "shell"}}, 0, start.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, title := build(input, 10, weekBar{}, start.Add(3*time.Hour), hidden, testLogger(t)); title != "⏸️ demo" {
		t.Errorf("брошенный агент и живой shell — пауза: %q", title)
	}
}

func TestBackgroundPart(t *testing.T) {
	now := time.Now()
	long := strings.Repeat("я", 50)
	tests := []struct {
		name       string
		background core.Background
		expected   string
	}{
		{name: "пусто", background: core.Background{}, expected: ""},
		{
			name:       "задача без описания и ещё одна",
			background: core.Background{Tasks: []core.TrackedTask{{Type: "subagent", Since: now.Add(-5 * time.Minute)}, {Type: "shell", Since: now}}},
			expected:   "фон: subagent 5 мин +1",
		},
		{
			name:       "длинное описание обрезается",
			background: core.Background{Tasks: []core.TrackedTask{{Description: long, Since: now}}},
			expected:   "фон: " + strings.Repeat("я", backgroundRunes-1) + "… 0 мин",
		},
		{name: "будильник", background: core.Background{Crons: 1}, expected: "фон: будильник"},
		{
			name:       "задача и будильники",
			background: core.Background{Crons: 2, Tasks: []core.TrackedTask{{Description: "сборка", Since: now.Add(-61 * time.Minute)}}},
			expected:   "фон: сборка 1 ч 1 мин, будильников: 2",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := plain(backgroundPart(tt.background, now)); got != tt.expected {
				t.Errorf("получено %q, ожидалось %q", got, tt.expected)
			}
		})
	}
}

func TestDetailsSkipsMissingData(t *testing.T) {
	// Claude Code передаёт не все поля: пустые сведения не должны давать пустых разделителей
	if parts := details(Input{}, 0, weekBar{label: "нед"}, time.Now()); len(parts) != 0 {
		t.Errorf("ожидался пустой список, получено %q", parts)
	}
}

func TestSessionDirIsLaunchDir(t *testing.T) {
	input := Input{CWD: "/from/cwd"}
	input.Workspace.CurrentDir = "/from/current"
	input.Workspace.ProjectDir = "/from/project"

	// Агент ушёл в подпапку чужого проекта: имя остаётся от каталога запуска
	if got := sessionDir(input); got != "/from/project" {
		t.Errorf("ожидалось /from/project, получено %q", got)
	}

	input.Workspace.ProjectDir = ""
	if got := sessionDir(input); got != "/from/cwd" {
		t.Errorf("без project_dir: ожидалось /from/cwd, получено %q", got)
	}
}

// testLogger — логгер для тестов пакета: пишет в stderr, который go test
// показывает только при провале
func testLogger(t *testing.T) core.Logger {
	t.Helper()
	logger, err := core.NewLogger(core.LoggerConfig{Level: "debug"})
	if err != nil {
		t.Fatalf("не удалось создать логгер: %v", err)
	}
	return logger
}

// Сброс 26.10 10:00Z — окно с 19.10 10:00Z
var weekResets = time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)

func TestWeeklyBar(t *testing.T) {
	windowStart := weekResets.Add(-weekWindow)
	tests := []struct {
		name     string
		used     float64
		now      time.Time
		color    string
		expected string
	}{
		{
			// 51% за 30 часов: темп 1,7%/ч, к сбросу ~286%
			name: "кончится до сброса", used: 51, now: time.Date(2026, 10, 20, 15, 58, 0, 0, time.UTC),
			color: red, expected: "нед ▓▓▓▓▓░░░░░ 51%",
		},
		{
			// 30% за половину недели: к сбросу 60%
			name: "хватит с запасом", used: 30, now: windowStart.Add(84 * time.Hour),
			color: green, expected: "нед ▓▓▓░░░░░░░ 30%",
		},
		{
			// 40% за 72 часа: к сбросу ~93%
			name: "хватит впритык", used: 40, now: windowStart.Add(72 * time.Hour),
			color: yellow, expected: "нед ▓▓▓▓░░░░░░ 40%",
		},
		{
			name: "сразу после сброса ничего не потрачено", used: 0, now: windowStart,
			color: green, expected: "нед ░░░░░░░░░░ 0%",
		},
		{
			// Темп за нулевое время не делится: потраченное сразу — кончится
			name: "сразу после сброса уже потрачено", used: 3, now: windowStart,
			color: red, expected: "нед ░░░░░░░░░░ 3%",
		},
		{
			name: "часы машины раньше начала окна", used: 3, now: windowStart.Add(-time.Hour),
			color: red, expected: "нед ░░░░░░░░░░ 3%",
		},
		{
			// Сброс прошёл, замер старый: прогноз — то, что уже потрачено
			name: "замер после сброса", used: 40, now: weekResets.Add(2 * time.Hour),
			color: green, expected: "нед ▓▓▓▓░░░░░░ 40%",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := weeklyBar(weekBar{label: "нед"}, weeklyUsage{used: tt.used, resetsAt: weekResets}, tt.now)
			if plain(got) != tt.expected {
				t.Errorf("получено %q, ожидалось %q", plain(got), tt.expected)
			}
			if !strings.Contains(got, tt.color+"▓") && !strings.Contains(got, tt.color+"░") {
				t.Errorf("полоса не того цвета: %q", got)
			}
		})
	}
}

// Границы цвета: 80% ещё зелёный, 100% ещё жёлтый
func TestClassifyPace(t *testing.T) {
	tests := []struct {
		name      string
		projected float64
		expected  pace
	}{
		{name: "с запасом", projected: 50, expected: paceSafe},
		{name: "ровно 80", projected: 80, expected: paceSafe},
		{name: "чуть выше 80", projected: 80.01, expected: paceNear},
		{name: "ровно 100", projected: 100, expected: paceNear},
		{name: "чуть выше 100", projected: 100.01, expected: paceOver},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyPace(tt.projected); got != tt.expected {
				t.Errorf("classifyPace(%v) = %v, ожидалось %v", tt.projected, got, tt.expected)
			}
		})
	}
}

func TestDetailsWeekly(t *testing.T) {
	now := time.Date(2026, 10, 20, 15, 58, 0, 0, time.UTC)
	used, resets := 51.0, weekResets.Unix()

	input := Input{}
	input.RateLimits.SevenDay = &WeeklyLimit{UsedPercentage: &used, ResetsAt: &resets}
	parts := details(input, 47, weekBar{label: "нед"}, now)
	expected := []string{"ctx ▓▓▓▓░░░░░░ 47%", "нед ▓▓▓▓▓░░░░░ 51%"}
	if len(parts) != len(expected) {
		t.Fatalf("ожидалось %q, получено %q", expected, parts)
	}
	for i, want := range expected {
		if got := plain(parts[i]); got != want {
			t.Errorf("элемент %d: ожидалось %q, получено %q", i, want, got)
		}
	}

	// Без seven_day или без resets_at блока нет совсем, нулей вместо него тоже
	for name, limit := range map[string]*WeeklyLimit{
		"нет seven_day": nil,
		"нет resets_at": {UsedPercentage: &used},
		"нет процента":  {ResetsAt: &resets},
	} {
		input.RateLimits.SevenDay = limit
		for _, part := range details(input, 47, weekBar{label: "нед"}, now) {
			if strings.Contains(plain(part), "нед") {
				t.Errorf("%s: блок недели не должен выводиться, получено %q", name, plain(part))
			}
		}
	}
}
