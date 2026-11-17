package tools

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// dbusTools средства командной строки, вызывающие методы D-Bus: через них
// systemd останавливает юниты (Manager.KillUnit, Unit.Stop), а logind
// завершает сеансы (KillSession) мимо systemctl и loginctl
var dbusTools = map[string]func(args []word, via string) (dbusCall, bool){
	"busctl":    busctlCall,
	"gdbus":     gdbusCall,
	"dbus-send": dbusSendCall,
	"qdbus":     qdbusCall,
	"qdbus6":    qdbusCall,
	"qdbus-qt5": qdbusCall,
	"qdbus-qt6": qdbusCall,
	"kquitapp":  kquitappCall,
	"kquitapp5": kquitappCall,
	"kquitapp6": kquitappCall,
}

// dbusCall вызов метода D-Bus из командной строки. bus — шина: user (шина
// сессии, на ней менеджер systemd --user), system, capsule (шина capsule
// busctl -C — не этой сессии), "" — адрес, менеджер по нему не виден.
// member — Интерфейс.Метод или Метод (qdbus). shiftAt — номер аргумента, с
// которого позиции слов не сопоставить (тип сигнатуры busctl перед ним —
// массив, структура или variant: его значение занимает не одно слово); -1
// — все на месте
type dbusCall struct {
	tool, bus          string
	dest, path, member word
	args               []word
	shiftAt            int
}

// shifted — позиция i-го аргумента не видна (shiftAt)
func (dc dbusCall) shifted(i int) bool {
	return dc.shiftAt >= 0 && i >= dc.shiftAt
}

// busctlShift shiftAt для сигнатуры busctl: первый тип с a, (, { или v;
// сигнатура из переменной — с нулевого
func busctlShift(sig word) int {
	if !sig.literal {
		return 0
	}
	if k := strings.IndexAny(sig.text, "a({v"); k >= 0 {
		return k
	}
	return -1
}

// arg i-й аргумент вызова без типа; нет его — пустой литерал, а под xargs
// — вычисляемый: его допишет ввод. i < 0 — у метода такого аргумента нет
func (dc dbusCall) arg(i int, via string) word {
	switch {
	case i < 0:
		return word{literal: true}
	case i < len(dc.args):
		return dc.args[i]
	}
	return absent(via)
}

// absent слово, которого нет в команде: под xargs его допишет ввод
func absent(via string) word {
	return word{literal: via == ""}
}

// operand i-е слово ops или absent
func operand(ops []word, i int, via string) word {
	if i < len(ops) {
		return ops[i]
	}
	return absent(via)
}

// busctlValues опции busctl со значением отдельным словом
var busctlValues = []string{
	"H", "host", "M", "machine", "C", "capsule", "address", "match", "size", "json", "expect-reply", "auto-start",
	"allow-interactive-authorization", "timeout", "augment-creds", "watch-bind", "destination", "N", "limit-messages", "J",
}

// busctlCall busctl [--user|--system|--address A] call SERVICE OBJECT
// INTERFACE METHOD [SIGNATURE [ARGUMENT…]]; шина по умолчанию — системная.
// Прочие команды (list, introspect, tree, monitor, get-property) — не вызов
func busctlCall(args []word, via string) (dbusCall, bool) {
	dc := dbusCall{tool: "busctl call", bus: busctlBus(args)}
	ops := optionOperands(args, busctlValues)
	if len(ops) == 0 || ops[0].literal && ops[0].text != "call" {
		return dc, false
	}
	dc.dest, dc.path = operand(ops, 1, via), operand(ops, 2, via)
	iface, method := operand(ops, 3, via), operand(ops, 4, via)
	dc.member = word{text: iface.text + "." + method.text, literal: iface.literal && method.literal}
	sig := operand(ops, 5, via)
	dc.shiftAt = busctlShift(sig)
	if len(ops) > 6 {
		dc.args = ops[6:]
	}
	return dc, true
}

