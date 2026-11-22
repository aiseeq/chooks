// Package modellaunch checks the model of the Claude process that invoked a hook.
package modellaunch

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"mvdan.cc/sh/v3/shell"

	"github.com/aiseeq/chooks/internal/core"
	"github.com/aiseeq/chooks/internal/tools"
)

const (
	rejectionPrefix = "claude запущен агентом на модели вне списка разрешённых"
	// hookCommandMarker — слово команды этого хука в настройках
	// ($HOME/bin/chooks user-prompt-submit): по нему оболочка и префикс,
	// которые claude поставил перед хуком, отличаются от самого claude
	hookCommandMarker = "user-prompt-submit"
	// shellPrefixEnv — префикс, которым Claude Code запускает команды хуков:
	// «<префикс> '<команда хука>'»
	shellPrefixEnv = "CLAUDE_CODE_SHELL_PREFIX"
	// maxSymlinkHops — предел звеньев ссылок пути настроек, как ELOOP у kernel
	maxSymlinkHops = 40
)

// promptFlags — флаги claude, значение которых — текст промпта: данные, а не
// команда, model_guard их не судит
var promptFlags = []string{"--system-prompt", "--append-system-prompt", "--system-prompt-file", "--append-system-prompt-file"}

// ProcessReader reads the process attributes used to identify and inspect Claude.
type ProcessReader interface {
	Parent(pid int) (int, error)
	TTYNr(pid int) (int64, error)
	Cgroup(pid int) (string, error)
	Exe(pid int) (string, error)
	Comm(pid int) (string, error)
	Environ(pid int) ([]string, error)
	Cmdline(pid int) ([]string, error)
	Cwd(pid int) (string, error)
}

// Judge судит команду запуска claude: model_guard
type Judge interface {
	ValidateTool(ctx context.Context, input *core.ToolInput) (*core.ValidationResult, error)
	CandidateModels(ctx context.Context, input *core.ToolInput) []string
}

// Guard checks the Claude process of the session that runs the hook.
type Guard struct {
	proc   ProcessReader
	judge  Judge
	logger core.Logger
}

func New(proc ProcessReader, judge Judge, logger core.Logger) *Guard {
	return &Guard{proc: proc, judge: judge, logger: logger}
}

// check — состояние одной проверки: прочитанные environ и cmdline
type check struct {
	*Guard
	env       map[int]readResult
	argv      map[int]readResult
	launchers []int
}

type readResult struct {
	values []string
	err    error
}

func (c *check) environ(pid int) ([]string, error) {
	if r, ok := c.env[pid]; ok {
		return r.values, r.err
	}
	values, err := c.proc.Environ(pid)
	c.env[pid] = readResult{values, err}
	return values, err
}

func (c *check) cmdline(pid int) ([]string, error) {
	if r, ok := c.argv[pid]; ok {
		return r.values, r.err
	}
	values, err := c.proc.Cmdline(pid)
	c.argv[pid] = readResult{values, err}
	return values, err
}

// Check returns a rejection message only when an agent launched Claude with a
// model that model_guard does not approve. Reads that fail for a user session
// pass; an agent's claude whose model cannot be read is rejected.
func (g *Guard) Check(ctx context.Context, hookPID int) (string, error) {
	c := &check{Guard: g, env: map[int]readResult{}, argv: map[int]readResult{}}
	claudePID, err := c.findSessionClaude(hookPID)
	if err != nil {
		g.logger.Warn("modellaunch: предки хука не прочитаны, проверка пропущена", "error", err)
		c.logDecision(0, "unknown", "ancestry_unreadable", "unknown", "skipped")
		return "", nil
	}
	if claudePID == 0 {
		c.logDecision(0, "unknown", "claude_not_found", "unknown", "skipped")
		return "", nil
	}
	agent, basis, complete := c.classify(claudePID)
	if !complete {
		c.logDecision(claudePID, "unknown", basis+"ancestor_read_incomplete", "not_checked", "skipped")
		return "", nil
	}
	if !agent {
		if message, err := c.judgeUserLaunchers(ctx, claudePID, basis); message != "" || err != nil {
			return message, err
		}
		c.logUserDecision(ctx, claudePID, basis)
		return "", nil
	}
	return c.judgeAgent(ctx, hookPID, claudePID, basis)
}

