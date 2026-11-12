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
}

// parseCurl разбирает аргументы curl: опции тела и метода, связки коротких
// опций, адрес комментария среди остальных слов. expand подставляет в слово
// переменные команды: адрес бывает в URL=…/comment; curl "$URL"
func parseCurl(args []string, expand func(string) string) curlRequest {
	var req curlRequest
	for i := 0; i < len(args); i++ {
		arg := args[i]
		next := func() string {
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		switch {
		case curlDataOptions[arg]:
			req.data = append(req.data, curlData{option: arg, value: next()})
			req.write = true
		case arg == "--request":
			req.write = req.write || curlWriteMethods[strings.ToUpper(next())]
		case arg == "--upload-file":
			// файл уходит телом как есть, как -d @f; - значит вход
			req.data = append(req.data, curlData{option: arg, value: "@" + next()})
			req.write = true
		case arg == "--form":
			next()
			req.write = true
		case strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") && len(arg) > 1:
			req.shortCluster(arg, next)
		default:
			req.url = req.url || jiraCommentEndpoint.MatchString(arg) || jiraCommentEndpoint.MatchString(expand(arg))
		}
	}
	return req
}

// shortCluster разбирает связку коротких опций: -sSX POST, -d@f, -XPOST
func (req *curlRequest) shortCluster(arg string, next func() string) {
	for j := 1; j < len(arg); j++ {
		letter := arg[j]
		if !strings.ContainsRune(curlShortValued, rune(letter)) {
			continue
		}
		value := arg[j+1:]
		if value == "" {
			value = next()
		}
		switch letter {
		case 'd':
			req.data = append(req.data, curlData{option: "-d", value: value})
			req.write = true
		case 'X':
			req.write = req.write || curlWriteMethods[strings.ToUpper(value)]
		case 'T':
			req.data = append(req.data, curlData{option: "-T", value: "@" + value})
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
	raw   string // путь как в команде, без кавычек
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
	cwds    []string // cwds[i] - каталог перед вызовом i
	sends   []commentSend
	written []writtenFile
}

// planCommand проходит вызовы по порядку: cd меняет каталог для
// относительных путей (только в своей области: подоболочка, звено
// конвейера, вложенный скрипт — shellScopes), записи файлов и отправки
// комментария запоминаются
func planCommand(command, cwd, home string, calls []shellcmd.Call) commandPlan {
	plan := commandPlan{command: command, home: home, calls: calls, cwds: make([]string, len(calls))}
	plan.vars, plan.varsErr = commandVars(command, home)
	scopes, scopesErr := commandScopes(command)
	var changes []dirChange
	for i := range calls {
		c := &calls[i]
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

// addWrites запоминает файлы, которые пишет вызов index: перенаправление
// вывода, tee, правка на месте sed -i или perl -i
func (p *commandPlan) addWrites(index int, cwd string) {
	c := &p.calls[index]
	for _, r := range c.Redirs {
		if r.Op == ">" || r.Op == ">>" || r.Op == ">|" || r.Op == "&>" || r.Op == "&>>" {
			p.addWritten(index, r.Target, cwd)
		}
	}
	inPlace := slices.ContainsFunc(c.Args, func(a string) bool {
		return strings.HasPrefix(a, "-i") || strings.HasPrefix(a, "--in-place") ||
			(c.Name == "perl" && perlInPlace.MatchString(a))
	})
	if c.Name == "tee" || ((c.Name == "sed" || c.Name == "perl") && inPlace) {
		for _, a := range operands(c.Args) {
			p.addWritten(index, a, cwd)
		}
	}
}

func (p *commandPlan) addWritten(index int, raw, cwd string) {
	raw = p.path(raw, index)
	p.written = append(p.written, writtenFile{index: index, raw: raw, abs: absPath(raw, cwd, p.home)})
}

// perlInPlace связка ключей perl с правкой на месте: -pi, -0pi, -i.bak
var perlInPlace = regexp.MustCompile(`^-[0-9alnp]*i`)

// path путь из слова вызова index без кавычек, с переменными, присвоенными
// в команде раньше вызова. Неизвестная переменная остаётся в пути как $NAME
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

// operands слова вызова, не похожие на опции
func operands(args []string) []string {
	var out []string
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			out = append(out, a)
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
	return absPath(p.path(dirs[0], index), cwd, p.home)
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
	case c.Name == "popd" || len(dirs) == 0 || strings.HasPrefix(dirs[0], "+"):
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

// absPath абсолютный путь от каталога cwd; "" - не раскрыть до запуска
func absPath(path, cwd, home string) string {
	if path == "" || unexpanded(path) {
		return ""
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home == "" {
			return ""
		}
		path = home + path[1:]
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
}

// payloadFiles файлы тела отправки: -d @f, --json @f, -d "$(cat f)",
// -d @- со входом из файла (< f) или из cat f | curl. Тело из генератора
// (python3 - <<EOF | curl, heredoc на вход curl, литерал в -d) стоит в самой
// команде, файла у него нет
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
		case isFile:
			files = append(files, payloadSource{raw: p.path(path, send.index), cwd: send.cwd, at: send.index})
		case strings.Contains(value, "$(") || strings.Contains(value, "`"):
			files = append(files, p.substitutedFiles(send, value)...)
		}
	}
	return files
}

// substitutedFiles файлы, которые cat читает в подстановке -d "$(cat f)"
func (p *commandPlan) substitutedFiles(send *commentSend, value string) []payloadSource {
	var files []payloadSource
	for i, c := range p.calls {
		if c.Name != "cat" || i <= send.index || !strings.Contains(value, c.Raw) {
			continue
		}
		for _, a := range operands(c.Args) {
			files = append(files, payloadSource{raw: p.path(a, send.index), cwd: send.cwd, at: send.index})
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
		if r := stdinRedir(p.calls[stage].Redirs); r != nil {
			if r.Op == "<" {
				files = append(files, payloadSource{raw: p.path(r.Target, stage), cwd: p.cwds[stage], at: stage})
			}
			// вход из файла или heredoc: конвейер выше на этот вызов не идёт
			break
		}
	}
	return files
}

// stdinRedir перенаправление входа вызова: < f или heredoc; nil - его нет
func stdinRedir(redirs []shellcmd.Redir) *shellcmd.Redir {
	for i := range redirs {
		switch redirs[i].Op {
		case "<", "<<", "<<-", "<<<":
			return &redirs[i]
		}
	}
	return nil
}

// argFiles файлы, которые вызов конвейера читает аргументами: у cat - все
// операнды (нет файла - отказ), у генератора - те, что есть на диске
func (p *commandPlan) argFiles(index int) []payloadSource {
	c := p.calls[index]
	var files []payloadSource
	for _, a := range operands(c.Args) {
		files = append(files, payloadSource{raw: p.path(a, index), cwd: p.cwds[index], at: index, optional: c.Name != "cat"})
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
	abs := absPath(src.raw, src.cwd, p.home)
	if p.writtenBefore(src.at, src.raw, abs) {
		return "", true, nil
	}
	switch {
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
}

// shellVarRef ссылка на переменную в пути: $NAME или ${NAME}
var shellVarRef = regexp.MustCompile(`\$(\w+)|\$\{(\w+)\}`)

// commandVars присваивания команды по порядку текста. Присваивание перед
// командой (S=x cmd) в её аргументы не попадает и не берётся
func commandVars(command, home string) ([]shellVar, error) {
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(command), "")
	if err != nil {
		return nil, fmt.Errorf("разбор присваиваний: %w", err)
	}
	var vars []shellVar
	add := func(assigns []*syntax.Assign) {
		for _, a := range assigns {
			if a.Name == nil || a.Value == nil {
				continue
			}
			start, end := int(a.Value.Pos().Offset()), int(a.Value.End().Offset())
			if start < 0 || end > len(command) || start > end {
				continue
			}
			offset := int(a.Pos().Offset())
			value := expandVars(vars, normalizeShellPath(command[start:end]), offset)
			if (value == "~" || strings.HasPrefix(value, "~/")) && home != "" {
				value = home + value[1:]
			}
			vars = append(vars, shellVar{offset: offset, name: a.Name.Value, value: value})
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
	return vars, nil
}

// expandVars подставляет в путь значения переменных, присвоенных в команде
// до смещения at. Ссылка на переменную без присваивания остаётся текстом
func expandVars(vars []shellVar, path string, at int) string {
	return shellVarRef.ReplaceAllStringFunc(path, func(ref string) string {
		m := shellVarRef.FindStringSubmatch(ref)
		name := m[1] + m[2]
		for i := len(vars) - 1; i >= 0; i-- {
			if v := vars[i]; v.name == name && v.offset < at && at >= 0 {
				return v.value
			}
		}
		return ref
	})
}
