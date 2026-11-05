package understandcheck

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/aiseeq/akit/shellcmd"
	"github.com/aiseeq/akit/transcript"
)

// Kind — какое действие уносит слово пользователя дальше
type Kind string

// Виды действий, на которых стоит проверка (исследование лида 02.11, отчёт
// ive/report.md §7: все пойманные ошибки — там, где слово уходит другим или
// в правило)
const (
	KindSend  Kind = "send"  // SendMessage: письмо другой сессии или агенту
	KindAgent Kind = "agent" // Agent: бриф суб-агенту
	KindRule  Kind = "rule"  // Edit, Write, MultiEdit или правка из Bash файла инструкций
	// KindReply — ответ пользователю с пересказом и планом: хук его не видит,
	// вид нужен разбору исторических случаев, где ошибка была в ответе
	KindReply Kind = "reply"
)

// Title — название вида для судьи
func (k Kind) Title() string {
	switch k {
	case KindSend:
		return "письмо другой сессии (SendMessage)"
	case KindAgent:
		return "задание суб-агенту (Agent)"
	case KindRule:
		return "правка файла инструкций агентов"
	case KindReply:
		return "ответ пользователю: как агент понял реплику и что делает"
	}
	return string(k)
}

// Case — действие агента в ходе после реплики пользователя и то, что было
// после: следующая реплика пользователя нужна разметке сухого прогона
type Case struct {
	ID      string    `json:"id"`
	Session string    `json:"session"`
	Name    string    `json:"name,omitempty"`
	Time    time.Time `json:"ts"`
	Kind    Kind      `json:"kind"`
	Tool    string    `json:"tool"`
	// File — файл инструкций у KindRule
	File string `json:"file,omitempty"`
	// Human — реплика пользователя, начавшая ход; HumanAt — её время
	Human   string    `json:"human"`
	HumanAt time.Time `json:"human_at"`
	// Prev — последний текст агента до реплики: на него отвечал пользователь
	Prev   string `json:"prev"`
	Action string `json:"action"`
	// Next — следующая реплика пользователя в сессии после действия, NextAt — её время
	Next   string    `json:"next,omitempty"`
	NextAt time.Time `json:"next_at,omitzero"`
}

// instructionFile — путь к файлу инструкций агентов: CLAUDE.md, AGENTS.md,
// скиллы, правила, память проектов, журнал решений, промпты Сна, настройки и
// хуки Claude Code
var instructionFile = regexp.MustCompile(`(?:^|/)(?:CLAUDE\.md|AGENTS\.md|SKILL\.md)$|/\.claude/(?:skills|rules|agents|hooks|improve/prompts)/|/\.claude/projects/[^/]+/memory/|/\.claude/(?:decisions\.md|settings(?:\.local)?\.json)$`)

// IsInstructionFile — путь ведёт к файлу инструкций агентов
func IsInstructionFile(path string) bool {
	return instructionFile.MatchString(filepath.Clean(path))
}

// bashPath — путь к файлу инструкций в команде Bash
var bashPath = regexp.MustCompile(`[~\w./-]*(?:CLAUDE\.md|AGENTS\.md|SKILL\.md|\.claude/(?:skills|rules|agents|hooks|improve/prompts)/[\w./-]+|\.claude/projects/[^/\s]+/memory/[\w./-]+|\.claude/decisions\.md|\.claude/settings(?:\.local)?\.json)`)

// inPlaceFlag — флаг правки на месте у sed и perl: -i, -i.bak, -Ei, -pi, -0pi, --in-place
var inPlaceFlag = regexp.MustCompile(`^-\w*i|^--in-place`)

// isEditor — программа с аргументами правит файлы, названные в команде: sub из
// akit, tee, sed -i, perl -i, они же под find -exec (его shellcmd не снимает)
func isEditor(name string, args []string) bool {
	switch name {
	case "sub", "tee":
		return true
	case "sed", "perl":
		return slices.ContainsFunc(args, inPlaceFlag.MatchString)
	case "find":
		for i, arg := range args[:max(len(args)-1, 0)] {
			switch arg {
			case "-exec", "-execdir", "-ok", "-okdir":
				if isEditor(filepath.Base(args[i+1]), args[i+2:]) {
					return true
				}
			}
		}
	}
	return false
}

