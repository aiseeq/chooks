package main

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Пороги меры тени (этап 2б-3а)
const (
	shadowP50Limit = 2 * time.Millisecond   // прирост p50 CLI с тенью
	badDaemonLimit = 100 * time.Millisecond // прирост при плохом демоне
	badDaemonCalls = 20
	badDaemonInput = 1 << 20 // команда в 1 МиБ: запись больше буфера сокета
)

// shadowInput — вход сравниваемой подкоманды
type shadowInput struct {
	hook  string
	stdin []byte
}

func shadowInputs(project, transcript string) ([]shadowInput, error) {
	specs := []struct {
		hook string
		in   obj
	}{
		{"pre-tool-use", obj{"session_id": "s1", "cwd": project, "transcript_path": transcript, "hook_event_name": "PreToolUse",
			"tool_name": "Bash", "tool_input": obj{"command": "git status"}}},
		{"no-prompt", obj{"session_id": "s1", "hook_event_name": "PermissionRequest", "tool_name": "Bash", "tool_input": obj{"command": "ls"}}},
		{"unsent-check", obj{"session_id": "s1", "hook_event_name": "Stop", "transcript_path": transcript, "last_assistant_message": "готово"}},
		{"stop", obj{"session_id": "s1", "hook_event_name": "Stop", "transcript_path": transcript, "background_tasks": []any{}, "session_crons": []any{}}},
		{"stop-failure", obj{"session_id": "s1", "hook_event_name": "StopFailure", "error": "rate_limit"}},
		{"notification", obj{"session_id": "s1", "hook_event_name": "Notification", "notification_type": "permission_prompt", "message": "нужен ответ"}},
		{"night-hint", obj{"session_id": "s1", "hook_event_name": "SessionStart", "source": "startup"}},
	}
	inputs := make([]shadowInput, 0, len(specs))
	for _, s := range specs {
		stdin, err := marshal(s.in)
		if err != nil {
			return nil, err
		}
		inputs = append(inputs, shadowInput{hook: s.hook, stdin: stdin})
	}
	return inputs, nil
}

// shadowConfig — конфиг песочницы: уведомление решает и ставит заголовок
// (OSC в stderr), но без звука и рабочего стола
func shadowConfig(shadow bool) string {
	return fmt.Sprintf("logger:\n  level: info\n  output: file\n  file: ~/.claude/logs/chooks.log\ndaemon:\n  shadow: %t\n"+
		"tools:\n  notifier: {enabled: true, sound: false, desktop: false}\n  night_hint: {enabled: true}\n  kill_guard: {enabled: true}\n"+
		"  git_guard: {enabled: true}\n  rm_guard: {enabled: true}\n", shadow)
}

// shadowSandbox — песочница меры: HOME, проект, XDG_RUNTIME_DIR и два
// XDG_CONFIG_HOME — без тени (off) и с тенью (on)
type shadowSandbox struct {
	home, project, run, off, on, transcript string
}

func newShadowSandbox(work string) (shadowSandbox, error) {
	root := filepath.Join(work, "shadow-sandbox")
	if err := os.RemoveAll(root); err != nil {
		return shadowSandbox{}, fmt.Errorf("reset sandbox: %w", err)
	}
	s := shadowSandbox{home: filepath.Join(root, "home"), project: filepath.Join(root, "work"), run: filepath.Join(root, "run"),
		off: filepath.Join(root, "config-off"), on: filepath.Join(root, "config-on")}
	s.transcript = s.home + "/.claude/projects/-work/s1.jsonl"
	for _, d := range []string{s.home + "/.config/models", s.home + "/.claude/logs", s.home + "/.claude/projects/-work", s.off + "/chooks", s.on + "/chooks", s.off + "/models", s.on + "/models", s.project, s.run} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return shadowSandbox{}, fmt.Errorf("sandbox: %w", err)
		}
	}
	for path, body := range map[string]string{
		s.off + "/chooks/config.yaml":       shadowConfig(false),
		s.on + "/chooks/config.yaml":        shadowConfig(true),
		s.off + "/models/approved":          "opus claude-opus-5-5\n",
		s.on + "/models/approved":           "opus claude-opus-5-5\n",
		s.home + "/.config/models/approved": "opus claude-opus-5-5\n",
		s.transcript:                        "",
	} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			return shadowSandbox{}, fmt.Errorf("sandbox file: %w", err)
		}
	}
	return s, nil
}

