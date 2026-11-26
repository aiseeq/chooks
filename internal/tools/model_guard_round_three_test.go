package tools

import (
	"slices"
	"strings"
	"testing"

	"github.com/aiseeq/akit/shellcmd"
	"github.com/aiseeq/chooks/internal/hookctx"
	"mvdan.cc/sh/v3/syntax"
)

func TestRoundThreeEscapedBuiltin(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{{`\printf -v X x`, "printf"}, {`r\ead X`, "read"}} {
		file, err := syntax.NewParser().Parse(strings.NewReader(tc.raw), "")
		if err != nil {
			t.Fatal(err)
		}
		call := file.Stmts[0].Cmd.(*syntax.CallExpr)
		got, known := shellEffectiveCommand(call.Args)
		if !known || got != tc.want {
			t.Errorf("%q: got %q known=%v, want %q", tc.raw, got, known, tc.want)
		}
	}
}

func TestRoundThreeANSINul(t *testing.T) {
	for _, raw := range []string{`--bare\c@x`, `--bare\c x`, `--bare\c` + "`" + `x`} {
		if _, err := shellANSIValue(raw); err == nil {
			t.Errorf("%q must be refused", raw)
		}
	}
	if value, err := shellANSIValue(`literal\\c@`); err != nil || value != `literal\c@` {
		t.Fatalf("escaped backslash: value=%q err=%v", value, err)
	}
}

func TestModelGuardRoundThreeImplicitWrites(t *testing.T) {
	tool := modelGuardForTest(t, "opus claude-opus-5-5\n")
	runClientOptionCases(t, tool, []clientOptionCase{
		{"Q50", "X=foo; builtin printf -v X %s --bare; claude -p --model opus \"$X\" hi", false, ""},
		{"Q51", "X=foo; command printf -v X %s --bare; claude -p --model opus \"$X\" hi", false, ""},
		{"Q52", "X=foo; : ${X:=--bare}; claude -p --model opus \"$X\" hi", false, ""},
		{"Q53", "cd /tmp && X=foo; builtin read -r X <<< --bare; claude -p --model opus \"$X\" hi", false, ""},
		{"Q54", "X=foo; \\printf -v X %s --bare; claude -p --model opus \"$X\" hi", false, ""},
		{"Q56", "X=foo; command -p read -r X <<< --bare; claude -p --model opus \"$X\" hi", false, ""},
		{"Q57", "cd /tmp && X=foo && builtin read -r X <<< --bare && claude -p --model opus \"$X\" hi", false, ""},
		{"Q60", "X=foo; exec 3<<< --bare; builtin read -u 3 X; claude -p --model opus \"$X\" hi", false, ""},
		{"R60", "X=foo; builtin printf -v X %s --bare; claude -p --model opus --name \"$X\" hi", false, ""},
		{"R61", "X=foo; command read -r X <<< --bare; claude -p --model opus \"$X\" hi", false, ""},
		{"R62", "X=foo; r\\ead -r X <<< --bare; claude -p --model opus \"$X\" hi", false, ""},
		{"R63", "cd /tmp && X=foo; : ${X:=x}; claude -p --model opus \"$X\" hi", false, ""},
		{"R64", "X=foo; : ${X:=--bare}; claude -p --model opus --name \"$X\" hi", false, ""},
		{"Q40", "FUNCNAME=foo; function --bare { claude -p --model opus \"$FUNCNAME\" hi; }; --bare", false, ""},
	})
}

func TestModelGuardRoundFourDeclarations(t *testing.T) {
	tool := modelGuardForTest(t, "opus claude-opus-5-5\n")
	runClientOptionCases(t, tool, []clientOptionCase{
		{"I36", `X=ok; builtin declare X=--bare; claude -p --model opus "$X" hi`, false, ""},
		{"I37", `X=ok; command export X=--bare; claude -p --model opus "$X" hi`, false, ""},
		{"I44", `X=ok; \export X=--bare; claude -p --model opus "$X" hi`, false, ""},
		{"I45", `X=ok; \declare X=--bare; claude -p --model opus "$X" hi`, false, ""},
		{"D07", `X=ok; \readonly X=--bare; claude -p --model opus "$X" hi`, false, ""},
		{"D08", `X=ok; \typeset X=--bare; claude -p --model opus "$X" hi`, false, ""},
		{"D09", `X=ok; f() { \local X=--bare; claude -p --model opus "$X" hi; }; f`, false, ""},
		{"D12", `\export PATH=/usr/bin:$PATH; claude -p --model opus hi`, true, ""},
		{"computed name", `export "$N=1"; claude -p --model opus hi`, false, ""},
	})
}

