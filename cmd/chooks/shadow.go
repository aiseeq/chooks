package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aiseeq/chooks/internal/core"
	"github.com/aiseeq/chooks/internal/desktop"
	"github.com/aiseeq/chooks/internal/hookctx"
	"github.com/aiseeq/chooks/internal/hookproxy"
	"github.com/aiseeq/chooks/internal/hookwire"
	"github.com/aiseeq/chooks/internal/nighthint"
	"github.com/aiseeq/chooks/internal/procfs"
	"github.com/aiseeq/chooks/internal/tools/notifier"
)

// Тень (тикет hooks-daemon, этап 2б-3а): CLI с daemon.shadow, исполнив
// сравниваемый хук сам, шлёт демону запись — запрос, свой результат, время
// запроса, ответы /proc, состояние до исполнения и после. Демон исполняет ту
// же работу подкоманды (dispatchHook) на записанном: время и /proc — из
// записи, состояние — во временном каталоге из снимка «до», лог — в никуда,
// эффекты уведомления только записываются, — сравнивает и пишет строку в журнал тени. Внешние
// эффекты пользователь получает один раз — от CLI.

// shadowHook — сравниваемая подкоманда: файлы состояния, которые она пишет
// и читает (снимок «до» и запись «после»); nil — состояния не пишет
type shadowHook struct {
	state func(sessionID string) ([]shadowPath, error)
}

// shadowHooks — подкоманды без внешних эффектов, кроме уведомления и
// состояния (решение мастера 22.11): уведомление в тени не исполняется,
// состояние сравнивается по снимку
var shadowHooks = map[string]shadowHook{
	hookPreToolUse:   {},
	hookNoPrompt:     {},
	hookUnsentCheck:  {},
	hookStop:         {state: sessionStatePaths},
	hookStopFailure:  {state: sessionStatePaths},
	hookNotification: {state: sessionStatePaths},
	hookNightHint:    {state: nightHintPaths},
}

// shadowOutside — подкоманды хуков вне тени и почему; им держится набор
// равенства scripts/daemon
var shadowOutside = map[string]string{
	hookUnderstandCheck:  "судья claude -p: внешний процесс модели",
	hookClaimStop:        "судья claude -p: внешний процесс модели",
	hookWebFetchFallback: "сеть: webtext скачивает страницу",
	hookUserPromptSubmit: "skill-reload запускает git (dry run), проверка модели claude читает /proc через modellaunch.ProcReader мимо записи",
	hookPostToolBatch:    "skill-reload запускает git (dry run)",
	hookStatusLine:       "запускает ротацию учёток (процесс), пишет снимок лимитов и читает git и транскрипт без снимка",
}

// Корни файлов состояния
const (
	shadowRootRuntime = "runtime" // RuntimeDir запроса: $XDG_RUNTIME_DIR/chooks
	shadowRootState   = "state"   // StateDir запроса: $XDG_STATE_HOME/chooks
)

// shadowPath — файл состояния: корень и путь от него
type shadowPath struct {
	Root string `json:"root"`
	Rel  string `json:"rel"`
}

func sessionStatePaths(sessionID string) ([]shadowPath, error) {
	var paths []shadowPath
	for _, rel := range core.SessionStateFiles(sessionID) {
		paths = append(paths, shadowPath{Root: shadowRootRuntime, Rel: rel})
	}
	return paths, nil
}

func nightHintPaths(sessionID string) ([]shadowPath, error) {
	name, err := nighthint.RecordName(sessionID)
	if err != nil {
		return nil, err
	}
	return []shadowPath{{Root: shadowRootState, Rel: filepath.Join("night-hint", name)}}, nil
}

// shadowFile — содержимое файла состояния: нет файла — Exists false;
// Err — файл не прочитан
type shadowFile struct {
	shadowPath
	Exists bool   `json:"exists"`
	Data   []byte `json:"data,omitempty"`
	Err    string `json:"err,omitempty"`
}

// abs — путь файла под корнем запроса. Rel, выходящий из корня («..»,
// абсолютный), — ошибка: путь приходит и из записи CLI
func (p shadowPath) abs(request hookctx.Context) (string, error) {
	if !filepath.IsLocal(p.Rel) {
		return "", fmt.Errorf("путь %q вне корня %s", p.Rel, p.Root)
	}
	switch p.Root {
	case shadowRootRuntime:
		return filepath.Join(request.RuntimeDir(), p.Rel), nil
	case shadowRootState:
		dir, err := request.StateDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(dir, p.Rel), nil
	}
	return "", fmt.Errorf("unknown state root %q", p.Root)
}

