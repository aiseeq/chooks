package tools

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/aiseeq/akit/shellcmd"
	"mvdan.cc/sh/v3/syntax"
)

// curlDataOptions опции curl с телом запроса. --data-urlencode несёт
// поля формы, не тело комментария, но запрос с ней - тоже запись
var curlDataOptions = map[string]bool{
	"-d": true, "--data": true, "--data-binary": true, "--data-raw": true,
	"--data-ascii": true, "--data-urlencode": true, "--json": true,
}

// curlShortValued короткие опции curl со значением: в связке (-sSd @f)
// после такой буквы остаток слова или следующее слово - её значение
const curlShortValued = "AbcCdDeEFHKmoPQrtTuUwxXyYz"

// curlWriteMethods методы, которыми комментарий создаётся или правится
var curlWriteMethods = map[string]bool{"POST": true, "PUT": true, "PATCH": true}

// curlRequest аргументы вызова curl, нужные проверке комментария
type curlRequest struct {
	data  []curlData // значения опций тела по порядку
	write bool       // есть тело, --json, -F, -T или метод POST/PUT/PATCH
	url   bool       // в аргументах вне тела есть адрес комментария Jira
}

// curlData значение опции тела: литерал - значение слова, иначе его
// исходный текст ("$(cat f)", @$S/c.json)
type curlData struct {
	option string
	value  string
	// upload - значение -T или --upload-file отдельным словом word (номер в
	// Args): путь стоит в начале слова, и ~ в нём bash раскрывает
	upload bool
	word   int
}

// parseCurl разбирает аргументы curl: опции тела и метода, связки коротких
// опций, длинные опции со значением через = (--data-binary=@f, curl 8.18
// понимает), адрес комментария среди остальных слов. expand подставляет в
// слово переменные команды: адрес бывает в URL=…/comment; curl "$URL"
func parseCurl(args []string, expand func(string) string) curlRequest {
	var req curlRequest
	for i := 0; i < len(args); i++ {
		arg, inline, hasInline := args[i], "", false
		if name, value, ok := strings.Cut(arg, "="); ok && strings.HasPrefix(arg, "--") {
			arg, inline, hasInline = name, value, true
		}
		// next значение опции: после = в том же слове либо следующее слово
		// (whole - значение занимает всё слово)
		next := func() (value string, whole bool) {
			switch {
			case hasInline:
				return inline, false
			case i+1 < len(args):
				i++
				return args[i], true
			}
			return "", false
		}
		switch {
		case curlDataOptions[arg]:
			value, _ := next()
			req.data = append(req.data, curlData{option: arg, value: value})
			req.write = true
		case arg == "--request":
			method, _ := next()
			req.write = req.write || curlWriteMethods[strings.ToUpper(method)]
		case arg == "--upload-file":
			// файл уходит телом как есть, как -d @f; - значит вход
			value, whole := next()
			req.data = append(req.data, curlData{option: arg, value: "@" + value, upload: whole, word: i})
			req.write = true
		case arg == "--form":
			next()
			req.write = true
		case strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") && len(arg) > 1:
			req.shortCluster(arg, next, func() int { return i })
		default:
			word := args[i]
			req.url = req.url || jiraCommentEndpoint.MatchString(word) || jiraCommentEndpoint.MatchString(expand(word))
		}
	}
	return req
}

// shortCluster разбирает связку коротких опций: -sSX POST, -d@f, -XPOST.
// next - значение следующим словом, at - номер слова после next
func (req *curlRequest) shortCluster(arg string, next func() (string, bool), at func() int) {
	for j := 1; j < len(arg); j++ {
		letter := arg[j]
		if !strings.ContainsRune(curlShortValued, rune(letter)) {
			continue
		}
		value, whole := arg[j+1:], false
		if value == "" {
			value, whole = next()
		}
		switch letter {
		case 'd':
			req.data = append(req.data, curlData{option: "-d", value: value})
			req.write = true
		case 'X':
			req.write = req.write || curlWriteMethods[strings.ToUpper(value)]
		case 'T':
			req.data = append(req.data, curlData{option: "-T", value: "@" + value, upload: whole, word: at()})
			req.write = true
		case 'F':
			req.write = true
		}
		return
	}
}

