package tools

// Мера мастера (rm-guard-shellcmd-wrappers): форма rm за обёрткой даёт то же
// решение, что без неё. Боец этот файл не правит.

import (
	"strings"
	"testing"
)

func TestMeasureRmGuardWrapperPairs(t *testing.T) {
	const proj = "/home/u/lab/proj"
	tool := newRmGuard(t)
	forms := []string{
		`rm -rf .`,
		`rm -rf "$NOPE/"`,
		`rm -rf "$NOPE"/*`,
		`rm -rf /home/u/lab/proj`,
		`rm -rf /home/u/lab/proj/sub`,
		`rm -f build/x.o`,
	}
	wrappers := []string{
		`sudo`, `sudo -u root`, `doas`, `exec`, `command`, `env`, `env -i`, `env -u HOME`, `noglob`,
		`nice -n 10`, `nohup`, `time`, `timeout 5`, `timeout -s 9 5`, `stdbuf -oL`,
		`setsid`, `setsid -f`, `ionice -c 3`, `unbuffer`, `unbuffer -p`, `unbuffer -noecho`,
		`unbuffer -noecho -p`, `stdbuf -oL nohup`, `FOO=1`,
		`unbuffer -nottycopy`, `unbuffer -ignore SIGINT`, `unbuffer -ign SIGINT`, `sudo --preserve-env=HOME`,
		`sudo -E`, `timeout --signal=KILL 5`, `timeout -k 1 5`, `nice --adjustment=5`, `nice -5`,
		`env -C /tmp`, `ionice -c3 -n7`, `setsid --fork`, `stdbuf -o0 -e0`, `doas -u root`,
	}
	for _, form := range forms {
		want := !validateRmWith(t, tool, form, proj).IsValid
		for _, w := range wrappers {
			command := w + " " + form
			if got := !validateRmWith(t, tool, command, proj).IsValid; got != want {
				t.Errorf("%q: отказ %v, без обёртки %v", command, got, want)
			}
		}
	}
}

// TestMeasureRmGuardWrapperChains две обёртки подряд (time sudo, nice env -u X)
// дают то же решение, что форма без них: проверка 80c45b5 нашла пропуск
// `time sudo rm -rf .`
func TestMeasureRmGuardWrapperChains(t *testing.T) {
	const proj = "/home/u/lab/proj"
	tool := newRmGuard(t)
	forms := []string{`rm -rf .`, `rm -rf "$NOPE"/*`, `rm -rf /home/u/lab/proj`, `rm -f build/x.o`}
	wrappers := []string{
		`sudo`, `sudo -u root`, `doas`, `exec`, `command`, `env`, `env -u HOME`, `env -C /tmp`, `noglob`,
		`nice -n 10`, `nohup`, `time`, `time -p`, `timeout 5`, `timeout -k 1 5`, `stdbuf -oL`,
		`setsid`, `ionice -c 3`, `unbuffer`, `FOO=1`, `builtin`,
	}
	for _, form := range forms {
		want := !validateRmWith(t, tool, form, proj).IsValid
		for _, outer := range wrappers {
			for _, inner := range wrappers {
				// `nice FOO=1 rm`: присваивание после обёртки принимают только env
				// и sudo, остальные запускают программу «FOO=1», и rm не исполняется
				if inner == `FOO=1` && !strings.HasPrefix(outer, "env") && !strings.HasPrefix(outer, "sudo") {
					continue
				}
				command := outer + " " + inner + " " + form
				if got := !validateRmWith(t, tool, command, proj).IsValid; got != want {
					t.Errorf("%q: отказ %v, без обёрток %v", command, got, want)
				}
			}
		}
	}
}
