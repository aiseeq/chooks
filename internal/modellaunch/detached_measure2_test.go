package modellaunch

// Мера мастера, круг 3 (фикстуры ревью круга 2). Боец этот файл не правит.
// Живое наблюдение 22.11 21:2x CET: осиротевшие процессы в agent-*.scope
// получают родителем systemd --user (pid 3431, сам под pid 1, subreaper), а не pid 1.

import "testing"

const r2Slice = "0::/user.slice/user-1000.slice/user@1000.service/"

// systemd --user как subreaper: pid 75, его родитель pid 1
func underUserManager(f *fakeProcess, pid int) {
	f.add(75, 1, "/usr/lib/systemd/systemd", nil, []string{"/usr/lib/systemd/systemd", "--user"})
	f.comm[75] = "systemd"
	f.tty[75] = 0
	f.cgroup[75] = r2Slice + "init.scope"
	f.parent[pid] = 75
}

func TestMeasure2AgentForms(t *testing.T) {
	t.Run("env -i setsid -f cl (orphan cl under systemd --user)", func(t *testing.T) {
		f := agentTree([]string{"claude", "--model", "fable"})
		f.add(80, 75, "/home/aisee/bin/cl", nil, []string{"cl", "--model", "fable"})
		f.comm[80], f.tty[80] = "cl", 0
		// cl наследует scope запустившего агента
		f.cgroup[80] = r2Slice + "agents.slice/agent-luna-123.scope"
		f.cgroup[90], f.tty[90] = r2Slice+"agents.slice/agent-claude-80.scope", 34901
		underUserManager(f, 80)
		wantReject(t, f)
	})
	t.Run("systemd-run --user --pty cl -p (claude = former cl, parent systemd --user)", func(t *testing.T) {
		f := agentTree([]string{"claude", "--model", "fable"})
		f.cgroup[90], f.tty[90] = r2Slice+"agents.slice/agent-claude-90.scope", 34902
		underUserManager(f, 90)
		wantReject(t, f)
	})
	t.Run("env -i setsid -f script -qc 'cl -p' (parent script, orphan)", func(t *testing.T) {
		f := agentTree([]string{"claude", "--model", "fable"})
		f.add(80, 75, "/usr/bin/script", nil, []string{"script", "-qc", "cl -p --model fable x", "/dev/null"})
		f.comm[80], f.tty[80] = "script", 0
		f.cgroup[80] = r2Slice + "agents.slice/agent-luna-123.scope"
		f.cgroup[90], f.tty[90] = r2Slice+"agents.slice/agent-claude-90.scope", 34903
		underUserManager(f, 80)
		wantReject(t, f)
	})
	t.Run("systemd-run --user --pty --unit=job claude (not run-*)", func(t *testing.T) {
		f := agentTree([]string{"claude", "--model", "fable"})
		f.cgroup[90], f.tty[90] = r2Slice+"app.slice/job.service", 34904
		underUserManager(f, 90)
		wantReject(t, f)
	})
}

func TestMeasure2UserForms(t *testing.T) {
	t.Run("cl interactive, cl parent bash in Konsole (live)", func(t *testing.T) {
		f := agentTree([]string{"claude", "--model", "fable"})
		f.add(80, 70, "/home/aisee/bin/cl", nil, []string{"cl"})
		f.add(70, 60, "/usr/bin/bash", nil, []string{"bash"})
		f.add(60, 1, "/usr/bin/konsole", nil, []string{"konsole"})
		f.comm[80], f.comm[70], f.comm[60] = "cl", "bash", "konsole"
		f.tty[80], f.tty[70] = 34824, 34824
		f.cgroup[90], f.tty[90] = r2Slice+"agents.slice/agent-akit-master-1332071.scope", 34829
		wantPass(t, f)
	})
	t.Run("cl -p from Konsole, name with dashes", func(t *testing.T) {
		f := agentTree([]string{"claude", "--model", "fable"})
		f.comm[80], f.tty[80] = "bash", 34817
		f.cgroup[90], f.tty[90] = r2Slice+"agents.slice/agent-my-job-name-90.scope", 34817
		wantPass(t, f)
	})
	t.Run("user cl whose Konsole tab closed (cl reparented to systemd --user)", func(t *testing.T) {
		f := agentTree([]string{"claude", "--model", "fable"})
		f.add(80, 75, "/home/aisee/bin/cl", nil, []string{"cl"})
		f.comm[80], f.tty[80] = "cl", 34824
		f.cgroup[90], f.tty[90] = r2Slice+"agents.slice/agent-akit-master-1332071.scope", 34829
		underUserManager(f, 80)
		wantPass(t, f)
	})
}
