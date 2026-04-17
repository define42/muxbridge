package server

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/define42/muxbridge/gen/tunnelpb"
)

// hijackableWriter embeds a ResponseRecorder and returns a net.Conn from Hijack.
type hijackableWriter struct {
	*httptest.ResponseRecorder
	conn net.Conn
}

func (h *hijackableWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	brw := bufio.NewReadWriter(bufio.NewReader(h.conn), bufio.NewWriter(h.conn))
	return h.conn, brw, nil
}

// skipHTTP101Headers reads and discards HTTP response headers up to the blank
// line, then returns a bufio.Reader for subsequent WebSocket frame reads.
func skipHTTP101Headers(t *testing.T, conn net.Conn) *bufio.Reader {
	t.Helper()
	br := bufio.NewReader(conn)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("reading HTTP 101 headers: %v", err)
		}
		if line == "\r\n" {
			return br
		}
	}
}

func newWSRequest(host string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "https://"+host+"/ws", nil)
	req.Host = host
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	return req
}

func newWSSession() *session {
	return &session{
		publicHost: "demo.example.com",
		username:   "demo",
		outbound:   make(chan *tunnelpb.ServerFrame, 8),
		done:       make(chan struct{}),
		inflight:   make(map[string]*responseState),
		wsMap:      make(map[string]*wsState),
	}
}

func TestServeWebSocketReturnsBadGatewayWhenNoSession(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	req := newWSRequest("demo.example.com")
	res := httptest.NewRecorder()

	srv.ServeHTTP(res, req)

	if res.Code != http.StatusBadGateway {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusBadGateway)
	}
}

func TestHandleClientFrameDispatchesWsFrames(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	sess := newWSSession()

	ws := &wsState{
		fromClient: make(chan []byte, 1),
		done:       make(chan struct{}),
	}
	sess.putWS("ws-1", ws)

	// WsData routes payload to ws.fromClient.
	srv.handleClientFrame(sess, &tunnelpb.ClientFrame{
		Msg: &tunnelpb.ClientFrame_WsData{WsData: &tunnelpb.WebSocketData{
			RequestId: "ws-1",
			Payload:   []byte("hello"),
		}},
	})
	select {
	case got := <-ws.fromClient:
		if string(got) != "hello" {
			t.Fatalf("fromClient = %q, want %q", string(got), "hello")
		}
	case <-time.After(time.Second):
		t.Fatal("did not receive WsData payload")
	}

	// WsClose closes ws.done.
	srv.handleClientFrame(sess, &tunnelpb.ClientFrame{
		Msg: &tunnelpb.ClientFrame_WsClose{WsClose: &tunnelpb.WebSocketClose{RequestId: "ws-1"}},
	})
	select {
	case <-ws.done:
	case <-time.After(time.Second):
		t.Fatal("ws.done was not closed after WsClose")
	}

	// Unknown request ID should not panic.
	srv.handleClientFrame(sess, &tunnelpb.ClientFrame{
		Msg: &tunnelpb.ClientFrame_WsData{WsData: &tunnelpb.WebSocketData{
			RequestId: "unknown",
			Payload:   []byte("ignored"),
		}},
	})
}

func TestFailAllClosesWebSocketStates(t *testing.T) {
	t.Parallel()

	sess := newWSSession()
	ws := &wsState{
		fromClient: make(chan []byte, 1),
		done:       make(chan struct{}),
	}
	sess.wsMap["ws-1"] = ws

	sess.failAll(errors.New("shutdown"))

	select {
	case <-ws.done:
	case <-time.After(time.Second):
		t.Fatal("ws.done not closed by failAll")
	}
	if _, exists := sess.wsMap["ws-1"]; exists {
		t.Fatal("ws-1 should have been removed from wsMap")
	}
}

func TestServeWebSocketForwardsRejection(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	sess := newWSSession()
	srv.putSession(sess)

	req := newWSRequest("demo.example.com")
	res := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.ServeHTTP(res, req)
	}()

	startFrame := <-sess.outbound
	requestID := startFrame.GetRequestStart().GetRequestId()
	<-sess.outbound // RequestEnd

	state := waitForState(t, sess, requestID)
	state.start <- &tunnelpb.ResponseStart{
		RequestId:  requestID,
		StatusCode: http.StatusForbidden,
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeHTTP did not return")
	}
	if res.Code != http.StatusForbidden {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusForbidden)
	}
}