// findSessionClaude — claude этой сессии: ближайший предок хука, который не
// запускает его команду. Пропускаются оболочка хука (sh -c '<команда хука>')
// и процессы префикса CLAUDE_CODE_SHELL_PREFIX с их потомками: их argv —
// хвост слов префикса и команда хука последним словом
func (c *check) findSessionClaude(hookPID int) (int, error) {
	hookPrefix := c.prefixOf(hookPID)
	pid, err := c.proc.Parent(hookPID)
	if err != nil {
		return 0, fmt.Errorf("hook parent %d: %w", hookPID, err)
	}
	seen := map[int]bool{}
	for pid > 1 && !seen[pid] {
		seen[pid] = true
		if !c.isLauncher(pid, hookPrefix) {
			return pid, nil
		}
		c.launchers = append(c.launchers, pid)
		next, err := c.proc.Parent(pid)
		if err != nil {
			return 0, fmt.Errorf("launcher %d parent: %w", pid, err)
		}
		pid = next
	}
	return 0, nil
}

func (c *check) isLauncher(pid int, hookPrefix prefix) bool {
	argv, err := c.cmdline(pid)
	if err != nil {
		return false
	}
	exe, exeErr := c.proc.Exe(pid)
	comm, commErr := c.proc.Comm(pid)
	if isHookShell(exe, exeErr, comm, commErr, argv) {
		return true
	}
	if hookPrefix.launches(argv) {
		return true
	}
	return c.prefixOf(pid).launches(argv)
}

// prefix — слова CLAUDE_CODE_SHELL_PREFIX, раскрытые в окружении процесса,
// как их раскрыла оболочка
type prefix struct{ words []string }

// prefixOf — префикс из environ процесса. Environ не прочитан или префикс
// не разобран — префикса нет: процесс не узнаётся как процесс префикса и
// судится как claude сессии
func (c *check) prefixOf(pid int) prefix {
	env, err := c.environ(pid)
	if err != nil {
		c.logger.Debug("modellaunch: environ не прочитан, префикс не известен", "pid", pid, "error", err)
		return prefix{}
	}
	p, err := parsePrefix(env)
	if err != nil {
		c.logger.Warn("modellaunch: префикс команд хуков не разобран", "pid", pid, "error", err)
		return prefix{}
	}
	return p
}

func parsePrefix(env []string) (prefix, error) {
	values := envMap(env)
	value := values[shellPrefixEnv]
	if strings.TrimSpace(value) == "" {
		return prefix{}, nil
	}
	words, err := shell.Fields(value, func(name string) string { return values[name] })
	if err != nil {
		return prefix{}, fmt.Errorf("%s=%q: %w", shellPrefixEnv, value, err)
	}
	return prefix{words: words}, nil
}

// launches — argv принадлежит процессу префикса, который запускает этот хук:
// после 0–2 слов интерпретатора (#! скрипта, тогда дальше путь скрипта) идёт
// хвост слов префикса, за ним команда хука. Процесс префикса и каждый его потомок на пути к хуку
// (timeout → bash -c) несут хвост тех же слов и ту же команду
func (p prefix) launches(argv []string) bool {
	words := p.words
	if len(words) == 0 {
		return false
	}
	hookAt := slices.IndexFunc(argv, func(arg string) bool { return strings.Contains(arg, hookCommandMarker) })
	if hookAt < 1 {
		return false
	}
	head := argv[:hookAt]
	for skip := 0; skip <= 2 && skip < len(head); skip++ {
		rest := head[skip:]
		if len(rest) > len(words) {
			continue
		}
		// за словами интерпретатора kernel ставит путь скрипта: он с «/»
		if skip > 0 && !strings.Contains(rest[0], "/") {
			continue
		}
		tail := words[len(words)-len(rest):]
		if sameProgram(rest[0], tail[0]) && slices.Equal(rest[1:], tail[1:]) {
			return true
		}
	}
	return false
}

