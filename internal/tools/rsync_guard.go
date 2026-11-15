package tools

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/aiseeq/akit/shellcmd"
	"github.com/aiseeq/chooks/internal/core"
)

// RsyncGuardTool отклоняет rsync с удалением, если назначение раскрывается при запуске.
type RsyncGuardTool struct{ *BaseTool }

// NewRsyncGuardTool создаёт проверку rsync для опасного назначения из переменной.
func NewRsyncGuardTool(config core.ToolConfig, logger core.Logger) (*RsyncGuardTool, error) {
	return &RsyncGuardTool{BaseTool: NewBaseTool("rsync_guard", config.Enabled, core.ShellTools, logger)}, nil
}

var (
	rsyncExpansion        = regexp.MustCompile(`\$\{[^}]*\}|\$\([^)]*\)|\$[A-Za-z_][A-Za-z0-9_]*|\$[0-9@*#?$!_-]|` + "`[^`]*`")
	rsyncRequiredVariable = regexp.MustCompile(`^\$\{[A-Za-z_][A-Za-z0-9_]*:?\?[^}]*\}$`)
)

var rsyncValueOptions = map[string]bool{
	"-e": true, "--rsh": true, "-f": true, "--filter": true,
	"--exclude": true, "--include": true, "--exclude-from": true, "--include-from": true,
	"--files-from": true, "--rsync-path": true, "-T": true, "--temp-dir": true,
	"--partial-dir": true, "--backup-dir": true, "--suffix": true, "--compare-dest": true,
	"--copy-dest": true, "--link-dest": true, "--chmod": true, "--chown": true,
	"--usermap": true, "--groupmap": true, "--timeout": true, "--contimeout": true,
	"--port": true, "--password-file": true, "--bwlimit": true, "--max-size": true,
	"--min-size": true, "--max-delete": true, "-B": true, "--block-size": true,
	"--modify-window": true, "--out-format": true, "--log-file": true,
	"--log-file-format": true, "--iconv": true, "-M": true, "--remote-option": true,
	"--info": true, "--debug": true, "--skip-compress": true, "--compress-choice": true,
	"--checksum-choice": true, "--address": true, "--sockopts": true,
	"--checksum-seed": true, "--compress-level": true, "--compress-threads": true,
	"--confine-root": true, "--copy-as": true, "--early-input": true,
	"--max-alloc": true, "--nice": true, "--only-write-batch": true,
	"--outbuf": true, "--protocol": true, "--read-batch": true,
	"--stderr": true, "--stop-after": true, "--stop-at": true,
	"--time-limit": true, "--write-batch": true,
	"--zl": true, "--zc": true, "--zt": true, "--cc": true, "-@": true,
}

func (t *RsyncGuardTool) ValidateTool(_ context.Context, input *core.ToolInput) (*core.ValidationResult, error) {
	valid := &core.ValidationResult{IsValid: true}
	if !t.IsEnabled() || input == nil || !core.IsShellTool(input.ToolName) || !strings.Contains(input.Command, "rsync") {
		return valid, nil
	}
	calls, err := shellcmd.Calls(input.Command)
	if err != nil {
		t.Logger().Warn("rsync_guard: shellcmd не разобрал команду, пропуск", "error", err)
		return valid, nil
	}
	var violations []core.Violation
	var suggestions []string
	for _, call := range calls {
		if call.Name != "rsync" {
			continue
		}
		operands, deleting, dryRun := rsyncArgs(call.Args)
		if !deleting || dryRun || len(operands) < 2 {
			continue
		}
		dst := operands[len(operands)-1]
		if !hasUnguardedExpansion(dst) {
			continue
		}
		option := "--delete"
		for _, arg := range call.Args {
			if arg == "--del" || strings.HasPrefix(arg, "--delete") {
				option = arg
				break
			}
		}
		reason := fmt.Sprintf("rsync %s: назначение %q раскрывается во время запуска; укажи литеральный абсолютный путь или перепиши как \"${R:?}\"/", option, dst)
		violations = append(violations, core.NewViolation("rsync_guard", reason, "", core.LevelCritical, 0, 0))
		suggestions = append(suggestions, reason)
	}
	if len(violations) == 0 {
		return valid, nil
	}
	return core.NewValidationResult(false, violations, suggestions), nil
}

// rsyncArgs собирает позиционные аргументы с учётом опций, значение которых
// передаётся отдельным словом. Опции действуют и после операндов, до --.
func rsyncArgs(args []string) (operands []string, deleting, dryRun bool) {
	endOptions := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !endOptions && a == "--" {
			endOptions = true
			continue
		}
		if !endOptions && strings.HasPrefix(a, "--") {
			name, _, hasValue := strings.Cut(a, "=")
			if isRsyncDeleteOption(name) {
				deleting = true
			}
			if name == "--dry-run" {
				dryRun = true
			}
			if rsyncValueOptions[name] && !hasValue && i+1 < len(args) {
				i++
			}
			continue
		}
		if !endOptions && isRsyncShortOption(a) {
			shortDryRun, takesValue := rsyncShortFlags(a)
			dryRun = dryRun || shortDryRun
			if takesValue && shortValueIsSeparate(a) && i+1 < len(args) {
				i++
			}
			continue
		}
		operands = append(operands, a)
	}
	return operands, deleting, dryRun
}

func isRsyncDeleteOption(name string) bool {
	return name == "--delete" || name == "--del" || strings.HasPrefix(name, "--delete-")
}

func isRsyncShortOption(arg string) bool {
	return len(arg) > 1 && arg[0] == '-'
}

func rsyncShortFlags(arg string) (dryRun, takesValue bool) {
	for pos := 1; pos < len(arg); pos++ {
		short := "-" + string(arg[pos])
		if short == "-n" {
			dryRun = true
		}
		if rsyncValueOptions[short] {
			return dryRun, true
		}
	}
	return dryRun, false
}

func shortValueIsSeparate(arg string) bool {
	for pos := 1; pos < len(arg); pos++ {
		if rsyncValueOptions["-"+string(arg[pos])] {
			return pos == len(arg)-1
		}
	}
	return false
}

func hasUnguardedExpansion(word string) bool {
	for _, match := range rsyncExpansion.FindAllStringIndex(word, -1) {
		expansion := word[match[0]:match[1]]
		if rsyncRequiredVariable.MatchString(expansion) {
			continue
		}
		return true
	}
	return false
}
