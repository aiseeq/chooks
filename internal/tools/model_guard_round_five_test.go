package tools

import "testing"

func TestModelGuardRoundFiveLaterClients(t *testing.T) {
	tool := modelGuardForTest(t, "opus claude-opus-5-5\n")
	runClientOptionCases(t, tool, []clientOptionCase{
		{"n03", "claude -p --model opus hi; export CLAUDE_CODE_SIMPLE=1; npx @anthropic-ai/claude-code -p --model opus hi", false, ""},
		{"n05", "claude -p --model opus hi; read HOME; npx @anthropic-ai/claude-code -p --model opus hi", false, ""},
		{"n06", "claude -p --model opus hi; \\export HOME=/tmp/x; npx -y @anthropic-ai/claude-code -p --model opus hi", false, ""},
		{"n10", "claude -p --model opus hi; source /tmp/unknown-env.sh; npx @anthropic-ai/claude-code -p --model opus hi", false, ""},
		{"l10", "claude -p --model opus hi; export CLAUDE_CODE_SIMPLE=1; bunx @anthropic-ai/claude-code -p --model opus hi", false, ""},
		{"l12", "cl -p --model opus hi; export CLAUDE_CODE_SIMPLE=1; npx @anthropic-ai/claude-code -p --model opus hi", false, ""},
		{"l14", "claude --version; export CLAUDE_CODE_SIMPLE=1; npx @anthropic-ai/claude-code -p --model opus hi", false, ""},
		{"l15", "claude -p --model opus hi; CLAUDE_CODE_SIMPLE=1; export CLAUDE_CODE_SIMPLE; npx @anthropic-ai/claude-code -p --model opus hi", false, ""},
		{"l17", "claude -p --model opus hi; export CLAUDE_CODE_SIMPLE=1; timeout 60 npx @anthropic-ai/claude-code -p --model opus hi", false, ""},
		{"l18", "claude -p --model opus hi; export CLAUDE_CODE_SIMPLE=1; env npx @anthropic-ai/claude-code -p --model opus hi", false, ""},
		{"l19", "claude -p --model opus hi; export CLAUDE_CODE_SIMPLE=1; node /usr/lib/node_modules/@anthropic-ai/claude-code/cli.js -p --model opus hi", false, ""},
		{"l21", "claude -p --model opus hi; eval \"$(cat /tmp/x)\"; npx @anthropic-ai/claude-code -p --model opus hi", false, ""},
		{"l22", "claude -p --model opus hi; printf -v HOME %s /tmp/x; npx @anthropic-ai/claude-code -p --model opus hi", false, ""},
		{"k06", "claude -p --model opus hi; export CLAUDE_CODE_SIMPLE=1; npx @anthropic-ai/claude-code@latest -p --model opus hi", false, ""},
		{"k07", "claude -p --model opus hi; export CLAUDE_CODE_SIMPLE=1; npx -y @anthropic-ai/claude-code -p --model opus hi", false, ""},
		{"k09", "claude -p --model opus hi; export CLAUDE_CODE_SIMPLE=1; npx --yes @anthropic-ai/claude-code -p --model opus hi", false, ""},
	})
}

func TestModelGuardRoundFiveReviewMutations(t *testing.T) {
	tool := modelGuardForTest(t, "opus claude-opus-5-5\n")
	runClientOptionCases(t, tool, []clientOptionCase{
		{"t02a", "claude -p --model opus hi; N=HOME; declare -x \"$N=/tmp/x\"; npx @anthropic-ai/claude-code -p --model opus hi", false, ""},
		{"t02b", "claude -p --model opus hi; env --unset=HOME npx @anthropic-ai/claude-code -p --model opus hi", false, ""},
		{"t06", "env -S \"$X\" claude -p --model opus hi", false, ""},
		{"t08", "env -uHOME claude -p --model opus hi", false, ""},
		{"t15", "X=read; $X HOME < /tmp/x; claude -p --model opus hi", false, ""},
		{"t21", "C=true; read C < /tmp/x; $C; claude -p --model opus hi", false, ""},
	})
}

func TestRoundFiveEnvironmentHazardAfterNpx(t *testing.T) {
	for _, command := range []string{
		`claude -p --model opus hi; N=HOME; declare -x "$N=/tmp/x"; npx @anthropic-ai/claude-code -p --model opus hi`,
		`claude -p --model opus hi; env --unset=HOME npx @anthropic-ai/claude-code -p --model opus hi`,
	} {
		if hazard, err := environmentHazard(command, true, "/home/aisee"); err != nil || !hazard {
			t.Errorf("%q: hazard=%v err=%v", command, hazard, err)
		}
	}
}

func TestRoundFiveUncertainEnvOptions(t *testing.T) {
	for _, args := range [][]envArgument{
		{{"-S", true}, {"$X", false}, {"claude", true}},
		{{"-S", true}, {"claude -p --model opus hi", false}},
		{{"-uHOME", true}, {"claude", true}},
	} {
		if !envArgumentsHazard(args, false) {
			t.Errorf("env args %+v must be refused", args)
		}
	}
	if shellSafeExternal("read") {
		t.Fatal("read writes shell variables and is not an external command")
	}
}

func TestRoundFiveKnownCommandsInvalidated(t *testing.T) {
	for _, command := range []string{
		`C=true; read C < /tmp/x; $C; claude -p --model opus hi`,
		`claude -p --model opus hi; C=true; read C < /tmp/x; $C; npx @anthropic-ai/claude-code -p --model opus hi`,
	} {
		if !environmentControlUnknown(command) {
			t.Errorf("%q: read invalidates the previously known command name", command)
		}
	}
}
