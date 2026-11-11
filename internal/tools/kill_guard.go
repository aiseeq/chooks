package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/aiseeq/chooks/internal/core"
)

// protectedComms процессы, без которых падает сессия пользователя целиком:
// init и менеджеры служб, шина, графическая оболочка, звук, вход по ssh
var protectedComms = map[string]bool{
	"systemd": true, "init": true, "systemd-logind": true,
	"dbus-daemon": true, "dbus-broker": true, "sshd": true, "sshd-session": true,
	"Xwayland": true, "Xorg": true, "kwin_wayland": true, "kwin_x11": true,
	"plasmashell": true, "gnome-shell": true, "sway": true, "Hyprland": true,
	"sddm": true, "gdm": true, "pipewire": true, "wireplumber": true,
}

// KillGuardTool блокирует kill, pkill и killall, цель которых нельзя
// проверить до запуска или которые заденут саму сессию: PID 1, systemd,
// предков хука (claude, терминал), оболочку, исполняющую команду
type KillGuardTool struct {
	*BaseTool
	procRoot string
	selfPID  int
}

// NewKillGuardTool создает защиту от убийства сессии
func NewKillGuardTool(config core.ToolConfig, logger core.Logger) (*KillGuardTool, error) {
	return &KillGuardTool{
		BaseTool: NewBaseTool("kill_guard", config.Enabled, core.ShellTools, logger),
		procRoot: "/proc",
		selfPID:  os.Getpid(),
	}, nil
}

const killGuardHint = "Найди PID отдельной командой (ps -o pid,ppid,comm,args -p … или pgrep -a '[x]yz'), посмотри на вывод и убей литералом: kill 12345"

// ValidateTool проверяет команды, посылающие сигналы процессам
func (t *KillGuardTool) ValidateTool(_ context.Context, input *core.ToolInput) (*core.ValidationResult, error) {
	if !t.IsEnabled() || !core.IsShellTool(input.ToolName) || !strings.Contains(input.Command, "kill") {
		return &core.ValidationResult{IsValid: true}, nil
	}

	scan := killScan{tool: t, command: input.Command}
	if err := scan.walk(input.Command, 0); err != nil {
		return nil, err
	}
	if len(scan.reasons) == 0 {
		return &core.ValidationResult{IsValid: true}, nil
	}

	violations := make([]core.Violation, 0, len(scan.reasons))
	for _, r := range scan.reasons {
		violations = append(violations, core.NewViolation("kill_guard",
			"Сигнал может убить не тот процесс или саму сессию:", r, core.LevelCritical, 0, 0))
	}
	return core.NewValidationResult(false, violations, append(scan.reasons, killGuardHint)), nil
}

// maxScriptNesting предел вложенности sh -c и eval: глубже разбор не идёт,
// команда отклоняется
const maxScriptNesting = 4

// killScan обход команды и вложенных в неё скриптов sh -c и eval
type killScan struct {
	tool *KillGuardTool
	// command — команда целиком: её текст стоит в cmdline оболочки, и pkill -f
	// по нему убьёт эту оболочку
	command string
	procs   []procInfo
	reasons []string
}

// walk проверяет простые команды скрипта; скрипт sh -c и eval — тем же
// разбором на уровень глубже
func (s *killScan) walk(script string, depth int) error {
	for _, cmd := range splitSimpleCommands(withoutHereDocBodies(script, true)) {
		name, args, viaXargs := unwrapCommand(cmd)
		if inner, ok := innerScript(name, args); ok {
			if depth >= maxScriptNesting {
				s.reasons = append(s.reasons, fmt.Sprintf("вложенность sh -c и eval глубже %d: проверить сигналы внутри нельзя", maxScriptNesting))
				continue
			}
			if err := s.walk(inner, depth+1); err != nil {
				return err
			}
			continue
		}
		if err := s.check(name, args, viaXargs); err != nil {
			return err
		}
	}
	return nil
}

// check проверяет одну простую команду, если она шлёт сигнал
func (s *killScan) check(name string, args []word, viaXargs bool) error {
	switch name {
	case "kill", "pkill", "killall", "killall5":
	default:
		return nil
	}
	if s.procs == nil {
		procs, err := s.tool.listProcs()
		if err != nil {
			return err
		}
		s.procs = procs
	}
	var reason string
	switch {
	case viaXargs:
		reason = name + " через xargs: цели приходят из вывода другой команды, проверить их до запуска нельзя"
	case name == "killall5":
		reason = "killall5 шлёт сигнал всем процессам системы вне своей сессии: погасит все сессии Claude"
	case name == "kill":
		reason = s.tool.checkKill(args, s.procs)
	default:
		reason = s.tool.checkPattern(name, args, s.command, s.procs)
	}
	if reason != "" {
		s.reasons = append(s.reasons, reason)
	}
	return nil
}