func TestModelGuardRoundFourMutationCases(t *testing.T) {
	tool := modelGuardForTest(t, "opus claude-opus-5-5\n")
	runClientOptionCases(t, tool, []clientOptionCase{
		{"M01", `tmux new -d -c /tmp 'HOME=/tmp/x claude -p --model opus hi'`, false, ""},
		{"M08", `tmux setenv -g "$N" /tmp/x; tmux new -d 'claude -p --model opus hi'`, false, ""},
		{"M09", `tmux -L s new -d -e HOME=/tmp/x 'claude -p --model opus hi'`, false, ""},
		{"M12", `X=ok; if true; then G=ls; fi; $G; claude -p --model opus hi`, false, ""},
		{"M04", `tmux new -d 'env -u HOME claude -p --model opus hi'`, false, ""},
		{"M05", `env -S 'bash -c claude' -p --model opus hi`, false, ""},
		{"M06", `env -u HOME claude -p --model opus hi`, false, ""},
		{"unset attached", `env -uHOME claude -p --model opus hi`, false, "env -u"},
		{"unset long attached", `env --unset=HOME claude -p --model opus hi`, false, "env -u"},
		{"unset safe attached", `env -uANTHROPIC_API_KEY claude -p --model opus hi`, true, ""},
		{"M07", `tmux new -d -e LD_PRELOAD=/tmp/x.so 'claude -p --model opus hi'`, false, ""},
		{"session chdir unsafe", `tmux new -d 'env -C /tmp HOME=/tmp/x claude -p --model opus hi'`, false, ""},
		{"session chdir safe", `tmux new -d 'env -C /tmp FOO=1 claude -p --model opus hi'`, true, ""},
		{"session chdir unset home", `tmux new -d 'env -C /tmp -u HOME claude -p --model opus hi'`, false, ""},
		{"session chdir unknown", `tmux new -d 'env -C "$D" HOME=/tmp/x claude -p --model opus hi'`, false, ""},
		{"environment parse error", `echo '<<'; claude -p --model opus hi <<EOF`, false, "окружение клиента claude"},
	})
}

func TestRoundFourEnvOptionValues(t *testing.T) {
	for _, option := range []string{"-C", "--chdir", "-u", "--unset"} {
		args := []envArgument{{option, true}, {"$VALUE", false}, {"claude", true}}
		if !envArgumentsHazard(args, true) {
			t.Errorf("%s with an unknown value must be refused", option)
		}
	}
}

func TestModelGuardRoundThreeSafeCommands(t *testing.T) {
	tool := modelGuardForTest(t, "opus claude-opus-5-5\n")
	runClientOptionCases(t, tool, []clientOptionCase{
		{"F01", "env -u ANTHROPIC_API_KEY claude -p --model opus hi", true, ""},
		{"F05", "timeout 60 bash -c 'true'; claude -p --model opus hi", true, ""},
		{"F08", "tmux new-session -d -s w -c /tmp 'claude -p --model opus hi' && tmux send-keys -t w Enter", true, ""},
		{"F10", "env -u CLAUDE_CODE_ENTRYPOINT -u CLAUDECODE claude -p --model opus hi", true, ""},
		{"G10", "timeout 60 sh -c 'echo x'; claude -p --model opus hi", true, ""},
		{"G15", "nohup bash -c 'sleep 1' & claude -p --model opus hi", true, ""},
		{"G16", "env -u ANTHROPIC_API_KEY -u ANTHROPIC_BASE_URL claude -p --model opus hi", true, ""},
		{"G20", "xargs -I{} bash -c 'echo {}' < /dev/null; claude -p --model opus hi", true, ""},
		{"H01", "G=ls; $G /tmp; claude -p --model opus hi", true, ""},
		{"H02", "B=/usr/bin/python3; \"$B\" -c 'print(1)'; claude -p --model opus hi", true, ""},
		{"H05", "G=git; f() { :; }; $G status; claude -p --model opus hi", true, ""},
		{"safe split", `env -S 'FOO=1 claude -p --model opus hi'`, true, ""},
		{"split rejects model", `env -S 'FOO=1 claude -p --model haiku hi'`, false, "haiku"},
	})
}

