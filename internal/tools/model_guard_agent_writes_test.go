package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiseeq/chooks/internal/core"
	"mvdan.cc/sh/v3/syntax"
)

// TestModelGuardAgentWriteWhitelist проверяет белый список записи в
// определение агента: известна только одна запись `cat > A <<'EOF'` на файл,
// всё прочее, что пишет или может писать в агента, — не проверить.
func TestModelGuardAgentWriteWhitelist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "approved")
	if err := os.WriteFile(path, []byte("opus claude-opus-5-5\nluna gpt-6-luna\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MODELS_APPROVED", path)
	logger, err := core.NewLogger(core.DefaultLoggerConfig())
	if err != nil {
		t.Fatal(err)
	}
	tool, _ := NewModelGuardTool(core.ToolConfig{Enabled: true}, logger)
	agent := filepath.Join(t.TempDir(), ".claude", "agents", "w.md")
	agents := filepath.Dir(agent)
	settings := filepath.Join(filepath.Dir(agents), "settings.json")
	const (
		opus      = "---\nname: w\nmodel: opus\n---\n"
		opusText  = "---\nname: w\nmodel: opus\n---\nRun `ls` and keep $HOME literal\n"
		fableText = "---\nname: w\nmodel: fable\n---\nRun `ls` and keep $HOME literal\n"
		run       = "claude -p hi"
		pass      = ""
		unknown   = "содержимое изменяемого определения агента не проверить"
		fable     = `модель "fable" не одобрена`
	)
	known := "cat > " + agent + " <<'EOF'\n" + opusText + "EOF\n"
	json := "echo '{\"model\":\"opus\"}' > " + settings + "\n"
	cases := []struct{ name, command, want string }{
		// E1: тело heredoc с кавычками — текст, $ и ` в нём не подстановка
		{"E1 'EOF'", known + run, pass},
		{"E1 \"EOF\"", "cat > " + agent + " <<\"EOF\"\n" + opusText + "EOF\n" + run, pass},
		{"E1 cat -", "cat - > " + agent + " <<'EOF'\n" + opusText + "EOF\n" + run, pass},
		{"E1 >|", "cat >| " + agent + " <<'EOF'\n" + opusText + "EOF\n" + run, pass},
		{"E1 1>", "cat 1> " + agent + " <<'EOF'\n" + opusText + "EOF\n" + run, pass},
		{"E1 fable 'EOF' отказ по модели", "cat > " + agent + " <<'EOF'\n" + fableText + "EOF\n" + run, fable},
		{"E1 fable \"EOF\" отказ по модели", "cat > " + agent + " <<\"EOF\"\n" + fableText + "EOF\n" + run, fable},
		{"escaped newline before second frontmatter", "cat > " + agent + " <<'EOF'\n---\nname: x # \\\\n---\nmodel: fable\n---\nEOF\n" + run, fable},
		{"unquoted без $ и обратной косой", "cat > " + agent + " <<EOF\n" + opus + "EOF\n" + run, pass},
		{"unquoted с экранированным \\$", "cat > " + agent + " <<EOF\n---\nname: \\$W\nmodel: opus\n---\nEOF\n" + run, unknown},
		// H-A: дозапись и запись не только stdout
		{"H-A &>>", "cat &>> " + agent + " <<'EOF'\n" + opusText + "EOF\n" + run, unknown},
		{"&>", "cat &> " + agent + " <<'EOF'\n" + opusText + "EOF\n" + run, unknown},
		{">>", "cat >> " + agent + " <<'EOF'\n" + opus + "EOF\n" + run, unknown},
		{"2> в агента", "cat 2> " + agent + " <<'EOF'\n" + opus + "EOF\n" + run, unknown},
		// H-B: соседний JSON и распознанная запись не снимают пометку
		{"H-B json + printf", known + json + "printf -- '---\\nmodel: fable\\n---\\n' > " + agent + "\n" + run, unknown},
		{"H-B json + cat file", known + json + "cat /tmp/evil.md > " + agent + "\n" + run, unknown},
		{"H-B json + exec 3<<", known + json + "exec 3<<'EOF'\n" + opus + "EOF\ncat <&3 > " + agent + "\n" + run, unknown},
		{"H-B json + { cat; } > A", known + json + "{ cat <<'EOF'\n" + opus + "EOF\n} > " + agent + "\n" + run, unknown},
		// H-C: cat подменён alias, функцией, PATH
		{"H-C shopt + alias", "shopt -s expand_aliases; alias cat='sed s/opus/fable/'\n" + known + run, unknown},
		{"alias без shopt", "alias cat='sed s/opus/fable/'\n" + known + run, unknown},
		{"shopt expand_aliases", "shopt -s expand_aliases\n" + known + run, unknown},
		{"function cat", "function cat { sed s/opus/fable/; }\n" + known + run, unknown},
		{"PATH", "PATH=/tmp/evil:$PATH\n" + known + run, unknown},
		{"присваивание перед cat", "LD_PRELOAD=/tmp/evil.so cat > " + agent + " <<'EOF'\n" + opusText + "EOF\n" + run, unknown},
		// H-D: каталог agents как цель
		{"H-D mv -t", known + "mv -t " + agents + " /tmp/w.md\n" + run, unknown},
		{"H-D install -t", known + "install -t " + agents + " /tmp/w.md\n" + run, unknown},
		{"mv в каталог без косой", "mv /tmp/w.md " + agents + "\n" + run, unknown},
		{"cp в каталог с косой", known + "cp /tmp/w.md " + agents + "/\n" + run, unknown},
		{"cp -r в .claude", "cp -r /tmp/d/agents " + filepath.Dir(agents) + "/\n" + run, unknown},
		// одна запись на файл
		{"две разрешённые записи в один файл", known + known + run, unknown},
		// единственное перенаправление fd 0 — heredoc
		{"heredoc на fd 3", "cat > " + agent + " 3<<'EOF'\n" + opus + "EOF\n" + run, unknown},
		{"stdin из файла после heredoc", "cat > " + agent + " <<'EOF' < /tmp/evil.md\n" + opus + "EOF\n" + run, unknown},
		{"stdin из файла до heredoc", "cat > " + agent + " < /tmp/evil.md <<'EOF'\n" + opus + "EOF\n" + run, unknown},
		{"cat - и файл", "cat - /tmp/evil.md > " + agent + " <<'EOF'\n" + opus + "EOF\n" + run, unknown},
		{"лишнее перенаправление stderr", "cat > " + agent + " 2>/dev/null <<'EOF'\n" + opus + "EOF\n" + run, unknown},
		// прочие писатели и пути
		{"rsync в agents", "rsync -a /tmp/a/ " + agents + "/\n" + run, unknown},
		{"ln в agents", "ln -sf /tmp/evil.md " + agent + "\n" + run, unknown},
		{"python пишет agents", "python3 -c \"open('" + agent + "','w').write('x')\"\n" + run, unknown},
		{"cd в agents", "cd " + agents + " && cp /tmp/evil.md . && " + run, unknown},
		{"переменная с путём", "D=" + agents + "; cp /tmp/evil.md \"$D/\"; " + run, unknown},
		{"путь в цикле", "for f in " + agents + "/*.md; do sed -i s/opus/fable/ \"$f\"; done\n" + run, unknown},
		{"путь через xargs", "ls " + agents + " | xargs -I{} cp /tmp/evil.md {}\n" + run, unknown},
		{"путь в файле", "echo " + agent + " > /tmp/l; xargs -a /tmp/l cp /tmp/evil.md; " + run, unknown},
		{"путь в скрипте heredoc", "sh <<'EOF'\ncp /tmp/evil.md " + agent + "\nEOF\n" + run, unknown},
		{"путь по умолчанию в параметре", "cp /tmp/evil.md \"${D:-" + agents + "}/\"\n" + run, unknown},
		{"опция после назначения cp", "cp /tmp/evil.md " + agent + " -S bak\n" + run, unknown},
		{"python копирует в .claude", "python3 -c \"import shutil; shutil.copytree('/tmp/d', '" + filepath.Dir(agents) + "', dirs_exist_ok=True)\"\n" + run, unknown},
		{"путь из подстановки в cp", "cp /tmp/evil.md \"$(ls -d " + agents + ")/\"\n" + run, unknown},
		{"нелитеральная цель", "cat > \"$PWD" + agent + "\" <<'EOF'\n" + opus + "EOF\n" + run, unknown},
		// чтение агента и промпт с путём — не запись
		{"mkdir и запись", "mkdir -p " + agents + " && " + known + run, pass},
		{"чтение агента", "cat " + agent + "; ls " + agents + " && " + run, pass},
		{"агент в claude по конвейеру", "cat " + agent + " | " + run, pass},
		{"путь в промпте heredoc", "claude -p \"$(cat <<'EOF'\nreview " + agent + "\nEOF\n)\"", pass},
		{"путь в промпте за timeout", "timeout 600 claude -p \"edit " + agent + "\"", pass},
		{"копия агента наружу", known + "cp " + agent + " /tmp/out.md\n" + run, pass},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			input := &core.ToolInput{ToolName: "Bash", Command: c.command}
			result, err := tool.ValidateTool(gitGuardTestContext(t), input)
			if err != nil {
				t.Fatal(err)
			}
			if c.want == pass {
				if !result.IsValid {
					t.Fatalf("отказ, ждали пропуск: %+v", result.Violations)
				}
				return
			}
			if result.IsValid {
				t.Fatalf("пропуск, ждали отказ %q", c.want)
			}
			if !strings.Contains(result.Violations[0].Message, c.want) {
				t.Fatalf("причина %q, ждали %q", result.Violations[0].Message, c.want)
			}
		})
	}
}