// busctlBus шина вызова по опциям busctl: system (по умолчанию), user
// (--user), "" (--address — любая), capsule (-C, --capsule — менеджер
// пользователя capsule, и с --user тоже). Опция — слово до --, кроме
// значения опции (--match -Cx: -Cx — правило --match); в связке коротких
// опций (-qC foo) буква со значением забирает остаток или следующее слово.
// busctl, как getopt, разбирает опции и после операндов: --user и
// --system там действуют. Капсула учитывается только до первого операнда:
// она ведёт к пропуску, и -C среди аргументов метода (строка «-Cx») пропуска
// не даёт — лишний отказ на редкую форму дешевле обхода. Капсула вместе с
// транспортом (--address, -M, -H) — не капсула: какую шину выберет busctl
// при обоих, не разбираем, и вызов проверяется по шине
func busctlBus(args []word) string {
	bus, capsule, operand, transport := "system", false, false, false
	for i := 0; i < len(args); i++ {
		t := args[i].text
		switch {
		case t == "--":
			i = len(args)
		case strings.HasPrefix(t, "--"):
			name, _, inline := strings.Cut(t[2:], "=")
			switch name {
			case "user":
				bus = "user"
			case "system":
				bus = "system"
			case "address":
				bus, transport = "", true
			case "machine", "host":
				transport = true
			case "capsule":
				capsule = capsule || !operand
			}
			if !inline && slices.Contains(busctlValues, name) {
				i++
			}
		case len(t) > 1 && t[0] == '-':
			letter, _, next := shortValue(t, busctlValues)
			capsule = capsule || letter == "C" && !operand
			transport = transport || letter == "M" || letter == "H"
			if next {
				i++
			}
		default:
			operand = true
		}
	}
	if capsule && !transport {
		return "capsule"
	}
	return bus
}

// shortValue опция со значением в связке коротких опций (-qC foo, -Cfoo):
// её буква, значение в той же связке (rest: -s9 → 9) и next — значение в
// следующем слове (буква последняя в связке). "" — в связке такой нет
func shortValue(bundle string, values []string) (letter, rest string, next bool) {
	for j := 1; j < len(bundle); j++ {
		if l := bundle[j : j+1]; slices.Contains(values, l) {
			return l, bundle[j+1:], j == len(bundle)-1
		}
	}
	return "", "", false
}

// gdbusValues опции gdbus call со значением: буква — длинное имя
var gdbusValues = map[string]string{"a": "address", "d": "dest", "o": "object-path", "m": "method", "t": "timeout"}

// gdbusCall gdbus call --session|--system|--address A --dest D
// --object-path P --method I.M [ARG…]: аргументы — текст GVariant
// ('x.service', int32 15). Прочие команды gdbus (introspect, monitor) — не вызов
func gdbusCall(args []word, via string) (dbusCall, bool) {
	dc := dbusCall{tool: "gdbus call", shiftAt: -1}
	if len(args) == 0 || args[0].literal && args[0].text != "call" {
		return dc, false
	}
	dc.dest, dc.path, dc.member = absent(via), absent(via), absent(via)
	values := map[string]*word{"dest": &dc.dest, "object-path": &dc.path, "method": &dc.member}
	for i := 1; i < len(args); i++ {
		a := args[i].text
		name, v, ok := gdbusOption(args, &i)
		switch {
		case a == "--":
			for _, w := range args[i+1:] {
				dc.args = append(dc.args, gvariantArg(w))
			}
			i = len(args)
		case !ok:
			dc.args = append(dc.args, gvariantArg(args[i]))
		case name == "session" || name == "e":
			dc.bus = "user"
		case name == "system" || name == "y":
			dc.bus = "system"
		case name == "address":
			dc.bus = ""
		case values[name] != nil:
			*values[name] = v
		}
	}
	return dc, true
}

