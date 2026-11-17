package tools

import (
	"regexp"
	"strings"
)

// parallelOptionSpecs опции GNU parallel 20260922 (options_hash в
// /usr/bin/parallel, пакет parallel Fedora 44) в записи Getopt::Long: имена
// через |, после имён тип значения: =s, =i, =f — обязательное, :s, :f —
// необязательное, без типа — флаг. parallel разбирает их Getopt::Long с
// bundling и require_order (parse_options)
var parallelOptionSpecs = []string{
	"B=s", "E=s", "H=i", "I=s", "L=s", "U=s", "W=s", "arg-file-sep|argfilesep=s", "arg-file|argfile|a=s",
	"arg-sep|argsep=s", "basefile|bf=s", "basenameextensionreplace|bner=s", "basenamereplace|bnr=s", "bin=s",
	"block-size|blocksize|block=s", "block-timeout|blocktimeout|bt=s", "col-sep|colsep|C=s",
	"combineexec|combine-exec|combineexecutable|combine-executable=s", "ctag-string|ctagstring=s", "debug|D=s",
	"delay=s", "delimiter|d=s", "dirnamereplace|dnr=s", "env=s", "eof|e:s", "extensionreplace|er=s", "filter=s",
	"group-by|groupby=s", "halt-on-error|haltonerror|halt=s", "header=s", "joblog|jl=s", "limit=s",
	"linkinputsource|xapplyinputsource=i", "load=s", "match=s", "max-args|maxargs|n=s", "max-chars|maxchars|s=s",
	"max-lines|maxlines|l:f", "max-procs|maxprocs|P|jobs|j=s", "max-replace-args|maxreplaceargs|N=s",
	"memfree=s", "memsuspend=s", "milestone|ms=s", "min-version|minversion=i", "nice=i", "parens=s",
	"process-slot-var|processslotvar=s", "profile|J=s", "recend=s", "recstart=s", "replace|i:s",
	"results|result|res=s", "retries=s", "return=s", "rpl=s", "rsync-opts|rsyncopts=s",
	"semaphore-name|semaphorename|id=s", "semaphore-timeout|semaphoretimeout|st=s", "seqreplace=s", "shard=s",
	"shell-completion|shellcompletion=s", "slotreplace=s", "sql-and-worker|sqlandworker=s",
	"sql-master|sqlmaster=s", "sql-worker|sqlworker=s", "sql=s", "ssh-delay|sshdelay=f", "ssh=s",
	"sshloginfile|slf=s", "sshlogin|S=s", "tag-string|tagstring=s", "template|tmpl=s", "term-seq|termseq=s",
	"timeout=s", "tmpdir|tempdir=s", "total-jobs|totaljobs|total=s",
	"transfer-file|transferfile|transfer-files|transferfiles|tf=s", "trc=s", "trim=s",
	"use-compress-program|compress-program|usecompressprogram|compressprogram=s",
	"use-decompress-program|decompress-program|usedecompressprogram|decompressprogram=s",
	"work-dir|workdir|wd=s", "_buf_cap=i", "_buf_growth=f", "_buf_start=i", "_parset=s", "_pipe_block=i",

	"T", "X", "Y", "bar", "bg", "bibtex|citation", "cat", "cleanup",
	"color-failed|colour-failed|colorfailed|colourfailed|color-fail|colour-fail|colorfail|colourfail|cf",
	"color|colour", "compress", "controlmaster|M", "csv", "ctag", "ctrl-c|ctrlc", "dry-run|dryrun|dr", "embed",
	"eta", "exit|x", "fast", "fg", "fifo", "filter-hosts|filterhosts|filter-host", "g", "gnu", "group", "help|h",
	"hgrp|hostgrp|hostgroup|hostgroups", "interactive|p", "keep-order|keeporder|k", "latest-line|latestline|ll",
	"line-buffer|line-buffered|linebuffer|linebuffered|lb", "link|xapply", "m",
	"max-line-length-allowed|maxlinelengthallowed", "no-ctrl-c|no-ctrlc|noctrlc",
	"no-keep-order|nokeeporder|nok|no-k", "no-run-if-empty|norunifempty|r", "nonall", "noswap", "null|0",
	"number-of-cores|numberofcores", "number-of-cpus|numberofcpus", "number-of-sockets|numberofsockets",
	"number-of-threads|numberofthreads", "onall", "open-tty|o", "output-as-files0|outputasfiles0|files0",
	"output-as-files|outputasfiles|files", "pipe-part|pipepart", "pipe|spreadstdin", "plain", "plus", "progress",
	"quote|q", "recordenv|record-env", "regexp|regex", "remove-rec-sep|removerecsep|rrs", "resume",
	"resume-failed|resumefailed", "retry-failed|retryfailed", "round-robin|roundrobin|round", "semaphore",
	"session", "shebang|hashbang", "shell-quote|shellquote|shell_quote", "show-limits|showlimits", "shuf",
	"silent", "skip-first-line|skipfirstline", "tag", "tee", "tmux", "tmux-pane|tmuxpane", "tollef", "transfer",
	"tty", "ungroup|u", "unsafe", "use-cores-instead-of-threads|usecoresinsteadofthreads",
	"use-cpus-instead-of-cores|usecpusinsteadofcores",
	"use-sockets-instead-of-threads|usesocketsinsteadofthreads", "v", "verbose|t", "version|V", "wait",
	"will-cite|willcite|nn|nonotice|no-notice", "xargs", "_bug", "_no_blocksize_warning", "_pipe-means-argfiles",
}

