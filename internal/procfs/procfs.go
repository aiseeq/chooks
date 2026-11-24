// Package procfs — чтение /proc хуком через подменяемый вход. CLI с тенью
// (daemon.shadow) записывает каждый ответ /proc (Recorder) и шлёт их демону;
// демон исполняет тот же хук на записанных ответах (Replay), а не на /proc
// своего момента: процессы к тому времени могли смениться (тикет
// hooks-daemon, этап 2б-3а; противник 2б, сценарий 6).
package procfs

import (
	"errors"
	"io/fs"
	"os"
	"slices"
	"sync"
)

// FS — чтение файлов и каталогов /proc. ReadDir отдаёт имена записей по
// возрастанию, как os.ReadDir
type FS interface {
	ReadFile(name string) ([]byte, error)
	ReadDir(name string) ([]string, error)
}

// OS — /proc машины
type OS struct{}

// ReadFile — os.ReadFile
func (OS) ReadFile(name string) ([]byte, error) { return os.ReadFile(name) }

// ReadDir — имена записей os.ReadDir
func (OS) ReadDir(name string) ([]string, error) {
	entries, err := os.ReadDir(name)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names, err
}

// Операции записи ответа
const (
	OpFile = "file"
	OpDir  = "dir"
)

// Read — один ответ /proc: путь, данные или имена, текст ошибки и признак
// «нет файла» (fs.ErrNotExist): по нему вызывающие отличают ушедший процесс
type Read struct {
	Op       string   `json:"op"`
	Path     string   `json:"path"`
	Data     []byte   `json:"data,omitempty"`
	Names    []string `json:"names,omitempty"`
	Err      string   `json:"err,omitempty"`
	NotExist bool     `json:"not_exist,omitempty"`
}

// Recorder читает через base и запоминает каждый ответ по порядку
type Recorder struct {
	base  FS
	mu    sync.Mutex
	reads []Read
}

// NewRecorder — запись ответов base
func NewRecorder(base FS) *Recorder { return &Recorder{base: base} }

// ReadFile читает и запоминает ответ
func (r *Recorder) ReadFile(name string) ([]byte, error) {
	data, err := r.base.ReadFile(name)
	r.add(Read{Op: OpFile, Path: name, Data: slices.Clone(data)}, err)
	return data, err
}

// ReadDir читает и запоминает ответ
func (r *Recorder) ReadDir(name string) ([]string, error) {
	names, err := r.base.ReadDir(name)
	r.add(Read{Op: OpDir, Path: name, Names: slices.Clone(names)}, err)
	return names, err
}

func (r *Recorder) add(read Read, err error) {
	if err != nil {
		read.Err, read.NotExist = err.Error(), errors.Is(err, fs.ErrNotExist)
	}
	r.mu.Lock()
	r.reads = append(r.reads, read)
	r.mu.Unlock()
}

// Reads — записанные ответы по порядку чтения
func (r *Recorder) Reads() []Read {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.reads)
}

// Replay отвечает записанными ответами. Повторное чтение того же пути
// получает следующий записанный ответ, а после последнего — его же. Чтение
// пути, которого нет в записи, — ошибка ErrNotRecorded и пометка Missing:
// сравнение такого исполнения с CLI невозможно
type Replay struct {
	mu      sync.Mutex
	reads   map[replayKey][]Read
	next    map[replayKey]int
	missing []string
}

type replayKey struct{ op, path string }

// NewReplay — ответы из записи CLI
func NewReplay(reads []Read) *Replay {
	r := &Replay{reads: map[replayKey][]Read{}, next: map[replayKey]int{}}
	for _, read := range reads {
		key := replayKey{read.Op, read.Path}
		r.reads[key] = append(r.reads[key], read)
	}
	return r
}

// ErrNotRecorded — путь не читался в CLI: ответа /proc для него нет
var ErrNotRecorded = notRecorded{}

type notRecorded struct{}

func (notRecorded) Error() string { return "procfs: read not recorded by CLI" }

func (r *Replay) take(op, path string) (Read, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := replayKey{op, path}
	list := r.reads[key]
	if len(list) == 0 {
		r.missing = append(r.missing, op+" "+path)
		return Read{}, false
	}
	i := min(r.next[key], len(list)-1)
	r.next[key] = i + 1
	return list[i], true
}

// ReadFile — записанный ответ
func (r *Replay) ReadFile(name string) ([]byte, error) {
	read, ok := r.take(OpFile, name)
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: name, Err: ErrNotRecorded}
	}
	return slices.Clone(read.Data), replayedError(read)
}

// ReadDir — записанный ответ
func (r *Replay) ReadDir(name string) ([]string, error) {
	read, ok := r.take(OpDir, name)
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: name, Err: ErrNotRecorded}
	}
	return slices.Clone(read.Names), replayedError(read)
}

// Missing — пути, которых не было в записи
func (r *Replay) Missing() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.missing)
}

// replayError — ошибка записи с тем же текстом; «нет файла» сохраняет
// errors.Is(err, fs.ErrNotExist)
type replayError struct {
	text     string
	notExist bool
}

func (e replayError) Error() string { return e.text }

func (e replayError) Is(target error) bool { return e.notExist && target == fs.ErrNotExist }

func replayedError(read Read) error {
	if read.Err == "" {
		return nil
	}
	return replayError{text: read.Err, notExist: read.NotExist}
}