// gdbusOption опция gdbus в args[*i]: длинное имя опции со значением (или
// флаг: session, e) и значение; ok false — не опция, а аргумент метода
func gdbusOption(args []word, i *int) (string, word, bool) {
	a := args[*i].text
	switch {
	case strings.HasPrefix(a, "--"):
		name, val, eq := strings.Cut(a[2:], "=")
		if !gdbusLongValue(name) {
			return name, word{}, true
		}
		if eq {
			return name, word{text: val, literal: args[*i].literal}, true
		}
		return name, optionValue(args, i, ""), true
	case len(a) > 1 && a[0] == '-' && args[*i].literal:
		if long, ok := gdbusValues[a[1:2]]; ok {
			return long, optionValue(args, i, a[2:]), true
		}
		return a[len(a)-1:], word{}, true
	}
	return "", word{}, false
}

// gdbusLongValue длинная опция gdbus call со значением
func gdbusLongValue(name string) bool {
	for _, long := range gdbusValues {
		if long == name {
			return true
		}
	}
	return false
}

// gvariantTypeRe тип перед значением в тексте GVariant: int32 15, @i 15
var gvariantTypeRe = regexp.MustCompile(`^(@\S+|boolean|byte|int16|uint16|int32|uint32|int64|uint64|handle|double|string|objectpath|signature)\s+`)

// gvariantArg аргумент gdbus call: значение GVariant без типа и кавычек
// ('x.service' → x.service). Строку с битой escape-последовательностью
// gdbus передаст как есть, с кавычками, — имя не проверить: не литерал
func gvariantArg(w word) word {
	v := strings.TrimSpace(w.text)
	for {
		t := gvariantTypeRe.ReplaceAllString(v, "")
		if t == v {
			break
		}
		v = t
	}
	if len(v) < 2 || v[0] != '\'' && v[0] != '"' || v[len(v)-1] != v[0] {
		return word{text: v, literal: w.literal}
	}
	text, err := gvariantUnescape(v[1 : len(v)-1])
	return word{text: text, literal: w.literal && err == nil}
}

// gvariantEscapes escape-последовательности строки GVariant с особым
// значением; прочие \c — сам символ c (\x2d — x2d, \055 — 055, \- — -),
// как у g_variant_parse (сверено с GLib 17.11)
var gvariantEscapes = map[byte]rune{'a': '\a', 'b': '\b', 'f': '\f', 'n': '\n', 'r': '\r', 't': '\t', 'v': '\v'}

