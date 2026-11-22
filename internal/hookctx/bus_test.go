package hookctx

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestBusAddressRequest(t *testing.T) {
	request := New(nil, nil, nil, nil, "/nonexistent-request", 1, 2)
	cases := map[string]string{
		"unix:path=bus":                         "unix:path=/nonexistent-request/bus",
		"unix:path=/run/user/1/bus":             "unix:path=/run/user/1/bus",
		"unix:abstract=bus":                     "unix:abstract=bus",
		"unix:path=@bus":                        "unix:path=@bus",
		"unix:guid=1,path=a%20b":                "unix:guid=1,path=/nonexistent-request/a%20b",
		"unix:path=":                            "unix:path=",
		"unix:path=%zz":                         "unix:path=%zz",
		"tcp:host=localhost,port=1":             "tcp:host=localhost,port=1",
		"nonce-tcp:host=h,port=1,noncefile=n":   "nonce-tcp:host=h,port=1,noncefile=/nonexistent-request/n",
		"unix:path=x;unix:path=/abs;tcp:host=h": "unix:path=/nonexistent-request/x;unix:path=/abs;tcp:host=h",
		"autolaunch:":                           "autolaunch:",
		"garbage":                               "garbage",
		"unix:path=a,path=b":                    "unix:path=/nonexistent-request/a,path=b",
	}
	for in, want := range cases {
		if got := request.BusAddress(in); got != want {
			t.Errorf("BusAddress(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestConnectBusSilentPeerReleasesResources(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bus")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_, _ = io.Copy(io.Discard, conn)
			_ = conn.Close()
		}
	}()
	defer func() { _ = listener.Close(); <-done }()
	resources := func() (int, int) {
		fds, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Fatal(err)
		}
		return runtime.NumGoroutine(), len(fds)
	}
	g, f := resources()
	req := New(nil, nil, nil, nil, "/", 1, 2)
	for range 20 {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
		start := time.Now()
		conn, err := req.ConnectBus(ctx, "unix:path="+path)
		cancel()
		if conn != nil {
			_ = conn.Close()
			t.Fatal("unexpected connection")
		}
		if err == nil || time.Since(start) > 115*time.Millisecond {
			t.Fatalf("timeout: %v after %v", err, time.Since(start))
		}
	}
	until := time.Now().Add(time.Second)
	for {
		gotG, gotF := resources()
		if gotG <= g && gotF <= f {
			break
		}
		if time.Now().After(until) {
			t.Fatalf("resources before %d/%d after %d/%d", g, f, gotG, gotF)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestBusAddressProcessUnchanged(t *testing.T) {
	request, err := FromProcess()
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range []string{"unix:path=bus", "nonce-tcp:host=h,port=1,noncefile=n", "unix:path=../x"} {
		if got := request.BusAddress(in); got != in {
			t.Errorf("process BusAddress(%q) = %q, want unchanged", in, got)
		}
	}
}
