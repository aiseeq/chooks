package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiseeq/chooks/internal/core"
	"github.com/aiseeq/chooks/internal/hookctx"
)

// rmGuardEnv окружение, в котором снимались вердикты Claude Code: HOME и PWD
// заданы, остальные переменные из таблицы — нет
var rmGuardEnv = map[string]string{"HOME": "/home/u", "PWD": "/home/u/proj"}

func newRmGuard(t *testing.T) *RmGuardTool {
	t.Helper()
	tool, err := NewRmGuardTool(core.ToolConfig{Enabled: true}, testLogger(t))
	if err != nil {
		t.Fatalf("NewRmGuardTool: %v", err)
	}
	tool.lookupEnv = func(name string) (string, bool) {
		v, ok := rmGuardEnv[name]
		return v, ok
	}
	return tool
}

func TestRmGuard330CdShapeAndVariables(t *testing.T) {
	for _, tc := range []struct {
		command string
		deny    bool
	}{
		{`cd ./build other && rm -f cache/*`, true},
		{`cd ./work extra && rmdir out/*`, true},
		{`cd -- ./build && rm -f cache/*`, true},
		{`cd -P ./work && rm -f out/*`, true},
		{`cd ./build >/dev/null && rm -f cache/*`, true},
		{`cd ./work 2>&1 && rmdir out/*`, true},
		{`K=v cd ./build && rm -f cache/*`, true},
		{`K=v cd ./work && rmdir out/*`, true},
		{`K=v; cd ./build && rm -f cache/*`, true},
		{`K=v; cd ./work && rmdir out/*`, true},
		{`cd ./build && rm -f cache/* | cat`, true},
		{`cd ./work && rmdir out/* | cat`, true},
		{`(cd ./build && rm -f cache/*)`, true},
		{`cd ./work && (rmdir out/*)`, true},
		{`cd /home/u/proj/build; rm -f cache/*`, true},
		{`cd /home/u/proj/work; rmdir out/*`, true},
		{`cd /home/u/proj; rm -f cache/*`, false},
		{`cd "./build" && rm -f cache/*`, false},
		{`cd ./build/ && rm -f cache/*`, false},
		{`cd ./a && cd ./b && rm -f out/*`, false},
		{`cd ./a && rm -f out/* && cd ./b`, false},
		{`false && rm -f ~/*`, true},
		{`false && rm -rf ~/*/`, true},
		{`false && rm -f ~/Documents/*`, false},
		{`false && rm -f ~/Downloads/*`, false},
		{`A=foo true; rm -f ${A%z}/*.log`, true},
		{`B=bar echo hi; rm -f ${B:+x}/*.log`, true},
		{`A=; rm -f ${A%z}/*.log`, true},
		{`B=''; rm -f ${B:+x}/*.log`, true},
		{`A=foo; rm -f ${A%z}/*.log`, false},
		{`cd ./build extra && cd ./next && rm -f cache/*`, true},
		{`cd ./work other && cd ./next && rmdir out/*`, true},
		{`K=v cd ./build && cd ./next && rm -f cache/*`, true},
		{`K=v cd ./work && cd ./next && rmdir out/*`, true},
		{`cd ./build >/dev/null && cd ./next && rm -f cache/*`, false},
		{`cd ./work 2>/dev/null && cd ./next && rmdir out/*`, false},
		{`A=/; false && rm -f ${A%z}/*.log`, true},
		{`B=//; false && rm -f ${B#x}/*.log`, true},
		{`A=foo; unset A; false && rm -f ${A%z}/*.log`, true},
		{`B=bar; unset B; false && rm -f ${B#x}/*.log`, true},
		{`A=/; A=foo; false && rm -f ${A%z}/*.log`, false},
		{`B=bar; unset B; B=next; false && rm -f ${B#x}/*.log`, false},
	} {
		r := validateRm(t, tc.command)
		if r.IsValid == tc.deny {
			t.Errorf("deny=%v, ожидалось %v: %q %v", !r.IsValid, tc.deny, tc.command, r.Suggestions)
		}
	}
	t.Run("PreToolUse", testRmGuardSubsequentCdHook)
}

func TestRmGuardNoglobCdRelativeGlob(t *testing.T) {
	for _, command := range []string{
		`noglob cd /tmp && rm -f dir/*`,
		`zsh -c 'noglob cd /tmp && rm -f dir/*'`,
		`zsh -c 'cd /tmp && noglob rm -f dir/*'`,
		`zsh -c 'zsh -c "noglob cd /tmp && rm -f dir/*"'`,
	} {
		t.Run(command, func(t *testing.T) {
			result := validateRmWith(t, newRmGuard(t), command, "/home/u/proj")
			if result.IsValid {
				t.Fatalf("ValidateTool разрешил относительный глоб после noglob cd: %q", command)
			}
		})
	}
}

func TestRmGuardWrappedCdDoesNotUseClaude330Exception(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "build"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{
		`builtin cd . && rm -f dir/*`,
		`command cd . && rm -f dir/*`,
		`builtin cd ./build && rm -f cache/*`,
		`command cd ./build && rm -f cache/*`,
		`\cd . && rm -f dir/*`,
		`/bin/cd . && rm -f dir/*`,
		`/usr/bin/cd ./build && rm -f cache/*`,
		`CD . && rm -f dir/*`,
		`time cd . && rm -f dir/*`,
		`! cd ./build && rm -f cache/*`,
		`time -- cd . && rm -f dir/*`,
		`time -p -- cd ./build && rm -f cache/*`,
		`zsh -c 'time -- cd . && rm -f dir/*'`,
	} {
		t.Run(command, func(t *testing.T) {
			if result := validateRmWith(t, newRmGuard(t), command, root); result.IsValid {
				t.Fatalf("обёрнутый cd должен быть отклонён: %q", command)
			}
		})
	}
}

func TestWrappedCommandWordsPreservesWordsOnShellcmdError(t *testing.T) {
	parsed := &shScript{}
	parseShell(`fish -c 'if'`, false, parsed)
	if len(parsed.cmds) != 1 {
		t.Fatalf("parseShell commands = %d, want 1", len(parsed.cmds))
	}
	words := parsed.cmds[0].words
	got := wrappedCommandWords(words, 0, false)
	if len(got) != len(words) {
		t.Fatalf("wrappedCommandWords length = %d, want original %d", len(got), len(words))
	}
	for i := range words {
		if got[i] != words[i] {
			t.Errorf("word %d = %#v, want original %#v", i, got[i], words[i])
		}
	}
}