func readShadowFiles(request hookctx.Context, paths []shadowPath) []shadowFile {
	files := make([]shadowFile, 0, len(paths))
	for _, p := range paths {
		file := shadowFile{shadowPath: p}
		path, err := p.abs(request)
		if err == nil {
			file.Data, err = os.ReadFile(path)
		}
		switch {
		case err == nil:
			file.Exists = true
		case errors.Is(err, os.ErrNotExist):
			file.Data = nil
		default:
			file.Data, file.Err = nil, err.Error()
		}
		files = append(files, file)
	}
	return files
}

// shadowAnswer — ответ зависимости ночной подсказки (logind, запись
// skill-reload). SkillUnknown сохраняет errors.Is(err, ErrSkillUnknown)
type shadowAnswer struct {
	Value        bool   `json:"value"`
	Err          string `json:"err,omitempty"`
	SkillUnknown bool   `json:"skill_unknown,omitempty"`
}

func (a shadowAnswer) result() (bool, error) {
	switch {
	case a.Err == "":
		return a.Value, nil
	case a.SkillUnknown:
		return a.Value, fmt.Errorf("%s: %w", a.Err, nighthint.ErrSkillUnknown)
	}
	return a.Value, errors.New(a.Err)
}

func answerOf(value bool, err error) shadowAnswer {
	a := shadowAnswer{Value: value}
	if err != nil {
		a.Err, a.SkillUnknown = err.Error(), errors.Is(err, nighthint.ErrSkillUnknown)
	}
	return a
}

// nightAnswers — ответы зависимостей night-hint по порядку вызовов
type nightAnswers struct {
	Locked    []shadowAnswer `json:"locked,omitempty"`
	LidClosed []shadowAnswer `json:"lid_closed,omitempty"`
	Skill     []shadowAnswer `json:"skill,omitempty"`
}

// shadowVerdict — решение хука: allow, deny, ask, block, alert, mute, hint…
// и текст причины
type shadowVerdict struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason,omitempty"`
}

// shadowRecord — запись CLI для демона. Запрос (stdin, env, cwd, pid,
// ppid) идёт в кадре hookwire, остальное — здесь
type shadowRecord struct {
	Session    string        `json:"session"`
	Time       time.Time     `json:"time"`
	ConfigPath string        `json:"config_path,omitempty"`
	Verbose    bool          `json:"verbose,omitempty"`
	Timeout    time.Duration `json:"timeout"`
	// Result CLI
	Code     int                `json:"code"`
	Stdout   []byte             `json:"stdout,omitempty"`
	Stderr   []byte             `json:"stderr,omitempty"`
	Err      string             `json:"err,omitempty"`
	Verdict  shadowVerdict      `json:"verdict"`
	Decision *notifier.Decision `json:"decision,omitempty"`
	// Effects — эффекты уведомления CLI по порядку (shadowCapture.effects)
	Effects []string `json:"effects,omitempty"`
	// Входы, которые демон берёт из записи
	Proc   []procfs.Read `json:"proc,omitempty"`
	Night  *nightAnswers `json:"night,omitempty"`
	Before []shadowFile  `json:"before,omitempty"`
	// After — файлы состояния после CLI
	After []shadowFile `json:"after,omitempty"`
	// Incomparable — CLI знает, что сравнения не будет, и почему
	Incomparable string `json:"incomparable,omitempty"`
}

// shadowCapture — то, что работа подкоманды оставляет для сравнения: решение
// pre-tool-use, notifier.Decision, ответы зависимостей night-hint. У CLI
// ответы записываются, у демона — берутся из записи (replay)
type shadowCapture struct {
	mu       sync.Mutex
	action   string
	message  string
	decision *notifier.Decision
	night    nightAnswers
	// replay — демон: ответы night из записи; used — сколько взято
	replay  *nightAnswers
	used    [3]int
	missing []string
	// effects — эффекты уведомления по порядку (заголовок Konsole,
	// оповещение); dry — демон: только запись, без исполнения
	effects []string
	dry     bool
	// paths — файлы состояния вызова; after — их снимок сразу за записью
	// состояния события сессии, до уведомления; afterTaken false — снимка
	// не было, «после» читается по окончании работы
	paths      []shadowPath
	after      []shadowFile
	afterTaken bool
}