// commentSend вызов curl, пишущий комментарий в Jira, и каталог, в котором
// он выполнится
type commentSend struct {
	index int // место вызова в списке shellcmd.Calls
	call  *shellcmd.Call
	req   curlRequest
	cwd   string // "" - каталог неизвестен (cd $X)
}

// writtenFile файл, который команда пишет до отправки: перенаправление
// вывода, tee, sed -i. Его прежнее содержимое телом не будет, а новый текст
// стоит в самой команде (heredoc, генератор, новая часть замены sed)
type writtenFile struct {
	index int
	raw   string // путь как в команде, без кавычек, ~ раскрыта как в bash
	abs   string // абсолютный путь; "" - не раскрыть до запуска
}

// commandPlan вызовы команды, нужные проверке: отправки комментария, записи
// файлов и каталог перед каждым вызовом
type commandPlan struct {
	command string
	home    string // домашний каталог для ~ и cd без аргументов; "" - неизвестен
	vars    []shellVar
	varsErr error // присваивания не разобраны: пути через переменные не раскрыть
	calls   []shellcmd.Call
	words   []callWords // words[i] - слова вызова i из разбора: по ним видны кавычки
	cwds    []string    // cwds[i] - каталог перед вызовом i
	sends   []commentSend
	written []writtenFile
}

// planCommand проходит вызовы по порядку: cd меняет каталог для
// относительных путей (только в своей области: подоболочка, звено
// конвейера, вложенный скрипт — shellScopes), записи файлов и отправки
// комментария запоминаются
func planCommand(command, cwd, home string, calls []shellcmd.Call) commandPlan {
	plan := commandPlan{command: command, home: home, calls: calls, cwds: make([]string, len(calls)), words: make([]callWords, len(calls))}
	var index map[int]callWords
	if file, err := parseBash(command); err != nil {
		plan.varsErr = fmt.Errorf("разбор присваиваний: %w", err)
	} else {
		plan.vars = commandVars(file, command, home)
		index = wordIndex(file, 0)
	}
	scopes, scopesErr := commandScopes(command)
	var changes []dirChange
	for i := range calls {
		c := &calls[i]
		plan.words[i] = findCallWords(index, command, c)
		here := dirAt(cwd, c.Offset, changes, scopesErr != nil)
		plan.cwds[i] = here
		plan.addWrites(i, here)
		switch c.Name {
		case "cd", "pushd", "popd":
			changes = append(changes, dirChange{offset: c.Offset, dir: plan.newDir(i, here), within: scopes.around(c)})
		case "curl":
			expand := func(arg string) string { return plan.path(arg, i) }
			if req := parseCurl(c.Args, expand); req.url && req.write {
				plan.sends = append(plan.sends, commentSend{index: i, call: c, req: req, cwd: here})
			}
		}
	}
	return plan
}

func parseBash(text string) (*syntax.File, error) {
	return syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(text), "")
}

// callWords слова простого вызова из разбора mvdan/sh. shellcmd в Args и
// Redirs отдаёт значение литерального слова уже без кавычек, а ~ bash
// раскрывает только без кавычек - это видно лишь по самому слову
type callWords struct {
	args   []*syntax.Word // слова после имени программы, по порядку Args
	redirs []*syntax.Word // цели перенаправлений, по порядку Redirs
}

// wordIndex слова простых вызовов разбора по смещению каждого слова в
// команде: имя программы за обёрткой (sudo curl) - тоже слово вызова. base -
// смещение разобранного текста в команде
func wordIndex(file *syntax.File, base int) map[int]callWords {
	index := map[int]callWords{}
	syntax.Walk(file, func(node syntax.Node) bool {
		stmt, ok := node.(*syntax.Stmt)
		if !ok {
			return true
		}
		call, ok := stmt.Cmd.(*syntax.CallExpr)
		if !ok {
			return true
		}
		redirs := make([]*syntax.Word, len(stmt.Redirs))
		for j, r := range stmt.Redirs {
			redirs[j] = r.Word
		}
		for k, w := range call.Args {
			index[base+int(w.Pos().Offset())] = callWords{args: call.Args[k+1:], redirs: redirs}
		}
		return true
	})
	return index
}

