package tools

import "testing"

func TestModelGuardDynamicEnvironmentReview(t *testing.T) {
	tool := modelGuardForTest(t, "opus claude-opus-5-5\n")
	runClientOptionCases(t, tool, []clientOptionCase{
		{"W01", "env -i -S 'CLAUDE_CODE_SIMPLE=1 claude -p --model opus hi'", false, ""},
		{"W02", "env --split-string='CLAUDE_CODE_SIMPLE=1 claude -p --model opus hi'", false, ""},
		{"W03", "env -S\"CLAUDE_CODE_SIMPLE=1 claude -p --model opus hi\"", false, ""},
		{"W04", "env -u Y $(cat /tmp/envf) claude -p --model opus hi", false, ""},
		{"W05", "env `cat /tmp/envf` claude -p --model opus hi", false, ""},
		{"W06", "V=$(cat /tmp/envf); env $V claude -p --model opus hi", false, ""},
		{"W07", "env \"$(cat /tmp/envf)\" claude -p --model opus hi", false, ""},
		{"W08", "tmux new-session -d -eCLAUDE_CODE_SIMPLE=1 'claude -p --model opus hi'", false, ""},
		{"W10", "tmux new-session -d -e\"$(cat /tmp/envf)\" 'claude -p --model opus hi'", false, ""},
		{"W11", "tmux set-environment -g CLAUDE_CODE_SIMPLE 1; tmux new-session -d 'claude -p --model opus hi'", false, ""},
		{"W20", "f(){ local -x \"C${X}_CODE_SIMPLE=1\"; claude -p --model opus hi; }; X=LAUDE; f", false, ""},
		{"W21", "X=LAUDE; export -- \"C${X}_CODE_SIMPLE=1\"; claude -p --model opus hi", false, ""},
		{"W22", "X=LAUDE; declare -x -- \"C${X}_CODE_SIMPLE=1\"; claude -p --model opus hi", false, ""},
		{"W23", "X=LAUDE; env -u Y \"C${X}_CODE_SIMPLE=1\" claude -p --model opus hi", false, ""},
		{"W26", "X=LAUDE; export Y=1 \"C${X}_CODE_SIMPLE=1\"; claude -p --model opus hi", false, ""},
		{"W27", "X=LAUDE; printf -v \"C${X}_CODE_SIMPLE\" 1; export \"C${X}_CODE_SIMPLE\"; claude -p --model opus hi", false, ""},
		{"W28", "X=LAUDE; read -r \"C${X}_CODE_SIMPLE\" <<< 1; export \"C${X}_CODE_SIMPLE\"; claude -p --model opus hi", false, ""},
		{"W31", "X=LAUDE; set -a; printf -v \"C${X}_CODE_SIMPLE\" 1; claude -p --model opus hi", false, ""},
		{"W35", "X=LAUDE; env Y=1 \"C${X}_CODE_SIMPLE=1\" claude -p --model opus hi", false, ""},
		{"W36", "X=LAUDE; export Y \"C${X}_CODE_SIMPLE=1\"; claude -p --model opus hi", false, ""},
		{"W42", "X=LAUDE; printf -v \"C${X}_CODE_SIMPLE\" 1; export \"C${X}_CODE_SIMPLE\"; bash -c :; claude -p --model opus hi", false, ""},
		{"W51", "X=LAUDE; f(){ local -x \"C${X}_CODE_SIMPLE=1\"; claude -p --model opus hi; }; f", false, ""},
	})
}
