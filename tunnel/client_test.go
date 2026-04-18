package tunnel

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/define42/muxbridge/gen/tunnelpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
)

type tunnelService struct {
	tunnelpb.UnimplementedTunnelServiceServer
	connect func(tunnelpb.TunnelService_ConnectServer) error
}

func (s *tunnelService) Connect(stream tunnelpb.TunnelService_ConnectServer) error {
	return s.connect(stream)
}

func TestNewValidatesConfigAndDefaults(t *testing.T) {
	t.Parallel()

	if _, err := New(Config{}); err == nil {
		t.Fatal("expected missing EdgeAddr error")
	}
	if _, err := New(Config{EdgeAddr: "127.0.0.1:1"}); err == nil {
		t.Fatal("expected missing Handler error")
	}
	if _, err := New(Config{EdgeAddr: "127.0.0.1:1", Handler: http.NewServeMux()}); err == nil {
		t.Fatal("expected missing Token error")
	}

	client, err := New(Config{
		EdgeAddr: "127.0.0.1:1",
		Handler:  http.NewServeMux(),
		Token:    "demo-token",
	})
	if err != nil {
		t.Fatalf("New error: %v", err)
	}
	if client.cfg.ReconnectBackoff != 2*time.Second {
		t.Fatalf("ReconnectBackoff = %v, want %v", client.cfg.ReconnectBackoff, 2*time.Second)
	}
}

func TestRunReturnsContextErrorWhenCanceled(t *testing.T) {
	t.Parallel()

	client, err := New(Config{
		EdgeAddr: "127.0.0.1:1",
		Handler:  http.NewServeMux(),
		Token:    "demo-token",
		Insecure: true,
	})
	if err != nil {
		t.Fatalf("New error: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := client.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want %v", err, context.Canceled)
	}
}

func TestClientLogfCloseAndConnHelpers(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	client := &Client{logger: log.New(&buf, "", 0)}
	client.logf("hello %s", "world")
	if got := buf.String(); !strings.Contains(got, "hello world") {
		t.Fatalf("log output = %q, want hello world", got)
	}

	if err := client.Close(); err != nil {
		t.Fatalf("Close error: %v", err)
	}
	if !client.isClosed() {
		t.Fatal("client should be closed after Close")
	}

	addr, cleanup := startTunnelGRPCServer(t, func(stream tunnelpb.TunnelService_ConnectServer) error {
		<-stream.Context().Done()
		return nil
	})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn := dialTunnelClientConn(t, ctx, addr)

	client = &Client{closed: true}
	client.setConn(conn)
	client.clearConn(conn)
}