// TestWrittenAgentDefinitionsUnparsed: команду, которую разбор не взял,
// с путём агентов не проверить
func TestWrittenAgentDefinitionsUnparsed(t *testing.T) {
	if _, unknown := writtenAgentDefinitionCandidates("cp /tmp/evil.md .claude/agents/x.md; ("); !unknown {
		t.Fatal("неразобранная запись в agents признана известной")
	}
	if _, unknown := writtenAgentDefinitionCandidates("echo hi; ("); unknown {
		t.Fatal("неразобранная команда без пути агентов признана записью")
	}
}

func TestModelGuardUnknownWriteReason(t *testing.T) {
	for _, tc := range []struct {
		command, reason string
		agent           bool
	}{
		{"cp /tmp/x .claude/agents/x.md; claude", ".claude/agents/x.md", true},
		{"echo x > /tmp/.claude/settings.json; claude", "/tmp/.claude/settings.json", false},
	} {
		candidates := writtenProtectedFileCandidates(tc.command)
		found := false
		for _, c := range candidates {
			if c.value == tc.reason && ((tc.agent && c.unknownAgentWrite) || (!tc.agent && c.unknownWrite)) {
				found = true
			}
		}
		if !found {
			t.Errorf("%q: missing reason %q in %+v", tc.command, tc.reason, candidates)
		}
	}
}

