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
	options := parseSystemdRunOptions(args)
	scope := (unitAction{user: options.user}).scope()
	for _, p := range options.properties {
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

// systemdRunOptions опции systemd-run, от которых зависит проверка: user —
// менеджер пользователя, scope — --scope (команда остаётся потомком
// агента), shell — --shell/-S (оболочка с ввода вместо команды: слово
// shellWord, буква shellLetter в связке, -1 — длинная опция), remote —
// команда исполнится на другой машине (последняя из -H, -M, -C), start —
// место первого слова команды, properties — значения -p и --*-property
type systemdRunOptions struct {
	user, scope, shell, remote, expandEnvironmentNo bool
	start                                           int
	shellWord, shellLetter                          int
	properties                                      []word
}

// systemdRunShortNames короткие опции systemd-run (getopt systemd v259
// "+hrC:H:M:E:p:tTPqvGdSu:") и их длинные имена. -R (--same-root-dir) в
// systemd-run --help есть, а в getopt нет: systemd-run -R отвечает invalid
// option и ничего не запускает
var systemdRunShortNames = map[string]string{
	"h": "help", "r": "remain-after-exit", "C": "capsule", "H": "host", "M": "machine", "E": "setenv",
	"p": "property", "t": "pty", "T": "pty-late", "P": "pipe", "q": "quiet", "v": "verbose",
	"G": "collect", "d": "same-dir", "S": "shell", "u": "unit",
}

// parseSystemdRunOptions разбирает все интересующие опции одним проходом,
// как getopt_long: длинные имена допускают однозначные сокращения, короткие
// флаги склеиваются, а опция со значением завершает связку.
func parseSystemdRunOptions(args []word) systemdRunOptions {
	var out systemdRunOptions
	for i := 0; i < len(args); i++ {
		t := args[i].text
		if t == "--" {
			out.start = i + 1
			return out
		}
		if !strings.HasPrefix(t, "-") || t == "-" {
			out.start = i
			return out
		}
		if strings.HasPrefix(t, "--") {
			raw, rest, inline := strings.Cut(t[2:], "=")
			name := systemdRunLongName(raw)
			if name == "" {
				continue
			}
			value := word{text: rest, literal: args[i].literal}
			if slices.Contains(systemdRunValues, name) && !inline {
				value = optionValue(args, &i, "")
			}
			applySystemdRunOption(&out, name, value)
			if name == "shell" {
				out.shellWord, out.shellLetter = i, -1
			}
			continue
		}
		for j := 1; j < len(t); j++ {
			letter := t[j : j+1]
			name := systemdRunShortNames[letter]
			if name == "" {
				continue
			}
			value := word{text: "", literal: args[i].literal}
			if slices.Contains(systemdRunValues, letter) {
				rest := t[j+1:]
				if rest == "" {
					value = optionValue(args, &i, "")
				} else {
					value.text = rest
					j = len(t)
				}
			}
			applySystemdRunOption(&out, name, value)
			if name == "shell" {
				out.shellWord, out.shellLetter = i, j
			}
			if slices.Contains(systemdRunValues, letter) {
				break
			}
		}
	}
	out.start = len(args)
	return out
}

func applySystemdRunOption(out *systemdRunOptions, name string, value word) {
	switch name {
	case "user":
		out.user = true
	case "system":
		out.user = false
	case "scope":
		out.scope = true
	case "shell":
		out.shell = true
	case "host":
		out.remote = true
	case "machine":
		out.remote = !strings.ContainsAny(value.text, "$`\"'\\") && !localMachine(value.text)
	case "capsule":
		out.remote = false
	case "expand-environment":
		out.expandEnvironmentNo = systemdFalse(value.text)
	}
	if slices.Contains(systemdRunProperties, name) {
		out.properties = append(out.properties, value)
	}
}

// systemdFalse — значение ложно по parse_boolean systemd (src/basic/parse-util.c):
// 0, no, n, false, f, off без учёта регистра. Истинное и неразборное — нет:
// с неразборным systemd-run выходит с ошибкой
func systemdFalse(value string) bool {
	switch strings.ToLower(value) {
	case "0", "no", "n", "false", "f", "off":
		return true
	}
	return false
}
