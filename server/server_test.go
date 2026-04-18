package server

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
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