// heredocEditor — редактор в теле heredoc. Тело — часто скрипт, который команда
// тут же запускает, и не всегда на оболочке: разбор shellcmd его не видит, ищется
// текстом. Лишний вызов судьи дешевле пропущенной правки (сухой прогон 05.11 за
// 7 дней: без этого из 3 ушедших случаев 2 — настоящие правки файлов скиллов
// скриптом perl)
var heredocEditor = regexp.MustCompile(`(?:^|[\s;&|(])(?:sub|tee)(?:\s|$)|\bsed\s+(?:-\w+\s+)*-i|\bperl\s+(?:-\w+\s+)*-\w*i`)

// callEdits — вызов правит файлы сам или телом heredoc
func callEdits(call shellcmd.Call) bool {
	return isEditor(call.Name, call.Args) || slices.ContainsFunc(call.Redirs, func(redir shellcmd.Redir) bool {
		return heredocEditor.MatchString(redir.Body)
	})
}

// bashRedirect — перенаправление вывода в файл инструкций. Регулярка, не
// shellcmd.Call.Redirs: те знают только простые команды, а `{ …; } >> файл`,
// `( … ) > файл`, `done > файл` и голый `> файл` — тоже правка
var bashRedirect = regexp.MustCompile(`>{1,2}\s*["']?(` + bashPath.String() + `)`)

// bashRuleFile — файл инструкций, который меняет команда Bash; пусто — команда
// такой файл не меняет. Редактор ищется разбором оболочки: его имя в кавычках и
// аргументах правкой не считается
func bashRuleFile(command string) string {
	if m := bashRedirect.FindStringSubmatch(command); m != nil {
		return m[1]
	}
	calls, err := shellcmd.Calls(command)
	var nested *shellcmd.NestedError
	if err != nil && !errors.As(err, &nested) {
		return "" // shellcmd не разбирает только то, что отвергает и bash -n: такую команду оболочка не выполнит
	}
	if slices.ContainsFunc(calls, callEdits) {
		// последний путь — файл правки: первые бывают каталогом в cd
		if all := bashPath.FindAllString(command, -1); len(all) > 0 {
			return all[len(all)-1]
		}
	}
	return ""
}

// Collector собирает случаи из событий одного транскрипта по порядку
type Collector struct {
	Session, Name string
	cases         []Case
	// open — случаи хода, ждущие следующей реплики пользователя
	open []int
	// humans — реплики, на которые агент ещё не ответил концом хода:
	// пользователь пишет несколько подряд, пока агент работает, и действие
	// отвечает не обязательно на последнюю (случай 8 лида: «снижать Luna» в
	// 16:07, письма в 16:13 после двух реплик о строке статуса)
	humans   []string
	humanAt  time.Time
	answered bool
	prev     string
	inTurn   bool
	// lastFinal — последний текст агента, которым кончился ход (end_turn):
	// его видел пользователь, когда писал; lastText — любой последний текст
	lastFinal, lastText string
	// anchored — неотвеченные реплики идут после завершённого ответа агента,
	// который виден в событиях: prev и начало реплик известны. Нет — события
	// начались посреди реплик (окно хвоста транскрипта)
	anchored bool
	// peerLast — последний ход начат письмом другой сессии
	peerLast bool
}

// maxHumans — сколько неотвеченных реплик подряд отдавать судье
const maxHumans = 4

// Add обрабатывает следующее событие транскрипта
func (c *Collector) Add(e transcript.Event) {
	if e.Sidechain {
		return
	}
	switch e.Role {
	case transcript.User, transcript.Ask:
		text := strings.TrimSpace(e.Shown())
		if text == "" {
			return
		}
		for _, i := range c.open {
			c.cases[i].Next = text
			c.cases[i].NextAt = e.Time
		}
		c.open = nil
		if c.answered || len(c.humans) == 0 {
			c.anchored = c.answered
			c.humans = nil
			c.prev = c.lastFinal
			if c.prev == "" {
				c.prev = c.lastText
			}
			c.answered = false
		}
		c.humans = append(c.humans, text)
		if len(c.humans) > maxHumans {
			c.humans = c.humans[len(c.humans)-maxHumans:]
		}
		c.humanAt = e.Time
		c.inTurn = true
		c.peerLast = false
	case transcript.Peer:
		// письмо другой сессии начинает ход не по слову пользователя; письмо
		// посреди хода, пока реплики не отвечены, ход не меняет: агент ещё
		// исполняет их (противник Terra 02.11, сценарий 1)
		if c.answered || len(c.humans) == 0 {
			c.inTurn = false
			c.peerLast = true
		}
	case transcript.Agent:
		t := strings.TrimSpace(e.Text)
		if t == "" {
			return
		}
		c.lastText = t
		if e.Reply != nil && e.Reply.StopReason == "end_turn" {
			c.lastFinal = t
			c.answered = true
		}
	case transcript.Cmd:
		if !c.inTurn {
			return
		}
		kind, file, action, ok := classify(e.Who, e.Raw)
		if !ok {
			return
		}
		c.cases = append(c.cases, Case{
			ID:      fmt.Sprintf("%s:%d", c.Session, e.Line),
			Session: c.Session,
			Name:    c.Name,
			Time:    e.Time,
			Kind:    kind,
			Tool:    e.Who,
			File:    file,
			Human:   c.human(),
			HumanAt: c.humanAt,
			Prev:    c.prev,
			Action:  action,
		})
		c.open = append(c.open, len(c.cases)-1)
	}
}

