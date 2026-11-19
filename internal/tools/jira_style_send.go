package tools

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/aiseeq/akit/shellcmd"
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
	arg    int
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
		case strings.HasPrefix(arg, "--") && strings.Contains(arg, "="):
			option, value, _ := strings.Cut(arg, "=")
			switch {
			case curlDataOptions[option]:
				req.data = append(req.data, curlData{option: option, value: value, arg: i})
				req.write = true
			case option == "--upload-file":
				req.data = append(req.data, curlData{option: option, value: "@" + value, arg: i})
				req.write = true
			case option == "--request":
				req.write = req.write || curlWriteMethods[strings.ToUpper(value)]
			case option == "--form":
				req.write = true
			}
		case curlDataOptions[arg]:
			value := next()
			req.data = append(req.data, curlData{option: arg, value: value, arg: i})
			req.write = true
		case arg == "--request":
			req.write = req.write || curlWriteMethods[strings.ToUpper(next())]
		case arg == "--upload-file":
			// файл уходит телом как есть, как -d @f; - значит вход
			value := next()
			req.data = append(req.data, curlData{option: arg, value: "@" + value, arg: i})
			req.write = true
		case arg == "--form":
			next()
			req.write = true
		case strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") && len(arg) > 1:
			req.shortCluster(arg, next, func() int { return i })
		default:
			req.url = req.url || jiraCommentEndpoint.MatchString(arg) || jiraCommentEndpoint.MatchString(expand(arg))
		}
	}
	return req
}

// shortCluster разбирает связку коротких опций: -sSX POST, -d@f, -XPOST
func (req *curlRequest) shortCluster(arg string, next func() string, nextIndex func() int) {
	for j := 1; j < len(arg); j++ {
		letter := arg[j]
		if !strings.ContainsRune(curlShortValued, rune(letter)) {
			continue
		}
		value := arg[j+1:]
		argIndex := -1
		if value == "" {
			value = next()
			argIndex = nextIndex()
		}
		switch letter {
		case 'd':
			req.data = append(req.data, curlData{option: "-d", value: value, arg: argIndex})
			req.write = true
		case 'X':
			req.write = req.write || curlWriteMethods[strings.ToUpper(value)]
		case 'T':
			req.data = append(req.data, curlData{option: "-T", value: "@" + value, arg: argIndex})
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
	vars    shellVars
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
	plan.vars, plan.varsErr = commandVars(command, home, calls)
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
			expand := func(arg string) string { return plan.path(arg, i, 0) }
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
			p.addWritten(index, r.Target, r.Tilde, cwd)
		}
	}
	inPlace := slices.ContainsFunc(c.Args, func(a string) bool {
		return strings.HasPrefix(a, "-i") || strings.HasPrefix(a, "--in-place") ||
			(c.Name == "perl" && perlInPlace.MatchString(a))
	})
	if c.Name == "tee" || ((c.Name == "sed" || c.Name == "perl") && inPlace) {
		for ai, a := range c.Args {
			if !strings.HasPrefix(a, "-") {
				p.addWritten(index, a, c.ArgTilde(ai), cwd)
			}
		}
	}
}

func (p *commandPlan) addWritten(index int, raw string, tilde shellcmd.Tilde, cwd string) {
	raw = p.path(raw, index, tilde)
	p.written = append(p.written, writtenFile{index: index, raw: raw, abs: absPath(raw, cwd)})
}

// perlInPlace связка ключей perl с правкой на месте: -pi, -0pi, -i.bak
var perlInPlace = regexp.MustCompile(`^-[0-9alnp]*i`)

// path путь из слова вызова index без кавычек, с переменными, присвоенными
// в команде раньше вызова. Неизвестная переменная остаётся в пути как $NAME
func (p *commandPlan) path(raw string, index int, tilde shellcmd.Tilde) string {
	raw = tildePath(normalizeShellPath(raw), tilde, p.home)
	return p.vars.expand(raw, p.callOffset(index))
}