// snapshotAfter снимает «после» сразу за записью состояния события сессии:
// до уведомления и до записей соседей (строка статуса, другой хук той же
// сессии), которые иначе попали бы в сравнение как запись CLI
func (c *shadowCapture) snapshotAfter(request hookctx.Context) {
	files := readShadowFiles(request, c.paths)
	c.mu.Lock()
	c.after, c.afterTaken = files, true
	c.mu.Unlock()
}

// afterFiles — снимок «после»: сделанный за записью состояния, иначе
// чтение сейчас
func (c *shadowCapture) afterFiles(request hookctx.Context) []shadowFile {
	c.mu.Lock()
	after, taken := c.after, c.afterTaken
	c.mu.Unlock()
	if taken {
		return after
	}
	return readShadowFiles(request, c.paths)
}

// notifierEffects — эффекты уведомления под тенью: у CLI исполняются и
// записываются, у демона (dry) только записываются. Заголовок OSC эффектом
// не считается: он пишется в stderr запроса, а stderr сравнивается
func (c *shadowCapture) notifierEffects(real notifier.Effects) notifier.Effects {
	note := func(effect string) {
		c.mu.Lock()
		c.effects = append(c.effects, effect)
		c.mu.Unlock()
	}
	return notifier.Effects{
		KonsoleTitle: func(service, session, title string, request hookctx.Context) error {
			note(fmt.Sprintf("konsole title %s %s %q", service, session, title))
			if c.dry {
				return nil
			}
			return real.KonsoleTitle(service, session, title, request)
		},
		Deliver: func(executable string, alert desktop.Alert, request hookctx.Context) error {
			note(fmt.Sprintf("alert %+v", alert))
			if c.dry {
				return nil
			}
			return real.Deliver(executable, alert, request)
		},
	}
}

// realNotifierEffects — исполняемые эффекты уведомления под тенью; тест
// подставляет обёртку со счётчиком доставок: вызов Deliver синхронен, и счёт
// виден сразу, без ожидания фонового процесса оповещения
var realNotifierEffects = notifier.RealEffects

type shadowCaptureKey struct{}

func shadowCaptureFrom(ctx context.Context) *shadowCapture {
	capture, _ := ctx.Value(shadowCaptureKey{}).(*shadowCapture)
	return capture
}

// nightDeps — зависимости night-hint тени: у CLI живые с записью ответов,
// у демона — ответы записи CLI. Store и Now не трогаются: Store лежит под
// StateDir запроса (у демона — временный каталог), Now — время запроса
func (c *shadowCapture) nightDeps(deps nighthint.Deps) nighthint.Deps {
	if c.replay != nil {
		deps.Locked = func(context.Context) (bool, error) { return c.take(0, "locked", c.replay.Locked) }
		deps.LidClosed = func(context.Context) (bool, error) { return c.take(1, "lid_closed", c.replay.LidClosed) }
		deps.SkillCalled = func(string) (bool, error) { return c.take(2, "skill", c.replay.Skill) }
		return deps
	}
	record := func(into *[]shadowAnswer, value bool, err error) {
		c.mu.Lock()
		*into = append(*into, answerOf(value, err))
		c.mu.Unlock()
	}
	if locked := deps.Locked; locked != nil {
		deps.Locked = func(ctx context.Context) (bool, error) {
			v, err := locked(ctx)
			record(&c.night.Locked, v, err)
			return v, err
		}
	}
	if lid := deps.LidClosed; lid != nil {
		deps.LidClosed = func(ctx context.Context) (bool, error) {
			v, err := lid(ctx)
			record(&c.night.LidClosed, v, err)
			return v, err
		}
	}
	if skill := deps.SkillCalled; skill != nil {
		deps.SkillCalled = func(sessionID string) (bool, error) {
			v, err := skill(sessionID)
			record(&c.night.Skill, v, err)
			return v, err
		}
	}
	return deps
}

// errShadowNotRecorded — демону нужен ответ, которого CLI не получал
var errShadowNotRecorded = errors.New("shadow: answer not recorded by CLI")

func (c *shadowCapture) take(i int, name string, answers []shadowAnswer) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.used[i] >= len(answers) {
		c.missing = append(c.missing, "night "+name)
		return false, errShadowNotRecorded
	}
	a := answers[c.used[i]]
	c.used[i]++
	return a.result()
}

// shadowMaxOutput — сколько stdout и stderr CLI держит для тени: больше —
// запись «не сравнимо», хук от этого не меняется
const shadowMaxOutput = 1 << 20