// findCallWords слова вызова c: по смещению имени в разборе команды, а
// вызова во вложенном скрипте (bash -c '…', heredoc на вход оболочке) - в
// разборе его текста Raw на том же месте команды. Пусто - слов не найти:
// имя из переменной ($C -X POST), текст вызова не из команды
func findCallWords(index map[int]callWords, command string, c *shellcmd.Call) callWords {
	if c.Offset < 0 {
		return callWords{}
	}
	if words, ok := index[c.Offset]; ok {
		return words.match(c)
	}
	for from := 0; c.Raw != ""; {
		at := strings.Index(command[from:], c.Raw)
		if at < 0 || from+at > c.Offset {
			break
		}
		start := from + at
		from = start + 1
		if c.Offset >= start+len(c.Raw) {
			continue
		}
		file, err := parseBash(c.Raw)
		if err != nil {
			break
		}
		return wordIndex(file, start)[c.Offset].match(c)
	}
	return callWords{}
}

// match слова, сверенные с вызовом: число целей перенаправлений другое
// (Raw вложенного вызова без хвостовых перенаправлений) - их нет
func (w callWords) match(c *shellcmd.Call) callWords {
	if len(w.redirs) != len(c.Redirs) {
		w.redirs = nil
	}
	return w
}

// arg слово аргумента i; nil - не найдено
func (w callWords) arg(i int) *syntax.Word {
	if i >= 0 && i < len(w.args) {
		return w.args[i]
	}
	return nil
}

// redir слово цели перенаправления i; nil - не найдено
func (w callWords) redir(i int) *syntax.Word {
	if i >= 0 && i < len(w.redirs) {
		return w.redirs[i]
	}
	return nil
}

// addWrites запоминает файлы, которые пишет вызов index: перенаправление
// вывода, tee, правка на месте sed -i или perl -i
func (p *commandPlan) addWrites(index int, cwd string) {
	c := &p.calls[index]
	for j, r := range c.Redirs {
		if r.Op == ">" || r.Op == ">>" || r.Op == ">|" || r.Op == "&>" || r.Op == "&>>" {
			p.addWritten(index, r.Target, p.words[index].redir(j), cwd)
		}
	}
	inPlace := slices.ContainsFunc(c.Args, func(a string) bool {
		return strings.HasPrefix(a, "-i") || strings.HasPrefix(a, "--in-place") ||
			(c.Name == "perl" && perlInPlace.MatchString(a))
	})
	if c.Name == "tee" || ((c.Name == "sed" || c.Name == "perl") && inPlace) {
		for _, k := range operands(c.Args) {
			p.addWritten(index, c.Args[k], p.words[index].arg(k), cwd)
		}
	}
}

func (p *commandPlan) addWritten(index int, raw string, word *syntax.Word, cwd string) {
	raw, known := p.wordPath(raw, index, word)
	abs := ""
	if known {
		abs = resolvePath(raw, cwd)
	}
	p.written = append(p.written, writtenFile{index: index, raw: raw, abs: abs})
}

// perlInPlace связка ключей perl с правкой на месте: -pi, -0pi, -i.bak
var perlInPlace = regexp.MustCompile(`^-[0-9alnp]*i`)

// path путь из слова вызова index без кавычек, с переменными, присвоенными
// в команде раньше вызова. Неизвестная переменная остаётся в пути как $NAME.
// ~ остаётся как есть: раскрыть её можно только по слову (wordPath)
func (p *commandPlan) path(raw string, index int) string {
	return expandVars(p.vars, normalizeShellPath(raw), p.callOffset(index))
}

// callOffset место вызова в тексте команды: у имени из переменной ($C -X
// POST) shellcmd смещения не даёт, тогда - начало его текста; -1 - не найти
func (p *commandPlan) callOffset(index int) int {
	c := p.calls[index]
	if c.Offset >= 0 {
		return c.Offset
	}
	return strings.Index(p.command, c.Raw)
}

// writtenBefore файл пишет вызов команды раньше вызова index
func (p *commandPlan) writtenBefore(index int, raw, abs string) bool {
	return slices.ContainsFunc(p.written, func(w writtenFile) bool {
		return w.index < index && (w.raw == raw || (abs != "" && w.abs == abs))
	})
}

