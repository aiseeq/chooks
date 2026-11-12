package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/aiseeq/akit/shellcmd"
	"github.com/aiseeq/chooks/internal/core"
)

// gitGuardHint — что делать суб-агенту вместо запрещённой команды
const gitGuardHint = "суб-агенту такие git-команды запрещены: они стирают незакоммиченную работу или меняют запушенное, а суб-агент работает в общем дереве мастера. " +
	"Нужна такая — напиши об этом в итоге мастеру; для своей работы — свой worktree (graft worktree add), там можно"

// GitGuardTool отказывает суб-агентам Claude (не основной сессии) в
// git-командах, которые стирают незакоммиченную работу или меняют
// запушенное: reset --hard, restore, checkout -- путь, clean, stash,
// force-push, удаление ветки на remote. Основная сессия делает их по «да»
// пользователя; у бойцов worker они запрещены своим механизмом
// (пользователь 12.11: «пускай то, что требует моего разрешения делает только
// мастер», выбор 4а — запрет кодом всем суб-агентам). В своём linked
// worktree суб-агенту можно всё, кроме push, меняющего запушенное: его
// можно только --force-with-lease своей ветки (git.md: после graft worktree
// sync); во временном каталоге — всё
type GitGuardTool struct {
	*BaseTool
	// tempRoots — временные каталоги: репо в них — клон или проба
	// суб-агента; тесты подставляют свои
	tempRoots []string
}

// NewGitGuardTool создаёт запрет разрушительных git-команд суб-агентам
func NewGitGuardTool(config core.ToolConfig, logger core.Logger) (*GitGuardTool, error) {
	return &GitGuardTool{BaseTool: NewBaseTool("git_guard", config.Enabled, core.ShellTools, logger), tempRoots: tempRoots()}, nil
}

// ValidateTool проверяет git-вызовы команды суб-агента
func (t *GitGuardTool) ValidateTool(_ context.Context, input *core.ToolInput) (*core.ValidationResult, error) {
	if !t.IsEnabled() || input == nil || input.AgentID == "" || !strings.Contains(input.Command, "git") {
		return &core.ValidationResult{IsValid: true}, nil
	}
	calls, err := shellcmd.Calls(input.Command)
	if calls == nil {
		if err != nil {
			// Команду не разбирает и bash -n: оболочка её не выполнит
			t.Logger().Warn("git_guard: shellcmd не разобрал команду", "error", err)
		}
		return &core.ValidationResult{IsValid: true}, nil
	}
	home, err := os.UserHomeDir()
	//nolint:fallback-return // home нужен только для ~: без него путь с ~ неизвестен, а неизвестный каталог — чужое дерево, то есть отказ (безопасная сторона)
	if err != nil {
		t.Logger().Warn("git_guard: домашний каталог неизвестен", "error", err)
	}
	plan := planCommand(input.Command, input.CWD, home, calls)
	var reasons []string
	for i, c := range calls {
		if c.Name != "git" {
			continue
		}
		g := parseGit(c.Args, plan.cwds[i], home, func(p string) string { return plan.path(p, i) })
		why := g.destructive()
		if why == "" || inTemp(g.dir, t.tempRoots) || linkedWorktree(g.dir) && (g.sub != "push" || leasePush(g.args)) {
			continue
		}
		reasons = append(reasons, fmt.Sprintf("git %s — %s", g.sub, why))
	}
	if len(reasons) == 0 {
		return &core.ValidationResult{IsValid: true}, nil
	}
	message := strings.Join(reasons, "; ") + ". " + gitGuardHint
	v := core.NewViolation("git_guard", message, "", core.LevelCritical, 0, 0)
	return core.NewValidationResult(false, []core.Violation{v}, []string{message}), nil
}

// gitCall — вызов git: каталог репо, подкоманда и её аргументы
type gitCall struct {
	dir  string // каталог, где работает git; "" — не раскрыть до запуска
	sub  string
	args []string
}

// gitValueOpts — глобальные опции git со значением отдельным словом
var gitValueOpts = []string{"-c", "--namespace", "--exec-path", "--config-env"}

// parseGit снимает глобальные опции git: -C меняет каталог, --git-dir и
// --work-tree делают его неизвестным (своё дерево не опознать). expand
// подставляет в слово переменные, присвоенные в команде раньше вызова
func parseGit(args []string, cwd, home string, expand func(string) string) gitCall {
	g := gitCall{dir: cwd}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-C" && i+1 < len(args):
			i++
			g.dir = absPath(expand(args[i]), g.dir, home)
		case a == "--git-dir" || a == "--work-tree":
			i++
			g.dir = ""
		case strings.HasPrefix(a, "--git-dir=") || strings.HasPrefix(a, "--work-tree="):
			g.dir = ""
		case slices.Contains(gitValueOpts, a):
			i++
		case strings.HasPrefix(a, "-"):
		default:
			g.sub, g.args = a, args[i+1:]
			return g
		}
	}
	return g
}

