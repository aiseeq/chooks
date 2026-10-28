package tools

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

const rmWorkspaceHeader = "rm рабочей папки Bash: Claude Code спросит пользователя и будет ждать ответа. Перепиши команду:"

// globTail хвост /* или /** цели, который Claude Code снимает перед сверкой
var globTail = regexp.MustCompile(`(/\*+)+/*$`)

// workspaceReasons третья форма: цель rm или rmdir — рабочая папка Bash или
// её родитель. Claude Code 2.1.312 останавливает такую команду запросом «This
// command would remove a workspace directory» и в bypassPermissions (сверено
// прогоном claude -p, таблица в тесте). Рабочая папка — cwd Bash на начало
// команды; cd перед rm он учитывает, только если команда начинается с
// cd <абсолютный путь> и дальше идут одни &&. Цель с /* на конце он сверяет
// ещё и с дополнительными рабочими папками (--add-dir) — хуку их не видно
func (t *RmGuardTool) workspaceReasons(s *shScript, cwd string) ([]string, error) {
	if cwd == "" {
		return nil, nil
	}
	cwd = leadingCd(s, cwd)
	dirs := []string{path.Clean(cwd)}
	real, err := realPath(dirs[0])
	if err != nil {
		return nil, err
	}
	if real != dirs[0] {
		dirs = append(dirs, real)
	}
	var reasons []string
	for _, c := range s.cmds {
		if c.nested {
			continue
		}
		words := commandWords(c.words)
		if len(words) == 0 {
			continue
		}
		if name := commandName(words[0].raw); name != "rm" && name != "rmdir" {
			continue
		}
		options := true
		for _, w := range words[1:] {
			if w.redirect {
				continue
			}
			if options && w.raw == "--" {
				options = false
				continue
			}
			if options && strings.HasPrefix(w.raw, "-") {
				continue
			}
			target, hit, err := t.workspaceTarget(w, cwd, dirs)
			if err != nil {
				return nil, err
			}
			if hit {
				reasons = append(reasons, t.workspaceHint(words, target, dirs[0]))
			}
		}
	}
	return reasons, nil
}

// leadingCd рабочая папка, от которой Claude Code сверяет цели: каталог
// ведущего cd <абсолютный путь>, если всё остальное соединено &&, иначе cwd
func leadingCd(s *shScript, cwd string) string {
	if s.group || s.function {
		return cwd
	}
	var top []shCommand
	for _, c := range s.cmds {
		if !c.nested {
			top = append(top, c)
		}
	}
	if len(top) < 2 || len(top[0].words) != 2 || commandName(top[0].words[0].raw) != "cd" {
		return cwd
	}
	for _, c := range top[1:] {
		if c.sep != "&&" {
			return cwd
		}
	}
	dir := top[0].words[1]
	if strings.ContainsAny(dir.text, "$`~") || !strings.HasPrefix(unquoteWord(dir.text), "/") {
		return cwd
	}
	return unquoteWord(dir.text)
}

// workspaceTarget абсолютный путь цели и попала ли она в рабочую папку или
// её родителя. Корень, каталог верхнего уровня и домашний Claude Code
// останавливает как критические — их не трогаем
func (t *RmGuardTool) workspaceTarget(w shWord, cwd string, dirs []string) (string, bool, error) {
	target, ok := t.literalTarget(w, cwd)
	if !ok {
		return "", false, nil
	}
	bare := globTail.ReplaceAllString(target, "")
	if bare == "" {
		bare = "/"
	}
	if strings.ContainsAny(bare, "*?[") {
		return "", false, nil
	}
	candidates := []string{bare}
	if bare != target {
		real, err := realPath(bare)
		if err != nil {
			return "", false, err
		}
		if real != bare {
			candidates = append(candidates, real)
		}
	}
	for _, p := range candidates {
		if p == "/" || path.Dir(p) == "/" || t.homeOrAbove(p) {
			return "", false, nil
		}
		for _, d := range dirs {
			if d == p || strings.HasPrefix(d, p+"/") {
				return target, true, nil
			}
		}
	}
	return "", false, nil
}

// literalTarget цель как абсолютный путь от cwd с раскрытой ~; цели с
// переменными и подстановками разбирают первые две формы
func (t *RmGuardTool) literalTarget(w shWord, cwd string) (string, bool) {
	if strings.ContainsAny(w.text, "$`") || strings.ContainsRune(w.raw, substMark) || braceExpansion.MatchString(w.text) {
		return "", false
	}
	target := unquoteWord(w.text)
	if target == "~" || strings.HasPrefix(target, "~/") {
		home, ok := t.lookupEnv("HOME")
		if !ok || home == "" {
			return "", false
		}
		target = home + target[1:]
	}
	if target == "" || strings.HasPrefix(target, "~") {
		return "", false
	}
	if !path.IsAbs(target) {
		target = path.Join(cwd, target)
	}
	return path.Clean(target), true
}

// realPath путь без символических ссылок; несуществующий путь Claude Code
// сверяет как есть, и мы так же
func realPath(p string) (string, error) {
	real, err := filepath.EvalSymlinks(p)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return p, nil
	}
	if err != nil {
		return "", fmt.Errorf("realpath %s: %w", p, err)
	}
	return real, nil
}

// homeOrAbove путь — домашний каталог или его предок
func (t *RmGuardTool) homeOrAbove(p string) bool {
	home, ok := t.lookupEnv("HOME")
	if !ok || home == "" {
		return false
	}
	home = path.Clean(home)
	return p == home || strings.HasPrefix(home, p+"/")
}

// workspaceHint что сделать: уйти из удаляемой папки отдельным вызовом cd и
// удалить следующим вызовом (rm — отдельным вызовом, ~/.claude/CLAUDE.md)
func (t *RmGuardTool) workspaceHint(words []shWord, target, cwd string) string {
	away := "<папка вне удаляемой>"
	if project, ok := t.lookupEnv("CLAUDE_PROJECT_DIR"); ok && project != "" &&
		project != target && !strings.HasPrefix(path.Clean(project)+"/", target+"/") {
		away = path.Clean(project)
	}
	rm := strings.Trim(rmInvocation(words, -1, ""), "`")
	return fmt.Sprintf("`%s` удаляет %s — рабочую папку Bash %s или её родителя. Сначала отдельным вызовом `cd %s`, потом отдельным вызовом `%s`",
		rm, target, cwd, away, rm)
}