// shadowSendTimeout — срок connect и срок записи кадра: демона нет, он не читает
// или кадр больше буфера сокета — CLI ждёт не дольше этого и молча идёт
// дальше (мера 3: прирост меньше 100 мс)
const shadowSendTimeout = 30 * time.Millisecond

// cappedBuffer — копия вывода CLI для тени, не больше shadowMaxOutput
type cappedBuffer struct {
	buf  bytes.Buffer
	over bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b.over || b.buf.Len()+len(p) > shadowMaxOutput {
		b.over = true
		return len(p), nil
	}
	b.buf.Write(p)
	return len(p), nil
}

// shadowSession — тень одного вызова CLI
type shadowSession struct {
	hookType string
	logger   core.Logger
	request  hookctx.Context
	data     []byte
	paths    []shadowPath
	record   shadowRecord
	proc     *procfs.Recorder
	capture  *shadowCapture
	stdout   *cappedBuffer
	stderr   *cappedBuffer
}

// beginShadow — до работы хука: время запроса, запись /proc, снимок
// состояния «до», копия stdout и stderr. Возвращает ctx и запрос, с
// которыми CLI исполняет хук
// session — сессия входа, уже разобранная для строки длительности: второй
// разбор входа в мегабайты стоил бы CLI десятки мс
func beginShadow(ctx context.Context, hook shadowHook, hookType string, data []byte, session string, config *core.Config, logger core.Logger, request hookctx.Context) (context.Context, hookctx.Context, *shadowSession) {
	var paths []shadowPath
	if hook.state != nil {
		var stateErr error
		paths, stateErr = hook.state(session)
		if stateErr != nil {
			logger.Warn("shadow state path unavailable", "hook", hookType, "error", stateErr)
			return ctx, request, nil
		}
	}
	s := &shadowSession{hookType: hookType, logger: logger, data: data, capture: &shadowCapture{}, stdout: &cappedBuffer{}, stderr: &cappedBuffer{}}
	s.proc = procfs.NewRecorder(request.ProcFS())
	request.Proc = s.proc
	s.paths = paths
	s.capture.paths = s.paths
	configPath := request.ConfigPath
	if configPath != "" && !filepath.IsAbs(configPath) {
		configPath = filepath.Join(request.Cwd, configPath)
	}
	s.record = shadowRecord{Session: session, Time: request.Now(), ConfigPath: configPath, Verbose: request.Verbose, Timeout: request.Timeout,
		Before: readShadowFiles(request, s.paths)}
	if config.Logger.Output != "file" {
		// Строки лога идут в stderr со временем и строками таймера CLI:
		// stderr демона с ними не совпадёт
		s.record.Incomparable = "logger.output не file: stderr несёт строки лога"
	}
	if !filepath.IsAbs(request.Cwd) {
		s.record.Incomparable = "cwd CLI не абсолютный (каталог удалён)"
	}
	request.Stdout = io.MultiWriter(request.Stdout, s.stdout)
	request.Stderr = io.MultiWriter(request.Stderr, s.stderr)
	s.request = request
	ctx = context.WithValue(hookctx.WithRequest(ctx, request), shadowCaptureKey{}, s.capture)
	return ctx, request, s
}

// send — после работы хука: connect к демону, затем запись (результат,
// решение, ответы, состояние «после») и кадр. Сбой — строкой Warn в журнал:
// хук уже ответил, а неотправленная запись видна в сводке shadow-report как
// пропущенная
func (s *shadowSession) send(code int, err error) {
	path := hookproxy.SocketPath(s.request.Getenv)
	if path == "" {
		return
	}
	if sendErr := hookproxy.SendShadow(path, shadowSendTimeout, func() ([]byte, error) { return s.frame(code, err) }); sendErr != nil {
		s.logger.Warn("shadow record not sent", "hook", s.hookType, "error", sendErr)
	}
}