func sameProgram(a, b string) bool {
	return a == b || filepath.Base(a) == filepath.Base(b)
}

func isHookShell(exe string, exeErr error, comm string, commErr error, argv []string) bool {
	if len(argv) < 3 {
		return false
	}
	base := ""
	if exeErr == nil {
		base = exeName(exe)
	}
	if base == "" && commErr == nil {
		base = strings.TrimSpace(comm)
	}
	if base == "" {
		base = filepath.Base(argv[0])
	}
	if base != "sh" && base != "bash" && base != "dash" {
		return false
	}
	for i := 1; i+1 < len(argv); i++ {
		if argv[i] == "-c" && strings.Contains(argv[i+1], hookCommandMarker) {
			return true
		}
	}
	return false
}

// exeName — имя бинаря из /proc/<pid>/exe без пометки kernel о бинаре,
// заменённом на диске обновлением пакета
func exeName(exe string) string {
	return filepath.Base(strings.TrimSuffix(exe, " (deleted)"))
}

// classify — запущен ли claude агентом: признаки агента в его environ или
// claude среди предков. Environ claude не читается (бинарь без права чтения)
// — признаки агента берутся и из environ читаемых предков
func (c *check) classify(claudePID int) (agent bool, basis string, complete bool) {
	env, envErr := c.environ(claudePID)
	if envErr == nil {
		if agent, key := agentEnvironment(env); agent {
			return true, key, true
		}
	} else {
		c.logger.Warn("modellaunch: окружение claude не прочитано, признаки агента ищутся у предков", "pid", claudePID, "error", envErr)
		basis = "claude_environ_unreadable;"
	}
	parent, err := c.proc.Parent(claudePID)
	if err != nil {
		c.logger.Warn("modellaunch: родитель claude не прочитан", "pid", claudePID, "error", err)
		return false, basis, false
	}
	found, ancestorBasis, complete := c.scanAncestors(parent, envErr != nil)
	if found {
		return true, basis + ancestorBasis, true
	}
	// Интерактивность задаёт scope непосредственного запуска. cl -p меняет
	// процесс на Claude, поэтому для него проверяется scope родителя cl.
	interactivePID := parent
	comm, commErr := c.proc.Comm(parent)
	if commErr == nil && strings.TrimSpace(comm) == "cl" {
		if grandparent, err := c.proc.Parent(parent); err == nil {
			interactivePID = grandparent
		} else {
			c.logger.Warn("modellaunch: родитель cl не прочитан", "pid", parent, "error", err)
			return false, basis + "parent_scope_unreadable", true
		}
	} else if commErr != nil {
		c.logger.Warn("modellaunch: имя родителя claude не прочитано", "pid", parent, "error", commErr)
		return false, basis + "parent_scope_unreadable", true
	}
	claudeCgroup, err := c.proc.Cgroup(claudePID)
	if err != nil {
		c.logger.Warn("modellaunch: cgroup процесса claude не прочитан", "pid", claudePID, "error", err)
		return false, basis + "claude_cgroup_unreadable", true
	}
	parentCgroup, err := c.proc.Cgroup(interactivePID)
	if err != nil {
		c.logger.Warn("modellaunch: cgroup родителя claude не прочитан", "pid", interactivePID, "error", err)
		return false, basis + "parent_scope_unreadable", true
	}
	tty, err := c.proc.TTYNr(claudePID)
	if err != nil {
		c.logger.Warn("modellaunch: tty процесса claude не прочитан", "pid", claudePID, "error", err)
		return false, basis + "tty_unreadable", true
	}
	parentIsCL := commErr == nil && strings.TrimSpace(comm) == "cl"
	if inInteractiveScope(claudeCgroup) || (inInteractiveScope(parentCgroup) && (parentIsCL || (interactiveParent(comm) && tty > 1))) {
		return false, basis + "parent_in_interactive_scope", true
	}
	if commErr == nil && strings.TrimSpace(comm) == "cl" {
		if clTTY, err := c.proc.TTYNr(parent); err == nil && clTTY != 0 {
			if manager, err := c.proc.Parent(parent); err == nil {
				if manager > 1 {
					return false, basis + "cl_with_live_shell_ancestry", true
				}
				if managerComm, err := c.proc.Comm(manager); err == nil && strings.TrimSpace(managerComm) == "systemd" {
					return false, basis + "cl_reparented_to_user_manager_with_tty", true
				}
				if shell, err := c.proc.Comm(manager); err == nil && interactiveParent(shell) {
					return false, basis + "cl_with_interactive_shell_parent", true
				}
			}
		}
	}
	return true, basis + "parent_outside_interactive_scope", complete
}