func TestModelGuardRoundThreeANSIWrappers(t *testing.T) {
	tool := modelGuardForTest(t, "opus claude-opus-5-5\n")
	runClientOptionCases(t, tool, []clientOptionCase{
		{"R01", "timeout 5 claude -p --model opus --name $'--b\\x61re' hi", false, ""},
		{"R02", "nohup claude -p --model opus --name $'--b\\x61re' hi", false, ""},
		{"R03", "command claude -p --model opus --name $'--b\\x61re' hi", false, ""},
		{"R04", "\\claude -p --model opus --name $'--b\\x61re' hi", false, ""},
		{"R05", "env claude -p --model opus --name $'--b\\x61re' hi", false, ""},
		{"R07", "timeout 5 claude -p --model opus --name $'--b\\x61re'\"\" hi", false, ""},
		{"R10", "cd /tmp && timeout 5 claude -p --model opus --name $'--b\\x61re' hi", false, ""},
		{"R11", "\"$(command -v claude)\" -p --model opus --name $'--b\\x61re' hi", false, ""},
		{"R13", "timeout 5 claude -p --model opus --name $'--safe-mod\\x65' hi", false, ""},
		{"R14", "timeout 5 claude -p --model opus --name $'--restricte\\x64' hi", false, ""},
		{"Q08", "env FOO=1 claude -p --model opus --name $'--b\\x61re' hi", false, ""},
		{"Q13", "timeout 5 claude -p --model opus --debug-file $'/tmp/x\\x2f../../srv/hook' hi", false, ""},
		{"R09", "claude -p --model opus --name $'--bare\\c@' hi", false, ""},
		{"Q30", "claude -p --model opus --name $'--bare\\c@x' hi", false, ""},
		{"H06", "claude -p --model opus --name $'--bare\\c x' hi", false, ""},
	})
}

func TestModelGuardRoundThreeEnvironmentOrder(t *testing.T) {
	tool := modelGuardForTest(t, "opus claude-opus-5-5\n")
	runClientOptionCases(t, tool, []clientOptionCase{
		{"K01", "tmux new -d -s s \"env HOM\"\"E=/tmp/x bash\" && tmux send-keys -t s 'claude' Enter", false, ""},
		{"K02", "tmux new -d -s s 'env HOM''E=/tmp/x bash' && tmux send-keys -t s claude Enter", false, ""},
		{"K03", "V=HOME=/tmp/x; tmux new -d -s s \"env $V bash\" && tmux send-keys -t s claude Enter", false, ""},
		{"K05", "tmux new -d -s s 'env CLAUDE_CODE_SIMPLE=1 bash' && tmux send-keys -t s claude Enter", false, ""},
		{"Q76", "f() { claude -p --model opus hi; }; N=HOME; export $N=/tmp/x; f", false, ""},
		{"R30", "for i in 1 2; do claude -p --model opus hi; N=HOME; export $N=/tmp/x; done", false, ""},
		{"R31", "f() { claude -p --model opus hi; }; N=CLAUDE_CODE_SIMPLE; declare -x $N=1; f", false, ""},
		{"R33", "f() { claude -p --model opus hi; }; N=HOME; printf -v $N /tmp/x; f", false, ""},
		{"R34", "f() { claude -p --model opus hi; }; N=HOME; read -r $N <<< /tmp/x; f", false, ""},
		{"R36", "f() { claude -p --model opus hi; }; export $(echo HOME)=/tmp/x; f", false, ""},
		{"Q80", "tmux set-e -g HOME /tmp/x; tmux new -d 'claude -p --model opus hi'", false, ""},
		{"Q81", "tmux new -de CLAUDE_CODE_SIMPLE=1 'claude -p --model opus hi'", false, ""},
		{"Q83", "\\tmux new -d -e HOME=/tmp/x 'claude -p --model opus hi'", false, ""},
		{"Q84", "command tmux new -d -e HOME=/tmp/x 'claude -p --model opus hi'", false, ""},
		{"R41", "tmux set-env -g HOME /tmp/x; tmux new -d 'claude -p --model opus hi'", false, ""},
		{"R50", "timeout 5 tmux new -d -e HOME=/tmp/x 'claude -p --model opus hi'", false, ""},
		{"R56", "command env -S 'CLAUDE_CODE_SIMPLE=1 claude -p --model opus hi'", false, ""},
		{"R57", "\\env -S 'CLAUDE_CODE_SIMPLE=1 claude -p --model opus hi'", false, ""},
		{"G18", "tmux new -d -s x -e TERM=xterm 'claude --model opus'", true, ""},
		{"G19", "tmux new -d -s s -e FOO=1 'claude -p --model opus hi'", true, ""},
		// переменная не из списка известных: отказ, как у прямой формы (new:F04)
		{"T06", "tmux new -d -s s -e CLAUDE_CODE_EFFORT_LEVEL=high 'claude --model opus'", false, "не в списке известных"},
		{"T01", "tmux capture-pane -p -e -t s; tmux send-keys -t s 'claude -p --model opus hi' Enter", true, ""},
		{"nested timeout env", `tmux new -d 'timeout 5 env HOME=/tmp/x claude -p --model opus hi'`, false, ""},
		{"nested shell env", `tmux new -d 'bash -c "env HOME=/tmp/x claude -p --model opus hi"'`, false, ""},
		{"safe nested timeout", `tmux new -d 'timeout 5 env FOO=1 claude -p --model opus hi'`, true, ""},
	})
}

