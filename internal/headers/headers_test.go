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
