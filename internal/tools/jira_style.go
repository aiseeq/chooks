package tools

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/aiseeq/akit/shellcmd"

	"github.com/aiseeq/chooks/internal/core"
)

// jiraCommentEndpoint запрос, создающий или редактирующий комментарий в Jira
var jiraCommentEndpoint = regexp.MustCompile(`rest/api/[23]/issue/[^/\s"']+/comment`)

// jiraWriteRequest признак записи в тексте команды, которую shellcmd не
// разобрал: тело запроса или явный POST/PUT. Чтение комментариев (GET)
// проверять нельзя — тире в остальной команде не относится к Jira
var jiraWriteRequest = regexp.MustCompile(`(?i)--data(?:-binary|-raw)?\b|(?:^|\s)-d[=\s]|-X\s*(?:POST|PUT)\b|--request\s*(?:POST|PUT)\b`)

// sedSubstitution замена s/старое/новое/ в sed или perl с разделителем
// /, | или #: одна из групп - старый текст
var sedSubstitution = regexp.MustCompile(`(?:^|[\s'"])s(?:/((?:[^/\\\n]|\\.)*)/|\|((?:[^|\\\n]|\\.)*)\||#((?:[^#\\\n]|\\.)*)#)`)

// stripSedPatterns убирает из команды старый текст замен sed: правка
// комментария после блокировки ('s/три проекта/3 проекта/') несёт в шаблоне
// именно то, что заменяется. Новый текст остаётся и проверяется
func stripSedPatterns(command string) string {
	var b strings.Builder
	last := 0
	for _, m := range sedSubstitution.FindAllStringSubmatchIndex(command, -1) {
		for group := 1; group <= 3; group++ {
			if start := m[2*group]; start >= 0 {
				b.WriteString(command[last:start])
				last = m[2*group+1]
			}
		}
	}
	b.WriteString(command[last:])
	return b.String()
}

// maxPayloadFileSize файл больше этого размера телом комментария не является
const maxPayloadFileSize = 1 << 20

// JiraStyleTool блокирует отправку комментария в Jira с маркерами агентского стиля
type JiraStyleTool struct {
	*BaseTool
}

// NewJiraStyleTool создает валидатор стиля Jira-комментариев
func NewJiraStyleTool(config core.ToolConfig, logger core.Logger) (*JiraStyleTool, error) {
	return &JiraStyleTool{
		BaseTool: NewBaseTool("jira_style", config.Enabled, []string{"Bash"}, logger),
	}, nil
}