// checkKill проверяет цели kill: только литеральные PID и job spec, ни один
// не задевает сессию. Сигнал 0 лишь проверяет, жив ли процесс, — цели любые
func (t *KillGuardTool) checkKill(args []word, procs []procInfo) string {
	targets := args
	if len(targets) > 0 && strings.HasPrefix(targets[0].text, "-") && targets[0].literal {
		switch targets[0].text {
		case "-l", "-L", "--list", "--table", "-0", "--signal=0":
			return ""
		case "-s", "-n", "--signal":
			if len(targets) > 1 && targets[1].literal && targets[1].text == "0" {
				return ""
			}
			targets = targets[min(2, len(targets)):]
		default:
			targets = targets[1:]
		}
	}
	if len(targets) > 0 && targets[0].text == "--" {
		targets = targets[1:]
	}
	ancestors := t.ancestors(procs)
	for _, w := range targets {
		if !w.literal {
			return fmt.Sprintf("kill %s: PID вычисляется при запуске, проверить его заранее нельзя", w.text)
		}
		if strings.HasPrefix(w.text, "%") {
			continue
		}
		pid, err := strconv.Atoi(w.text)
		if err != nil {
			return fmt.Sprintf("kill %s: цель не PID (%v)", w.text, err)
		}
		if reason := t.dangerous(pid, procs, ancestors); reason != "" {
			return fmt.Sprintf("kill %d: %s", pid, reason)
		}
	}
	return ""
}

// patternArgs разобранные аргументы pkill или killall
type patternArgs struct {
	full, exact, isRegex bool
	// info — справка, версия или список сигналов: сигнала не будет
	info     bool
	patterns []string
}

// parsePatternArgs разбирает флаги и шаблоны; непустая строка — причина отказа
func parsePatternArgs(name string, args []word) (patternArgs, string) {
	pa := patternArgs{isRegex: name == "pkill"}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !a.literal {
			return pa, fmt.Sprintf("%s %s: шаблон вычисляется при запуске, проверить его заранее нельзя", name, a.text)
		}
		switch {
		case a.text == "-f" || a.text == "--full":
			pa.full = true
		case a.text == "-x" || a.text == "--exact" || a.text == "-e":
			pa.exact = true
		case a.text == "-r" || a.text == "--regexp":
			pa.isRegex = true
		case infoOption(name, a.text):
			pa.info = true
		case optionTakesValue(name, a.text):
			i++
		case strings.HasPrefix(a.text, "-"):
			// сигнал или флаг без значения
		default:
			pa.patterns = append(pa.patterns, a.text)
		}
	}
	if name == "pkill" && len(pa.patterns) > 1 {
		pa.patterns = pa.patterns[len(pa.patterns)-1:]
	}
	if len(pa.patterns) == 0 && !pa.info {
		return pa, name + " без шаблона выбирает процессы только по фильтру (-u, -P, -t, -g, -s): pkill -u $USER гасит все сессии разом"
	}
	return pa, ""
}

// infoOption флаг справки, версии или списка сигналов: утилита ничего не шлёт
func infoOption(name, opt string) bool {
	switch opt {
	case "--help", "-V", "--version":
		return true
	case "-l", "--list":
		return name == "killall"
	}
	return false
}

// regexp шаблон в том виде, как его сопоставляет утилита: killall без -r
// сравнивает имя целиком, pkill -x — тоже
func (pa patternArgs) regexp(p string) (*regexp.Regexp, error) {
	expr, exact := p, pa.exact
	if !pa.isRegex {
		expr, exact = regexp.QuoteMeta(p), true
	}
	if exact {
		expr = "^(?:" + expr + ")$"
	}
	return regexp.Compile(expr)
}

