// Package hookwire — протокол между прокси хука (chooks-hook) и демоном
// (chooks serve) по unix-сокету. Пакет без зависимостей, кроме errors и io:
// его импортирует прокси, чей запуск — часть времени каждого хука.
//
// Обмен на одно соединение:
//
//	прокси → демон  запрос: "CHK1", u32 длина тела, тело (EncodeRequest)
//	демон → прокси  ответ приёма: 'A' — принято, или 'F', u32 длина, причина —
//	                «в CLI: <причина>», после него демон закрывает соединение
//	прокси → демон  'C' — прокси получил «принято» в свой срок и ждёт результат
//	демон → прокси  результат: 'R', i32 код, u32 длина, stdout, u32 длина, stderr
//
// Демон исполняет запрос только после 'C': прокси, не дождавшийся «принято»
// к сроку, закрывает соединение без 'C' и уходит в CLI — исполнение не
// начинается, и эффект хука не удваивается. После 'C' прокси в CLI не уходит:
// обрыв — код 1. Любое чтение демона после 'C' (обрыв, лишние байты) отменяет
// запрос.
//
// Числа — big-endian. Строка — u32 длина и байты.
package hookwire

import (
	"errors"
	"io"
	"unicode/utf8"
)

// Magic — начало запроса и версия протокола
const Magic = "CHK1"

// Байты кадров
const (
	Accepted = 'A'
	Fallback = 'F'
	Commit   = 'C'
	Result   = 'R'
)

// Пределы: запрос сверх них демон не читает и отвечает «в CLI»
const (
	MaxStdin   = 64 << 20 // stdin хука
	MaxString  = 1 << 20  // одна строка: аргумент, запись env, cwd
	MaxCount   = 8192     // число аргументов или записей env
	MaxHeader  = 16 << 20 // всё, кроме stdin
	MaxRequest = MaxHeader + MaxStdin
	MaxReason  = 1024     // причина «в CLI»
	MaxOutput  = 64 << 20 // stdout или stderr результата
)

// Request — один вызов хука: то, что CLI взял бы из своего процесса
type Request struct {
	Subcommand string
	Args       []string // аргументы после подкоманды
	Env        []string
	Cwd        string
	PID        int
	PPID       int
	Stdin      []byte
}

// ErrTooLarge — запрос или его поле больше предела
var ErrTooLarge = errors.New("hookwire: request too large")

// ErrMalformed — тело запроса не разбирается
var ErrMalformed = errors.New("hookwire: malformed request")

// ErrMagic — начало не "CHK1": другой протокол или версия
var ErrMagic = errors.New("hookwire: bad magic")

