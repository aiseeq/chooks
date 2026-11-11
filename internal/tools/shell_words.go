package tools

import (
	"path/filepath"

	"slices"
	"strings"
)

// word слово оболочки без кавычек; literal — в нём нет подстановок $ и `
type word struct {
	text    string
	literal bool
}

// cmdStdin откуда простая команда читает stdin, если это видно из текста:
// из вывода команды слева от | или из here-string <<<
type cmdStdin struct {
	piped         bool
	hasHereString bool
	hereString    string
}

// shellLexer грубый разбор оболочки: слова и простые команды
type shellLexer struct {
	cmds [][]word
	// stdin — stdin каждой команды из cmds, по тем же индексам
	stdin    []cmdStdin
	curStdin cmdStdin
	// hereString — цель перенаправления — текст here-string (<<<)
	hereString bool
	cur        []word
	buf        strings.Builder
	literal    bool
	inWord     bool
	quote      rune
	// redirTarget — следующее слово цель перенаправления (файл или номер
	// дескриптора), а не аргумент команды
	redirTarget bool
	// hereDoc — цель перенаправления — разделитель here-doc (<< или <<-)
	hereDoc  bool
	stripTab bool
	hereDocs []hereDoc
}

// hereDoc разделитель тела here-doc; stripTab — <<-, строки тела и
// разделитель могут начинаться табуляцией
type hereDoc struct {
	name     string
	stripTab bool
}

func (l *shellLexer) flushWord() {
	if l.inWord {
		switch {
		case l.hereDoc:
			l.hereDocs = append(l.hereDocs, hereDoc{name: l.buf.String(), stripTab: l.stripTab})
		case l.hereString:
			l.curStdin.hasHereString, l.curStdin.hereString = true, l.buf.String()
		case !l.redirTarget:
			l.cur = append(l.cur, word{text: l.buf.String(), literal: l.literal})
		}
		l.redirTarget, l.hereDoc, l.hereString = false, false, false
	}
	l.buf.Reset()
	l.literal, l.inWord = true, false
}

func (l *shellLexer) flushCmd() {
	l.flushWord()
	l.redirTarget, l.hereDoc, l.hereString = false, false, false
	if len(l.cur) > 0 {
		l.cmds = append(l.cmds, l.cur)
		l.stdin = append(l.stdin, l.curStdin)
		// stdin пустой команды (скобка после |) переходит к следующей
		l.curStdin = cmdStdin{}
	}
	l.cur = nil
}

// redirect разбирает перенаправление, начатое с runes[i] (> < или &>), и
// возвращает индекс его последнего символа. Номер дескриптора перед ним
// (2>/dev/null) и цель после (2>&1, >/dev/null) в аргументы не попадают:
// иначе kill PID 2>&1 видел бы цели «2>» и «1»
func (l *shellLexer) redirect(runes []rune, i int) int {
	if l.inWord && l.literal && isDigits(l.buf.String()) {
		l.buf.Reset()
		l.inWord = false
	}
	l.flushWord()
	l.redirTarget = true
	switch {
	case runes[i] == '&':
		i++ // &> и &>>
	case runes[i] == '<' && next(runes, i) == '<' && next(runes, i+1) == '<':
		l.hereString = true
		return i + 2
	case runes[i] == '<' && next(runes, i) == '<':
		i++
		l.hereDoc, l.stripTab = true, next(runes, i) == '-'
		if l.stripTab {
			i++
		}
		return i
	}
	for strings.ContainsRune(">&|", next(runes, i)) {
		i++ // >> >& >| <&
	}
	return i
}

// next символ после runes[i]; за концом строки — 0
func next(runes []rune, i int) rune {
	if i+1 < len(runes) {
		return runes[i+1]
	}
	return 0
}

