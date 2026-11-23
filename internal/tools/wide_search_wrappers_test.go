package tools

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"testing"
)

func TestWideSearchWrappedCommandsDoNotPanic(t *testing.T) {
	files, err := filepath.Glob("wide_search*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	prefixes := []string{"timeout 10 ", "sudo ", "nice -n 19 ionice -c3 "}
	commands := make(map[string]bool)
	for _, file := range files {
		f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok || len(lit.Elts) != 4 {
				return true
			}
			for _, el := range lit.Elts[:3] {
				if _, ok := el.(*ast.BasicLit); !ok {
					return true
				}
			}
			command, ok := lit.Elts[1].(*ast.BasicLit)
			if !ok || command.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(command.Value)
			if err == nil {
				commands[value] = true
			}
			return true
		})
	}
	if len(commands) == 0 {
		t.Fatal("no command cases found")
	}
	for command := range commands {
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