// wordPath путь из целого слова, как path, и ~ в его начале - как раскроет
// bash: ~ и ~/… без кавычек - домашний каталог, в кавычках или за \ -
// буквальный каталог «~». word - то же слово из разбора; nil - не найдено.
// known=false - путь начинается с ~, а во что она раскроется, до запуска не
// узнать: ~user, ~+, домашний каталог неизвестен, слова нет в разборе
func (p *commandPlan) wordPath(raw string, index int, word *syntax.Word) (path string, known bool) {
	path = p.path(raw, index)
	if !strings.HasPrefix(normalizeShellPath(raw), "~") {
		return path, true
	}
	if word == nil {
		return path, false
	}
	switch leadingTilde(word) {
	case tildeNone:
		return path, true
	case tildeHome:
		if p.home != "" {
			return p.home + path[1:], true
		}
	}
	return path, false
}

// tilde что bash сделает с ~ на месте, где он её раскрывает
type tilde int

const (
	tildeNone  tilde = iota // ~ в кавычках или за \: остаётся литерой
	tildeHome               // ~ или ~/…: домашний каталог
	tildeOther              // ~user, ~+, ~-: каталог, который до запуска не узнать
)

// leadingTilde ~ в начале слова word
func leadingTilde(word *syntax.Word) tilde {
	if len(word.Parts) == 0 {
		return tildeNone
	}
	lit, ok := word.Parts[0].(*syntax.Lit)
	if !ok || !strings.HasPrefix(lit.Value, "~") {
		return tildeNone
	}
	return tildePrefix(lit.Value, len(word.Parts) == 1, "/")
}