// env — окружение CLI без тени и с тенью на сокете демона socket
func (s shadowSandbox) env(socket string) (off, on []string) {
	env := []string{"HOME=" + s.home, "XDG_RUNTIME_DIR=" + s.run, "PATH=/usr/bin:/bin", "LANG=C.UTF-8", "CHOOKS_HOOK_SOCKET=" + socket}
	return append(slices.Clone(env), "XDG_CONFIG_HOME="+s.off), append(slices.Clone(env), "XDG_CONFIG_HOME="+s.on)
}

// runShadowTiming — CLI с тенью и без неё на демоне `chooks serve`
func runShadowTiming(bin binaries, n int) (code int, err error) {
	if err := bin.resolveCLI(); err != nil {
		return 2, err
	}
	box, err := newShadowSandbox(bin.work)
	if err != nil {
		return 2, err
	}
	inputs, err := shadowInputs(box.project, box.transcript)
	if err != nil {
		return 2, err
	}
	logFile, err := os.Create(filepath.Join(bin.work, "shadow-daemon.log"))
	if err != nil {
		return 2, fmt.Errorf("daemon log: %w", err)
	}
	defer func() {
		if closeErr := logFile.Close(); closeErr != nil {
			code, err = 2, errors.Join(err, fmt.Errorf("daemon log: %w", closeErr))
		}
	}()
	d, err := startDaemon(bin.cli, logFile, "logger:\n  level: info\n  output: file\n  file: ~/.claude/logs/chooks.log\n")
	if err != nil {
		return 2, err
	}
	journal := filepath.Join(d.dir, "home", ".claude", "logs", "chooks-shadow.tsv")
	envOff, envOn := box.env(d.socket)
	fail, err := timeShadow(bin.cli, inputs, envOff, envOn, box.project, n)
	if err != nil {
		d.stop()
		return 2, err
	}
	want := n * len(inputs)
	if err := waitJournal(journal, want, 60*time.Second); err != nil {
		d.stop()
		return 2, err
	}
	byHook, details, err := journalOutcomes(journal)
	if err != nil {
		d.stop()
		return 2, err
	}
	bad, badFail, err := badDaemon(bin.cli, box.project, envOff, envOn)
	// Журнал тени и лог демона — в --work до остановки: stop убирает его
	// каталог, а пропуски разбираются по ним
	refusals := keepDaemonLogs(d.dir, bin.work)
	daemonExit := d.stop()
	if err != nil {
		return 2, err
	}
	compared, mismatches := printJournal(inputs, byHook, details, n)
	fmt.Printf("daemon refusals (request left to CLI): %d\n", len(refusals))
	for _, r := range refusals {
		fmt.Println("  " + r)
	}
	fmt.Print(bad)
	fmt.Printf("daemon exit: %d\n", daemonExit)
	// Пропуск (запись не дошла до журнала) — строка сводки, а не провал
	// меры: CLI молча продолжает по замыслу; провал — расхождение, ноль
	// сравнений, прирост сверх порога, другой ответ CLI
	if fail || badFail || compared == 0 || mismatches > 0 || daemonExit != 0 {
		return 1, nil
	}
	return 0, nil
}

// timeShadow — замер каждой подкоманды без тени и с тенью; true — прирост
// p50 сверх порога или ответ, отличный от первого ответа CLI
func timeShadow(cli string, inputs []shadowInput, envOff, envOn []string, dir string, n int) (bool, error) {
	fail := false
	fmt.Printf("calls: %d each side per subcommand, interleaved\n", n)
	for _, in := range inputs {
		without, with, differ, err := interleave(cli, in, envOff, envOn, dir, n)
		if err != nil {
			return false, err
		}
		delta := p50(with) - p50(without)
		fmt.Printf("%-13s without shadow: %s\n", in.hook, stats(without))
		fmt.Printf("%-13s with shadow:    %s\n", in.hook, stats(with))
		fmt.Printf("%-13s p50 increase %.2f ms (limit %.0f), answers differing from first CLI answer: %d\n", in.hook, ms(delta), ms(shadowP50Limit), differ)
		if delta > shadowP50Limit || differ > 0 {
			fail = true
		}
	}
	return fail, nil
}

