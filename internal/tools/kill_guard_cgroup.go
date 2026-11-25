package tools

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/aiseeq/akit/shellcmd"
)

// cgroupRoot корень cgroupfs
const cgroupRoot = "/sys/fs/cgroup"

// cgroupControls файлы cgroup, запись в которые гасит или душит все
// процессы cgroup и вложенных: что делает запись
var cgroupControls = map[string]string{
	"cgroup.kill":   "убивает все процессы cgroup и вложенных",
	"cgroup.freeze": "замораживает все процессы cgroup и вложенных",
	"memory.max":    "ставит лимит памяти cgroup и вложенных: при нехватке ядро убивает их процессы",
	"memory.high":   "ставит порог памяти cgroup и вложенных: выше него процессы душит возврат памяти",
	"pids.max":      "ставит лимит числа процессов cgroup и вложенных: новые процессы не запустятся",
	"cpu.max":       "ставит лимит CPU cgroup и вложенных: процессы встанут",
}

// cgroupWriteOps операторы перенаправления, пишущие в файл
var cgroupWriteOps = []string{">", ">>", ">|", "&>", "&>>"}

// cgroupHint подсказка к отказу на запись в cgroup сессии
const cgroupHint = "Cgroup и срезы сессии и рабочего стола не трогай. Свой PID: pstop PID; осиротевший свой из прошлого вызова — попробуй pstop --own-unit PID. Отказал — сигнал не шли. Лимит юниту без процессов сессии: systemctl --user set-property юнит MemoryMax=…"

// cgroupWriteTarget файл, в который пишет вызов, и записываемое значение:
// "" — не видно до запуска (переменная, ввод tee, файл cp и dd)
type cgroupWriteTarget struct {
	file, value string
}

// cgroupWrites файлы, в которые пишет вызов: цели перенаправлений, файлы
// tee, последний аргумент cp, of= у dd. Слова — как в исходнике: цель с $
// или ` не литерал. Значение видно у перенаправления вывода echo и printf
// с литеральными аргументами
func cgroupWrites(c shellcmd.Call) []cgroupWriteTarget {
	var out []cgroupWriteTarget
	for _, r := range c.Redirs {
		if !slices.Contains(cgroupWriteOps, r.Op) {
			continue
		}
		w := cgroupWriteTarget{file: r.Target}
		if r.N == "" || r.N == "1" {
			w.value = echoedValue(c)
		}
		out = append(out, w)
	}
	operands := slices.DeleteFunc(slices.Clone(c.Args), func(a string) bool { return strings.HasPrefix(a, "-") })
	switch c.Name {
	case "tee":
		for _, f := range operands {
			out = append(out, cgroupWriteTarget{file: f})
		}
	case "cp":
		if len(operands) > 1 {
			out = append(out, cgroupWriteTarget{file: operands[len(operands)-1]})
		}
	case "dd":
		for _, a := range c.Args {
			if f, ok := strings.CutPrefix(a, "of="); ok {
				out = append(out, cgroupWriteTarget{file: f})
			}
		}
	}
	return out
}

// echoedValue что печатает echo или printf с литеральными аргументами, без
// опций и перевода строки; "" — не видно
func echoedValue(c shellcmd.Call) string {
	if c.Name != "echo" && c.Name != "printf" {
		return ""
	}
	for _, a := range c.Args {
		if strings.ContainsAny(a, "$`") || c.Name == "printf" && strings.Contains(a, "%") {
			return ""
		}
	}
	args := c.Args
	for len(args) > 0 && c.Name == "echo" && (args[0] == "-n" || args[0] == "-e" || args[0] == "-E") {
		args = args[1:]
	}
	return strings.TrimSpace(strings.TrimSuffix(strings.Join(args, " "), `\n`))
}

// cgroupReleaseRe значения, которые снимают ограничение файла cgroup: max у
// лимитов (у cpu.max — и с периодом), 0 у cgroup.freeze. У cgroup.kill
// такого нет
var cgroupReleaseRe = map[string]*regexp.Regexp{
	"memory.max":    regexp.MustCompile(`^max$`),
	"memory.high":   regexp.MustCompile(`^max$`),
	"pids.max":      regexp.MustCompile(`^max$`),
	"cpu.max":       regexp.MustCompile(`^max( [0-9]+)?$`),
	"cgroup.freeze": regexp.MustCompile(`^0$`),
}

// cgroupWrite отказ на запись в файл управления cgroup (cgroup.kill,
// memory.max…) литералом под /sys/fs/cgroup, если в этой cgroup или
// вложенной идёт процесс сессии: предок этой сессии Claude или рабочий
// стол (тот же поиск, что у systemctl stop). Пишет в cgroupfs своего
// менеджера и сам пользователь, без sudo
func (s *killScan) cgroupWrite(c shellcmd.Call) error {
	if slices.Contains(c.Via, "ssh") {
		return nil // запишет на другой машине
	}
	for _, w := range cgroupWrites(c) {
		file, base := w.file, filepath.Base(w.file)
		effect := cgroupControls[base]
		dir, under := strings.CutPrefix(filepath.Dir(filepath.Clean(file)), cgroupRoot)
		release := cgroupReleaseRe[base] != nil && cgroupReleaseRe[base].MatchString(w.value)
		if effect == "" || release || !under || strings.ContainsAny(file, "$`") || dir != "" && dir[0] != '/' {
			continue
		}
		if err := s.loadProcs(); err != nil {
			return err
		}
		host := ""
		s.tool.eachSessionCgroup(s.procs, "", func(p procInfo, why, path string) {
			if host == "" && path != "" && (dir == "" || path == dir || strings.HasPrefix(path, dir+"/")) {
				host = fmt.Sprintf("в ней идёт %s (PID %d) — %s", p.comm, p.pid, why)
			}
		})
		if host != "" {
			s.addToolReason(fmt.Sprintf("запись в %s %s: %s", file, effect, host), cgroupHint, false)
		}
	}
	return nil
}
