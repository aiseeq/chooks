package tools

import (
	"slices"
	"strings"
	"testing"
)

// Тексты отказа systemctl без юнита (тикет kill-guard-hint-text) — по man
// systemctl: isolate, default, rescue и emergency переключают цель и гасят
// юниты вне неё; poweroff, halt, reboot, kexec, soft-reboot и exit (вне
// контейнера — как poweroff) гасят машину; switch-root меняет корень и
// системный менеджер. С --user это те же действия системного менеджера
// (systemctl может уйти в logind), кроме exit: он гасит менеджер --user.
// Отказ и подсказка говорят одно и то же
func TestKillGuardEndTexts(t *testing.T) {
	const (
		tail     = ": гаснут сессии Claude, терминал и графическая оболочка"
		sysHint  = ": такое запускает пользователь сам. Остановить один юнит, который ты запустил сам и где нет процессов сессии, — назови его литералом: systemctl stop юнит; чужое не трогай, сообщи в ответе"
		userHint = ": такое запускает пользователь сам. Остановить один юнит, который ты запустил сам и где нет процессов сессии, — назови его литералом: systemctl --user stop юнит; чужое не трогай, сообщи в ответе"
	)
	for _, tc := range []struct{ command, effect string }{
		{`sudo systemctl isolate rescue.target`, "переключает цель системы и гасит юниты вне неё"},
		{`sudo systemctl emergency`, "переключает цель системы и гасит юниты вне неё"},
		{`sudo systemctl rescue`, "переключает цель системы и гасит юниты вне неё"},
		{`sudo systemctl default`, "переключает цель системы и гасит юниты вне неё"},
		{`systemctl --user isolate default.target`, "переключает цель менеджера systemctl --user и гасит юниты вне неё"},
		{`systemctl --user default`, "переключает цель менеджера systemctl --user и гасит юниты вне неё"},
		{`systemctl --user rescue`, "переключает цель менеджера systemctl --user и гасит юниты вне неё"},
		{`systemctl --user emergency`, "переключает цель менеджера systemctl --user и гасит юниты вне неё"},
		{`sudo systemctl poweroff`, "выключает машину"},
		{`sudo systemctl exit`, "выключает машину"},
		{`sudo systemctl halt`, "останавливает машину"},
		{`systemctl reboot`, "перезагружает машину"},
		{`sudo systemctl kexec`, "перезагружает машину"},
		{`sudo systemctl soft-reboot`, "перезапускает систему без перезагрузки ядра"},
		{`sudo systemctl switch-root /sysroot`, "меняет корень системы и запускает на нём новый системный менеджер"},
		{`systemctl --user exit`, "гасит менеджер systemctl --user"},
		{`systemctl --user poweroff`, "выключает машину"},
		{`systemctl --user halt`, "останавливает машину"},
		{`systemctl --user reboot`, "перезагружает машину"},
		{`systemctl --user kexec`, "перезагружает машину"},
		{`systemctl --user soft-reboot`, "перезапускает систему без перезагрузки ядра"},
		{`systemctl --user switch-root /sysroot`, "меняет корень системы и запускает на нём новый системный менеджер"},
	} {
		scope, verb := "systemctl", ""
		hint := sysHint
		fields := strings.Fields(strings.TrimPrefix(tc.command, "sudo "))
		if fields[1] == "--user" {
			scope, hint = "systemctl --user", userHint
			verb = fields[2]
		} else {
			verb = fields[1]
		}
		res := validateKill(t, tc.command)
		if res.IsValid {
			t.Errorf("%s: ждали отказ", tc.command)
			continue
		}
		reason := scope + " " + verb + " " + tc.effect + tail
		hint = "Команда " + tc.effect + hint
		if !slices.Contains(res.Suggestions, reason) || !slices.Contains(res.Suggestions, hint) {
			t.Errorf("%s: ждали отказ %q и подсказку %q, получили %q", tc.command, reason, hint, res.Suggestions)
		}
	}
	// Ни одна команда не остаётся без текста: у каждой из systemctlEndVerbs
	// при обоих менеджерах фраза не пустая; «гасит менеджер» — только у
	// systemctl --user exit
	for _, verb := range systemctlEndVerbs {
		for _, scope := range []string{"systemctl", "systemctl --user"} {
			e := systemctlEndEffect(scope, verb)
			manager := strings.Contains(e, "гасит менеджер")
			if e == "" || manager != (verb == "exit" && scope == "systemctl --user") {
				t.Errorf("%s %s: фраза %q", scope, verb, e)
			}
		}
	}
}