// frame — кадр тени по исполнению CLI
func (s *shadowSession) frame(code int, err error) ([]byte, error) {
	rec := s.record
	rec.Code = code
	if err != nil {
		rec.Err = err.Error()
	}
	rec.Stdout, rec.Stderr = s.stdout.buf.Bytes(), s.stderr.buf.Bytes()
	if s.stdout.over || s.stderr.over {
		rec.Stdout, rec.Stderr, rec.Incomparable = nil, nil, "вывод больше 1 МиБ"
	}
	s.capture.mu.Lock()
	rec.Decision = s.capture.decision
	rec.Effects = s.capture.effects
	if night := s.capture.night; len(night.Locked)+len(night.LidClosed)+len(night.Skill) > 0 {
		rec.Night = &night
	}
	s.capture.mu.Unlock()
	rec.Verdict = verdictOf(s.hookType, code, rec.Stdout, s.capture)
	rec.Proc = s.proc.Reads()
	rec.After = s.capture.afterFiles(s.request)
	blob, err := json.Marshal(rec)
	if err != nil {
		return nil, fmt.Errorf("запись тени: %w", err)
	}
	wire := hookwire.Request{Subcommand: s.hookType, Env: s.request.Env, Cwd: s.request.Cwd, PID: s.request.PID, PPID: s.request.PPID, Stdin: s.data}
	return hookwire.EncodeShadow(wire, blob), nil
}

// verdictOf — решение хука для сравнения. pre-tool-use — Action и Message
// ответа проверок; no-prompt, unsent-check и night-hint — поля их stdout;
// события сессии — notifier.Decision (Alert и Reason)
func verdictOf(hookType string, code int, stdout []byte, capture *shadowCapture) shadowVerdict {
	switch hookType {
	case hookPreToolUse:
		if capture.action == "" {
			return shadowVerdict{Decision: fmt.Sprintf("none (code %d)", code)}
		}
		return shadowVerdict{Decision: capture.action, Reason: capture.message}
	case hookNoPrompt:
		var out struct {
			HookSpecificOutput struct {
				Decision struct {
					Behavior string `json:"behavior"`
					Message  string `json:"message"`
				} `json:"decision"`
				Action string `json:"action"`
			} `json:"hookSpecificOutput"`
		}
		if json.Unmarshal(stdout, &out) != nil {
			return shadowVerdict{Decision: "none"}
		}
		if out.HookSpecificOutput.Action != "" {
			return shadowVerdict{Decision: out.HookSpecificOutput.Action}
		}
		return shadowVerdict{Decision: out.HookSpecificOutput.Decision.Behavior, Reason: out.HookSpecificOutput.Decision.Message}
	case hookUnsentCheck:
		var out stopBlockOutput
		if json.Unmarshal(stdout, &out) != nil || out.Decision == "" {
			return shadowVerdict{Decision: "allow"}
		}
		return shadowVerdict{Decision: out.Decision, Reason: out.Reason}
	case hookNightHint:
		var out hookContextOutput
		if json.Unmarshal(stdout, &out) != nil || out.HookSpecificOutput.AdditionalContext == "" {
			return shadowVerdict{Decision: "none"}
		}
		return shadowVerdict{Decision: "hint", Reason: out.HookSpecificOutput.AdditionalContext}
	}
	capture.mu.Lock()
	defer capture.mu.Unlock()
	if capture.decision == nil {
		return shadowVerdict{Decision: "none"}
	}
	if capture.decision.Alert {
		return shadowVerdict{Decision: "alert", Reason: capture.decision.Reason}
	}
	return shadowVerdict{Decision: "mute", Reason: capture.decision.Reason}
}

// Исход сравнения в журнале тени
const (
	shadowMatch        = "match"
	shadowMismatch     = "mismatch"
	shadowIncomparable = "incomparable"
)

// shadowJournalFile — журнал тени рядом с chooks.log
const shadowJournalFile = "chooks-shadow.tsv"

// shadowOutcome — исполнение демона по записи
type shadowOutcome struct {
	Code     int
	Stdout   []byte
	Stderr   []byte
	Err      string
	Verdict  shadowVerdict
	Decision *notifier.Decision
	Effects  []string
	After    []shadowFile
	// Missing — ответы, которых нет в записи: сравнение невозможно
	Missing []string
	// roots — каталоги состояния демона и CLI для нормализации путей
	roots [][2]string
}

// shadowNormalizers — поля, доказанно разные у CLI и демона при одном входе
// и одних ответах; всё остальное сравнивается байт в байт. Применяются к
// stdout, stderr и тексту ошибки обеих сторон
var shadowNormalizers = []struct {
	name, reason string
	apply        func(text string, roots [][2]string) string
}{
	{
		name:   "process_time",
		reason: "строка «⏱ N ms» pre-tool-use --verbose — замер часов исполнителя (HookResponse.ProcessTime от time.Now в processor)",
		apply: func(text string, _ [][2]string) string {
			return processTimeRe.ReplaceAllString(text, "⏱  <ms> ms")
		},
	},
	{
		name:   "state_roots",
		reason: "пути состояния: демон пишет состояние во временный каталог вместо RuntimeDir и StateDir запроса",
		apply: func(text string, roots [][2]string) string {
			for _, r := range roots {
				text = strings.ReplaceAll(text, r[0], r[1])
			}
			return text
		},
	},
}

