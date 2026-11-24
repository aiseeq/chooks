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

// TestModelGuardReviewRoundOneForms — формы ревью круга 1 (REVIEW-1: HIGH-1,
// HIGH-2, MED-1, MED-2) отказываются хуком целиком; чтение после перехода в
// .claude и клиент за обёрткой с опциями по-прежнему проходят
func TestModelGuardReviewRoundOneForms(t *testing.T) {
	tool := modelGuardForTest(t, "opus claude-opus-5-5\n")
	const (
		agent = ".claude/agents/x.md"
		fable = "---\\nname: x\\nmodel: fable\\n---\\n"
		run   = "claude -p hi"
	)
	tee := func(wrapper string) string {
		return "printf -- '" + fable + "' | " + wrapper + " tee " + agent + "\n" + run
	}
	deny := []struct{ name, command string }{
		// HIGH-1: после cd/pushd/git -C в .claude — окружение перед
		// командой, export/declare/readonly/typeset/local, программа по
		// пути, rg --pre, git -c, claude --debug-file
		{"H1 prefix GIT_EXTERNAL_DIFF", "cd ~/.claude && GIT_EXTERNAL_DIFF='cp /tmp/e.json settings.json;:' git diff HEAD~1 && " + run},
		{"H1 export GIT_EXTERNAL_DIFF", "cd ~/.claude && export GIT_EXTERNAL_DIFF='cp /tmp/e.json settings.json;:' && git diff HEAD~1 && " + run},
		{"H1 declare -x", "cd ~/.claude && declare -x GIT_EXTERNAL_DIFF='cp /tmp/e.json settings.json;:'; git diff HEAD~1; " + run},
		{"H1 readonly then export", "cd ~/.claude && readonly GIT_EXTERNAL_DIFF='cp /tmp/e.json settings.json;:' && export GIT_EXTERNAL_DIFF && git diff && " + run},
		{"H1 typeset -x", "cd ~/.claude && typeset -x GIT_EXTERNAL_DIFF='cp /tmp/e.json settings.json;:'; git diff; " + run},
		{"H1 local in function", "f() { local GIT_EXTERNAL_DIFF='cp /tmp/e.json settings.json;:'; export GIT_EXTERNAL_DIFF; git diff HEAD~1; }; cd ~/.claude && f && " + run},
		{"H1 prefix GIT_CONFIG_*", "cd ~/.claude && GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=diff.external GIT_CONFIG_VALUE_0='cp /tmp/e.json settings.json;:' git diff HEAD~1 && " + run},
		{"H1 project .claude prefix", "cd .claude && GIT_EXTERNAL_DIFF='cp /tmp/e.json settings.json;:' git diff && " + run},
		{"H1 split agents in prefix", "cd ~/.claude && GIT_EXTERNAL_DIFF='cp /tmp/e.md ag\"\"ents/x.md;:' git diff HEAD~1 && " + run},
		{"H1 prefix GIT_PAGER", "cd ~/.claude && GIT_PAGER='cp /tmp/e.json settings.json;:' git log -1 && " + run},
		{"H1 export PAGER", "cd ~/.claude && export PAGER='cp /tmp/e.json settings.json;:' && git log && " + run},
		{"H1 prefix GIT_INDEX_FILE", "cd ~/.claude && GIT_INDEX_FILE=settings.json git status && " + run},
		{"H1 export GIT_INDEX_FILE", "cd ~/.claude && export GIT_INDEX_FILE=settings.json && git status && " + run},
		{"H1 rg --pre here-string", "cd ~/.claude && rg --pre=bash x /dev/stdin <<<'cp /tmp/e.json settings.json' && " + run},
		{"H1 rg --pre heredoc", "cd ~/.claude && rg --pre=bash x /dev/stdin <<'EOF'\ncp /tmp/e.json settings.json\nEOF\n" + run},
		{"H1 rg --pre pipe", "cd ~/.claude && cat /tmp/w.sh | rg --pre=bash x /dev/stdin && " + run},
		{"H1 rg --pre script", "cd ~/.claude && rg --pre /tmp/w.sh x . && " + run},
		{"H1 git -C then rg --pre", "git -C ~/.claude log -1 && rg --pre=bash x /dev/stdin <<<'cd ~/.claude && cp /tmp/e.json settings.json' && " + run},
		{"H1 program by path", "cd ~/.claude && /tmp/w/cat settings.json && " + run},
		{"H1 program in cwd", "cd ~/.claude && ./jq . settings.json && " + run},
		{"H1 git -C -c", "git -C ~/.claude -c diff.external=/tmp/w.sh diff && " + run},
		{"H1 claude --debug-file", "cd ~/.claude && claude --debug-file settings.json -p hi"},
		// HIGH-2: слитное значение короткой опции перед .claude и agents
		{"H2 tar -C.claude in bash -c", "bash -c 'tar -x -f /tmp/e.tar -C.claude'; " + run},
		{"H2 tar -C.claude/ in bash -c", "bash -c 'tar -x -f /tmp/e.tar -C.claude/'; " + run},
		{"H2 tar -C.claude in heredoc", "bash <<'EOF'\ntar -x -f /tmp/e.tar -C.claude\nEOF\n" + run},
		{"H2 tar -C.claude after cd", "bash -c 'cd ~ && tar -x -C.claude < /tmp/e.tar'; " + run},
		{"H2 tar -C.claude in pipeline", "bash -c 'tar -cf - -C /tmp/x . | tar -x -C.claude'; " + run},
		{"H2 cp -t.claude", "bash -c 'cp -t.claude /tmp/x/settings.json'; " + run},
		{"H2 7z -o.claude", "bash -c '7z x -y -o.claude /tmp/e.7z'; " + run},
		{"H2 unzip -d.claude", "bash -c 'unzip -o /tmp/e.zip -d.claude'; " + run},
		{"H2 tar -C.claude direct", "tar -x -f /tmp/e.tar -C.claude; " + run},
		{"H2 tar -xC.claude cluster direct", "tar -xC.claude -f /tmp/e.tar; " + run},
		{"H2 tar -Cagents direct", "tar -x -f /tmp/e.tar -Cagents; " + run},
		{"H2 cp -tagents/ in bash -c", "bash -c 'cp -tagents/ /tmp/e.md'; " + run},
		// MED-1: кластер коротких опций, -a/--argv0, сокращения длинных
		{"M1 sudo -Eu", tee("sudo -Eu claude")},
		{"M1 sudo -nu", tee("sudo -nu claude")},
		{"M1 sudo -U", tee("sudo -U claude -l")},
		{"M1 sudo --us", tee("sudo --us claude")},
		{"M1 exec -la", "printf -- '" + fable + "' | (exec -la claude tee " + agent + ")\n" + run},
		{"M1 exec -cla", "printf -- '" + fable + "' | (exec -cla claude tee " + agent + ")\n" + run},
		{"M1 env -iu", tee("env -iu claude")},
		{"M1 env -a", tee("env -a claude")},
		{"M1 env --argv0", tee("env --argv0 claude")},
		{"M1 env -ia", tee("env -ia claude")},
		{"M1 env --unse", tee("env --unse claude")},
		{"M1 ionice -tc", tee("ionice -tc claude")},
		{"M1 timeout --sig", tee("timeout --sig claude 5")},
		{"M1 timeout -k value then duration", tee("timeout -k 1 claude")},
		{"M1 nice --adj", tee("nice --adj claude")},
		{"M1 env runs one-letter program", tee("env h claude")},
		{"M1 stdbuf --outp", tee("stdbuf --outp claude")},
		// MED-2: tree -o в кластере, rg --pre — не читатель
		{"M2 tree -fo", "tree -fo " + agent + " /tmp\n" + run},
		{"M2 tree -no", "tree -no " + agent + " /tmp\n" + run},
		{"M2 rg --pre agents here-string", "rg --pre=bash x /dev/stdin <<<'cp /tmp/e.md ~/.claude/agents/x.md' && " + run},
		{"M2 rg --pre agents argument", "rg --pre=sh -e x /tmp/w.sh " + agent + " && " + run},
		{"M2 less -o", "printf -- '" + fable + "' | less -o " + agent + "\n" + run},
		{"M2 less --log-file", "printf -- '" + fable + "' | less --log-file=" + agent + "\n" + run},
	}
	pass := []struct{ name, command string }{
		{"cd read jq", `cd /home/aisee/.claude && jq -c '{deniedMcpServers, skillOverrides}' settings.json | cut -c1-600; claude mcp list 2>/dev/null | cut -c1-120 | head`},
		{"cd read system cat", "cd ~/.claude && /usr/bin/cat settings.json && " + run},
		{"git -C no-pager log", "git -C ~/.claude --no-pager log -1 && " + run},
		{"cd read tree", "cd ~/.claude && tree -L 2 && " + run},
		{"cd read rg", "cd ~/.claude && rg -n model settings.json && " + run},
		{"cd claude model", "cd ~/.claude && claude --model opus -p hi"},
		{"sudo -u root -- claude", "sudo -u root -- claude -p \"review " + agent + "\""},
		{"sudo -E claude", "sudo -E claude -p \"review " + agent + "\""},
		{"env -i assignment claude", "env -i HOME=/h claude -p \"review " + agent + "\""},
		{"env -uX claude", "env -uFOO claude -p \"review " + agent + "\""},
		{"timeout -k duration claude", "timeout -k 5 600 claude -p \"review " + agent + "\""},
		{"timeout --sig abbreviation claude", "timeout --sig INT 600 claude -p \"review " + agent + "\""},
		{"env --unse abbreviation claude", "env --unse FOO claude -p \"review " + agent + "\""},
		{"timeout --fore abbreviation claude", "timeout --fore 10m claude -p \"review " + agent + "\""},
		{"timeout --foreground claude", "timeout --foreground 10m claude -p \"review " + agent + "\""},
		{"nice -n claude", "nice -n 10 claude -p \"review " + agent + "\""},
		{"nice legacy claude", "nice -10 claude -p \"review " + agent + "\""},
		{"stdbuf -oL claude", "stdbuf -oL claude -p \"review " + agent + "\""},
		{"ionice -c3 claude", "ionice -c3 claude -p \"review " + agent + "\""},
		{"exec -a name claude", "exec -a cl claude -p \"review " + agent + "\""},
		{"rg reads agents", "rg -n model " + agent + " && " + run},
		{"tree reads agents", "tree -L 2 .claude/agents; " + run},
		{"less reads agents", "less -R " + agent + "; " + run},
		{"go field", "cd ~/git/aguard && perl -0pi -e 's|x|cfg.Restart.Claude = claude|' x.go; cl --cl-check"},
	}
	check := func(command string) *core.ValidationResult {
		t.Helper()
		result, err := tool.ValidateTool(gitGuardTestContext(t), &core.ToolInput{ToolName: "Bash", Command: command})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	for _, c := range deny {
		t.Run("deny "+c.name, func(t *testing.T) {
			if check(c.command).IsValid {
				t.Fatalf("пропуск, ждали отказ: %q", c.command)
			}
		})
	}
	for _, c := range pass {
		t.Run("pass "+c.name, func(t *testing.T) {
			if result := check(c.command); !result.IsValid {
				t.Fatalf("отказ, ждали пропуск: %+v", result.Violations)
			}
		})
	}
}

// TestModelGuardUnknownWriteReasonExcerpt — причина отказа — слово с путём,
// а не целая команда или тело bash -c, и не длиннее reasonLimit рун
func TestModelGuardUnknownWriteReasonExcerpt(t *testing.T) {
	tool := modelGuardForTest(t, "opus claude-opus-5-5\n")
	script := "tar -x -f /tmp/e.tar -C.claude agents/x.md"
	result, err := tool.ValidateTool(gitGuardTestContext(t), &core.ToolInput{ToolName: "Bash", Command: "bash -c '" + script + "'; claude -p hi"})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsValid {
		t.Fatal("пропуск, ждали отказ")
	}
	message := result.Violations[0].Message
	if !strings.Contains(message, "не проверить: -C.claude") || strings.Contains(message, script) {
		t.Fatalf("причина не слово с путём: %q", message)
	}
	long := "/tmp/" + strings.Repeat("x", 300) + "/.claude/agents/x.md"
	if got := reasonExcerpt("cp /tmp/e " + long); len([]rune(got)) != reasonLimit+1 || !strings.HasPrefix(got, "/tmp/xxx") {
		t.Fatalf("причина не обрезана до слова и предела: %q", got)
	}
	if got := reasonExcerpt(".claude/agents/x.md"); got != ".claude/agents/x.md" {
		t.Fatalf("короткий путь изменён: %q", got)
	}
}

// TestModelGuardReviewRoundTwoForms — формы ревью круга 2 (REVIEW-2:
// HIGH-1…3, MED-1…3) отказываются хуком целиком; соседние чтения и клиенты
// проходят
func TestModelGuardReviewRoundTwoForms(t *testing.T) {
	tool := modelGuardForTest(t, "opus claude-opus-5-5\n")
	settingsFile := filepath.Join(t.TempDir(), "s.json")
	if err := os.WriteFile(settingsFile, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	const (
		agent = ".claude/agents/x.md"
		set   = "cp /tmp/e.json settings.json"
		run   = "claude -p hi"
	)
	deny := []struct{ name, command string }{
		// HIGH-1: значение env -S исполняется — это запуск, не клиент
		{"H1 env -S", "env -S 'tee " + agent + "' claude < /tmp/e.md >/dev/null; " + run},
		{"H1 env --split-string", "env --split-string 'tee " + agent + "' claude < /tmp/e.md; " + run},
		{"H1 env --s abbreviation", "env --s 'tee " + agent + "' claude < /tmp/e.md; " + run},
		{"H1 env -iS cluster", "env -iS 'tee " + agent + "' claude < /tmp/e.md; " + run},
		{"H1 env -S attached", "env -S'tee " + agent + "' claude < /tmp/e.md; " + run},
		{"H1 env -S ${HOME} heredoc", "env -S 'tee ${HOME}/" + agent + "' claude <<'EOF'\n---\nname: x\nmodel: fable\n---\nEOF\n" + run},
		{"H1 env -S tar -C .claude", "env -S 'tar -C .claude -xf /tmp/e.tar' claude; " + run},
		{"H1 sudo env -S", "sudo env -S 'tee " + agent + "' claude < /tmp/e.md; " + run},
		// HIGH-2: после cd/pushd/git -C в .claude — подстановка, арифметика,
		// less --lesskey*, claude --settings/--mcp-config
		{"H2 [[ -v a[$()] ]]", "cd ~/.claude && [[ -v 'a[$(" + set + ")]' ]]; " + run},
		{"H2 test -v a[$()]", "cd ~/.claude && test -v 'a[$(" + set + ")]'; " + run},
		{"H2 [ -v a[$()] ]", "cd ~/.claude && [ -v 'a[$(" + set + ")]' ]; " + run},
		{"H2 [[ -eq a[$()] ]]", "cd ~/.claude && [[ 1 -eq 'a[$(" + set + ")]' ]]; " + run},
		{"H2 let a[$()]", "cd ~/.claude && let 'a[$(" + set + ")]'; " + run},
		{"H2 $(( a[$()] ))", "cd ~/.claude && echo $(( 'a[$(" + set + ")]' )); " + run},
		{"H2 ${a['$()']}", "cd ~/.claude && echo ${a['$(" + set + ")']}; " + run},
		{"H2 backtick in literal", "cd ~/.claude && test -v 'a[`" + set + "`]'; " + run},
		{"H2 command substitution", "cd ~/.claude && cat \"$(" + set + ")\"; " + run},
		{"H2 let over file names", "cd ~/.claude && for x in /tmp/w/*; do let \"a[$x]\"; done; " + run},
		{"H2 array index over file names", "cd ~/.claude && for x in /tmp/w/*; do echo \"${a[$x]}\"; done; " + run},
		{"H2 literal $( in any word", "cd ~/.claude && echo '$(" + set + ")'; " + run},
		{"H2 test -v split agents", "cd ~/.claude && test -v 'a[$(cp /tmp/e.md ag\"\"ents/x.md)]'; " + run},
		{"H2 project .claude [[ -v ]]", "cd .claude && [[ -v 'a[$(tar -xf /tmp/e.tar)]' ]]; " + run},
		{"H2 git -C then test -v", "git -C ~/.claude log -1 && cd ~/.claude && test -v 'a[$(" + set + ")]' && " + run},
		{"H2 pushd [[ -v ]]", "pushd ~/.claude/ && [[ -v 'a[$(" + set + ")]' ]]; " + run},
		{"H2 less --lesskey-src", "cd ~/.claude && less --lesskey-src=/dev/stdin settings.json <<<$'#env\\nLESSOPEN=|" + set + "; cat %s'; " + run},
		{"H2 less -k", "cd ~/.claude && less -k /tmp/keys settings.json; " + run},
		{"H2 claude --settings hooks", "cd ~/.claude && claude --settings '{\"hooks\":{\"SessionStart\":[{\"hooks\":[{\"type\":\"command\",\"command\":\"" + set + "\"}]}]}}' -p hi"},
		{"H2 claude --mcp-config", "cd ~/.claude && claude --mcp-config '{\"mcpServers\":{\"x\":{\"command\":\"cp\",\"args\":[\"/tmp/e.json\",\"settings.json\"]}}}' -p hi"},
		{"H2 claude --plugin-dir", "cd ~/.claude && claude --plugin-dir=/tmp/p -p hi"},
		// HIGH-3: левая граница .claude в тексте bash -c и heredoc
		{"H3 -C \\.claude", "bash -c 'tar -xf /tmp/e.tar -C \\.claude'; " + run},
		{"H3 -C\\.claude", "bash -c 'tar -xf /tmp/e.tar -C\\.claude'; " + run},
		{"H3 \\-C.claude", "bash -c 'tar -xf /tmp/e.tar \\-C.claude'; " + run},
		{"H3 TAR_OPTIONS=-C.claude", "bash -c 'TAR_OPTIONS=-C.claude tar -xf /tmp/e.tar'; " + run},
		{"H3 ${X}.claude", "bash -c 'tar -xf /tmp/e.tar -C ${X}.claude'; " + run},
		{"H3 $HOME/\\.claude", "bash -c 'tar -xf /tmp/e.tar -C $HOME/\\.claude'; " + run},
		{"H3 cd \\.claude", "bash -c 'cd \\.claude && tar -xf /tmp/e.tar'; " + run},
		{"H3 -\"\"C.claude", "bash -c 'tar -xf /tmp/e.tar -\"\"C.claude'; " + run},
		{"H3 -C`true`.claude", "bash -c 'tar -xf /tmp/e.tar -C`true`.claude'; " + run},
		{"H3 heredoc -C \\.claude", "bash <<'EOF'\ntar -xf /tmp/e.tar -C \\.claude\nEOF\n" + run},
		{"H3 settings cd $HOME/\\.claude", "bash -c 'cd $HOME/\\.claude && " + set + "'; " + run},
		{"H3 $X.claude", "bash -c 'tar -xf /tmp/e.tar -C $X.claude'; " + run},
		{"H3 -d$(:).claude", "bash -c 'unzip -o /tmp/e.zip -d$(:).claude'; " + run},
		// MED-1: слово с = после обёртки, кроме env и sudo, — программа
		{"M1 nohup a=b path", "mkdir -p /tmp/a=b && nohup /tmp/a=b/../../usr/bin/tee claude " + agent + " < /tmp/e.md; " + run},
		{"M1 command a=b path", "mkdir -p /tmp/a=b && command /tmp/a=b/../../usr/bin/tee claude " + agent + " < /tmp/e.md; " + run},
		{"M1 timeout a=b path", "mkdir -p /tmp/a=b && timeout 5 /tmp/a=b/../../usr/bin/tee claude " + agent + " < /tmp/e.md; " + run},
		// MED-2: npx запускает другую команду
		{"M2 npx -p -c", "npx -p @anthropic-ai/claude-code -c 'tee " + agent + "' < /tmp/e.md; " + run},
		{"M2 npx -y -p", "npx -y -p @anthropic-ai/claude-code tee " + agent + " < /tmp/e.md; " + run},
		// MED-3: слово-путь вне текста
		{"M3 tar -C \\.claude", "tar -xf /tmp/e.tar -C \\.claude; " + run},
		{"M3 cd \\.claude", "cd \\.claude && tar -xf /tmp/e.tar && " + run},
		{"M3 tar -C ${X}.claude", "tar -xf /tmp/e.tar -C ${X}.claude; " + run},
		{"M3 tar \\-C.claude", "tar -xf /tmp/e.tar \\-C.claude; " + run},
		{"M3 tar -C\\.claude", "tar -xf /tmp/e.tar -C\\.claude; " + run},
		// без cd: $( в слове проверки и значения опций клиента
		{"test -v tar -C .claude", "test -v 'a[$(tar -xf /tmp/e.tar -C .claude)]'; " + run},
		{"[[ -v ]] tar -C ~/.claude", "[[ -v 'a[$(tar -xf /tmp/e.tar -C ~/.claude)]' ]]; " + run},
		{"claude --mcp-config agents", "claude --mcp-config '{\"mcpServers\":{\"x\":{\"command\":\"cp\",\"args\":[\"/tmp/e.md\",\"/home/u/.claude/agents/x.md\"]}}}' -p hi"},
		{"claude --settings= agents", "claude --settings='{\"hooks\":{\"command\":\"cp /tmp/e.md /home/u/.claude/agents/x.md\"}}' -p hi"},
		{"file -C", "cd ~/.claude && file -C -m settings; " + run},
	}
	pass := []struct{ name, command string }{
		{"cd cat settings", "cd ~/.claude && cat settings.json; " + run},
		{"cd claude mcp list", "cd ~/.claude && claude mcp list 2>/dev/null | head"},
		{"test -f agent", "test -f " + agent + " && " + run},
		{"[[ -f agent ]]", "[[ -f " + agent + " ]] && " + run},
		{"npx claude-code", "npx @anthropic-ai/claude-code -p \"review " + agent + "\""},
		{"npx -y claude-code", "npx -y @anthropic-ai/claude-code -p \"review " + agent + "\""},
		{"env assignment claude", "env FOO=1 claude -p \"review " + agent + "\""},
		{"sudo assignment claude", "sudo FOO=1 claude -p \"review " + agent + "\""},
		{"claude --settings prompt", "claude --settings " + settingsFile + " -p \"review " + agent + "\""},
		{"claude --mcp-config list prompt", "claude --mcp-config /tmp/a.json /tmp/b.json -p \"review " + agent + "\""},
		{"go field", "cd ~/git/aguard && perl -0pi -e 's|x|cfg.Restart.Claude = claude|' x.go; cl --cl-check"},
	}
	check := func(command string) *core.ValidationResult {
		t.Helper()
		result, err := tool.ValidateTool(gitGuardTestContext(t), &core.ToolInput{ToolName: "Bash", Command: command})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	for _, c := range deny {
		t.Run("deny "+c.name, func(t *testing.T) {
			if check(c.command).IsValid {
				t.Fatalf("пропуск, ждали отказ: %q", c.command)
			}
		})
	}
	for _, c := range pass {
		t.Run("pass "+c.name, func(t *testing.T) {
			if result := check(c.command); !result.IsValid {
				t.Fatalf("отказ, ждали пропуск: %+v", result.Violations)
			}
		})
	}
}