func interactiveParent(comm string) bool {
	switch strings.TrimSpace(comm) {
	case "bash", "sh", "dash", "zsh", "fish", "cl":
		return true
	default:
		return false
	}
}

func cgroupPath(cgroup string) string {
	for line := range strings.SplitSeq(cgroup, "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) == 3 {
			return parts[2]
		}
	}
	return ""
}

func inAgentScope(cgroup string) bool {
	parts := strings.Split(cgroupPath(cgroup), "/")
	for i := 0; i+1 < len(parts); i++ {
		if parts[i] == "agents.slice" && strings.HasPrefix(parts[i+1], "agent-") && strings.HasSuffix(parts[i+1], ".scope") {
			return true
		}
	}
	return false
}

func inInteractiveScope(cgroup string) bool {
	for part := range strings.SplitSeq(cgroupPath(cgroup), "/") {
		if (strings.HasPrefix(part, "session-") && strings.HasSuffix(part, ".scope")) || (strings.HasPrefix(part, "app-") && (strings.HasSuffix(part, ".scope") || strings.HasSuffix(part, ".service"))) {
			return true
		}
	}
	return false
}

func (c *check) scanAncestors(pid int, markers bool) (bool, string, bool) {
	seen := map[int]bool{}
	for pid > 1 && !seen[pid] {
		seen[pid] = true
		exe, exeErr := c.proc.Exe(pid)
		comm, commErr := c.proc.Comm(pid)
		argv, argvErr := c.cmdline(pid)
		if isClaude(exe, exeErr, comm, commErr, argv, argvErr) {
			return true, "ancestor_claude", true
		}
		if markers {
			if env, err := c.environ(pid); err == nil {
				if agent, key := agentEnvironment(env); agent {
					return true, "ancestor_environ_" + key, true
				}
			}
		}
		parent, err := c.proc.Parent(pid)
		if err != nil {
			c.logger.Warn("modellaunch: родитель предка claude не прочитан", "pid", pid, "error", err)
			return false, "", false
		}
		pid = parent
	}
	return false, "", true
}

func isClaude(exe string, exeErr error, comm string, commErr error, argv []string, argvErr error) bool {
	if exeErr == nil && exeName(exe) == "claude" {
		return true
	}
	if commErr == nil && strings.TrimSpace(comm) == "claude" {
		return true
	}
	if argvErr != nil {
		return false
	}
	if len(argv) == 0 {
		return false
	}
	if filepath.Base(argv[0]) == "claude" {
		return true
	}
	if len(argv) < 2 {
		return false
	}
	// Claude Code's launcher is a Node script: argv[0] is node and argv[1]
	// names the installed claude entry point.
	return filepath.Base(argv[1]) == "claude" || filepath.Base(argv[1]) == "claude.js"
}

func agentEnvironment(env []string) (bool, string) {
	for _, item := range env {
		key, value, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		switch key {
		case "CLAUDECODE":
			if value == "1" {
				return true, key
			}
		case "CLAUDE_CODE_SESSION_ID", "CLAUDE_PID":
			return true, key
		}
	}
	return false, ""
}

// launch — команда запуска для model_guard: слова модели из argv процесса,
// переменные модели из его окружения и его cwd
type launch struct {
	pid  int
	args []string
	env  []string
	cwd  string
}

