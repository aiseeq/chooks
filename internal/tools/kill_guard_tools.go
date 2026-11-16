package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/aiseeq/akit/shellcmd"
)

// sessionToolWordRe отдельное слово средства, которое гасит процессы или
// сессию, а слова kill в команде может не быть (loginctl terminate-user,
// systemctl --user stop, fuser -k). У skill, tmux kill-* и os.kill в
// python -c подстрока kill есть всегда
var sessionToolWordRe = regexp.MustCompile(`(^|[^[:alnum:]_.-])(loginctl|systemctl|fuser)([^[:alnum:]_.-]|$)`)

// sessionTool проверяет вызов средств, гасящих процессы или сессию, кроме
// kill, pkill и killall: loginctl, systemctl, fuser -k, skill, os.kill в
// скрипте python, tmux kill-*. Широта цели — как у kill: названная узко
// (юнит, порт, PID, сессия tmux) — можно, всё, группа, сессия, юнит предка
// — отказ. via — обёртка, подающая аргументы из ввода (xargs). Вызов в
// скрипте ssh исполнится на другой машине: сессию здесь он не тронет
// (ssh host 'sudo systemctl reboot')
func (s *killScan) sessionTool(c shellcmd.Call, args []word, via string) error {
	if slices.Contains(c.Via, "ssh") {
		return nil
	}
	var reason string
	group := false
	switch {
	case c.Name == "loginctl":
		reason = loginctlReason(args)
	case c.Name == "systemctl":
		if err := s.loadProcs(); err != nil {
			return err
		}
		reason = s.tool.systemctlReason(args, via, s.procs)
	case c.Name == "fuser":
		reason = fuserReason(args, via)
	case c.Name == "skill":
		reason = skillReason(args)
	case c.Name == "tmux":
		reason = tmuxReason(args)
	case pythonNameRe.MatchString(c.Name):
		if err := s.loadProcs(); err != nil {
			return err
		}
		reason, group = s.tool.pythonReason(c, args, s.procs)
	}
	if reason != "" {
		s.addReason(reason)
	}
	if group && !slices.Contains(s.groupReasons, reason) {
		s.groupReasons = append(s.groupReasons, reason)
	}
	return nil
}

// optionValue значение опции со значением: из слова (--signal=9, -s9) или
// следующим словом; i сдвигается на слово значения
func optionValue(args []word, i *int, rest string) word {
	if rest != "" {
		return word{text: rest, literal: args[*i].literal}
	}
	*i++
	if *i < len(args) {
		return args[*i]
	}
	return word{literal: true}
}

// optionOperands слова вызова без опций: values — опции со значением отдельным
// словом (короткие — буквой, длинные — с --), их значения не операнды
func optionOperands(args []word, values []string) []word {
	var out []word
	for i := 0; i < len(args); i++ {
		a := args[i].text
		switch {
		case a == "--":
			return append(out, args[i+1:]...)
		case strings.HasPrefix(a, "--"):
			if !strings.Contains(a, "=") && slices.Contains(values, a[2:]) {
				i++
			}
		case len(a) > 1 && a[0] == '-':
			if slices.Contains(values, a[len(a)-1:]) {
				i++
			}
		default:
			out = append(out, args[i])
		}
	}
	return out
}

// loginctlValues опции loginctl со значением
var loginctlValues = []string{"H", "host", "M", "machine", "p", "property", "s", "signal", "kill-whom", "n", "lines", "o", "output"}

// loginctlReason loginctl terminate-* и kill-* завершают процессы сеанса или
// всех сеансов пользователя: свой сеанс среди них не отличить по номеру
// (аудит-сеанс процесса и сеанс logind бывают разными)
func loginctlReason(args []word) string {
	ops := optionOperands(args, loginctlValues)
	if len(ops) == 0 {
		return ""
	}
	switch verb := ops[0].text; verb {
	case "terminate-user", "kill-user", "terminate-session", "kill-session", "terminate-seat":
		return "loginctl " + verb + " завершает процессы сеанса или всех сеансов пользователя: сессии Claude, терминал и графическую оболочку"
	}
	return ""
}