// ValidateTool проверяет команду, постящую комментарий в Jira
func (t *JiraStyleTool) ValidateTool(_ context.Context, input *core.ToolInput) (*core.ValidationResult, error) {
	if !t.IsEnabled() || input.ToolName != "Bash" || input.Command == "" {
		return &core.ValidationResult{IsValid: true}, nil
	}
	if !jiraCommentEndpoint.MatchString(input.Command) {
		return &core.ValidationResult{IsValid: true}, nil
	}

	calls, err := shellcmd.Calls(input.Command)
	if err != nil {
		// Вызовы до ошибки во вложенном скрипте всё равно проверяются
		t.Logger().Warn("jira_style: shellcmd не разобрал команду", "error", err)
	}
	if calls == nil && err != nil {
		// Команду не разобрать: вызов curl не выделить, отправкой считается
		// текст с адресом и признаком записи, файлы тела не читаются
		if !jiraWriteRequest.MatchString(input.Command) {
			return &core.ValidationResult{IsValid: true}, nil
		}
		return t.checkTexts(stripSedPatterns(input.Command), nil), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// ~ и cd без аргументов не раскрыть: такие пути тела - отказ
		t.Logger().Warn("jira_style: домашний каталог неизвестен", "error", err)
	}
	plan := planCommand(input.Command, input.CWD, home, calls)
	if plan.varsErr != nil {
		// Пути через переменные команды не раскрыть: такие файлы тела - отказ
		t.Logger().Warn("jira_style: присваивания команды не разобраны", "error", plan.varsErr)
	}
	if len(plan.sends) == 0 {
		return &core.ValidationResult{IsValid: true}, nil
	}

	// Текст комментария - раскодированные строки JSON (тело в -d '...',
	// heredoc или генераторе в той же команде) и файлы тела. Остальная
	// команда - код: шаблон grep, assert, комментарий shell. Левая часть
	// замены sed - старый текст, который команда и убирает
	var bodies, unreadable []string
	for i := range plan.sends {
		send := &plan.sends[i]
		for _, src := range plan.payloadFiles(send) {
			body, written, err := plan.readSource(src)
			switch {
			case written:
				continue
			case err != nil:
				unreadable = append(unreadable, fmt.Sprintf("%s (%s): %v", src.raw, describeSend(send), err))
				continue
			}
			bodies = append(bodies, body)
		}
	}
	result := t.checkTexts(stripSedPatterns(input.Command), bodies)
	if len(unreadable) == 0 {
		return result, nil
	}
	t.Logger().Info("jira payload file unavailable, send refused", "files", unreadable)
	// Непрочитанный файл - первым: ответ хука показывает первое сообщение,
	// а маркеры в видимой части тела идут подсказками следом
	violations := make([]core.Violation, 0, len(unreadable)+len(result.Violations))
	for _, u := range unreadable {
		violations = append(violations, core.NewViolation(
			"jira_comment_payload",
			"Тело комментария для Jira в файле, который хук не может прочитать до отправки:",
			u,
			core.LevelCritical,
			0,
			0,
		))
	}
	suggestions := append(slices.Clone(unreadable),
		"Дай путь к уже существующему файлу тела (-d @/абсолютный/путь; файл из генератора - создай отдельной командой до отправки) или запиши файл в этой же команде перед curl перенаправлением (cat > /абсолютный/путь <<'EOF' … EOF)")
	return core.NewValidationResult(false, append(violations, result.Violations...),
		append(suggestions, result.Suggestions...)), nil
}

// checkTexts ищет маркеры в теле комментария: строки JSON команды и файлов
// тела, сами файлы. Маркеры не только для текста ищутся и по всей команде
func (t *JiraStyleTool) checkTexts(command string, bodies []string) *core.ValidationResult {
	texts := jsonStrings(command)
	for _, body := range bodies {
		texts = append(texts, jsonStrings(body)...)
	}
	// Строки JSON первыми: фрагмент в подсказке из них читается без
	// JSON-обвязки
	texts = append(texts, bodies...)
	everywhere := append(slices.Clone(texts), command)

	var violations []core.Violation
	var suggestions []string
	for _, marker := range jiraStyleMarkers {
		scope := everywhere
		if marker.textOnly {
			scope = texts
		}
		for _, text := range scope {
			fragment := marker.Find(text)
			if fragment == "" {
				continue
			}
			suggestion := fmt.Sprintf("%s: %q, нужно: %s", marker.Description, strings.TrimSpace(fragment), marker.Fix)
			violations = append(violations, core.NewViolation(
				"jira_comment_style",
				// Сообщение одно на все нарушения: ответ хука показывает
				// первое, а список найденного идёт подсказками
				"Комментарий для Jira написан мимо правил формы write-as-user, исправить:",
				suggestion,
				core.LevelCritical,
				0,
				0,
			))
			suggestions = append(suggestions, suggestion)
			break
		}
	}
	if len(suggestions) > 0 {
		suggestions = append(suggestions, "Перепиши комментарий по скиллу write-as-user и отправь заново")
	}

	return core.NewValidationResult(len(violations) == 0, violations, suggestions)
}

// readPayloadFile читает файл с телом запроса по абсолютному пути. Ошибка
// называет причину, по которой файл не годится: нет файла, каталог, слишком
// большой, нечитаемый
func readPayloadFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("файла нет или он недоступен: %w", err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s - каталог", path)
	}
	if info.Size() > maxPayloadFileSize {
		return "", fmt.Errorf("%s: %d байт, больше %d", path, info.Size(), maxPayloadFileSize)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("файл не прочитать: %w", err)
	}
	return string(data), nil
}
