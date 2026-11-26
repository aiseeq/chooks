package tools

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/aiseeq/chooks/internal/core"
)

// modelGuardTmuxVerdict — вердикт model_guard на команду Bash в cwd
func modelGuardTmuxVerdict(t *testing.T, tool *ModelGuardTool, command, cwd string) *core.ValidationResult {
	t.Helper()
	result, err := tool.ValidateTool(gitGuardTestContext(t), &core.ToolInput{ToolName: "Bash", Command: command, CWD: cwd})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// TestModelGuardTmuxShellText — текст, который tmux отдаст оболочке,
// судится как команда Bash (ревью mgwv круга 3, пункты 1–2)
func TestModelGuardTmuxShellText(t *testing.T) {
	tool := modelGuardForTest(t, "opus claude-opus-5-5\n")
	project := filepath.Join(t.TempDir(), "a b")
	if err := os.MkdirAll(filepath.Join(project, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".claude", "settings.json"), []byte(`{"model":"haiku"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, command string
		allowed       bool
	}{
		{"обычная сессия", `tmux new -d -s s 'ls'`, true},
		{"обычный send-keys", `tmux send-keys -t s 'git status' Enter`, true},
		{"сессия с именем claude", `tmux send-keys -t claude 'ls -la' Enter`, true},
		{"одобренный запуск в сессии", `tmux new -d -s s 'claude -p --model opus hi'`, true},
		{"одобренный набор", `tmux send-keys -t s 'claude -p --model opus hi' Enter`, true},
		{"список без текста", `tmux ls`, true},

		{"T01 send-keys с HOME", `tmux send-keys -t s 'HOME=/tmp/x claude -p --model opus hi' Enter`, false},
		{"T03 send-keys --bare", `tmux send-keys -t s 'claude --bare -p --model opus hi' Enter`, false},
		{"U04 send-keys чужая модель", `tmux send-keys -t s 'claude -p --model haiku hi' Enter`, false},
		{"T04 export и claude разными send-keys", `tmux send-keys -t s 'export HOME=/tmp/x' Enter; tmux send-keys -t s 'claude -p --model opus hi' Enter`, false},
		{"T06 new-window -e argv", `tmux new-window -d -e HOME=/tmp/x claude -p --model opus hi`, false},
		{"new-window argv --bare", `tmux new-window claude --bare -p hi`, false},
		{"T14 глобальная -c", `tmux -c 'HOME=/tmp/x claude -p --model opus hi'`, false},
		{"T16 связка -dc", `tmux new -dc /tmp 'claude --bare -p hi'`, false},
		{"T31 значение -f", `tmux new -d -f no-output 'claude --bare -p hi'`, false},
		{"-- перед командой", `tmux new -d -- 'claude --bare -p hi'`, false},
		{"имя сессии не исполняется", `tmux new -d -s 'claude --bare' 'ls'`, true},
		{"T19 default-command и send-keys", `tmux set -g default-command 'HOME=/tmp/x bash'; tmux new -d -s s; tmux send-keys -t s claude Enter`, false},
		{"O14 оболочка с env и send-keys в функции", `f() { tmux send-keys -t s claude Enter; }; tmux new -d -s s 'env HOME=/tmp/x bash'; f`, false},
		{"-e и send-keys", `tmux new -d -s s -e HOME=/tmp/x; tmux send-keys -t s claude Enter`, false},
		{"setenv -g HOME", `tmux setenv -g HOME /tmp/x; tmux new -d 'claude -p --model opus hi'`, false},
		{"setenv без значения", `tmux setenv -g CLAUDE_CONFIG_DIR; tmux new -d 'claude -p --model opus hi'`, false},
		// -u и -r снимают HOME, как env -u HOME у прямого запуска
		{"setenv -u HOME", `tmux setenv -gu HOME; tmux new -d 'claude -p --model opus hi'`, false},
		{"setenv -r HOME", `tmux setenv -r HOME; tmux new -d 'claude -p --model opus hi'`, false},
		{"setenv -u прочей", `tmux setenv -gu FOO; tmux new -d 'claude -p --model opus hi'`, true},
		{"setenv без имени", `tmux setenv; tmux new -d 'claude -p --model opus hi'`, true},
		{"T21 команды через \\;", `tmux new -d true \; send-keys 'claude --bare -p hi' Enter`, false},
		{"команда через слово на ;", `tmux display hi\; run 'claude --bare -p hi'`, false},
		{"отдельное ; после run", `tmux run 'claude --bare -p hi' \; ls`, false},
		{"литерал \\; не делит", `tmux display 'a\;' run 'claude --bare -p hi'`, true},
		{"T22 run-shell", `tmux run 'claude --bare -p hi'`, false},
		{"run -C — команда tmux", `tmux run -C 'new-window "claude --bare -p hi"'`, false},
		{"run -C не оболочка", `tmux run -C 'claude --bare -p hi'`, true},
		{"T24 popup -e", `tmux display-popup -E -e HOME=/tmp/x 'claude -p --model opus hi'`, false},
		{"T32 if-shell ветвь", `tmux if-shell true 'new-window "claude --bare -p hi"'`, false},
		{"if-shell вторая ветвь", `tmux if true 'display x' 'run "claude --bare -p hi"'`, false},
		{"if-shell условие", `tmux if 'claude --bare -p hi' 'display x'`, false},
		{"pipe-pane", `tmux pipep -o 'claude --bare -p hi'`, false},
		{"set-hook", `tmux set-hook -g after-new-window 'run-shell "claude --bare -p hi"'`, false},
		{"set-hook -u без команды", `tmux set-hook -gu after-new-window`, true},
		{"хук опцией", `tmux set -g pane-died[3] 'run "claude --bare -p hi"'`, false},
		{"опция не исполняется", `tmux set -g status-left 'claude --bare -p hi'`, true},
		{"опция без значения", `tmux set -g default-command`, true},
		{"bind-key", `tmux bind C run 'claude --bare -p hi'`, false},
		{"bind-key без команды", `tmux bind C`, true},
		{"confirm-before", `tmux confirm 'run "claude --bare -p hi"'`, false},
		{"command-prompt -p не исполняется", `tmux command-prompt -p 'claude --bare -p hi' 'display %%'`, true},
		{"display-menu команда", `tmux menu -T t a b 'run "claude --bare -p hi"'`, false},
		{"display-menu имя не исполняется", `tmux menu 'claude --bare -p hi' k 'display x'`, true},
		{"формат #()", `tmux display -p '#(claude --bare -p hi)'`, false},
		{"вложенные скобки формата", `tmux display -p '#(echo $(claude --bare -p hi))'`, false},
		{"незакрытый формат", `tmux display -p '#(claude --bare -p hi'`, false},
		{"второй формат", `tmux display -p '#(date) #(claude --bare -p hi)'`, false},
		{"однозначное начало имени", `tmux spl 'claude --bare -p hi'`, false},
		{"неоднозначное начало имени", `tmux respawn 'claude --bare -p hi'`, true},
		{"команда вне набора", `tmux list-keys 'claude --bare -p hi'`, true},
		{"набор частями", `tmux send-keys -t s 'claude --ba'; tmux send-keys -t s 're -p hi' Enter`, false},
		{"Space — пробел", `tmux send-keys -t s claude Space --bare Enter`, false},
		{"Tab — пробел", `tmux send-keys -t s claude Tab --bare Enter`, false},
		{"C-c обрывает строку", `tmux send-keys -t s echo C-c 'claude --bare -p hi' Enter`, false},
		{"-l — всё текст", `tmux send-keys -t s -l claude Space --bare`, true},
		{"-X — не набор", `tmux send-keys -t s -X 'claude --bare -p hi'`, true},
		{"-H — байты", `tmux send -H 63 6c 61 75 64 65 20 2d 2d 62 61 72 65`, false},
		{"-H — заглавные", `tmux send -H 63 6C 61 75 64 65 20 2D 2D 62 61 72 65`, false},
		{"-H — не число", `tmux send -H zz 636c 6`, true},
		{"слово с подстановкой", `tmux new -d "claude --model $M -p hi"`, false},
		{"-e с подстановкой", `tmux new -d -e "HOME=$X" 'claude -p --model opus hi'`, false},
		{"npx в send-keys", `tmux send-keys -t s 'npx @anthropic-ai/claude-code --bare -p hi' Enter`, false},
		{"tmux под timeout", `timeout 5 tmux new -d 'claude --bare -p hi'`, false},
		{"tmux в bash -c", `bash -c "tmux new -d 'claude --bare -p hi'"`, false},
		{"неразобранный набор с claude", `tmux send-keys -t s 'claude "unbalanced --bare' Enter`, false},
		{"неразобранный набор с claude-code", `tmux send-keys -t s 'npx @anthropic-ai/claude-code "x' Enter`, false},
		{"неразобранный набор без claude", `tmux send-keys -t s 'echo "unbalanced' Enter`, true},
		{"неразобранный echo claude", `tmux send-keys -t s 'echo "claude' Enter`, true},
		{"неразобранная часть не прячет панель", `tmux new -d 'claude -p --model opus hi'; tmux send-keys -t s 'echo "x' Enter`, true},
		{"-c каталог с настройками проекта", `tmux new -d -c ` + singleQuotedWord(project) + ` 'claude -p hi'`, false},
		{"popup -d каталог", `tmux popup -E -d ` + singleQuotedWord(project) + ` 'claude -p hi'`, false},
		{"run -c каталог", `tmux run -c ` + singleQuotedWord(project) + ` 'claude -p hi'`, false},
		{"пустой -c", `tmux new -d -c '' 'claude -p --model opus hi'`, true},
		{"глобальные опции со значением", `tmux -f /dev/null -L a -S /tmp/s -T 256 run 'claude --bare -p hi'`, false},
		{"argv run-shell — слова, не скрипт", `tmux run claude -p --model opus 'hi; claude --bare'`, true},
		{"-e HOME без =", `tmux new -d -e HOME 'claude -p --model opus hi'`, false},
		{"-e прочей без =", `tmux new -d -e FOO 'claude -p --model opus hi'`, false},
		{"-e значением -e", `tmux new -d -s s -e -e HOME=/tmp/x 'claude -p --model opus hi'`, false},
		{"-e без = без claude", `tmux new -d -e FOO 'ls'`, true},
		{"обёрнутый env -u HOME", `tmux new -d -s s 'timeout 9 env -u HOME claude -p --model opus hi'`, false},
		{"обёрнутый env -u HOME прямо", `timeout 9 env -u HOME claude -p --model opus hi`, false},
		{"обёрнутый env без снятия", `timeout 9 env FOO=1 claude -p --model opus hi`, true},
		{"M07 -e LD_PRELOAD", `tmux new -d -e LD_PRELOAD=/tmp/x.so 'claude -p --model opus hi'`, false},
		{"setenv NODE_OPTIONS и набор", `tmux setenv -g NODE_OPTIONS '--require /tmp/x.js'; tmux send-keys -t s 'claude -p --model opus hi' Enter`, false},
		{"-e LD_PRELOAD без claude", `tmux new -d -e LD_PRELOAD=/tmp/x.so 'ls'`, true},
		{"-e одобренной модели", `tmux new -d -e ANTHROPIC_MODEL=opus 'claude -p hi'`, true},
		{"-e чужой модели", `tmux new -d -e ANTHROPIC_MODEL=haiku 'claude -p hi'`, false},
		{"-e с ; в имени не исполняется", `tmux new -d -e 'A;claude --bare;B=1' ls`, true},
		{"подстановка в одинарных кавычках", `tmux run 'claude -p --model "$M" hi'`, false},
		{"текст после #() не исполняется", `tmux display -p '#(echo $(date)) claude --bare'`, true},
		{"кавычки и подстановка в слове", `tmux new -d 'claude --bare'"$X"`, false},
		{"printf без -v", `tmux new -d 'printf "$F" x'; claude -p --model opus hi`, true},

		// команды строки tmux через ; — каждая своя команда tmux
		{"D01 if-shell", `tmux if true 'display x; run "claude --bare -p hi"'`, false},
		{"D02 set-hook", `tmux set-hook -g after-new-window 'display x ; run-shell "claude --bare -p hi"'`, false},
		{"D03 run -C", `tmux run -C 'display x; new-window "claude --bare -p hi"'`, false},
		{"D05 bind-key", `tmux bind C 'display x; run "claude --bare -p hi"'`, false},
		{"D06 confirm-before", `tmux confirm 'display x; run "claude --bare -p hi"'`, false},
		{"строки tmux через перевод строки", "tmux if true 'display x\nrun \"claude --bare -p hi\"'", false},
		{"строка tmux без запуска", `tmux if true 'display x; display y'`, true},

		// присваивания команды вокруг tmux
		{"E01 команда в переменной", `C='claude --bare -p hi'; tmux new -d "$C"`, false},
		{"E03 набор из переменной", `CMD="claude -p --model haiku hi"; tmux send-keys -t s "$CMD" Enter`, false},
		{"E05 модель из переменной", `M=haiku; tmux send-keys -t s "claude -p --model $M hi" Enter`, false},
		{"присваивание ниже вызова в функции", `f() { tmux new -d "$C"; }; C='claude --bare -p hi'; f`, false},
		{"присваивание только команде", `HOME=/tmp/x make; tmux send-keys -t s 'claude -p --model opus hi' Enter`, true},
		{"присваивание у частей неразобранного скрипта", `C='claude --bare -p hi'; tmux new -d "$C"; tmux send-keys -t s 'echo "x' Enter`, false},
		{"setenv -r переменной claude", `tmux setenv -r CLAUDE_CODE_SIMPLE; tmux new -d 'claude -p --model opus hi'`, false},
		{"слово env не обёртка", `echo env -u HOME; claude -p --model opus hi`, true},
		{"имя сессии из переменной", `S=sess; tmux new -d -s $S 'claude -p --model opus hi'`, true},
		{"вычисляемое имя вокруг набора", `export "$N=1"; tmux send-keys -t s 'claude -p --model opus hi' Enter`, false},
		{"вычисляемое имя в наборе", `tmux send-keys -t s 'export "$N=1"; claude -p --model opus hi' Enter`, false},
		{"O17 npx в функции", `f() { npx @anthropic-ai/claude-code -p --model opus hi; }; declare -x "$N=/tmp/x"; f`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := modelGuardTmuxVerdict(t, tool, tc.command, t.TempDir())
			if result.IsValid != tc.allowed {
				t.Fatalf("allowed=%v, want %v: %s; violations=%v", result.IsValid, tc.allowed, tc.command, result.Violations)
			}
		})
	}
}

// TestModelGuardTmuxReasons — отказ называет tmux; префикс хуков судится и в
// тексте tmux
func TestModelGuardTmuxReasons(t *testing.T) {
	tool := modelGuardForTest(t, "opus claude-opus-5-5\n")
	cases := []struct{ command, want string }{
		{`tmux send-keys -t s 'claude --bare -p hi' Enter`, "tmux отдаст оболочке текст, где опция --bare"},
		{`tmux send-keys -t s 'claude "x --bare' Enter`, "текст, который tmux отдаст оболочке, не разобран"},
		// слово с $ не разбирается целиком: tmux получит его как есть
		{`tmux send-keys -t s '$x "claude --bare' Enter`, "текст, который tmux отдаст оболочке, не разобран"},
		{`tmux new -d 'CLAUDE_CODE_SHELL_PREFIX=relative claude -p --model opus hi'`, shellPrefixFailure},
		{`tmux send-keys -t s 'claude -p --model haiku hi' Enter`, `модель "haiku" не одобрена`},
		{`tmux new -d -e HOME=/tmp/x 'claude -p --model opus hi'`, "переменная HOME у клиента claude берёт настройки"},
	}
	for _, tc := range cases {
		result := modelGuardTmuxVerdict(t, tool, tc.command, t.TempDir())
		if result.IsValid || len(result.Violations) == 0 || !strings.Contains(result.Violations[0].Message, tc.want) {
			t.Errorf("%s: want refusal with %q, got %v", tc.command, tc.want, result.Violations)
		}
	}
}

// TestModelGuardTmuxNesting — tmux в тексте tmux разбирается
// tmuxNestingLimit кругов, глубже текст с claude — отказ
func TestModelGuardTmuxNesting(t *testing.T) {
	nested := func(levels int, inner string) string {
		text := inner
		for range levels {
			text = "tmux run " + singleQuotedWord(text)
		}
		return text
	}
	for levels := 1; levels <= tmuxNestingLimit+2; levels++ {
		scripts := tmuxShellScripts(nested(levels, "claude -p hi"), testLogger(t))
		if len(scripts) != min(levels, tmuxNestingLimit+1) {
			t.Fatalf("levels %d: scripts %d", levels, len(scripts))
		}
		last := scripts[len(scripts)-1]
		deep := levels > tmuxNestingLimit
		if last.tooDeep != deep || !strings.Contains(last.text, "claude -p hi") || !deep && last.text != "claude -p hi" {
			t.Fatalf("levels %d: last %+v", levels, last)
		}
	}
	tool := modelGuardForTest(t, "opus claude-opus-5-5\n")
	if result := modelGuardTmuxVerdict(t, tool, nested(tmuxNestingLimit, "claude -p --model opus hi"), t.TempDir()); !result.IsValid {
		t.Fatalf("approved launch %d levels deep refused: %v", tmuxNestingLimit, result.Violations)
	}
	if result := modelGuardTmuxVerdict(t, tool, nested(tmuxNestingLimit+1, "claude -p --model opus hi"), t.TempDir()); result.IsValid {
		t.Fatal("launch deeper than the limit allowed")
	}
	if result := modelGuardTmuxVerdict(t, tool, nested(tmuxNestingLimit+1, "ls"), t.TempDir()); !result.IsValid {
		t.Fatalf("text without claude deeper than the limit refused: %v", result.Violations)
	}
	if scripts := tmuxShellScripts("tmux ls", testLogger(t)); len(scripts) != 0 {
		t.Fatalf("tmux ls: scripts %+v", scripts)
	}
}

// TestModelGuardTmuxMatchesDirect — текст сессии tmux судится так же, как
// тот же текст прямой командой
func TestModelGuardTmuxMatchesDirect(t *testing.T) {
	tool := modelGuardForTest(t, "opus claude-opus-5-5\n")
	for _, text := range []string{
		`bash -c 'env HOME=/tmp/x claude -p --model opus hi'`,
		`echo | bash -s; claude -p --model opus hi`,
		`"$PROGRAM" --model opus; claude -p --model opus hi`,
		`bash -c '"$(cat /tmp/command)"'; claude -p --model opus hi`,
		`env -C /tmp HOME=/tmp/x claude -p --model opus hi`,
		`env -C /tmp FOO=1 claude -p --model opus hi`,
		`unset HOME; claude -p --model opus hi`,
		`timeout 9 env -u HOME claude -p --model opus hi`,
		`claude -p --model opus hi`,
	} {
		direct := modelGuardTmuxVerdict(t, tool, text, t.TempDir()).IsValid
		session := modelGuardTmuxVerdict(t, tool, "tmux new -d "+singleQuotedWord(text), t.TempDir()).IsValid
		typed := modelGuardTmuxVerdict(t, tool, "tmux send-keys -t s "+singleQuotedWord(text)+" Enter", t.TempDir()).IsValid
		if session != direct || typed != direct {
			t.Errorf("%s: direct allowed=%v, session %v, send-keys %v", text, direct, session, typed)
		}
	}
}

// TestModelGuardTmuxHomeValue — HOME через tmux пропускается только словом
// -e HOME=<домашний каталог> у claude -p (как в 97817a1), прочее — отказ
func TestModelGuardTmuxHomeValue(t *testing.T) {
	tool := modelGuardForTest(t, "opus claude-opus-5-5\n")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	h := singleQuotedWord(home)
	for command, allowed := range map[string]bool{
		`tmux new -d -e HOME=` + h + ` 'claude -p --model opus hi'`:                          true,
		`tmux new -d -eHOME=` + h + ` 'claude --print --model opus hi'`:                      true,
		`tmux new -d -e HOME=` + h + ` 'echo hi'`:                                            true,
		`tmux new -d -e HOME=` + h + ` 'claude --model opus hi'`:                             false,
		`tmux new -d -e HOME=` + h + ` 'claude -p --model opus hi | claude'`:                 false,
		`tmux new -d -e HOME=` + h + ` claude -p --model opus hi`:                            false,
		`tmux new -d -de HOME=` + h + ` 'claude -p --model opus hi'`:                         false,
		`tmux new -d -e HOME=` + singleQuotedWord(home+"/") + ` 'claude -p --model opus hi'`: false,
		`tmux setenv -g HOME ` + h + `; tmux new -d 'claude -p --model opus hi'`:             false,
		`tmux new -d -e HOME=` + h + ` 'cl --model opus hi'`:                                 false,
		`tmux new -d -e HOME=` + h + ` 'claude -p --model opus hi; bash -c "echo \"x"'`:      false,
	} {
		if result := modelGuardTmuxVerdict(t, tool, command, t.TempDir()); result.IsValid != allowed {
			t.Errorf("%s: allowed=%v, want %v: %v", command, result.IsValid, allowed, result.Violations)
		}
	}
}

// TestTmuxFlags — опции tmux по правилам getopt
func TestTmuxFlags(t *testing.T) {
	cases := []struct {
		args, operands []string
		flags          map[byte][]string
	}{
		{[]string{"--", "-c", "x"}, []string{"-c", "x"}, map[byte][]string{}},
		{[]string{"-cx", "y"}, []string{"y"}, map[byte][]string{'c': {"x"}}},
		{[]string{"-dc", "y", "z"}, []string{"z"}, map[byte][]string{'d': {""}, 'c': {"y"}}},
		{[]string{"-cdx"}, []string{}, map[byte][]string{'c': {"dx"}}},
		{[]string{"-", "x"}, []string{"-", "x"}, map[byte][]string{}},
		{[]string{"-c"}, []string{}, map[byte][]string{'c': {""}}},
	}
	for _, tc := range cases {
		flags, operands := tmuxFlags(tc.args, "c")
		if !reflect.DeepEqual(flags, tc.flags) || !slices.Equal(operands, tc.operands) {
			t.Errorf("tmuxFlags(%q) = %q %q, want %q %q", tc.args, flags, operands, tc.flags, tc.operands)
		}
	}
}

// TestTmuxHexKey — send-keys -H: байт до ff, прочее как есть
func TestTmuxHexKey(t *testing.T) {
	for key, want := range map[string]string{"63": "c", "2D": "-", "a": "\n", "636c": "636c", "zz": "zz", "": ""} {
		if got := tmuxHexKey(key); got != want {
			t.Errorf("tmuxHexKey(%q) = %q, want %q", key, got, want)
		}
	}
}
