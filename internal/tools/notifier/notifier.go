package notifier

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aiseeq/chooks/internal/core"
	"github.com/aiseeq/chooks/internal/desktop"
)

// WatchCommand имя скрытой подкоманды, которая доставляет оповещение в фоне
const WatchCommand = "deliver-alert"

// Время жизни уведомлений: запрос ответа висит дольше, его ждёт человек
const (
	stopTimeout         = 10 * time.Second
	notificationTimeout = 30 * time.Second
)

// Notifier показывает человеку, что сессия закончила работу или ждёт ответа:
// заголовок окна, звук и уведомление с переходом к окну по клику
type Notifier struct {
	logger          core.Logger
	sound           bool
	desktop         bool
	activateOnClick bool
}

// New создает notifier по конфигурации
func New(config core.ToolConfig, logger core.Logger) *Notifier {
	return &Notifier{
		logger:          logger.With("tool", "notifier"),
		sound:           config.Sound,
		desktop:         config.Desktop,
		activateOnClick: config.ActivateOnClick,
	}
}

// Announce доводит решение до человека. Заголовок окна меняется на каждом
// переходе в «готово» или «ждёт ответа», даже без звонка: это подсказка в
// панели задач. Звук и уведомление уходят в отдельный процесс — хук не ждёт
// ни звука, ни клика
func (n *Notifier) Announce(eventName string, input *core.ToolInput, decision Decision) error {
	if decision.State != core.StateDone && decision.State != core.StateWaiting && decision.State != core.StateError {
		return nil
	}

	projectName := n.ProjectName(input)
	alert, err := n.buildAlert(eventName, input, projectName)
	if err != nil {
		return err
	}

	// Значок — по новому состоянию, как в строке статуса: минутное
	// напоминание без конца хода — это «готово», а не вопрос.
	// Без заголовка уведомление всё равно уходит
	if err := desktop.SetTerminalTitle(decision.State.Marker() + " " + projectName); err != nil {
		n.logger.Warn("terminal title not set", "error", err)
	}

	if !decision.Alert || !(alert.Sound || alert.Desktop) {
		return nil
	}

	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to locate own executable: %w", err)
	}
	if err := desktop.DeliverInBackground(executable, WatchCommand, alert); err != nil {
		return err
	}

	// Info, а не Debug: по этой записи отличают уведомление хука от
	// собственных уведомлений Claude Code — со стороны они неразличимы
	n.logger.Info("alert delivered",
		"event", eventName,
		"project", projectName,
		"activate_pids", len(alert.ActivatePIDs),
	)
	return nil
}

// buildAlert собирает оповещение под конкретное событие
func (n *Notifier) buildAlert(eventName string, input *core.ToolInput, projectName string) (desktop.Alert, error) {
	alert := desktop.Alert{
		AppName:     "Claude Code",
		Icon:        "utilities-terminal",
		Urgency:     desktop.UrgencyNormal,
		Sound:       n.sound,
		Desktop:     n.desktop,
		ActionLabel: "Перейти к окну",
	}

	if n.activateOnClick {
		// Окно принадлежит одному из предков: сам хук окна не имеет
		ancestors, err := desktop.ProcessAncestors(os.Getpid())
		if err != nil {
			// Best effort: собранная часть цепочки — настоящие PID, окно может
			// быть у них; без окна клик просто ничего не активирует
			n.logger.Warn("process ancestry incomplete", "error", err)
		}
		alert.ActivatePIDs = ancestors
	}

	switch eventName {
	case core.EventStop:
		alert.Title = "Claude Code завершил работу"
		alert.Message = "Проект: " + projectName
		alert.Timeout = stopTimeout

	case core.EventStopFailure:
		alert.Title = fmt.Sprintf("Claude Code остановлен ошибкой (%s)", projectName)
		alert.Message = input.Error
		if input.ErrorDetails != "" {
			alert.Message += ": " + input.ErrorDetails
		}
		alert.Timeout = stopTimeout

	case core.EventNotification:
		alert.Title = fmt.Sprintf("Claude Code ждёт ответа (%s)", projectName)
		// Claude Code сообщает, чего именно ждёт: разрешения на инструмент или ввода
		alert.Message = input.Message
		if alert.Message == "" {
			alert.Message = "Проект: " + projectName
		}
		alert.Timeout = notificationTimeout

	default:
		return desktop.Alert{}, fmt.Errorf("notifier does not handle event %q", eventName)
	}

	return alert, nil
}