// isRedirect — с runes[i] начинается перенаправление; >( и <( — подстановка
// процесса, её команда разбирается как обычная
func isRedirect(runes []rune, i int) bool {
	r := runes[i]
	if r == '&' {
		return i+1 < len(runes) && runes[i+1] == '>' && (i+2 >= len(runes) || runes[i+2] != '(')
	}
	return (r == '>' || r == '<') && (i+1 >= len(runes) || runes[i+1] != '(')
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// add добавляет символ слова; $ и ` делают слово вычисляемым
func (l *shellLexer) add(r rune) {
	if r == '$' || r == '`' {
		l.literal = false
	}
	l.buf.WriteRune(r)
	l.inWord = true
}

// quoted обрабатывает символ внутри кавычек
func (l *shellLexer) quoted(r rune) {
	switch {
	case r == l.quote:
		l.quote = 0
	case l.quote == '\'':
		l.buf.WriteRune(r)
	default:
		l.add(r)
	}
}

// isPipe — | в runes[i] соединяет команды конвейером: не || и не >|
func isPipe(runes []rune, i int) bool {
	return runes[i] == '|' && next(runes, i) != '|' && (i == 0 || runes[i-1] != '|' && runes[i-1] != '>')
}

// splitSimpleCommands режет команду на простые по ; & | ( ) и переводу строки
// вне кавычек, перенаправления в слова не попадают; \ снимается, как в
// оболочке, комментарий # отбрасывается. Разбор грубый, но для поиска kill
// и его аргументов достаточен
func splitSimpleCommands(command string) [][]word {
	return lexShell(command).cmds
}

// lexShell разбирает команду на простые команды и разделители here-doc
func lexShell(command string) *shellLexer {
	l := &shellLexer{literal: true}
	runes := []rune(command)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case l.quote != 0:
			l.quoted(r)
		case r == '\'' || r == '"':
			l.quote, l.inWord = r, true
		case r == '\\':
			// \x — буква x как есть (\kill — это kill), \ и перевод строки — продолжение строки
			if i+1 < len(runes) {
				i++
				if runes[i] != '\n' {
					l.buf.WriteRune(runes[i])
					l.inWord = true
				}
			}
		case r == '#' && !l.inWord:
			for i+1 < len(runes) && runes[i+1] != '\n' {
				i++
			}
		case isRedirect(runes, i):
			i = l.redirect(runes, i)
		case strings.ContainsRune(";&|\n()", r):
			l.flushCmd()
			if isPipe(runes, i) {
				l.curStdin.piped = true
			}
		case r == ' ' || r == '\t':
			l.flushWord()
		default:
			l.add(r)
		}
	}
	l.flushCmd()
	return l
}

// wrapperValueOpts обёртки, которые исполняют команду после своих опций, и
// их опции с отдельным значением: без них значение (sudo -u aisee) читалось
// бы как имя команды
var wrapperValueOpts = map[string][]string{
	"sudo": {"-u", "-g", "-C", "-D", "-p", "-R", "-T", "-U", "-r", "-t", "--user", "--group",
		"--close-from", "--chdir", "--prompt", "--chroot", "--command-timeout", "--other-user", "--role", "--type"},
	"doas":    {"-u", "-C"},
	"env":     {"-u", "--unset", "-C", "--chdir"},
	"exec":    {"-a"},
	"nice":    {"-n", "--adjustment"},
	"ionice":  {"-c", "-n", "--class", "--classdata"},
	"time":    {"-f", "--format", "-o", "--output"},
	"stdbuf":  {"-i", "-o", "-e", "--input", "--output", "--error"},
	"timeout": {"-s", "--signal", "-k", "--kill-after"},
	"runuser": {"-u", "-g", "-G", "--user", "--group", "--supp-group", "-w", "--whitelist-environment"},
	"systemd-run": {"-u", "--unit", "-p", "--property", "-E", "--setenv", "--description", "--slice",
		"--uid", "--gid", "--nice", "--working-directory", "-M", "--machine", "-H", "--host",
		"--on-active", "--on-boot", "--on-startup", "--on-unit-active", "--on-unit-inactive", "--on-calendar"},
	"flock":    {"-w", "--timeout", "-E", "--conflict-exit-code"},
	"xargs":    {"-I", "-L", "-n", "-P", "-s", "-d", "-E", "-a", "--arg-file", "--delimiter", "--max-args", "--max-procs", "--max-lines"},
	"parallel": {"-j", "--jobs", "-S", "--sshlogin", "-I", "--colsep", "-a", "--arg-file", "-L", "-n"},
	"nohup":    nil, "command": nil, "setsid": nil, "busybox": nil,
}

