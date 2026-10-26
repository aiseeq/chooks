package tools

import "testing"

func TestRewritePgrepAncestors(t *testing.T) {
	tests := []struct {
		name, command, want string
	}{
		{"цикл ожидания конца", `until ! pgrep -f 'run.sh' >/dev/null; do sleep 5; done`,
			`until ! pgrep -A -f 'run.sh' >/dev/null; do sleep 5; done`},
		{"шаблон дважды в одной команде", `pgrep -f 'x.sh' && echo up; while pgrep -f 'x.sh'; do sleep 1; done`,
			`pgrep -A -f 'x.sh' && echo up; while pgrep -A -f 'x.sh'; do sleep 1; done`},
		{"подоболочка", `n=$(pgrep -fc 'worker'); echo "$n"`, `n=$(pgrep -A -fc 'worker'); echo "$n"`},
		{"подоболочка в кавычках", `echo "живых: $(pgrep -f worker | wc -l)"`, `echo "живых: $(pgrep -A -f worker | wc -l)"`},
		{"слитые ключи", `pgrep -af 'claude --model'`, `pgrep -A -af 'claude --model'`},
		{"длинный ключ", `pgrep --full x`, `pgrep -A --full x`},
		{"ключ со значением раньше -f", `pgrep -u aisee -f worker`, `pgrep -A -u aisee -f worker`},
		{"после префиксов", `timeout 600 sudo pgrep -f x`, `timeout 600 sudo pgrep -A -f x`},
		{"после ||", `false || pgrep -f x`, `false || pgrep -A -f x`},
		{"с новой строки", "sleep 1\npgrep -f x", "sleep 1\npgrep -A -f x"},
		{"переменная-шаблон", `pgrep -f "$P"`, `pgrep -A -f "$P"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, changed := RewritePgrepAncestors(tt.command)
			if !changed || got != tt.want {
				t.Errorf("RewritePgrepAncestors(%q) = %q, %v; want %q", tt.command, got, changed, tt.want)
			}
		})
	}
}

func TestRewritePgrepAncestorsKeeps(t *testing.T) {
	commands := map[string]string{
		"без -f":                  `pgrep -x claude`,
		"уже -A":                  `pgrep -A -f x`,
		"уже --ignore-ancestors":  `pgrep --ignore-ancestors -f x`,
		"слитый -A":               `pgrep -fA x`,
		"шаблон после имени":      `pgrep x -f`,
		"в одинарных кавычках":    `ssh host 'pgrep -f x'`,
		"аргумент ssh":            `ssh host pgrep -f x`,
		"аргумент grep":           `grep -n 'pgrep -f' notes.md`,
		"текст echo":              `echo pgrep -f x`,
		"here-doc":                "cat <<EOF\npgrep -f x\nEOF",
		"pkill":                   `pkill -f x`,
		"слово с продолжением":    `pgrepx -f x`,
		"без pgrep":               `ls -la`,
		"двойные кавычки без $()": `echo "pgrep -f x"`,
	}
	for name, command := range commands {
		t.Run(name, func(t *testing.T) {
			if got, changed := RewritePgrepAncestors(command); changed || got != command {
				t.Errorf("RewritePgrepAncestors(%q) = %q, %v; want unchanged", command, got, changed)
			}
		})
	}
}

// Скрипт bash -c — тоже код: его шелл и шелл Claude Code — оба предки pgrep
func TestRewritePgrepAncestorsShellScript(t *testing.T) {
	tests := map[string]string{
		`bash -c 'until ! pgrep -f "trace.sh -p 655[23]" >/dev/null; do sleep 5; done'`:      `bash -c 'until ! pgrep -A -f "trace.sh -p 655[23]" >/dev/null; do sleep 5; done'`,
		`timeout 900 bash -c "while pgrep -f eval1.sh >/dev/null; do sleep 10; done"; cat x`: `timeout 900 bash -c "while pgrep -A -f eval1.sh >/dev/null; do sleep 10; done"; cat x`,
		`bash -euo pipefail -c 'date; pgrep -af "graft exec"'`:                               `bash -euo pipefail -c 'date; pgrep -A -af "graft exec"'`,
		`pgrep -f a; sh -c 'pgrep -f a'`:                                                     `pgrep -A -f a; sh -c 'pgrep -A -f a'`,
	}
	for command, want := range tests {
		if got, changed := RewritePgrepAncestors(command); !changed || got != want {
			t.Errorf("RewritePgrepAncestors(%q) = %q, %v; want %q", command, got, changed, want)
		}
	}
	for _, command := range []string{`ssh pc 'bash -c "pgrep -f x"'`, `echo bash -c 'pgrep -f x'`} {
		if got, changed := RewritePgrepAncestors(command); changed {
			t.Errorf("RewritePgrepAncestors(%q) = %q; want unchanged", command, got)
		}
	}
}
