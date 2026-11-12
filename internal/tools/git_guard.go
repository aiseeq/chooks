package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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
// запушенное: reset --hard, restore, checkout пути, clean, stash,
// force-push, удаление ветки. Основная сессия делает их по «да»
// пользователя (пользователь 12.11: «пускай то, что требует моего разрешения
// делает только мастер», выбор 4а — запрет кодом всем суб-агентам).
// В своём linked worktree суб-агенту можно всё, что меняет только это
// дерево; общее для всех worktree репо (ветки, ссылки, remote) — нельзя,
// кроме push --force-with-lease своей ветки (git.md: после graft worktree
// sync). Во временном каталоге — всё: там клоны и пробы
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
	gits, ctx := gitCalls(input, calls, home)
	var reasons []string
	for i, c := range calls {
		if c.Name != "git" {
			continue
		}
		g := gits[i]
		why, shared := g.destructive()
		if why == "" || t.ownTree(g, shared, ctx) {
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

// gitConfigEnv — присваивание переменной, добавляющей git настройки
// (GIT_CONFIG_NOSYSTEM и GIT_CONFIG_GLOBAL только изолируют от файлов)
var gitConfigEnv = regexp.MustCompile(`GIT_CONFIG_(COUNT|KEY_\d+|PARAMETERS)=`)

// gitCalls разбирает git-вызовы команды (gits[i] — вызов calls[i]) и
// собирает её контекст
func gitCalls(input *core.ToolInput, calls []shellcmd.Call, home string) ([]gitCall, commandContext) {
	// GIT_DIR и GIT_WORK_TREE переносят git в другое репо: каталог неизвестен
	envRepo := strings.Contains(input.Command, "GIT_DIR") || strings.Contains(input.Command, "GIT_WORK_TREE")
	envConfig := gitConfigEnv.MatchString(input.Command)
	ctx := commandContext{master: sessionProject(input.TranscriptPath)}
	gits := make([]gitCall, len(calls))
	plan := planCommand(input.Command, input.CWD, home, calls)
	for i, c := range calls {
		switch c.Name {
		case "ln":
			ctx.links = true
		case "git":
			g := parseGit(c.Args, plan.cwds[i], home, func(p string) string { return plan.path(p, i) })
			g.config = g.config || envConfig
			if envRepo {
				g.dir = ""
			}
			ctx.branchMoved = ctx.branchMoved || g.sub == "switch" || g.sub == "checkout"
			gits[i] = g
		}
	}
	return gits, ctx
}

// commandContext — что в команде влияет на опознание своего дерева
type commandContext struct {
	master      string // каталог проекта основной сессии (sessionProject)
	links       bool   // команда создаёт ссылки (ln): несуществующий пока каталог может оказаться ссылкой
	branchMoved bool   // команда переключает ветку: HEAD worktree на момент push не тот, что сейчас
}

// ownTree — опасная подкоманда g работает в дереве суб-агента: каталог во
// временном или в linked worktree, где не работает основная сессия.
// shared — подкоманда меняет общее для всех worktree (ветки, ссылки,
// remote): в worktree можно только push --force-with-lease его ветки.
// Каталог неизвестен — не своё
func (t *GitGuardTool) ownTree(g gitCall, shared bool, ctx commandContext) bool {
	if g.dir == "" {
		return false
	}
	dir := g.dir
	real, err := filepath.EvalSymlinks(dir)
	switch {
	case err == nil:
		dir = real
	case ctx.links:
		// Каталога ещё нет, а команда делает ссылки: куда он ведёт — не узнать
		return false
	}
	if inTemp(dir, t.tempRoots) {
		return true
	}
	wt, err := linkedWorktree(dir)
	if err != nil {
		// Worktree не прочитать — не опознать своё: отказ (безопасная сторона)
		t.Logger().Warn("git_guard: worktree не прочитан", "dir", dir, "error", err)
		return false
	}
	switch {
	case wt.root == "" || masterIn(wt.root, ctx.master):
		return false
	case !shared:
		return true
	}
	return g.sub == "push" && !ctx.branchMoved && leasePush(g.args, wt.branch)
}

// gitCall — вызов git: каталог репо, подкоманда и её аргументы
type gitCall struct {
	dir  string // каталог, где работает git; "" — не раскрыть до запуска
	sub  string
	args []string
	// config — настройка из команды (-c, --config-env, GIT_CONFIG_*) может
	// сделать подкоманду алиасом или push принудительным
	config bool
}

// gitValueOpts — глобальные опции git со значением отдельным словом
var gitValueOpts = []string{"-c", "--namespace", "--exec-path", "--config-env"}

// riskyConfig — настройка git, меняющая смысл подкоманды: алиас, refspec
// или зеркало remote
func riskyConfig(setting string) bool {
	key, _, _ := strings.Cut(strings.ToLower(setting), "=")
	return strings.HasPrefix(key, "alias.") ||
		strings.HasPrefix(key, "remote.") && (strings.HasSuffix(key, ".push") || strings.HasSuffix(key, ".mirror"))
}

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
			if (a == "-c" || a == "--config-env") && i < len(args) && riskyConfig(args[i]) {
				g.config = true
			}
		case strings.HasPrefix(a, "--config-env="):
			g.config = g.config || riskyConfig(strings.TrimPrefix(a, "--config-env="))
		case strings.HasPrefix(a, "-"):
		default:
			g.sub, g.args = a, args[i+1:]
			return g
		}
	}
	return g
}

