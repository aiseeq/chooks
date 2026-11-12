package tools

import (
	"fmt"
	"slices"
	"strings"

	"github.com/aiseeq/akit/shellcmd"
	"mvdan.cc/sh/v3/syntax"
)

// span — участок текста команды [start, end)
type span struct{ start, end int }

func (s span) has(offset int) bool { return s.start <= offset && offset < s.end }

// callSpan — простой вызов в тексте команды и смещение имени программы
type callSpan struct {
	span
	name int
}

// shellScopes — области команды, за которые cd не действует: подоболочка,
// подстановка, звено конвейера, фоновое задание, тело here-doc и простой
// вызов (cd во вложенном скрипте bash -c или за обёрткой вроде timeout)
type shellScopes struct {
	spans []span
	calls []callSpan
}

// commandScopes находит области cd в команде
func commandScopes(command string) (shellScopes, error) {
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(command), "")
	if err != nil {
		return shellScopes{}, fmt.Errorf("разбор областей cd: %w", err)
	}
	var s shellScopes
	add := func(n syntax.Node) {
		s.spans = append(s.spans, span{int(n.Pos().Offset()), int(n.End().Offset())})
	}
	syntax.Walk(file, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.Subshell, *syntax.CmdSubst, *syntax.ProcSubst:
			add(n)
		case *syntax.BinaryCmd:
			if n.Op == syntax.Pipe || n.Op == syntax.PipeAll {
				add(n.X)
				add(n.Y)
			}
		case *syntax.Stmt:
			if n.Background || n.Coprocess {
				add(n)
			}
		case *syntax.Redirect:
			if n.Hdoc != nil {
				add(n.Hdoc)
			}
		case *syntax.CallExpr:
			if len(n.Args) > 0 {
				s.calls = append(s.calls, callSpan{span{int(n.Pos().Offset()), int(n.End().Offset())}, int(n.Args[0].Pos().Offset())})
			}
		}
		return true
	})
	return s, nil
}

// around — области, в которых лежит вызов cd: за ними его каталог не
// действует. Свой простой вызов cd (и за builtin или command) — не область
func (s shellScopes) around(c *shellcmd.Call) []span {
	var out []span
	for _, sp := range s.spans {
		if sp.has(c.Offset) {
			out = append(out, sp)
		}
	}
	builtin := !slices.ContainsFunc(c.Via, func(v string) bool { return v != "builtin" && v != "command" })
	var own *callSpan
	for i := range s.calls {
		cs := &s.calls[i]
		if !cs.has(c.Offset) {
			continue
		}
		if cs.name == c.Offset || builtin && (own == nil || cs.start >= own.start) {
			own = cs
		}
	}
	for i := range s.calls {
		if cs := &s.calls[i]; cs.has(c.Offset) && cs != own {
			out = append(out, cs.span)
		}
	}
	return out
}

// dirChange — смена каталога вызовом cd, pushd или popd: смещение вызова в
// команде (-1 — неизвестно), новый каталог ("" — неизвестен) и области,
// за которые она не действует
type dirChange struct {
	offset int
	dir    string
	within []span
}

// dirAt — каталог вызова со смещением offset: последняя смена каталога
// из changes (вызовы раньше по списку) в той же области. Смещение вызова или смены неизвестно, или
// области не разобраны, а смена в подоболочке — каталог неизвестен
func dirAt(cwd string, offset int, changes []dirChange, unscoped bool) string {
	for _, ch := range changes {
		switch {
		case ch.offset < 0 || unscoped:
			cwd = ""
		case offset < 0:
			// Вызов списком после смены, а в её ли области — не узнать
			cwd = when(len(ch.within) == 0, ch.dir)
		case !slices.ContainsFunc(ch.within, func(sp span) bool { return !sp.has(offset) }):
			cwd = ch.dir
		}
	}
	return cwd
}
