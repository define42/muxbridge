package tunnel

import (
	"net/http"
	"sync"

	"github.com/define42/muxbridge/gen/tunnelpb"
	"github.com/define42/muxbridge/internal/headers"
)

type tunnelResponseWriter struct {
	mu          sync.Mutex
	header      http.Header
	wroteHeader bool
	ended       bool
	statusCode  int
	requestID   string
	send        func(*tunnelpb.ClientFrame) error
}

func newTunnelResponseWriter(requestID string, send func(*tunnelpb.ClientFrame) error) *tunnelResponseWriter {
	return &tunnelResponseWriter{
		header:     make(http.Header),
		statusCode: http.StatusOK,
		requestID:  requestID,
		send:       send,
	}
}

func (w *tunnelResponseWriter) Header() http.Header {
	return w.header
}

func (w *tunnelResponseWriter) WriteHeader(status int) {
	w.mu.Lock()
	if w.wroteHeader || w.ended {
		w.mu.Unlock()
		return
	}
	w.wroteHeader = true
	w.statusCode = status
	h := headers.ToProto(w.header)
	w.mu.Unlock()

	_ = w.send(&tunnelpb.ClientFrame{
		Msg: &tunnelpb.ClientFrame_ResponseStart{ResponseStart: &tunnelpb.ResponseStart{
			RequestId:  w.requestID,
			StatusCode: int32(status),
			Headers:    h,
		}},
	})
}

func (w *tunnelResponseWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	needHeader := !w.wroteHeader
	w.mu.Unlock()
	if needHeader {
		w.WriteHeader(http.StatusOK)
	}

	if err := w.send(&tunnelpb.ClientFrame{
		Msg: &tunnelpb.ClientFrame_ResponseBody{ResponseBody: &tunnelpb.ResponseBody{
			RequestId: w.requestID,
			Chunk:     append([]byte(nil), p...),
		}},
	}); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Flush ensures response headers are flushed to the edge. Each call to Write
// already sends a ResponseBody frame immediately, so there's nothing to
// buffer; however callers that invoke Flush before any Write expect the
// headers to have been sent, so trigger WriteHeader lazily.
func (w *tunnelResponseWriter) Flush() {
	w.mu.Lock()
	needHeader := !w.wroteHeader && !w.ended
	w.mu.Unlock()
	if needHeader {
		w.WriteHeader(http.StatusOK)
	}
}

func (w *tunnelResponseWriter) Finish() error {
	w.mu.Lock()
	if w.ended {
		w.mu.Unlock()
		return nil
	}
	needHeader := !w.wroteHeader
	w.mu.Unlock()

	if needHeader {
		w.WriteHeader(w.statusCode)
	}
	w.mu.Lock()
	if w.ended {
		w.mu.Unlock()
		return nil
	}
	w.ended = true
	w.mu.Unlock()

	return w.send(&tunnelpb.ClientFrame{
		Msg: &tunnelpb.ClientFrame_ResponseEnd{ResponseEnd: &tunnelpb.ResponseEnd{
			RequestId: w.requestID,
		}},
	})
}

func (w *tunnelResponseWriter) Fail(err error) error {
	if err == nil {
		return nil
	}

	w.mu.Lock()
	if w.ended {
		w.mu.Unlock()
		return nil
	}
	w.ended = true
	w.mu.Unlock()

	return w.send(&tunnelpb.ClientFrame{
		Msg: &tunnelpb.ClientFrame_ResponseError{ResponseError: &tunnelpb.ResponseError{
			RequestId: w.requestID,
			Message:   err.Error(),
		}},
	})
}

var _ http.ResponseWriter = (*tunnelResponseWriter)(nil)
var _ http.Flusher = (*tunnelResponseWriter)(nil)
