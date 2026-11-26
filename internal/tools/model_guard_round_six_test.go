package tools

import "testing"

func TestModelGuardRoundSixWholeCommand(t *testing.T) {
	tool := modelGuardForTest(t, "opus claude-opus-5-5\n")
	runClientOptionCases(t, tool, []clientOptionCase{
		{"p01", "claude -p --model opus hi; export CLAUDE_CODE_SIMPLE=1; tmux new -d -s s 'npx @anthropic-ai/claude-code -p --model opus hi'", false, ""},
		{"p02", "claude -p --model opus hi; export CLAUDE_CODE_SIMPLE=1; tmux new -d -s s 'bunx @anthropic-ai/claude-code -p --model opus hi'", false, ""},
		{"p04", "B=claude; claude -p --model opus hi; export CLAUDE_CODE_SIMPLE=1; $B -p --model opus hi", false, ""},
		{"p06", "claude -p --model opus hi; export CLAUDE_CODE_SIMPLE=1; printf '%s\\n' 'claude -p --model opus hi' | bash", false, ""},
		{"p07", "claude -p --model opus hi; export CLAUDE_CODE_SIMPLE=1; cat /tmp/run-claude.sh | bash", false, ""},
		{"p17", "f() { bunx @anthropic-ai/claude-code -p --model opus hi; }; claude -p --model opus hi; export CLAUDE_CODE_SIMPLE=1; f", false, ""},
		{"p20", "claude -p --model opus hi; export CLAUDE_CODE_SIMPLE=1; X=claude; $X -p --model opus hi", false, ""},
		{"q01", "claude -p --model opus hi; export CLAUDE_CODE_SIMPLE=1; echo done", false, ""},
		{"q02", "claude -p --model opus hi; export HOME=/tmp/x; ls ~/.claude", false, ""},
		{"q03", "claude -p --model opus hi; export CLAUDE_CODE_SIMPLE=1; grep claude /tmp/log", false, ""},
		{"q04", "claude -p --model opus hi > /tmp/o; export CLAUDE_CODE_SIMPLE=1; cat /tmp/o | grep claude", false, ""},
		{"q05", "claude -p --model opus hi; export CLAUDE_CODE_SIMPLE=1; git log --grep claude", false, ""},
		{"q06", "claude -p --model opus hi; export CLAUDE_CODE_SIMPLE=1; ls ~/.claude/agents", false, ""},
		{"q07", "claude -p --model opus hi; export CLAUDE_CODE_SIMPLE=1; jq . ~/.claude/settings.json", false, ""},
		{"q08", "claude -p --model opus hi; source /tmp/env.sh; wc -l /tmp/claude-out.txt", false, ""},
		{"agents escaped apostrophe", `claude -p --model opus --agents $'{"x":{"prompt":"it\'s","model":"opus"}}' hi`, false, "двойных кавычках"},
		{"agents apostrophe in double quotes", `claude -p --model opus --agents "{\"x\":{\"prompt\":\"it's\",\"model\":\"opus\"}}" hi`, true, ""},
	})
}

func TestRoundSixEnvironmentHazardWholeCommand(t *testing.T) {
	for _, tc := range []struct {
		command string
		want    bool
	}{
		{`export "$N=1"; claude -p --model opus hi`, true},
		{`claude -p --model opus hi; export "$N=1"`, true},
		{`echo hi; export "$N=1"`, false},
	} {
		got, err := environmentHazard(tc.command, true)
		if err != nil || got != tc.want {
			t.Errorf("%q: hazard=%v err=%v, want %v", tc.command, got, err, tc.want)
		}
	}
}

func TestRoundSixEnvironmentControlWholeCommand(t *testing.T) {
	for _, command := range []string{
		`C=true; read C < /tmp/x; $C; claude -p --model opus hi`,
		`claude -p --model opus hi; C=true; read C < /tmp/x; $C`,
	} {
		if !environmentControlUnknown(command) {
			t.Errorf("%q: command name is unknown after read", command)
		}
	}
}

func TestRoundSixEnvironmentControlParseError(t *testing.T) {
	if !environmentControlUnknown("claude; export '") {
		t.Fatal("an unparsed command must have unknown environment control")
	}
}

func TestRoundSixTmuxClientWithExpandedArguments(t *testing.T) {
	command := `tmux new-session -d "env HOME=$H CLAUDE_CONFIG_DIR=$H/.claude bash" && tmux send-keys "$R/cl -p hi" Enter`
	tool := modelGuardForTest(t, "opus claude-opus-5-5\n")
	if result := modelGuardTmuxVerdict(t, tool, command, t.TempDir()); result.IsValid {
		t.Fatal("tmux session environment from variables allowed")
	}
}
