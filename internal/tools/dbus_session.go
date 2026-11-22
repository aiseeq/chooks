package tools

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/aiseeq/chooks/internal/hookctx"
)

var runtimeUserDirRe = regexp.MustCompile(`^/run/user/[0-9]+(/|$)`)

func processArgs(raw []byte) []string {
	line := strings.TrimSuffix(string(raw), "\x00")
	if line == "" {
		return nil
	}
	return strings.Split(line, "\x00")
}

func requestSessionBusAddress(ctx context.Context) string {
	if request, ok := hookctx.Request(ctx); ok {
		return request.Getenv("DBUS_SESSION_BUS_ADDRESS")
	}
	return ""
}

// Be conservative unless argv and its single unix:path endpoint are unambiguous.
func dbusSessionBus(p procInfo, requestAddress string, resolve func(string) (string, error)) (bool, error) {
	if p.comm != "dbus-daemon" {
		return false, nil
	}
	address, ok := daemonAddress(p.argv)
	if !ok {
		return true, nil
	}
	processPath, ok := unixPath(address)
	if !ok {
		return true, nil
	}
	processPath, err := canonicalPath(processPath, resolve)
	if err != nil || runtimeUserDirRe.MatchString(processPath) {
		return true, err
	}
	requestPaths, ok := addressUnixPaths(requestAddress)
	if !ok {
		return true, nil
	}
	for _, requestPath := range requestPaths {
		requestPath, err = canonicalPath(requestPath, resolve)
		if err != nil || processPath == requestPath {
			return true, err
		}
	}
	return false, nil
}

func daemonAddress(argv []string) (string, bool) {
	if len(argv) == 0 || argv[0] == "" {
		return "", false
	}
	var address string
	count := 0
	for i := 1; i < len(argv); i++ {
		arg := argv[i]
		switch {
		case strings.HasPrefix(arg, "--address="):
			count++
			address = strings.TrimPrefix(arg, "--address=")
		case arg == "--address" || arg == "--session" || arg == "--system" ||
			arg == "--systemd-activation" || arg == "--exit-with-session" || arg == "--introspect":
			return "", false
		case arg == "--config-file":
			if i+1 >= len(argv) {
				return "", false
			}
			i++
		case strings.HasPrefix(arg, "--config-file="):
			if strings.TrimPrefix(arg, "--config-file=") == "" {
				return "", false
			}
		case arg == "--fork", arg == "--nofork", arg == "--nopidfile",
			arg == "--nosyslog", arg == "--syslog", arg == "--stderr":
		case arg == "--print-address" || strings.HasPrefix(arg, "--print-address="):
		default:
			return "", false
		}
	}
	return address, count == 1 && address != ""
}

// unixPath parses one process address; multiple transports are ambiguous.
func unixPath(address string) (string, bool) {
	transport, _, ok := strings.Cut(address, ":")
	if !ok || transport != "unix" || strings.Contains(address, ";") {
		return "", false
	}
	paths, ok := addressUnixPaths(address)
	if !ok || len(paths) != 1 {
		return "", false
	}
	return paths[0], true
}

// addressUnixPaths parses transports and comma-separated key=value parameters.
func addressUnixPaths(address string) ([]string, bool) {
	if address == "" {
		return nil, true
	}
	var paths []string
	for endpoint := range strings.SplitSeq(address, ";") {
		transport, params, ok := strings.Cut(endpoint, ":")
		if !ok || transport == "" {
			return nil, false
		}
		if params == "" {
			if transport == "unix" {
				return nil, false
			}
			continue
		}
		seenPath := false
		for param := range strings.SplitSeq(params, ",") {
			key, value, ok := strings.Cut(param, "=")
			if !ok || key == "" || value == "" {
				return nil, false
			}
			if transport == "unix" && key == "path" {
				if seenPath {
					return nil, false
				}
				seenPath = true
				paths = append(paths, value)
			}
		}
	}
	return paths, true
}

func canonicalPath(path string, resolve func(string) (string, error)) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("address path is not absolute: %q", path)
	}
	clean := filepath.Clean(path)
	if resolve == nil {
		resolve = filepath.EvalSymlinks
	}
	resolved, err := resolve(clean)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return clean, nil
		}
		return "", fmt.Errorf("resolve address path %q: %w", clean, err)
	}
	if !filepath.IsAbs(resolved) {
		return "", fmt.Errorf("resolved address path is not absolute: %q", resolved)
	}
	return filepath.Clean(resolved), nil
}
