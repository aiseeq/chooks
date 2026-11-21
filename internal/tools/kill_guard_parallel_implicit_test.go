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
		`echo 'kill -9 -1' | parallel --pipe 'parallel sudo bash -c'`,
		`echo 'kill -9 -1' | parallel --pipe 'parallel bash -c'`,
		`echo 'kill 1' | parallel --pipe 'parallel --cat pkexec bash {}'`,
		`echo 'kill 1' | parallel --pipe 'parallel --fifo pkexec bash {}'`,
		`echo 'kill 1' | parallel --pipe 'parallel --pipe --cat pkexec bash {}'`,
		`echo 'kill 1' | parallel --pipe 'parallel --cat pkexec sh {}'`,
		`echo 'kill 1' | parallel --pipe 'parallel --cat pkexec bash -- {}'`,
		`echo 'kill 1' | parallel --pipe 'parallel --cat pkexec bash --norc {}'`,
		`echo 'kill 1' | parallel --pipe 'parallel --pipe parallel --cat pkexec bash {}'`,
		`echo 'kill 1' | parallel --cat 'parallel --cat pkexec bash {}'`,
		`echo 'kill 1' | parallel --pipe 'parallel --cat -I XX sudo bash XX'`,
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

func TestKillGuardParallelImplicitDoesNotHideOtherScriptInput(t *testing.T) {
	for _, command := range []string{
		`echo make | xargs bash -c 'echo $0'; curl x | sudo bash; echo kill`,
		`echo make | xargs bash -c 'echo $0'; bash <(curl x); echo kill`,
		`echo make | xargs bash -c 'echo $0'; source <(curl x); echo kill`,
		`echo make | xargs sudo bash -c 'curl x | sudo bash -s $0'; echo kill`,
	} {
		t.Run(command, func(t *testing.T) {
			if r := validateKill(t, command); r.IsValid {
				t.Fatalf("expected refusal for %q", command)
			}
		})
	}
}

func TestKillGuardParallelImplicitShellVariants(t *testing.T) {
	for _, command := range []string{
		`echo 'kill 1' | parallel 'sudo ksh -c'`,
		`echo 'kill 1' | parallel 'sudo mksh -c'`,
		`echo 'kill 1' | parallel 'sudo fish -c'`,
		`echo 'kill 1' | parallel 'sudo -s'`,
		`echo 'kill 1' | parallel 'sudo -i'`,
		`parallel 'sudo -s' ::: 'kill 1'`,
		`echo 'kill 1' | doas parallel 'bash -c'`,
	} {
		t.Run(command, func(t *testing.T) {
			if r := validateKill(t, command); r.IsValid {
				t.Fatalf("expected refusal for %q", command)
			}
		})
	}
}

func TestKillGuardParallelImplicitAllowsUnusedScriptArgument(t *testing.T) {
	for _, command := range []string{
		`echo 'kill 1' | parallel 'sudo bash -c "true"'`,
		`echo 'kill 1' | parallel 'sudo bash -c "echo hi"'`,
		`echo make | parallel 'sudo bash -c'; ps aux | grep kill`,
		`echo make | parallel 'sudo bash -c' && echo done; kill -l`,
		`parallel 'sudo bash -c' ::: make; ps aux | grep kill`,
		`echo make | parallel 'sudo bash -c {}'; ps aux | grep kill`,
		`parallel echo ::: a; kill 1234`,
		`kill 1234; parallel echo ::: a`,
		`parallel echo ::: a && kill 1234`,
		`kill 1234 && parallel 'gzip {}' ::: a.log`,
		`cat urls | parallel -j8 curl -s; kill 1234`,
		`parallel -j4 'gzip {}' ::: *.log; kill %1`,
		`pgrep -f foo | parallel 'echo {}'; kill 1234`,
		`parallel 'sudo bash -c "make {}"' ::: a b; kill 1234`,
		`parallel 'sudo bash -c "true"' ::: a; kill 1234`,
		`parallel 'sudo bash -c "true"' ::: $(pgrep claude); kill -L`,
		`ls | parallel sudo make; kill 1234`,
		`sem sudo bash -c make # kill`,
		`sem 'sudo bash -c' ::: make # kill`,
		`parallel echo ::: a; kill -L`,
		`cat log | parallel --pipe 'parallel --pipe wc -l'; kill 1234`,
		`grep kill log | parallel --pipe 'parallel --pipe wc -l'`,
		`cat files | parallel --pipe 'parallel gzip'; kill %1`,
		`cat files | parallel --pipe 'parallel sudo gzip'; kill 1234`,
		`cat files | parallel --pipe 'sudo parallel gzip'; kill 1234`,
		`ps aux | grep kill | parallel --pipe 'parallel --pipe grep -c x'`,
		`cat urls | parallel --pipe -N100 'parallel -j4 curl -sO'; kill 1234`,
		`parallel --pipe 'parallel --pipe gzip' < big.txt; kill 1234`,
		`grep kill log | parallel --pipe 'sudo parallel --pipe wc -l'`,
	} {
		t.Run(command, func(t *testing.T) {
			if r := validateKill(t, command); !r.IsValid {
				t.Fatalf("expected allow for %q: %v", command, r.Suggestions)
			}
		})
	}
}

