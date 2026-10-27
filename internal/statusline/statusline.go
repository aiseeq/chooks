package statusline

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"time"

	"github.com/aiseeq/chooks/internal/accounts"
	"github.com/aiseeq/chooks/internal/core"
	"github.com/aiseeq/chooks/internal/desktop"
)

// ANSI-последовательности. Строка статуса должна читаться боковым зрением,
// поэтому имя проекта выводится плашкой, а не обычным текстом.
//
// Цвета заданы палитрой из 256 значений, а не восьмицветной: базовые цвета
// терминала темы переопределяют, и жёлтый фон выходит грязно-коричневым
const (
	reset  = "\033[0m"
	dim    = "\033[2;37m"
	green  = "\033[38;5;77m"
	yellow = "\033[38;5;214m"
	red    = "\033[38;5;203m"
	cyan   = "\033[38;5;81m"

	badgeBlue  = "\033[1;38;5;231;48;5;33m"
	badgeAmber = "\033[1;38;5;16;48;5;214m"
	badgeGreen = "\033[1;38;5;231;48;5;28m"
	badgeLeaf  = "\033[1;38;5;231;48;5;71m"
	badgeSlate = "\033[1;38;5;231;48;5;61m"
	badgeRed   = "\033[1;38;5;231;48;5;160m"
)

// backgroundRunes — сколько знаков описания фоновой задачи помещается в строку
const backgroundRunes = 40

// contextShades — оттенки голубого от светлого к густому по четвертям
// заполнения. Полнота контекста — количество, а не оценка, поэтому без
// светофора (пользователь 26.10: «пустой контекст это не обязательно
// "хорошо", а полный - не обязательно "плохо" же»)
var contextShades = []string{"\033[38;5;153m", "\033[38;5;111m", "\033[38;5;69m", "\033[38;5;33m"}

// barSegments — клеток в полосе доли; клетка — 10%
const barSegments = 10

// weekWindow — недельное окно лимита: 168 часов до resets_at
const weekWindow = 168 * time.Hour

// weekNearPercent — прогноз на сброс, выше которого неделя желтеет: лимита
// хватит, но запаса почти нет
const weekNearPercent = 80

// Input описывает данные, которые Claude Code передаёт строке статуса
type Input struct {
	SessionID string `json:"session_id"`
	CWD       string `json:"cwd"`

	Model struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
	} `json:"model"`

	Workspace struct {
		CurrentDir string `json:"current_dir"`
		ProjectDir string `json:"project_dir"`
	} `json:"workspace"`

	// ContextWindow — заполнение окна модели. used_percentage — доля всего
	// окна, а не порога автосжатия: полоса считает свою долю по токенам
	ContextWindow struct {
		UsedPercentage float64       `json:"used_percentage"`
		Size           int           `json:"context_window_size"`
		CurrentUsage   *ContextUsage `json:"current_usage"`
	} `json:"context_window"`

	Cost struct {
		LinesAdded   int `json:"total_lines_added"`
		LinesRemoved int `json:"total_lines_removed"`
	} `json:"cost"`

	Effort struct {
		Level string `json:"level"`
	} `json:"effort"`

	RateLimits struct {
		SevenDay *WeeklyLimit `json:"seven_day"`
		FiveHour *WeeklyLimit `json:"five_hour"`
	} `json:"rate_limits"`
}