func (l launch) input() *core.ToolInput {
	return &core.ToolInput{ToolName: "Bash", Command: literalCommand(l.args, l.env), CWD: l.cwd}
}

// judgeUserLaunchers судит процессы между claude пользователя и хуком со
// словами модели в argv. Процесс префикса таких слов не несёт, а claude под
// видом префикса несёт: агент ставит префикс «<claude> --model haiku -p» и
// запускает «<claude> --model haiku -p user-prompt-submit», проход по предкам
// его пропускает и доходит до claude пользователя. Судятся только слова argv:
// env и cwd этих процессов — env и cwd claude пользователя, поэтому отказ —
// только когда без слов argv та же проверка проходит
func (c *check) judgeUserLaunchers(ctx context.Context, claudePID int, basis string) (string, error) {
	for _, pid := range c.launchers {
		// запускатель узнан по argv: его cmdline прочитан и лежит в кэше
		args := modelArgs(c.argv[pid].values)
		if len(args) == 0 {
			continue
		}
		l := c.launcherLaunch(pid, "")
		message, err := c.argvRejection(ctx, l)
		if err != nil {
			c.logDecision(claudePID, "user", basis, "unknown", "error")
			return "", err
		}
		if message != "" {
			c.logDecision(claudePID, "user", basis+";claude_like_launcher="+strconv.Itoa(pid), strings.Join(args, " "), "rejected_launcher")
			return message, nil
		}
	}
	return "", nil
}

// argvRejection — отказ из-за слов модели в argv запускателя: с ними проверка
// не проходит, без них проходит. Не проходит и без них — модель вне списка в
// настройках пользователя, а не в argv: не отказ. Env запускателя не судится,
// HOME из него только раскрывает «~/» в пути --settings
func (c *check) argvRejection(ctx context.Context, l launch) (string, error) {
	if badPath, err := fdSettingsPath(l); badPath != "" {
		c.logger.Warn("modellaunch: модель из --settings запускателя не проверить", "pid", l.pid, "path", badPath, "error", err)
		return rejectionPrefix + ": модель из --settings не удалось проверить: " + badPath, nil
	}
	judged := launch{pid: l.pid, args: l.args, cwd: l.cwd}
	with, err := c.judge.ValidateTool(ctx, judged.input())
	if err != nil {
		return "", err
	}
	if with.IsValid {
		return "", nil
	}
	judged.args = nil
	without, err := c.judge.ValidateTool(ctx, judged.input())
	if err != nil {
		return "", err
	}
	if !without.IsValid {
		c.logger.Info("modellaunch: модель вне списка — в настройках пользователя, не в argv запускателя", "pid", l.pid)
		return "", nil
	}
	if len(with.Violations) == 0 {
		return rejectionPrefix, nil
	}
	return rejectionPrefix + ": " + with.Violations[0].Message, nil
}

// logUserDecision — строка лога о сессии пользователя: её модель не судится
func (c *check) logUserDecision(ctx context.Context, claudePID int, basis string) {
	model, err := c.userModel(ctx, claudePID)
	if err != nil {
		c.logDecision(claudePID, "user", basis+";model_unreadable="+err.Error(), "unknown", "allowed")
		return
	}
	c.logDecision(claudePID, "user", basis, model, "allowed")
}

// userModel — модель сессии пользователя для строки лога; не судится
func (c *check) userModel(ctx context.Context, pid int) (string, error) {
	argv, err := c.cmdline(pid)
	if err != nil {
		return "", fmt.Errorf("cmdline %d: %w", pid, err)
	}
	cwd, err := c.proc.Cwd(pid)
	if err != nil {
		return "", fmt.Errorf("cwd %d: %w", pid, err)
	}
	return c.models(ctx, launch{pid: pid, args: modelArgs(argv), env: c.modelEnv(pid), cwd: cwd}), nil
}

// modelEnv — переменные модели из environ процесса; environ не прочитан —
// их нет: модель окружения судится по environ хука
func (c *check) modelEnv(pid int) []string {
	env, err := c.environ(pid)
	if err != nil {
		c.logger.Debug("modellaunch: environ не прочитан, переменные модели берутся у хука", "pid", pid, "error", err)
		return nil
	}
	return envWords(env)
}

