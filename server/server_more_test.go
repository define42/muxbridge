package server

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/define42/muxbridge/gen/tunnelpb"
)

func TestNewRejectsInvalidConfig(t *testing.T) {
	t.Parallel()

	tests := []Config{
		{PublicDomain: "localhost", TokenUsers: map[string]string{"demo-token": "demo"}},
		{PublicDomain: "example.com", TokenUsers: map[string]string{"": "demo"}},
		{PublicDomain: "example.com", TokenUsers: map[string]string{"demo-token": "demo.user"}},
		{PublicDomain: "example.com", TokenUsers: map[string]string{"demo-token": "edge"}},
		{PublicDomain: "example.com", TokenUsers: map[string]string{"demo-token": "demo", "other-token": "demo"}},
	}

	for _, cfg := range tests {
		if _, err := New(cfg); err == nil {
			t.Fatalf("New(%+v) succeeded, want error", cfg)
		}
	}
}

func TestPublicHostsReturnsSortedHosts(t *testing.T) {
	t.Parallel()

	srv, err := New(Config{
		PublicDomain: "example.com",
		TokenUsers: map[string]string{
			"z-token": "zebra",
			"a-token": "alpha",
		},
	})
	if err != nil {
		t.Fatalf("New error: %v", err)
	}

	got := srv.PublicHosts()
	want := []string{"alpha.example.com", "zebra.example.com"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("PublicHosts = %#v, want %#v", got, want)
	}
}

func TestServeHTTPReturnsBadGatewayWhenHostHasNoSession(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "https://demo.example.com/", nil)
	req.Host = "demo.example.com"
	res := httptest.NewRecorder()

	srv.ServeHTTP(res, req)

	if res.Code != http.StatusBadGateway {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusBadGateway)
	}
}

func TestServeHTTPStreamsResponseHeadersAndBody(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	sess := &session{
		publicHost: "demo.example.com",
		username:   "demo",
		outbound:   make(chan *tunnelpb.ServerFrame, 4),
		done:       make(chan struct{}),
		inflight:   make(map[string]*responseState),
	}
	srv.putSession(sess)

	req := httptest.NewRequest(http.MethodGet, "https://demo.example.com/stream", nil)
	req.Host = "demo.example.com"
	res := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		srv.ServeHTTP(res, req)
		close(done)
	}()

	startFrame := <-sess.outbound
	requestStart := startFrame.GetRequestStart()
	if requestStart == nil {
		t.Fatalf("first frame = %T, want request start", startFrame.GetMsg())
	}
	<-sess.outbound

	state := waitForState(t, sess, requestStart.GetRequestId())
	state.start <- &tunnelpb.ResponseStart{
		RequestId:  requestStart.GetRequestId(),
		StatusCode: http.StatusCreated,
		Headers: []*tunnelpb.Header{
			{Key: "Content-Type", Values: []string{"text/plain"}},
			{Key: "X-Test", Values: []string{"one", "two"}},
		},
	}
	state.body <- []byte("hello ")
	state.body <- []byte("world")
	go func() {
		time.Sleep(20 * time.Millisecond)
		state.finish()
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeHTTP did not return")
	}

	if res.Code != http.StatusCreated {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusCreated)
	}
	if got := res.Header().Values("X-Test"); len(got) != 2 || got[0] != "one" || got[1] != "two" {
		t.Fatalf("X-Test = %#v, want [one two]", got)
	}
	if got := res.Body.String(); got != "hello world" {
		t.Fatalf("body = %q, want %q", got, "hello world")
	}
}

func TestServeHTTPDrainsBufferedBodyAfterDone(t *testing.T) {
	t.Parallel()

	for i := 0; i < 20; i++ {
		srv := newTestServer(t)
		sess := &session{
			publicHost: "demo.example.com",
			username:   "demo",
			outbound:   make(chan *tunnelpb.ServerFrame, 4),
			done:       make(chan struct{}),
			inflight:   make(map[string]*responseState),
		}
		srv.putSession(sess)

		req := httptest.NewRequest(http.MethodGet, "https://demo.example.com/drain", nil)
		req.Host = "demo.example.com"
		res := newBlockingRecorder()

		done := make(chan struct{})
		go func() {
			srv.ServeHTTP(res, req)
			close(done)
		}()

		startFrame := <-sess.outbound
		requestStart := startFrame.GetRequestStart()
		if requestStart == nil {
			t.Fatalf("first frame = %T, want request start", startFrame.GetMsg())
		}
		<-sess.outbound

		state := waitForState(t, sess, requestStart.GetRequestId())
		state.start <- &tunnelpb.ResponseStart{
			RequestId:  requestStart.GetRequestId(),
			StatusCode: http.StatusOK,
			Headers: []*tunnelpb.Header{
				{Key: "Content-Type", Values: []string{"text/plain"}},
			},
		}

		select {
		case <-res.writeHeaderSeen:
		case <-time.After(2 * time.Second):
			t.Fatal("WriteHeader was not called")
		}

		state.body <- []byte("hello through grpc tunnel\n")
		state.finish()
		close(res.writeHeaderGate)

		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("ServeHTTP did not return")
		}

		if got := res.Body.String(); got != "hello through grpc tunnel\n" {
			t.Fatalf("iteration %d body = %q, want greeting", i, got)
		}
	}
}

