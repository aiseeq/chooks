// Command chooks-hook — прокси хука: команда хука в settings.json вызывает
// его вместо chooks, он пересылает вызов демону chooks serve, а без демона
// исполняет chooks сам (internal/hookproxy)
package main

import (
	"os"

	"github.com/aiseeq/chooks/internal/hookproxy"
)

func main() {
	os.Exit(hookproxy.Main(os.Args))
}