func (c *check) models(ctx context.Context, l launch) string {
	model := strings.Join(c.judge.CandidateModels(ctx, l.input()), ",")
	if model == "" {
		return "unspecified"
	}
	return model
}

// judgeAgent судит модель claude, запущенного агентом. Кроме claude судятся
// процессы между ним и хуком и сам хук: их окружение и cwd унаследованы от
// claude сессии, а процесс, принятый за оболочку или префикс, мог быть claude
func (c *check) judgeAgent(ctx context.Context, hookPID, claudePID int, basis string) (string, error) {
	argv, err := c.cmdline(claudePID)
	if err != nil {
		c.logger.Warn("modellaunch: cmdline claude не прочитан", "pid", claudePID, "error", err)
		c.logDecision(claudePID, "agent", basis, "unknown", "rejected_cmdline_unreadable")
		return rejectionPrefix + ": командная строка claude не прочитана, модель не проверить", nil
	}
	cwd, err := c.sessionCWD(claudePID, hookPID)
	if err != nil {
		c.logger.Warn("modellaunch: каталог claude не прочитан", "pid", claudePID, "error", err)
		c.logDecision(claudePID, "agent", basis, "unknown", "rejected_cwd_unreadable")
		return rejectionPrefix + ": каталог claude не прочитан, модель не проверить", nil
	}
	primary := launch{pid: claudePID, args: modelArgs(argv), env: c.modelEnv(claudePID), cwd: cwd}
	launches := []launch{primary}
	for _, pid := range c.launchers {
		launches = c.addLaunch(launches, c.launcherLaunch(pid, cwd))
	}
	launches = c.addLaunch(launches, c.launcherLaunch(hookPID, cwd))
	model := c.models(ctx, primary)
	for _, l := range launches {
		if badPath, err := fdSettingsPath(l); badPath != "" {
			c.logger.Warn("modellaunch: модель из --settings не проверить: путь читается в процессе хука, а не claude", "pid", l.pid, "path", badPath, "error", err)
			c.logDecision(claudePID, "agent", basis, "unreadable_settings", "rejected_settings_unreadable")
			return rejectionPrefix + ": модель из --settings не удалось проверить: " + badPath, nil
		}
	}
	for _, l := range launches {
		result, err := c.judge.ValidateTool(ctx, l.input())
		if err != nil {
			c.logDecision(claudePID, "agent", basis, model, "error")
			return "", err
		}
		if result.IsValid {
			continue
		}
		c.logDecision(claudePID, "agent", basis, model, "rejected")
		if len(result.Violations) == 0 {
			return rejectionPrefix, nil
		}
		return rejectionPrefix + ": " + result.Violations[0].Message, nil
	}
	c.logDecision(claudePID, "agent", basis, model, "allowed")
	return "", nil
}

// sessionCWD — каталог claude сессии. Его cwd закрыт (бинарь без права
// чтения) — каталог хука: claude запускает хук в своём каталоге
func (c *check) sessionCWD(claudePID, hookPID int) (string, error) {
	cwd, err := c.proc.Cwd(claudePID)
	if err == nil {
		return cwd, nil
	}
	hookCWD, hookErr := c.proc.Cwd(hookPID)
	if hookErr != nil {
		return "", fmt.Errorf("cwd claude %d: %v; cwd хука %d: %w", claudePID, err, hookPID, hookErr)
	}
	c.logger.Debug("modellaunch: cwd claude не прочитан, судится cwd хука", "pid", claudePID, "error", err)
	return hookCWD, nil
}

// launcherLaunch — запуск процесса между claude и хуком или самого хука:
// слова модели из его argv, его переменные модели и cwd. Cwd не прочитан —
// судится в каталоге claude сессии
func (c *check) launcherLaunch(pid int, sessionCWD string) launch {
	l := launch{pid: pid, env: c.modelEnv(pid), cwd: sessionCWD}
	if argv, err := c.cmdline(pid); err == nil {
		l.args = modelArgs(argv)
	} else {
		c.logger.Debug("modellaunch: cmdline не прочитан", "pid", pid, "error", err)
	}
	if cwd, err := c.proc.Cwd(pid); err == nil {
		l.cwd = cwd
	} else {
		c.logger.Debug("modellaunch: cwd не прочитан, судится каталог claude", "pid", pid, "error", err)
	}
	return l
}