// systemctlValues опции systemctl со значением отдельным словом
var systemctlValues = []string{
	"t", "type", "s", "signal", "p", "property", "P", "H", "host", "M", "machine", "n", "lines", "o", "output",
	"state", "job-mode", "root", "image", "kill-whom", "kill-value", "what", "timestamp", "when", "message",
	"boot-loader-entry", "reboot-argument", "drop-in", "check-inhibitors", "preset-mode",
}

// systemctlEndVerbs команды systemctl, которые гасят все процессы
// менеджера или системы: переключение цели, выход менеджера, выключение
var systemctlEndVerbs = []string{"isolate", "exit", "poweroff", "reboot", "halt", "kexec", "soft-reboot", "emergency", "rescue", "default", "switch-root"}

// systemctlStopVerbs команды systemctl, которые останавливают, перезапускают
// или замораживают процессы юнитов (disable и mask — с --now)
var systemctlStopVerbs = []string{"stop", "kill", "restart", "try-restart", "reload-or-restart", "try-reload-or-restart", "condrestart", "force-reload", "freeze"}

// unitSuffixes типы юнитов: имя без них systemctl дополняет .service
var unitSuffixes = []string{".service", ".socket", ".target", ".scope", ".slice", ".timer", ".mount", ".automount", ".path", ".device", ".swap"}

// systemctlReason отказ на systemctl, который гасит юнит, где идёт предок
// этой сессии или процесс, на котором держится сессия пользователя (по
// cgroup процесса: user.slice/…/user@1000.service/app.slice/app-…scope),
// а также на юнит из переменной, шаблон юнитов, цель (.target: что
// остановится с ней, не видно) и команды, гасящие весь менеджер. Юниты,
// пришедшие из ввода (xargs), — тоже отказ
func (t *KillGuardTool) systemctlReason(args []word, via string, procs []procInfo) string {
	user, now, zero := systemctlFlags(args)
	ops := optionOperands(args, systemctlValues)
	if len(ops) == 0 {
		return ""
	}
	verb := ops[0].text
	scope := "systemctl"
	if user {
		scope += " --user"
	}
	switch {
	case slices.Contains(systemctlEndVerbs, verb):
		return scope + " " + verb + " гасит все юниты менеджера: сессии Claude, терминал и графическую оболочку"
	case verb == "kill" && zero:
		return ""
	case slices.Contains(systemctlStopVerbs, verb) || now && (verb == "disable" || verb == "mask"):
	default:
		return ""
	}
	if via != "" {
		return scope + " " + verb + " через " + via + ": юниты приходят из ввода, проверить их до запуска нельзя"
	}
	hosts := t.sessionUnits(procs, user)
	for _, u := range ops[1:] {
		if reason := unitReason(u, hosts); reason != "" {
			return fmt.Sprintf("%s %s %s: %s", scope, verb, u.text, reason)
		}
	}
	return ""
}

// systemctlFlags опции systemctl, от которых зависит отказ: --user (юниты
// менеджера пользователя), --now (disable и mask останавливают), сигнал 0
// у kill (последний -s или --signal)
func systemctlFlags(args []word) (user, now, zero bool) {
	for i := 0; i < len(args); i++ {
		switch a := args[i].text; {
		case a == "--user":
			user = true
		case a == "--now":
			now = true
		case a == "-s" || a == "--signal":
			zero = optionValue(args, &i, "").text == "0"
		case strings.HasPrefix(a, "--signal=") || strings.HasPrefix(a, "-s"):
			zero = a == "--signal=0" || a == "-s0"
		}
	}
	return user, now, zero
}