var processTimeRe = regexp.MustCompile(`⏱  [0-9.]+ ms`)

func normalizeShadow(text string, roots [][2]string) string {
	for _, n := range shadowNormalizers {
		text = n.apply(text, roots)
	}
	return text
}

// compareShadow — исход и подробность: поля расхождения или причина
// несравнимости
func compareShadow(rec shadowRecord, out shadowOutcome) (string, string) {
	if rec.Incomparable != "" {
		return shadowIncomparable, rec.Incomparable
	}
	if len(out.Missing) > 0 {
		return shadowIncomparable, "нет в записи CLI: " + strings.Join(out.Missing, ", ")
	}
	var fields []string
	if rec.Code != out.Code {
		fields = append(fields, fmt.Sprintf("code %d/%d", rec.Code, out.Code))
	}
	if rec.Verdict.Decision != out.Verdict.Decision {
		fields = append(fields, fmt.Sprintf("decision %s/%s", rec.Verdict.Decision, out.Verdict.Decision))
	}
	if normalizeShadow(rec.Verdict.Reason, out.roots) != normalizeShadow(out.Verdict.Reason, out.roots) {
		fields = append(fields, "reason")
	}
	for _, f := range []struct {
		name     string
		cli, dmn string
	}{
		{"stdout", string(rec.Stdout), string(out.Stdout)},
		{"stderr", string(rec.Stderr), string(out.Stderr)},
		{"error", rec.Err, out.Err},
	} {
		if normalizeShadow(f.cli, out.roots) != normalizeShadow(f.dmn, out.roots) {
			fields = append(fields, f.name)
		}
	}
	if !sameDecision(rec.Decision, out.Decision) {
		fields = append(fields, "notifier.decision")
	}
	if !slices.Equal(rec.Effects, out.Effects) {
		fields = append(fields, "effects")
	}
	fields = append(fields, stateDiff(rec.After, out.After)...)
	if len(fields) > 0 {
		return shadowMismatch, strings.Join(fields, ", ")
	}
	return shadowMatch, ""
}

func sameDecision(a, b *notifier.Decision) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// stateDiff — файлы состояния, разные после CLI и после демона
func stateDiff(cli, dmn []shadowFile) []string {
	byPath := map[shadowPath]shadowFile{}
	for _, f := range dmn {
		byPath[f.shadowPath] = f
	}
	var fields []string
	for _, f := range cli {
		d, ok := byPath[f.shadowPath]
		if !ok || d.Exists != f.Exists || !bytes.Equal(d.Data, f.Data) || d.Err != f.Err {
			fields = append(fields, "state "+f.Root+":"+f.Rel)
		}
	}
	return fields
}

// shadowExecGrace — срок теневого исполнения в демоне сверх таймаута
// подкоманды: обработчик, не вернувшийся к нему, — «не сравнимо»
const shadowExecGrace = time.Second

