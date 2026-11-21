package tools

import "testing"

func TestKillGuardParallelImplicitShellInput(t *testing.T) {
	for _, command := range []string{
		`echo 'kill 1' | parallel 'pkexec bash -c'`,
		`parallel 'pkexec bash -c' ::: 'kill 1'`,
		`parallel 'pkexec sh -c' ::: 'kill 1'`,
		`parallel 'sudo bash -c' ::: 'kill 1'`,
		`parallel 'script -qc' ::: 'kill 1'`,
		`echo 'kill 1' | parallel 'systemd-run --user true; pkexec bash -c'`,
		`echo make | parallel --pipe 'pkexec systemd-run --user --pipe bash'`,
		`echo 'kill 1' | parallel --pipe 'parallel --pipe pkexec bash'`,
		`parallel pkexec bash -c ::: 'kill 1'`,
		`parallel 'pkexec bash -c' <<< 'kill 1'`,
		`parallel 'sudo bash -c' <<< 'kill 1'`,
		`parallel 'pkexec sh -ec' <<< 'kill 1'`,
		`echo 'kill 1' | parallel 'doas bash -c'`,
		`cat f | parallel 'run0 bash -c'`,
		`echo 'kill 1' | parallel 'su -c'`,
		`xargs pkexec bash -c <<< 'kill 1'`,
		`echo 'kill 1' | xargs doas bash -c`,
		`echo make | xargs run0 sh -c`,
		`echo 'kill 1' | parallel 'pkexec bash -c --'`,
		`echo 'kill 1' | parallel 'pkexec bash --norc -c'`,
		`echo 'kill 1' | xargs sudo bash -c --`,
		`echo 'kill 1' | xargs script -qc`,
		`echo 'kill 1' | xargs pkexec script -qc`,
		`echo 'kill 1' | parallel 'su -lc'`,
		`echo 'kill 1' | setsid parallel 'sudo bash -c'`,
		`echo 'kill 1' | parallel 'sudo bash -c' :::: -`,
		`echo 'kill 1' | parallel -I XX 'sudo bash -c XX'`,
		`parallel -I XX pkexec bash -c XX ::: 'kill 1'`,
		`parallel sudo bash -c 'timeout 5 $0' ::: 'kill 1'`,
		`parallel sudo bash -c 'timeout 5 $0' ::: "$CMD" # systemctl`,
		`echo 'kill 1' | xargs sudo bash -c 'timeout 5 $0'`,
		`echo 'kill 1' | sem 'sudo bash -c'`,
	} {
		t.Run(command, func(t *testing.T) {
			if r := validateKill(t, command); r.IsValid {
				t.Fatalf("expected refusal for %q", command)
			}
		})
	}
}

func TestKillGuardParallelImplicitShellInputAllowsLiteral(t *testing.T) {
	for _, command := range []string{
		`echo make | parallel 'pkexec bash -c'`,
		`parallel 'pkexec bash -c' ::: 'make -j2'`,
		`parallel 'bash -c' ::: 'echo done'`,
		`parallel sudo bash -c 'timeout 5 $0' ::: 'make -j2'`,
		`parallel 'pkexec bash -c "echo safe"' ::: 'kill 1'`,
		`parallel 'pkexec bash -c {}' ::: 'echo hi'`,
		`parallel 'pkexec bash -c' <<< make`,
		`echo make | parallel 'doas bash -c'`,
		`echo make | xargs sudo bash -c`,
		`echo 1 | run0 xargs sh -c 'echo "$@"' _`,
	} {
		t.Run(command, func(t *testing.T) {
			if r := validateKill(t, command); !r.IsValid {
				t.Fatalf("expected allow for %q: %v", command, r.Suggestions)
			}
		})
	}
}

func TestKillGuardParallelImplicitKeepsExistingScriptDenial(t *testing.T) {
	for _, command := range []string{
		`parallel sudo bash -c 'timeout 5 $0' ::: 'kill 1'`,
		`parallel -I XX pkexec bash -c XX ::: 'kill 1'`,
	} {
		t.Run(command, func(t *testing.T) {
			if r := validateKill(t, command); r.IsValid {
				t.Fatalf("expected refusal for %q", command)
			}
		})
	}
}
