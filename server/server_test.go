package server

import (
	"bytes"
	"context"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/define42/muxbridge/gen/tunnelpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

func TestConnectRejectsMissingRegisterFrame(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	addr, cleanup := startServerGRPC(t, srv)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := grpc.DialContext(ctx, addr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		t.Fatalf("DialContext error: %v", err)
	}
	defer func() {
		_ = conn.Close()
	}()

	stream, err := tunnelpb.NewTunnelServiceClient(conn).Connect(ctx)
	if err != nil {
		t.Fatalf("Connect error: %v", err)
	}
	if err := stream.Send(&tunnelpb.ClientFrame{
		Msg: &tunnelpb.ClientFrame_Pong{Pong: &tunnelpb.Pong{UnixNano: 1}},
	}); err != nil {
		t.Fatalf("Send error: %v", err)
	}

	_, err = stream.Recv()
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("Connect error code = %v, want %v (err=%v)", status.Code(err), codes.InvalidArgument, err)
	}
}

func TestConnectRejectsUnknownToken(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	addr, cleanup := startServerGRPC(t, srv)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := grpc.DialContext(ctx, addr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		t.Fatalf("DialContext error: %v", err)
	}
	defer func() {
		_ = conn.Close()
	}()

	stream, err := tunnelpb.NewTunnelServiceClient(conn).Connect(ctx)
	if err != nil {
		t.Fatalf("Connect error: %v", err)
	}
	if err := stream.Send(&tunnelpb.ClientFrame{
		Msg: &tunnelpb.ClientFrame_Register{Register: &tunnelpb.Register{Token: "bad-token"}},
	}); err != nil {
		t.Fatalf("Send error: %v", err)
	}

	_, err = stream.Recv()
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("Connect error code = %v, want %v (err=%v)", status.Code(err), codes.Unauthenticated, err)
	}
}

func TestConnectRejectsMissingToken(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	addr, cleanup := startServerGRPC(t, srv)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := grpc.DialContext(ctx, addr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		t.Fatalf("DialContext error: %v", err)
	}
	defer func() {
		_ = conn.Close()
	}()

	stream, err := tunnelpb.NewTunnelServiceClient(conn).Connect(ctx)
	if err != nil {
		t.Fatalf("Connect error: %v", err)
	}
	if err := stream.Send(&tunnelpb.ClientFrame{
		Msg: &tunnelpb.ClientFrame_Register{Register: &tunnelpb.Register{}},
	}); err != nil {
		t.Fatalf("Send error: %v", err)
	}

	_, err = stream.Recv()
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("Connect error code = %v, want %v (err=%v)", status.Code(err), codes.Unauthenticated, err)
	}
}

func TestConnectReplacesPreviousSessionForSameUser(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	oldSession := &session{
		publicHost: "demo.example.com",
		username:   "demo",
		outbound:   make(chan *tunnelpb.ServerFrame, 1),
		done:       make(chan struct{}),
		inflight:   make(map[string]*responseState),
	}
	srv.putSession(oldSession)

	addr, cleanup := startServerGRPC(t, srv)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := grpc.DialContext(ctx, addr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		t.Fatalf("DialContext error: %v", err)
	}
	defer func() {
		_ = conn.Close()
	}()

	stream, err := tunnelpb.NewTunnelServiceClient(conn).Connect(ctx)
	if err != nil {
		t.Fatalf("Connect error: %v", err)
	}
	if err := stream.Send(&tunnelpb.ClientFrame{
		Msg: &tunnelpb.ClientFrame_Register{Register: &tunnelpb.Register{Token: "demo-token"}},
	}); err != nil {
		t.Fatalf("Send error: %v", err)
	}

	waitFor(t, func() bool {
		current := srv.getSession("demo.example.com")
		return current != nil && current != oldSession
	})

	select {
	case <-oldSession.done:
	case <-time.After(2 * time.Second):
		t.Fatal("old session was not shut down")
	}
}