func TestServeHTTPReturnsBadGatewayWhenTunnelEndsWithoutStart(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	sess := &session{
		publicHost: "demo.example.com",
		username:   "demo",
		outbound:   make(chan *tunnelpb.ServerFrame, 4),
		done:       make(chan struct{}),
		inflight:   make(map[string]*responseState),
	}
	srv.putSession(sess)

	req := httptest.NewRequest(http.MethodGet, "https://demo.example.com/nostart", nil)
	req.Host = "demo.example.com"
	res := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		srv.ServeHTTP(res, req)
		close(done)
	}()

	startFrame := <-sess.outbound
	requestStart := startFrame.GetRequestStart()
	if requestStart == nil {
		t.Fatalf("first frame = %T, want request start", startFrame.GetMsg())
	}
	<-sess.outbound

	state := waitForState(t, sess, requestStart.GetRequestId())
	state.finish()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeHTTP did not return")
	}
	if res.Code != http.StatusBadGateway {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusBadGateway)
	}
}

func TestResponseStateConsumeBufferedStartAndBody(t *testing.T) {
	t.Parallel()

	state := newResponseState()
	start := &tunnelpb.ResponseStart{RequestId: "req-1", StatusCode: http.StatusCreated}
	state.start <- start
	state.body <- []byte("payload")
	state.finish()

	if got := state.consumeStart(); got != start {
		t.Fatalf("consumeStart = %#v, want %#v", got, start)
	}

	gotBody, ok := state.consumeBody()
	if !ok {
		t.Fatal("consumeBody reported no body, want payload")
	}
	if string(gotBody) != "payload" {
		t.Fatalf("consumeBody = %q, want %q", string(gotBody), "payload")
	}
}

func TestServeHTTPStopsWhenLateBodyErrorArrives(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	sess := &session{
		publicHost: "demo.example.com",
		username:   "demo",
		outbound:   make(chan *tunnelpb.ServerFrame, 4),
		done:       make(chan struct{}),
		inflight:   make(map[string]*responseState),
	}
	srv.putSession(sess)

	req := httptest.NewRequest(http.MethodGet, "https://demo.example.com/stream", nil)
	req.Host = "demo.example.com"
	res := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		srv.ServeHTTP(res, req)
		close(done)
	}()

	startFrame := <-sess.outbound
	requestStart := startFrame.GetRequestStart()
	if requestStart == nil {
		t.Fatalf("first frame = %T, want request start", startFrame.GetMsg())
	}
	<-sess.outbound

	state := waitForState(t, sess, requestStart.GetRequestId())
	state.start <- &tunnelpb.ResponseStart{RequestId: requestStart.GetRequestId(), StatusCode: http.StatusOK}
	state.body <- []byte("partial")
	// Simulate a client-side ResponseError arriving after the body has
	// already started streaming: fail() both delivers the error and closes
	// done, which is how handleClientFrame signals an error in production.
	state.fail(errors.New("late error"))

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeHTTP did not return")
	}
	if res.Code != http.StatusOK {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusOK)
	}
}

type blockingRecorder struct {
	*httptest.ResponseRecorder
	writeHeaderSeen chan struct{}
	writeHeaderGate chan struct{}
	once            sync.Once
}

func newBlockingRecorder() *blockingRecorder {
	return &blockingRecorder{
		ResponseRecorder: httptest.NewRecorder(),
		writeHeaderSeen:  make(chan struct{}),
		writeHeaderGate:  make(chan struct{}),
	}
}

func (r *blockingRecorder) WriteHeader(code int) {
	r.ResponseRecorder.WriteHeader(code)
	r.once.Do(func() {
		close(r.writeHeaderSeen)
	})
	<-r.writeHeaderGate
}