// tildePrefix ~ в начале s - текста без кавычек в исходном виде (с \).
// Префикс тянется до первого символа из stops без кавычек; wordEnd - s
// кончает слово. Префикс, уходящий в кавычки или подстановку (~"/x"), или с
// \ внутри bash не раскрывает
func tildePrefix(s string, wordEnd bool, stops string) tilde {
	n := strings.IndexAny(s, stops)
	if n < 0 {
		if !wordEnd {
			return tildeNone
		}
		n = len(s)
	}
	switch name := s[1:n]; {
	case strings.Contains(name, `\`):
		return tildeNone
	case name == "":
		return tildeHome
	}
	return tildeOther
}

// operands номера слов вызова, не похожих на опции
func operands(args []string) []int {
	var out []int
	for i, a := range args {
		if !strings.HasPrefix(a, "-") {
			out = append(out, i)
		}
	}
	return out
}

// changeDir каталог после cd в вызове index; "" - не раскрыть до запуска
func (p *commandPlan) changeDir(index int, cwd string) string {
	args := p.calls[index].Args
	dirs := operands(args)
	switch {
	case slices.Contains(args, "-"):
		// cd - — прежний каталог
		return ""
	case len(dirs) == 0:
		return p.home
	}
	dir, known := p.wordPath(args[dirs[0]], index, p.words[index].arg(dirs[0]))
	if !known {
		return ""
	}
	return resolvePath(dir, cwd)
}

// newDir — каталог после cd, pushd или popd вызова index. pushd -n и
// popd -n каталог не меняют; pushd +N, pushd без каталога и popd берут его
// из стека: неизвестен
func (p *commandPlan) newDir(index int, cwd string) string {
	c := &p.calls[index]
	dirs := operands(c.Args)
	switch {
	case c.Name == "cd":
		return p.changeDir(index, cwd)
	case slices.Contains(c.Args, "-n"):
		return cwd
	case c.Name == "popd" || len(dirs) == 0 || strings.HasPrefix(c.Args[dirs[0]], "+"):
		return ""
	}
	return p.changeDir(index, cwd)
}

// shellBraceVar ${NAME} - та же переменная, что $NAME
var shellBraceVar = regexp.MustCompile(`\$\{(\w+)\}`)

// normalizeShellPath путь из слова команды без кавычек: у нелитерального
// слова shellcmd отдаёт исходный текст ("$S/c.json", @"$S"/c.json)
func normalizeShellPath(raw string) string {
	raw = strings.NewReplacer(`"`, "", `'`, "").Replace(strings.TrimSpace(raw))
	return shellBraceVar.ReplaceAllString(raw, `$$$1`)
}

// unexpanded путь с подстановкой, которую видно только при запуске
func unexpanded(path string) bool {
	return strings.ContainsAny(path, "$`")
}

// absPath абсолютный путь от каталога cwd, ~ в начале - домашний каталог
// home; "" - не раскрыть до запуска. Кавычек путь уже не помнит: ~ из
// "~/x" bash не раскрывает. Пути из слов вызова - wordPath и resolvePath
func absPath(path, cwd, home string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home == "" {
			return ""
		}
		path = home + path[1:]
	}
	return resolvePath(path, cwd)
}

// resolvePath абсолютный путь от каталога cwd; "" - не раскрыть до запуска.
// ~ здесь - литера: раскрытая bash тильда уже заменена (wordPath)
func resolvePath(path, cwd string) string {
	if path == "" || unexpanded(path) {
		return ""
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	if cwd == "" || path == "-" {
		return ""
	}
	return filepath.Join(cwd, path)
}

// payloadSource файл, из которого curl возьмёт тело комментария
type payloadSource struct {
	raw string // путь как в команде
	cwd string // каталог, от которого он считается
	at  int    // место вызова, читающего файл
	// optional - аргумент генератора в конвейере (jq . f, python3 x.py f):
	// он бывает и не файлом (фильтр jq), поэтому нет такого файла - пропуск
	optional bool
	// tilde - путь начинается с ~, которую до запуска не раскрыть (wordPath)
	tilde bool
}

// payloadFiles файлы тела отправки: -d @f, --json @f, -d "$(cat f)",
// -d @- со входом из файла (< f) или из cat f | curl. Тело из генератора
// (python3 - <<EOF | curl, heredoc на вход curl, литерал в -d) стоит в самой
// команде, файла у него нет. ~ после @ не в начале слова: bash её не
// раскрывает, и curl читает каталог «~» от cwd
func (p *commandPlan) payloadFiles(send *commentSend) []payloadSource {
	var files []payloadSource
	for _, d := range send.req.data {
		value := strings.TrimSpace(d.value)
		if d.option == "--data-urlencode" {
			continue
		}
		path, isFile := strings.CutPrefix(normalizeShellPath(value), "@")
		switch {
		case isFile && path == "-":
			files = append(files, p.stdinFiles(send)...)
		case isFile && d.upload:
			files = append(files, p.wordSource(p.calls[send.index].Args[d.word], send.index, p.words[send.index].arg(d.word), send.cwd, send.index))
		case isFile:
			files = append(files, payloadSource{raw: p.path(path, send.index), cwd: send.cwd, at: send.index})
		case strings.Contains(value, "$(") || strings.Contains(value, "`"):
			files = append(files, p.substitutedFiles(send, value)...)
		}
	}
	return files
}

// wordSource файл тела из целого слова raw: переменные - на месте вызова
// vars, ~ - по слову word
func (p *commandPlan) wordSource(raw string, vars int, word *syntax.Word, cwd string, at int) payloadSource {
	path, known := p.wordPath(raw, vars, word)
	return payloadSource{raw: path, cwd: cwd, at: at, tilde: !known}
}

// substitutedFiles файлы, которые cat читает в подстановке -d "$(cat f)"
func (p *commandPlan) substitutedFiles(send *commentSend, value string) []payloadSource {
	var files []payloadSource
	for i, c := range p.calls {
		if c.Name != "cat" || i <= send.index || !strings.Contains(value, c.Raw) {
			continue
		}
		for _, k := range operands(c.Args) {
			files = append(files, p.wordSource(c.Args[k], send.index, p.words[i].arg(k), send.cwd, send.index))
		}
	}
	return files
}

// stdinFiles файлы, из которых собран вход curl -d @-: перенаправление < f
// у curl и у вызовов конвейера перед ним, операнды cat и аргументы-файлы
// генераторов (jq --rawfile b f, jq . f, python3 -c '…' f). Heredoc и
// литерал генератора стоят в самой команде
func (p *commandPlan) stdinFiles(send *commentSend) []payloadSource {
	var files []payloadSource
	for stage := send.index; stage >= 0; stage = p.pipedFrom(stage) {
		if stage != send.index {
			files = append(files, p.argFiles(stage)...)
		}
		if j := stdinRedir(p.calls[stage].Redirs); j >= 0 {
			if r := p.calls[stage].Redirs[j]; r.Op == "<" {
				files = append(files, p.wordSource(r.Target, stage, p.words[stage].redir(j), p.cwds[stage], stage))
			}
			// вход из файла или heredoc: конвейер выше на этот вызов не идёт
			break
		}
	}
	return files
}

// stdinRedir номер перенаправления входа вызова: < f или heredoc; -1 - его нет
func stdinRedir(redirs []shellcmd.Redir) int {
	for i := range redirs {
		switch redirs[i].Op {
		case "<", "<<", "<<-", "<<<":
			return i
		}
	}
	return -1
}

// argFiles файлы, которые вызов конвейера читает аргументами: у cat - все
// операнды (нет файла - отказ), у генератора - те, что есть на диске
func (p *commandPlan) argFiles(index int) []payloadSource {
	c := p.calls[index]
	var files []payloadSource
	for _, k := range operands(c.Args) {
		src := p.wordSource(c.Args[k], index, p.words[index].arg(k), p.cwds[index], index)
		src.optional = c.Name != "cat"
		files = append(files, src)
	}
	return files
}

// pipedFrom вызов, чей вывод идёт на вход отправки через |: между его
// текстом и именем curl только перенаправления и |; -1 - такого нет
func (p *commandPlan) pipedFrom(index int) int {
	to := p.calls[index]
	if to.Offset < 0 {
		return -1
	}
	for i := index - 1; i >= 0; i-- {
		c := p.calls[i]
		end := c.Offset + len(c.Raw)
		if c.Offset < 0 || end > to.Offset || !strings.HasPrefix(p.command[c.Offset:], c.Raw) {
			continue
		}
		gap := strings.TrimSpace(p.command[end:to.Offset])
		if !strings.HasSuffix(gap, "|") || strings.HasSuffix(gap, "||") ||
			strings.ContainsAny(gap, ";\n'\"()`") || strings.Contains(gap, "&&") {
			return -1
		}
		return i
	}
	return -1
}

// readSource читает файл тела. Файл, записанный раньше в этой же команде,
// не читается: его текст в команде (written), а на диске - прежний
func (p *commandPlan) readSource(src payloadSource) (body string, written bool, err error) {
	abs := ""
	if !src.tilde {
		abs = resolvePath(src.raw, src.cwd)
	}
	if p.writtenBefore(src.at, src.raw, abs) {
		return "", true, nil
	}
	switch {
	case src.tilde:
		return "", false, fmt.Errorf("путь с ~, которую до запуска не раскрыть (~user, ~+, домашний каталог неизвестен или слово не разобрано)")
	case unexpanded(src.raw):
		return "", false, fmt.Errorf("путь через переменную или подстановку, файл до запуска не найти")
	case abs == "":
		return "", false, fmt.Errorf("относительный путь, а каталог команды неизвестен")
	}
	body, err = readPayloadFile(abs)
	return body, false, err
}

// describeSend адрес отправки для сообщений: аргумент с адресом комментария
func describeSend(send *commentSend) string {
	for _, a := range send.call.Args {
		if m := jiraCommentEndpoint.FindString(a); m != "" {
			return m
		}
	}
	return "comment"
}

// shellVar значение переменной после присваивания в команде: S=/abs/dir,
// export D="$S/sub". Подстановка, которую видно только при запуске, остаётся
// в значении текстом ($JIRA_BASE_URL, $(pwd))
type shellVar struct {
	offset int
	name   string
	value  string
	// unknown - значение до запуска не узнать (S=~user/x): ссылка на
	// переменную остаётся в пути текстом, как у неприсвоенной
	unknown bool
}

// shellVarRef ссылка на переменную в пути: $NAME или ${NAME}
var shellVarRef = regexp.MustCompile(`\$(\w+)|\$\{(\w+)\}`)

// commandVars присваивания команды по порядку текста. Присваивание перед
// командой (S=x cmd) в её аргументы не попадает и не берётся
func commandVars(file *syntax.File, command, home string) []shellVar {
	var vars []shellVar
	add := func(assigns []*syntax.Assign) {
		for _, a := range assigns {
			if a.Name == nil || a.Value == nil {
				continue
			}
			offset := int(a.Pos().Offset())
			value, known := assignValue(a.Value, command, home)
			if known {
				value = expandVars(vars, value, offset)
			}
			vars = append(vars, shellVar{offset: offset, name: a.Name.Value, value: value, unknown: !known})
		}
	}
	syntax.Walk(file, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.CallExpr:
			if len(n.Args) == 0 {
				add(n.Assigns)
			}
		case *syntax.DeclClause:
			add(n.Args)
		}
		return true
	})
	slices.SortStableFunc(vars, func(a, b shellVar) int { return a.offset - b.offset })
	return vars
}