func TestClientDebugLoggingAndWSDispatch(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	client := &Client{
		logger: log.New(&buf, "", 0),
		debug:  true,
	}

	client.debugReceivedFrame(&tunnelpb.ServerFrame{
		Msg: &tunnelpb.ServerFrame_RequestStart{RequestStart: &tunnelpb.RequestStart{
			RequestId:  "req-1",
			Method:     http.MethodGet,
			Host:       "demo.example.com",
			Path:       "/ws",
			RawQuery:   "x=1",
			RemoteAddr: "127.0.0.1:1234",
			Headers: []*tunnelpb.Header{
				{Key: "Connection", Values: []string{"Upgrade"}},
				{Key: "Upgrade", Values: []string{"websocket"}},
			},
		}},
	})
	client.debugReceivedFrame(&tunnelpb.ServerFrame{
		Msg: &tunnelpb.ServerFrame_RequestBody{RequestBody: &tunnelpb.RequestBody{RequestId: "req-1", Chunk: []byte("body")}},
	})
	client.debugReceivedFrame(&tunnelpb.ServerFrame{
		Msg: &tunnelpb.ServerFrame_RequestEnd{RequestEnd: &tunnelpb.RequestEnd{RequestId: "req-1"}},
	})
	client.debugReceivedFrame(&tunnelpb.ServerFrame{
		Msg: &tunnelpb.ServerFrame_CancelRequest{CancelRequest: &tunnelpb.CancelRequest{RequestId: "req-2"}},
	})
	client.debugReceivedFrame(&tunnelpb.ServerFrame{
		Msg: &tunnelpb.ServerFrame_WsData{WsData: &tunnelpb.WebSocketData{RequestId: "req-3", Payload: []byte("ws")}},
	})
	client.debugReceivedFrame(&tunnelpb.ServerFrame{
		Msg: &tunnelpb.ServerFrame_WsClose{WsClose: &tunnelpb.WebSocketClose{RequestId: "req-3"}},
	})
	client.debugReceivedFrame(&tunnelpb.ServerFrame{
		Msg: &tunnelpb.ServerFrame_Ping{Ping: &tunnelpb.Ping{UnixNano: 42}},
	})

	client.debugSentFrame(&tunnelpb.ClientFrame{
		Msg: &tunnelpb.ClientFrame_Register{Register: &tunnelpb.Register{Token: "demo-token"}},
	})
	client.debugSentFrame(&tunnelpb.ClientFrame{
		Msg: &tunnelpb.ClientFrame_ResponseStart{ResponseStart: &tunnelpb.ResponseStart{RequestId: "req-1", StatusCode: http.StatusCreated}},
	})
	client.debugSentFrame(&tunnelpb.ClientFrame{
		Msg: &tunnelpb.ClientFrame_ResponseBody{ResponseBody: &tunnelpb.ResponseBody{RequestId: "req-1", Chunk: []byte("reply")}},
	})
	client.debugSentFrame(&tunnelpb.ClientFrame{
		Msg: &tunnelpb.ClientFrame_ResponseEnd{ResponseEnd: &tunnelpb.ResponseEnd{RequestId: "req-1"}},
	})
	client.debugSentFrame(&tunnelpb.ClientFrame{
		Msg: &tunnelpb.ClientFrame_ResponseError{ResponseError: &tunnelpb.ResponseError{RequestId: "req-2", Message: "boom"}},
	})
	client.debugSentFrame(&tunnelpb.ClientFrame{
		Msg: &tunnelpb.ClientFrame_WsData{WsData: &tunnelpb.WebSocketData{RequestId: "req-3", Payload: []byte("ws")}},
	})
	client.debugSentFrame(&tunnelpb.ClientFrame{
		Msg: &tunnelpb.ClientFrame_WsClose{WsClose: &tunnelpb.WebSocketClose{RequestId: "req-3"}},
	})
	client.debugSentFrame(&tunnelpb.ClientFrame{
		Msg: &tunnelpb.ClientFrame_Pong{Pong: &tunnelpb.Pong{UnixNano: 42}},
	})

	out := buf.String()
	for _, want := range []string{
		"recv request_start",
		"websocket=true",
		"recv request_body",
		"recv request_end",
		"recv cancel_request",
		"recv ws_data",
		"recv ws_close",
		"recv ping",
		"sent register",
		"sent response_start",
		"sent response_body",
		"sent response_end",
		"sent response_error",
		"sent ws_data",
		"sent ws_close",
		"sent pong",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("debug output missing %q:\n%s", want, out)
		}
	}

	ch := make(chan []byte, 1)
	payload := []byte("payload")
	if err := dispatchWSInbound(context.Background(), ch, payload); err != nil {
		t.Fatalf("dispatchWSInbound error: %v", err)
	}
	if got := <-ch; string(got) != string(payload) {
		t.Fatalf("dispatchWSInbound payload = %q, want %q", got, payload)
	}

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := dispatchWSInbound(canceledCtx, make(chan []byte), payload); !errors.Is(err, context.Canceled) {
		t.Fatalf("dispatchWSInbound error = %v, want %v", err, context.Canceled)
	}
}

func TestClientDebugfHonorsFlag(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	client := &Client{logger: log.New(&buf, "", 0)}

	client.debugf("hidden %d", 1)
	if got := buf.String(); got != "" {
		t.Fatalf("debug output with debug disabled = %q, want empty", got)
	}

	client.debug = true
	client.debugf("shown %d", 2)
	if got := buf.String(); !strings.Contains(got, "shown 2") {
		t.Fatalf("debug output = %q, want shown message", got)
	}
}