func TestServeHTTPReturnsBadGatewayWhenForwardRequestFails(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	sess := &session{
		publicHost: "demo.example.com",
		username:   "demo",
		outbound:   make(chan *tunnelpb.ServerFrame, 4),
		done:       make(chan struct{}),
		inflight:   make(map[string]*responseState),
	}
	srv.putSession(sess)

	bodyR, bodyW := io.Pipe()
	_ = bodyW.CloseWithError(errors.New("body boom"))
	req := httptest.NewRequest(http.MethodPost, "https://demo.example.com/upload", bodyR)
	req.Host = "demo.example.com"
	res := httptest.NewRecorder()

	srv.ServeHTTP(res, req)

	if res.Code != http.StatusBadGateway {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusBadGateway)
	}
}

func TestServeHTTPReturnsResponseError(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	sess := &session{
		publicHost: "demo.example.com",
		username:   "demo",
		outbound:   make(chan *tunnelpb.ServerFrame, 4),
		done:       make(chan struct{}),
		inflight:   make(map[string]*responseState),
	}
	srv.putSession(sess)

	req := httptest.NewRequest(http.MethodGet, "https://demo.example.com/error", nil)
	req.Host = "demo.example.com"
	res := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		srv.ServeHTTP(res, req)
		close(done)
	}()

	startFrame := <-sess.outbound
	requestStart := startFrame.GetRequestStart()
	<-sess.outbound

	state := waitForState(t, sess, requestStart.GetRequestId())
	state.fail(errors.New("boom"))

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeHTTP did not return")
	}

	if res.Code != http.StatusBadGateway {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusBadGateway)
	}
	if got := res.Body.String(); got != publicBadGatewayMessage+"\n" {
		t.Fatalf("body = %q, want %q", got, publicBadGatewayMessage+"\\n")
	}
}

func TestServeHTTPCancelRequestOnContextDone(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	sess := &session{
		publicHost: "demo.example.com",
		username:   "demo",
		outbound:   make(chan *tunnelpb.ServerFrame, 4),
		done:       make(chan struct{}),
		inflight:   make(map[string]*responseState),
	}
	srv.putSession(sess)

	req := httptest.NewRequest(http.MethodGet, "https://demo.example.com/cancel", nil)
	req.Host = "demo.example.com"
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	res := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		srv.ServeHTTP(res, req)
		close(done)
	}()

	startFrame := <-sess.outbound
	requestStart := startFrame.GetRequestStart()
	if requestStart == nil {
		t.Fatalf("first frame = %T, want request start", startFrame.GetMsg())
	}
	<-sess.outbound

	cancel()

	cancelFrame := <-sess.outbound
	if cancelMsg := cancelFrame.GetCancelRequest(); cancelMsg == nil || cancelMsg.GetRequestId() != requestStart.GetRequestId() {
		t.Fatalf("cancel frame = %#v, want cancel for request %q", cancelFrame.GetMsg(), requestStart.GetRequestId())
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeHTTP did not return")
	}
}

func TestForwardRequestIncludesBodyAndHTTPSMetadata(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	sess := &session{
		publicHost: "demo.example.com",
		username:   "demo",
		outbound:   make(chan *tunnelpb.ServerFrame, 4),
		done:       make(chan struct{}),
		inflight:   make(map[string]*responseState),
	}

	req := httptest.NewRequest(http.MethodPost, "http://demo.example.com/upload/a%2Fb?x=1", io.NopCloser(strings.NewReader("payload")))
	req.Host = "demo.example.com:443"
	req.RemoteAddr = "127.0.0.1:1234"
	req.TLS = &tls.ConnectionState{}
	req.Header.Add("X-Test", "value")

	if err := srv.forwardRequest(req, sess, "42"); err != nil {
		t.Fatalf("forwardRequest error: %v", err)
	}

	start := (<-sess.outbound).GetRequestStart()
	if start == nil {
		t.Fatal("expected request start frame")
	}
	if start.GetRequestId() != "42" || start.GetScheme() != "https" || start.GetHost() != "demo.example.com:443" || start.GetPath() != "/upload/a/b" || start.GetRawPath() != "/upload/a%2Fb" || start.GetRawQuery() != "x=1" || start.GetRemoteAddr() != "127.0.0.1:1234" {
		t.Fatalf("unexpected request start: %#v", start)
	}
	if len(start.GetHeaders()) != 1 || start.GetHeaders()[0].GetKey() != "X-Test" {
		t.Fatalf("headers = %#v, want X-Test", start.GetHeaders())
	}

	body := (<-sess.outbound).GetRequestBody()
	if body == nil || string(body.GetChunk()) != "payload" {
		t.Fatalf("body frame = %#v, want payload", body)
	}

	end := (<-sess.outbound).GetRequestEnd()
	if end == nil || end.GetRequestId() != "42" {
		t.Fatalf("end frame = %#v, want request end for 42", end)
	}
}

