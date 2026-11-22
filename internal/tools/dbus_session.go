package tools

import (
	"context"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/aiseeq/chooks/internal/hookctx"
)

var runtimeBusPathRe = regexp.MustCompile(`^/run/user/[0-9]+/bus$`)
var runtimeUserDirRe = regexp.MustCompile(`^/run/user/[0-9]+(/|$)`)

func requestSessionBusAddress(ctx context.Context) string {
	if request, ok := hookctx.Request(ctx); ok {
		return request.Getenv("DBUS_SESSION_BUS_ADDRESS")
	}
	return ""
}

// Unknown dbus-daemon addresses stay protected; known other unix sockets do not.
func dbusSessionBus(p procInfo, requestAddress string) bool {
	if p.comm != "dbus-daemon" {
		return false
	}
	args := strings.Fields(p.cmdline)
	var address string
	for i := 1; i < len(args); i++ {
		switch {
		case args[i] == "--address" && i+1 < len(args):
			i++
			address = args[i]
		case strings.HasPrefix(args[i], "--address="):
			address = strings.TrimPrefix(args[i], "--address=")
		}
	}
	if address == "" || address == "systemd:" {
		return true
	}
	if requestAddress != "" && addressMatches(address, requestAddress) {
		return true
	}
	for endpoint := range strings.SplitSeq(address, ";") {
		if !strings.HasPrefix(endpoint, "unix:") {
			return true
		}
		known := false
		for field := range strings.SplitSeq(strings.TrimPrefix(endpoint, "unix:"), ",") {
			key, value, ok := strings.Cut(field, "=")
			if !ok {
				continue
			}
			switch key {
			case "path":
				known = true
				if runtimeBusPathRe.MatchString(value) {
					return true
				}
			case "tmpdir":
				known = true
				if runtimeUserDirRe.MatchString(filepath.Clean(value)) {
					return true
				}
			case "abstract":
				known = true
				if runtimeUserDirRe.MatchString(value) {
					return true
				}
			}
		}
		if !known {
			return true
		}
	}
	return false
}

func addressMatches(a, b string) bool {
	for left := range strings.SplitSeq(a, ";") {
		if slices.Contains(strings.Split(b, ";"), left) {
			return true
		}
	}
	return false
}
