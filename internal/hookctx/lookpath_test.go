package hookctx

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// lookPathTree — cwd запроса с tools/tool и cwd исполнителя с такой же
// приманкой: найденный путь показывает, от чьего cwd шёл поиск
func lookPathTree(t *testing.T) (request, executor string) {
	t.Helper()
	root := t.TempDir()
	request = filepath.Join(root, "request")
	executor = filepath.Join(root, "executor")
	for _, dir := range []string{request, executor} {
		if err := os.MkdirAll(filepath.Join(dir, "tools"), 0o700); err != nil {
			t.Fatal(err)
		}
		for _, file := range []string{filepath.Join(dir, "tools", "tool"), filepath.Join(dir, "tool")} {
			if err := os.WriteFile(file, []byte("#!/bin/sh\n"), 0o700); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Chdir(executor)
	// Abs берёт cwd запроса без симлинков: найденный путь — от него
	real, err := filepath.EvalSymlinks(request)
	if err != nil {
		t.Fatal(err)
	}
	return real, executor
}

func TestLookPathRequestGodebug(t *testing.T) {
	request, _ := lookPathTree(t)
	cases := []struct {
		path, godebug string
		want          string
		err           error
	}{
		{"tools", "", "", exec.ErrDot},
		{"tools", "execerrdot=0", filepath.Join(request, "tools", "tool"), nil},
		{"tools", "execerrdot=0,execerrdot=1", "", exec.ErrDot},
		{"tools", "execerrdot=1,execerrdot=0", filepath.Join(request, "tools", "tool"), nil},
		{"tools", "http2debug=1,execerrdot=0#T", filepath.Join(request, "tools", "tool"), nil},
		{":/nonexistent", "execerrdot=0", filepath.Join(request, "tool"), nil},
		{":/nonexistent", "", "", exec.ErrDot},
		{"/nonexistent", "execerrdot=0", "", exec.ErrNotFound},
		{filepath.Join(request, "tools"), "", filepath.Join(request, "tools", "tool"), nil},
	}
	for _, tc := range cases {
		env := []string{"PATH=" + tc.path}
		if tc.godebug != "" {
			env = append(env, "GODEBUG="+tc.godebug)
		}
		got, err := New(nil, nil, nil, env, request, 1, 2).LookPath("tool")
		if got != tc.want || !errors.Is(err, tc.err) {
			t.Errorf("PATH=%q GODEBUG=%q: LookPath = %q, %v; want %q, %v", tc.path, tc.godebug, got, err, tc.want, tc.err)
		}
	}
}

// У CLI поиск — exec.LookPath процесса с его PATH, GODEBUG и cwd, как в базе
func TestLookPathProcessIsExecLookPath(t *testing.T) {
	lookPathTree(t)
	t.Setenv("PATH", "tools")
	for _, godebug := range []string{"execerrdot=0", "execerrdot=1"} {
		t.Setenv("GODEBUG", godebug)
		request, err := FromProcess()
		if err != nil {
			t.Fatal(err)
		}
		got, gotErr := request.LookPath("tool")
		want, wantErr := exec.LookPath("tool")
		if got != want || (gotErr == nil) != (wantErr == nil) || (wantErr != nil && gotErr.Error() != wantErr.Error()) {
			t.Errorf("GODEBUG=%s: LookPath = %q, %v; exec.LookPath = %q, %v", godebug, got, gotErr, want, wantErr)
		}
		if godebug == "execerrdot=0" && got != filepath.Join("tools", "tool") {
			t.Errorf("GODEBUG=%s: LookPath = %q, want relative tools/tool as in base", godebug, got)
		}
	}
}