// checkPattern прикидывает, что заденет pkill или killall сейчас: шаблон
// сверяется с теми же полями процессов, что у самой утилиты
func (t *KillGuardTool) checkPattern(name string, args []word, command string, procs []procInfo) string {
	pa, reason := parsePatternArgs(name, args)
	if reason != "" {
		return reason
	}
	ancestors := t.ancestors(procs)
	for _, p := range pa.patterns {
		re, err := pa.regexp(p)
		if err != nil {
			// утилита сама откажется от такого шаблона
			t.Logger().Debug("kill pattern does not compile", "pattern", p, "error", err)
			continue
		}
		if pa.full && re.MatchString(command) {
			return fmt.Sprintf("%s -f %q совпадает с текстом самой команды и убьёт оболочку, которая её исполняет; шаблон вида '[x]yz' себя не ловит", name, p)
		}
		for _, pr := range procs {
			subject := pr.comm
			if pa.full {
				subject = pr.cmdline
			}
			if !re.MatchString(subject) {
				continue
			}
			if reason := t.dangerous(pr.pid, procs, ancestors); reason != "" {
				return fmt.Sprintf("%s %q задевает PID %d: %s", name, p, pr.pid, reason)
			}
		}
	}
	return ""
}

// dangerous возвращает причину, по которой процесс трогать нельзя
func (t *KillGuardTool) dangerous(pid int, procs []procInfo, ancestors map[int]bool) string {
	if pid <= 1 {
		return "PID ≤ 1 — это init или все процессы пользователя (группа процессов)"
	}
	if pid == t.selfPID || ancestors[pid] {
		return "предок этой сессии Claude (claude, оболочка, терминал или менеджер сессии)"
	}
	for _, p := range procs {
		if p.pid == pid && protectedComms[p.comm] {
			return fmt.Sprintf("%s — на нём держится сессия пользователя", p.comm)
		}
	}
	return ""
}

// procInfo процесс по /proc
type procInfo struct {
	pid, ppid     int
	comm, cmdline string
}

// listProcs читает все процессы из procRoot. Процесс, завершившийся между
// ReadDir и чтением его файлов, пропускается
func (t *KillGuardTool) listProcs() ([]procInfo, error) {
	entries, err := os.ReadDir(t.procRoot)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", t.procRoot, err)
	}
	procs := make([]procInfo, 0, len(entries))
	for _, e := range entries {
		if name := e.Name(); name[0] < '0' || name[0] > '9' {
			continue // не процесс: self, sys, meminfo
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			t.Logger().Debug("proc entry skipped", "name", e.Name(), "error", err)
			continue
		}
		p, err := readProc(filepath.Join(t.procRoot, e.Name()), pid)
		if err != nil {
			t.Logger().Debug("process skipped", "pid", pid, "error", err)
			continue
		}
		procs = append(procs, p)
	}
	return procs, nil
}

// readProc читает stat и cmdline одного процесса
func readProc(dir string, pid int) (procInfo, error) {
	stat, err := os.ReadFile(filepath.Join(dir, "stat"))
	if err != nil {
		return procInfo{}, fmt.Errorf("read stat: %w", err)
	}
	// comm в stat может содержать пробелы и скобки: поля после последней ')'
	s := string(stat)
	start, end := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
	if start < 0 || end < start {
		return procInfo{}, fmt.Errorf("stat без comm: %q", s)
	}
	fields := strings.Fields(s[end+1:])
	if len(fields) < 2 {
		return procInfo{}, fmt.Errorf("stat без ppid: %q", s)
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return procInfo{}, fmt.Errorf("ppid: %w", err)
	}
	cmdline, err := os.ReadFile(filepath.Join(dir, "cmdline"))
	if err != nil {
		return procInfo{}, fmt.Errorf("read cmdline: %w", err)
	}
	return procInfo{
		pid:     pid,
		ppid:    ppid,
		comm:    s[start+1 : end],
		cmdline: strings.TrimSpace(strings.ReplaceAll(string(cmdline), "\x00", " ")),
	}, nil
}

// ancestors цепочка родителей хука до init
func (t *KillGuardTool) ancestors(procs []procInfo) map[int]bool {
	parent := make(map[int]int, len(procs))
	for _, p := range procs {
		parent[p.pid] = p.ppid
	}
	out := map[int]bool{}
	for pid := parent[t.selfPID]; pid > 1 && !out[pid]; pid = parent[pid] {
		out[pid] = true
	}
	return out
}

// optionTakesValue флаги pkill и killall, за которыми идёт отдельное значение
func optionTakesValue(name, opt string) bool {
	if name == "pkill" {
		switch opt {
		case "-u", "-U", "-g", "-G", "-P", "-s", "-t", "--signal", "--euid", "--uid",
			"--pgroup", "--group", "--parent", "--session", "--terminal", "--ns", "--nslist":
			return true
		}
		return false
	}
	switch opt {
	case "-u", "--user", "-s", "--signal", "-o", "--older-than", "-y", "--younger-than", "-n", "--ns":
		return true
	}
	return false
}