// executeShadow исполняет запись CLI без эффектов: состояние — во временном
// каталоге из снимка «до», /proc и ответы зависимостей — из записи, время —
// время запроса, лог — в никуда, уведомления выключены. Ошибка — исполнение
// не состоялось (не сравнимо)
func executeShadow(wire hookwire.Request, rec shadowRecord, logger core.Logger) (shadowOutcome, error) {
	hook, covered := shadowHooks[wire.Subcommand]
	if !covered {
		return shadowOutcome{}, fmt.Errorf("подкоманда вне тени")
	}
	if !filepath.IsAbs(wire.Cwd) {
		return shadowOutcome{}, fmt.Errorf("cwd не абсолютный")
	}
	// Пути состояния демон вычисляет сам по подкоманде и сессии; из записи
	// берётся только содержимое этих путей
	var paths []shadowPath
	if hook.state != nil {
		var stateErr error
		paths, stateErr = hook.state(rec.Session)
		if stateErr != nil {
			return shadowOutcome{}, fmt.Errorf("shadow state paths: %w", stateErr)
		}
	}
	before, err := shadowSnapshot(paths, rec.Before)
	if err != nil {
		return shadowOutcome{}, fmt.Errorf("снимок «до»: %w", err)
	}
	if _, err := shadowSnapshot(paths, rec.After); err != nil {
		return shadowOutcome{}, fmt.Errorf("снимок «после»: %w", err)
	}
	tmp, err := os.MkdirTemp("", "chooks-shadow-")
	if err != nil {
		return shadowOutcome{}, fmt.Errorf("каталог тени: %w", err)
	}
	stdout, stderr := &outputBuffer{}, &outputBuffer{}
	plain := hookctx.New(bytes.NewReader(wire.Stdin), stdout, stderr, withoutEnv(wire.Env, hookproxy.EnvFallback), wire.Cwd, wire.PID, wire.PPID)
	plain.ConfigPath, plain.Verbose, plain.Timeout, plain.Time = rec.ConfigPath, rec.Verbose, rec.Timeout, rec.Time
	replay := procfs.NewReplay(rec.Proc)
	plain.Proc = replay
	request := plain.WithStateRoots(filepath.Join(tmp, "run"), filepath.Join(tmp, "state"))
	roots, err := shadowRoots(plain, request)
	if err != nil {
		return shadowOutcome{}, errors.Join(err, os.RemoveAll(tmp))
	}
	out := shadowOutcome{roots: roots}
	if err := writeShadowFiles(request, before); err != nil {
		return shadowOutcome{}, errors.Join(fmt.Errorf("снимок состояния: %w", err), os.RemoveAll(tmp))
	}
	capture := &shadowCapture{replay: &nightAnswers{}, dry: true, paths: paths}
	if rec.Night != nil {
		capture.replay = rec.Night
	}
	result := make(chan struct{})
	go func() {
		defer close(result)
		defer func() {
			if r := recover(); r != nil {
				out.Code, out.Err = exitBlocked, fmt.Sprintf("panic: %v\n%s", r, debug.Stack())
			}
		}()
		out.Code, out.Err = shadowDispatch(request, wire.Subcommand, wire.Stdin, capture)
	}()
	timeout := hookTimeouts[wire.Subcommand] + shadowExecGrace
	select {
	case <-result:
	case <-time.After(timeout):
		// Обработчик не вернулся: каталог убирает он сам по возврату
		go func() {
			<-result
			if err := os.RemoveAll(tmp); err != nil {
				logger.Warn("shadow temp directory cleanup failed", "error", err)
			}
		}()
		return shadowOutcome{}, fmt.Errorf("исполнение дольше %s", timeout)
	}
	defer os.RemoveAll(tmp)
	stdout.seal()
	stderr.seal()
	out.Stdout, out.Stderr = stdout.Bytes(), stderr.Bytes()
	capture.mu.Lock()
	out.Decision = capture.decision
	out.Effects = capture.effects
	out.Missing = append(capture.missing, replay.Missing()...)
	capture.mu.Unlock()
	out.Verdict = verdictOf(wire.Subcommand, out.Code, out.Stdout, capture)
	out.After = capture.afterFiles(request)
	return out, nil
}

// shadowSnapshot — файлы записи CLI ровно по путям paths, вычисленным
// демоном, в их порядке. Файл записи вне paths, повтор или путь без файла —
// ошибка: запись не от CLI этой подкоманды и сессии
func shadowSnapshot(paths []shadowPath, files []shadowFile) ([]shadowFile, error) {
	byPath := make(map[shadowPath]shadowFile, len(files))
	for _, f := range files {
		if !slices.Contains(paths, f.shadowPath) {
			return nil, fmt.Errorf("путь вне охвата %s:%q", f.Root, f.Rel)
		}
		if _, twice := byPath[f.shadowPath]; twice {
			return nil, fmt.Errorf("путь дважды %s:%q", f.Root, f.Rel)
		}
		byPath[f.shadowPath] = f
	}
	snapshot := make([]shadowFile, 0, len(paths))
	for _, p := range paths {
		f, ok := byPath[p]
		if !ok {
			return nil, fmt.Errorf("нет файла %s:%s", p.Root, p.Rel)
		}
		snapshot = append(snapshot, f)
	}
	return snapshot, nil
}