// ProjectName определяет имя проекта по каталогу, где запущена сессия: его
// кодирует каталог транскрипта. Текущий каталог (cwd) агент меняет по ходу
// работы, поэтому он только запасной вариант
func (n *Notifier) ProjectName(input *core.ToolInput) string {
	if transcriptPath := input.TranscriptPath; transcriptPath != "" {
		encoded := filepath.Base(filepath.Dir(transcriptPath))
		if dir := decodeProjectDir(encoded, n.logger); dir != "" {
			return core.ProjectNameForDir(dir)
		}
	}

	if input.CWD != "" {
		return core.ProjectNameForDir(input.CWD)
	}

	if wd, err := os.Getwd(); err == nil {
		return core.ProjectNameForDir(wd)
	}

	return "unknown"
}

// decodeProjectDir восстанавливает путь проекта из имени каталога транскриптов
// Claude Code: всё, кроме латинских букв и цифр, он заменяет дефисом
// (/home/user/.cache/my-app → -home-user--cache-my-app, видно по
// ~/.claude/projects). Кодирование неоднозначно, поэтому путь ищется по
// записям каталогов: на каждом уровне — каталог, чьё закодированное имя
// продолжает остаток, с возвратом из тупика. Так различаются
// ~/work/projecta/frontend/admin-app и ~/work/chooks, а имя с UUID не рвётся
// на части. Два подходящих пути — неоднозначность, "" (ProjectName берёт
// cwd); ни одного (каталог снесён или переименован) — от самого глубокого
// найденного каталога остаток с дефисами-разделителями
func decodeProjectDir(encoded string, logger core.Logger) string {
	rest := strings.TrimPrefix(encoded, "-")
	if rest == "" || rest == "." || rest == string(filepath.Separator) {
		return ""
	}
	root := string(filepath.Separator)
	var found []string
	deepest, deepestRest := root, rest
	var walk func(dir, rest string)
	walk = func(dir, rest string) {
		if len(found) > 1 {
			return
		}
		if len(dir) > len(deepest) {
			deepest, deepestRest = dir, rest
		}
		for _, child := range childDirs(dir, rest, logger) {
			name := encodePathPart(child)
			next := filepath.Join(dir, child)
			if rest == name {
				found = append(found, next)
				continue
			}
			walk(next, rest[len(name)+1:])
		}
	}
	walk(root, rest)

	switch len(found) {
	case 1:
		return found[0]
	case 0:
		return filepath.Join(deepest, strings.ReplaceAll(deepestRest, "-", string(filepath.Separator)))
	default:
		return ""
	}
}

// childDirs — подкаталоги dir, чьё закодированное имя начинает остаток rest
// целым сегментом. Каталог без права чтения (x без r, чужой home 711) списка не
// даёт: тогда пробуются имена, где дефисы остатка — буквальные дефисы, через
// stat, как в прежнем декодере
func childDirs(dir, rest string, logger core.Logger) []string {
	matches := func(name string) bool {
		encoded := encodePathPart(name)
		return rest == encoded || strings.HasPrefix(rest, encoded+"-")
	}
	var names []string
	entries, err := os.ReadDir(dir)
	if err != nil {
		logger.Debug("project dir decode: directory not listable, probing names", "dir", dir, "error", err)
		segments := strings.Split(rest, "-")
		for k := 1; k <= len(segments); k++ {
			name := strings.Join(segments[:k], "-")
			if name != "" && isDir(filepath.Join(dir, name)) {
				names = append(names, name)
			}
		}
		return names
	}
	for _, entry := range entries {
		if !matches(entry.Name()) {
			continue
		}
		if entry.IsDir() || (entry.Type()&os.ModeSymlink != 0 && isDir(filepath.Join(dir, entry.Name()))) {
			names = append(names, entry.Name())
		}
	}
	return names
}

// encodePathPart кодирует имя, как Claude Code каталог транскриптов: всё,
// кроме латинских букв и цифр, — дефис, по одному на кодовую единицу UTF-16
func encodePathPart(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r > 0xFFFF:
			b.WriteString("--")
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// isDir сообщает, существует ли каталог по указанному пути
func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
