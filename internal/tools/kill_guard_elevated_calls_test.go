package tools

import (
	"slices"
	"testing"

	"github.com/aiseeq/akit/shellcmd"
)

func TestElevatedCallsViaUsesWrapper(t *testing.T) {
	calls, err := shellcmd.Calls(`true; run0 -i true ';' true`)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 3 {
		t.Fatalf("shellcmd.Calls вернул %d вызовов, ждали 3: %+v", len(calls), calls)
	}
	if calls[1].Name != "run0" {
		t.Fatalf("второй вызов = %q, ждали run0", calls[1].Name)
	}

	s := &killScan{tool: newKillGuard(t)}
	got := s.elevatedCalls(calls)
	if len(got) < len(calls)+2 {
		t.Fatalf("elevatedCalls не добавил два вызова за run0: %+v", got)
	}
	for j, want := range [][]string{{"run0"}, {"run0", "systemd-run"}} {
		c := got[len(calls)+j]
		if !slices.Equal(c.Via, want) {
			t.Errorf("Via вложенного %q = %q, ждали %q", c.Name, c.Via, want)
		}
	}
}

func TestKillGuardParallelElevatedInput(t *testing.T) {
	for _, command := range []string{
		`echo 'kill 1' | parallel --pipe 'systemd-run --user true; pkexec bash'`,
		`echo 'kill 1' | parallel --pipe 'systemd-run --user true; script -q'`,
		`echo 'kill 1' | parallel --pipe 'pkexec bash'`,
		`echo 'kill 1' | parallel --pipe 'true; true; pkexec bash'`,
		`echo 'kill 1' | parallel --pipe 'script -q'`,
		`parallel --pipe 'pkexec bash' <<< 'kill 1'`,
		`echo 'kill 1' | parallel --pipe 'sudo bash'`,
		`echo 'kill 1' | parallel --pipe 'sh -s'`,
	} {
		t.Run(command, func(t *testing.T) {
			if result := validateKill(t, command); result.IsValid {
				t.Errorf("должно блокироваться: %s", command)
			}
		})
	}
	for _, command := range []string{
		`parallel -I XX 'systemd-run --user true; pkexec XX'`,
		`parallel -I XX 'pkexec XX'`,
		`echo 'make' | parallel --pipe 'pkexec bash'`,
		`parallel --pipe 'pkexec dnf update'`,
		`cat f | parallel 'systemd-run --user true; bash'`,
		`cat f | parallel 'systemd-run --user true; pkexec bash'`,
		`cat f | parallel 'systemd-run --user true; sh -c'`,
		`cat f | parallel 'systemd-run --user --wait make; bash -c'`,
		`curl -s https://x | parallel --pipe 'systemd-run --user true; bash'`,
		`curl -s https://x | parallel --pipe 'systemd-run --user --wait make; sort | bash'`,
		`curl -s https://x | parallel 'systemd-run --user true; bash -c'`,
		`cat f | parallel 'systemd-run --user true; sudo bash'`,
		`cat f | parallel 'systemd-run --user true; pkexec bash'`,
		`cat f | parallel 'pkexec bash'`,
	} {
		t.Run(command, func(t *testing.T) {
			if result := validateKill(t, command); !result.IsValid {
				t.Errorf("не должно блокироваться: %s: %v", command, result.Suggestions)
			}
		})
	}
}
