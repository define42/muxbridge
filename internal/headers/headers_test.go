package headers

import (
	"net/http"
	"testing"

	"github.com/define42/muxbridge/gen/tunnelpb"
)

func TestToProtoFromProtoAndCopy(t *testing.T) {
	t.Parallel()

	src := http.Header{
		"X-Test":     {"one", "two"},
		"Set-Cookie": {"a=1"},
	}

	protoHeaders := ToProto(src)
	src["X-Test"][0] = "changed"

	roundTrip := FromProto(protoHeaders)
	if got := roundTrip.Values("X-Test"); len(got) != 2 || got[0] != "one" || got[1] != "two" {
		t.Fatalf("roundTrip X-Test = %#v, want [one two]", got)
	}
	if got := roundTrip.Values("Set-Cookie"); len(got) != 1 || got[0] != "a=1" {
		t.Fatalf("roundTrip Set-Cookie = %#v, want [a=1]", got)
	}

	dst := http.Header{
		"X-Test": {"zero"},
	}
	Copy(dst, roundTrip)

	if got := dst.Values("X-Test"); len(got) != 3 || got[0] != "zero" || got[1] != "one" || got[2] != "two" {
		t.Fatalf("copied X-Test = %#v, want [zero one two]", got)
	}
	if got := dst.Values("Set-Cookie"); len(got) != 1 || got[0] != "a=1" {
		t.Fatalf("copied Set-Cookie = %#v, want [a=1]", got)
	}
}

func TestFromProtoSkipsNilEntries(t *testing.T) {
	t.Parallel()

	got := FromProto([]*tunnelpb.Header{
		nil,
		{Key: "X-Test", Values: []string{"ok"}},
	})

	if got.Get("X-Test") != "ok" {
		t.Fatalf("X-Test = %q, want %q", got.Get("X-Test"), "ok")
	}
}

func TestToProtoStripsHopByHopHeaders(t *testing.T) {
	t.Parallel()

	src := http.Header{
		"Connection":        {"keep-alive, X-Foo"},
		"Keep-Alive":        {"timeout=5"},
		"Transfer-Encoding": {"chunked"},
		"Proxy-Authenticate": {"Basic"},
		"X-Foo":             {"hop"},
		"X-Bar":             {"keep"},
	}

	out := ToProto(src)
	got := map[string]bool{}
	for _, h := range out {
		got[h.Key] = true
	}
	for _, forbidden := range []string{"Connection", "Keep-Alive", "Transfer-Encoding", "Proxy-Authenticate", "X-Foo"} {
		if got[forbidden] {
			t.Errorf("header %q was forwarded but should be stripped", forbidden)
		}
	}
	if !got["X-Bar"] {
		t.Error("header X-Bar was unexpectedly stripped")
	}
}

func TestToProtoPreservesUpgradeHeaders(t *testing.T) {
	t.Parallel()

	src := http.Header{
		"Connection":            {"Upgrade"},
		"Upgrade":               {"websocket"},
		"Sec-Websocket-Key":     {"dGhlIHNhbXBsZSBub25jZQ=="},
		"Sec-Websocket-Version": {"13"},
	}

	out := ToProto(src)
	got := map[string]bool{}
	for _, h := range out {
		got[h.Key] = true
	}
	for _, required := range []string{"Connection", "Upgrade", "Sec-Websocket-Key", "Sec-Websocket-Version"} {
		if !got[required] {
			t.Errorf("header %q was stripped but should survive websocket upgrade", required)
		}
	}
}

func TestFromProtoStripsHopByHopHeaders(t *testing.T) {
	t.Parallel()

	in := []*tunnelpb.Header{
		{Key: "Connection", Values: []string{"close"}},
		{Key: "Transfer-Encoding", Values: []string{"chunked"}},
		{Key: "X-Keep", Values: []string{"value"}},
	}
	got := FromProto(in)
	if got.Get("Connection") != "" {
		t.Error("Connection should be stripped when no upgrade is in progress")
	}
	if got.Get("Transfer-Encoding") != "" {
		t.Error("Transfer-Encoding should be stripped")
	}
	if got.Get("X-Keep") != "value" {
		t.Errorf("X-Keep = %q, want %q", got.Get("X-Keep"), "value")
	}
}
