package tools

import (
	"fmt"
	"slices"
	"strings"
)

// systemdRunValues опции systemd-run со значением (getopt systemd v259
// "+hrC:H:M:E:p:tTPqvGdSu:" и длинные с required_argument)
var systemdRunValues = []string{
	"C", "H", "M", "E", "p", "u",
	"capsule", "unit", "description", "slice", "expand-environment", "host", "machine", "service-type", "uid", "gid",
	"nice", "setenv", "property", "on-active", "on-boot", "on-startup", "on-unit-active", "on-unit-inactive",
	"on-calendar", "timer-property", "path-property", "socket-property", "working-directory", "root-directory",
	"job-mode", "background", "json",
}

// systemdRunProperties опции systemd-run, задающие свойства юнитов: службы
// (-p), таймера, пути, сокета
var systemdRunProperties = []string{"p", "property", "timer-property", "path-property", "socket-property"}

// unitEndActions действия SuccessAction= и FailureAction= (без -force,
// -immediate), которые гасят менеджер или машину, — как команда systemctl:
// выключения logind и exit
var unitEndActions = append([]string{"exit"}, loginShutdownKinds...)

// systemdRunReason systemd-run [--user] -p SuccessAction=exit-force …:
// когда временная служба (или таймер --on-*) завершится, менеджер выполнит
// действие — exit гасит менеджер пользователя, poweroff и прочие — машину,
// как systemctl --user exit и systemctl poweroff. Опции systemd-run
// кончаются на первом слове команды ("+" в getopt)
func systemdRunReason(args []word) (reason, hint string) {
	var a unitAction
	var props []word
	for i := 0; i < len(args); i++ {
		t := args[i].text
		if t == "--" || !strings.HasPrefix(t, "-") || t == "-" {
			break
		}
		name, value := t[2:], word{}
		if !strings.HasPrefix(t, "--") {
			var rest string
			name, rest, _ = shortValue(t, systemdRunValues)
			if name == "" {
				continue
			}
			value = optionValue(args, &i, rest)
		} else if n, v, inline := strings.Cut(name, "="); inline {
			name, value = n, word{text: v, literal: args[i].literal}
		} else if slices.Contains(systemdRunValues, name) {
			value = optionValue(args, &i, "")
		}
		a.user = a.user && name != "system" || name == "user"
		if slices.Contains(systemdRunProperties, name) {
			props = append(props, value)
		}
	}
	scope := a.scope()
	for _, p := range props {
		key, action, _ := strings.Cut(p.text, "=")
		end := strings.TrimSuffix(strings.TrimSuffix(action, "-force"), "-immediate")
		if !p.literal || key != "SuccessAction" && key != "FailureAction" || !slices.Contains(unitEndActions, end) {
			continue
		}
		label := strings.Replace(scope, "systemctl", "systemd-run", 1)
		return fmt.Sprintf("%s -p %s: когда служба завершится, менеджер выполнит %s — %s: гаснут сессии Claude, терминал и графическая оболочка", label, p.text, action, systemctlEndEffect(scope, end)), systemctlEndHint(scope, end)
	}
	return "", ""
}