// fedWrappers обёртки, которые берут аргументы команды из ввода или списка:
// цели сигнала до запуска не видны
var fedWrappers = map[string]bool{"xargs": true, "parallel": true}

// shellKeywords ключевые слова оболочки, после которых идёт команда
var shellKeywords = map[string]bool{
	"if": true, "then": true, "else": true, "elif": true, "while": true, "until": true,
	"do": true, "!": true, "{": true, "builtin": true, "coproc": true,
}

// unwrapCommand снимает ключевые слова оболочки (if, do, !, {), присваивания
// перед командой и обёртки (sudo -u x, nice -n 10, timeout 5, xargs, env -S
// и другие из wrapperValueOpts) и возвращает имя утилиты и её аргументы;
// via — обёртка, подающая аргументы из ввода (xargs, parallel)
func unwrapCommand(cmd []word) (name string, args []word, via string) {
	for len(cmd) > 0 {
		head := filepath.Base(cmd[0].text)
		cmd = cmd[1:]
		valueOpts, wrapper := wrapperValueOpts[head]
		switch {
		case shellKeywords[head] || assignWord.MatchString(head):
		case head == "env":
			cmd = unwrapEnv(cmd)
		case head == "runuser" && hasOptionScript(cmd):
			return head, cmd, via
		case wrapper:
			cmd = skipOptions(cmd, valueOpts)
			if fedWrappers[head] {
				via = head
			}
			cmd = afterWrapperOptions(head, cmd)
		default:
			return head, cmd, via
		}
	}
	return "", nil, via
}

// afterWrapperOptions снимает позиционные аргументы обёртки перед командой:
// длительность timeout, файл блокировки flock (flock файл -c 'скрипт' — как
// sh -c)
func afterWrapperOptions(head string, cmd []word) []word {
	switch head {
	case "timeout":
		if len(cmd) > 0 {
			cmd = cmd[1:]
		}
	case "flock":
		if len(cmd) > 0 {
			cmd = cmd[1:]
		}
		if len(cmd) > 1 && (cmd[0].text == "-c" || cmd[0].text == "--command") {
			cmd = append([]word{{text: "sh", literal: true}}, cmd...)
		}
	}
	return cmd
}

// unwrapEnv снимает опции и присваивания env; -S 'строка' разбивается на
// слова, как это делает env
func unwrapEnv(cmd []word) []word {
	for len(cmd) > 0 {
		t := cmd[0].text
		switch {
		case t == "-S" || t == "--split-string":
			if len(cmd) < 2 {
				return nil
			}
			return append(splitWords(cmd[1]), cmd[2:]...)
		case strings.HasPrefix(t, "-S"):
			return append(splitWords(word{text: t[2:], literal: cmd[0].literal}), cmd[1:]...)
		case strings.HasPrefix(t, "--split-string="):
			return append(splitWords(word{text: strings.TrimPrefix(t, "--split-string="), literal: cmd[0].literal}), cmd[1:]...)
		case t == "--":
			return cmd[1:]
		case slices.Contains(wrapperValueOpts["env"], t):
			cmd = cmd[min(2, len(cmd)):]
		case strings.HasPrefix(t, "-") || assignWord.MatchString(t):
			cmd = cmd[1:]
		default:
			return cmd
		}
	}
	return cmd
}

// splitWords слова строки через пробелы с вычисляемостью исходного слова
func splitWords(w word) []word {
	fields := strings.Fields(w.text)
	out := make([]word, 0, len(fields))
	for _, f := range fields {
		out = append(out, word{text: f, literal: w.literal})
	}
	return out
}

