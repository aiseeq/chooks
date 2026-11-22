package tools

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/aiseeq/chooks/internal/hookctx"
)

var (
	runtimeUserDirRe = regexp.MustCompile(`^/run/user/`)
	dbusUnixPathRe   = regexp.MustCompile(`^unix:path=(/[A-Za-z0-9._/-]+)$`)
)

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

func dbusSessionBus(p procInfo, requestAddress string, resolve func(string) (string, error), lstat func(string) (fs.FileInfo, error)) (bool, error) {
	if p.comm != "dbus-daemon" {
		return false, nil
	}
	address, ok := daemonAddress(p.argv)
	if !ok {
		return true, nil
	}
	path, ok := dbusUnixPath(address)
	if !ok || filepath.Clean(path) != path || runtimeUserDirRe.MatchString(path) {
		return true, nil
	}
	for part := range strings.SplitSeq(path, "/") {
		if part == "." || part == ".." {
			return true, nil
		}
	}
	dir := filepath.Dir(path)
	if resolve == nil {
		resolve = filepath.EvalSymlinks
	}
	resolved, err := resolve(dir)
	if err != nil || resolved != dir {
		return true, err
	}
	if lstat == nil {
		lstat = os.Lstat
	}
	info, err := lstat(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return true, err
	}
	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		return true, nil
	}
	requestPath, parsed := dbusUnixPath(requestAddress)
	if parsed {
		return path == requestPath, nil
	}
	return strings.Contains(requestAddress, path), nil
}

func daemonAddress(argv []string) (string, bool) {
	if len(argv) == 0 || argv[0] == "" {
		return "", false
	}
	address, count := "", 0
	for i := 1; i < len(argv); i++ {
		a := argv[i]
		switch {
		case strings.HasPrefix(a, "--address="):
			address, count = strings.TrimPrefix(a, "--address="), count+1
		case a == "--address" || a == "--session" || a == "--system" || a == "--print-address" || strings.HasPrefix(a, "--print-address="):
			return "", false
		case a == "--config-file":
			if i+1 >= len(argv) || argv[i+1] == "" || strings.HasPrefix(argv[i+1], "-") {
				return "", false
			}
			i++
		case strings.HasPrefix(a, "--config-file="):
			if strings.TrimPrefix(a, "--config-file=") == "" {
				return "", false
			}
		case a == "--fork" || a == "--nofork" || a == "--nopidfile" || a == "--nosyslog" || a == "--syslog" || a == "--stderr":
		default:
			return "", false
		}
	}
	return address, count == 1 && address != ""
}

func dbusUnixPath(address string) (string, bool) {
	m := dbusUnixPathRe.FindStringSubmatch(address)
	if m == nil {
		return "", false
	}
	return m[1], true
}