// unitReason почему юнит u останавливать нельзя: вычисляется, шаблон, цель
// или корневой срез, юнит сессии из hosts; "" — можно. Имя без типа
// systemctl дополняет .service
func unitReason(u word, hosts map[string]string) string {
	name := u.text
	if !slices.ContainsFunc(unitSuffixes, func(s string) bool { return strings.HasSuffix(name, s) }) {
		name += ".service"
	}
	switch {
	case !u.literal:
		return "юнит вычисляется при запуске, проверить его заранее нельзя"
	case strings.ContainsAny(name, "*?["):
		return "шаблон выбирает юниты при запуске, среди них может быть юнит этой сессии"
	case strings.HasSuffix(name, ".target") || name == "-.slice":
		return "с целью или корневым срезом остановятся связанные юниты, проверка их не сверяет"
	}
	return hosts[name]
}

// userManagerRe юнит менеджера пользователя в пути cgroup: user@1000.service
var userManagerRe = regexp.MustCompile(`^user@[0-9]+\.service$`)

// sessionUnits юниты, в cgroup которых идут предки этой сессии и процессы,
// без которых падает сессия пользователя, — с причиной. user — юниты
// менеджера пользователя (после user@UID.service в пути), иначе системного
func (t *KillGuardTool) sessionUnits(procs []procInfo, user bool) map[string]string {
	ancestors := t.ancestors(procs)
	out := map[string]string{}
	for _, p := range procs {
		why := t.dangerous(p.pid, procs, ancestors)
		if why == "" || p.pid <= 1 {
			continue
		}
		data, err := os.ReadFile(filepath.Join(t.procRoot, fmt.Sprint(p.pid), "cgroup"))
		if err != nil {
			t.Logger().Debug("cgroup не читается", "pid", p.pid, "error", err)
			continue
		}
		for line := range strings.Lines(string(data)) {
			_, path, _ := strings.Cut(strings.TrimSpace(line), "::")
			parts := strings.Split(strings.Trim(path, "/"), "/")
			k := slices.IndexFunc(parts, userManagerRe.MatchString)
			units := parts
			if k >= 0 && user {
				units = parts[k+1:]
			} else if k >= 0 {
				units = parts[:k+1]
			} else if user {
				units = nil
			}
			for _, u := range units {
				if out[u] == "" {
					out[u] = fmt.Sprintf("в юните идёт %s (PID %d) — %s", p.comm, p.pid, why)
				}
			}
		}
	}
	return out
}

// portRe порт fuser: 8080/tcp, 8080,host,80/udp
var portRe = regexp.MustCompile(`^[0-9]*(,[^/]*)?/(tcp|udp)$`)

// barePortRe порт без /tcp при fuser -n tcp: 8080, 8080,host,80
var barePortRe = regexp.MustCompile(`^[0-9]+(,.*)?$`)

// fuserArgs разобранные аргументы fuser: -k, -m (вся файловая система),
// -n tcp|udp (имена — порты) и имена
type fuserArgs struct {
	kill, mount, ports bool
	names              []word
}

// parseFuserArgs разбирает опции fuser; -SIGNAL (-TERM, -9) — сигнал
func parseFuserArgs(args []word) fuserArgs {
	var fa fuserArgs
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a.text == "--kill":
			fa.kill = true
		case a.text == "--mount" || a.text == "--ismountpoint":
			fa.mount = true
		case a.text == "--namespace":
			fa.ports = isPortSpace(optionValue(args, &i, "").text)
		case a.text == "-" || strings.HasPrefix(a.text, "--") || len(a.text) > 1 && a.text[0] == '-' && signalNameRe.MatchString(a.text[1:]):
		case len(a.text) > 1 && a.text[0] == '-':
			fa.shortOptions(args, &i)
		default:
			fa.names = append(fa.names, a)
		}
	}
	return fa
}

// shortOptions связка коротких опций fuser: -k, -m, -M, -n со значением
func (fa *fuserArgs) shortOptions(args []word, i *int) {
	a := args[*i].text
	for k := 1; k < len(a); k++ {
		switch a[k] {
		case 'k':
			fa.kill = true
		case 'm', 'M':
			fa.mount = true
		case 'n':
			fa.ports = isPortSpace(optionValue(args, i, a[k+1:]).text)
			return
		}
	}
}

