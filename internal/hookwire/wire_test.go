package hookwire

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"testing"
	"unicode/utf8"
)

func TestRequestRoundTrip(t *testing.T) {
	want := Request{Subcommand: "pre-tool-use", Args: []string{"-v", "--timeout", "2s"}, Env: []string{"HOME=/h", "EMPTY="},
		Cwd: "/work", PID: 1234, PPID: 99, Stdin: bytes.Repeat([]byte{0, 1, 2, 0xff}, 50000)}
	got, err := ReadRequest(bytes.NewReader(EncodeRequest(want)))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
	empty, err := ReadRequest(bytes.NewReader(EncodeRequest(Request{})))
	if err != nil || empty.Subcommand != "" || len(empty.Args) != 0 || len(empty.Stdin) != 0 {
		t.Fatalf("empty: %+v %v", empty, err)
	}
}

func TestRequestLimits(t *testing.T) {
	frame := EncodeRequest(Request{Subcommand: "x", Stdin: []byte("abc")})
	for name, c := range map[string]struct {
		raw  []byte
		want error
	}{
		"magic":       {append([]byte("CHK2"), frame[4:]...), ErrMagic},
		"body size":   {append([]byte(Magic), 0xff, 0xff, 0xff, 0xff), ErrTooLarge},
		"truncated":   {frame[:len(frame)-1], io.ErrUnexpectedEOF},
		"no header":   {frame[:3], io.ErrUnexpectedEOF},
		"string size": {append([]byte(Magic), 0, 0, 0, 4, 0x10, 0, 0, 0), ErrTooLarge},
		"count":       {append([]byte(Magic), 0, 0, 0, 8, 0, 0, 0, 0, 0xff, 0xff, 0xff, 0xff), ErrTooLarge},
		"short field": {append([]byte(Magic), 0, 0, 0, 5, 0, 0, 0, 9, 'x'), ErrMalformed},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ReadRequest(bytes.NewReader(c.raw)); !errors.Is(err, c.want) {
				t.Fatalf("err %v, want %v", err, c.want)
			}
		})
	}
	// Лишние байты в теле — не тот протокол
	body := EncodeRequest(Request{Subcommand: "x"})[8:]
	if _, err := DecodeRequest(append(body, 0)); !errors.Is(err, ErrMalformed) {
		t.Fatalf("trailing: %v", err)
	}
}

func TestResultAndReason(t *testing.T) {
	frame := EncodeResult(-1, []byte("out"), []byte("err"))
	if frame[0] != Result {
		t.Fatalf("frame %q", frame[:1])
	}
	code, stdout, stderr, err := ReadResult(bytes.NewReader(frame[1:]))
	if err != nil || code != -1 || string(stdout) != "out" || string(stderr) != "err" {
		t.Fatalf("%d %q %q %v", code, stdout, stderr, err)
	}
	if _, _, _, err := ReadResult(bytes.NewReader(frame[1 : len(frame)-1])); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated result: %v", err)
	}
	reason := EncodeFallback("env: HOME relative")
	if reason[0] != Fallback {
		t.Fatalf("reason %q", reason[:1])
	}
	if got, err := ReadReason(bytes.NewReader(reason[1:])); err != nil || got != "env: HOME relative" {
		t.Fatalf("%q %v", got, err)
	}
	long := EncodeFallback(string(bytes.Repeat([]byte("x"), MaxReason+10)))
	if got, err := ReadReason(bytes.NewReader(long[1:])); err != nil || len(got) != MaxReason {
		t.Fatalf("long reason %d %v", len(got), err)
	}
}

// Длинная причина режется по границе руны: остаётся валидным UTF-8
func TestReasonCutAtRune(t *testing.T) {
	long := EncodeFallback("ж" + string(bytes.Repeat([]byte("я"), MaxReason)))
	got, err := ReadReason(bytes.NewReader(long[1:]))
	if err != nil || !utf8.ValidString(got) || len(got) > MaxReason || len(got) < MaxReason-1 {
		t.Fatalf("reason %d bytes, valid %v, err %v", len(got), utf8.ValidString(got), err)
	}
}