// waitJournal ждёт want строк журнала тени не дольше timeout. Не дошедшие к
// сроку строки — пропуски в сводке, не ошибка; ошибка — журнал не прочитан
func waitJournal(path string, want int, timeout time.Duration) error {
	for deadline := time.Now().Add(timeout); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		got, err := countLines(path)
		if err != nil {
			return err
		}
		if got >= want {
			return nil
		}
	}
	return nil
}

// printJournal — сводка журнала тени по подкомандам; возвращает число
// сравнений и расхождений
func printJournal(inputs []shadowInput, byHook map[string][3]int, details []string, n int) (compared, mismatches int) {
	missed := 0
	fmt.Printf("shadow journal (want %d lines):\n", n*len(inputs))
	for _, in := range inputs {
		c := byHook[in.hook]
		compared += c[0] + c[1]
		mismatches += c[1]
		missed += n - c[0] - c[1] - c[2]
		fmt.Printf("  %-13s match %d, mismatch %d, incomparable %d, missed %d\n", in.hook, c[0], c[1], c[2], n-c[0]-c[1]-c[2])
	}
	for _, line := range details {
		fmt.Println("  " + line)
	}
	fmt.Printf("compared %d of %d, mismatches %d, missed %d\n", compared, n*len(inputs), mismatches, missed)
	return compared, mismatches
}

// interleave — n вызовов без тени и n с тенью попеременно; ответы сверяются
// с первым ответом без тени
func interleave(cli string, in shadowInput, envOff, envOn []string, dir string, n int) (without, with []time.Duration, differ int, err error) {
	first, err := runOne(cli, []string{in.hook}, envOff, dir, in.stdin)
	if err != nil {
		return nil, nil, 0, err
	}
	for range n {
		for _, side := range []struct {
			env  []string
			into *[]time.Duration
		}{{envOff, &without}, {envOn, &with}} {
			o, err := runOne(cli, []string{in.hook}, side.env, dir, in.stdin)
			if err != nil {
				return nil, nil, 0, err
			}
			*side.into = append(*side.into, o.elapsed)
			if o.code != first.code || !bytes.Equal(o.stdout, first.stdout) || !bytes.Equal(o.stderr, first.stderr) {
				differ++
			}
		}
	}
	return without, with, differ, nil
}

// deafDaemon — сокет, который принимает соединения и не читает их
type deafDaemon struct {
	listener *net.UnixListener
	held     []net.Conn
	done     chan struct{}
	// acceptErr — приём остановился не закрытием сокета
	acceptErr error
}

func listenDeaf(path string) (*deafDaemon, error) {
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("deaf daemon: %w", err)
	}
	d := &deafDaemon{listener: listener, done: make(chan struct{})}
	go func() {
		defer close(d.done)
		for {
			conn, err := listener.Accept()
			if errors.Is(err, net.ErrClosed) {
				return
			}
			if err != nil {
				d.acceptErr = fmt.Errorf("deaf daemon accept: %w", err)
				return
			}
			d.held = append(d.held, conn)
		}
	}()
	return d, nil
}

// close закрывает сокет и принятые соединения; ошибка — приём сорвался или
// сокет не закрыт
func (d *deafDaemon) close() error {
	err := d.listener.Close()
	<-d.done
	for _, c := range d.held {
		// Соединение мог закрыть CLI: ошибка его закрытия меру не меняет
		err = errors.Join(err, ignoreClosed(c.Close()))
	}
	return errors.Join(d.acceptErr, err)
}