func TestModelGuardMentionFollowups(t *testing.T) {
	cases := []struct {
		name, command string
		unknown       bool
	}{
		{"cd read", `cd /home/aisee/.claude && jq -c '{deniedMcpServers, skillOverrides}' settings.json | cut -c1-600; claude mcp list 2>/dev/null | cut -c1-120 | head`, false},
		{"git read", `git -C ~/.claude log && claude`, false},
		{"pushd read", `pushd ~/.claude && claude`, false},
		{"cd redirect", `cd ~/.claude && jq . settings.json > settings.json.new; claude`, true},
		{"cd copy", `cd ~/.claude && cp -a x/. .; claude`, true},
		{"cd move", `cd ~/.claude && mv settings.json.new settings.json; claude`, true},
		{"cd tar", `cd ~/.claude && tar -xf archive.tar; claude`, true},
		{"cd pull", `cd ~/.claude && git pull; claude`, true},
		{"cd heredoc", "cd ~/.claude && cat > settings.json <<'EOF'\n{}\nEOF\nclaude", true},
		{"go field", `cd ~/git/aguard && perl -0pi -e 's|x|cfg.Restart.Claude = claude|' x.go; cl --cl-check`, false},
		{"sudo option", `echo x | sudo -u claude tee .claude/agents/x.md; claude`, true},
		{"exec option", `exec -a claude tee .claude/agents/x.md; claude`, true},
		{"codex output", `codex exec -o .claude/agents/x.md hi; claude`, true},
		{"tree output", `tree -o .claude/agents/x.md; claude`, true},
		{"git diff output", `git diff --output=.claude/agents/x.md; claude`, true},
		{"git show output", `git show HEAD:x --output=.claude/agents/x.md; claude`, true},
		{"timeout option", `timeout -s claude tee .claude/agents/x.md; claude`, true},
		{"env option", `env -u claude tee .claude/agents/x.md; claude`, true},
		{"nice option", `nice -n claude tee .claude/agents/x.md; claude`, true},
		{"ionice option", `ionice -c claude tee .claude/agents/x.md; claude`, true},
		{"stdbuf option", `stdbuf -o claude tee .claude/agents/x.md; claude`, true},
		{"uppercase agent directory", "bash -c \"cat > .claude/Agents/x.md <<'EOF'\n---\nmodel: fable\n---\nEOF\"; claude", true},
		{"uppercase agent extension", "bash -c \"cat > .claude/agents/x.MD <<'EOF'\n---\nmodel: fable\n---\nEOF\"; claude", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, got := writtenAgentDefinitionCandidates(c.command)
			if got != c.unknown {
				t.Fatalf("unknown = %v, want %v", got, c.unknown)
			}
		})
	}
}