// parallelOpt опция parallel: первое имя, тип значения (0 — флаг, 's', 'i',
// 'f') и необязательность значения
type parallelOpt struct {
	name     string
	kind     byte
	optional bool
}

// parallelSet опция в командной строке: имя (первое из parallelOptionSpecs) и
// значение (пусто, если его нет)
type parallelSet struct {
	name, value string
}

// parallelShort — однобуквенные имена с учётом регистра (пачка -kS host);
// parallelLong — имена для --имя и +имя без учёта регистра: Getopt::Long
// сравнивает их в нижнем регистре, однобуквенные заглавные (-S, -I, -C) там
// не находятся (--S — это -s)
var parallelShort, parallelLong = parallelOptionTables()

var (
	getoptIntRe   = regexp.MustCompile(`^[-+]?[0-9]+$`)
	getoptFloatRe = regexp.MustCompile(`^[-+]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][-+]?[0-9]+)?$`)
)

func parallelOptionTables() (map[string]parallelOpt, map[string]parallelOpt) {
	short, long := map[string]parallelOpt{}, map[string]parallelOpt{}
	for _, spec := range parallelOptionSpecs {
		names, o := spec, parallelOpt{}
		if i := strings.IndexAny(spec, "=:"); i >= 0 {
			names, o.optional, o.kind = spec[:i], spec[i] == ':', spec[i+1]
		}
		aliases := strings.Split(names, "|")
		o.name = aliases[0]
		for _, a := range aliases {
			if len(a) == 1 {
				short[a] = o
			}
			if len(a) > 1 || a == strings.ToLower(a) {
				long[strings.ToLower(a)] = o
			}
		}
	}
	return short, long
}

// parallelLongOption опция по имени из --имя: точное совпадение или
// единственная опция, имя которой начинается с него (--sshl неоднозначно:
// sshlogin и sshloginfile)
func parallelLongOption(name string) (parallelOpt, bool) {
	name = strings.ToLower(name)
	if o, ok := parallelLong[name]; ok {
		return o, true
	}
	hits := map[string]parallelOpt{}
	for k, o := range parallelLong {
		if strings.HasPrefix(k, name) {
			hits[o.name] = o
		}
	}
	for _, o := range hits {
		return o, len(hits) == 1
	}
	return parallelOpt{}, false
}

// valid значение подходит по типу (=i — целое, =f — число)
func (o parallelOpt) valid(v string) bool {
	switch o.kind {
	case 'i':
		return getoptIntRe.MatchString(v)
	case 'f':
		return getoptFloatRe.MatchString(v)
	}
	return true
}

// next значение опции из слова args[i]: обязательное — любое слово,
// необязательное — слово, не похожее на опцию (у :f — число). j — следующее
// слово, ok — false, когда обязательного значения нет
func (o parallelOpt) next(args []string, i int) (value string, has bool, j int, ok bool) {
	if i >= len(args) {
		return "", false, i, o.optional
	}
	a := args[i]
	switch {
	case !o.optional:
	case o.kind == 'f' && !getoptFloatRe.MatchString(a):
		return "", false, i, true
	case len(a) > 1 && (a[0] == '-' || a[0] == '+'):
		return "", false, i, true
	}
	return a, true, i + 1, true
}

