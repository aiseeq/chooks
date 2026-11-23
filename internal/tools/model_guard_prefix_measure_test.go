package tools

// Мера мастера (model-guard-detached-launch, круг 2): CLAUDE_CODE_SHELL_PREFIX
// не путём к исполняемому файлу гасит все хуки (127). Боец этот файл не правит.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiseeq/chooks/internal/core"
)

func TestMeasureShellPrefix(t *testing.T) {
	tool := modelGuardForTest(t, "opus claude-opus-5-5\n")
	home := t.TempDir()
	t.Setenv("HOME", home)
	exe := filepath.Join(home, "bin", "prefix")
	if err := os.MkdirAll(filepath.Dir(exe), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nexec \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(home, "plain")
	if err := os.WriteFile(plain, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	const bad = `CLAUDE_CODE_SHELL_PREFIX='timeout 60'`
	for _, tc := range []struct {
		command string
		allowed bool
	}{
		{bad + " bash launch.sh", false},
		{"export " + bad, false},
		{"env " + bad + " bash launch.sh", false},
		{"declare -x " + bad, false},
		{"typeset -x " + bad, false},
		{"readonly " + bad, false},
		{"env -i HOME=/home/x PATH=/usr/bin " + bad + " bash launch.sh", false},
		{"env -u FOO " + bad + " bash launch.sh", false},
		{`env HOME="$HOME" ` + bad + " bash launch.sh", false},
		{"/usr/bin/env " + bad + " bash launch.sh", false},
		{"env -- " + bad + " bash launch.sh", false},
		{"setsid -f env " + bad + " bash launch.sh", false},
		{"nohup env " + bad + " bash launch.sh &", false},
		{`bash -c "` + bad + ` bash launch.sh"`, false},
		{"CLAUDE_CODE_SHELL_PREFIX=" + plain + " bash launch.sh", false},
		{"CLAUDE_CODE_SHELL_PREFIX=nice bash launch.sh", false},
		{"CLAUDE_CODE_SHELL_PREFIX=" + exe + " bash launch.sh", true},
		{"env -i HOME=/home/x CLAUDE_CODE_SHELL_PREFIX=" + exe + " bash launch.sh", true},
		{"CLAUDE_CODE_SHELL_PREFIX=~/bin/prefix bash launch.sh", true},
		{"CLAUDE_CODE_SHELL_PREFIX= bash launch.sh", true},
		{"export CLAUDE_CODE_SHELL_PREFIX=", true},
		{"export CLAUDE_CODE_SHELL_PREFIX", true},
		{"unset CLAUDE_CODE_SHELL_PREFIX", true},
		{"echo " + bad, true},
		{"env CLAUDE_CODE_SHELL_PREFIX= bash launch.sh", true},
		// строка оболочки за обёрткой (ревью круга 4; круги 2-3 ловили)
		{`setsid -f bash -c "CLAUDE_CODE_SHELL_PREFIX=timeout bash launch.sh"`, false},
		{`env -i bash -c 'CLAUDE_CODE_SHELL_PREFIX=timeout bash launch.sh'`, false},
		{`timeout 5 bash -c "env CLAUDE_CODE_SHELL_PREFIX=timeout bash launch.sh"`, false},
		{`timeout 5 bash -c "CLAUDE_CODE_SHELL_PREFIX=timeout bash launch.sh"`, false},
		{`nohup sh -c 'export CLAUDE_CODE_SHELL_PREFIX=timeout; bash launch.sh' &`, false},
		{`script -qc "env CLAUDE_CODE_SHELL_PREFIX=timeout bash launch.sh" /dev/null`, false},
		{`script -qc "CLAUDE_CODE_SHELL_PREFIX=timeout bash launch.sh" /dev/null`, false},
		// -E у обёрнутой команды systemd-run — не присваивание
		{"systemd-run --user --wait grep -E CLAUDE_CODE_SHELL_PREFIX=timeout /tmp/x", true},
		// неразобранный хвост не прячет присваивание: bash исполнит начало
		{"env " + bad + " bash launch.sh <<EOF", false},
		{"env " + bad + " bash launch.sh; echo $((", false},
		{"echo ok <<EOF", true},
		// env за обёртками, которые его прячут от поиска по имени вызова
		{"exec env " + bad + " bash launch.sh", false},
		{"exec env -i HOME=/home/x " + bad + " bash launch.sh", false},
		{"command env " + bad + " bash launch.sh", false},
		{"stdbuf -oL env " + bad + " bash launch.sh", false},
		{"systemd-run --user --setenv=CLAUDE_CODE_SHELL_PREFIX=timeout bash launch.sh", false},
		// NAME= в аргументах обёрнутой команды — не присваивание
		{"nohup grep -rn CLAUDE_CODE_SHELL_PREFIX=timeout docs/ &", true},
		{"timeout 10 grep -rn CLAUDE_CODE_SHELL_PREFIX=timeout docs/", true},
		{"timeout 10 echo CLAUDE_CODE_SHELL_PREFIX=timeout", true},
		{"sudo -u aisee " + bad + " bash launch.sh", false},
		// присваивание только в тексте: кавычки, сообщение коммита, here-doc, текст промпта
		{`echo "env ` + bad + ` bash launch.sh"`, true},
		{`git commit -m "fix: отказ на env CLAUDE_CODE_SHELL_PREFIX=timeout"`, true},
		{`grep -rn "env ` + bad + `" docs/`, true},
		{`tl --grep 'env -i CLAUDE_CODE_SHELL_PREFIX=timeout'`, true},
		{"cat > note.md <<'EOF'\nenv " + bad + " bash launch.sh\nEOF", true},
		{`claude --model opus -p 'разбор: env CLAUDE_CODE_SHELL_PREFIX=timeout'`, true},
		// HOME и CLAUDE_CONFIG_DIR в тексте промпта — не запуск
		{`claude --model opus -p 'проверь: env CLAUDE_CONFIG_DIR=$DIR claude'`, true},
		{`claude --model opus -p 'проверь: env HOME=$X claude'`, true},
		// круг 6, ревью r5: systemd-run — нелитеральная опция, опции с значением, -t без значения
		{`systemd-run --user --unit="agent-$N" -E CLAUDE_CODE_SHELL_PREFIX=timeout bash launch.sh`, false},
		{`systemd-run --user --working-directory="$PWD" -E CLAUDE_CODE_SHELL_PREFIX=timeout bash launch.sh`, false},
		{`systemd-run --user --nice 5 -E CLAUDE_CODE_SHELL_PREFIX=timeout bash launch.sh`, false},
		{`systemd-run --user --on-active 30 -E CLAUDE_CODE_SHELL_PREFIX=timeout bash launch.sh`, false},
		{`systemd-run --user -t -E CLAUDE_CODE_SHELL_PREFIX=timeout bash launch.sh`, false},
		{`systemd-run --user -t grep -E CLAUDE_CODE_SHELL_PREFIX=timeout f`, true},
		// круг 6, ревью r5: ошибка разбора соседнего скрипта не снимает проверку
		{`setsid -f bash -c "CLAUDE_CODE_SHELL_PREFIX=timeout bash launch.sh"; bash -c 'echo $(('`, false},
		{`script -qc "CLAUDE_CODE_SHELL_PREFIX=timeout bash launch.sh" /dev/null; sh -c 'if'`, false},
		{`timeout 5 bash -c "env CLAUDE_CODE_SHELL_PREFIX=timeout bash launch.sh; echo \$(("`, false},
		// круг 6, ревью r5: флаги оболочки, склеенные с -c, и -- после -c за обёрткой
		{`timeout 5 bash -lc "CLAUDE_CODE_SHELL_PREFIX=timeout bash launch.sh"`, false},
		{`timeout 5 bash -ec "CLAUDE_CODE_SHELL_PREFIX=timeout bash launch.sh"`, false},
		{`setsid -f bash -c -- "CLAUDE_CODE_SHELL_PREFIX=timeout bash launch.sh"`, false},
		{`setsid -f bash -c "timeout 5 bash -lc 'CLAUDE_CODE_SHELL_PREFIX=timeout bash launch.sh'"`, false},
		{`timeout 5 bash -lc "echo CLAUDE_CODE_SHELL_PREFIX=timeout"`, true},
		// круг 6, ревью r5: script — другие склейки и --command
		{`script -eqc "CLAUDE_CODE_SHELL_PREFIX=timeout bash launch.sh" /dev/null`, false},
		{`script -fqc "CLAUDE_CODE_SHELL_PREFIX=timeout bash launch.sh" /dev/null`, false},
		{`script --command "CLAUDE_CODE_SHELL_PREFIX=timeout bash launch.sh" /dev/null`, false},
		// круг 7, ревью r6: опция systemd-run со значением через = не берёт следующее слово
		{`systemd-run --user --on-active=30 -E CLAUDE_CODE_SHELL_PREFIX=timeout bash launch.sh`, false},
		{`systemd-run --user --on-calendar=daily -E CLAUDE_CODE_SHELL_PREFIX=timeout bash launch.sh`, false},
		{`systemd-run --user --on-active=5m --setenv=CLAUDE_CODE_SHELL_PREFIX=timeout bash launch.sh`, false},
		{`systemd-run --user --on-active=30 grep -E CLAUDE_CODE_SHELL_PREFIX=timeout f`, true},
		// круг 7, ревью r6: -C (--capsule) со значением, склейка коротких флагов, script --command=
		{`systemd-run --user -C cap -E CLAUDE_CODE_SHELL_PREFIX=timeout bash launch.sh`, false},
		{`systemd-run --user -qtE CLAUDE_CODE_SHELL_PREFIX=timeout bash launch.sh`, false},
		{`script --command="CLAUDE_CODE_SHELL_PREFIX=timeout bash launch.sh" /dev/null`, false},
		// круг 7, ревью r6: сломанный вложенный скрипт с именем — «не проверить»
		{`timeout 5 bash -c 'grep -E CLAUDE_CODE_SHELL_PREFIX=x f; echo $(('`, false},
		// круг 7, ревью r6: после имени скрипта аргументы позиционные
		{`timeout 5 bash ./check.sh -c "CLAUDE_CODE_SHELL_PREFIX=timeout"`, true},
		// круг 8, ревью r7: опции оболочки со значением и флаги с + за обёрткой
		{`timeout 5 bash -o pipefail -c "CLAUDE_CODE_SHELL_PREFIX=timeout bash launch.sh"`, false},
		{`setsid -f bash -o pipefail -c "CLAUDE_CODE_SHELL_PREFIX=timeout bash launch.sh"`, false},
		{`timeout 5 bash -euo pipefail -c "CLAUDE_CODE_SHELL_PREFIX=timeout bash launch.sh"`, false},
		{`timeout 5 bash -O extglob -c "CLAUDE_CODE_SHELL_PREFIX=timeout bash launch.sh"`, false},
		{`timeout 5 bash +O extglob -c "CLAUDE_CODE_SHELL_PREFIX=timeout bash launch.sh"`, false},
		{`timeout 5 bash +x -c "CLAUDE_CODE_SHELL_PREFIX=timeout bash launch.sh"`, false},
		{`timeout 5 bash --rcfile ./rc -c "CLAUDE_CODE_SHELL_PREFIX=timeout bash launch.sh"`, false},
		{`timeout 5 zsh -o errexit -c "CLAUDE_CODE_SHELL_PREFIX=timeout bash launch.sh"`, false},
		{`timeout 5 bash - -c "CLAUDE_CODE_SHELL_PREFIX=timeout"`, true},
		{`timeout 5 bash -o pipefail ./check.sh -c "CLAUDE_CODE_SHELL_PREFIX=timeout"`, true},
		// круг 8, ревью r7: script разбирает опции и после имени файла; опции script со значением
		{`script -q /tmp/log -c "CLAUDE_CODE_SHELL_PREFIX=timeout bash launch.sh"`, false},
		{`script -E never -c "CLAUDE_CODE_SHELL_PREFIX=timeout bash launch.sh" /dev/null`, false},
		{`script -O /tmp/o -c "CLAUDE_CODE_SHELL_PREFIX=timeout bash launch.sh" /dev/null`, false},
		{`script -m advanced -T /tmp/t -c "CLAUDE_CODE_SHELL_PREFIX=timeout bash launch.sh" /dev/null`, false},
		{`script --echo never -c "CLAUDE_CODE_SHELL_PREFIX=timeout bash launch.sh" /dev/null`, false},
	} {
		t.Run(tc.command, func(t *testing.T) {
			result, err := tool.ValidateTool(gitGuardTestContext(t), &core.ToolInput{ToolName: "Bash", Command: tc.command})
			if err != nil {
				t.Fatal(err)
			}
			if result.IsValid != tc.allowed {
				t.Fatalf("allowed=%v, want %v; violations=%v", result.IsValid, tc.allowed, result.Violations)
			}
			// отказ по префиксу — без подсказки про список моделей
			if !tc.allowed && strings.Contains(strings.Join(result.Suggestions, "\n"), "models/approved") {
				t.Fatalf("подсказка про список моделей к отказу по префиксу: %q", result.Suggestions)
			}
		})
	}
}