// testRmGuardSubsequentCdHook проверяет выход процесса и текст, который
// PreToolUse передаёт вызывающей стороне при блокировке.
func testRmGuardSubsequentCdHook(t *testing.T) {
	dir, err := os.MkdirTemp(".", ".rm-guard-hook-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("удаление временного каталога хука: %v", err)
		}
	})
	dir, err = filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "chooks")
	build := exec.Command("go", "build", "-o", binary, "../../cmd/chooks")
	build.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("сборка хука: %v\n%s", err, out)
	}
	config := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(config, []byte("logger:\n  level: error\ntools:\n  rm_guard:\n    enabled: true\n  git_guard:\n    enabled: false\n  rsync_guard:\n    enabled: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cwd := filepath.Join(dir, "work")
	for _, p := range []string{"build/next", "work/nested"} {
		if err := os.MkdirAll(filepath.Join(cwd, p), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		command string
		target  string
		deny    bool
	}{
		{`cd ./build && cd ./next extra && rm -f cache/*`, "cache/*", true},
		{`cd ./work && cd ./nested other && rmdir out/*`, "out/*", true},
		{`cd ./build && K=v cd ./next && rm -f cache/*`, "cache/*", true},
		{`cd ./work && X=y cd ./nested && rmdir out/*`, "out/*", true},
		{`cd ./build && cd ./next && rm -f cache/*`, "", false},
	} {
		t.Run(tc.command, func(t *testing.T) {
			input, err := json.Marshal(map[string]any{
				"hook_event_name": "PreToolUse", "session_id": "rm-guard-test", "cwd": cwd,
				"tool_name": "Bash", "tool_input": map[string]string{"command": tc.command},
			})
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(binary, "--config", config, "pre-tool-use")
			cmd.Env = []string{"HOME=" + dir, "XDG_CONFIG_HOME=" + dir, "PATH=" + os.Getenv("PATH")}
			cmd.Stdin = bytes.NewReader(input)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			err = cmd.Run()
			if !tc.deny {
				if err != nil {
					t.Fatalf("ожидался пропуск: %v; stderr: %s", err, stderr.String())
				}
				return
			}
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
				t.Fatalf("ожидался rc=2, получено %v; stderr: %s", err, stderr.String())
			}
			for _, want := range []string{rmCdGlobHeader, "после cd каталог глоба", tc.target} {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("нет %q в отказе: %s", want, stderr.String())
				}
			}
		})
	}
}