// tildePath applies only the expansions identified by shellcmd. Other or
// mode-dependent forms become an unresolvable path so callers fail closed.
func tildePath(path string, mark shellcmd.Tilde, home string) string {
	if mark&(shellcmd.TildeOther|shellcmd.TildeModeUnknown) != 0 || (mark&shellcmd.TildeHome != 0 && home == "") {
		return "$__akit_tilde_unknown"
	}
	if mark&shellcmd.TildeHome == 0 {
		return path
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		return home + path[1:]
	}
	return path
}

// tildeAssignmentPath expands a single assignment-value tilde whose position
// is recoverable from the shellcmd bitmask. Multiple candidates are
// ambiguous because Assign.Tilde does not identify which occurrence expanded.
func tildeAssignmentPath(path string, mark shellcmd.Tilde, home string) string {
	if mark&(shellcmd.TildeOther|shellcmd.TildeModeUnknown) != 0 || mark&shellcmd.TildeHome != 0 && home == "" {
		return "$__akit_tilde_unknown"
	}
	if mark&shellcmd.TildeHome == 0 {
		return path
	}
	candidate := -1
	for i := 0; i < len(path); i++ {
		if path[i] != '~' || i > 0 && path[i-1] != ':' || i+1 < len(path) && path[i+1] != '/' {
			continue
		}
		if candidate >= 0 {
			return "$__akit_tilde_unknown"
		}
		candidate = i
	}
	if candidate < 0 {
		return "$__akit_tilde_unknown"
	}
	return path[:candidate] + home + path[candidate+1:]
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

func firstOperandIndex(args []string) int {
	for i, a := range args {
		if !strings.HasPrefix(a, "-") {
			return i
		}
	}
	return -1
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
	return absPath(p.path(dirs[0], index, p.calls[index].ArgTilde(firstOperandIndex(args))), cwd)
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
func absPath(path, cwd string) string {
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
			mark := shellcmd.Tilde(0)
			if d.arg >= 0 {
				mark = send.call.ArgTilde(d.arg)
			}
			files = append(files, payloadSource{raw: p.path(path, send.index, mark), cwd: send.cwd, at: send.index})
		case strings.Contains(value, "$(") || strings.Contains(value, "`"):
			files = append(files, p.substitutedFiles(send, value)...)
		}
	}
	return files
}

