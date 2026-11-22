package modellaunch

// Мера мастера, круг 4 (фикстуры ревью круга 3). Боец этот файл не правит.

// Фикстуры ревью, круг 3 (go test -overlay). Утверждают ЖЕЛАЕМОЕ поведение.
// Живое 22.11 21:58 CET: все 16 процессов cl лежат в app-org.kde.konsole-*.scope/tab(N).scope;
// осиротевшие процессы получают родителем systemd --user (pid 3431).

import "testing"

const r3Slice = "0::/user.slice/user-1000.slice/user@1000.service/"
const r3Tab = r3Slice + "app.slice/app-org.kde.konsole-7438.scope/tab(7492).scope"

func r3UserManager(f *fakeProcess) {
	f.add(75, 1, "/usr/lib/systemd/systemd", nil, []string{"/usr/lib/systemd/systemd", "--user"})
	f.comm[75], f.tty[75], f.cgroup[75] = "systemd", 0, r3Slice+"init.scope"
}

// claude 90 — бывший cl (cl -p), родитель 80 с comm parentComm во вкладке Konsole, над ним bash 70
func r3ClPrint(parentComm string, parentTTY int64) *fakeProcess {
	f := agentTree([]string{"claude", "--model", "fable"})
	f.add(80, 70, "/usr/bin/"+parentComm, nil, []string{parentComm})
	f.add(70, 1, "/usr/bin/bash", nil, []string{"bash"})
	f.comm[80], f.tty[80], f.cgroup[80] = parentComm, parentTTY, r3Tab
	f.comm[70], f.tty[70], f.cgroup[70] = "bash", 34817, r3Tab
	f.cgroup[90], f.tty[90] = r3Slice+"agents.slice/agent-claude-90.scope", 34817
	return f
}

func TestMeasure3AgentForms(t *testing.T) {
	// systemd-run --user --pty cl --model fable 'prompt': cl — главный процесс run-*.service с pty,
	// родитель systemd --user; claude — ребёнок cl в своём agent-*.scope с pty cl
	t.Run("systemd-run --user --pty cl (interactive)", func(t *testing.T) {
		f := agentTree([]string{"claude", "--model", "fable"})
		f.add(80, 75, "/home/aisee/bin/cl", nil, []string{"cl", "--model", "fable", "prompt"})
		f.comm[80], f.tty[80], f.cgroup[80] = "cl", 34905, r3Slice+"app.slice/run-u5.service"
		f.cgroup[90], f.tty[90] = r3Slice+"agents.slice/agent-claude-80.scope", 34906
		r3UserManager(f)
		wantReject(t, f)
	})
	// env -i … setsid -f script -qc "cl --model fable prompt" /dev/null
	t.Run("setsid -f script -qc cl (interactive)", func(t *testing.T) {
		f := agentTree([]string{"claude", "--model", "fable"})
		f.add(80, 70, "/home/aisee/bin/cl", nil, []string{"cl", "--model", "fable", "prompt"})
		f.add(70, 75, "/usr/bin/script", nil, []string{"script", "-qc", "cl --model fable prompt", "/dev/null"})
		f.comm[80], f.tty[80], f.cgroup[80] = "cl", 34907, r3Slice+"agents.slice/agent-luna-123.scope"
		f.comm[70], f.tty[70], f.cgroup[70] = "script", 0, r3Slice+"agents.slice/agent-luna-123.scope"
		f.cgroup[90], f.tty[90] = r3Slice+"agents.slice/agent-claude-80.scope", 34908
		r3UserManager(f)
		wantReject(t, f)
	})
}