func TestRmGuard330CdSymlink(t *testing.T) {
	root := t.TempDir()
	out := t.TempDir()
	for _, name := range []string{"escape", "link"} {
		if err := os.Symlink(out, filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
		command := "cd ./" + name + " && rm -f files/*"
		if validateRmWith(t, newRmGuard(t), command, root).IsValid {
			t.Errorf("symlink наружу должен отклоняться: %s", command)
		}
		for _, command := range []string{
			"cd ./" + name + " && cd ./child && rm -f files/*",
			"cd ./" + name + " && rm -f files/* && cd ./child",
		} {
			if validateRmWith(t, newRmGuard(t), command, root).IsValid {
				t.Errorf("symlink перед rm должен отклоняться: %s", command)
			}
		}
	}
}

func TestRmGuard330HintUsesLiteralPath(t *testing.T) {
	for _, original := range []string{`rm -f $Q/*.tmp`, `rm -f ${Q%z}/*.tmp`, `rm -f ${Q#z}/*.tmp`, `rm -f ${Q:+literal}/*.tmp`, `rm -f ${Q:0:7}/*.tmp`, `rm -f ${Q:-$Y}/*.tmp`} {
		r := validateRm(t, original)
		if r.IsValid || len(r.Suggestions) == 0 {
			t.Fatalf("нет подсказки: %s", original)
		}
		_, tail, found := strings.Cut(r.Suggestions[0], "подставь путь каталога буквально, например: `")
		example, _, closed := strings.Cut(tail, "`")
		if !found || !closed {
			t.Fatalf("нет команды в подсказке: %v", r.Suggestions)
		}
		if strings.ContainsAny(tail, "$") || strings.Contains(tail, "RM_GUARD_TARGET") {
			t.Errorf("подсказка переписывает раскрытие: %s", tail)
		}
		if !validateRm(t, example).IsValid {
			t.Errorf("пример отклоняется: %s", example)
		}
	}
}

func validateRm(t *testing.T, command string) *core.ValidationResult {
	t.Helper()
	result, err := newRmGuard(t).ValidateTool(rmTestContext(),
		&core.ToolInput{ToolName: "Bash", Command: command, CWD: "/home/u/proj"})
	if err != nil {
		t.Fatalf("ValidateTool: %v", err)
	}
	return result
}

// TestRmGuardMatchesClaudeCode сверяет 198 команд с вердиктами Claude Code
// 2.1.330 в bypassPermissions; true — Claude Code остановил команду запросом
// о возможно пустой переменной или критическом пути.
func TestRmGuardMatchesClaudeCode(t *testing.T) {
	for _, tc := range []struct {
		command string
		deny    bool
	}{
		{`S=/home/u/sb; rm -f $S/*.zz`, false},
		{`S=/home/u/sb; rm -f "$S"/*.zz`, false},
		{`S=/home/u/sb; rm -f "${S:?}"/*.zz`, false},
		{`rm -f /home/u/sb/*.zz`, false},
		{`S=/home/u/sb; rm -f "$S/"`, false},
		{`S=/home/u/sb; rm -f "$S/file1.zz"`, false},
		{`S=/home/u/sb; rm -f $S/.hidden`, false},
		{`S=/home/u/sb; ls $S && rm -f ${S}/*.zz`, true},
		{`S=/home/u/sb; echo x | rm -f $S/*.zz`, true},
		{`S=/home/u/sb; X=$(rm -f $S/*.zz)`, false},
		{`S=/home/u/sb; echo "$(rm -f $S/*.zz)"`, true},
		{"S=/home/u/sb; cat <<EOT\nrm -f $S/*.zz\nEOT", false},
		{"S=/home/u/sb; cat <<'EOT'\nrm -f $S/*.zz\nEOT", false},
		{`S=/home/u/sb; echo "rm -f $S/*.zz"`, false},
		{`S=/home/u/sb; echo 'a; rm -f $S/*.zz'`, false},
		{`S=/home/u/sb; echo "a && rm -f $S/*.zz"`, false},
		{`S=/home/u/sb; bash -c "rm -f $S/*.zz"`, false},
		{`bash -c 'rm -f "$1"/*.zz' _ /home/u/sb`, false},
		{`false && rm -f "$1"/*.zz`, true},
		{`false && rm -f $UNSETVAR_XYZ/tmp`, true},
		{`D=/home/u/sb; false && rm -f $D/tmp`, false},
		{`false && rm -f $HOME/*`, true},
		{`D=$(pwd); false && rm -rf "$D"`, true},
		{`S=/home/u/sb; find /home/u/sb -name '*.qq' -exec rm -f $S/{} \;`, false},
		{`S=/home/u/sb; ls /home/u/sb | xargs -I{} rm -f $S/{}.zz`, true},
		{`S=/home/u/sb; eval "rm -f $S/*.zz"`, false},
		{`S=/home/u/sb; trap 'rm -f "$S"/*.zz' EXIT; echo t`, false},
		{`S=/home/u/sb; rm -f '$S'/*.zz`, false},
		{`S=/home/u/sb; (rm -f $S/*.zz)`, true},
		{`S=/home/u/sb; { rm -f $S/*.zz; }`, true},
		{`S=/home/u/sb; rm -f ${S:-}/*.zz`, true},
		{`S=/home/u/sb; rm -f -- $S/*.zz`, false},
		{`S=/home/u/sb/nodir; rmdir $S/`, false},
		{`S=/home/u/sb; echo rm -f $S/*.zz`, false},
		{`S=/home/u/sb; true # rm -f $S/*.zz`, false},
		{`F=$(dirname /home/u/sb/x); false && rm -rf "$F"`, false},
		{`T=$(mktemp -d); rm -rf "$T"`, false},
		{`S=/home/u/sb; rm -f $S/$NOPE_N`, true},
		{`false && rm -f $1/x`, false},
		{`S=/home/u/sb; command rm -f $S/*.zz`, false},
		{`S=/home/u/sb; \rm -f $S/*.zz`, false},
		{`S=/home/u/sb; /bin/rm -f $S/*.zz`, false},
		{`S=/home/u/sb; timeout 5 rm -f $S/*.zz`, false},
		{"S=/home/u/sb\nrm -f $S/*.zz", false},
		{`false && rm -f "$PREFIX_UNSET_Q/usr"`, true},
		{`false && rm -f "${PREFIX_UNSET_Q:?}/usr"`, false},
		{`S=/home/u/sb; rm -f $S/b.zz`, false},
		{`S=/home/u/sb; git log -1 --format=%s -m "x; rm -f $S/*" >/dev/null 2>&1; true`, false},
		{`S=/home/u/sb; sh -c 'rm -f "$S"/*.zz'`, true},
		{`S=/home/u/sb; sudo -n rm -f $S/*.zz`, false},
		{"S=/home/u/sb; cat > /home/u/sb/x.sh <<'EOT'\nS=foo\nrm -rf $S/*\nEOT", false},
		{`S=/home/u/sb; rm -f "$S"/a.zz "$S"/b.zz`, false},
		{`S=/home/u/sb; rm -f ${S}/*.zz`, true},
		{`S=/home/u/sb; ls $S && rm -f $S/*.zz`, false},
		{`S=/home/u/sb; for f in a b; do echo $f; done; rm -f $S/*.zz`, true},
		{`S=/home/u/sb; cd /home/u/sb; rm -f $S/*.zz`, false},
		{`S=/home/u/sb; echo "$(date)"; rm -f $S/*.zz`, true},
		{`S=/home/u/sb; X=$(date); rm -f $S/*.zz`, false},
		{`S=/home/u/sb; (echo hi); rm -f $S/*.zz`, true},
		{`S=/home/u/sb; if true; then rm -f $S/*.zz; fi`, false},
		{`S=/home/u/sb; while false; do :; done; rm -f $S/*.zz`, false},
		{`S="/home/u/sb"; rm -f $S/*.zz`, false},
		{`S=$PWD/x; false && rm -f $S/*.zz`, true},
		{`S=$(mktemp -d); false && rm -rf $S/*`, true},
		{`export S=/home/u/sb; rm -f $S/*.zz`, false},
		{`S=/home/u/sb; echo a > /dev/null; rm -f $S/*.zz`, false},
		{`S=/home/u/sb; rm -f $S/*.zz 2>/dev/null`, false},
		{`S=/home/u/sb; rm -f $S/*.zz || true`, false},
		{`S=/home/u/sb; cat /home/u/sb/a.tmp | head -1; rm -f $S/*.zz`, true},
		{`S=/home/u/sb; [ -d $S ] && rm -f $S/*.zz`, false},
		{`S=/home/u/sb; test -d "$S" && rm -f "$S"/*.zz`, false},
		{`S=/home/u/sb; mkdir -p $S; rm -f $S/*.zz; ls $S`, false},
		{`S=/home/u/sb; echo $S; rm -f $S/*.zz; echo done`, false},
		{`ls /home/u/sb; S=/home/u/sb; rm -f $S/*.zz; ls /home/u/sb`, true},
		{`S=/home/u/sb; S2=$S/sub; rm -f $S2/*.zz`, false},
		{`for S in /home/u/sb; do rm -f $S/*.zz; done`, true},
		{`S=/home/u/sb; sleep 0 && rm -f "$S"/*.zz && echo ok`, false},
		{`S=/home/u/sb; rm -rf $S/*.zz`, false},
		{`S=/home/u/sb/nodir; rm -rf $S/`, false},
		{`S=/home/u/sb; rm -f $S/*`, false},
		{`S=/home/u/sb; ls $S | wc -l; rm -f $S/*.zz`, true},
		{`S=/home/u/sb; echo "${S}"; rm -f $S/*.zz`, true},
		{`S=/home/u/sb; echo $((1+1)); rm -f $S/*.zz`, false},
		{`S=/home/u/sb; echo "a" | grep -q a && rm -f $S/*.zz`, true},
		{"S=/home/u/sb; cat <<'EOT' >/dev/null\nhello\nEOT\nrm -f $S/*.zz", true},
		{`S=/home/u/sb; rm -f $S/*.zz # cleanup`, false},
		{`S=/home/u/sb && rm -f $S/*.zz`, false},
		{`S=/home/u/sb; rm -f "$S"/*.zz "$S"/*.yy`, false},
		{`S=/home/u/sb; ls $(echo /home/u/sb) >/dev/null; rm -f $S/*.zz`, true},
		{`S=/home/u/sb; f() { :; }; rm -f $S/*.zz`, true},
		{`S=/home/u/sb; rm -f $S/*.zz; for i in 1; do :; done`, false},
		{"S=/home/u/sb; echo `date`; rm -f $S/*.zz", true},
		{`S=/home/u/sb; rm -f $S/*.zz; X=1`, false},
		{`S=/home/u/sb; rm -f $S/*.zz; echo ${S}`, true},
		{`S=/home/u/sb; rm -f $S/*.zz; echo "$(date)"`, true},
		{`S=/home/u/sb; echo hi; X=1; rm -f $S/*.zz`, false},
		{`S=/home/u/sb; for f in a; do :; done; rm -f $S/*.zz`, false},
		{`S=/home/u/sb; echo '${x}'; rm -f $S/*.zz`, false},
		{`S=/home/u/sb; echo '$(date)'; rm -f $S/*.zz`, false},
		{`S=/home/u/sb; cat <<<hi; rm -f $S/*.zz`, false},
		{`S=/home/u/sb; { echo; }; rm -f $S/*.zz`, true},
		{`S=/home/u/sb; rm -f $S/*.zz; (echo)`, true},
		{"S=/home/u/sb; rm -f $S/*.zz; cat <<'EOT' >/dev/null\nx\nEOT", true},
		{`cd /home/u/sb && S=/home/u/sb; rm -f $S/*.zz`, true},
		{`S=/home/u/sb; echo a | read X; rm -f $S/*.zz`, true},
		{`S=/home/u/sb T=/home/u/sb; rm -f $S/*.zz`, true},
		{`S=/home/u/sb false && rm -f $S/*.zz`, true},
		{`declare S=/home/u/sb; rm -f $S/*.zz`, false},
		{`S=/home/u/sb; X=$(date) Y=1; rm -f $S/*.zz`, true},
		{`S=/home/u/sb; if [ -n "$S" ]; then rm -rf "$S"/*.zz; fi`, false},
		{`S=/home/u/sb; rm -f $S/*.zz && echo "done: $(date)"`, false},
		{"S=/home/u/sb; X=`date`; rm -f $S/*.zz", false},
		{`S=/home/u/sb; S=$S/sub; rm -f $S/*.zz`, true},
		{`S=/home/u/sb; unset S; false && rm -f $S/*.zz`, true},
		{`S=/home/u/sb; echo x; rm -f $S/*.zz; S=/home/u/sb; rm -f $S/*.yy`, true},
		{`export S=/home/u/sb T=/home/u/sb; rm -f $T/*.zz`, false},
		{`S=/home/u/sb; rm -f "$S"/*.zz $(echo /home/u/sb/none.zz)`, true},
		{"S=/home/u/sb\necho a\nrm -f $S/*.zz", false},
		{`S=/home/u/sb; echo a; echo b; rm -f $S/*.zz`, false},
		{`S=/home/u/sb; ls /home/u/sb | while read f; do :; done; rm -f $S/*.zz`, true},
		{`S=/home/u/sb; [[ -d $S ]] && rm -f $S/*.zz`, false},
		{`S=/home/u/sb; case x in x) rm -f $S/*.zz;; esac`, false},
		{`S=/home/u/sb; time rm -f $S/*.zz`, false},
		{`S=/home/u/sb; nohup rm -f $S/*.zz >/dev/null 2>&1`, false},
		{`S=/home/u/sb; unbuffer -noecho rm -f $S/*.zz`, false},
		{`S=/home/u/sb; unbuffer -p rm -f $S/*.zz`, false},
		{`S=/home/u/sb; rm -f $S/*.zz &`, false},
		{`S="/home/u/sb/x y"; rm -f "$S"/*.zz`, false},
		{`S=~/nonexist-q; rm -f $S/*.zz`, true},
		{`S=/home/u/sb; S2="$S"; rm -f $S2/*.zz`, false},
		{`S=/home/u/sb; X=1 echo hi; rm -f $S/*.zz`, false},
		{`true; S=/home/u/sb; rm -f $S/*.zz`, true},
		{`echo $((1)); S=/home/u/sb; rm -f $S/*.zz`, true},
		{`S=/home/u/sb; rm -f $S/*.zz; for f in a; do echo $f; done`, true},
		{`T=$(mktemp -d); false && rm -rf "$T"`, false},
		{`T=$(mktemp -d); false && rm -rf "$T"/*`, true},
		{`D=/home/u/sb; for f in $D/*.zz; do rm -f "$f"; done`, false},
		{`D=/home/u/sb; rm -f "$D"/a.zz "$D"/b.zz && ls $D`, false},
		{`D=/home/u/sb; cd $D && rm -f ./*.zz`, false},
		{`OUT=/home/u/sb; rm -rf "$OUT"/*.zz; mkdir -p "$OUT"`, false},
		{`cd /home/u/sb && OUT=/home/u/sb && rm -rf "$OUT"/*.zz`, true},
		{`S=/home/u/sb; rm -f $S/*.zz; echo "removed from ${S}"`, true},
		{`S=/home/u/sb; rm -f "$S"/*.zz 2>&1 | tail -1`, false},
		{`ls /home/u/sb >/dev/null && S=/home/u/sb && rm -f $S/*.zz`, true},
		{`false && rm -rf "$WORKDIR_UNSET_Q"/*`, true},
		{`false && rm -rf "${WORKDIR_UNSET_Q:?}"/*`, false},
		{`S=/home/u/sb; X=$(ls $S | wc -l); rm -f $S/*.zz`, true},
		{`S=/home/u/sb; echo "$(ls $S)" >/dev/null; rm -f $S/*.zz`, true},
		{`S=/home/u/sb; rm -f $S/*.zz $S/*.yy`, false},
		{`S=/home/u/sb; printf '%s\n' a; rm -f $S/*.zz`, false},
		{`S=/home/u/sb; rm -f "$S"/*.zz; echo 'done ${x}'`, false},
		{`S=/home/u/sb; rm -fv -- "$S"/*.zz`, false},
		{`S=/home/u/sb; rm -f $S/*.zz # ${not}`, false},
		{`S=/home/u/sb; awk '{print $1}' /home/u/sb/a.tmp; rm -f $S/*.zz`, false},
		{`S=/home/u/sb; echo $S/*.zz | xargs rm -f`, false},
		{`S=/home/u/sb; git -C /home/u/sb status 2>/dev/null || rm -f $S/*.zz`, false},
		{`S=/home/u/sb; S="$S"; rm -f $S/*.zz`, false},
		{`S=/home/u/sb; rm -f $S/*.zz; T=/home/u/sb; rm -f $T/*.zz`, true},
		{`S=/home/u/sb; (cd $S && ls) >/dev/null; rm -f $S/*.zz`, true},
		{`S=/home/u/sb; rm -f "$S"/{a,b}.zz`, true},
		{`S=/home/u/sb; rm -f $S/[ab].zz`, false},
		{`false && rm -rf "$1/"`, true},
		{`f() { rm -rf "$1"/*.zz; }; f /home/u/sb`, false},
		{`S=/home/u/sb; diff <(ls $S) <(ls /home/u/sb); rm -f $S/*.zz`, true},
		{`S=/home/u/sb; echo $(( $(ls $S | wc -l) + 1 )); rm -f $S/*.zz`, true},
		{`S=/home/u/sb; rm -f $S/*.zz && echo ok || echo fail`, false},
		{`S=/home/u/sb; rmdir $S/nodir 2>/dev/null; true`, false},
		{`export S=/home/u/sb; bash -c 'rm -f "$S"/*.zz'`, false},
		{`S=/home/u/sb; sh -c "rm -f '$S'/*.zz"`, false},
		{`S=/home/u/sb; timeout 10 bash -c "rm -f $S/*.zz"`, false},
		{`S=/home/u/sb; echo ${#S}; rm -f $S/*.zz`, true},
		{`S=/home/u/sb; ls -la $S | head -3; rm -f "$S"/*.zz; ls $S`, true},
		{`S=/home/u/sb; mkdir -p "$S" && touch "$S/x.zz" && rm -f "$S"/*.zz`, false},
		{`false && sudo rm -rf $NOT_SET_Q/*`, true},
		{`S=/home/u/sb; rm -f "${S:?S пуст}"/b.zz`, false},
		{`S=/home/u/sb; rm -rf "${S:?}/x"`, false},
		{`false && rm -rf "${NOPE_Q:?нет NOPE}"/*`, false},
		{`false && rm -rf "${NOPE_Q:?}/x"`, false},
		{`false && rm -rf "${NOPE_Q:?msg}/usr"`, false},
		{`false && rm -rf ${NOPE_Q:?}/*`, false},
		{`false && rm -rf "${NOPE_Q?}"/*`, true},
		{`false && rm -rf "${NOPE_Q:?}"`, false},
		{`false && rm -rf "$NOPE_Q/usr"`, true},
		{`false && rm -rf "${NOPE_Q:=}"/*`, true},
		{`false && rm -rf "${NOPE_Q=}"/*`, true},
		{`false && rm -rf "${NOPE_Q:=}/usr"`, false},
		{`false && rm -rf "${NOPE_Q:+x}"/*`, true},
		{`false && rm -rf "${NOPE_Q%/}"/*`, true},
		{`false && rm -rf "${NOPE_Q#x}"/*`, true},
		{`false && rm -rf "${NOPE_Q/a/b}"/*`, true},
		{`false && rm -rf "${NOPE_Q:0:3}"/*`, true},
		{`false && rm -rf "${NOPE_Q%/}/usr"`, false},
		{`false && rm -rf "${NOPE_Q:=/home/nonexist-q}"/*`, false},
		{`false && rm -rf "${NOPE_Q:-}"/*`, true},
		{`false && rm -rf "${NOPE_Q=}/usr"`, false},
		{`false && rm -rf "${NOPE_Q:+x}/usr"`, false},
		{`false && rm -rf "${NOPE_Q#x}/usr"`, false},
		{`false && rm -rf "${NOPE_Q/a/b}/usr"`, false},
		{`false && rm -rf "${NOPE_Q:0:3}/usr"`, false},
		{`false && rm -rf "${NOPE_Q:-}/usr"`, true},
	} {
		r := validateRm(t, tc.command)
		if r.IsValid == tc.deny {
			t.Errorf("deny=%v, ожидалось %v: %q %v", !r.IsValid, tc.deny, tc.command, r.Suggestions)
		}
	}
}

// TestRmGuardExtra дополнительные проверки общих форм правила.
func TestRmGuardExtra(t *testing.T) {
	for _, tc := range []struct {
		command string
		deny    bool
	}{
		// случай из сессии лида: перед присваиванием есть другие команды
		{`cd /home/u/proj && ls; S=/home/u/scratchpad/sl; rm -f $S/*; ls`, true},
		{"T=$(mktemp -d)\nfalse && rm -rf \"$T\"/*", true},
		{`T=$(mktemp -d); rm -rf "${T:?}"/*`, false},
		{`false && rm -rf "$NOPE"/*.log | tee /dev/null`, true},
		{`false && xargs rm -f $NOPE/*`, true},
		{`false && find . -name x -exec rm -rf $NOPE/{} +`, true},
		{`echo $(false && rm -rf $NOPE/*)`, true},
		{"cat <<EOF\nrm -rf $NOPE/*\nEOF", false},
		{`grep -n 'rm -rf $NOPE/*' notes.txt`, false},
		{`echo "rm -rf $NOPE/*" > notes.txt`, false},
		{`git commit -m "rm -rf $NOPE/* больше не нужен"`, false},
		{`false && rm -rf /home/u/sb/*`, false},
		{`false && rm -rf "$NOPE"`, false},
		{`false && unbuffer -noecho rm -rf "$NOPE/"`, true},
		{`false && rm -rf $NOPE/build`, false},
		{`D=$(git rev-parse --show-toplevel); false && rm -rf "$D"`, true},
		{`D=$(cd /home/u/proj/x && pwd); false && rm -rf "$D"`, false},
	} {
		r := validateRm(t, tc.command)
		if r.IsValid == tc.deny {
			t.Errorf("deny=%v, ожидалось %v: %q %v", !r.IsValid, tc.deny, tc.command, r.Suggestions)
		}
	}
}

// TestRmGuardReason причина доходит до агента с готовой переписанной формой
func TestRmGuardReason(t *testing.T) {
	for _, tc := range []struct {
		command, want string
	}{
		{`S=/home/u/sb; echo x | rm -f $S/*.zz`,
			"`rm -f $S/*.zz`: подставь путь каталога буквально, например: `rm -f /abs/dir/*.tmp`"},
		{`false && rm -f $HOME/*`,
			"`rm -f $HOME/*`: удаление всего в HOME Claude Code остановит, укажи более узкий литеральный путь"},
		{`false && rm -f "${HOME:?}"/*`,
			"`rm -f \"${HOME:?}\"/*`: удаление всего в HOME Claude Code остановит, укажи более узкий литеральный путь"},
		{`cd /x && ls; S=/home/u/scratchpad/sl; rm -f $S/*; ls`,
			"`rm -f $S/*`: подставь путь каталога буквально, например: `rm -f /abs/dir/*.tmp`"},
		{`false && rm -rf "$NOPE/"`,
			"`rm -rf \"$NOPE/\"`: подставь путь каталога буквально, например: `rm -f /abs/dir/*.tmp`"},
		{`false && rm -f ${NOPE:-}/*.zz`,
			"`rm -f ${NOPE:-}/*.zz`: подставь путь каталога буквально, например: `rm -f /abs/dir/*.tmp`"},
		{`false && rm -f "$1"/*.zz`,
			"`rm -f \"$1\"/*.zz`: $1 в команде Bash пуст, укажи литеральный путь"},
		{`D=$(pwd); false && rm -rf "$D"`,
			"`rm -rf \"$D\"`: $D задан из `$(pwd)` и может оказаться рабочей папкой, защита ${D:?} не поможет, укажи литеральный путь"},
		{`S=/x; sh -c 'rm -f "$S"/*.zz'`,
			"`rm -f \"$S\"/*.zz`: подставь путь каталога буквально, например: `rm -f /abs/dir/*.tmp`"},
	} {
		r := validateRm(t, tc.command)
		if r.IsValid {
			t.Errorf("должно отклоняться: %q", tc.command)
			continue
		}
		if len(r.Violations) == 0 || !strings.Contains(r.Violations[0].Message, "Claude Code спросит пользователя") {
			t.Errorf("нет заголовка причины: %q %v", tc.command, r.Violations)
		}
		if !strings.Contains(strings.Join(r.Suggestions, "\n"), tc.want) {
			t.Errorf("%q:\nесть  %q\nждали %q", tc.command, r.Suggestions, tc.want)
		}
	}
}

// TestRmGuardCdGlobMatchesClaudeCode сверяет 105 команд с вердиктами Claude
// Code 2.1.330 в bypassPermissions; true — Claude Code остановил команду.
func TestRmGuardCdGlobMatchesClaudeCode(t *testing.T) {
	for _, tc := range []struct {
		command string
		deny    bool
	}{
		{`cd /home/u/sb && rm -f cl/*`, true},
		{`cd /home/u/sb && rm -f ./cl/*`, true},
		{`cd /home/u/sb && rm -f cl/x`, false},
		{`cd /home/u/sb && rm -f /home/u/sb/cl/*`, false},
		{`cd /home/u/sb && node -e 'const x="rm -f cl/*"; console.log(x.length)'`, false},
		{`cd /home/u/sb && rm -f */x`, false},
		{`cd /home/u/sb && rm -rf cl/*/`, true},
		{`cd /home/u/sb && echo $? && rm -f cl/*`, true},
		{`cd /home/u/sb && rm -f cl/* && echo "$(date +%s)"`, true},
		{"cd /home/u/sb && cat > hd.txt <<'EOT'\nx\nEOT\nrm -f cl/*", true},
		{`cd /home/u/sb && rm -f cl/* 2>/dev/null`, true},
		{`cd /home/u/sb && rm -f cl/*; for f in a; do :; done`, true},
		{`cd /home/u/sb && D=x && rm -f cl/*`, true},
		{`cd /home/u/sb && rm -f ../sb/cl/*`, true},
		{`cd /home/u/sb && rmdir cl/* 2>/dev/null; true`, true},
		{`cd /home/u/sb && X=$(ls) && rm -f cl/*`, true},
		{`cd /home/u/sb && rm -f cl/* a/*`, true},
		{`cd /home/u/sb && rm -f -- cl/*`, true},
		{`cd /home/u/sb && touch cl/1 && for t in a; do echo $t; done && rm -f cl/*`, true},
		{`cd /home/u/sb && echo ${HOME} && rm -f /home/u/sb/cl/*`, false},
		{`cd /home/u/sb && rm -f cl/*.png`, false},
		{`cd /home/u/sb && rm -f cl/**`, false},
		{`rm -f cl/*`, false},
		{`rm -f a/*`, false},
		{`X=$(cd /home/u/sb && pwd); rm -f a/*`, true},
		{`bash -c 'cd /home/u/sb && rm -f cl/*'`, true},
		{`cd /home/u/sb && bash -c 'rm -f cl/*'`, true},
		{`cd /home/u/sb && rm -f cl/[ab]*`, false},
		{`cd /home/u/sb && rm -f cl/a*`, false},
		{`cd /home/u/sb && rm -rf a/* && rm -f ~/nonexistent-q/*`, true},
		{`cd /home/u/sb && sh -c 'echo hi'; rm -f a/*`, true},
		{`cd /home/u/sb && rm -f a/.??*`, false},
		{`cd /home/u/sb && rm -rf a`, false},
		{`cd /home/u/sb && rm -f a/z && rm -f cl/z`, false},
		{`cd /home/u/sb && echo 'rm -f cl/*' > note.txt`, false},
		{`cd /home/u/sb && git status 2>/dev/null; echo rm -f cl/*`, false},
		{`cd /home/u/sb; rm -f cl/*`, true},
		{`cd /home/u/sb/a && rm -rf *`, true},
		{`D=/home/u/sb/a; cd $D && rm -rf *`, true},
		{`cd /home/u/sb && for t in a b; do touch cl/$t; done; rm -f cl/*`, true},
		{`cd /home/u/sb && rm -f cl/* && echo ${HOME:0:1}`, true},
		{`cd /home/u/sb && { rm -f cl/*; }`, true},
		{`pushd /home/u/sb >/dev/null && rm -f cl/*; popd >/dev/null`, true},
		{`(cd /home/u/sb && rm -f cl/*)`, true},
		{`cd "/home/u/sb" && rm -f "cl"/*`, true},
		{`D=/home/u/sb; cd "$D" && rm -rf cl/* && echo ${D}`, true},
		{`cd /home/u/sb/a && rm -rf .[!.]* *`, true},
		{`cd /home/u/sb && rm -f cl/* && magick light/1440x900-full.png -crop 800x1100+320+0 +repage cl/l-%02d.png && magick dark/1440x900-full.png -crop 800x1100+320+0 +repage cl/d-%02d.png && ls cl`, true},
		{`cd /home/u/sb && rm -f cl/* && ls cl`, true},
		{`cd /home/u/sb && rm -f cl/* | cat`, true},
		{`cd /home/u/sb && rm -f cl/*; ls | head -1`, true},
		{`cd /home/u/sb && rm -f ~/nonexistent-q/*`, false},
		{`cd /home/u/sb && for f in a; do rm -f cl/*; done`, true},
		{`if true; then cd /home/u/sb; fi; rm -f a/*`, true},
		{`cd /home/u/sb && if true; then rm -f cl/*; fi`, true},
		{`{ cd /home/u/sb; rm -f cl/*; }`, true},
		{`pushd /home/u/sb; rm -f cl/*`, true},
		{`cd /home/u/sb && timeout 5 rm -f cl/*`, true},
		{`cd /home/u/sb && command rm -f cl/*`, true},
		{`cd /home/u/sb && xargs rm -f cl/* </dev/null`, false},
		{`cd /home/u/sb && rm -f "cl/*"`, true},
		{`echo $(cd /home/u/sb; rm -f cl/*)`, true},
		{`cd /home/u/sb && eval 'rm -f cl/*'`, false},
		{`rm -f a/*; cd /home/u/sb`, true},
		{`cd /home/u/sb && find . -name zz -exec rm -f cl/* \;`, false},
		{`cd /home/u/sb && rm -f cl/* && ls cl | wc -l`, true},
		{`cd /home/u/sb || true; rm -f a/*`, true},
		{`true || true; cd /home/u/sb && rm -f cl/*`, true},
		{`cd /home/u/sb && rm -f cl/* || true`, true},
		{`cd /home/u/sb && echo "a|b" && rm -f cl/*`, true},
		{`cd /home/u/sb && (ls | head -1); rm -f cl/*`, true},
		{`for d in /home/u/sb/a; do cd /home/u/sb/a; rm -rf *; done`, true},
		{`for d in a; do cd $d; done; rm -f cl/*`, true},
		{`cd /home/u/sb/a; for f in x; do rm -rf *; done`, true},
		{`while false; do cd /home/u/sb; done; rm -f a/*`, true},
		{`for f in a; do cd /home/u/sb; done; rm -f a/*`, true},
		{`if true; then cd /home/u/sb; fi && rm -f a/*`, true},
		{`true && { cd /home/u/sb; }; rm -f a/*`, true},
		{`if cd /home/u/sb; then rm -f cl/*; fi`, true},
		{`! cd /home/u/sb; rm -f a/*`, true},
		{`cd /home/u/sb && sudo -n rm -f cl/* 2>/dev/null; true`, false},
		{`cd /home/u/sb && env rm -f cl/*`, true},
		{`cd /home/u/sb && nice rm -f cl/*`, true},
		{`cd /home/u/sb && nohup rm -f cl/* >/dev/null 2>&1`, true},
		{`cd /home/u/sb && time rm -f cl/*`, true},
		{`cd /home/u/sb && LC_ALL=C rm -f cl/*`, true},
		{`cd /home/u/sb && \rm -f cl/*`, true},
		{`cd /home/u/sb && /bin/rm -f cl/*`, true},
		{`time cd /home/u/sb; rm -f a/*`, true},
		{`command cd /home/u/sb; rm -f a/*`, true},
		{`builtin cd /home/u/sb && rm -f cl/*`, true},
		{`cd /home/u/sb && rm -f cl/./*`, true},
		{`cd /home/u/sb && rm -f cl//*`, true},
		{`cd /home/u/sb && rm -f cl/*/*`, true},
		{`cd /home/u/sb && rm -f */`, true},
		{`echo a | cat; cd /home/u/sb && rm -f cl/* && echo ${HOME:0:1}`, true},
		{`echo a | cat; { cd /home/u/sb; rm -f cl/*; }`, true},
		{"echo a | cat; cd /home/u/sb && cat > hd.txt <<'EOT'\nx\nEOT\nrm -f cl/*", true},
		{`echo a | cat; cd /home/u/sb && rm -f cl/*; echo $(date +%s)`, true},
		{`echo a | cat; cd /home/u/sb && rm -f cl/*; echo $?`, true},
		{`cd /home/u/sb && ls 2>&1 | grep -v png; rm -f cl/*; for t in light dark; do echo ${t:0:1}; done`, true},
		{`ls | grep -v png; cd /home/u/sb && rm -f cl/*; for t in a; do echo $t; done`, true},
		{`ls | cat; cd /home/u/sb && (rm -f cl/*)`, true},
		{`echo a | cat; cd /home/u/sb && rm -f cl/*; echo ${HOME}`, true},
		{`D=/home/u/sb/a; for f in x; do cd $D; rm -rf *; done`, true},
	} {
		r := validateRm(t, tc.command)
		if r.IsValid == tc.deny {
			t.Errorf("deny=%v, ожидалось %v: %q %v", !r.IsValid, tc.deny, tc.command, r.Suggestions)
		}
	}
}

// TestRmGuard330Extras проверяет 80 проб Extra с CWD /home/u/proj.
func TestRmGuard330Extras(t *testing.T) {
	for _, tc := range []struct {
		command string
		deny    bool
	}{
		{`cd ./sub && rm -f cl/*`, false},
		{`cd /home/u/proj/sub && rm -f cl/*`, false},
		{`cd /home/u/proj/sub/ && rm -f cl/*`, false},
		{`cd "/home/u/proj/sub" && rm -f cl/*`, false},
		{`cd /home/u/proj && rm -f sub/cl/*`, false},
		{`cd sub && rm -f cl/*`, true},
		{`cd sub/a && rm -f cl/*`, true},
		{`cd ./sub/a && rm -f cl/*`, false},
		{`cd /home/aisee/.cache/claude-tmp/rmcc330/sb && rm -f cl/*`, true},
		{`cd /home/aisee/.cache/claude-tmp/rmcc330/sb/Extra-0/sub && rm -f cl/*`, true},
		{`cd ./nonexist && rm -f cl/*`, false},
		{`cd /home/u/proj/nonexist && rm -f cl/*`, false},
		{`cd ./sub; rm -f cl/*`, true},
		{`cd ./sub && rm -rf cl/*/`, true},
		{`cd ./sub && rm -rf cl/*`, false},
		{`cd ./sub && rm -f ./cl/*`, false},
		{`cd ./sub && echo x && rm -f cl/*`, false},
		{`cd ./sub && rm -f ../sub/cl/*`, true},
		{`cd ./sub && rm -f cl/* 2>/dev/null`, false},
		{`cd ./sub && rmdir cl/*`, false},
		{`cd ./sub && D=x && rm -f cl/*`, false},
		{`cd ./sub/../.. && rm -f cl/*`, true},
		{`cd ./../x && rm -f cl/*`, true},
		{`cd /home/u/proj/../Extra-1 && rm -f cl/*`, true},
		{`cd /home/u/proj/sub && rm -rf cl/*/`, true},
		{`cd /home/u/proj/sub; rm -f cl/*`, true},
		{`cd ~ && rm -f cl/*`, true},
		{`cd . && rm -f cl/*`, false},
		{`cd /home/u/proj && rm -f cl/*`, false},
		{`cd -P ./sub && rm -f cl/*`, true},
		{`cd ./sub || exit; rm -f cl/*`, true},
		{`cd ./sub && cd cl && rm -f */x*`, false},
		{`cd sub/ && rm -f cl/*`, true},
		{`cd "./sub" && rm -f cl/*`, false},
		{`cd ./sub && rm -f cl/*`, false},
		{`cd ./sub ; rm -f cl/*`, true},
		{`cd ./sub && rm -rf cl/*/`, true},
		{`cd ./sub && rm -f ../sub/cl/*`, true},
		{`cd ./sub && rm -f */x`, false},
		{`cd /home/u/proj/sub && rm -f cl/*`, false},
		{`cd /home/u/proj/sub ; rm -f cl/*`, true},
		{`cd /home/u/proj/sub && rm -rf cl/*/`, true},
		{`cd /home/u/proj/sub && rm -f ../sub/cl/*`, true},
		{`cd /home/u/proj/sub && rm -f */x`, false},
		{`cd . && rm -f cl/*`, false},
		{`cd . ; rm -f cl/*`, true},
		{`cd . && rm -rf cl/*/`, true},
		{`cd . && rm -f ../sub/cl/*`, true},
		{`cd . && rm -f */x`, false},
		{`cd /home/u/proj && rm -f cl/*`, false},
		{`cd /home/u/proj ; rm -f cl/*`, false},
		{`cd /home/u/proj && rm -rf cl/*/`, false},
		{`cd /home/u/proj && rm -f ../sub/cl/*`, false},
		{`cd /home/u/proj && rm -f */x`, false},
		{`cd sub && rm -f cl/*`, true},
		{`cd sub ; rm -f cl/*`, true},
		{`cd sub && rm -rf cl/*/`, true},
		{`cd sub && rm -f ../sub/cl/*`, true},
		{`cd sub && rm -f */x`, false},
		{`S=/home/u/sb; echo x | rm -f $S/*.zz`, true},
		{`false && rm -f $HOME/*`, true},
		{`S=/home/u/sb; ls /home/u/sb | xargs -I{} rm -f $S/{}.zz`, true},
		{`S=/home/u/sb; cat /home/u/sb/a.tmp | head -1; rm -f $S/*.zz`, true},
		{`S=/home/u/sb; ls $S | wc -l; rm -f $S/*.zz`, true},
		{`S=/home/u/sb; echo x | rm -f "${S:?}"/*.zz`, false},
		{`false && rm -f "${HOME:?}"/*`, true},
		{`S=/home/u/sb; ls /home/u/sb | xargs -I{} rm -f "${S:?}"/{}.zz`, false},
		{`S=/home/u/sb; cat /home/u/sb/a.tmp | head -1; rm -f "${S:?}"/*.zz`, false},
		{`S=/home/u/sb; ls $S | wc -l; rm -f "${S:?}"/*.zz`, false},
		{`S=/home/u/sb; echo "a" | grep -q a && rm -f "${S:?}"/*.zz`, false},
		{`S=/home/u/sb; echo a | read X; rm -f "${S:?}"/*.zz`, false},
		{`S=/home/u/sb; ls /home/u/sb | while read f; do :; done; rm -f "${S:?}"/*.zz`, false},
		{`S=/home/u/sb; X=$(ls $S | wc -l); rm -f "${S:?}"/*.zz`, false},
		{`S=/home/u/sb; ls -la $S | head -3; rm -f "${S:?}"/*.zz; ls $S`, false},
		{`false && rm -rf "${NOPE_Q:?}"/*`, false},
		{`S=/home/u/sb; ls $S | wc -l; rm -f /home/u/sb/*.zz`, false},
		{`S=/home/u/sb; ls $S | wc -l; rm -f $S/b.zz`, false},
		{`S=/home/u/sb; ls /home/u/sb | wc -l; rm -f $S/*.zz`, true},
		{`S=/home/u/sb; true | true; rm -f $S/*.zz`, true},
		{`S=/home/u/sb; true; rm -f $S/*.zz`, false},
	} {
		r := validateRm(t, tc.command)
		if r.IsValid == tc.deny {
			t.Errorf("deny=%v, ожидалось %v: %q %v", !r.IsValid, tc.deny, tc.command, r.Suggestions)
		}
	}
}

// TestRmGuard330Gen checks 40 generalized Claude Code 2.1.330 probes at CWD /home/u/proj.
func TestRmGuard330Gen(t *testing.T) {
	for _, tc := range []struct {
		command string
		deny    bool
	}{
		{`S=/home/u/sb; rm -f $S/*.zz; ls | wc -l`, false},
		{`S=/home/u/sb; ls | wc -l && rm -f "$S"/*.zz`, true},
		{`S=/home/u/sb; rm -f $S/*.zz | cat`, false},
		{`S=/home/u/sb; echo hi |& cat; rm -f $S/*.zz`, true},
		{`S=/home/u/sb; ls $S | wc -l; rm -rf $S/sub`, false},
		{`S=/home/u/sb; ls $S | wc -l; rm -f "$S"`, false},
		{`S=/home/u/sb; ls | wc -l; rmdir $S/*`, true},
		{`S=/home/u/sb; printf 'a|b\\n'; rm -f $S/*.zz`, false},
		{`S=/home/u/sb; echo 'x | y'; rm -f $S/*.zz`, false},
		{`S=/home/u/sb; ls | wc -l; rm -f ${S:?}/*.zz`, false},
		{`false && rm -rf "${NOPE_Q:-/home/nonexist-q}"/*`, false},
		{`false && rm -rf "${NOPE_Q:=$HOME}"/*`, true},
		{`false && rm -rf "${NOPE_Q:-$PWD}"/*`, true},
		{`false && rm -rf "${NOPE_Q:?x}/usr"`, false},
		{`false && rm -rf "${NOPE_Q^^}"/*`, true},
		{`false && rm -rf "${NOPE_Q,,}"/*`, true},
		{`false && rm -rf "${#NOPE_Q}"/*`, false},
		{`false && rm -rf "${!NOPE_Q}"/*`, false},
		{`false && rm -f $HOME/*.log`, false},
		{`false && rm -f "$HOME"/x/*`, false},
		{`false && rm -f ${HOME}/*`, true},
		// Здесь cwd /home/u/proj, а не каталог внутри ~/.cache, как в пробе.
		{`false && rm -rf $HOME/.cache/*`, false},
		{`cd ./a/b && rm -f c/*`, false},
		{`cd ./a && cd ./b && rm -f c/*`, false},
		{`cd . && cd ./a && rm -f c/*`, false},
		{`cd ./a && ls && rm -f c/*`, false},
		{`cd ./a && rm -f c/* d/*`, false},
		{`cd ./a && rm -f "c"/*`, false},
		{`cd './a' && rm -f c/*`, false},
		{`cd ./a && sudo rm -f c/*`, false},
		{`cd /home/u/sb/a && cd ./b && rm -f c/*`, true},
		{`cd /home/u/sb/a && cd /home/u/proj/b && rm -f c/*`, true},
		{`cd ./a && rm -f c/*.log && cd .. && rm -f d/*`, true},
		{`cd /home/u/proj/a/b && rm -f c/*`, false},
		{`cd /home/u/proj/a/../a && rm -f c/*`, true},
		{`cd /home/u/proj/a && cd ./b && rm -f c/*`, false},
		{`cd /home/u/proj/a && cd b && rm -f c/*`, false},
		{`cd /home/u/proj/a; cd /home/u/sb; rm -f c/*`, true},
		{`cd /home/u/proj/a && rm -rf ../a/c/*/`, true},
		{`cd /home/u/projx && rm -f c/*`, true},
		{`cd /home/u/proj/a && sudo rm -f c/*`, false},
	} {
		r := validateRm(t, tc.command)
		if r.IsValid == tc.deny {
			t.Errorf("deny=%v, ожидалось %v: %q %v", !r.IsValid, tc.deny, tc.command, r.Suggestions)
		}
	}
}

// TestRmGuard330ParameterOperators проверяет распознаваемые исходником формы.
func TestRmGuard330ParameterOperators(t *testing.T) {
	for _, tc := range []struct {
		command string
		deny    bool
	}{
		{`rm -f ${X@Q}/*`, false},
		{`rm -f ${X:?message}/*`, false},
		{`rm -f ${X:-literal}/*`, false},
		{`rm -f ${X:=literal}/*`, false},
		{`rm -f ${X:-/}/*`, true},
		{`rm -f ${X:=/./}/*`, true},
		{`rm -f ${X?message}/*`, true},
		{`rm -f ${X:+literal}/*`, true},
		{`rm -f ${X%/}/*`, true},
		{`rm -f ${X:0:3}/*`, true},
		{`X=literal; rm -f ${X%/}/*`, false},
		{`X=literal; true | true; rm -f ${X%/}/*`, true},
		{`X=literal; rm -f ${X%/}/* | cat`, false},
	} {
		r := validateRm(t, tc.command)
		if r.IsValid == tc.deny {
			t.Errorf("deny=%v, ожидалось %v: %q %v", !r.IsValid, tc.deny, tc.command, r.Suggestions)
		}
	}
}

func TestRmGuard330HomeWorkspace(t *testing.T) {
	tool := newRmGuard(t)
	for _, tc := range []struct {
		cwd  string
		deny bool
	}{
		{"/home/u/.cache/project", true},
		{"/home/u/proj", false},
	} {
		result := validateRmWith(t, tool, `false && rm -rf $HOME/.cache/*`, tc.cwd)
		if result.IsValid == tc.deny {
			t.Errorf("cwd=%s: deny=%v, ожидалось %v", tc.cwd, !result.IsValid, tc.deny)
		}
	}
	const sandboxHome = "/home/aisee/.cache/claude-tmp/rmcc330/home"
	// HOME приходит из контекста запроса: ValidateTool берёт env оттуда
	rmGuardEnv["HOME"] = sandboxHome
	t.Cleanup(func() { rmGuardEnv["HOME"] = "/home/u" })
	r := validateRmWith(t, tool, `false && rm -rf $HOME/.cache/*`, sandboxHome+"/.cache/w")
	if r.IsValid {
		t.Error("удаление предка cwd песочницы должно отклоняться")
	}
}

// TestRmGuard330SingleCdChainAndGlobs проверяет три условия из g0 и dIe.
func TestRmGuard330SingleCdChainAndGlobs(t *testing.T) {
	for _, tc := range []struct {
		command string
		deny    bool
	}{
		{`cd /home/u/proj/build && rm -f logs/*; echo done`, true},
		{`cd ./build && rmdir cache/*; printf done`, true},
		{`cd ./build && rm -f logs/* && echo done`, false},
		{`cd /home/u/proj/build; rm -rf logs/*/`, true},
		{`cd /home/u/proj/work; rmdir cache/*/`, true},
		{`cd /home/u/proj/build; rm -rf logs/*`, true},
		{`cd /home/u/proj/build && rm -rf logs/*/`, true},
		{`cd . && rm -f */logs/*`, true},
		{`cd . && rmdir ?/cache/*`, true},
		{`cd . && rm -f */logs`, false},
		{`cd . && rm -f logs/*.txt`, false},
	} {
		r := validateRm(t, tc.command)
		if r.IsValid == tc.deny {
			t.Errorf("deny=%v, ожидалось %v: %q %v", !r.IsValid, tc.deny, tc.command, r.Suggestions)
		}
	}
}

// TestRmGuardCdGlobStricter сохраняет намеренно строгие формы, не входившие
// в таблицу вердиктов Claude Code 2.1.330.
func TestRmGuardCdGlobStricter(t *testing.T) {
	for _, command := range []string{
		`cd /home/u/sb && ls | head -1; rm -f cl/*`,
		`ls | head -1 && cd /home/u/sb && rm -f cl/*`,
		`cd /home/u/sb | true; rm -f a/*`,
		`cd /home/u/sb && ls cl | wc -l && rm -f cl/*`,
		`cd /home/u/sb && echo a | grep -q a; rm -f cl/*`,
		`echo a |& cat; cd /home/u/sb && rm -f cl/*`,
		`X=$(ls | head -1); cd /home/u/sb && rm -f cl/*`,
		`cd /home/u/sb && echo a | cat && rm -f cl/*`,
		`echo a | cat; cd /home/u/sb; rm -f cl/*`,
		"cd /home/u/sb; echo a | cat\nrm -f cl/*",
		`echo a | cat; cd /home/u/sb && rm -f cl/*; for t in a; do :; done`,
		`echo a | cat; cd /home/u/sb && rm -f cl/*; X=1`,
		`for d in /home/u/sb/a /home/u/sb/b; do cd $d; rm -rf *; done`,
		`for d in /home/u/sb/a; do cd $d && rm -rf *; done`,
		`for d in /home/u/sb/a /home/u/sb/b; do cd $d; rm -rf *; done; echo ${HOME}`,
		`for d in /home/u/sb/a /home/u/sb/b; do cd $d; rm -rf cl/*; done`,
		`for d in /home/u/sb/a; do cd "$d"; rm -rf *; done`,
		`for d in /home/u/sb/a; do cd $d/; rm -rf *; done`,
		`while read d; do cd $d; rm -rf *; done < /dev/null`,
	} {
		if validateRm(t, command).IsValid {
			t.Errorf("должно отклоняться: %q", command)
		}
	}
}

// TestRmGuardCdGlobReason причина ведёт к литеральному абсолютному пути
func TestRmGuardCdGlobReason(t *testing.T) {
	r := validateRm(t, `cd /home/u/sb && rm -f cl/* && ls cl`)
	if r.IsValid {
		t.Fatal("должно отклоняться")
	}
	if !strings.Contains(r.Violations[0].Message, "после cd") {
		t.Errorf("заголовок: %q", r.Violations[0].Message)
	}
	want := "`rm -f cl/*`: после cd каталог глоба cl/* заранее не известен, укажи литеральный абсолютный путь: `rm -f /полный/путь/cl/*`"
	if got := strings.Join(r.Suggestions, "\n"); got != want {
		t.Errorf("\nесть  %q\nждали %q", got, want)
	}
}

func rmTestContext() context.Context {
	env := make([]string, 0, len(rmGuardEnv))
	for k, v := range rmGuardEnv {
		env = append(env, k+"="+v)
	}
	return hookctx.WithRequest(context.Background(), hookctx.New(nil, nil, nil, env, "/home/u/proj", 1, 2))
}