func TestModelGuardRoundThreeEnvironmentAdvice(t *testing.T) {
	tool := modelGuardForTest(t, "opus claude-opus-5-5\n")
	runClientOptionCases(t, tool, []clientOptionCase{
		{"Q85", `tmux new -d 'env HOME=/tmp/x claude -p --model opus hi'`, false, "переменная HOME у клиента claude"},
		{"K04", `tmux new -d -s s "env HOME=/tmp/x bash" && tmux send-keys -t s claude Enter`, false, "переменная HOME у клиента claude"},
	})
}

func TestRoundThreeConditionalAssignment(t *testing.T) {
	for _, tc := range []struct {
		command string
		want    bool
	}{
		{"X=safe && true", true},
		{"X=safe || true", false},
		{"X=safe external-command", false},
	} {
		file, err := syntax.NewParser().Parse(strings.NewReader(tc.command), "")
		if err != nil {
			t.Fatal(err)
		}
		if got := assignmentOnSuccess(file.Stmts[0], "X"); got != tc.want {
			t.Errorf("%q: got %v, want %v", tc.command, got, tc.want)
		}
	}
}

func TestRoundThreeEscapedGlob(t *testing.T) {
	for _, tc := range []struct {
		word string
		want bool
	}{{`a\*b`, false}, {`a*b`, true}, {`a\?b`, false}, {`a[b`, true}} {
		if got := shellUnquotedGlob(tc.word); got != tc.want {
			t.Errorf("%q: got %v, want %v", tc.word, got, tc.want)
		}
	}
}

func TestRoundThreeUnknownCommandMayWriteValue(t *testing.T) {
	for _, command := range []string{
		`X=foo; "$G"; claude -p --model opus "$X"`,
		`X=foo; \printf -v X %s --bare; claude -p --model opus "$X"`,
		`X=foo; r\ead -r X <<< --bare; claude -p --model opus "$X"`,
		`X=foo; : ${X:=--bare}; claude -p --model opus "$X"`,
	} {
		values := clientWordValues(command, hookctx.Context{}, nil)
		if got, known := values("X"); known {
			t.Errorf("%q: command may change X, got known values %q", command, got)
		}
	}
}

func TestRoundThreeEnvironmentParserBoundaries(t *testing.T) {
	command := `echo claude; export "$N=1"`
	if hazard, err := environmentHazard(command, false); err != nil || hazard {
		t.Fatalf("no client: hazard=%v, err=%v", hazard, err)
	}
	if hazard, err := environmentHazard(command, true); err != nil || !hazard {
		t.Fatalf("whole command: hazard=%v, err=%v", hazard, err)
	}
	if !slices.Contains(clientEnvironmentNames(`echo $'CLAUDE_CODE_\x53IMPLE'`), "CLAUDE_CODE_SIMPLE") {
		t.Fatal("escaped environment name was not decoded")
	}
}

func TestRoundThreeAssignmentAndReadonlyBoundaries(t *testing.T) {
	file, err := syntax.NewParser().Parse(strings.NewReader("readonly X=foo"), "")
	if err != nil {
		t.Fatal(err)
	}
	if !hasDynamicValueWrite(file) {
		t.Fatal("readonly must make conditional values uncertain")
	}
	if assignmentBeforeEveryClient(file, "readonly X=foo", "X") {
		t.Fatal("no client is not proof that the assignment dominates a client")
	}
	const computed = `X=$Y; claude -p --model opus "$X"`
	file, err = syntax.NewParser().Parse(strings.NewReader(computed), "")
	if err != nil {
		t.Fatal(err)
	}
	if literalLoopOnly(file, computed, "X", nil) {
		t.Fatal("computed assignment is not a known literal alternative")
	}
}