// destructive — чем подкоманда опасна для чужой работы; "" — безопасна.
// Пропускается (решение 12.11): git stash list/show, restore --staged без
// --worktree (только снимает из индекса), checkout -b/--orphan (новая ветка),
// checkout и switch на ветку (переключение не стирает правки: git
// отказывает при конфликте), clean -n, reset без --hard/--merge/--keep
// (дерево не трогает), обычный push
func (g gitCall) destructive() string {
	switch g.sub {
	case "reset":
		if hasAny(g.args, "--hard", "--merge", "--keep") {
			return "стирает правки дерева"
		}
	case "restore":
		staged := hasAny(g.args, "--staged", "-S") || hasShort(g.args, 'S')
		worktree := hasAny(g.args, "--worktree", "-W") || hasShort(g.args, 'W')
		if !staged || worktree {
			return "стирает правки файлов"
		}
	case "checkout":
		return g.checkout()
	case "switch":
		switch {
		case hasAny(g.args, "-f", "--force", "--discard-changes"):
			return "стирает правки дерева"
		case hasAny(g.args, "-C", "--force-create"):
			return "переставляет существующую ветку"
		}
	case "clean":
		if !hasAny(g.args, "--dry-run") && !hasShort(g.args, 'n') {
			return "удаляет неотслеживаемые файлы"
		}
	case "stash":
		if ops := gitOperands(g.args); len(ops) == 0 || (ops[0] != "list" && ops[0] != "show") {
			return "убирает правки из дерева или стирает отложенные"
		}
	case "push":
		return pushDanger(g.args)
	}
	return ""
}

// checkout — опасная форма git checkout: путь (после -- или существующий
// файл), принудительный, по частям, -B
func (g gitCall) checkout() string {
	switch {
	case hasAny(g.args, "-f", "--force") || hasShort(g.args, 'f'):
		return "стирает правки дерева"
	case hasAny(g.args, "-p", "--patch"):
		return "стирает правки файла по частям"
	case hasAny(g.args, "-B"):
		return "переставляет существующую ветку"
	case slices.Contains(g.args, "--"):
		return "checkout -- путь стирает правки файла"
	case hasAny(g.args, "-b", "--orphan"):
		return ""
	}
	ops := gitOperands(g.args)
	switch {
	case len(ops) > 1:
		return "checkout <коммит> <путь> стирает правки файла"
	case len(ops) == 1 && (ops[0] == "." || g.isPath(ops[0])):
		return "checkout пути стирает правки файла; ветку переключай git switch"
	}
	return ""
}

// isPath — слово называет файл или каталог дерева. Каталог неизвестен —
// похожее на путь слово (с «/» или «*») считается путём
func (g gitCall) isPath(op string) bool {
	if g.dir == "" {
		return strings.ContainsAny(op, "/*")
	}
	_, err := os.Lstat(filepath.Join(g.dir, op))
	return err == nil
}

// pushDanger — push, меняющий запушенное: принудительный, зеркальный,
// удаление ветки на remote
func pushDanger(args []string) string {
	switch {
	case hasAny(args, "--force", "--mirror", "--force-if-includes") || hasShort(args, 'f') ||
		slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "--force-with-lease") }):
		return "принудительный push меняет запушенное"
	case hasAny(args, "--delete", "--prune") || hasShort(args, 'd'):
		return "удаляет ветку на remote"
	}
	for _, op := range gitOperands(args) {
		switch {
		case strings.HasPrefix(op, "+"):
			return "принудительный push меняет запушенное"
		case strings.HasPrefix(op, ":"):
			return "удаляет ветку на remote"
		}
	}
	return ""
}

// hasAny — среди слов есть одно из названных
func hasAny(args []string, names ...string) bool {
	return slices.ContainsFunc(args, func(a string) bool { return slices.Contains(names, a) })
}

// hasShort — короткий ключ в связке (-fd, -nx); только до «--»
func hasShort(args []string, flag rune) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if len(a) > 1 && a[0] == '-' && a[1] != '-' && strings.ContainsRune(a[1:], flag) {
			return true
		}
	}
	return false
}

// gitOperands — слова подкоманды, не опции, до «--»
func gitOperands(args []string) []string {
	var out []string
	for _, a := range args {
		if a == "--" {
			break
		}
		if !strings.HasPrefix(a, "-") {
			out = append(out, a)
		}
	}
	return out
}

// tempRoots — временные каталоги: репо в них — клон или проба суб-агента
// (scratchpad Claude Code — в CLAUDE_CODE_TMPDIR), деревья мастеров там не
// лежат. Сухой прогон 12.11: 112 из 122 отказов суб-агентам — их клоны в
// scratchpad
func tempRoots() []string {
	roots := []string{os.TempDir(), "/tmp"}
	if dir := os.Getenv("CLAUDE_CODE_TMPDIR"); dir != "" {
		roots = append(roots, dir)
	}
	return roots
}

// leasePush — push без опасных форм, кроме --force-with-lease (и
// --force-if-includes при нём): обновление своей ветки после sync
func leasePush(args []string) bool {
	rest := slices.DeleteFunc(slices.Clone(args), func(a string) bool {
		return strings.HasPrefix(a, "--force-with-lease") || a == "--force-if-includes"
	})
	return pushDanger(rest) == ""
}

// inTemp — каталог во временном: репо там — клон или проба суб-агента.
// Каталог неизвестен — не временный
func inTemp(dir string, temp []string) bool {
	if dir == "" {
		return false
	}
	for _, root := range temp {
		if rel, err := filepath.Rel(filepath.Clean(root), dir); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
			return true
		}
	}
	return false
}

// linkedWorktree — каталог внутри linked worktree (.git — файл): дерево
// суб-агента, не общее дерево мастера. Каталог неизвестен — не свой
func linkedWorktree(dir string) bool {
	if dir == "" {
		return false
	}
	for d := dir; ; d = filepath.Dir(d) {
		if info, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return info.Mode().IsRegular()
		}
		if filepath.Dir(d) == d {
			return false
		}
	}
}
