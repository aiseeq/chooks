package tools

import (
	"testing"

	"github.com/aiseeq/akit/shellcmd"
)

func TestWideSearchWrappedCommandsDoNotPanic(t *testing.T) {
	prefixes := []string{"timeout 10 ", "sudo ", "nice -n 19 ionice -c3 "}
	commands := []string{
		"find ~ -maxdepth 1 -name x",
		"find ~ -maxdepth 1 -exec du -sh {} \\;",
		"find ~ -maxdepth 1 | xargs -I{} grep -r x {}",
		"find ~ -maxdepth 1 | parallel -j4 du -s",
		"find ~ -maxdepth 1 | parallel 'du -s {}'",
		"timeout 10 find ~ -maxdepth 1 | xargs du -s",
		"find ~ -maxdepth 1 2>&1 | xargs du -s",
		"find ~ -maxdepth 1 | while read d; do du -s \"$d\"; done",
	}
	for _, command := range commands {
		for _, prefix := range prefixes {
			wrapped := prefix + command
			t.Run(wrapped, func(t *testing.T) {
				defer func() {
					if recovered := recover(); recovered != nil {
						t.Fatalf("WideSearch panicked: %v", recovered)
					}
				}()
				_, _ = WideSearch(wrapped, "/tmp", "/home/u")
			})
		}
	}
}

func TestParallelHasUnknownRecursivePathParseError(t *testing.T) {
	call := &shellcmd.Call{Name: "parallel", Args: []string{"du ("}}
	if !parallelHasUnknownRecursivePath(call, "/home/u") {
		t.Fatal("parse error should mean the recursive path is unknown")
	}
}