// parallelGetopt разбирает опции parallel, как Getopt::Long с bundling и
// require_order: до первого слова не опции или после --. ok — false, когда
// разбор не прошёл (неизвестная или неоднозначная опция, нет значения,
// значение не того типа): parallel тогда выходит, ничего не запустив
func parallelGetopt(args []string) (sets []parallelSet, rest []string, ok bool) {
	ok = true
	i := 0
	for i < len(args) {
		a := args[i]
		if a == "--" {
			i++
			break
		}
		var good bool
		switch {
		case strings.HasPrefix(a, "--"):
			sets, i, good = parallelLongArg(a[2:], args, i+1, sets)
		case len(a) > 1 && a[0] == '+':
			sets, i, good = parallelLongArg(a[1:], args, i+1, sets)
		case len(a) > 1 && a[0] == '-':
			sets, i, good = parallelBundle(a[1:], args, i+1, sets)
		default:
			return sets, args[i:], ok
		}
		ok = ok && good
	}
	return sets, args[i:], ok
}

// parallelLongArg опция --имя[=значение]; i — слово после неё
func parallelLongArg(body string, args []string, i int, sets []parallelSet) ([]parallelSet, int, bool) {
	name, value, inline := strings.Cut(body, "=")
	o, found := parallelLongOption(name)
	if !found {
		return sets, i, false
	}
	if o.kind == 0 {
		return append(sets, parallelSet{name: o.name}), i, !inline
	}
	has, ok := inline, true
	if !inline {
		value, has, i, ok = o.next(args, i)
	}
	return append(sets, parallelSet{o.name, value}), i, ok && (!has || o.valid(value))
}

// parallelBundle пачка однобуквенных опций -kj4 (-k -j 4): буква со
// значением забирает остаток пачки или следующее слово
func parallelBundle(bundle string, args []string, i int, sets []parallelSet) ([]parallelSet, int, bool) {
	for k := range len(bundle) {
		o, found := parallelShort[bundle[k:k+1]]
		if !found {
			return sets, i, false
		}
		if o.kind == 0 {
			sets = append(sets, parallelSet{name: o.name})
			continue
		}
		value, has, ok := bundle[k+1:], k+1 < len(bundle), true
		if !has {
			value, has, i, ok = o.next(args, i)
		}
		return append(sets, parallelSet{o.name, value}), i, ok && (!has || o.valid(value))
	}
	return sets, i, true
}

var (
	sshGroupRe = regexp.MustCompile(`^@[^/]+/`)
	sshCPUsRe  = regexp.MustCompile(`^[0-9]+/`)
	sshCmdRe   = regexp.MustCompile(`^.* `)
	sshUserRe  = regexp.MustCompile(`^.*@`)
	sshNameRe  = regexp.MustCompile(`^[-a-zA-Z0-9._]+`)
	sshRFCRe   = regexp.MustCompile(`(?i)^\\[\[\]box0-9a-f.]+`)
	sshIPv6Re  = regexp.MustCompile(`(?i)^\[([0-9a-f:]+)\]|^([0-9a-f:]+)`)
)

// sshLoginHost машина записи --sshlogin, как SSHLogin->new у parallel:
// [@группы/][ncpu/][команда ssh ][user[:pass]@]host[:port]. host — пусто,
// когда записи машины нет (-S @grp/ — только группа); «:» — эта машина
func sshLoginHost(s string) string {
	for _, re := range []*regexp.Regexp{sshGroupRe, sshCPUsRe, sshCmdRe, sshUserRe} {
		s = re.ReplaceAllLiteralString(s, "")
	}
	if strings.Count(s, ":") < 2 {
		if m := sshNameRe.FindString(s); m != "" {
			return m
		}
	}
	if m := sshRFCRe.FindString(s); m != "" {
		return m
	}
	if m := sshIPv6Re.FindStringSubmatch(s); m != nil {
		return m[1] + m[2]
	}
	return s
}

// parallelLocalLogins в списках машин -S есть эта машина («:», 4/:,
// @grp/:, ssh :) или машины, которых не видно до запуска: значение из
// подстановки, «-» и «..» (список из ввода и ~/.parallel/sshloginfile).
// hosts — есть хотя бы одна машина
func parallelLocalLogins(logins []string) (local, hosts bool) {
	protect := strings.NewReplacer(`\,`, "\x00", ",,", "\x00")
	for _, l := range logins {
		if computedArg(l) {
			return true, true
		}
		for _, s := range strings.FieldsFunc(protect.Replace(l), func(r rune) bool { return r == ',' || r == '\n' }) {
			s = strings.TrimRight(s, " \t\r\f\v")
			if s == ".." || s == "-" {
				return true, true
			}
			switch sshLoginHost(s) {
			case ":":
				return true, true
			case "":
			default:
				hosts = true
			}
		}
	}
	return false, hosts
}