func TestRoundThreeUnsetEnvironmentReason(t *testing.T) {
	const command = `unset HOME; claude -p --model opus hi`
	calls, err := shellcmd.Calls(command)
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := unknownEnvironmentCandidates(command, calls, []string{"HOME"})
	if err != nil || len(candidates) != 1 || !strings.Contains(candidates[0].clientOption, "снимает переменную HOME") {
		t.Fatalf("unset HOME: candidates=%v err=%v", candidates, err)
	}
}

func TestRoundFourSourceAfterClient(t *testing.T) {
	const command = `$G; claude -p --model opus hi; source /tmp/env.sh`
	calls, err := shellcmd.Calls(command)
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := unknownEnvironmentCandidates(command, calls, []string{"HOME"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(candidates, func(candidate modelCandidate) bool {
		return strings.Contains(candidate.clientOption, "source или eval")
	}) {
		t.Fatalf("source after client must be checked: %v", candidates)
	}
}

func TestRoundFourConditionalDominance(t *testing.T) {
	for _, tc := range []struct {
		command string
		want    bool
	}{
		{`X=ok && claude -p --model opus "$X"`, true},
		{`X=ok && true; claude -p --model opus "$X"`, false},
		{`claude -p --model opus "$X"; X=ok && true`, false},
	} {
		file, err := syntax.NewParser().Parse(strings.NewReader(tc.command), "")
		if err != nil {
			t.Fatal(err)
		}
		if got := assignmentBeforeEveryClient(file, tc.command, "X"); got != tc.want {
			t.Errorf("%q: dominates=%v, want %v", tc.command, got, tc.want)
		}
	}
}

func TestRoundFourWrappedChildClient(t *testing.T) {
	for _, tc := range []struct {
		command string
		want    bool
	}{
		{`timeout 5 bash -c 'true'`, true},
		{`timeout 5 bash -c 'claude -p --model opus hi'`, false},
	} {
		file, err := syntax.NewParser().Parse(strings.NewReader(tc.command), "")
		if err != nil {
			t.Fatal(err)
		}
		call := file.Stmts[0].Cmd.(*syntax.CallExpr)
		if got := wrappedChildShell(tc.command, call); got != tc.want {
			t.Errorf("%q: child-only=%v, want %v", tc.command, got, tc.want)
		}
	}
}

func TestRoundFourShellWrappersAreNotExternal(t *testing.T) {
	for _, name := range []string{"builtin", "command"} {
		if shellSafeExternal(name) {
			t.Errorf("%q can execute shell builtins and must not be classified as external", name)
		}
	}
}

func TestRoundThreeConditionalValueAlternatives(t *testing.T) {
	const command = `false && X=safe; claude -p --model opus "$X"`
	for _, tc := range []struct {
		env  []string
		want string
	}{
		{[]string{"X=--bare"}, "--bare"},
		{nil, ""},
	} {
		values := clientWordValues(command, hookctx.Context{Env: tc.env}, nil)
		got, known := values("X")
		if !known || !slices.Contains(got, tc.want) {
			t.Errorf("env %q: got values %q known=%v, want alternative %q", tc.env, got, known, tc.want)
		}
	}
}

func TestRoundThreeANSIArgumentPositions(t *testing.T) {
	for _, command := range []string{
		`claude --name $'\c@'$'ok'`,
		`npx @anthropic-ai/claude-code --name $'\c@'`,
	} {
		calls, err := shellcmd.Calls(command)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for i := range calls {
			_, words, client := clientCallWords(&calls[i])
			if client && slices.ContainsFunc(words, func(w word) bool { return w.invalidANSI }) {
				found = true
			}
		}
		if !found {
			t.Errorf("%q: NUL escape lost from argument metadata", command)
		}
	}
	const command = `claude -p hi; claude --name $'--bare\c@'`
	calls, err := shellcmd.Calls(command)
	if err != nil || len(calls) != 2 {
		t.Fatalf("calls: %v, err=%v", calls, err)
	}
	calls[1].Raw = command
	_, ansi := shellCallArgumentMetadata(&calls[1])
	if len(ansi) != 2 || !ansi[1].invalid {
		t.Fatalf("second client lost its argument metadata: %+v", ansi)
	}
}
