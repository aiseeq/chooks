package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiseeq/chooks/internal/core"
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
		unknown   = "содержимое изменяемого файла настроек не проверить"
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
	if _, unknown := writtenAgentDefinitionCandidates("cp /tmp/evil.md .claude/agents/x.md; (", testLogger(t)); !unknown {
		t.Fatal("неразобранная запись в agents признана известной")
	}
	if _, unknown := writtenAgentDefinitionCandidates("echo hi; (", testLogger(t)); unknown {
		t.Fatal("неразобранная команда без пути агентов признана записью")
	}
}
