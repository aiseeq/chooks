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
// systemctl --user stop, fuser -k, busctl call … StopUnit). У skill, tmux kill-* и os.kill в
// python -c подстрока kill есть всегда
var sessionToolWordRe = regexp.MustCompile(`(^|[^[:alnum:]_.-])(loginctl|systemctl|fuser|busctl|gdbus|dbus-send|qdbus(6|-qt[56])?)([^[:alnum:]_.-]|$)`)

// sessionTool проверяет вызов средств, гасящих процессы или сессию, кроме
// kill, pkill и killall: loginctl, systemctl, вызов тех же действий по
// D-Bus (busctl, gdbus, dbus-send, qdbus), fuser -k, skill, os.kill в
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
	hint := toolHints[c.Name]
	switch {
	case c.Name == "loginctl":
		reason = loginctlReason(args)
	case c.Name == "systemctl":
		if err := s.loadProcs(); err != nil {
			return err
		}
		reason, hint = s.tool.systemctlReason(args, via, s.procs)
	case c.Name == "fuser":
		reason = fuserReason(args, via)
	case dbusTools[c.Name] != nil:
		dc, ok := dbusTools[c.Name](args, via)
		if !ok {
			return nil
		}
		if err := s.loadProcs(); err != nil {
			return err
		}
		reason, hint = s.tool.dbusReason(dc, via, s.procs)
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
		if hint != "" {
			if s.toolHints == nil {
				s.toolHints = map[string]string{}
			}
			s.toolHints[reason] = hint
		}
	}
	if group && !slices.Contains(s.groupReasons, reason) {
		s.groupReasons = append(s.groupReasons, reason)
	}
	return nil
}

// toolHints подсказка к отказу на средство, где узкая цель — не PID: сессия
// tmux, порт. У systemctl она своя по отказу (systemctlHint)
var toolHints = map[string]string{
	"tmux":  "Назови цель литералом: tmux kill-session -t имя",
	"fuser": "Укажи порт вместо файла: fuser -k 8080/tcp",
}

// systemctlHint подсказка к отказу systemctl с менеджером scope (systemctl
// или systemctl --user): юнит из переменной, шаблона или ввода — назвать
// литералом; юнит назван, но задевает сессию, — остановить юнит без
// процессов сессии или лишний процесс по PID. Команде без юнита, гасящей
// сессию целиком (systemctlEndVerbs), — systemctlEndHint
func systemctlHint(scope string, named bool) string {
	if !named {
		return "Назови юнит литералом и проверь, что он не держит сессию: " + scope + " status юнит"
	}
	return "Останавливай юнит, где нет процессов сессии: посмотри их (" + scope + " status юнит) и убей лишний процесс литералом: kill 12345"
}

// systemctlEndEffect что делает команда из systemctlEndVerbs по man
// systemctl: смена цели (isolate, default, rescue, emergency) — у любого
// менеджера; exit у менеджера --user гасит сам менеджер; прочее — то, что
// пишет systemctlSystemEffects, и с --user тоже: эти действия поддерживает
// только системный менеджер, и systemctl может уйти в logind и погасить
// машину. Одна фраза на отказ и подсказку, чтобы они не расходились
func systemctlEndEffect(scope, verb string) string {
	if slices.Contains(systemctlTargetVerbs, verb) {
		if scope == "systemctl" {
			return "переключает цель системы и гасит юниты вне неё"
		}
		return "переключает цель менеджера " + scope + " и гасит юниты вне неё"
	}
	if verb == "exit" && scope != "systemctl" {
		return "гасит менеджер " + scope
	}
	return systemctlSystemEffects[verb]
}

// systemctlEndHint подсказка к отказу на команду без юнита, гасящую сессию
// целиком (systemctl --user exit, systemctl poweroff, systemctl isolate):
// подсказка про юнит из status тут не к месту
func systemctlEndHint(scope, verb string) string {
	return "Команда " + systemctlEndEffect(scope, verb) + ": такое запускает пользователь сам. Остановить один юнит без процессов сессии — назови его литералом: " + scope + " stop юнит"
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

// loginEndEffect что делают loginctl terminate-*, kill-* и те же методы
// logind по D-Bus (KillSession, TerminateUser)
const loginEndEffect = "завершает процессы сеанса или всех сеансов пользователя: сессии Claude, терминал и графическую оболочку"

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
		return "loginctl " + verb + " " + loginEndEffect
	}
	return ""
}

// systemctlValues опции systemctl со значением отдельным словом
var systemctlValues = []string{
	"t", "type", "s", "signal", "p", "property", "P", "H", "host", "M", "machine", "n", "lines", "o", "output",
	"state", "job-mode", "root", "image", "kill-whom", "kill-value", "what", "timestamp", "when", "message",
	"boot-loader-entry", "reboot-argument", "drop-in", "check-inhibitors", "preset-mode",
}