func TestServeWebSocketProxiesFrames(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	sess := newWSSession()
	srv.putSession(sess)

	browserConn, serverSide := net.Pipe()
	defer func() {
		_ = browserConn.Close()
	}()

	hw := &hijackableWriter{ResponseRecorder: httptest.NewRecorder(), conn: serverSide}
	req := newWSRequest("demo.example.com")

	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.serveWebSocket(hw, req, sess)
	}()

	startFrame := <-sess.outbound
	requestID := startFrame.GetRequestStart().GetRequestId()
	if startFrame.GetRequestStart().GetMethod() != http.MethodGet {
		t.Fatalf("RequestStart method = %q, want GET", startFrame.GetRequestStart().GetMethod())
	}
	<-sess.outbound // RequestEnd

	state := waitForState(t, sess, requestID)
	state.start <- &tunnelpb.ResponseStart{
		RequestId:  requestID,
		StatusCode: http.StatusSwitchingProtocols,
		Headers: []*tunnelpb.Header{
			{Key: "Upgrade", Values: []string{"websocket"}},
			{Key: "Connection", Values: []string{"Upgrade"}},
		},
	}

	// Drain the 101 response written to the browser side.
	br := skipHTTP101Headers(t, browserConn)

	// Browser → edge → outbound as WsData.
	if _, err := browserConn.Write([]byte("browser-payload")); err != nil {
		t.Fatalf("browserConn.Write: %v", err)
	}
	var wsData *tunnelpb.WebSocketData
	for wsData == nil {
		select {
		case frame := <-sess.outbound:
			wsData = frame.GetWsData()
		case <-time.After(2 * time.Second):
			t.Fatal("timeout waiting for WsData on outbound")
		}
	}
	if string(wsData.GetPayload()) != "browser-payload" {
		t.Fatalf("WsData payload = %q, want %q", wsData.GetPayload(), "browser-payload")
	}

	// Tunnel client sends WsData → edge writes it to browser.
	srv.handleClientFrame(sess, &tunnelpb.ClientFrame{
		Msg: &tunnelpb.ClientFrame_WsData{WsData: &tunnelpb.WebSocketData{
			RequestId: requestID,
			Payload:   []byte("handler-payload"),
		}},
	})
	buf := make([]byte, len("handler-payload"))
	if _, err := io.ReadFull(br, buf); err != nil {
		t.Fatalf("reading handler-payload from browser side: %v", err)
	}
	if string(buf) != "handler-payload" {
		t.Fatalf("browser received %q, want %q", string(buf), "handler-payload")
	}

	// Tunnel client sends WsClose → serveWebSocket should return.
	srv.handleClientFrame(sess, &tunnelpb.ClientFrame{
		Msg: &tunnelpb.ClientFrame_WsClose{WsClose: &tunnelpb.WebSocketClose{RequestId: requestID}},
	})
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("serveWebSocket did not return after WsClose")
	}
}

func TestServeWebSocketSendsWsCloseWhenBrowserDisconnects(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	sess := newWSSession()
	srv.putSession(sess)

	browserConn, serverSide := net.Pipe()

	hw := &hijackableWriter{ResponseRecorder: httptest.NewRecorder(), conn: serverSide}
	req := newWSRequest("demo.example.com")

	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.serveWebSocket(hw, req, sess)
	}()

	startFrame := <-sess.outbound
	requestID := startFrame.GetRequestStart().GetRequestId()
	<-sess.outbound // RequestEnd

	state := waitForState(t, sess, requestID)
	state.start <- &tunnelpb.ResponseStart{
		RequestId:  requestID,
		StatusCode: http.StatusSwitchingProtocols,
	}
	skipHTTP101Headers(t, browserConn)

	// Closing the browser connection should trigger a WsClose toward the tunnel client.
	_ = browserConn.Close()

	select {
	case frame := <-sess.outbound:
		if frame.GetWsClose() == nil || frame.GetWsClose().GetRequestId() != requestID {
			t.Fatalf("expected WsClose for %q, got %T", requestID, frame.GetMsg())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive WsClose after browser disconnect")
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("serveWebSocket did not return after browser disconnect")
	}
}