func TestCloseClosesActiveConn(t *testing.T) {
	t.Parallel()

	addr, cleanup := startTunnelGRPCServer(t, func(stream tunnelpb.TunnelService_ConnectServer) error {
		<-stream.Context().Done()
		return nil
	})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn := dialTunnelClientConn(t, ctx, addr)

	client := &Client{conn: conn}
	if err := client.Close(); err != nil {
		t.Fatalf("Close error: %v", err)
	}
	if !client.closed {
		t.Fatal("client.closed = false, want true")
	}
	if client.conn != nil {
		t.Fatal("client.conn != nil after Close")
	}
	if state := conn.GetState(); state != connectivity.Shutdown {
		t.Fatalf("conn state = %v, want %v", state, connectivity.Shutdown)
	}
}

func TestRunSessionExchangesFrames(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("ReadAll error: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if r.Method != http.MethodPost || r.URL.Scheme != "https" || r.Host != "demo.example.com" || r.URL.Path != "/echo" || r.URL.RawQuery != "x=1" {
			t.Errorf("unexpected request metadata: method=%q scheme=%q host=%q path=%q rawQuery=%q", r.Method, r.URL.Scheme, r.Host, r.URL.Path, r.URL.RawQuery)
		}
		if got := r.Header.Get("X-Test"); got != "alpha" {
			t.Errorf("X-Test = %q, want %q", got, "alpha")
		}
		if string(body) != "payload" {
			t.Errorf("body = %q, want %q", string(body), "payload")
		}
		w.Header().Set("X-Reply", "ok")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("echo:" + string(body)))
	})
	mux.HandleFunc("/panic", func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})
	mux.HandleFunc("/wait", func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})

	addr, cleanup := startTunnelGRPCServer(t, func(stream tunnelpb.TunnelService_ConnectServer) error {
		frame, err := stream.Recv()
		if err != nil {
			return err
		}
		register := frame.GetRegister()
		if register == nil || register.GetToken() != "demo-token" || register.GetTunnelId() != "" {
			t.Fatalf("register = %#v, want demo-token and empty tunnel id", register)
		}

		if err := stream.Send(&tunnelpb.ServerFrame{
			Msg: &tunnelpb.ServerFrame_RequestStart{RequestStart: &tunnelpb.RequestStart{
				RequestId: "req-1",
				Method:    http.MethodPost,
				Scheme:    "https",
				Host:      "demo.example.com",
				Path:      "/echo",
				RawQuery:  "x=1",
				Headers:   []*tunnelpb.Header{{Key: "X-Test", Values: []string{"alpha"}}},
			}},
		}); err != nil {
			return err
		}
		if err := stream.Send(&tunnelpb.ServerFrame{
			Msg: &tunnelpb.ServerFrame_RequestBody{RequestBody: &tunnelpb.RequestBody{RequestId: "req-1", Chunk: []byte("payload")}},
		}); err != nil {
			return err
		}
		if err := stream.Send(&tunnelpb.ServerFrame{
			Msg: &tunnelpb.ServerFrame_RequestEnd{RequestEnd: &tunnelpb.RequestEnd{RequestId: "req-1"}},
		}); err != nil {
			return err
		}

		var responseBody strings.Builder
		sawResponseEnd := false
		for !sawResponseEnd {
			frame, err = stream.Recv()
			if err != nil {
				return err
			}
			switch msg := frame.GetMsg().(type) {
			case *tunnelpb.ClientFrame_ResponseStart:
				if msg.ResponseStart.GetStatusCode() != http.StatusCreated || msg.ResponseStart.GetHeaders()[0].GetKey() != "X-Reply" {
					t.Fatalf("response start = %#v, want 201 and X-Reply header", msg.ResponseStart)
				}
			case *tunnelpb.ClientFrame_ResponseBody:
				responseBody.Write(msg.ResponseBody.GetChunk())
			case *tunnelpb.ClientFrame_ResponseEnd:
				sawResponseEnd = true
			default:
				t.Fatalf("unexpected client frame: %T", frame.GetMsg())
			}
		}
		if got := responseBody.String(); got != "echo:payload" {
			t.Fatalf("response body = %q, want %q", got, "echo:payload")
		}

		if err := stream.Send(&tunnelpb.ServerFrame{
			Msg: &tunnelpb.ServerFrame_Ping{Ping: &tunnelpb.Ping{UnixNano: 42}},
		}); err != nil {
			return err
		}
		frame, err = stream.Recv()
		if err != nil {
			return err
		}
		if pong := frame.GetPong(); pong == nil || pong.GetUnixNano() != 42 {
			t.Fatalf("pong = %#v, want unix_nano 42", pong)
		}

		if err := stream.Send(&tunnelpb.ServerFrame{
			Msg: &tunnelpb.ServerFrame_RequestStart{RequestStart: &tunnelpb.RequestStart{
				RequestId: "req-2",
				Method:    http.MethodGet,
				Scheme:    "https",
				Host:      "demo.example.com",
				Path:      "/panic",
			}},
		}); err != nil {
			return err
		}
		frame, err = stream.Recv()
		if err != nil {
			return err
		}
		if respErr := frame.GetResponseError(); respErr == nil || !strings.Contains(respErr.GetMessage(), "panic while serving request") {
			t.Fatalf("panic response = %#v, want panic error", frame.GetMsg())
		}

		if err := stream.Send(&tunnelpb.ServerFrame{
			Msg: &tunnelpb.ServerFrame_RequestStart{RequestStart: &tunnelpb.RequestStart{
				RequestId: "req-3",
				Method:    "BAD METHOD",
				Scheme:    "https",
				Host:      "demo.example.com",
				Path:      "/echo",
			}},
		}); err != nil {
			return err
		}
		frame, err = stream.Recv()
		if err != nil {
			return err
		}
		if respErr := frame.GetResponseError(); respErr == nil || respErr.GetRequestId() != "req-3" || respErr.GetMessage() == "" {
			t.Fatalf("invalid request response = %#v, want response error", frame.GetMsg())
		}

		if err := stream.Send(&tunnelpb.ServerFrame{
			Msg: &tunnelpb.ServerFrame_RequestStart{RequestStart: &tunnelpb.RequestStart{
				RequestId: "req-4",
				Method:    http.MethodGet,
				Scheme:    "https",
				Host:      "demo.example.com",
				Path:      "/wait",
			}},
		}); err != nil {
			return err
		}
		if err := stream.Send(&tunnelpb.ServerFrame{
			Msg: &tunnelpb.ServerFrame_CancelRequest{CancelRequest: &tunnelpb.CancelRequest{RequestId: "req-4"}},
		}); err != nil {
			return err
		}

		sawResponseEnd = false
		for !sawResponseEnd {
			frame, err = stream.Recv()
			if err != nil {
				return err
			}
			switch msg := frame.GetMsg().(type) {
			case *tunnelpb.ClientFrame_ResponseStart:
				if msg.ResponseStart.GetStatusCode() != http.StatusOK {
					t.Fatalf("cancel response status = %d, want %d", msg.ResponseStart.GetStatusCode(), http.StatusOK)
				}
			case *tunnelpb.ClientFrame_ResponseEnd:
				sawResponseEnd = true
			default:
				t.Fatalf("unexpected cancel response frame: %T", frame.GetMsg())
			}
		}

		return nil
	})
	defer cleanup()

	client, err := New(Config{
		EdgeAddr: "passthrough:///" + addr,
		Handler:  mux,
		Token:    "demo-token",
		Insecure: true,
	})
	if err != nil {
		t.Fatalf("New error: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.runSession(ctx); err != nil {
		t.Fatalf("runSession error: %v", err)
	}
}

