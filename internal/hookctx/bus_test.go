package hookctx

import (
	"testing"
)

func TestBusAddressRequest(t *testing.T) {
	request := New(nil, nil, nil, nil, "/nonexistent-request", 1, 2)
	cases := map[string]string{
		"unix:path=bus":                         "unix:path=/nonexistent-request/bus",
		"unix:path=/run/user/1/bus":             "unix:path=/run/user/1/bus",
		"unix:abstract=bus":                     "unix:abstract=bus",
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