// addLaunch добавляет команду, если она судит что-то сверх уже собранных:
// без слов модели, с тем же окружением модели и cwd она ничего не добавит
func (c *check) addLaunch(launches []launch, l launch) []launch {
	for _, have := range launches {
		if len(l.args) == 0 && slices.Equal(l.env, have.env) && l.cwd == have.cwd {
			return launches
		}
		if slices.Equal(l.args, have.args) && slices.Equal(l.env, have.env) && l.cwd == have.cwd {
			return launches
		}
	}
	return append(launches, l)
}

func (c *check) logDecision(pid int, actor, basis, model, outcome string) {
	c.logger.Info("modellaunch: runtime model decision", "claude_pid", pid, "actor", actor, "basis", basis, "model", model, "result", outcome, "launchers", len(c.launchers))
}

// modelArgs — слова командной строки claude, которые читает model_guard:
// флаги модели, настроек, агентов и каталогов агентов со значениями. argv[0]
// не судится (exec -a), интерпретатор, его опции и скрипт — позиционные слова
// и отбрасываются, как и промпт: позиционные слова, слова после «--» и
// значения флагов системного промпта — данные, а не команда
func modelArgs(argv []string) []string {
	if len(argv) == 0 {
		return nil
	}
	args := argv[1:] // argv[0] may be changed by exec -a; command name is always claude
	modelFlags := tools.ClaudeModelArgFlags()
	var kept []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		name, _, inline := strings.Cut(arg, "=")
		if slices.Contains(promptFlags, name) {
			if !inline {
				i++
			}
			continue
		}
		if !slices.Contains(modelFlags, name) {
			continue
		}
		kept = append(kept, arg)
		if !inline && i+1 < len(args) {
			kept = append(kept, args[i+1])
			i++
		}
	}
	return kept
}

func envMap(env []string) map[string]string {
	values := make(map[string]string, len(env))
	for _, item := range env {
		if key, value, ok := strings.Cut(item, "="); ok {
			values[key] = value
		}
	}
	return values
}

// envWords — переменные окружения, задающие модель claude, словами K=V
func envWords(env []string) []string {
	values := envMap(env)
	var words []string
	for _, key := range append(tools.ModelEnvironmentNames(), "CLAUDE_CONFIG_DIR", "HOME") {
		if value, ok := values[key]; ok {
			words = append(words, key+"="+value)
		}
	}
	return words
}

// literalCommand — команда «K='v' 'claude' 'слово'…»: переменные —
// присваиваниями перед командой, а не через env. За «env» model_guard
// сдвигает разбор кавычек слов claude, и путь --agents выходит нелитеральным:
// отказ агенту с файлом --agents на одобренной модели. Имена переменных — из
// envWords, они не требуют кавычек
func literalCommand(args, env []string) string {
	words := make([]string, 0, len(env)+len(args)+1)
	for _, item := range env {
		key, value, _ := strings.Cut(item, "=")
		words = append(words, key+"="+shellQuote(value))
	}
	words = append(words, shellQuote("claude"))
	for _, arg := range args {
		words = append(words, shellQuote(arg))
	}
	return strings.Join(words, " ")
}