func TestMeasure3UserForms(t *testing.T) {
	t.Run("timeout 300 cl -p from Konsole", func(t *testing.T) { wantPass(t, r3ClPrint("timeout", 34817)) })
	t.Run("python3 script runs cl -p from Konsole", func(t *testing.T) { wantPass(t, r3ClPrint("python3", 34817)) })
	t.Run("make runs cl -p from Konsole", func(t *testing.T) { wantPass(t, r3ClPrint("make", 34817)) })
	t.Run("bash cl -p from Konsole", func(t *testing.T) { wantPass(t, r3ClPrint("bash", 34817)) })
	// оболочка вкладки убита, cl жив: cl остаётся в scope вкладки, родитель — systemd --user
	t.Run("cl after its shell died (cl in tab scope, parent systemd --user)", func(t *testing.T) {
		f := agentTree([]string{"claude", "--model", "fable"})
		f.add(80, 75, "/home/aisee/bin/cl", nil, []string{"cl"})
		f.comm[80], f.tty[80], f.cgroup[80] = "cl", 34824, r3Tab
		f.cgroup[90], f.tty[90] = r3Slice+"agents.slice/agent-akit-master-1332071.scope", 34829
		r3UserManager(f)
		wantPass(t, f)
	})
	t.Run("VS Code terminal cl", func(t *testing.T) {
		f := agentTree([]string{"claude", "--model", "fable"})
		vs := r3Slice + "app.slice/app-code-4321.scope"
		f.add(80, 70, "/home/aisee/bin/cl", nil, []string{"cl"})
		f.add(70, 60, "/usr/bin/bash", nil, []string{"bash"})
		f.add(60, 1, "/usr/share/code/code", nil, []string{"code"})
		f.comm[80], f.tty[80], f.cgroup[80] = "cl", 34950, vs
		f.comm[70], f.tty[70], f.cgroup[70] = "bash", 34950, vs
		f.comm[60], f.tty[60], f.cgroup[60] = "code", 0, vs
		f.cgroup[90], f.tty[90] = r3Slice+"agents.slice/agent-x-80.scope", 34951
		wantPass(t, f)
	})
	t.Run("VS Code terminal cl -p", func(t *testing.T) {
		f := r3ClPrint("bash", 34950)
		vs := r3Slice + "app.slice/app-code-4321.scope"
		f.cgroup[80], f.cgroup[70], f.tty[90] = vs, vs, 34950
		wantPass(t, f)
	})
	t.Run("VS Code extension claude without tty", func(t *testing.T) {
		f := agentTree([]string{"claude", "--model", "fable"})
		f.comm[80], f.tty[80], f.cgroup[80] = "code", 0, r3Slice+"app.slice/app-code@abc.service"
		f.cgroup[90], f.tty[90] = r3Slice+"app.slice/app-code@abc.service", 0
		wantPass(t, f)
	})
	t.Run("GoLand terminal claude", func(t *testing.T) {
		f := agentTree([]string{"claude", "--model", "fable"})
		g := r3Slice + "app.slice/app-jetbrains-goland-5555.scope"
		f.comm[80], f.tty[80], f.cgroup[80] = "bash", 34960, g
		f.cgroup[90], f.tty[90] = g, 34960
		wantPass(t, f)
	})
	t.Run("ssh with tty, cl", func(t *testing.T) {
		f := agentTree([]string{"claude", "--model", "fable"})
		s := "0::/user.slice/user-1000.slice/session-12.scope"
		f.add(80, 70, "/home/aisee/bin/cl", nil, []string{"cl"})
		f.add(70, 1, "/usr/bin/bash", nil, []string{"-bash"})
		f.comm[80], f.tty[80], f.cgroup[80] = "cl", 34970, s
		f.comm[70], f.tty[70], f.cgroup[70] = "bash", 34970, s
		f.cgroup[90], f.tty[90] = r3Slice+"agents.slice/agent-x-80.scope", 34971
		wantPass(t, f)
	})
	// ssh host cl -p: sshd-session → bash -c 'cl -p …' (exec) → claude, без tty
	t.Run("ssh host cl -p without tty", func(t *testing.T) {
		f := agentTree([]string{"claude", "--model", "fable"})
		f.comm[80], f.tty[80], f.cgroup[80] = "sshd-session", 0, "0::/user.slice/user-1000.slice/session-13.scope"
		f.cgroup[90], f.tty[90] = r3Slice+"agents.slice/agent-claude-90.scope", 0
		wantPass(t, f)
	})
	t.Run("tty4 claude", func(t *testing.T) {
		f := agentTree([]string{"claude", "--model", "fable"})
		f.comm[80], f.tty[80], f.cgroup[80] = "bash", 1028, "0::/user.slice/user-1000.slice/session-11.scope"
		f.cgroup[90], f.tty[90] = "0::/user.slice/user-1000.slice/session-11.scope", 1028
		wantPass(t, f)
	})
	t.Run("Chrome native host", func(t *testing.T) {
		f := agentTree([]string{"claude", "--model", "fable"})
		c := r3Slice + "app.slice/app-com.google.Chrome-657987.scope"
		f.comm[80], f.tty[80], f.cgroup[80] = "chrome", 0, c
		f.cgroup[90], f.tty[90] = c, 0
		wantPass(t, f)
	})
}