func TestHandleClientFrameDispatchesMessages(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	sess := &session{
		publicHost: "demo.example.com",
		username:   "demo",
		outbound:   make(chan *tunnelpb.ServerFrame, 1),
		done:       make(chan struct{}),
		inflight:   make(map[string]*responseState),
	}

	startState := newResponseState()
	sess.put("start", startState)
	srv.handleClientFrame(sess, &tunnelpb.ClientFrame{
		Msg: &tunnelpb.ClientFrame_ResponseStart{ResponseStart: &tunnelpb.ResponseStart{RequestId: "start", StatusCode: http.StatusAccepted}},
	})
	select {
	case got := <-startState.start:
		if got.GetStatusCode() != http.StatusAccepted {
			t.Fatalf("status code = %d, want %d", got.GetStatusCode(), http.StatusAccepted)
		}
	case <-time.After(time.Second):
		t.Fatal("did not receive response start")
	}

	bodyState := newResponseState()
	sess.put("body", bodyState)
	srv.handleClientFrame(sess, &tunnelpb.ClientFrame{
		Msg: &tunnelpb.ClientFrame_ResponseBody{ResponseBody: &tunnelpb.ResponseBody{RequestId: "body", Chunk: []byte("chunk")}},
	})
	select {
	case got := <-bodyState.body:
		if string(got) != "chunk" {
			t.Fatalf("body chunk = %q, want %q", string(got), "chunk")
		}
	case <-time.After(time.Second):
		t.Fatal("did not receive response body")
	}

	endState := newResponseState()
	sess.put("end", endState)
	srv.handleClientFrame(sess, &tunnelpb.ClientFrame{
		Msg: &tunnelpb.ClientFrame_ResponseEnd{ResponseEnd: &tunnelpb.ResponseEnd{RequestId: "end"}},
	})
	select {
	case <-endState.done:
	case <-time.After(time.Second):
		t.Fatal("response end did not finish state")
	}

	errState := newResponseState()
	sess.put("err", errState)
	srv.handleClientFrame(sess, &tunnelpb.ClientFrame{
		Msg: &tunnelpb.ClientFrame_ResponseError{ResponseError: &tunnelpb.ResponseError{RequestId: "err", Message: "boom"}},
	})
	select {
	case err := <-errState.err:
		if err == nil || err.Error() != "boom" {
			t.Fatalf("error = %v, want boom", err)
		}
	case <-time.After(time.Second):
		t.Fatal("did not receive response error")
	}

	srv.handleClientFrame(sess, &tunnelpb.ClientFrame{Msg: &tunnelpb.ClientFrame_Pong{Pong: &tunnelpb.Pong{UnixNano: 1}}})
	srv.handleClientFrame(sess, &tunnelpb.ClientFrame{Msg: &tunnelpb.ClientFrame_Register{Register: &tunnelpb.Register{Token: "demo-token"}}})
}

