package selflaunch

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	for _, arg := range os.Args[1:] {
		if !strings.HasPrefix(arg, "-test.") {
			fmt.Fprintf(os.Stderr, "selflaunch test binary refuses positional argument %q\n", arg)
			os.Exit(2)
		}
	}
	os.Exit(m.Run())
}