func TestRequestURL(t *testing.T) {
	t.Parallel()

	if got := requestURL(&tunnelpb.RequestStart{
		Scheme:   "https",
		Host:     "demo.example.com",
		Path:     "/files/a/b",
		RawPath:  "/files/a%2Fb",
		RawQuery: "x=1",
	}); got != "https://demo.example.com/files/a%2Fb?x=1" {
		t.Fatalf("requestURL = %q, want %q", got, "https://demo.example.com/files/a%2Fb?x=1")
	}
	if got := requestURL(&tunnelpb.RequestStart{
		Scheme: "https",
		Host:   "demo.example.com",
	}); got != "https://demo.example.com/" {
		t.Fatalf("requestURL empty path = %q, want %q", got, "https://demo.example.com/")
	}
}

func TestRunReconnectsAfterSessionError(t *testing.T) {
	t.Parallel()

	attempts := make(chan struct{}, 4)
	addr, cleanup := startTunnelGRPCServer(t, func(stream tunnelpb.TunnelService_ConnectServer) error {
		frame, err := stream.Recv()
		if err != nil {
			return err
		}
		if frame.GetRegister() == nil {
			t.Fatalf("expected Register, got %T", frame.GetMsg())
		}
		attempts <- struct{}{}
		if len(attempts) == 1 {
			return errors.New("boom")
		}
		<-stream.Context().Done()
		return nil
	})
	defer cleanup()

	client, err := New(Config{
		EdgeAddr:         "passthrough:///" + addr,
		Handler:          http.NewServeMux(),
		Token:            "demo-token",
		Insecure:         true,
		ReconnectBackoff: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New error: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	err = client.Run(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run error = %v, want %v", err, context.DeadlineExceeded)
	}
	if len(attempts) < 2 {
		t.Fatalf("attempts = %d, want at least 2", len(attempts))
	}
}

func TestRunReturnsNilAfterClose(t *testing.T) {
	t.Parallel()

	registered := make(chan struct{}, 1)
	addr, cleanup := startTunnelGRPCServer(t, func(stream tunnelpb.TunnelService_ConnectServer) error {
		frame, err := stream.Recv()
		if err != nil {
			return err
		}
		if frame.GetRegister() == nil {
			t.Fatalf("expected Register, got %T", frame.GetMsg())
		}
		registered <- struct{}{}
		return nil
	})
	defer cleanup()

	client, err := New(Config{
		EdgeAddr: "passthrough:///" + addr,
		Handler:  http.NewServeMux(),
		Token:    "demo-token",
		Insecure: true,
	})
	if err != nil {
		t.Fatalf("New error: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- client.Run(context.Background())
	}()

	select {
	case <-registered:
	case <-time.After(2 * time.Second):
		t.Fatal("client did not register before timeout")
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close error: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run error = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after Close")
	}
}

func TestRunStopsBeforeReconnectTimerFires(t *testing.T) {
	t.Parallel()

	firstAttempt := make(chan struct{}, 1)
	addr, cleanup := startTunnelGRPCServer(t, func(stream tunnelpb.TunnelService_ConnectServer) error {
		frame, err := stream.Recv()
		if err != nil {
			return err
		}
		if frame.GetRegister() == nil {
			t.Fatalf("expected Register, got %T", frame.GetMsg())
		}
		firstAttempt <- struct{}{}
		return errors.New("boom")
	})
	defer cleanup()

	client, err := New(Config{
		EdgeAddr:         "passthrough:///" + addr,
		Handler:          http.NewServeMux(),
		Token:            "demo-token",
		Insecure:         true,
		ReconnectBackoff: time.Hour,
	})
	if err != nil {
		t.Fatalf("New error: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- client.Run(ctx)
	}()

	select {
	case <-firstAttempt:
	case <-time.After(2 * time.Second):
		t.Fatal("client did not make the first attempt")
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run error = %v, want %v", err, context.Canceled)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestRunSessionRespectsCanceledSecureContext(t *testing.T) {
	t.Parallel()

	client, err := New(Config{
		EdgeAddr: "127.0.0.1:1",
		Handler:  http.NewServeMux(),
		Token:    "demo-token",
	})
	if err != nil {
		t.Fatalf("New error: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := client.runSession(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("runSession error = %v, want %v", err, context.Canceled)
	}
}

func startTunnelGRPCServer(t *testing.T, connect func(tunnelpb.TunnelService_ConnectServer) error) (string, func()) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen error: %v", err)
	}

	server := grpc.NewServer()
	tunnelpb.RegisterTunnelServiceServer(server, &tunnelService{connect: connect})
	go func() {
		_ = server.Serve(listener)
	}()

	return listener.Addr().String(), func() {
		server.Stop()
		_ = listener.Close()
	}
}

func dialTunnelClientConn(t *testing.T, ctx context.Context, target string) *grpc.ClientConn {
	t.Helper()

	if !strings.Contains(target, "://") {
		target = "passthrough:///" + target
	}

	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("NewClient error: %v", err)
	}

	conn.Connect()
	for {
		state := conn.GetState()
		switch state {
		case connectivity.Ready:
			return conn
		case connectivity.Shutdown:
			_ = conn.Close()
			t.Fatal("client connection shut down before becoming ready")
		}
		if !conn.WaitForStateChange(ctx, state) {
			_ = conn.Close()
			t.Fatalf("client connection did not become ready from state %s: %v", state, ctx.Err())
		}
	}
}