// substitutedFiles файлы, которые cat читает в подстановке -d "$(cat f)"
func (p *commandPlan) substitutedFiles(send *commentSend, value string) []payloadSource {
	var files []payloadSource
	for i := range p.calls {
		c := &p.calls[i]
		if c.Name != "cat" || i <= send.index || !strings.Contains(value, c.Raw) {
			continue
		}
		for ai, a := range c.Args {
			if strings.HasPrefix(a, "-") {
				continue
			}
			files = append(files, payloadSource{raw: p.path(a, send.index, c.ArgTilde(ai)), cwd: send.cwd, at: send.index})
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
				files = append(files, payloadSource{raw: p.path(r.Target, stage, r.Tilde), cwd: p.cwds[stage], at: stage})
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
	for ai, a := range c.Args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		files = append(files, payloadSource{raw: p.path(a, index, c.ArgTilde(ai)), cwd: p.cwds[index], at: index, optional: c.Name != "cat"})
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
	abs := absPath(src.raw, src.cwd)
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
// в значении текстом ($JIRA_BASE_URL, $(pwd)). У присваивания с неизвестным
// значением (assign.Unknown: вызов функции, которая меняет переменную,
// S[0]=, переменная for) value пустое: ссылка на такую переменную не раскрывается
type shellVar struct {
	assign shellcmd.Assign
	name   string
	value  string
}

// shellVars присваивания команды с их значениями и области команды (тела
// функций, звенья конвейера), по которым ссылка на переменную раскрывается
// или остаётся текстом
type shellVars struct {
	vars    []shellVar
	assigns []shellcmd.Assign // все присваивания команды, и после ссылки
	scopes  []shellcmd.Scope
	calls   []shellcmd.Call
}

// shellVarRef ссылка на переменную в пути: $NAME или ${NAME}
var shellVarRef = regexp.MustCompile(`\$(\w+)|\$\{(\w+)\}`)

// commandVars присваивания команды по порядку текста, каждое со своей областью
// видимости (подоболочка, звено конвейера, тело функции). Присваивание перед
// командой (S=x cmd) в её аргументы не попадает и не берётся
func commandVars(command, home string, calls []shellcmd.Call) (shellVars, error) {
	assigns, err := shellcmd.Assigns(command)
	if err != nil {
		return shellVars{}, fmt.Errorf("разбор присваиваний: %w", err)
	}
	scopes, err := shellcmd.Scopes(command)
	if err != nil {
		return shellVars{}, fmt.Errorf("разбор областей: %w", err)
	}
	sv := shellVars{assigns: assigns, scopes: scopes, calls: calls}
	for _, a := range assigns {
		if a.Unknown {
			sv.vars = append(sv.vars, shellVar{assign: a, name: a.Name})
			continue
		}
		value := sv.expand(tildeAssignmentPath(normalizeShellPath(a.Value), a.Tilde, home), a.Offset)
		if a.Append {
			value = sv.expand("${"+a.Name+"}", a.Offset) + value
		}
		sv.vars = append(sv.vars, shellVar{assign: a, name: a.Name, value: value})
	}
	return sv, nil
}

// expand подставляет в путь значения переменных, присвоенных в команде и
// видимых на смещении at. Ссылка остаётся текстом - такой путь до запуска не
// раскрыть, прежнее значение не берётся, - когда у переменной нет видимого
// присваивания, последнее видимое - неизвестного значения (Unknown) или
// значение на месте ссылки зависит от вызова функции (unsettled)
func (sv *shellVars) expand(path string, at int) string {
	return shellVarRef.ReplaceAllStringFunc(path, func(ref string) string {
		m := shellVarRef.FindStringSubmatch(ref)
		name := m[1] + m[2]
		if at < 0 || sv.unsettled(name, at) {
			return ref
		}
		if v, ok := sv.lastVisible(name, at); ok && !v.assign.Unknown {
			return v.value
		}
		return ref
	})
}

// lastVisible последнее присваивание name, видимое на смещении at
func (sv *shellVars) lastVisible(name string, at int) (shellVar, bool) {
	for i := len(sv.vars) - 1; i >= 0; i-- {
		if v := sv.vars[i]; v.name == name && v.assign.VisibleAt(at) {
			return v, true
		}
	}
	return shellVar{}, false
}

// unsettled значение переменной name на смещении at статически не узнать,
// хотя видимое присваивание может быть (известное значение или Unknown):
//   - тело функции присваивает name, а ссылка - вне этого тела после
//     определения функции: функцию мог вызвать alias, trap DEBUG/ERR,
//     mapfile -C или другая функция. Отказ и тогда, когда функция не
//     вызвана, - цена правила;
//   - ссылка в теле функции, а name присвоено в теле другой функции, после
//     определения этой функции или в её же теле после ссылки: тело
//     выполнится при вызове, после этих присваиваний. Вызовы самой функции
//     не в счёт, когда значение на месте ссылки задано известным
//     присваиванием в её теле до ссылки (selfCalls);
//   - name присвоено где-либо в звене конвейера, на любой глубине: при
//     shopt -s lastpipe (его включают и trap, alias, eval, source, тело
//     функции, повтор цикла) присваивание последнего звена переживает
//     конвейер. Включён ли lastpipe и в каком порядке, не выясняется: отказ
//     и на конвейере без lastpipe - цена правила
func (sv *shellVars) unsettled(name string, at int) bool {
	// Встроенные команды меняют переменные, но Assigns их не возвращает.
	// Учитываем даже вызовы после ссылки: они могут быть в цикле или функции.
	for i := range sv.calls {
		c := &sv.calls[i]
		if builtinMayWrite(*c, name) {
			return true
		}
	}
	var around []shellcmd.Scope // тела функций, в которых ссылка
	for _, s := range sv.scopes {
		if s.Kind == shellcmd.ScopeFunctionBody && s.Start <= at && at < s.End {
			around = append(around, s)
		}
	}
	self := sv.selfCalls(name, at, around)
	for _, a := range sv.assigns {
		if a.Name != name || self(a) {
			continue
		}
		if body, ok := sv.leakingBody(a); ok && !(body.Start <= at && at < body.End) && at > body.DeclOffset {
			return true
		}
		if len(around) > 0 && sv.laterForBody(a, at, around) {
			return true
		}
		if sv.inScope(a.Offset, shellcmd.ScopePipePart) {
			return true
		}
	}
	return false
}

// builtinMayWrite консервативно учитывает имена аргументов встроенных команд.
// Совпадение с аргументом опции тоже даёт отказ.
func builtinMayWrite(c shellcmd.Call, name string) bool {
	switch c.Name {
	case "read", "unset", "mapfile", "readarray", "getopts":
		for _, arg := range c.Args {
			if arg == name || strings.HasPrefix(arg, name+"[") || unexpanded(arg) {
				return true
			}
		}
		return c.Name == "read" && name == "REPLY" && len(c.Args) == 0
	case "printf":
		for i, arg := range c.Args {
			if arg == "-v" && i+1 < len(c.Args) {
				target := c.Args[i+1]
				return target == name || strings.HasPrefix(target, name+"[") || unexpanded(target)
			}
			if strings.HasPrefix(arg, "-v") && len(arg) > 2 {
				target := arg[2:]
				return target == name || strings.HasPrefix(target, name+"[") || unexpanded(target)
			}
		}
	}
	return false
}

// selfCalls отбирает Unknown-присваивания name от вызовов функции, в теле
// которой стоит ссылка at, когда последнее видимое на месте ссылки
// присваивание name - известное и стоит прямо в этом теле (не в подоболочке):
// вызов функции снова выполнит это присваивание до ссылки. Вызов через eval
// не распознаётся и остаётся в счёте
func (sv *shellVars) selfCalls(name string, at int, around []shellcmd.Scope) func(shellcmd.Assign) bool {
	none := func(shellcmd.Assign) bool { return false }
	v, ok := sv.lastVisible(name, at)
	if !ok || v.assign.Unknown {
		return none
	}
	i := slices.IndexFunc(around, func(s shellcmd.Scope) bool {
		return s.Start == v.assign.ScopeStart && s.End == v.assign.ScopeEnd
	})
	if i < 0 {
		return none
	}
	fn := around[i].Name
	return func(a shellcmd.Assign) bool {
		if !a.Unknown {
			return false
		}
		for i := range sv.calls {
			c := &sv.calls[i]
			if c.Name == fn && c.Offset == a.Offset {
				return true
			}
		}
		return false
	}
}

// leakingBody тело функции, из которого присваивание a доходит до
// вызвавшей оболочки: a - прямо в теле (не в подоболочке и не в звене
// конвейера внутри него), тело в фигурных скобках, не f() ( … )
func (sv *shellVars) leakingBody(a shellcmd.Assign) (shellcmd.Scope, bool) {
	for _, s := range sv.scopes {
		if s.Kind == shellcmd.ScopeFunctionBody && !s.Isolated && s.Start == a.ScopeStart && s.End == a.ScopeEnd {
			return s, true
		}
	}
	return shellcmd.Scope{}, false
}

// laterForBody присваивание a может сработать до ссылки at, которая стоит в
// телах функций around: a - в теле другой функции, вне around после
// определения внешней из них или в around после ссылки (повторный вызов)
func (sv *shellVars) laterForBody(a shellcmd.Assign, at int, around []shellcmd.Scope) bool {
	inside := func(s shellcmd.Scope) bool { return s.Start <= a.Offset && a.Offset < s.End }
	if slices.ContainsFunc(around, inside) {
		return a.Offset > at
	}
	if sv.inScope(a.Offset, shellcmd.ScopeFunctionBody) {
		return true
	}
	decl := at
	for _, s := range around {
		decl = min(decl, s.DeclOffset)
	}
	return a.Offset > decl
}

// inScope смещение off внутри области вида kind
func (sv *shellVars) inScope(off int, kind shellcmd.ScopeKind) bool {
	return slices.ContainsFunc(sv.scopes, func(s shellcmd.Scope) bool {
		return s.Kind == kind && s.Start <= off && off < s.End
	})
}
