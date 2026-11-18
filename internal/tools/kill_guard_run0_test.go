package tools

import "testing"

// Тикет kill-guard-run0-signal: run0 запускает команду временной службой
// менеджера, а не потомком агента — сигнал под run0 ядро не сверяет: отказ.
// sudo и pkexec делают exec в своём процессе — как было
func TestKillGuardRun0Signal(t *testing.T) {
	for _, command := range []string{
		`run0 kill 12345`,
		`run0 pkill opencode`,
		`run0 --user=aisee killall sleep`,
		`run0 systemd-run --scope kill 12345`,
		`run0 sh -c 'kill 12345'`,
		`nice run0 /bin/kill -TERM 12345`,
		// ввод оболочки за run0 — её скрипт
		"run0 bash <<EOF\nkill 12345\nEOF",
		"run0 bash <<EOF\nkill -9 -1\nEOF",
		`run0 bash <<< 'kill 12345'`,
		"run0 sh -s <<EOF\nkill 12345\nEOF",
		"run0 systemd-run --scope bash <<EOF\nkill 12345\nEOF",
		// тело с ошибкой разбора: bash исполнит kill до неё
		"run0 bash <<EOF\nkill 1234\nfi\nEOF",
		"run0 bash <<EOF\nkill -9 -1\n)\nEOF",
		// ввод из подстановки не виден
		`run0 bash < <(echo kill -9 -1)`,
		`echo kill 12345 | run0 bash`,
		// ввод задан блоку или циклу вокруг run0 bash
		"{ read x; run0 bash; } <<EOF\nx\nkill 1234\nEOF",
		"{ read x; run0 bash; } <<-EOF\n\tx\n\tkill 1234\n\tEOF",
		"while read x; do run0 bash; done <<EOF\nx\nkill 1234\nEOF",
		`while read x; do run0 bash; done < <(printf 'x\nkill 1234\n')`,
		`printf 'x\nkill 1234\n' | { read x; run0 bash; }`,
		`cat kill-list | xargs run0 bash`,
		// функция, exec <, xargs с heredoc, вложенный heredoc
		"f() { run0 bash; }; f <<EOF\nkill 1234\nEOF",
		`f() { run0 bash; }; f <<< 'kill 1234'`,
		`f() { run0 bash; }; echo 'kill 1234' | f`,
		`exec < <(echo kill 1234); run0 bash`,
		"exec <<EOF\nkill 1234\nEOF\nrun0 bash",
		"{ xargs run0 bash -c; } <<EOF\n'kill 1234'\nEOF",
		"xargs run0 bash -c <<EOF\n'kill 1234'\nEOF",
		"bash <<EOF\n{ read x; run0 bash; } <<IN\nx\nkill 1234\nIN\nEOF",
		// цена правила: скрипт с ввода за run0 не разбирается, kill в
		// тексте — отказ, даже когда сам скрипт литеральный
		`echo make | run0 bash; echo skill`,
		`run0 sh -c 'echo kill 1234 | bash'`,
	} {
		if validateKill(t, command).IsValid {
			t.Errorf("должно блокироваться: %s", command)
		}
	}
	for _, command := range []string{
		`sudo kill 12345`,
		`pkexec kill 12345`,
		`run0 systemctl --user status x.service`,
		`systemd-run --user --scope kill 12345`,
		"{ read x; run0 bash; } <<EOF\nx\nmake\nEOF",
		`while read x; do run0 bash; done < <(printf 'x\nmake\n')`,
		// без kill в тексте сторож команду не разбирает: ошибка разбора
		// скрипта на вводе — не отказ
		"systemd-run --user bash <<EOF\nmake\nfi\nEOF",
		"systemd-run --user --scope bash <<EOF\nmake\nfi\nEOF",
		`systemd-run --user --pipe bash <<< 'echo (('`,
		`systemd-run --user --pipe bash <<< 'make; fi'`,
		"run0 bash <<EOF\nmake\nfi\nEOF",
		`systemd-run --user bash < <(echo make)`,
		`systemd-run --user --scope sh < <(echo make)`,
		`{ read x; systemd-run --user --pipe bash; } < <(printf 'x\nmake\n')`,
		// скрипт аргументом и файлом за run0 — не с ввода
		`run0 bash -c 'make'; echo skill`,
		`run0 bash ./build.sh; echo skill`,
		// скрипт с ввода прочитан разбором
		`run0 sh -c 'echo make | bash'; echo skill`,
		// без run0 скрипт с ввода — потомок агента, ядро его сверяет
		`bash < ./build.sh; echo skill`,
		"f() { sudo bash; }; f <<EOF\nmake\nEOF\necho skill",
		// без kill в тексте скрипт с ввода — не отказ
		`echo make | run0 bash; systemctl --user status x.service`,
	} {
		if r := validateKill(t, command); !r.IsValid {
			t.Errorf("не должно блокироваться: %s: %v", command, r.Suggestions)
		}
	}
}