// systemctlEndVerbs команды systemctl без юнита, которые гасят сессию
// целиком: переключение цели, выход менеджера, выключение, смена корня
var systemctlEndVerbs = []string{"isolate", "exit", "poweroff", "reboot", "halt", "kexec", "soft-reboot", "emergency", "rescue", "default", "switch-root"}

// systemctlTargetVerbs команды из systemctlEndVerbs, которые переключают
// цель менеджера и останавливают юниты вне её (default, rescue и emergency
// — isolate своей цели)
var systemctlTargetVerbs = []string{"isolate", "default", "rescue", "emergency"}

// systemctlSystemEffects что делают прочие команды из systemctlEndVerbs
// (man systemctl; их выполняет только системный менеджер, и с --user тоже
// — кроме exit у --user, который гасит свой менеджер): exit вне контейнера — то же, что
// poweroff; soft-reboot перезапускает систему, оставляя ядро; switch-root
// машину не гасит, а меняет корень и запускает на нём новый системный менеджер
var systemctlSystemEffects = map[string]string{
	"poweroff": "выключает машину", "exit": "выключает машину", "halt": "останавливает машину",
	"reboot": "перезагружает машину", "kexec": "перезагружает машину",
	"soft-reboot": "перезапускает систему без перезагрузки ядра",
	"switch-root": "меняет корень системы и запускает на нём новый системный менеджер",
}

// systemctlStopVerbs команды systemctl, которые останавливают, перезапускают
// или замораживают процессы юнитов (disable и mask — с --now)
var systemctlStopVerbs = []string{"stop", "kill", "restart", "try-restart", "reload-or-restart", "try-reload-or-restart", "condrestart", "force-reload", "freeze"}

// systemctlRestartVerbs команды systemctl из systemctlStopVerbs, которые
// запускают юнит снова
var systemctlRestartVerbs = []string{"restart", "try-restart", "reload-or-restart", "try-reload-or-restart", "condrestart", "force-reload"}

// restartableComms юниты (имя без .service и .socket), рестарт которых
// сессию не гасит, и процесс в них: plasmashell (панель, рабочий стол)
// поднимается заново, окна и терминалы живут — в отличие от kwin; звук
// (pipewire, wireplumber) перезапускают, когда он пропал. pipewire-pulse
// сессию не держит (нет в protectedComms): его юнит можно и так. Стоп и
// kill — отказ, как у прочих
var restartableComms = map[string]string{
	"plasma-plasmashell": "plasmashell", "pipewire": "pipewire", "wireplumber": "wireplumber",
}

// restartableComm процесс, которому рестарт юнита name (с типом) не
// страшен; "" — такого нет
func restartableComm(name string) string {
	base, ok := strings.CutSuffix(name, ".service")
	if !ok {
		base, _ = strings.CutSuffix(name, ".socket")
	}
	return restartableComms[base]
}

// unitSuffixes типы юнитов: имя без них systemctl дополняет .service
var unitSuffixes = []string{".service", ".socket", ".target", ".scope", ".slice", ".timer", ".mount", ".automount", ".path", ".device", ".swap"}

// systemctlReason отказ на systemctl, который гасит юнит, где идёт предок
// этой сессии или процесс, на котором держится сессия пользователя (по
// cgroup процесса: user.slice/…/user@1000.service/app.slice/app-…scope),
// а также на юнит из переменной, шаблон юнитов, цель (.target: что
// остановится с ней, не видно) и команды, гасящие весь менеджер. Юниты,
// пришедшие из ввода (xargs), — тоже отказ. hint — подсказка к отказу.
// Решение — unitActionReason, общее с вызовом тех же действий по D-Bus
func (t *KillGuardTool) systemctlReason(args []word, via string, procs []procInfo) (reason, hint string) {
	user, now, zero := systemctlFlags(args)
	ops := optionOperands(args, systemctlValues)
	if len(ops) == 0 {
		return "", ""
	}
	a := unitAction{user: user, now: now, zero: zero, verb: ops[0].text, units: ops[1:]}
	return t.unitActionReason(a.scope()+" "+a.verb, a, via, procs)
}

// unitAction действие менеджера systemd над юнитами: verb — команда
// systemctl (stop, kill, isolate), user — менеджер пользователя, now — с
// --now (disable и mask останавливают), zero — сигнал 0 у kill
type unitAction struct {
	user, now, zero bool
	verb            string
	units           []word
}

// scope менеджер действия так, как его пишет systemctl
func (a unitAction) scope() string {
	if a.user {
		return "systemctl --user"
	}
	return "systemctl"
}

