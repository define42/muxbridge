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

// TestTunnelResponseWriterWriteHeaderSendErrorBlocksWrite ensures that when
// WriteHeader's send fails, the recorded error short-circuits the following
// Write without producing a second send call.
func TestTunnelResponseWriterWriteHeaderSendErrorBlocksWrite(t *testing.T) {
	t.Parallel()

	sendErr := errors.New("start send failed")
	sendCalls := 0
	w := newTunnelResponseWriter("req-4", func(frame *tunnelpb.ClientFrame) error {
		sendCalls++
		if frame.GetResponseStart() != nil {
			return sendErr
		}
		return nil
	})

	w.WriteHeader(http.StatusTeapot)
	if sendCalls != 1 {
		t.Fatalf("sendCalls after WriteHeader = %d, want 1", sendCalls)
	}

	n, err := w.Write([]byte("body"))
	if !errors.Is(err, sendErr) {
		t.Fatalf("Write err = %v, want %v", err, sendErr)
	}
	if n != 0 {
		t.Fatalf("Write n = %d, want 0", n)
	}
	// No additional send should be attempted once WriteHeader has failed.
	if sendCalls != 1 {
		t.Fatalf("sendCalls after Write = %d, want 1", sendCalls)
	}
}

// TestTunnelResponseWriterWriteSkippedWhenHeaderSendFailedLazily covers the
// path where Write triggers an implicit WriteHeader that fails: the error
// returned by WriteHeader must be surfaced to the caller without attempting
// to send the body frame.
func TestTunnelResponseWriterWriteSkippedWhenHeaderSendFailedLazily(t *testing.T) {
	t.Parallel()

	sendErr := errors.New("header send failed")
	var bodyAttempts int
	w := newTunnelResponseWriter("req-4b", func(frame *tunnelpb.ClientFrame) error {
		if frame.GetResponseStart() != nil {
			return sendErr
		}
		if frame.GetResponseBody() != nil {
			bodyAttempts++
		}
		return nil
	})

	n, err := w.Write([]byte("body"))
	if !errors.Is(err, sendErr) {
		t.Fatalf("Write err = %v, want %v", err, sendErr)
	}
	if n != 0 {
		t.Fatalf("Write n = %d, want 0", n)
	}
	if bodyAttempts != 0 {
		t.Fatalf("body send attempts = %d, want 0", bodyAttempts)
	}
}

// TestTunnelResponseWriterFinishPropagatesSendError ensures an error from
// sending the ResponseEnd frame is returned to the caller.
func TestTunnelResponseWriterFinishPropagatesSendError(t *testing.T) {
	t.Parallel()

	sendErr := errors.New("end send failed")
	w := newTunnelResponseWriter("req-5", func(frame *tunnelpb.ClientFrame) error {
		if frame.GetResponseEnd() != nil {
			return sendErr
		}
		return nil
	})

	if err := w.Finish(); !errors.Is(err, sendErr) {
		t.Fatalf("Finish err = %v, want %v", err, sendErr)
	}
}

// TestTunnelResponseWriterFailNilIsNoop verifies Fail(nil) returns nil
// without sending any frame.
func TestTunnelResponseWriterFailNilIsNoop(t *testing.T) {
	t.Parallel()

	var frames []*tunnelpb.ClientFrame
	w := newTunnelResponseWriter("req-6", func(frame *tunnelpb.ClientFrame) error {
		frames = append(frames, frame)
		return nil
	})

	if err := w.Fail(nil); err != nil {
		t.Fatalf("Fail(nil) = %v, want nil", err)
	}
	if len(frames) != 0 {
		t.Fatalf("frames after Fail(nil) = %d, want 0", len(frames))
	}

	// The writer is not marked ended, so normal flow still works.
	if _, err := w.Write([]byte("ok")); err != nil {
		t.Fatalf("Write after Fail(nil) error: %v", err)
	}
	if err := w.Finish(); err != nil {
		t.Fatalf("Finish after Fail(nil) error: %v", err)
	}
	if len(frames) != 3 {
		t.Fatalf("frames after full flow = %d, want 3", len(frames))
	}
}

// TestTunnelResponseWriterFailAfterFinishIsNoop ensures calling Fail after
// the writer is already ended returns nil and does not emit more frames.
func TestTunnelResponseWriterFailAfterFinishIsNoop(t *testing.T) {
	t.Parallel()

	var frames []*tunnelpb.ClientFrame
	w := newTunnelResponseWriter("req-7", func(frame *tunnelpb.ClientFrame) error {
		frames = append(frames, frame)
		return nil
	})

	w.WriteHeader(http.StatusOK)
	if err := w.Finish(); err != nil {
		t.Fatalf("Finish error: %v", err)
	}
	before := len(frames)
	if err := w.Fail(errors.New("late")); err != nil {
		t.Fatalf("Fail after Finish = %v, want nil", err)
	}
	if len(frames) != before {
		t.Fatalf("frames grew after Fail post-Finish: %d -> %d", before, len(frames))
	}
}

// TestTunnelResponseWriterFailPropagatesSendError verifies that if sending
// the ResponseError frame fails, the error is returned to the caller.
func TestTunnelResponseWriterFailPropagatesSendError(t *testing.T) {
	t.Parallel()

	sendErr := errors.New("error frame send failed")
	w := newTunnelResponseWriter("req-8", func(frame *tunnelpb.ClientFrame) error {
		if frame.GetResponseError() != nil {
			return sendErr
		}
		return nil
	})

	if err := w.Fail(errors.New("boom")); !errors.Is(err, sendErr) {
		t.Fatalf("Fail err = %v, want %v", err, sendErr)
	}
}

// TestTunnelResponseWriterFlushAfterFinishIsNoop guards the defensive
// `!w.ended` check in Flush: once Finish has completed, Flush must not
// attempt to emit another ResponseStart frame.
func TestTunnelResponseWriterFlushAfterFinishIsNoop(t *testing.T) {
	t.Parallel()

	var frames []*tunnelpb.ClientFrame
	w := newTunnelResponseWriter("req-9", func(frame *tunnelpb.ClientFrame) error {
		frames = append(frames, frame)
		return nil
	})

	// Finish before any explicit WriteHeader: the writer lazily sends
	// ResponseStart and then ResponseEnd.
	if err := w.Finish(); err != nil {
		t.Fatalf("Finish error: %v", err)
	}
	before := len(frames)
	w.Flush()
	if len(frames) != before {
		t.Fatalf("frames grew after post-Finish Flush: %d -> %d", before, len(frames))
	}
}