// destructive — чем подкоманда опасна для чужой работы ("" — безопасна) и
// меняет ли она общее для всех worktree репо (ветки, ссылки, remote)
func (g gitCall) destructive() (why string, shared bool) {
	if g.config {
		return "настройку из команды (алиас, push remote, GIT_CONFIG_*) не проверить", true
	}
	if check, ok := gitDangers[g.sub]; ok {
		return check(g)
	}
	return "", false
}

// gitDangers — проверки опасных подкоманд. Пропускается (решение 12.11):
// git stash list/show, restore --staged без --worktree (только снимает из
// индекса), checkout -b/--orphan (новая ветка), checkout и switch на ветку
// (переключение не стирает правки: git отказывает при конфликте), clean -n,
// reset без --hard/--merge/--keep (дерево не трогает), обычный push.
// worktree remove --force и branch -D не проверяются: сухой прогон 12.11 —
// 107 таких команд суб-агентов, все уборка их же временных worktree и
// веток проверки
var gitDangers = map[string]func(gitCall) (string, bool){
	"reset": func(g gitCall) (string, bool) {
		return when(hasLong(g.args, "--hard", "--merge", "--keep"), "стирает правки дерева"), false
	},
	"restore": func(g gitCall) (string, bool) {
		staged := hasLong(g.args, "--staged") || hasShort(g.args, 'S', "s")
		worktree := hasLong(g.args, "--worktree") || hasShort(g.args, 'W', "s")
		return when(!staged || worktree, "стирает правки файлов"), false
	},
	"checkout": func(g gitCall) (string, bool) { return g.checkout(), hasShort(g.args, 'B', "b") },
	"switch": func(g gitCall) (string, bool) {
		if hasLong(g.args, "--force", "--discard-changes") || hasShort(g.args, 'f', "cC") {
			return "стирает правки дерева", false
		}
		return when(hasLong(g.args, "--force-create") || hasShort(g.args, 'C', "c"), "переставляет существующую ветку"), true
	},
	"clean": func(g gitCall) (string, bool) {
		args := dropValues(g.args, "-e", "--exclude")
		return when(!hasLong(args, "--dry-run") && !hasShort(args, 'n', "e"), "удаляет неотслеживаемые файлы"), false
	},
	"stash": func(g gitCall) (string, bool) {
		ops := gitOperands(g.args)
		return when(len(ops) == 0 || (ops[0] != "list" && ops[0] != "show"), "убирает правки из дерева или стирает отложенные"), false
	},
	"rm": func(g gitCall) (string, bool) {
		// --cached трогает только индекс
		why, _ := forced("стирает правки файлов")(g)
		return when(!hasLong(g.args, "--cached"), why), false
	},
	"checkout-index": forced("стирает правки файлов"),
	"read-tree": func(g gitCall) (string, bool) {
		return when(hasLong(g.args, "--reset") && hasShort(g.args, 'u', ""), "стирает правки дерева"), false
	},
	"reflog": func(g gitCall) (string, bool) {
		ops := gitOperands(g.args)
		return when(len(ops) > 0 && (ops[0] == "expire" || ops[0] == "delete"), "стирает историю ссылок"), true
	},
	"push": func(g gitCall) (string, bool) { return pushDanger(g.args), true },
}