// fdSettingsPath — значение --settings или --agents, которое указывает в
// /proc или /dev (/dev/fd/63, /dev/stdin, /proc/self/fd/N): такой путь хук
// читает в своём процессе, а не в процессе claude, и модель не проверить.
// Прочие пути (FIFO, нет файла, не читается) отклоняет сам model_guard
func fdSettingsPath(l launch) (string, error) {
	home := envMap(l.env)["HOME"]
	for i := 0; i < len(l.args); i++ {
		name, value, inline := strings.Cut(l.args[i], "=")
		if name != "--settings" && name != "--agents" {
			continue
		}
		if !inline {
			if i+1 >= len(l.args) {
				continue
			}
			i++
			value = l.args[i]
		}
		if strings.HasPrefix(strings.TrimSpace(value), "{") {
			continue // inline JSON
		}
		path := value
		if rest, ok := strings.CutPrefix(path, "~/"); ok && home != "" {
			path = filepath.Join(home, rest)
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(l.cwd, path)
		}
		links, err := linksIntoProcess(path)
		if err != nil {
			return value, err
		}
		if links {
			return value, nil
		}
	}
	return "", nil
}

// linksIntoProcess — путь или звено его ссылок (сам файл или каталог)
// ведёт в /proc или /dev
func linksIntoProcess(path string) (bool, error) {
	for range maxSymlinkHops {
		if perProcessPath(path) {
			return true, nil
		}
		if dir, err := filepath.EvalSymlinks(filepath.Dir(path)); err == nil && perProcessPath(dir) {
			return true, nil
		}
		target, err := os.Readlink(path)
		if errors.Is(err, syscall.EINVAL) || errors.Is(err, fs.ErrNotExist) {
			return false, nil // не ссылка или нет файла: путь судит model_guard
		}
		if err != nil {
			return false, fmt.Errorf("ссылка %s не прочитана: %w", path, err)
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		path = target
	}
	return false, fmt.Errorf("больше %d звеньев ссылок: %s", maxSymlinkHops, path)
}

func perProcessPath(path string) bool {
	clean := filepath.Clean(path)
	return strings.HasPrefix(clean, "/proc/") || strings.HasPrefix(clean, "/dev/")
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

// ProcReader is the Linux /proc implementation of ProcessReader.
type ProcReader struct{ Root string }

func (p ProcReader) root() string {
	if p.Root == "" {
		return "/proc"
	}
	return p.Root
}

func (p ProcReader) Parent(pid int) (int, error) {
	fields, err := p.statFields(pid)
	if err != nil {
		return 0, err
	}
	if len(fields) < 2 {
		return 0, fmt.Errorf("malformed stat")
	}
	return strconv.Atoi(fields[1])
}

func (p ProcReader) TTYNr(pid int) (int64, error) {
	fields, err := p.statFields(pid)
	if err != nil {
		return 0, err
	}
	if len(fields) < 5 {
		return 0, fmt.Errorf("malformed stat")
	}
	return strconv.ParseInt(fields[4], 10, 64)
}

func (p ProcReader) statFields(pid int) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(p.root(), strconv.Itoa(pid), "stat"))
	if err != nil {
		return nil, err
	}
	close := strings.LastIndex(string(data), ")")
	if close < 0 {
		return nil, fmt.Errorf("malformed stat")
	}
	return strings.Fields(string(data[close+1:])), nil
}

func (p ProcReader) Cgroup(pid int) (string, error) {
	data, err := os.ReadFile(filepath.Join(p.root(), strconv.Itoa(pid), "cgroup"))
	return string(data), err
}

func (p ProcReader) Exe(pid int) (string, error) {
	return os.Readlink(filepath.Join(p.root(), strconv.Itoa(pid), "exe"))
}

func (p ProcReader) Comm(pid int) (string, error) {
	data, err := os.ReadFile(filepath.Join(p.root(), strconv.Itoa(pid), "comm"))
	return strings.TrimSpace(string(data)), err
}

func (p ProcReader) Environ(pid int) ([]string, error) {
	return splitNULFile(filepath.Join(p.root(), strconv.Itoa(pid), "environ"))
}

func (p ProcReader) Cmdline(pid int) ([]string, error) {
	return splitNULFile(filepath.Join(p.root(), strconv.Itoa(pid), "cmdline"))
}

func (p ProcReader) Cwd(pid int) (string, error) {
	return os.Readlink(filepath.Join(p.root(), strconv.Itoa(pid), "cwd"))
}

func splitNULFile(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(string(data), "\x00")
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts, nil
}