func TestSendFrameAndStateFailures(t *testing.T) {
	t.Parallel()

	sess := &session{
		publicHost: "demo.example.com",
		username:   "demo",
		outbound:   make(chan *tunnelpb.ServerFrame, 1),
		done:       make(chan struct{}),
		inflight:   make(map[string]*responseState),
	}

	frame := &tunnelpb.ServerFrame{Msg: &tunnelpb.ServerFrame_Ping{Ping: &tunnelpb.Ping{UnixNano: 1}}}
	if err := (&Server{}).sendFrame(sess, frame); err != nil {
		t.Fatalf("sendFrame error: %v", err)
	}
	if got := <-sess.outbound; got.GetPing().GetUnixNano() != 1 {
		t.Fatalf("ping unix nano = %d, want 1", got.GetPing().GetUnixNano())
	}

	closedSess := &session{
		done: make(chan struct{}),
	}
	close(closedSess.done)
	if err := (&Server{}).sendFrame(closedSess, frame); !errors.Is(err, errSessionClosed) {
		t.Fatalf("sendFrame error = %v, want %v", err, errSessionClosed)
	}

	stateA := newResponseState()
	stateB := newResponseState()
	sess.inflight["a"] = stateA
	sess.inflight["b"] = stateB
	sess.failAll(errors.New("boom"))

	for id, state := range map[string]*responseState{"a": stateA, "b": stateB} {
		select {
		case err := <-state.err:
			if err == nil || err.Error() != "boom" {
				t.Fatalf("%s error = %v, want boom", id, err)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s state did not fail", id)
		}
	}

	stateC := newResponseState()
	stateC.fail(nil)
	select {
	case <-stateC.done:
	case <-time.After(time.Second):
		t.Fatal("stateC did not finish")
	}
}

func TestSchemeOf(t *testing.T) {
	t.Parallel()

	httpsReq := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	httpsReq.TLS = &tls.ConnectionState{}
	if got := schemeOf(httpsReq); got != "https" {
		t.Fatalf("schemeOf TLS request = %q, want %q", got, "https")
	}

	forwardedReq := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	forwardedReq.Header.Set("X-Forwarded-Proto", "https")
	if got := schemeOf(forwardedReq); got != "http" {
		t.Fatalf("schemeOf spoofed forwarded request = %q, want %q", got, "http")
	}

	plainReq := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	if got := schemeOf(plainReq); got != "http" {
		t.Fatalf("schemeOf plain request = %q, want %q", got, "http")
	}
}

func TestForwardRequestReturnsSessionClosed(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	sess := &session{
		publicHost: "demo.example.com",
		username:   "demo",
		outbound:   make(chan *tunnelpb.ServerFrame, 1),
		done:       make(chan struct{}),
		inflight:   make(map[string]*responseState),
	}
	sess.shutdown(errors.New("closed"))

	req := httptest.NewRequest(http.MethodGet, "https://demo.example.com/", nil)
	req.Host = "demo.example.com"

	if err := srv.forwardRequest(req, sess, "42"); !errors.Is(err, errSessionClosed) {
		t.Fatalf("forwardRequest error = %v, want %v", err, errSessionClosed)
	}
}

func TestServeWebSocketRequiresHijacker(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	sess := newWSSession()
	srv.putSession(sess)

	req := newWSRequest("demo.example.com")
	res := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.serveWebSocket(res, req, sess)
	}()

	startFrame := <-sess.outbound
	requestID := startFrame.GetRequestStart().GetRequestId()
	<-sess.outbound

	state := waitForState(t, sess, requestID)
	state.start <- &tunnelpb.ResponseStart{
		RequestId:  requestID,
		StatusCode: http.StatusSwitchingProtocols,
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("serveWebSocket did not return")
	}
	if res.Code != http.StatusHTTPVersionNotSupported {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusHTTPVersionNotSupported)
	}
}

func TestServeWebSocketReturnsBadGatewayWhenSessionCloses(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	sess := newWSSession()
	srv.putSession(sess)

	req := newWSRequest("demo.example.com")
	res := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.serveWebSocket(res, req, sess)
	}()

	startFrame := <-sess.outbound
	requestID := startFrame.GetRequestStart().GetRequestId()
	<-sess.outbound

	if state := waitForState(t, sess, requestID); state == nil {
		t.Fatal("missing response state")
	}
	sess.shutdown(errors.New("closed"))

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("serveWebSocket did not return")
	}
	if res.Code != http.StatusBadGateway {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusBadGateway)
	}
	if got := res.Body.String(); got != "session closed\n" {
		t.Fatalf("body = %q, want %q", got, "session closed\\n")
	}
}

func TestServeWebSocketReturnsBadGatewayOnResponseError(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	sess := newWSSession()
	srv.putSession(sess)

	req := newWSRequest("demo.example.com")
	res := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.serveWebSocket(res, req, sess)
	}()

	startFrame := <-sess.outbound
	requestID := startFrame.GetRequestStart().GetRequestId()
	<-sess.outbound

	state := waitForState(t, sess, requestID)
	state.err <- errors.New("boom")

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("serveWebSocket did not return")
	}
	if res.Code != http.StatusBadGateway {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusBadGateway)
	}
}

func TestServeWebSocketReturnsBadGatewayWhenTunnelEndsBeforeUpgrade(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	sess := newWSSession()
	srv.putSession(sess)

	req := newWSRequest("demo.example.com")
	res := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.serveWebSocket(res, req, sess)
	}()

	startFrame := <-sess.outbound
	requestID := startFrame.GetRequestStart().GetRequestId()
	<-sess.outbound

	state := waitForState(t, sess, requestID)
	state.finish()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("serveWebSocket did not return")
	}
	if res.Code != http.StatusBadGateway {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusBadGateway)
	}
}
