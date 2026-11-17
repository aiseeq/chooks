package tools

import (
	"fmt"
	"path/filepath"
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
const cgroupHint = "Cgroup сессии, рабочего стола и их срезов не трогай: лишний процесс убей литералом (kill 12345), лимит ставь юниту без процессов сессии (systemctl --user set-property юнит MemoryMax=…)"

// cgroupWrites файлы, в которые пишет вызов: цели перенаправлений, файлы
// tee, последний аргумент cp, of= у dd. Слова — как в исходнике: цель с $
// или ` не литерал
func cgroupWrites(c shellcmd.Call) []string {
	var out []string
	for _, r := range c.Redirs {
		if slices.Contains(cgroupWriteOps, r.Op) {
			out = append(out, r.Target)
		}
	}
	operands := slices.DeleteFunc(slices.Clone(c.Args), func(a string) bool { return strings.HasPrefix(a, "-") })
	switch c.Name {
	case "tee":
		out = append(out, operands...)
	case "cp":
		if len(operands) > 1 {
			out = append(out, operands[len(operands)-1])
		}
	case "dd":
		for _, a := range c.Args {
			if f, ok := strings.CutPrefix(a, "of="); ok {
				out = append(out, f)
			}
		}
	}
	return out
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
	for _, file := range cgroupWrites(c) {
		effect := cgroupControls[filepath.Base(file)]
		dir, under := strings.CutPrefix(filepath.Dir(filepath.Clean(file)), cgroupRoot)
		if effect == "" || !under || strings.ContainsAny(file, "$`") || dir != "" && dir[0] != '/' {
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