// assignValue значение присваивания без кавычек и \, как его даст bash:
// ~ без кавычек в начале значения и после : - домашний каталог home (S=~/x,
// PATH=a:~/bin, export S=~/x), в кавычках или за \ - литера (S="~/x").
// Подстановки остаются текстом ($S, $(pwd)). known=false - ~ раскроется во
// что-то, чего до запуска не узнать: ~user, ~+, home неизвестен
func assignValue(word *syntax.Word, command, home string) (value string, known bool) {
	var b strings.Builder
	for i, part := range word.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			text, ok := assignLit(p.Value, i == 0, i == len(word.Parts)-1, home)
			if !ok {
				return "", false
			}
			b.WriteString(text)
		case *syntax.SglQuoted:
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			for _, inner := range p.Parts {
				if lit, ok := inner.(*syntax.Lit); ok {
					b.WriteString(unescapeDouble(lit.Value))
				} else {
					b.WriteString(sourceText(inner, command))
				}
			}
		default:
			b.WriteString(sourceText(part, command))
		}
	}
	return b.String(), true
}

// assignLit часть значения присваивания без кавычек (исходный текст с \):
// снимает \ и раскрывает ~ в начале значения (first) и после : без кавычек.
// last - часть кончает значение
func assignLit(s string, first, last bool, home string) (string, bool) {
	var b strings.Builder
	afterColon := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\':
			if i+1 < len(s) && s[i+1] != '\n' {
				b.WriteByte(s[i+1])
			}
			i++
			afterColon = false
			continue
		case c == '~' && ((i == 0 && first) || afterColon):
			switch tildePrefix(s[i:], last, "/:") {
			case tildeHome:
				if home == "" {
					return "", false
				}
				b.WriteString(home)
				afterColon = false
				continue
			case tildeOther:
				return "", false
			}
		}
		b.WriteByte(c)
		afterColon = c == ':'
	}
	return b.String(), true
}