func TestConnectStreamsOutboundFrames(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	addr, cleanup := startServerGRPC(t, srv)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := grpc.DialContext(ctx, addr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		t.Fatalf("DialContext error: %v", err)
	}
	defer func() {
		_ = conn.Close()
	}()

	stream, err := tunnelpb.NewTunnelServiceClient(conn).Connect(ctx)
	if err != nil {
		t.Fatalf("Connect error: %v", err)
	}
	if err := stream.Send(&tunnelpb.ClientFrame{
		Msg: &tunnelpb.ClientFrame_Register{Register: &tunnelpb.Register{Token: "demo-token"}},
	}); err != nil {
		t.Fatalf("Send error: %v", err)
	}

	var sess *session
	waitFor(t, func() bool {
		sess = srv.getSession("demo.example.com")
		return sess != nil
	})
	if err := srv.sendFrame(sess, &tunnelpb.ServerFrame{
		Msg: &tunnelpb.ServerFrame_Ping{Ping: &tunnelpb.Ping{UnixNano: 99}},
	}); err != nil {
		t.Fatalf("sendFrame error: %v", err)
	}

	frame, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv error: %v", err)
	}
	if ping := frame.GetPing(); ping == nil || ping.GetUnixNano() != 99 {
		t.Fatalf("ping = %#v, want unix_nano 99", ping)
	}
}

func TestConnectSendsHeartbeatOnIdleSession(t *testing.T) {
	t.Parallel()

	srv, err := New(Config{
		PublicDomain:      "example.com",
		HeartbeatInterval: 20 * time.Millisecond,
		TokenUsers: map[string]string{
			"demo-token": "demo",
		},
	})
	if err != nil {
		t.Fatalf("New error: %v", err)
	}

	addr, cleanup := startServerGRPC(t, srv)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := grpc.DialContext(ctx, addr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		t.Fatalf("DialContext error: %v", err)
	}
	defer func() {
		_ = conn.Close()
	}()

	stream, err := tunnelpb.NewTunnelServiceClient(conn).Connect(ctx)
	if err != nil {
		t.Fatalf("Connect error: %v", err)
	}
	if err := stream.Send(&tunnelpb.ClientFrame{
		Msg: &tunnelpb.ClientFrame_Register{Register: &tunnelpb.Register{Token: "demo-token"}},
	}); err != nil {
		t.Fatalf("Send error: %v", err)
	}

	frame, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv error: %v", err)
	}
	if ping := frame.GetPing(); ping == nil || ping.GetUnixNano() == 0 {
		t.Fatalf("frame = %#v, want heartbeat ping", frame.GetMsg())
	}
}

func TestNewUsesMaxInflightDefaultsAndOverrides(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	if srv.maxInflight != defaultMaxInflightPerSession {
		t.Fatalf("default maxInflight = %d, want %d", srv.maxInflight, defaultMaxInflightPerSession)
	}

	limited, err := New(Config{
		PublicDomain:          "example.com",
		MaxInflightPerSession: 7,
		TokenUsers: map[string]string{
			"demo-token": "demo",
		},
	})
	if err != nil {
		t.Fatalf("New error: %v", err)
	}
	if limited.maxInflight != 7 {
		t.Fatalf("configured maxInflight = %d, want %d", limited.maxInflight, 7)
	}
	if limited.maxTotalInflight != defaultMaxTotalInflight {
		t.Fatalf("default maxTotalInflight = %d, want %d", limited.maxTotalInflight, defaultMaxTotalInflight)
	}

	totalLimited, err := New(Config{
		PublicDomain:     "example.com",
		MaxTotalInflight: 11,
		TokenUsers: map[string]string{
			"demo-token": "demo",
		},
	})
	if err != nil {
		t.Fatalf("New error: %v", err)
	}
	if totalLimited.maxTotalInflight != 11 {
		t.Fatalf("configured maxTotalInflight = %d, want %d", totalLimited.maxTotalInflight, 11)
	}
}