// gvariantUnescape тело строки GVariant в кавычках: \uXXXX, \UXXXXXXXX,
// \n и прочие; ошибка — последовательность битая (\u002, \ в конце)
func gvariantUnescape(body string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(body); i++ {
		if body[i] != '\\' {
			b.WriteByte(body[i])
			continue
		}
		i++
		if i >= len(body) {
			return b.String(), fmt.Errorf("\\ в конце строки GVariant")
		}
		switch c := body[i]; {
		case c == 'u' || c == 'U':
			n := map[byte]int{'u': 4, 'U': 8}[c]
			if i+1+n > len(body) {
				return b.String(), fmt.Errorf("\\%c: меньше %d цифр", c, n)
			}
			r, err := strconv.ParseUint(body[i+1:i+1+n], 16, 32)
			if err != nil {
				return b.String(), fmt.Errorf("\\%c: %w", c, err)
			}
			b.WriteRune(rune(r))
			i += n
		case gvariantEscapes[c] != 0:
			b.WriteRune(gvariantEscapes[c])
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), nil
}

// dbusSendCall dbus-send [--system|--session|--bus=A|--peer=A] [--dest=D]
// [--print-reply] PATH I.M [ТИП:значение…]; шина по умолчанию — сессии. Тип
// сообщения (--type=signal) не различается: сигнал с именем метода systemd
// не исполнит, лишний отказ на него безвреден
func dbusSendCall(args []word, via string) (dbusCall, bool) {
	dc := dbusCall{tool: "dbus-send", bus: "user", dest: word{literal: true}, shiftAt: -1}
	var ops []word
	for _, a := range args {
		switch t := a.text; {
		case len(ops) >= 2:
			ops = append(ops, word{text: dbusSendValue(t), literal: a.literal})
		case t == "--system":
			dc.bus = "system"
		case t == "--session":
			dc.bus = "user"
		case strings.HasPrefix(t, "--bus=") || strings.HasPrefix(t, "--peer=") || strings.HasPrefix(t, "--address="):
			dc.bus = ""
		case strings.HasPrefix(t, "--dest="):
			dc.dest = word{text: strings.TrimPrefix(t, "--dest="), literal: a.literal}
		case strings.HasPrefix(t, "-") && a.literal:
		default:
			ops = append(ops, a)
		}
	}
	dc.path, dc.member = operand(ops, 0, via), operand(ops, 1, via)
	if len(ops) > 2 {
		dc.args = ops[2:]
	}
	return dc, true
}

// dbusSendValue значение аргумента dbus-send без типа: string:x → x,
// int32:15 → 15, variant:int32:0 → 0
func dbusSendValue(text string) string {
	v, _ := strings.CutPrefix(text, "variant:")
	if _, rest, ok := strings.Cut(v, ":"); ok {
		return rest
	}
	return v
}

// qdbusCall qdbus [--system|--address A] [--literal] SERVICE PATH
// [[I.]METHOD [ARG…]]; шина по умолчанию — сессии. Без метода qdbus
// перечисляет службы, объекты или методы — не вызов
func qdbusCall(args []word, via string) (dbusCall, bool) {
	dc := dbusCall{tool: "qdbus", bus: "user", shiftAt: -1}
	var ops []word
	for i := 0; i < len(args); i++ {
		switch t := args[i].text; {
		case len(ops) >= 3:
			ops = append(ops, args[i])
		case t == "--system":
			dc.bus = "system"
		case t == "--address":
			dc.bus = ""
			i++
		case strings.HasPrefix(t, "--") && args[i].literal:
		default:
			ops = append(ops, args[i])
		}
	}
	if len(ops) < 3 && via == "" {
		return dc, false
	}
	dc.dest, dc.path, dc.member = operand(ops, 0, via), operand(ops, 1, via), operand(ops, 2, via)
	if len(ops) > 3 {
		dc.args = ops[3:]
	}
	return dc, true
}

// kquitappApps приложения kquitapp, чья служба — не org.kde.ИМЯ: процессы
// композитора KWin
var kquitappApps = map[string]string{"kwin_wayland": "org.kde.KWin", "kwin_x11": "org.kde.KWin"}

// kquitappCall kquitapp6 [--service S] [--path P] ПРИЛОЖЕНИЕ — вызов quit у
// службы org.kde.ПРИЛОЖЕНИЕ (или S) по шине сессии, то же, что qdbus
// org.kde.plasmashell /MainApplication quit. Без приложения и службы —
// не вызов (--help, --version)
func kquitappCall(args []word, via string) (dbusCall, bool) {
	dc := dbusCall{tool: "kquitapp", bus: "user", shiftAt: -1,
		path: word{text: "/MainApplication", literal: true}, member: word{text: "quit", literal: true}}
	var service, app *word
	for i := 0; i < len(args); i++ {
		t := args[i].text
		if !strings.HasPrefix(t, "-") || t == "-" {
			if app == nil {
				app = &args[i]
			}
			continue
		}
		name, value, inline := strings.Cut(strings.TrimLeft(t, "-"), "=")
		if name != "service" && name != "path" {
			continue
		}
		v := word{text: value, literal: args[i].literal}
		if !inline {
			v = optionValue(args, &i, "")
		}
		if name == "service" {
			service = &v
		} else {
			dc.path = v
		}
	}
	switch {
	case service != nil:
		dc.dest = *service
	case app != nil:
		dc.dest = word{text: "org.kde." + app.text, literal: app.literal}
		if s := kquitappApps[app.text]; s != "" {
			dc.dest.text = s
		}
	default:
		return dc, false
	}
	return dc, true
}

// dbusMethod разрушающий метод systemd: verb — команда systemctl с тем же
// действием ("" — по типу задания job); индексы аргументов (-1 — нет):
// unit — имя юнита, signal — сигнал, job — тип задания (stop, restart),
// mode — режим задания (isolate гасит юниты вне цели)
type dbusMethod struct {
	verb                    string
	unit, signal, job, mode int
}

// systemdManagerMethods методы org.freedesktop.systemd1.Manager (объект
// /org/freedesktop/systemd1), которые гасят юнит или весь менеджер или
// меняют свойства юнита (лимит памяти убьёт его процессы)
var systemdManagerMethods = map[string]dbusMethod{
	"KillUnit":               {"kill", 0, 2, -1, -1},
	"KillUnitSubgroup":       {"kill", 0, 3, -1, -1},
	"QueueSignalUnit":        {"kill", 0, 2, -1, -1},
	"StopUnit":               {"stop", 0, -1, -1, -1},
	"RestartUnit":            {"restart", 0, -1, -1, -1},
	"TryRestartUnit":         {"try-restart", 0, -1, -1, -1},
	"ReloadOrRestartUnit":    {"reload-or-restart", 0, -1, -1, -1},
	"ReloadOrTryRestartUnit": {"try-reload-or-restart", 0, -1, -1, -1},
	"FreezeUnit":             {"freeze", 0, -1, -1, -1},
	"EnqueueUnitJob":         {"", 0, -1, 1, 2},
	"StartUnit":              {"start", 0, -1, -1, 1},
	"StartUnitWithFlags":     {"start", 0, -1, -1, 1},
	"StartUnitReplace":       {"start", 1, -1, -1, 2},
	"Exit":                   {"exit", -1, -1, -1, -1},
	"Reboot":                 {"reboot", -1, -1, -1, -1},
	"SoftReboot":             {"soft-reboot", -1, -1, -1, -1},
	"PowerOff":               {"poweroff", -1, -1, -1, -1},
	"Halt":                   {"halt", -1, -1, -1, -1},
	"KExec":                  {"kexec", -1, -1, -1, -1},
	"SwitchRoot":             {"switch-root", -1, -1, -1, -1},
	"SetUnitProperties":      {"set-property", 0, -1, -1, -1},
}

// systemdUnitMethods методы org.freedesktop.systemd1.Unit (объект юнита
// /org/freedesktop/systemd1/unit/ИМЯ), которые его гасят; юнит — из пути
var systemdUnitMethods = map[string]dbusMethod{
	"Kill":               {"kill", -1, 1, -1, -1},
	"QueueSignal":        {"kill", -1, 1, -1, -1},
	"Stop":               {"stop", -1, -1, -1, -1},
	"Restart":            {"restart", -1, -1, -1, -1},
	"TryRestart":         {"try-restart", -1, -1, -1, -1},
	"ReloadOrRestart":    {"reload-or-restart", -1, -1, -1, -1},
	"ReloadOrTryRestart": {"try-reload-or-restart", -1, -1, -1, -1},
	"Freeze":             {"freeze", -1, -1, -1, -1},
	"EnqueueJob":         {"", -1, -1, 0, 1},
	"Start":              {"start", -1, -1, -1, 0},
	"SetProperties":      {"set-property", -1, -1, -1, -1},
}

// jobVerbs тип задания systemd → команда systemctl; прочие типы (reload,
// verify-active) юнит не гасят
var jobVerbs = map[string]string{"start": "start", "stop": "stop", "restart": "restart", "try-restart": "try-restart"}

// action команда systemctl, которую исполнит метод с аргументами dc: тип
// задания и режим isolate — из аргументов (start без isolate — start:
// запуск exit.target гасит менеджер); what — что из них вычисляется при
// запуске и не даёт решить
func (m dbusMethod) action(dc dbusCall, via string) (verb, what string) {
	verb = m.verb
	if m.job >= 0 {
		job := dc.arg(m.job, via)
		if !job.literal {
			return "", "тип задания"
		}
		verb = jobVerbs[job.text]
	}
	if verb == "start" && m.mode >= 0 {
		mode := dc.arg(m.mode, via)
		if !mode.literal {
			return "", "режим задания"
		}
		if mode.text == "isolate" {
			return "isolate", ""
		}
	}
	return verb, ""
}

const (
	dbusOther = iota
	dbusSystemd
	dbusLogin
	dbusUnknown
)

// dbusTarget кому вызов: systemd или logind — по имени получателя,
// интерфейсу или пути (их объекты — только под /org/freedesktop/systemd1 и
// /org/freedesktop/login1); путь, получатель по имени или интерфейс
// литералом, не их, — другой службе. Иначе получатель не виден
func dbusTarget(dc dbusCall) int {
	iface := ""
	if dc.member.literal {
		iface, _ = splitMember(dc.member.text)
	}
	mentions := func(service string) bool {
		return dc.dest.literal && strings.HasPrefix(dc.dest.text, "org.freedesktop."+service) ||
			dc.path.literal && strings.HasPrefix(dc.path.text, "/org/freedesktop/"+service) ||
			strings.HasPrefix(iface, "org.freedesktop."+service)
	}
	switch {
	case mentions("login1"):
		return dbusLogin
	case mentions("systemd1"):
		return dbusSystemd
	case dc.path.literal && dc.path.text != "",
		dc.dest.literal && dc.dest.text != "" && !strings.HasPrefix(dc.dest.text, ":"),
		iface != "":
		return dbusOther
	}
	return dbusUnknown
}

// splitMember Интерфейс.Метод → интерфейс и метод; у метода без
// интерфейса (qdbus) интерфейс пустой
func splitMember(member string) (iface, method string) {
	k := strings.LastIndexByte(member, '.')
	if k < 0 {
		return "", member
	}
	return member[:k], member[k+1:]
}

// loginEndMethod метод logind, завершающий сеанс, пользователя или место:
// Manager.KillSession, TerminateUser, Session.Kill, Seat.Terminate,
// ReleaseSession (сеанс закрывается, его процессы logind может погасить)
func loginEndMethod(method string) bool {
	return strings.HasPrefix(method, "Kill") || strings.HasPrefix(method, "Terminate") || method == "ReleaseSession"
}

// loginPowerMethods методы org.freedesktop.login1.Manager, которые гасят
// машину, — команда systemctl с тем же действием (systemctl poweroff сам
// идёт в logind). Сон (Suspend, Hibernate) systemctl не отбивает — и тут нет
var loginPowerMethods = map[string]string{
	"PowerOff": "poweroff", "PowerOffWithFlags": "poweroff", "Reboot": "reboot", "RebootWithFlags": "reboot",
	"Halt": "halt", "HaltWithFlags": "halt", "KExec": "kexec", "KExecWithFlags": "kexec",
	"SoftReboot": "soft-reboot", "SoftRebootWithFlags": "soft-reboot",
}

// loginShutdownKinds типы ScheduleShutdown, которые гасят машину; dry-* —
// только сообщение
var loginShutdownKinds = []string{"poweroff", "reboot", "halt", "kexec", "soft-reboot"}

// loginPowerMethod метод logind, который гасит машину сейчас или по расписанию
func loginPowerMethod(method string) bool {
	return loginPowerMethods[method] != "" || method == "ScheduleShutdown"
}

// loginReason отказ на вызов logind: Kill* и Terminate* — целиком, как
// loginctl; выключение и перезагрузка (PowerOff, RebootWithFlags,
// ScheduleShutdown poweroff) — как systemctl poweroff и reboot, общей
// проверкой unitActionReason
func (t *KillGuardTool) loginReason(dc dbusCall, method, via string, procs []procInfo) (reason, hint string) {
	verb := loginPowerMethods[method]
	switch {
	case loginEndMethod(method):
		return fmt.Sprintf("%s %s (как loginctl kill-* и terminate-*) %s", dc.tool, dc.member.text, loginEndEffect), ""
	case method == "ScheduleShutdown":
		kind := dc.arg(0, via)
		if !kind.literal || dc.shifted(0) {
			return dbusUnresolved(dc, "тип выключения"), dbusHint
		}
		if slices.Contains(loginShutdownKinds, kind.text) {
			verb = kind.text
		}
	}
	if verb == "" {
		return "", ""
	}
	label := fmt.Sprintf("%s %s (как systemctl %s)", dc.tool, dc.member.text, verb)
	return t.unitActionReason(label, unitAction{verb: verb}, via, procs)
}

// systemdMethod разрушающий метод systemd с таким именем у менеджера или юнита
func systemdMethod(method string) bool {
	_, manager := systemdManagerMethods[method]
	_, unit := systemdUnitMethods[method]
	return manager || unit
}

// dbusHint подсказка к отказу на вызов D-Bus, который не разобрать
const dbusHint = "Назови получателя, объект, метод и юнит литералами — или останови юнит через systemctl с юнитом литералом: systemctl --user stop юнит"

// systemdUnitPath путь объекта юнита systemd
const systemdUnitPath = "/org/freedesktop/systemd1/unit/"

// dbusReason отказ на вызов D-Bus, который гасит то же, что systemctl или
// loginctl: метод systemd приводится к действию systemctl (KillUnit → kill,
// StopUnit → stop, StartUnit … isolate → isolate) и решается общей
// проверкой unitActionReason; Kill* и Terminate* logind — отказ целиком,
// как loginctl. Метод, получатель, объект, тип задания из переменной у
// вызова, который может быть разрушающим, — отказ
func (t *KillGuardTool) dbusReason(dc dbusCall, via string, procs []procInfo) (reason, hint string) {
	if dc.bus == "capsule" {
		return "", ""
	}
	if reason := kdeReason(dc); reason != "" {
		return reason, kdeHint
	}
	target := dbusTarget(dc)
	if target == dbusOther {
		return "", ""
	}
	if !dc.member.literal {
		return dbusUnresolved(dc, "метод"), dbusHint
	}
	_, method := splitMember(dc.member.text)
	switch {
	case target == dbusLogin:
		return t.loginReason(dc, method, via, procs)
	case target == dbusUnknown && (loginEndMethod(method) || loginPowerMethod(method) || systemdMethod(method)):
		return dbusUnresolved(dc, "получатель или объект"), dbusHint
	case target != dbusSystemd:
		return "", ""
	}
	m, unit, why := systemdTarget(dc, method, via)
	switch {
	case why != "":
		return dbusUnresolved(dc, why), dbusHint
	case m == nil:
		return "", ""
	}
	return t.systemdMethodReason(dc, *m, unit, via, procs)
}

// systemdTarget разрушающий метод вызова к systemd и его юнит: у менеджера
// — из аргумента, у объекта юнита — из пути. m nil — метод не гасит юнит
// или объект не юнит; why — что вычисляется или не названо и не даёт решить
func systemdTarget(dc dbusCall, method, via string) (m *dbusMethod, unit word, why string) {
	path := dc.path.text
	switch {
	case !dc.path.literal:
		if systemdMethod(method) {
			return nil, word{}, "объект"
		}
	case path == "/org/freedesktop/systemd1":
		if mm, ok := systemdManagerMethods[method]; ok {
			return &mm, dc.arg(mm.unit, via), ""
		}
	case strings.HasPrefix(path, systemdUnitPath):
		um, ok := systemdUnitMethods[method]
		if !ok {
			return nil, word{}, ""
		}
		// без типа юнита в пути — не имя: self (юнит самой команды), ID
		// запуска (32 цифры или GUID, в любом регистре), пустое _
		name := busLabelUnescape(strings.TrimPrefix(path, systemdUnitPath))
		if !hasUnitSuffix(name) {
			return nil, word{}, "юнит объекта " + path + " (self — юнит самой команды, ID запуска — любой юнит)"
		}
		return &um, word{text: name, literal: true}, ""
	}
	return nil, word{}, ""
}

// systemdMethodReason отказ на метод m systemd с юнитом unit: действие
// systemctl, сигнал 0 и шина — как у systemctl; шина по адресу — оба менеджера
func (t *KillGuardTool) systemdMethodReason(dc dbusCall, m dbusMethod, unit word, via string, procs []procInfo) (reason, hint string) {
	verb, what := m.action(dc, via)
	switch {
	case what != "":
		return dbusUnresolved(dc, what), dbusHint
	case verb == "":
		return "", ""
	case m.unit >= 0 && dc.shifted(m.unit) || m.signal >= 0 && dc.shifted(m.signal):
		return dbusUnresolved(dc, "позиция аргументов (сигнатура с массивом, структурой или variant)"), dbusHint
	}
	a := unitAction{verb: verb, dbus: true}
	if m.unit >= 0 || unit.text != "" {
		a.units = []word{unit}
	}
	if m.signal >= 0 {
		sig := dc.arg(m.signal, via)
		a.zero = sig.literal && sig.text == "0"
	}
	users := map[string][]bool{"user": {true}, "system": {false}, "": {true, false}}[dc.bus]
	for _, user := range users {
		a.user = user
		label := fmt.Sprintf("%s %s (как %s %s)", dc.tool, dc.member.text, a.scope(), verb)
		if reason, hint := t.unitActionReason(label, a, via, procs); reason != "" {
			return reason, hint
		}
	}
	return "", ""
}

// kdeLogoutServices службы KDE, у которых методы logout* и closeSession
// завершают сеанс (org.kde.Shutdown.logout, logoutAndReboot; ksmserver
// /KSMServer logout, closeSession)
var kdeLogoutServices = []string{"org.kde.Shutdown", "org.kde.ksmserver"}

// kdeDesktopServices службы рабочего стола KDE, у которых quit и exit
// гасят их процесс: панель и рабочий стол, композитор
var kdeDesktopServices = map[string]string{
	"org.kde.plasmashell": "plasmashell (панель и рабочий стол), как systemctl --user stop plasma-plasmashell",
	"org.kde.KWin":        "KWin — композитор: с ним падает графический сеанс",
	"org.kde.kwin":        "KWin — композитор: с ним падает графический сеанс",
	"org.kde.ksmserver":   "ksmserver — менеджер сеанса KDE: с ним завершается сеанс",
}

// kdeHint подсказка к отказу на выход из сеанса KDE и остановку рабочего стола
const kdeHint = "Выход из сеанса запускает пользователь сам; панель перезапускает systemctl --user restart plasma-plasmashell"

// kdeReason вызов KDE, который завершает сеанс (logout*) или гасит
// рабочий стол (plasmashell, KWin: quit, exit); "" — не такой
func kdeReason(dc dbusCall) string {
	if !dc.dest.literal || !dc.member.literal {
		return ""
	}
	_, method := splitMember(dc.member.text)
	method = strings.ToLower(method)
	switch {
	case slices.Contains(kdeLogoutServices, dc.dest.text) && (strings.HasPrefix(method, "logout") || method == "closesession"):
		return fmt.Sprintf("%s %s к %s завершает сеанс KDE: рабочий стол, терминалы и сессии Claude в нём", dc.tool, dc.member.text, dc.dest.text)
	case kdeDesktopServices[dc.dest.text] != "" && (method == "quit" || method == "exit"):
		return fmt.Sprintf("%s %s к %s гасит %s", dc.tool, dc.member.text, dc.dest.text, kdeDesktopServices[dc.dest.text])
	}
	return ""
}

// dbusUnresolved причина отказа на вызов, где what вычисляется или не назван
func dbusUnresolved(dc dbusCall, what string) string {
	return fmt.Sprintf("%s %s: %s не виден до запуска, а вызов может остановить юнит systemd или сеанс logind — проверить его заранее нельзя", dc.tool, dc.member.text, what)
}

// busLabelUnescape имя из метки пути D-Bus (sd_bus_path_decode): _XX —
// байт в шестнадцатеричной записи (plasma_2dkwin_5fwayland_2eservice →
// plasma-kwin_wayland.service), одно _ — пустое имя
func busLabelUnescape(label string) string {
	if label == "_" {
		return ""
	}
	var b strings.Builder
	for i := 0; i < len(label); i++ {
		if label[i] == '_' && i+2 < len(label) {
			if c, err := strconv.ParseUint(label[i+1:i+3], 16, 8); err == nil {
				b.WriteByte(byte(c))
				i += 2
				continue
			}
		}
		b.WriteByte(label[i])
	}
	return b.String()
}