// Cases — собранные случаи
func (c *Collector) Cases() []Case {
	return c.cases
}

// Turn — ход к моменту после последнего события: что отдать судье
type Turn struct {
	// InTurn — ход начат репликой пользователя, не письмом другой сессии
	InTurn  bool
	Human   string
	HumanAt time.Time
	Prev    string
	// Anchored — реплики идут после завершённого ответа агента в событиях
	Anchored bool
	// Decided — событий хватает: ход начат письмом другой сессии или реплики
	// пользователя идут после завершённого ответа агента
	Decided bool
}

// Turn — текущий ход по событиям, поданным в Add
func (c *Collector) Turn() Turn {
	return Turn{
		InTurn:   c.inTurn,
		Human:    c.human(),
		HumanAt:  c.humanAt,
		Prev:     c.prev,
		Anchored: c.anchored,
		Decided:  c.peerLast || (c.inTurn && c.anchored),
	}
}

// classify решает, уносит ли вызов инструмента слово пользователя дальше, и
// собирает текст действия для судьи
func classify(tool string, raw json.RawMessage) (Kind, string, string, bool) {
	var in map[string]any
	if json.Unmarshal(raw, &in) != nil {
		return "", "", "", false
	}
	str := func(k string) string { s, _ := in[k].(string); return s }
	switch tool {
	case "SendMessage":
		message := str("message")
		if message == "" {
			// чистая подписка notify_when_idle: слов не несёт
			return "", "", "", false
		}
		return KindSend, "", "Кому: " + str("to") + "\n" + message, true
	case "Agent", "Task":
		header := "Тип: " + str("subagent_type") + "\nОписание: " + str("description") + "\n"
		return KindAgent, "", header + str("prompt"), true
	case "Edit":
		file := str("file_path")
		if !IsInstructionFile(file) {
			return "", "", "", false
		}
		return KindRule, file, "Файл: " + file + "\nБыло:\n" + str("old_string") + "\nСтало:\n" + str("new_string"), true
	case "MultiEdit":
		file := str("file_path")
		if !IsInstructionFile(file) {
			return "", "", "", false
		}
		var b strings.Builder
		b.WriteString("Файл: " + file)
		edits, _ := in["edits"].([]any)
		for _, item := range edits {
			edit, _ := item.(map[string]any)
			old, _ := edit["old_string"].(string)
			repl, _ := edit["new_string"].(string)
			b.WriteString("\nБыло:\n" + old + "\nСтало:\n" + repl)
		}
		return KindRule, file, b.String(), true
	case "Write":
		file := str("file_path")
		if !IsInstructionFile(file) {
			return "", "", "", false
		}
		return KindRule, file, "Файл: " + file + " (записан целиком)\n" + str("content"), true
	case "Bash":
		command := str("command")
		file := bashRuleFile(command)
		if file == "" {
			return "", "", "", false
		}
		return KindRule, file, "Команда:\n" + command, true
	}
	return "", "", "", false
}

// human — неотвеченные реплики пользователя: одна — как есть, несколько —
// по порядку с номерами
func (c *Collector) human() string {
	if len(c.humans) == 1 {
		return c.humans[0]
	}
	var b strings.Builder
	for i, h := range c.humans {
		if i > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "[реплика %d из %d]\n%s", i+1, len(c.humans), h)
	}
	return b.String()
}