func TestServeHTTPRoutesNormalizedKnownHost(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	sess := &session{
		publicHost: "demo.example.com",
		username:   "demo",
		outbound:   make(chan *tunnelpb.ServerFrame, 2),
		done:       make(chan struct{}),
		inflight:   make(map[string]*responseState),
	}
	srv.putSession(sess)

	req := httptest.NewRequest(http.MethodGet, "https://demo.example.com/hello", nil)
	req.Host = "DEMO.EXAMPLE.COM:443"
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
	if requestStart.Host != "DEMO.EXAMPLE.COM:443" {
		t.Fatalf("forwarded host = %q, want original host", requestStart.Host)
	}

	<-sess.outbound // request end
	state := waitForState(t, sess, requestStart.RequestId)
	state.start <- &tunnelpb.ResponseStart{
		RequestId:  requestStart.RequestId,
		StatusCode: http.StatusOK,
	}
	state.finish()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeHTTP did not return")
	}

	if res.Code != http.StatusOK {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusOK)
	}
}

func TestServeHTTPRejectsUnknownPublicHost(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "https://unknown.example.com/", nil)
	req.Host = "unknown.example.com"
	res := httptest.NewRecorder()

	srv.ServeHTTP(res, req)

	if res.Code != http.StatusNotFound {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusNotFound)
	}
}

func TestServeHTTPReturnsServiceUnavailableWhenInflightLimitReached(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	srv.maxInflight = 1

	sess := &session{
		publicHost: "demo.example.com",
		username:   "demo",
		outbound:   make(chan *tunnelpb.ServerFrame, 1),
		done:       make(chan struct{}),
		inflight:   make(map[string]*responseState),
	}
	sess.put("existing", newResponseState())
	srv.putSession(sess)

	req := httptest.NewRequest(http.MethodGet, "https://demo.example.com/blocked", nil)
	req.Host = "demo.example.com"
	res := httptest.NewRecorder()

	srv.ServeHTTP(res, req)

	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusServiceUnavailable)
	}
	if len(sess.inflight) != 1 {
		t.Fatalf("inflight size = %d, want %d", len(sess.inflight), 1)
	}
}

func TestServeWebSocketReturnsServiceUnavailableWhenInflightLimitReached(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	srv.maxInflight = 1

	sess := &session{
		publicHost: "demo.example.com",
		username:   "demo",
		outbound:   make(chan *tunnelpb.ServerFrame, 1),
		done:       make(chan struct{}),
		inflight:   make(map[string]*responseState),
		wsMap:      make(map[string]*wsState),
	}
	sess.put("existing", newResponseState())
	srv.putSession(sess)

	req := httptest.NewRequest(http.MethodGet, "https://demo.example.com/ws", nil)
	req.Host = "demo.example.com"
	req.Header.Set("Upgrade", "websocket")
	res := httptest.NewRecorder()

	srv.ServeHTTP(res, req)

	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusServiceUnavailable)
	}
}

