package modellaunch

// Мера мастера (model-guard-detached-launch, круг 2): кто запустил claude по
// cgroup, tty и цепочке родителей. Боец этот файл не правит.

import (
	"os"
	"path/filepath"
	"testing"
)

const measureSlice = "0::/user.slice/user-1000.slice/user@1000.service/"

// claude 90 (cgroup, tty) под родителем 80 с comm parentComm; над 80 — pid 1
func measureTree(model, cgroup string, tty int64, parentComm string) *fakeProcess {
	f := agentTree([]string{"claude", "--model", model})
	f.comm[80] = parentComm
	f.cgroup[90], f.tty[90] = cgroup, tty
	return f
}

// над 80 — цепочка пользователя: bash 70 с tty в вкладке Konsole → konsole 60
func underUserShell(f *fakeProcess) *fakeProcess {
	f.add(80, 70, "/usr/bin/bash", nil, []string{"bash"})
	f.add(70, 60, "/usr/bin/bash", nil, []string{"bash"})
	f.add(60, 1, "/usr/bin/konsole", nil, []string{"konsole"})
	f.comm[80], f.comm[70], f.comm[60] = "bash", "bash", "konsole"
	f.tty[80], f.tty[70] = 34817, 34817
	f.cgroup[80] = measureSlice + "app.slice/app-org.kde.konsole-7438.scope/tab(7492).scope"
	return f
}

// над 80 — login 70 на tty4 → pid 1
func underLogin(f *fakeProcess) *fakeProcess {
	f.add(80, 70, "/usr/bin/bash", nil, []string{"-bash"})
	f.add(70, 1, "/usr/bin/login", nil, []string{"login"})
	f.comm[80], f.comm[70] = "bash", "login"
	f.tty[80], f.tty[70] = 1028, 1028
	f.cgroup[80] = "0::/user.slice/user-1000.slice/session-11.scope"
	return f
}

func TestMeasureUserLaunches(t *testing.T) {
	cl := func(cgroup string, tty int64) *fakeProcess {
		f := measureTree("fable", cgroup, tty, "cl")
		userChain(f)
		f.comm[80] = "cl"
		f.tty[70] = tty
		return f
	}
	for _, tc := range []struct {
		name string
		f    *fakeProcess
	}{
		{"cl interactive", cl(measureSlice+"agents.slice/agent-chooks-master-1331786-r3.scope", 34824)},
		{"tty4 login bash", underLogin(measureTree("fable", "0::/user.slice/user-1000.slice/session-11.scope", 1028, "bash"))},
		{"ssh without tty in session scope", measureTree("fable", "0::/user.slice/user-1000.slice/session-12.scope", 0, "bash")},
		{"Konsole tab", measureTree("fable", measureSlice+"app.slice/app-org.kde.konsole-7438.scope/tab(7492).scope", 34817, "bash")},
		{"Chrome native host", measureTree("fable", measureSlice+"app.slice/app-com.google.Chrome-657987.scope", 0, "chrome")},
		// cl -p: syscall.Exec(systemd-run --scope) — claude = бывший cl, scope по его pid, родитель — оболочка пользователя
		{"cl -p from Konsole", underUserShell(measureTree("fable", measureSlice+"agents.slice/agent-claude-90.scope", 34817, "bash"))},
		{"cl -p from tty4", underLogin(measureTree("fable", measureSlice+"agents.slice/agent--90.scope", 1028, "bash"))},
		// запуск с рабочего стола KDE: app-<id>@<uuid>.service без tty
		{"desktop app service without tty", measureTree("fable", measureSlice+"app.slice/app-code@7585c116998f4cfb8fa921f2c6606755.service", 0, "code")},
	} {
		t.Run(tc.name, func(t *testing.T) { wantPass(t, tc.f) })
	}
}

func TestMeasureAgentDetachedForms(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    *fakeProcess
	}{
		{"script -qc in agent scope", measureTree("fable", measureSlice+"agents.slice/agent-luna-123.scope", 34900, "script")},
		{"nohup/setsid in agent scope", measureTree("fable", measureSlice+"agents.slice/agent-luna-123.scope", 0, "systemd")},
		{"at job", measureTree("fable", "0::/system.slice/atd.service", 0, "atd")},
		{"systemd-run --user", measureTree("fable", measureSlice+"app.slice/run-u123.service", 0, "systemd")},
		{"systemd-run --user --pty", measureTree("fable", measureSlice+"app.slice/run-u123.service", 34900, "systemd")},
		{"worker job service", measureTree("fable", measureSlice+"agents.slice/worker-round-1.service", 0, "bash")},
		// решение мастера 22.11: свой сервис пользователя вне session/app — автоматика, судится как агент
		{"user systemd service", measureTree("fable", measureSlice+"app.slice/legal-aggregator.service", 0, "aggregator")},
		// env -i setsid -f cl --model X: cl осиротел (родитель pid 1), claude в scope cl с pty
		{"detached cl started by agent", measureTree("fable", measureSlice+"agents.slice/agent-claude-777.scope", 34901, "cl")},
		// env -i setsid -f cl -p: claude = бывший cl, scope по своему pid, но без tty и с родителем systemd
		{"detached cl -p started by agent", measureTree("fable", measureSlice+"agents.slice/agent-claude-90.scope", 0, "systemd")},
	} {
		t.Run(tc.name, func(t *testing.T) { wantReject(t, tc.f) })
	}
}

func TestMeasureApprovedModelServicePasses(t *testing.T) {
	wantPass(t, measureTree("opus", measureSlice+"app.slice/legal-aggregator.service", 0, "aggregator"))
}

// ошибки чтения: признак не срабатывает — «пользователь»
func TestMeasureReadErrorsFailOpen(t *testing.T) {
	f := measureTree("fable", measureSlice+"app.slice/run-u1.service", 0, "systemd")
	f.cgroupErr[90] = errDenied
	wantPass(t, f)
	f = measureTree("fable", measureSlice+"app.slice/run-u1.service", 0, "systemd")
	f.ttyErr[90] = errDenied
	wantPass(t, f)
}

// ProcReader на фикстуре /proc: tty_nr — 7-е поле stat, comm с «) » внутри
func TestMeasureProcReader(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "42")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte("42 (we) ird) S 7 42 42 34817 42 4194560 0 0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cg := "0::/user.slice/user-1000.slice/user@1000.service/agents.slice/agent-x-1.scope\n"
	if err := os.WriteFile(filepath.Join(dir, "cgroup"), []byte(cg), 0600); err != nil {
		t.Fatal(err)
	}
	p := ProcReader{Root: root}
	if tty, err := p.TTYNr(42); err != nil || tty != 34817 {
		t.Fatalf("TTYNr = %d, %v; want 34817", tty, err)
	}
	if got, err := p.Cgroup(42); err != nil || !inAgentScope(got) || inInteractiveScope(got) {
		t.Fatalf("Cgroup = %q, %v", got, err)
	}
}