// unitActionReason отказ на действие a: гасит юнит сессии или весь
// менеджер. label — как действие названо в причине (systemctl --user stop,
// busctl call …KillUnit); via — обёртка, подающая юниты из ввода
func (t *KillGuardTool) unitActionReason(label string, a unitAction, via string, procs []procInfo) (reason, hint string) {
	scope, verb := a.scope(), a.verb
	switch {
	case slices.Contains(systemctlEndVerbs, verb):
		return label + " " + systemctlEndEffect(scope, verb) + ": гаснут сессии Claude, терминал и графическая оболочка", systemctlEndHint(scope, verb)
	case verb == "kill" && a.zero:
		return "", ""
	case slices.Contains(systemctlStopVerbs, verb) || a.now && (verb == "disable" || verb == "mask"):
	default:
		return "", ""
	}
	if via != "" {
		return label + " через " + via + ": юниты приходят из ввода, проверить их до запуска нельзя", systemctlHint(scope, false)
	}
	hosts := t.sessionUnits(procs, a.user, "")
	for _, u := range a.units {
		h := hosts
		if spare := restartableComm(unitName(u.text)); spare != "" && slices.Contains(systemctlRestartVerbs, verb) {
			h = t.sessionUnits(procs, a.user, spare)
		}
		if reason := unitReason(u, h); reason != "" {
			named := u.literal && !strings.ContainsAny(u.text, "*?[{")
			return fmt.Sprintf("%s %s: %s", label, shownUnit(u.text), reason), systemctlHint(scope, named)
		}
	}
	return "", ""
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

// unitName имя юнита с типом: имя без типа systemctl дополняет .service
func unitName(text string) string {
	if !hasUnitSuffix(text) {
		return text + ".service"
	}
	return text
}

// hasUnitSuffix — у имени есть тип юнита (.service, .scope)
func hasUnitSuffix(text string) bool {
	return slices.ContainsFunc(unitSuffixes, func(s string) bool { return strings.HasSuffix(text, s) })
}

// sessionScopeRe юнит сеанса logind: session-3.scope, session-c2.scope
var sessionScopeRe = regexp.MustCompile(`^session-[^.]+\.scope$`)

// unitAliases алиасы юнитов, за которыми идёт процесс сессии: в cgroup
// стоит имя настоящего юнита (dbus.service → dbus-broker.service)
var unitAliases = map[string][]string{
	"display-manager.service": {"sddm.service", "gdm.service", "lightdm.service"},
	"dbus.service":            {"dbus-broker.service", "dbus-daemon.service"},
}

// unitHosts юниты, чьи процессы гаснут со стопом юнита name: он сам, за
// сокетом — служба того же имени (dbus.socket → dbus.service: служба
// требует свой сокет и останавливается с ним), за алиасом — настоящий юнит
func unitHosts(name string) []string {
	out := []string{name}
	if base, ok := strings.CutSuffix(name, ".socket"); ok {
		out = append(out, base+".service")
	}
	for _, n := range out {
		out = append(out, unitAliases[n]...)
	}
	return out
}

// unitReason почему юнит u останавливать нельзя: вычисляется, пустое имя,
// шаблон или {…}, цель или корневой срез, сеанс logind, юнит сессии из
// hosts (и за сокетом или алиасом); "" — можно
func unitReason(u word, hosts map[string]string) string {
	name := unitName(u.text)
	switch {
	case !u.literal:
		return "юнит вычисляется при запуске, проверить его заранее нельзя"
	case u.text == "":
		// systemd по D-Bus (KillUnit "") берёт юнит вызывающего
		return "пустое имя — юнит самой команды, то есть этой сессии Claude"
	case strings.ContainsAny(name, "*?[{"):
		return "шаблон или {…} выбирает юниты при запуске, среди них может быть юнит этой сессии"
	case strings.HasSuffix(name, ".target") || name == "-.slice":
		return "с целью или корневым срезом остановятся связанные юниты, проверка их не сверяет"
	case sessionScopeRe.MatchString(name):
		return "сеанс logind: с ним завершатся процессы сеанса, как от loginctl terminate-session"
	}
	for _, n := range unitHosts(name) {
		switch {
		case hosts[n] == "":
		case n == name:
			return hosts[n]
		default:
			return "за ним " + n + ": " + hosts[n]
		}
	}
	return ""
}

// userManagerRe юнит менеджера пользователя в пути cgroup: user@1000.service
var userManagerRe = regexp.MustCompile(`^user@[0-9]+\.service$`)

// sessionUnits юниты, в cgroup которых идут предки этой сессии и процессы,
// без которых падает сессия пользователя, — с причиной. user — юниты
// менеджера пользователя (после user@UID.service в пути), иначе системного.
// spare — имя процесса, которому рестарт не страшен: он не держит юнит,
// если не предок сессии
func (t *KillGuardTool) sessionUnits(procs []procInfo, user bool, spare string) map[string]string {
	ancestors := t.ancestors(procs)
	out := map[string]string{}
	for _, p := range procs {
		why := t.dangerous(p.pid, procs, ancestors)
		if why == "" || p.pid <= 1 || p.comm == spare && !ancestors[p.pid] {
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
// в глобальных опциях, кроме сервера по умолчанию) и цель литералом (-t
// name) — можно
func tmuxReason(args []word) string {
	i, own := tmuxGlobalOptions(args)
	if own {
		return ""
	}
	var segment []word
	for _, a := range append(tmuxWords(args[i:]), word{text: ";", literal: true}) {
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

// tmuxGlobalOptions разбирает опции tmux до команды: i — первое слово
// команды, own — сервер не тот, где по умолчанию идут сессии. -c, -f, -L,
// -S, -T берут значение (остаток связки или следующее слово), прочие буквы
// и -- значения не берут; -S задаёт сокет и перекрывает -L, из нескольких
// -L действует последний. Сервер по
// умолчанию — -L default и сокет default в каталоге tmux-UID
// (/tmp/tmux-1000/default, "$TMUX_TMPDIR/tmux-$UID/default": у слова с $
// текст — исходник с кавычками)
func tmuxGlobalOptions(args []word) (int, bool) {
	label, path := "default", ""
	i := 0
	for ; i < len(args) && strings.HasPrefix(args[i].text, "-"); i++ {
		a := args[i].text
		k := strings.IndexAny(a, "cfLST")
		if k < 0 {
			continue
		}
		switch v := optionValue(args, &i, a[k+1:]).text; a[k] {
		case 'L':
			label = v
		case 'S':
			path = v
		}
	}
	if path != "" {
		return min(i, len(args)), !defaultTmuxSocket(unquoted(path))
	}
	return min(i, len(args)), label != "default"
}

// defaultTmuxSocket сокет сервера tmux по умолчанию: default в каталоге
// tmux-UID
func defaultTmuxSocket(path string) bool {
	p := filepath.Clean(path)
	return filepath.Base(p) == "default" && strings.HasPrefix(filepath.Base(filepath.Dir(p)), "tmux-")
}

// tmuxWords слова команд tmux с разделителями: слово, кончающееся на ;
// (kill-server\; в оболочке), tmux делит на слово без ; и разделитель
// команд, а \; на конце слова — литерал ;. Отдельное ; даёт пустое слово
// и разделитель: пустое слово цели не задаёт и команды не называет
func tmuxWords(args []word) []word {
	var out []word
	for _, a := range args {
		body, ok := strings.CutSuffix(a.text, ";")
		switch {
		case !ok:
			out = append(out, a)
		case strings.HasSuffix(body, `\`):
			out = append(out, word{text: strings.TrimSuffix(body, `\`) + ";", literal: a.literal})
		default:
			out = append(out, word{text: body, literal: a.literal}, word{text: ";", literal: true})
		}
	}
	return out
}

// tmuxKillCommands команды tmux, гасящие сервер, сессию, окно или панель
var tmuxKillCommands = []string{"kill-server", "kill-session", "kill-window", "kill-pane"}

// tmuxCommand полное имя команды tmux kill-*: tmux принимает и алиас
// (killw, killp), и однозначное начало имени (kill-ser, kill-ses, kill-p).
// Других команд на kill у tmux нет; прочее — как есть
func tmuxCommand(name string) string {
	switch name {
	case "killw":
		return "kill-window"
	case "killp":
		return "kill-pane"
	}
	var found []string
	for _, c := range tmuxKillCommands {
		if strings.HasPrefix(c, name) {
			found = append(found, c)
		}
	}
	if len(found) == 1 {
		return found[0]
	}
	return name
}

// tmuxCommandReason одна команда tmux из цепочки через ;
func tmuxCommandReason(cmd []word) string {
	if len(cmd) == 0 {
		return ""
	}
	name := tmuxCommand(cmd[0].text)
	switch {
	case name == "kill-server":
		return "tmux kill-server гасит все сессии tmux, в том числе ту, где может идти Claude; свой сервер — tmux -L имя"
	case !slices.Contains(tmuxKillCommands, name):
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
			// пустая цель (-t '') у tmux — текущая
			target = v.text != ""
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

// shownUnit имя юнита в причине отказа: пустое — кавычками
func shownUnit(name string) string {
	if name == "" {
		return `""`
	}
	return name
}