func TestServerDebugLoggingAndAccessors(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	srv := &Server{
		logger:           log.New(&buf, "", 0),
		debug:            true,
		maxTotalInflight: 512,
	}
	srv.totalInflight.Store(7)

	if got := srv.TotalInflight(); got != 7 {
		t.Fatalf("TotalInflight = %d, want %d", got, 7)
	}
	if got := srv.MaxTotalInflight(); got != 512 {
		t.Fatalf("MaxTotalInflight = %d, want %d", got, 512)
	}

	sess := &session{id: 42}

	srv.debugSentFrame(sess, &tunnelpb.ServerFrame{
		Msg: &tunnelpb.ServerFrame_RequestStart{RequestStart: &tunnelpb.RequestStart{
			RequestId:  "req-1",
			Method:     http.MethodGet,
			Host:       "demo.example.com",
			Path:       "/hello",
			RawQuery:   "x=1",
			RemoteAddr: "203.0.113.1:1234",
		}},
	})
	srv.debugSentFrame(sess, &tunnelpb.ServerFrame{
		Msg: &tunnelpb.ServerFrame_RequestBody{RequestBody: &tunnelpb.RequestBody{RequestId: "req-1", Chunk: []byte("body")}},
	})
	srv.debugSentFrame(sess, &tunnelpb.ServerFrame{
		Msg: &tunnelpb.ServerFrame_RequestEnd{RequestEnd: &tunnelpb.RequestEnd{RequestId: "req-1"}},
	})
	srv.debugSentFrame(sess, &tunnelpb.ServerFrame{
		Msg: &tunnelpb.ServerFrame_CancelRequest{CancelRequest: &tunnelpb.CancelRequest{RequestId: "req-2"}},
	})
	srv.debugSentFrame(sess, &tunnelpb.ServerFrame{
		Msg: &tunnelpb.ServerFrame_Ping{Ping: &tunnelpb.Ping{UnixNano: 11}},
	})
	srv.debugSentFrame(sess, &tunnelpb.ServerFrame{
		Msg: &tunnelpb.ServerFrame_WsData{WsData: &tunnelpb.WebSocketData{RequestId: "ws-1", Payload: []byte("ws")}},
	})
	srv.debugSentFrame(sess, &tunnelpb.ServerFrame{
		Msg: &tunnelpb.ServerFrame_WsClose{WsClose: &tunnelpb.WebSocketClose{RequestId: "ws-1"}},
	})

	srv.debugReceivedFrame(sess, &tunnelpb.ClientFrame{
		Msg: &tunnelpb.ClientFrame_Register{Register: &tunnelpb.Register{Token: "demo-token"}},
	})
	srv.debugReceivedFrame(sess, &tunnelpb.ClientFrame{
		Msg: &tunnelpb.ClientFrame_ResponseStart{ResponseStart: &tunnelpb.ResponseStart{RequestId: "req-1", StatusCode: http.StatusCreated}},
	})
	srv.debugReceivedFrame(sess, &tunnelpb.ClientFrame{
		Msg: &tunnelpb.ClientFrame_ResponseBody{ResponseBody: &tunnelpb.ResponseBody{RequestId: "req-1", Chunk: []byte("body")}},
	})
	srv.debugReceivedFrame(sess, &tunnelpb.ClientFrame{
		Msg: &tunnelpb.ClientFrame_ResponseEnd{ResponseEnd: &tunnelpb.ResponseEnd{RequestId: "req-1"}},
	})
	srv.debugReceivedFrame(sess, &tunnelpb.ClientFrame{
		Msg: &tunnelpb.ClientFrame_ResponseError{ResponseError: &tunnelpb.ResponseError{RequestId: "req-2", Message: "boom"}},
	})
	srv.debugReceivedFrame(sess, &tunnelpb.ClientFrame{
		Msg: &tunnelpb.ClientFrame_WsData{WsData: &tunnelpb.WebSocketData{RequestId: "ws-1", Payload: []byte("payload")}},
	})
	srv.debugReceivedFrame(sess, &tunnelpb.ClientFrame{
		Msg: &tunnelpb.ClientFrame_WsClose{WsClose: &tunnelpb.WebSocketClose{RequestId: "ws-1"}},
	})
	srv.debugReceivedFrame(sess, &tunnelpb.ClientFrame{
		Msg: &tunnelpb.ClientFrame_Pong{Pong: &tunnelpb.Pong{UnixNano: time.Now().Add(-time.Millisecond).UnixNano()}},
	})

	output := buf.String()
	for _, want := range []string{
		"sent request_start",
		"sent request_body",
		"sent request_end",
		"sent cancel_request",
		"sent ping",
		"sent ws_data",
		"sent ws_close",
		"recv register",
		"recv response_start",
		"recv response_body",
		"recv response_end",
		"recv response_error",
		"recv ws_data",
		"recv ws_close",
		"recv pong",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("debug log output missing %q in %q", want, output)
		}
	}
}

func TestServerDebugfHonorsFlag(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	srv := &Server{logger: log.New(&buf, "", 0)}

	srv.debugf("hidden %d", 1)
	if got := buf.String(); got != "" {
		t.Fatalf("debug output with debug disabled = %q, want empty", got)
	}

	srv.debug = true
	srv.debugf("shown %d", 2)
	if got := buf.String(); !strings.Contains(got, "shown 2") {
		t.Fatalf("debug output = %q, want shown message", got)
	}
}