// when — why, если условие верно, иначе ""
func when(cond bool, why string) string {
	if cond {
		return why
	}
	return ""
}

// dropValues — слова без значений опций, данных отдельным словом
// (-e -n: «-n» — шаблон исключения, не ключ)
func dropValues(args []string, opts ...string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		out = append(out, args[i])
		if args[i] == "--" {
			out = append(out, args[i+1:]...)
			break
		}
		if slices.Contains(opts, args[i]) {
			i++
		}
	}
	return out
}

// forced — подкоманда опасна только с --force/-f
func forced(why string) func(gitCall) (string, bool) {
	return func(g gitCall) (string, bool) {
		return when(hasLong(g.args, "--force") || hasShort(g.args, 'f', ""), why), false
	}
}

// checkout — опасная форма git checkout: путь (после -- или существующий
// файл, шаблон, pathspec), принудительный, по частям, -B
func (g gitCall) checkout() string {
	switch {
	case hasLong(g.args, "--force") || hasShort(g.args, 'f', "bB"):
		return "стирает правки дерева"
	case hasLong(g.args, "--patch") || hasShort(g.args, 'p', "bB"):
		return "стирает правки файла по частям"
	case hasShort(g.args, 'B', "b"):
		return "переставляет существующую ветку"
	case slices.Contains(g.args, "--") || hasLong(g.args, "--pathspec-from-file"):
		return "checkout -- путь стирает правки файла"
	case hasLong(g.args, "--orphan") || hasShort(g.args, 'b', "B"):
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

// isPath — слово называет файл или каталог дерева. Шаблон и pathspec
// («*», «?», «[», «:») именем ветки быть не могут (git check-ref-format).
// Каталог неизвестен — похожее на путь слово (с «/») считается путём
func (g gitCall) isPath(op string) bool {
	if strings.ContainsAny(op, "*?[:") {
		return true
	}
	if g.dir == "" {
		return strings.Contains(op, "/")
	}
	_, err := os.Lstat(filepath.Join(g.dir, op))
	return err == nil
}

// pushDanger — push, меняющий запушенное: принудительный, зеркальный,
// удаление ветки на remote
func pushDanger(args []string) string {
	switch {
	case hasLong(args, "--force", "--mirror", "--force-if-includes", "--force-with-lease") || hasShort(args, 'f', "o"):
		return "принудительный push меняет запушенное"
	case hasLong(args, "--delete", "--prune") || hasShort(args, 'd', "o"):
		return "удаляет ветку на remote"
	}
	for _, op := range pushOperands(args) {
		switch {
		case strings.HasPrefix(op, "+"):
			return "принудительный push меняет запушенное"
		case strings.HasPrefix(op, ":"):
			return "удаляет ветку на remote"
		}
	}
	return ""
}

// leasePush — push --force-with-lease только ветки branch своего worktree
// (refspec без приёмника или с приёмником branch), без других опасных форм
func leasePush(args []string, branch string) bool {
	rest := slices.DeleteFunc(slices.Clone(args), func(a string) bool {
		name, _, _ := strings.Cut(a, "=")
		// «--force-» и короче — сокращение и для --force: не lease
		return len(name) > len("--force-") && (strings.HasPrefix("--force-with-lease", name) || strings.HasPrefix("--force-if-includes", name))
	})
	if branch == "" || pushDanger(rest) != "" || hasLong(rest, "--all", "--branches", "--tags") {
		return false
	}
	ops := pushOperands(rest)
	if len(ops) == 0 {
		return true
	}
	for _, refspec := range ops[1:] {
		src, dst, mapped := strings.Cut(refspec, ":")
		if !mapped {
			dst = src
		}
		dst = strings.TrimPrefix(dst, "refs/heads/")
		if dst != branch && !(dst == "HEAD" && !mapped) {
			return false
		}
	}
	return true
}

// pushOperands — remote и refspec push: слова не опции, и после «--» тоже
// (refspec с «-» не начинается)
func pushOperands(args []string) []string {
	return slices.DeleteFunc(slices.Clone(args), func(a string) bool { return strings.HasPrefix(a, "-") })
}

// hasLong — длинная опция из названных до «--», в том числе сокращённая:
// git принимает однозначное начало имени (--har, --forc). Неоднозначное git
// отвергает сам, так что начало любого из названных — уже опасно
func hasLong(args []string, names ...string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		name, _, _ := strings.Cut(a, "=")
		if len(name) < 3 || !strings.HasPrefix(name, "--") {
			continue
		}
		if slices.ContainsFunc(names, func(n string) bool { return strings.HasPrefix(n, name) }) {
			return true
		}
	}
	return false
}

// hasShort — короткий ключ в связке (-fd, -nx) до «--»; values — ключи со
// значением: после такого ключа остаток слова — значение (-bfix, -enode)
func hasShort(args []string, flag rune, values string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if len(a) < 2 || a[0] != '-' || a[1] == '-' {
			continue
		}
		for _, c := range a[1:] {
			if c == flag {
				return true
			}
			if strings.ContainsRune(values, c) {
				break
			}
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

// inTemp — каталог во временном: репо там — клон или проба суб-агента
func inTemp(dir string, temp []string) bool {
	for _, root := range temp {
		if rel, err := filepath.Rel(filepath.Clean(root), dir); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
			return true
		}
	}
	return false
}

// worktree — linked worktree: корень и ветка в нём ("" — HEAD отделён)
type worktree struct {
	root, branch string
}

// linkedWorktree — linked worktree, в котором лежит каталог: .git — файл
// со ссылкой в .git/worktrees основного репо (у подмодуля — .git/modules,
// он часть дерева мастера). Не linked worktree — пустой
func linkedWorktree(dir string) (worktree, error) {
	for d := dir; ; d = filepath.Dir(d) {
		dotGit := filepath.Join(d, ".git")
		if info, err := os.Lstat(dotGit); err == nil {
			if !info.Mode().IsRegular() {
				return worktree{}, nil
			}
			return readWorktree(d, dotGit)
		}
		if filepath.Dir(d) == d {
			return worktree{}, nil
		}
	}
}

// readWorktree читает ссылку .git-файла и ветку из HEAD worktree (HEAD
// отделён — ветки нет: push с lease не пройдёт)
func readWorktree(root, dotGit string) (worktree, error) {
	data, err := os.ReadFile(dotGit)
	if err != nil {
		return worktree{}, fmt.Errorf("чтение worktree %s: %w", root, err)
	}
	gitdir, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir: ")
	if !ok || !strings.Contains(filepath.ToSlash(gitdir), "/worktrees/") {
		return worktree{}, nil
	}
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(root, gitdir)
	}
	head, err := os.ReadFile(filepath.Join(gitdir, "HEAD"))
	if err != nil {
		return worktree{}, fmt.Errorf("чтение worktree %s: %w", root, err)
	}
	wt := worktree{root: root}
	if branch, ok := strings.CutPrefix(strings.TrimSpace(string(head)), "ref: refs/heads/"); ok {
		wt.branch = branch
	}
	return wt, nil
}

// sessionProject — каталог проекта основной сессии в ~/.claude/projects:
// её стартовый каталог, где не буквы и не цифры заменены на «-». Для
// суб-агента transcript_path — транскрипт основной сессии
func sessionProject(transcript string) string {
	if transcript == "" {
		return ""
	}
	return filepath.Base(filepath.Dir(transcript))
}

// encodeProject — имя каталога проекта Claude Code для пути
func encodeProject(path string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, path)
}

// masterIn — основная сессия начата в дереве root (или в нём самом): это
// дерево мастера. Кодирование с потерями: совпадение с соседом (x и x-y) —
// в сторону отказа
func masterIn(root, master string) bool {
	if master == "" {
		return false
	}
	enc := encodeProject(root)
	return master == enc || strings.HasPrefix(master, enc+"-")
}