// isPortSpace пространство имён fuser -n, где имена — порты
func isPortSpace(space string) bool { return space == "tcp" || space == "udp" }

// fuserReason fuser -k шлёт сигнал всем, кто держит файл, каталог или
// терминал (оболочка в каталоге, claude с открытым журналом, терминал на
// pts), с -m — всем на файловой системе. Порт (8080/tcp) слушает сервер,
// которого просили остановить, — можно
func fuserReason(args []word, via string) string {
	fa := parseFuserArgs(args)
	switch {
	case !fa.kill:
		return ""
	case via != "":
		return "fuser -k через " + via + ": цели приходят из ввода, проверить их до запуска нельзя"
	case fa.mount:
		return "fuser -km шлёт сигнал всем процессам на файловой системе: оболочке, claude, терминалу"
	}
	for _, n := range fa.names {
		switch {
		case !n.literal:
			return fmt.Sprintf("fuser -k %s: цель вычисляется при запуске, проверить её заранее нельзя", n.text)
		case portRe.MatchString(n.text) || fa.ports && barePortRe.MatchString(n.text):
		default:
			return fmt.Sprintf("fuser -k %s шлёт сигнал всем, кто держит файл, каталог или терминал (оболочка в каталоге, claude, терминал на pts): посмотри fuser -v %s и убей PID литералом", n.text, n.text)
		}
	}
	return ""
}

// skillReason skill выбирает процессы по терминалу, пользователю или
// команде (skill -9 -u aisee — все сессии): справка, список сигналов и
// -n (только показать) никого не трогают
func skillReason(args []word) string {
	if len(args) == 0 || slices.ContainsFunc(args, func(a word) bool {
		return slices.Contains([]string{"-l", "-L", "-V", "-h", "-n", "--list", "--table", "--help", "--version", "--no-action"}, a.text)
	}) {
		return ""
	}
	return "skill выбирает процессы по терминалу, пользователю или команде (skill -9 -u $USER гасит все сессии): проверка его не разбирает"
}

// tmuxReason tmux kill-server гасит все сессии tmux на сокете, kill-session,
// kill-window и kill-pane без -t — текущую (вне tmux — последнюю), с -a —
// все, кроме неё: сессия Claude может быть среди них. Свой сервер (-L, -S
// в глобальных опциях) и цель литералом (-t name) — можно
func tmuxReason(args []word) string {
	i := 0
	for ; i < len(args) && strings.HasPrefix(args[i].text, "-"); i++ {
		a := args[i].text
		if a == "--" {
			i++
			break
		}
		if strings.ContainsAny(a, "LS") {
			return ""
		}
		if strings.ContainsAny(a[len(a)-1:], "fcTLS") {
			i++
		}
	}
	var segment []word
	for _, a := range append(slices.Clone(args[min(i, len(args)):]), word{text: ";", literal: true}) {
		if a.text != ";" {
			segment = append(segment, a)
			continue
		}
		if reason := tmuxCommandReason(segment); reason != "" {
			return reason
		}
		segment = nil
	}
	return ""
}

// tmuxCommandReason одна команда tmux из цепочки через ;
func tmuxCommandReason(cmd []word) string {
	if len(cmd) == 0 {
		return ""
	}
	name := cmd[0].text
	switch name {
	case "kill-server":
		return "tmux kill-server гасит все сессии tmux, в том числе ту, где может идти Claude; свой сервер — tmux -L имя"
	case "kill-session", "kill-window", "killw", "kill-pane", "killp":
	default:
		return ""
	}
	target := false
	for i := 1; i < len(cmd); i++ {
		a := cmd[i].text
		switch {
		case a == "-t":
			v := optionValue(cmd, &i, "")
			if !v.literal {
				return fmt.Sprintf("tmux %s -t %s: цель вычисляется при запуске, проверить её заранее нельзя", name, v.text)
			}
			target = true
		case strings.HasPrefix(a, "-t"):
			target = cmd[i].literal
		case len(a) > 1 && a[0] == '-' && strings.Contains(a, "a"):
			return "tmux " + name + " -a гасит все, кроме текущей: среди них может быть сессия Claude"
		}
	}
	if !target {
		return "tmux " + name + " без -t гасит текущую (вне tmux — последнюю) сессию, окно или панель: это может быть сессия Claude; назови цель: -t имя"
	}
	return ""
}