func ignoreClosed(err error) error {
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

// badDaemon — мера 3: сокета нет; демон принимает соединения и не читает,
// а запись (no-prompt с командой в 1 МиБ) больше буфера сокета
func badDaemon(cli, dir string, envOff, envOn []string) (report string, failed bool, err error) {
	// Сокет — в TMPDIR (запрет брифа): путь не длиннее 107 байт при
	// TMPDIR до ~80 байт
	sockDir, err := os.MkdirTemp("", "chooks-deaf-")
	if err != nil {
		return "", false, fmt.Errorf("deaf daemon dir: %w", err)
	}
	defer os.RemoveAll(sockDir)
	deaf := filepath.Join(sockDir, "deaf.sock")
	daemon, err := listenDeaf(deaf)
	if err != nil {
		return "", false, err
	}
	defer func() {
		if closeErr := daemon.close(); closeErr != nil {
			report, failed, err = "", false, errors.Join(err, closeErr)
		}
	}()
	stdin, err := marshal(obj{"session_id": "s1", "hook_event_name": "PermissionRequest", "tool_name": "Bash",
		"tool_input": obj{"command": "echo " + strings.Repeat("a", badDaemonInput)}})
	if err != nil {
		return "", false, err
	}
	in := shadowInput{hook: "no-prompt", stdin: stdin}
	var out strings.Builder
	for _, c := range []struct{ name, socket string }{{"no socket", filepath.Join(sockDir, "none.sock")}, {"daemon not reading", deaf}} {
		off := withSocket(envOff, c.socket)
		without, with, differ, err := interleave(cli, in, off, withSocket(envOn, c.socket), dir, badDaemonCalls)
		if err != nil {
			return "", false, err
		}
		var worst time.Duration
		for i := range with {
			worst = max(worst, with[i]-without[i])
		}
		fmt.Fprintf(&out, "bad daemon, %s (input %d B, %d calls each): without %s; with %s; p50 increase %.1f ms, worst pair %.1f ms (limit %.0f), answers differing %d\n",
			c.name, len(stdin), badDaemonCalls, stats(without), stats(with), ms(p50(with)-p50(without)), ms(worst), ms(badDaemonLimit), differ)
		if worst >= badDaemonLimit || differ > 0 {
			failed = true
		}
	}
	return out.String(), failed, nil
}

func withSocket(env []string, socket string) []string {
	out := slices.DeleteFunc(slices.Clone(env), func(e string) bool { return strings.HasPrefix(e, "CHOOKS_HOOK_SOCKET=") })
	return append(out, "CHOOKS_HOOK_SOCKET="+socket)
}

// journalOutcomes — счёт строк журнала тени по подкоманде: match,
// mismatch, incomparable; details — подробности расхождений и несравнимых
func journalOutcomes(path string) (map[string][3]int, []string, error) {
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, nil, fmt.Errorf("shadow journal: %w", err)
	}
	counts := map[string][3]int{}
	seen := map[string]int{}
	// Пустая подробность — последнее поле строки «match»: TrimSpace срезал
	// бы его вместе с табом
	for line := range strings.SplitSeq(strings.TrimRight(string(data), "\n"), "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 5 {
			continue
		}
		c := counts[f[1]]
		switch f[3] {
		case "match":
			c[0]++
		case "mismatch":
			c[1]++
			seen[f[1]+" mismatch: "+f[4]]++
		case "incomparable":
			c[2]++
			seen[f[1]+" incomparable: "+f[4]]++
		}
		counts[f[1]] = c
	}
	var details []string
	for _, k := range slices.Sorted(maps.Keys(seen)) {
		details = append(details, fmt.Sprintf("%s — %d", k, seen[k]))
	}
	return counts, details, nil
}

// countLines — строк в журнале тени; нет журнала — ноль
func countLines(path string) (int, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("shadow journal: %w", err)
	}
	return bytes.Count(data, []byte("\n")), nil
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// p50 — медиана, как в stats
func p50(d []time.Duration) time.Duration {
	s := slices.Sorted(slices.Values(d))
	if len(s) == 0 {
		return 0
	}
	if len(s)%2 == 0 {
		return (s[len(s)/2-1] + s[len(s)/2]) / 2
	}
	return s[len(s)/2]
}

// keepDaemonLogs копирует журнал тени и лог демона в work и возвращает
// строки отказов демона (запрос или запись оставлены CLI)
func keepDaemonLogs(daemonDir, work string) []string {
	var refusals []string
	for _, name := range []string{"chooks.log", "chooks-shadow.tsv"} {
		data, err := os.ReadFile(filepath.Join(daemonDir, "home", ".claude", "logs", name))
		if err != nil {
			fmt.Fprintln(os.Stderr, "daemon: keep", name+":", err)
			continue
		}
		if err := os.WriteFile(filepath.Join(work, "shadow-daemon-"+name), data, 0o600); err != nil {
			fmt.Fprintln(os.Stderr, "daemon: keep", name+":", err)
		}
		for line := range strings.SplitSeq(string(data), "\n") {
			if strings.Contains(line, "request left to CLI") {
				refusals = append(refusals, line)
			}
		}
	}
	return refusals
}