func TestServeHTTPReturnsServiceUnavailableWhenTotalInflightLimitReached(t *testing.T) {
	t.Parallel()

	srv, err := New(Config{
		PublicDomain:     "example.com",
		MaxTotalInflight: 1,
		TokenUsers: map[string]string{
			"demo-token":  "demo",
			"admin-token": "admin",
		},
	})
	if err != nil {
		t.Fatalf("New error: %v", err)
	}

	demoSess := &session{
		publicHost: "demo.example.com",
		username:   "demo",
		outbound:   make(chan *tunnelpb.ServerFrame, 1),
		done:       make(chan struct{}),
		inflight:   make(map[string]*responseState),
	}
	adminSess := &session{
		publicHost: "admin.example.com",
		username:   "admin",
		outbound:   make(chan *tunnelpb.ServerFrame, 1),
		done:       make(chan struct{}),
		inflight:   make(map[string]*responseState),
	}
	srv.putSession(demoSess)
	srv.putSession(adminSess)

	state, limit := srv.tryStartRequest(demoSess, "existing")
	if state == nil || limit != inflightLimitNone {
		t.Fatalf("tryStartRequest = (%v, %v), want admitted request", state, limit)
	}
	defer srv.finishRequest(demoSess, "existing")

	req := httptest.NewRequest(http.MethodGet, "https://admin.example.com/blocked", nil)
	req.Host = "admin.example.com"
	res := httptest.NewRecorder()

	srv.ServeHTTP(res, req)

	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusServiceUnavailable)
	}
	if got := strings.TrimSpace(res.Body.String()); got != "too many total in-flight requests on edge" {
		t.Fatalf("body = %q, want total inflight message", got)
	}
}

func TestFinishRequestReleasesGlobalInflightSlot(t *testing.T) {
	t.Parallel()

	srv, err := New(Config{
		PublicDomain:     "example.com",
		MaxTotalInflight: 1,
		TokenUsers: map[string]string{
			"demo-token":  "demo",
			"admin-token": "admin",
		},
	})
	if err != nil {
		t.Fatalf("New error: %v", err)
	}

	demoSess := &session{
		publicHost: "demo.example.com",
		username:   "demo",
		outbound:   make(chan *tunnelpb.ServerFrame, 1),
		done:       make(chan struct{}),
		inflight:   make(map[string]*responseState),
	}
	adminSess := &session{
		publicHost: "admin.example.com",
		username:   "admin",
		outbound:   make(chan *tunnelpb.ServerFrame, 1),
		done:       make(chan struct{}),
		inflight:   make(map[string]*responseState),
	}

	if state, limit := srv.tryStartRequest(demoSess, "a"); state == nil || limit != inflightLimitNone {
		t.Fatalf("first tryStartRequest = (%v, %v), want admitted request", state, limit)
	}
	if state, limit := srv.tryStartRequest(adminSess, "b"); state != nil || limit != inflightLimitTotal {
		t.Fatalf("second tryStartRequest = (%v, %v), want total inflight rejection", state, limit)
	}

	srv.finishRequest(demoSess, "a")

	if state, limit := srv.tryStartRequest(adminSess, "b"); state == nil || limit != inflightLimitNone {
		t.Fatalf("third tryStartRequest = (%v, %v), want admitted request after release", state, limit)
	}
}

func newTestServer(t *testing.T) *Server {
	t.Helper()

	srv, err := New(Config{
		PublicDomain: "example.com",
		TokenUsers: map[string]string{
			"demo-token": "demo",
		},
	})
	if err != nil {
		t.Fatalf("New error: %v", err)
	}
	return srv
}

func startServerGRPC(t *testing.T, srv *Server) (string, func()) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen error: %v", err)
	}

	grpcServer := grpc.NewServer()
	tunnelpb.RegisterTunnelServiceServer(grpcServer, srv)
	go func() {
		_ = grpcServer.Serve(listener)
	}()

	return listener.Addr().String(), func() {
		grpcServer.Stop()
		_ = listener.Close()
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition was not met before timeout")
}

func waitForState(t *testing.T, sess *session, requestID string) *responseState {
	t.Helper()

	var state *responseState
	waitFor(t, func() bool {
		state = sess.get(requestID)
		return state != nil
	})
	return state
}
