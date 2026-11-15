package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/aiseeq/chooks/internal/core"
)

func TestRsyncGuardProbes(t *testing.T) {
	probes := []struct {
		command string
		blocked bool
	}{
		{`rsync -a --delete src/ $R/`, true},
		{`rsync --delete-after a/ "$X"/`, true},
		{`rsync -a --del src/ "$HOME/x"`, true},
		{`rsync -av src/ "${D}" --delete`, true},
		{`sudo rsync -a --delete src/ $R/`, true},
		{`rsync -a --delete-excluded src/ $(pwd)/out/`, true},
		{`rsync -a --delete -e "ssh -p 22" src/ host:$D/`, true},
		{`cd /tmp && rsync -a --delete src/ "$HOME"`, true},
		{`bash -c "rsync -a --delete src/ \$R/"`, true},
		{`rsync -a --delete-during src/ ` + "`pwd`" + `/x/`, true},
		{`rsync -a --delete src/ "$1"/`, true},
		{`rsync -a --delete src/ $@`, true},
		{`rsync -a --delete src/ "${R/:?/}"/`, true},
		{`rsync -a --delete src/ "${R:-}"/`, true},
		{`rsync -a --delete src/ "$D" --compress-level 9`, true},
		{`rsync -a --delete src/ "$D" --write-batch x`, true},
		{`rsync -a --delete src/ "$D" -@ 1`, true},
		{`rsync -a --delete src/ "$D" --zl 9`, true},
		{`rsync -a --delete src/ "$D" --stop-at 2026-12-01T00:00`, true},
		{`rsync -a src/ dst/`, false},
		{`rsync -a --delete src/ /tmp/x/`, false},
		{`rsync -a src/ $R/`, false},
		{`rsync -a --delete --exclude "$PAT" src/ /tmp/x/`, false},
		{`rsync -a --delete --backup-dir=$B src/ /tmp/x/`, false},
		{`rsync -a --delete -e "ssh -i $K" src/ host:/srv/x/`, false},
		{`rsync -a --delete src/ "${R:?}"/`, false},
		{`rsync -a --delete src/ "${R:?не задан}/out/"`, false},
		{`rsync -avn --delete src/ $R/`, false},
		{`rsync -a --dry-run --delete src/ $R/`, false},
		{`echo rsync -a --delete src/ $R/`, false},
		{`rsync --version`, false},
		{`rsync -a --delete src/ /x/ --write-batch "$B"`, false},
		{`rsync -a --delete src/ /x/ --compress-level "$L"`, false},
		{`rsync -a --delete src/ /x/ -@ "$W"`, false},
		{`rsync -a --delete src/ /x/ --copy-as "$U"`, false},
		{`rsync -a --delete src/ /x/ --max-alloc "$M"`, false},
		{`rsync -a --delete $S/ /tmp/x/`, false},
	}
	for _, probe := range probes {
		t.Run(probe.command, func(t *testing.T) {
			tool, err := NewRsyncGuardTool(core.ToolConfig{Enabled: true}, testLogger(t))
			if err != nil {
				t.Fatal(err)
			}
			result, err := tool.ValidateTool(context.Background(), &core.ToolInput{ToolName: "Bash", Command: probe.command})
			if err != nil {
				t.Fatalf("ValidateTool: %v", err)
			}
			if got := !result.IsValid; got != probe.blocked {
				t.Fatalf("blocked=%v, want %v; violations=%+v", got, probe.blocked, result.Violations)
			}
			if probe.blocked && (len(result.Violations) == 0 || !strings.Contains(result.Violations[0].Message, "rsync")) {
				t.Fatalf("block reason must name rsync: %+v", result.Violations)
			}
		})
	}
}
