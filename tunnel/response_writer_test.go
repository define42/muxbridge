package tunnel

import (
	"errors"
	"net/http"
	"testing"

	"github.com/define42/muxbridge/gen/tunnelpb"
)

func TestTunnelResponseWriterWriteAndFinish(t *testing.T) {
	t.Parallel()

	var frames []*tunnelpb.ClientFrame
	w := newTunnelResponseWriter("req-1", func(frame *tunnelpb.ClientFrame) error {
		frames = append(frames, frame)
		return nil
	})

	w.Header().Add("X-Test", "one")
	w.Header().Add("X-Test", "two")
	w.Flush()

	n, err := w.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("Write error: %v", err)
	}
	if n != len("hello") {
		t.Fatalf("Write n = %d, want %d", n, len("hello"))
	}
	if err := w.Finish(); err != nil {
		t.Fatalf("Finish error: %v", err)
	}
	if err := w.Finish(); err != nil {
		t.Fatalf("second Finish error: %v", err)
	}

	if len(frames) != 3 {
		t.Fatalf("frames len = %d, want 3", len(frames))
	}
	if start := frames[0].GetResponseStart(); start == nil || start.GetStatusCode() != http.StatusOK {
		t.Fatalf("start frame = %#v, want status 200", frames[0].GetMsg())
	}
	if got := frames[0].GetResponseStart().GetHeaders(); len(got) != 1 || got[0].GetKey() != "X-Test" || len(got[0].GetValues()) != 2 {
		t.Fatalf("response headers = %#v, want X-Test with two values", got)
	}
	if body := frames[1].GetResponseBody(); body == nil || string(body.GetChunk()) != "hello" {
		t.Fatalf("body frame = %#v, want hello", frames[1].GetMsg())
	}
	if end := frames[2].GetResponseEnd(); end == nil || end.GetRequestId() != "req-1" {
		t.Fatalf("end frame = %#v, want req-1", frames[2].GetMsg())
	}
}

func TestTunnelResponseWriterWriteHeaderAndFail(t *testing.T) {
	t.Parallel()

	var frames []*tunnelpb.ClientFrame
	w := newTunnelResponseWriter("req-2", func(frame *tunnelpb.ClientFrame) error {
		frames = append(frames, frame)
		return nil
	})

	w.WriteHeader(http.StatusCreated)
	w.WriteHeader(http.StatusAccepted)

	if err := w.Fail(errors.New("boom")); err != nil {
		t.Fatalf("Fail error: %v", err)
	}
	if err := w.Fail(errors.New("later")); err != nil {
		t.Fatalf("second Fail error: %v", err)
	}

	if len(frames) != 2 {
		t.Fatalf("frames len = %d, want 2", len(frames))
	}
	if start := frames[0].GetResponseStart(); start == nil || start.GetStatusCode() != http.StatusCreated {
		t.Fatalf("start frame = %#v, want status 201", frames[0].GetMsg())
	}
	if respErr := frames[1].GetResponseError(); respErr == nil || respErr.GetMessage() != "boom" {
		t.Fatalf("error frame = %#v, want boom", frames[1].GetMsg())
	}
}

func TestTunnelResponseWriterWritePropagatesSendError(t *testing.T) {
	t.Parallel()

	sendCalls := 0
	w := newTunnelResponseWriter("req-3", func(frame *tunnelpb.ClientFrame) error {
		sendCalls++
		if frame.GetResponseBody() != nil {
			return errors.New("send failed")
		}
		return nil
	})

	n, err := w.Write([]byte("hello"))
	if err == nil {
		t.Fatal("expected send error")
	}
	if n != 0 {
		t.Fatalf("Write n = %d, want 0", n)
	}
	if sendCalls != 2 {
		t.Fatalf("sendCalls = %d, want 2", sendCalls)
	}
}