func TestModelGuardAgentWriteRegression(t *testing.T) {
	known := "cat > .claude/agents/x.md <<'EOF'\n---\nmodel: opus\n---\nEOF\n"
	cases := []struct {
		name, command string
		unknown       bool
	}{
		{"known cat", known + "claude", false},
		{"hash", "hash -p /tmp/cat cat\n" + known + "claude", true},
		{"enable", "enable -n cat\n" + known + "claude", true},
		{"source", "source /tmp/x\n" + known + "claude", true},
		{"dot", ". /tmp/x\n" + known + "claude", true},
		{"bash env", "BASH_ENV=/tmp/x\n" + known + "claude", true},
		{"env", "ENV=/tmp/x\n" + known + "claude", true},
		{"multiple functions", "function foo cat { echo evil; }\n" + known + "claude", true},
		{"cp trailing option", "cp /tmp/evil.md .claude/agents/x.md -v; claude -p hi", true},
		{"append both", "claude -p hi &>> .claude/agents/x.md", true},
		{"duplicate output", "claude -p hi >&.claude/agents/x.md", true},
		{"substitution output", `claude -p hi > "$(echo .claude/agents/x.md)"`, true},
		{"nested shell", "bash -c \"cat > .claude/agents/x.md <<'EOF'\n---\nmodel: fable\n---\nEOF\"; claude", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			models, got := writtenAgentDefinitionCandidates(c.command)
			if got != c.unknown {
				t.Fatalf("unknown = %v, want %v", got, c.unknown)
			}
			_ = models
		})
	}
}

func TestModelGuardAgentWriteFunctionNames(t *testing.T) {
	scan := &agentWriteScan{}
	scan.visit(&syntax.FuncDecl{Names: []*syntax.Lit{{Value: "other"}, {Value: "cat"}}})
	if !scan.shadow {
		t.Fatal("cat в списке имён функции не распознан как подмена")
	}
}
