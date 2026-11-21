package core

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aiseeq/chooks/internal/hookctx"
)

func TestLoadConfigForRequest(t *testing.T) {
	root := t.TempDir()
	for _, item := range []struct{ name, level string }{{"first", "debug"}, {"second", "error"}} {
		dir := filepath.Join(root, item.name)
		config := filepath.Join(dir, "config", "chooks", "config.yaml")
		if err := os.MkdirAll(filepath.Dir(config), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(config, []byte("logger:\n  level: "+item.level+"\n  output: file\n  file: ~/logs/chooks.log\n"), 0600); err != nil {
			t.Fatal(err)
		}
		request := hookctx.New(nil, nil, nil, []string{"HOME=" + dir, "XDG_CONFIG_HOME=" + filepath.Join(dir, "config")}, dir, 1, 2)
		got, err := LoadConfigForRequest("", request)
		if err != nil {
			t.Fatal(err)
		}
		if got.Logger.Level != item.level || got.Logger.LogFile != filepath.Join(dir, "logs/chooks.log") {
			t.Errorf("%s: got %+v", item.name, got.Logger)
		}
	}
}