// shadowDispatch — работа подкоманды в демоне: конфиг запроса без
// уведомлений, лог в никуда, таймер без записи, затем dispatchHook — та же
// функция, что у CLI
func shadowDispatch(request hookctx.Context, hookType string, data []byte, capture *shadowCapture) (int, string) {
	config, err := core.LoadConfigForRequest(request.ConfigPath, request)
	if err != nil {
		return exitError, fmt.Errorf("failed to load config: %w", err).Error()
	}
	// Эффекты уведомления в тени не исполняются: capture.dry, их запись
	// сравнивается с записью CLI
	loggerConfig := core.DefaultLoggerConfig()
	loggerConfig.Level, loggerConfig.Format = config.Logger.Level, config.Logger.Format
	logger, err := core.NewLoggerIn(loggerConfig, io.Discard, request.Location())
	if err != nil {
		return exitError, err.Error()
	}
	ctx := context.WithValue(hookctx.WithRequest(context.Background(), request), shadowCaptureKey{}, capture)
	// Отсчёт — от входа в обработчик: старт процесса демона к вызову
	// отношения не имеет
	timer := requestTimer(hookType, request)
	timer.start = timer.now()
	timer.mark = timer.start
	code, err := dispatchHook(ctx, hookType, data, config, logger, timer, request)
	err = errors.Join(err, logger.Close())
	if err != nil {
		return code, err.Error()
	}
	return code, ""
}

// shadowRoots — пары «каталог демона → каталог CLI» для нормализации путей.
// Каталог состояния CLI не вычислен (нет $HOME и $XDG_STATE_HOME) —
// исполнение не сравнимо: у демона каталог свой, временный, и подкоманда,
// упавшая у CLI на StateDir, у него прошла бы
func shadowRoots(plain, request hookctx.Context) ([][2]string, error) {
	dir, err := request.StateDir()
	if err != nil {
		return nil, fmt.Errorf("каталог состояния тени: %w", err)
	}
	cliDir, err := plain.StateDir()
	if err != nil {
		return nil, fmt.Errorf("каталог состояния CLI: %w", err)
	}
	return [][2]string{{request.RuntimeDir(), plain.RuntimeDir()}, {dir, cliDir}}, nil
}

// writeShadowFiles кладёт снимок «до» в каталоги состояния демона
func writeShadowFiles(request hookctx.Context, files []shadowFile) error {
	for _, f := range files {
		if f.Err != "" {
			return fmt.Errorf("%s:%s не прочитан CLI: %s", f.Root, f.Rel, f.Err)
		}
		if !f.Exists {
			continue
		}
		path, err := f.abs(request)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return fmt.Errorf("каталог снимка: %w", err)
		}
		if err := os.WriteFile(path, f.Data, 0o600); err != nil {
			return fmt.Errorf("файл снимка: %w", err)
		}
	}
	return nil
}

// shadowLine — строка журнала тени: время запроса, подкоманда, сессия,
// исход, подробность
func shadowLine(at time.Time, hookType, session, status, detail string) string {
	return strings.Join([]string{at.Format(shadowTimeLayout), tsvField(hookType), tsvField(session), status, tsvField(detail)}, "\t")
}

// shadowSlots — теневых исполнений в демоне одновременно; сверх — «не
// сравнимо: busy», обычные запросы слоты тени не занимают
const shadowSlots = 4

// handleShadow — теневая запись CLI: разбор, исполнение без эффектов,
// сравнение, строка журнала. uid отправителя проверил handle
func (s *server) handleShadow(frame hookwire.Frame) {
	if s.shadowLog == "" {
		return
	}
	hookType := frame.Request.Subcommand
	var rec shadowRecord
	if err := json.Unmarshal(frame.Record, &rec); err != nil {
		s.writeShadow(time.Now(), hookType, "", shadowIncomparable, "запись CLI не разобрана: "+err.Error())
		return
	}
	at := rec.Time
	if at.IsZero() {
		at = time.Now()
	}
	select {
	case s.shadowSem <- struct{}{}:
	default:
		s.writeShadow(at, hookType, rec.Session, shadowIncomparable, "busy")
		return
	}
	defer func() { <-s.shadowSem }()
	out, err := s.shadowRun(frame.Request, rec)
	if err != nil {
		s.writeShadow(at, hookType, rec.Session, shadowIncomparable, err.Error())
		return
	}
	status, detail := compareShadow(rec, out)
	s.writeShadow(at, hookType, rec.Session, status, detail)
}

func (s *server) writeShadow(at time.Time, hookType, session, status, detail string) {
	if s.shadowLog == "" {
		return
	}
	if err := core.AppendLine(s.shadowLog, s.shadowMaxMB, shadowLine(at.In(time.Local), hookType, session, status, detail)); err != nil {
		s.logger.Warn("daemon: shadow line not written", "error", err)
	}
}

// shadowCovered — подкоманды тени по имени, для сводки
func shadowCovered() []string {
	return slices.Sorted(maps.Keys(shadowHooks))
}
