package tools

import (
	"slices"
	"testing"
)

const sweepHome = "/home/aisee"

func TestCheckCorpusSweepBlocks(t *testing.T) {
	cases := []struct {
		name, command string
		sweeps        []string
	}{
		{"rg по корню", `rg -l foo ~/.claude/projects`, []string{"~/.claude/projects"}},
		{"grep -r абсолютный путь", `grep -r foo /home/aisee/.claude/projects/`, []string{"/home/aisee/.claude/projects/"}},
		{"glob всех сессий", `rg foo ~/.claude/projects/*/*.jsonl`, []string{"~/.claude/projects/*/*.jsonl"}},
		{"find | xargs grep", `find ~/.claude/projects -name '*.jsonl' | xargs grep -l foo`, []string{"~/.claude/projects"}},
		{"find по codex без чтения", `find /home/aisee/.codex/sessions -name '*.jsonl' -newer x`, []string{"/home/aisee/.codex/sessions"}},
		{"cd в корень и rg", `cd ~/.local/state/worker && rg -l foo`, []string{"~/.local/state/worker"}},
		{"xargs -0 rg", `ls ~/.claude/projects/*/*.jsonl | xargs -0 rg foo`, []string{"~/.claude/projects/*/*.jsonl"}},
		{"цикл for", "for f in ~/.claude/projects/*/*.jsonl; do grep -c foo \"$f\"; done", []string{"~/.claude/projects/*/*.jsonl"}},
		{"переменная $HOME", `P=$HOME/.claude/projects; rg foo "$P"`, []string{"$HOME/.claude/projects"}},
		{"bash -c", `bash -c 'rg foo ~/.codex'`, []string{"~/.codex"}},
		{"timeout и папка проекта", `timeout 60 rg foo "${HOME}/.claude/projects/-home-aisee-git-chooks/"`, []string{"${HOME}/.claude/projects/-home-aisee-git-chooks/"}},
		{"относительный путь", `rg foo .claude/projects`, []string{".claude/projects"}},
		{"фигурные скобки", `grep foo ~/.claude/projects/x/{a,b}.jsonl`, []string{"~/.claude/projects/x/{a,b}.jsonl"}},
		{"egrep", `egrep -r 'a|b' ~/.codex/sessions/2026/10`, []string{"~/.codex/sessions/2026/10"}},
		{"ключ со значением через =", `rg foo --glob=*.jsonl ~/.claude/projects`, []string{"~/.claude/projects"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := CheckCorpusSweep(c.command, sweepHome)
			if !v.Blocked {
				t.Fatalf("не остановлено: %q, вердикт %+v", c.command, v)
			}
			if !slices.Equal(v.Sweeps, c.sweeps) {
				t.Fatalf("sweeps %q, ждали %q", v.Sweeps, c.sweeps)
			}
		})
	}
}

func TestCheckCorpusSweepAllows(t *testing.T) {
	cases := []struct {
		name, command string
		points        []string
	}{
		{"один файл сессии", `rg foo ~/.claude/projects/-home-aisee-git-chooks/5de41ca4.jsonl`, []string{"~/.claude/projects/-home-aisee-git-chooks/5de41ca4.jsonl"}},
		{"tl", `tl lead --since 12:00`, nil},
		{"rg по проекту", `rg foo internal/`, nil},
		{"ls корпуса без поиска", `ls ~/.claude/projects`, nil},
		{"конфиг codex", `grep model ~/.codex/config.toml`, []string{"~/.codex/config.toml"}},
		{"лог бойца", `grep -c x ~/.local/state/worker/abc/out.log`, []string{"~/.local/state/worker/abc/out.log"}},
		{"файл через переменные", `rg foo "$HOME/.claude/projects/$P/$S.jsonl"`, []string{"$HOME/.claude/projects/$P/$S.jsonl"}},
		{"путь в тексте echo", `echo "see ~/.claude/projects" | grep see`, nil},
		{"путь в here-doc", "cat > notes.md <<'EOF'\nrg foo ~/.claude/projects\nEOF\ngrep -c rg notes.md", nil},
		{"другой .claude", `rg foo ~/.claude/skills`, nil},
		{"похожее имя", `rg foo ~/.codex-old/x`, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := CheckCorpusSweep(c.command, sweepHome)
			if v.Blocked {
				t.Fatalf("остановлено: %q, вердикт %+v", c.command, v)
			}
			if !slices.Equal(v.Points, c.points) {
				t.Fatalf("points %q, ждали %q", v.Points, c.points)
			}
		})
	}
}