// pythonNameRe интерпретатор python: python, python3, python3.12
var pythonNameRe = regexp.MustCompile(`^python[0-9.]*$`)

// pyKillRe вызов os.kill или os.killpg с двумя аргументами (один уровень скобок в каждом)
var pyKillRe = regexp.MustCompile(`\bos\s*\.\s*(kill|killpg)\s*\(\s*((?:[^,()]|\([^()]*\))*?)\s*,\s*((?:[^,()]|\([^()]*\))*?)\s*\)`)

// pyIntRe целое литералом
var pyIntRe = regexp.MustCompile(`^-?[0-9]+$`)

// pyKillWordRe упоминание os.kill или os.killpg в скрипте
var pyKillWordRe = regexp.MustCompile(`\bos\s*\.\s*kill(pg)?\b`)

// pythonReason os.kill и os.killpg в скрипте python, который виден в
// команде (-c или here-doc и here-string на вход без файла скрипта): PID —
// по правилам kill (литерал чужого процесса — можно), killpg — группа,
// сигнал 0 — можно. Вызов, который не разобрать, — отказ
func (t *KillGuardTool) pythonReason(c shellcmd.Call, args []word, procs []procInfo) (string, bool) {
	for _, script := range pythonScripts(c, args) {
		calls := pyKillRe.FindAllStringSubmatch(script, -1)
		if len(calls) < len(pyKillWordRe.FindAllString(script, -1)) {
			return c.Name + ": os.kill в скрипте не разобрать (PID и сигнал — два простых аргумента), проверить цель нельзя", false
		}
		for _, m := range calls {
			if m[3] == "0" || m[1] == "kill" && m[2] == "os.getpid()" {
				// сигнал 0 или сигнал себе: python завершит только себя
				continue
			}
			if m[1] == "killpg" {
				return fmt.Sprintf("%s: os.killpg(%s, …) шлёт сигнал группе процессов целиком; кто в ней, проверка не сверяет", c.Name, m[2]), true
			}
			pid := word{text: m[2], literal: pyIntRe.MatchString(m[2])}
			if reason, group := t.checkKill([]word{{text: "--", literal: true}, pid}, procs); reason != "" {
				return fmt.Sprintf("%s: os.kill(%s, …): %s", c.Name, m[2], reason), group
			}
		}
	}
	return "", false
}

// pythonScripts тексты, которые python исполнит как скрипт: значение -c,
// а без файла скрипта и модуля (-m) — тело here-doc и here-string на вход
func pythonScripts(c shellcmd.Call, args []word) []string {
	var scripts []string
	file := false
	for i := 0; i < len(args); i++ {
		switch a := args[i].text; {
		case a == "-c":
			scripts = append(scripts, optionValue(args, &i, "").text)
			i = len(args)
		case a == "-m" || a == "-W" || a == "-X":
			// модуль или файл запускает свой код, а у -W, -X значение
			i++
			file = file || a == "-m"
		case a == "-":
		case strings.HasPrefix(a, "-"):
		default:
			file = true
			i = len(args)
		}
	}
	if !file {
		for _, r := range c.Redirs {
			switch {
			case r.Op == "<<" || r.Op == "<<-":
				scripts = append(scripts, r.Body)
			case r.Op == "<<<":
				scripts = append(scripts, r.Target)
			}
		}
	}
	return scripts
}
