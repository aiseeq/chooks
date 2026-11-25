package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// runTiming — N последовательных pre-tool-use `git status`: сначала CLI,
// потом прокси к демону; каждый ответ сверяется с первым ответом CLI.
// Старт прокси меряется отдельно: без сокета он сразу исполняет CLI, здесь
// /bin/true; разница с прямым /bin/true — цена старта прокси
func runTiming(argv []string) (code int, retErr error) {
	fsFlags := flag.NewFlagSet("timing", flag.ContinueOnError)
	var bin binaries
	bin.register(fsFlags)
	n := fsFlags.Int("n", 200, "число вызовов каждой стороны")
	mode := fsFlags.String("mode", "proxy", "proxy — CLI против прокси+демона; shadow — CLI с тенью и без неё")
	if err := fsFlags.Parse(argv); err != nil {
		return 2, fmt.Errorf("flags: %w", err)
	}
	switch *mode {
	case "proxy":
	case "shadow":
		return runShadowTiming(bin, *n)
	default:
		return 2, fmt.Errorf("unknown --mode %q", *mode)
	}
	if err := bin.resolve(); err != nil {
		return 2, err
	}
	stagedCLI, stagedProxy, err := prepareBinaries(bin.work, bin.cli, bin.proxy)
	if err != nil {
		return 2, err
	}
	bin.cli, bin.proxy = stagedCLI, stagedProxy
	root := filepath.Join(bin.work, "sandbox")
	if err := os.RemoveAll(root); err != nil {
		return 2, fmt.Errorf("reset sandbox: %w", err)
	}
	home, project, run := filepath.Join(root, "home"), filepath.Join(root, "work"), filepath.Join(root, "run")
	for _, d := range []string{home + "/.config/chooks", home + "/.config/models", home + "/.claude/logs", home + "/.claude/projects/-work", home + "/bin", project, run} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return 2, fmt.Errorf("sandbox: %w", err)
		}
	}
	transcript := home + "/.claude/projects/-work/s1.jsonl"
	fallbackLog := filepath.Join(bin.work, "fallbacks")
	wrapper := filepath.Join(bin.work, "cli-fallback.sh")
	files := map[string]string{
		home + "/.config/chooks/config.yaml": "logger:\n  level: info\n  output: file\n  file: ~/.claude/logs/chooks.log\n",
		home + "/.config/models/approved":    "opus claude-opus-5-5\n",
		transcript:                           "",
		fallbackLog:                          "",
		wrapper:                              "#!/bin/sh\nprintf '%s\\n' \"$CHOOKS_FALLBACK\" >> " + strconv.Quote(fallbackLog) + "\nexec " + strconv.Quote(bin.cli) + " \"$@\"\n",
	}
	for path, body := range files {
		if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
			return 2, fmt.Errorf("sandbox file: %w", err)
		}
	}
	stdin, err := marshal(obj{"session_id": "s1", "cwd": project, "transcript_path": transcript, "hook_event_name": "PreToolUse",
		"tool_name": "Bash", "tool_input": obj{"command": "git status"}})
	if err != nil {
		return 2, err
	}
	logFile, err := createDaemonLog(bin.work)
	if err != nil {
		return 2, err
	}
	defer closeDaemonLog(&retErr, logFile)
	d, err := startDaemon(bin.cli, logFile, "")
	if err != nil {
		return 2, err
	}
	env := []string{"HOME=" + home, "XDG_RUNTIME_DIR=" + run, "PATH=" + home + "/bin:/usr/bin:/bin", "LANG=C.UTF-8",
		"CHOOKS_HOOK_SOCKET=" + d.socket, "CHOOKS_HOOK_CLI=" + wrapper}
	m, err := measure(bin, env, project, stdin, d.socket, *n)
	daemonExit := d.stop()
	if err != nil {
		return 2, err
	}
	logged, err := os.ReadFile(fallbackLog)
	if err != nil {
		return 2, fmt.Errorf("fallback log: %w", err)
	}
	fallbacks := strings.Fields(string(logged))
	fmt.Printf("calls: %d each, answer code %d, stdout %d B, stderr %d B\n", *n, m.want.code, len(m.want.stdout), len(m.want.stderr))
	fmt.Printf("CLI chooks pre-tool-use:    %s\n", stats(m.cli))
	fmt.Printf("proxy+daemon:               %s\n", stats(m.proxy))
	fmt.Printf("/bin/true:                  %s\n", stats(m.trueRun))
	fmt.Printf("proxy start, no socket -> /bin/true: %s\n", stats(m.proxyStart))
	fmt.Printf("answers differing from CLI: %d\n", m.differ)
	fmt.Printf("proxy CLI fallbacks: %d\n", len(fallbacks))
	fmt.Printf("daemon exit: %d\n", daemonExit)
	if m.differ > 0 || len(fallbacks) > 0 || daemonExit != 0 {
		return 1, nil
	}
	return 0, nil
}

type measures struct {
	want                            outcome
	cli, proxy, trueRun, proxyStart []time.Duration
	differ                          int
}

func measure(bin binaries, env []string, dir string, stdin []byte, socket string, n int) (measures, error) {
	var m measures
	var err error
	if m.want, err = runOne(bin.cli, []string{"pre-tool-use"}, env, dir, stdin); err != nil {
		return m, err
	}
	same := func(o outcome) bool {
		return o.code == m.want.code && bytes.Equal(o.stdout, m.want.stdout) && bytes.Equal(o.stderr, m.want.stderr)
	}
	for _, side := range []struct {
		binary string
		into   *[]time.Duration
	}{{bin.cli, &m.cli}, {bin.proxy, &m.proxy}} {
		for range n {
			o, err := runOne(side.binary, []string{"pre-tool-use"}, env, dir, stdin)
			if err != nil {
				return m, err
			}
			*side.into = append(*side.into, o.elapsed)
			if !same(o) {
				m.differ++
			}
		}
	}
	startEnv := []string{"CHOOKS_HOOK_SOCKET=" + filepath.Join(filepath.Dir(socket), "none.sock"), "CHOOKS_HOOK_CLI=/bin/true"}
	for range n {
		o, err := runOne("/bin/true", nil, nil, dir, nil)
		if err != nil {
			return m, err
		}
		m.trueRun = append(m.trueRun, o.elapsed)
		if o, err = runOne(bin.proxy, []string{"pre-tool-use"}, startEnv, dir, nil); err != nil {
			return m, err
		}
		m.proxyStart = append(m.proxyStart, o.elapsed)
	}
	return m, nil
}

// stats — p50, p95, min и max в мс; p95 — ближайший ранг снизу, как в
// прежнем скрипте
func stats(d []time.Duration) string {
	if len(d) == 0 {
		return "no samples"
	}
	s := slices.Sorted(slices.Values(d))
	ms := func(x time.Duration) float64 { return float64(x.Microseconds()) / 1000 }
	median := ms(s[len(s)/2])
	if len(s)%2 == 0 {
		median = (ms(s[len(s)/2-1]) + ms(s[len(s)/2])) / 2
	}
	return fmt.Sprintf("p50 %.1f ms, p95 %.1f ms, min %.1f, max %.1f", median, ms(s[int(0.95*float64(len(s)-1))]), ms(s[0]), ms(s[len(s)-1]))
}