// unescapeDouble текст в двойных кавычках без \ перед $ ` " \ и переводом строки
func unescapeDouble(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) && strings.IndexByte("$`\"\\\n", s[i+1]) >= 0 {
			if s[i+1] != '\n' {
				b.WriteByte(s[i+1])
			}
			i++
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// sourceText исходный текст узла: подстановка в значении ($S, "${S}",
// $(pwd)); ${NAME} - как $NAME
func sourceText(node syntax.Node, command string) string {
	start, end := int(node.Pos().Offset()), int(node.End().Offset())
	if start < 0 || end > len(command) || start > end {
		return ""
	}
	return shellBraceVar.ReplaceAllString(command[start:end], `$$$1`)
}

// expandVars подставляет в путь значения переменных, присвоенных в команде
// до смещения at. Ссылка на переменную без присваивания или с неизвестным
// значением остаётся текстом
func expandVars(vars []shellVar, path string, at int) string {
	return shellVarRef.ReplaceAllStringFunc(path, func(ref string) string {
		m := shellVarRef.FindStringSubmatch(ref)
		name := m[1] + m[2]
		for i := len(vars) - 1; i >= 0; i-- {
			if v := vars[i]; v.name == name && v.offset < at && at >= 0 {
				if v.unknown {
					return ref
				}
				return v.value
			}
		}
		return ref
	})
}