// skipOptions снимает опции до первой не-опции; опции из valueOpts — со
// значением, -- заканчивает опции
func skipOptions(cmd []word, valueOpts []string) []word {
	for len(cmd) > 0 {
		t := cmd[0].text
		if t == "--" {
			return cmd[1:]
		}
		if !strings.HasPrefix(t, "-") || t == "-" {
			return cmd
		}
		cmd = cmd[1:]
		if slices.Contains(valueOpts, t) && len(cmd) > 0 {
			cmd = cmd[1:]
		}
	}
	return cmd
}

// scriptSource откуда команда берёт скрипт оболочки
type scriptSource int

const (
	noScript    scriptSource = iota
	scriptArg                // строка-аргумент: sh -c, eval, watch, trap, su -c
	scriptStdin              // оболочка без -c и файла читает скрипт из stdin
)

// shellValueOpts опции sh, bash и подобных с отдельным значением
var shellValueOpts = []string{"-o", "+o", "-O", "+O", "--rcfile", "--init-file"}

// innerScript скрипт, который команда исполнит как команды оболочки: аргумент
// после -c у sh, bash и подобных (флаги -lc, -ec тоже), аргументы eval и
// watch через пробел, первый аргумент trap, -c у su и runuser. scriptStdin —
// оболочка читает скрипт из stdin (echo … | bash, bash <<< …)
func innerScript(name string, args []word) (string, scriptSource) {
	switch name {
	case "eval":
		return joinedScript(args)
	case "watch":
		return joinedScript(skipOptions(args, []string{"-n", "--interval", "-q", "--equexit"}))
	case "trap":
		args = skipOptions(args, nil)
		if len(args) == 0 {
			return "", noScript
		}
		return args[0].text, scriptArg
	case "su", "runuser":
		return optionScript(args)
	}
	if name == "ssh" || !shellNames[name] {
		return "", noScript
	}
	return shellArgScript(args)
}

// joinedScript аргументы через пробел, как их склеивают eval и watch
func joinedScript(args []word) (string, scriptSource) {
	if len(args) == 0 {
		return "", noScript
	}
	texts := make([]string, 0, len(args))
	for _, a := range args {
		texts = append(texts, a.text)
	}
	return strings.Join(texts, " "), scriptArg
}

// optionScript значение -c или --command у su и runuser в любом месте
func optionScript(args []word) (string, scriptSource) {
	for i, a := range args {
		switch {
		case (a.text == "-c" || a.text == "--command") && i+1 < len(args):
			return args[i+1].text, scriptArg
		case strings.HasPrefix(a.text, "--command="):
			return strings.TrimPrefix(a.text, "--command="), scriptArg
		}
	}
	return "", noScript
}

// hasOptionScript — у su или runuser есть -c: команда — скрипт, а не слова
// после опций
func hasOptionScript(args []word) bool {
	_, source := optionScript(args)
	return source == scriptArg
}

// shellArgScript скрипт sh, bash и подобных: строка после -c, иначе stdin,
// если нет файла скрипта или есть -s
func shellArgScript(args []word) (string, scriptSource) {
	withC, withS := false, false
	for i := 0; i < len(args); i++ {
		a := args[i].text
		switch {
		case slices.Contains(shellValueOpts, a):
			i++
		case a == "--":
			if withC && i+1 < len(args) {
				return args[i+1].text, scriptArg
			}
			return shellStdin(withS, i+1 < len(args))
		case len(a) > 1 && (a[0] == '-' || a[0] == '+'):
			// -c, -lc, -ec, -s; длинные опции (--norc) и +x не в счёт
			short := a[0] == '-' && a[1] != '-'
			withC = withC || short && strings.ContainsRune(a[1:], 'c')
			withS = withS || short && strings.ContainsRune(a[1:], 's')
		case withC:
			return a, scriptArg
		default:
			return shellStdin(withS, true)
		}
	}
	return shellStdin(withS, false)
}

// shellStdin оболочка читает скрипт из stdin без файла скрипта или с -s
func shellStdin(withS, scriptFile bool) (string, scriptSource) {
	if withS || !scriptFile {
		return "", scriptStdin
	}
	return "", noScript
}