func appendU32(b []byte, v uint32) []byte {
	return append(b, byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

func u32(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

func appendString(b []byte, s string) []byte {
	b = appendU32(b, uint32(len(s)))
	return append(b, s...)
}

// EncodeRequest — запрос целиком, с началом и длиной тела. Предел размера
// проверяет демон: прокси шлёт как есть, а большой stdin до кадра не доходит
// (прокси уходит в CLI сам)
func EncodeRequest(r Request) []byte {
	size := 4 + 4 + 4 + len(r.Subcommand) + 4 + 4 + len(r.Cwd) + 8 + 4 + len(r.Stdin)
	for _, a := range r.Args {
		size += 4 + len(a)
	}
	for _, e := range r.Env {
		size += 4 + len(e)
	}
	b := make([]byte, 0, size+8)
	b = append(b, Magic...)
	b = appendU32(b, 0)
	b = appendString(b, r.Subcommand)
	b = appendU32(b, uint32(len(r.Args)))
	for _, a := range r.Args {
		b = appendString(b, a)
	}
	b = appendU32(b, uint32(len(r.Env)))
	for _, e := range r.Env {
		b = appendString(b, e)
	}
	b = appendString(b, r.Cwd)
	b = appendU32(b, uint32(r.PID))
	b = appendU32(b, uint32(r.PPID))
	b = appendU32(b, uint32(len(r.Stdin)))
	b = append(b, r.Stdin...)
	body := uint32(len(b) - 8)
	b[4], b[5], b[6], b[7] = byte(body>>24), byte(body>>16), byte(body>>8), byte(body)
	return b
}

// ReadRequest читает запрос: начало, длину и тело целиком. Ошибка чтения
// (обрыв, срок) возвращается как есть — запрос не прочитан целиком
func ReadRequest(r io.Reader) (Request, error) {
	var head [8]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return Request{}, err
	}
	if string(head[:4]) != Magic {
		return Request{}, ErrMagic
	}
	size := u32(head[4:])
	if size > MaxRequest {
		return Request{}, ErrTooLarge
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(r, body); err != nil {
		return Request{}, err
	}
	return DecodeRequest(body)
}

type decoder struct {
	b      []byte
	header int
	err    error
}

func (d *decoder) u32() uint32 {
	if d.err != nil {
		return 0
	}
	if len(d.b) < 4 {
		d.err = ErrMalformed
		return 0
	}
	v := u32(d.b)
	d.b = d.b[4:]
	return v
}

func (d *decoder) bytes(limit uint32) []byte {
	n := d.u32()
	if d.err != nil {
		return nil
	}
	if n > limit {
		d.err = ErrTooLarge
		return nil
	}
	if uint32(len(d.b)) < n {
		d.err = ErrMalformed
		return nil
	}
	v := d.b[:n:n]
	d.b = d.b[n:]
	return v
}

func (d *decoder) string() string {
	v := d.bytes(MaxString)
	d.header += len(v) + 4
	if d.err == nil && d.header > MaxHeader {
		d.err = ErrTooLarge
	}
	return string(v)
}

func (d *decoder) strings() []string {
	n := d.u32()
	if d.err != nil {
		return nil
	}
	if n > MaxCount {
		d.err = ErrTooLarge
		return nil
	}
	out := make([]string, 0, n)
	for i := uint32(0); i < n && d.err == nil; i++ {
		out = append(out, d.string())
	}
	return out
}

// DecodeRequest разбирает тело запроса (без начала и длины)
func DecodeRequest(body []byte) (Request, error) {
	d := &decoder{b: body}
	var r Request
	r.Subcommand = d.string()
	r.Args = d.strings()
	r.Env = d.strings()
	r.Cwd = d.string()
	r.PID = int(d.u32())
	r.PPID = int(d.u32())
	stdin := d.bytes(MaxStdin)
	if d.err != nil {
		return Request{}, d.err
	}
	if len(d.b) != 0 {
		return Request{}, ErrMalformed
	}
	r.Stdin = stdin
	return r, nil
}

// EncodeFallback — ответ «в CLI: reason»
func EncodeFallback(reason string) []byte {
	if len(reason) > MaxReason {
		// Срез по границе руны: причина остаётся валидным UTF-8
		n := MaxReason
		for n > 0 && !utf8.RuneStart(reason[n]) {
			n--
		}
		reason = reason[:n]
	}
	b := make([]byte, 0, 5+len(reason))
	b = append(b, Fallback)
	return appendString(b, reason)
}

// ReadReason читает причину «в CLI» после байта 'F'
func ReadReason(r io.Reader) (string, error) {
	var n [4]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return "", err
	}
	size := u32(n[:])
	if size > MaxReason {
		return "", ErrTooLarge
	}
	reason := make([]byte, size)
	if _, err := io.ReadFull(r, reason); err != nil {
		return "", err
	}
	return string(reason), nil
}

// EncodeResult — результат исполнения: код, stdout, stderr
func EncodeResult(code int, stdout, stderr []byte) []byte {
	b := make([]byte, 0, 13+len(stdout)+len(stderr))
	b = append(b, Result)
	b = appendU32(b, uint32(int32(code)))
	b = appendU32(b, uint32(len(stdout)))
	b = append(b, stdout...)
	b = appendU32(b, uint32(len(stderr)))
	return append(b, stderr...)
}

// ReadResult читает результат после байта 'R'. Обрыв посреди — ошибка:
// неполный вывод прокси не отдаёт
func ReadResult(r io.Reader) (code int, stdout, stderr []byte, err error) {
	var n [4]byte
	if _, err = io.ReadFull(r, n[:]); err != nil {
		return 0, nil, nil, err
	}
	code = int(int32(u32(n[:])))
	if stdout, err = readBlock(r); err != nil {
		return 0, nil, nil, err
	}
	if stderr, err = readBlock(r); err != nil {
		return 0, nil, nil, err
	}
	return code, stdout, stderr, nil
}

func readBlock(r io.Reader) ([]byte, error) {
	var n [4]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return nil, err
	}
	size := u32(n[:])
	if size > MaxOutput {
		return nil, ErrTooLarge
	}
	b := make([]byte, size)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, err
	}
	return b, nil
}
