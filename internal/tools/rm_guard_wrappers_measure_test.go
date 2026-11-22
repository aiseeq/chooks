package tools

// Мера мастера (rm-guard-shellcmd-wrappers): форма rm за обёрткой даёт то же
// решение, что без неё. Боец этот файл не правит.

import "testing"

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
		`sudo`, `sudo -u root`, `doas`, `exec`, `command`, `env`, `env -i`, `env -u HOME`,
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