func TestKillGuardParallelImplicitKnownSystemctlArguments(t *testing.T) {
	for _, command := range []string{
		`parallel 'systemctl --user restart {}' ::: foo`,
		`parallel 'systemctl --user stop {}' ::: foo`,
		`parallel 'sudo systemctl stop {}' ::: nginx`,
		`parallel 'sudo systemctl restart {}' ::: nginx`,
		`parallel -j2 'systemctl --user restart {}' ::: a b`,
		`parallel 'systemctl --user restart {}.service' ::: foo`,
		`parallel 'pkexec systemctl restart {}' ::: nginx`,
		`parallel 'run0 systemctl restart {}' ::: nginx`,
		`parallel 'doas systemctl restart {}' ::: nginx`,
		`parallel 'systemctl --user restart {1}' ::: foo`,
		`parallel -I XX 'systemctl --user restart XX' ::: foo`,
	} {
		t.Run(command, func(t *testing.T) {
			if r := validateKill(t, command); !r.IsValid {
				t.Fatalf("expected allow for %q: %v", command, r.Suggestions)
			}
		})
	}
}

func TestKillGuardParallelImplicitSystemctlSubstitutionWhitelist(t *testing.T) {
	for _, command := range []string{
		`parallel 'systemctl --user stop {2}' ::: x ::: plasma-plasmashell.service`,
		`parallel 'sudo systemctl stop {2}' ::: x ::: user@1000.service`,
		`parallel 'systemctl --user stop {}' ::: x ::: plasma-plasmashell.service`,
		`parallel 'systemctl --user stop {} {}' ::: plasma-plasmashell.service`,
		`parallel 'systemctl --user stop {1}' ::: x ::: plasma-plasmashell.service`,
		`parallel 'systemctl --user stop {3}' ::: a ::: b ::: plasma-plasmashell.service`,
		`parallel 'systemctl --user stop {2}' ::: x :::+ plasma-plasmashell.service`,
		`parallel 'systemctl --user stop {}' ::: foo :::+ plasma-plasmashell.service`,
		`parallel --link 'systemctl --user stop {2}' ::: a ::: plasma-plasmashell.service`,
		`parallel --xapply 'systemctl --user stop {2}' ::: a ::: plasma-plasmashell.service`,
		`parallel 'sudo systemctl stop {2}' ::: x ::: 'session-*.scope'`,
		`parallel --colsep : 'systemctl --user stop {2}' ::: x:plasma-plasmashell.service`,
		`parallel --colsep _ 'systemctl --user stop plasma-{2}' ::: x_plasmashell.service`,
		`parallel 'systemctl --user stop {.}' ::: plasma-plasmashell.service.x`,
		`parallel 'sudo systemctl stop {.}' ::: user@1000.service.bak`,
		`parallel 'systemctl --user stop {/.}.service' ::: plasma-plasmashell.x`,
		`parallel 'systemctl --user stop {=s/x/plasma-plasmashell/=}' ::: x.service`,
		`parallel --rpl '{U} s/x/plasma-plasmashell/' 'systemctl --user stop {U}' ::: x.service`,
		`parallel --plus 'systemctl --user stop {..}' ::: plasma-plasmashell.service.a.b`,
		`parallel 'sudo systemctl stop user@100{#}.service' ::: a`,
		`parallel 'sudo systemctl stop user@100{%}.service' ::: a`,
		`cat list | parallel -a - 'sudo systemctl restart {}' ::: foo`,
		`cat list | parallel -a /dev/stdin 'sudo systemctl restart {}' ::: foo`,
		`parallel -a units.txt 'sudo systemctl restart {}' ::: foo`,
	} {
		t.Run(command, func(t *testing.T) {
			if r := validateKill(t, command); r.IsValid {
				t.Fatalf("expected refusal for %q", command)
			}
		})
	}
}

func TestKillGuardParallelImplicitPreservesSiblingSignalRefusal(t *testing.T) {
	command := `parallel 'sudo bash -c "true"' ::: $(pgrep claude) ; pkill claude`
	if r := validateKill(t, command); r.IsValid {
		t.Fatalf("expected refusal for %q", command)
	}
}