// ContextUsage — токены последнего запроса; контекст — вход вместе с кэшем
type ContextUsage struct {
	InputTokens              int `json:"input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// contextUsed — заполнение контекста в процентах порога автосжатия: 100% —
// сейчас сожмётся (пользователь 25.10: «полоску контекста отрегулировать»).
// Порога нет (сжатие выключено) или нет токенов во входе — доля всего окна
func contextUsed(input Input, threshold int) float64 {
	usage := input.ContextWindow.CurrentUsage
	if threshold <= 0 || usage == nil {
		return input.ContextWindow.UsedPercentage
	}
	tokens := usage.InputTokens + usage.CacheCreationInputTokens + usage.CacheReadInputTokens
	return float64(tokens) * 100 / float64(threshold)
}

// contextFill — заполнение контекста до порога автосжатия по settings.json
// из claudeDir и окружению Claude Code
func contextFill(input Input, claudeDir string, logger core.Logger) float64 {
	settings, err := loadCompactSettings(claudeDir)
	if err != nil {
		// Явный failover: без settings порог штатный — по полному окну модели
		logger.Warn("settings unreadable, context bar uses the default compaction window", "error", err)
	}
	return contextUsed(input, compactThreshold(input.ContextWindow.Size, input.Model.ID, settings, os.Getenv))
}

// Render читает данные Claude Code и возвращает строку статуса.
// Попутно обновляется заголовок окна: строка статуса видна только в активном
// окне, а по заголовку сессию видно в панели задач и в переключателе окон.
func Render(ctx context.Context, stdin io.Reader, logger core.Logger) (string, error) {
	data, err := io.ReadAll(stdin)
	if err != nil {
		return "", fmt.Errorf("failed to read status line input: %w", err)
	}

	var input Input
	if err := json.Unmarshal(data, &input); err != nil {
		return "", fmt.Errorf("failed to parse status line input: %w", err)
	}

	now := time.Now()
	store, paths := loadAccounts(logger)
	owner := ""
	var snapshot *limitsSnapshot
	limit := input.RateLimits.SevenDay
	if week, ok := limit.usage(); ok {
		smp := sample{week: &week, session: input.SessionID, account: store.ActiveEmail()}
		if five, ok := input.RateLimits.FiveHour.usage(); ok {
			smp.five = &five
		}
		res, err := recordLimits(smp, now)
		switch {
		case err != nil:
			// Явный failover: снимок не сведён, полоса рисуется по доле самой сессии
			logger.Warn("weekly limit not saved, showing the session one", "error", err)
		case res.verdict == weekStale:
			// Полоса показывает свежайшую долю своей недели: своя доля
			// сессии — из её последнего ответа API, он мог быть часы назад.
			// В лог не пишется: строка статуса рисуется чаще раза в секунду на
			// сессию, и эта запись вытесняла из лога всё остальное за часы
			input.RateLimits.SevenDay = res.merged.limit()
		}
		owner, snapshot = res.owner, res.snapshot
	} else if limit != nil {
		logger.Warn("weekly limit incomplete or invalid, not shown",
			"has_used_percentage", limit.UsedPercentage != nil, "has_resets_at", limit.ResetsAt != nil)
	}
	if active := store.ActiveEmail(); active != "" && owner != active {
		// Доля во входе — из последнего ответа API сессии: после переключения
		// он от прежней учётки, а после /login его нет вовсе, пока сессия не
		// сделает запрос. Полоса показывает неделю учётки, что сейчас у Claude
		// Code, если она известна
		if snapshot == nil {
			var err error
			if snapshot, err = diskLimits(); err != nil {
				// Сбой не срывает строку статуса: полосы нет, сбой — в лог
				logger.Warn("limits snapshot unreadable, weekly bar of the active account not shown", "error", err)
			}
		}
		if week, ok := snapshot.weekOf(active, now); ok {
			input.RateLimits.SevenDay = week.limit()
			owner = active
		}
	}
	if store != nil {
		rotate(paths, store, snapshot, now, logger)
	}
	bar := weekBar{label: "нед"}
	if store != nil && len(store.Accounts) > 1 && owner != "" {
		// Учёток несколько: полоса подписана учёткой, чья неделя, без «нед»
		// (пользователь 22.10: достаточно «<учётка> ▓▓▓▓▓▓▓▓▓░ 96%»)
		bar.label = store.ShortName(owner)
	}
	if store != nil && len(store.Accounts) > 0 {
		bar.pool = poolPace(store, snapshot, now, logger)
	}

	visible := func() (bool, error) { return desktop.TabVisible(ctx) }
	line, title := build(input, contextFill(input, paths.ClaudeDir, logger), bar, now, visible, logger)
	if err := desktop.SetTerminalTitle(title); err != nil {
		logger.Warn("terminal title not set", "error", err)
	}

	return line, nil
}

// build собирает строку статуса и заголовок окна. Сбой чтения состояния
// сессии строку не срывает: она рисуется по тому, что удалось прочитать, а
// сбой уходит в лог. Ветки и worktree в строке нет (пользователь 24.10 о
// строке всех агентов: «Хочу видеть только вот это»)
func build(input Input, ctxUsed float64, bar weekBar, now time.Time, visible func() (bool, error), logger core.Logger) (string, string) {
	dir := sessionDir(input)
	state, recorded, err := core.LookupSessionState(input.SessionID)
	if err != nil {
		// Явный failover: без записи состояние помечается неизвестным, и
		// плашка остаётся нейтральной — ни «ждёт ответа», ни «готово»
		logger.Warn("session state unavailable", "session", input.SessionID, "error", err)
		state = core.StateUnknown
	} else if !recorded && input.SessionID == "" {
		state = core.StateWorking
	} else if !recorded {
		// Запросов ещё не было: сессия только открыта или сброшена /clear и
		// стоит на пустом приглашении. Это не работа (26.10 после /clear
		// projectA стоял пустым с ⚙️, прежним значком работы), а показ — не запись: хуки её не видят
		state = core.StateIdle
	}
	// Фоновая работа известна на конец хода: пока ход идёт, список устарел
	var background core.Background
	if state == core.StatePaused || state == core.StateDone || state == core.StateIdle {
		background, err = core.LoadBackground(input.SessionID)
		if err != nil {
			// Сбой не срывает строку: фон не показывается, сбой — в лог
			logger.Warn("background tasks unreadable, not shown", "session", input.SessionID, "error", err)
		}
	}
	if state == core.StatePaused && background.Abandoned(now) {
		state = abandon(input.SessionID, logger)
	}
	if state == core.StatePaused && background.AgentsWorking(now) {
		state = core.StateAgents
	}
	if state == core.StateDone {
		state = seen(input.SessionID, visible, logger)
	}

	// Путь не показывается: плашка уже называет проект, а полный путь
	// повторяет её и занимает место
	var output strings.Builder
	output.WriteString(projectBadge(dir, state))

	parts := details(input, ctxUsed, bar, now)
	if part := backgroundPart(background, now); part != "" {
		parts = append(parts, part)
	}
	for _, part := range parts {
		output.WriteString(dim + " · " + reset + part)
	}

	return output.String(), terminalTitle(dir, state)
}

// abandon переводит паузу в «готово», когда все задачи сессии висят дольше
// срока, а будильников нет: Claude их больше не ждёт, а конец хода был
// раньше, чем задачи стали брошенными. Без этого вкладка показывала бы паузу
// до следующего хода
func abandon(sessionID string, logger core.Logger) core.SessionState {
	swapped, err := core.SwapSessionState(sessionID, core.StatePaused, core.StateDone)
	if err != nil {
		logger.Warn("abandoned background tasks, pause kept", "session", sessionID, "error", err)
		return core.StatePaused
	}
	if !swapped {
		return reloadState(sessionID, logger)
	}
	logger.Info("background tasks abandoned, session done", "session", sessionID)
	return core.StateDone
}

// backgroundPart показывает, что держит сессию после хода: первая задача с
// описанием и возрастом — задача агентов, если есть, — число остальных и
// будильники с расписаниями. Пусто — держать нечему, сессию можно сжимать и сбрасывать
func backgroundPart(background core.Background, now time.Time) string {
	var items []string
	if len(background.Tasks) > 0 {
		task := background.Tasks[0]
		for _, candidate := range background.Tasks {
			if candidate.Agent() && !candidate.Abandoned(now) {
				task = candidate
				break
			}
		}
		item := truncateRunes(task.Description, backgroundRunes)
		if item == "" {
			item = task.Type
		}
		item += " " + age(now.Sub(task.Since))
		if task.Abandoned(now) {
			item += " " + yellow + "брошена" + reset
		}
		if more := len(background.Tasks) - 1; more > 0 {
			item += fmt.Sprintf(" +%d", more)
		}
		items = append(items, item)
	}
	switch crons := background.Crons; {
	case crons == 1:
		items = append(items, "будильник")
	case crons > 1:
		items = append(items, fmt.Sprintf("будильников: %d", crons))
	}
	switch schedules := background.Schedules; {
	case schedules == 1:
		items = append(items, "расписание")
	case schedules > 1:
		items = append(items, fmt.Sprintf("расписаний: %d", schedules))
	}
	if len(items) == 0 {
		return ""
	}
	return dim + "фон: " + reset + strings.Join(items, ", ")
}

// age — возраст задачи до минуты: «14 мин», «3 ч 28 мин»
func age(d time.Duration) string {
	minutes := int(d.Minutes())
	if minutes < 60 {
		return fmt.Sprintf("%d мин", minutes)
	}
	return fmt.Sprintf("%d ч %d мин", minutes/60, minutes%60)
}

// truncateRunes обрезает текст до limit знаков с многоточием
func truncateRunes(text string, limit int) string {
	runes := []rune(strings.TrimSpace(text))
	if len(runes) <= limit {
		return string(runes)
	}
	return strings.TrimSpace(string(runes[:limit-1])) + "…"
}

// seen переводит законченный ход в «посмотрел», если человек видит вкладку.
// Строка статуса — единственное, что запускается, пока сессия стоит
// (statusLine.refreshInterval), поэтому смотрит она. Сбой — «новое» остаётся:
// лишний значок лучше пропущенного ответа
func seen(sessionID string, visible func() (bool, error), logger core.Logger) core.SessionState {
	shown, err := visible()
	if err != nil {
		logger.Warn("tab visibility unknown, keeping the turn unseen", "session", sessionID, "error", err)
		return core.StateDone
	}
	if !shown {
		return core.StateDone
	}
	swapped, err := core.SwapSessionState(sessionID, core.StateDone, core.StateIdle)
	if err != nil {
		logger.Warn("session not marked seen", "session", sessionID, "error", err)
		return core.StateDone
	}
	if !swapped {
		return reloadState(sessionID, logger)
	}
	return core.StateIdle
}

// reloadState перечитывает состояние, которое хук записал между чтением и
// заменой: им и рисовать. Не прочиталось — значка нет, сбой в лог
func reloadState(sessionID string, logger core.Logger) core.SessionState {
	state, err := core.LoadSessionState(sessionID)
	if err != nil {
		logger.Warn("session state changed but unreadable, no badge", "session", sessionID, "error", err)
		return core.StateUnknown
	}
	return state
}

// terminalTitle собирает заголовок окна: значок состояния и проект.
// Значок идёт первым — по нему окно находится взглядом в панели задач; ветка
// есть в строке статуса, во вкладке она вытесняет имя проекта
func terminalTitle(dir string, state core.SessionState) string {
	return state.Marker() + " " + core.ProjectNameForDir(dir)
}

// projectBadge рисует имя проекта плашкой, цвет которой показывает состояние:
// синяя — работает, в том числе своими агентами после хода, сине-серая — ход
// кончился и ждёт внешнего: shell, монитор, будильник (что именно — в
// «фон:»), янтарная — ждёт ответа человека, тёмно-зелёная — новое, что
// он ещё не видел, светло-зелёная, как ✅, — просмотрена (пользователь 26.10:
// «давай сделаем зелёным в режиме "зелёная галочка"»), красная — ход оборван
// ошибкой API. Заполнение контекста видно по полосе ctx, плашка его не
// повторяет (пользователь 26.10: убрать красный с плашки)
func projectBadge(dir string, state core.SessionState) string {
	badge := badgeBlue
	switch state {
	case core.StatePaused:
		badge = badgeSlate
	case core.StateWaiting:
		badge = badgeAmber
	case core.StateDone:
		badge = badgeGreen
	case core.StateIdle:
		badge = badgeLeaf
	case core.StateError:
		badge = badgeRed
	}

	name := strings.ToUpper(core.ProjectNameForDir(dir))
	return badge + " " + name + " " + reset
}

// details собирает вторую строку из доступных сведений
func details(input Input, ctxUsed float64, bar weekBar, now time.Time) []string {
	var parts []string

	if model := shortModel(input.Model.DisplayName); model != "" {
		if effort := input.Effort.Level; effort != "" {
			model += "·" + effort
		}
		parts = append(parts, cyan+model+reset)
	}

	if ctxUsed > 0 {
		parts = append(parts, contextBar(ctxUsed))
	}

	if week, ok := input.RateLimits.SevenDay.usage(); ok {
		parts = append(parts, weeklyBar(bar, week, now))
	}

	if added, removed := input.Cost.LinesAdded, input.Cost.LinesRemoved; added > 0 || removed > 0 {
		parts = append(parts, fmt.Sprintf("%s+%d%s %s−%d%s", green, added, reset, red, removed, reset))
	}

	return parts
}

// shortModel убирает уточнение в скобках: «Opus 5 (1M context)» занимает
// в одной строке заметное место, а размер контекста и так виден по полосе
func shortModel(model string) string {
	if open := strings.Index(model, " ("); open > 0 {
		model = model[:open]
	}
	return strings.TrimSpace(model)
}

// contextBar рисует полосу заполнения контекста до порога автосжатия
func contextBar(used float64) string {
	shade := min(max(int(used)*len(contextShades)/100, 0), len(contextShades)-1)
	return meter("ctx", used, contextShades[shade], "")
}

// weekBar — подпись полосы недели и прогноз пула учёток; pool nil —
// прогноза пула нет, цвет — по прогнозу одной недели
type weekBar struct {
	label string
	pool  *pace
}

// weeklyBar рисует долю недельного лимита активной учётки. Цвет — прогноз:
// пула учёток, если он есть, иначе недели на сброс. Значка нет —
// пользователю цвета достаточно (22.10: «мне цвета достаточно в целом»)
func weeklyBar(bar weekBar, week weeklyUsage, now time.Time) string {
	p := weeklyPace(week, now)
	if bar.pool != nil {
		p = *bar.pool
	}
	switch p {
	case paceOver:
		return meter(bar.label, week.used, red, "")
	case paceNear:
		return meter(bar.label, week.used, yellow, "")
	default:
		return meter(bar.label, week.used, green, "")
	}
}

// poolPace — прогноз пула учёток для цвета полосы. Сбой (нет снимка,
// короткая история) — явный failover на прогноз одной недели, сбой — в лог
func poolPace(store *accounts.Store, snapshot *limitsSnapshot, now time.Time, logger core.Logger) *pace {
	path, err := LimitsFile()
	if err == nil && snapshot == nil {
		snapshot, err = diskLimits()
	}
	var f PoolForecast
	if err == nil {
		f, err = forecastPool(store, snapshot, limitsHistory(path), now)
	}
	if err != nil {
		logger.Info("pool forecast unavailable, weekly bar colored by the active week", "error", err)
		return nil
	}
	var p pace
	switch f.Forecast.Pace {
	case accounts.PaceSafe:
		p = paceSafe
	case accounts.PaceNear:
		p = paceNear
	case accounts.PaceOver:
		p = paceOver
	}
	return &p
}

// meter рисует долю полосой: подпись, клетки, проценты и пометка. Одна форма
// для контекста и недели, цвет выбирает вызывающий
func meter(label string, used float64, color, mark string) string {
	if used > 100 {
		used = 100
	}

	// Клетки — от того же округления, что и проценты: 99,6% — «100%» и полная полоса
	filled := int(math.Round(used)) * barSegments / 100
	bar := strings.Repeat("▓", filled) + strings.Repeat("░", barSegments-filled)
	return fmt.Sprintf("%s%s %s%s %.0f%%%s%s", dim, label, color, bar, used, mark, reset)
}

// pace — прогноз недельного лимита
type pace int

const (
	// paceSafe — прогноз на сброс до 80%: хватит с запасом
	paceSafe pace = iota
	// paceNear — прогноз больше 80% и до 100%: хватит впритык
	paceNear
	// paceOver — прогноз больше 100%: лимит кончится до сброса
	paceOver
)

// weeklyPace прогнозирует неделю по среднему темпу с начала окна.
// Окно — 168 часов до resets_at; now передаёт вызывающий, чтобы расчёт
// не зависел от часов машины
func weeklyPace(week weeklyUsage, now time.Time) pace {
	elapsed := now.Sub(week.resetsAt.Add(-weekWindow)).Hours()
	if elapsed <= 0 {
		// Окно только началось: темп за нулевое время не определён. Ничего
		// не потрачено — хватит; уже потрачено — темп без предела, кончится
		if week.used > 0 {
			return paceOver
		}
		return paceSafe
	}

	// Сброс уже прошёл, а замер старый: часов до сброса не осталось
	remaining := max(week.resetsAt.Sub(now).Hours(), 0)
	rate := week.used / elapsed
	return classifyPace(week.used + rate*remaining)
}

// classifyPace относит прогноз на сброс (%) к цвету
func classifyPace(projected float64) pace {
	switch {
	case projected > 100:
		return paceOver
	case projected > weekNearPercent:
		return paceNear
	default:
		return paceSafe
	}
}

// sessionDir — каталог, где запущена сессия: им называется проект в плашке и
// заголовке. Текущий каталог агента уходит в подпапки и чужие проекты (cd по
// ходу работы), и заголовок вкладки тогда называл бы не ту сессию
func sessionDir(input Input) string {
	return firstDir(input.Workspace.ProjectDir, input.CWD, input.Workspace.CurrentDir)
}

func firstDir(candidates ...string) string {
	for _, candidate := range candidates {
		if candidate != "" {
			return candidate
		}
	}

	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}

// loadAccounts читает хранилище учёток. Сбой не срывает строку статуса:
// неделя пишется без учётки, ротации нет
func loadAccounts(logger core.Logger) (*accounts.Store, accounts.Paths) {
	paths, err := accounts.DefaultPaths()
	if err != nil {
		logger.Warn("accounts paths unavailable, no rotation", "error", err)
		return nil, paths
	}
	store, err := accounts.Load(paths)
	if err != nil {
		logger.Warn("accounts store unreadable, no rotation", "error", err)
		return nil, paths
	}
	return store, paths
}

// rotateArgs — подкоманда фоновой ротации учёток
var rotateArgs = []string{"account", "rotate"}

// startRotation запускает фоновую ротацию; тесты подменяют его, чтобы не
// запускать свой бинарь
var startRotation = accounts.MaybeRotate

// rotate запускает фоновую ротацию учёток, если ей есть работа. Снимок из
// отрисовки, а без замера недели — с диска
func rotate(paths accounts.Paths, store *accounts.Store, snapshot *limitsSnapshot, now time.Time, logger core.Logger) {
	var usage map[string]accounts.Usage
	if snapshot != nil {
		usage = accountUsage(snapshot, now)
	} else {
		var err error
		if usage, err = (SnapshotLimits{}).Usage(); err != nil {
			logger.Warn("limits snapshot unreadable, no rotation", "error", err)
			return
		}
	}
	executable, err := os.Executable()
	if err != nil {
		logger.Warn("own executable unknown, no rotation", "error", err)
		return
	}
	why, err := startRotation(paths, store, usage, now, executable, rotateArgs...)
	if err != nil {
		logger.Warn("account rotation not started", "error", err)
		return
	}
	if why != "" {
		logger.Info("account rotation started", "reason", why)
	}
}
