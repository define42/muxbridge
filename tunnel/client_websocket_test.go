package tunnel

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/define42/muxbridge/gen/tunnelpb"
)

func TestIsWebSocketUpgrade(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		headers []*tunnelpb.Header
		want    bool
	}{
		{
			name:    "websocket upgrade",
			headers: []*tunnelpb.Header{{Key: "Upgrade", Values: []string{"websocket"}}},
			want:    true,
		},
		{
			name:    "case insensitive header key",
			headers: []*tunnelpb.Header{{Key: "UPGRADE", Values: []string{"websocket"}}},
			want:    true,
		},
		{
			name:    "case insensitive header value",
			headers: []*tunnelpb.Header{{Key: "Upgrade", Values: []string{"WebSocket"}}},
			want:    true,
		},
		{
			name:    "no upgrade header",
			headers: []*tunnelpb.Header{{Key: "Content-Type", Values: []string{"application/json"}}},
			want:    false,
		},
		{
			name:    "upgrade to other protocol",
			headers: []*tunnelpb.Header{{Key: "Upgrade", Values: []string{"h2c"}}},
			want:    false,
		},
		{
			name:    "empty headers",
			headers: nil,
			want:    false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := isWebSocketUpgrade(&tunnelpb.RequestStart{Headers: tc.headers})
			if got != tc.want {
				t.Fatalf("isWebSocketUpgrade = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestOneConnListenerAcceptAndClose(t *testing.T) {
	t.Parallel()

	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	listener := newOneConnListener(c1)
	if listener.Addr().String() != c1.LocalAddr().String() {
		t.Fatalf("Addr = %q, want %q", listener.Addr(), c1.LocalAddr())
	}

	// First Accept returns the connection without error.
	conn, err := listener.Accept()
	if err != nil {
		t.Fatalf("first Accept error: %v", err)
	}
	if conn != c1 {
		t.Fatal("Accept returned wrong connection")
	}

	// Close makes the next Accept return an error immediately.
	if err := listener.Close(); err != nil {
		t.Fatalf("Close error: %v", err)
	}
	if _, err := listener.Accept(); err == nil {
		t.Fatal("Accept after Close should return an error")
	}

	// Double Close must not panic.
	if err := listener.Close(); err != nil {
		t.Fatalf("double Close error: %v", err)
	}
}

func TestOneConnListenerCloseBeforeAccept(t *testing.T) {
	t.Parallel()

	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	listener := newOneConnListener(c1)
	if err := listener.Close(); err != nil {
		t.Fatalf("Close error: %v", err)
	}

	// The buffered connection is still in the channel, so the first Accept
	// returns it (http.Server can still serve it before stopping).
	conn, err := listener.Accept()
	if err != nil {
		t.Fatalf("first Accept on closed-but-buffered listener: %v", err)
	}
	if conn != c1 {
		t.Fatal("Accept returned wrong connection")
	}

	// Once the buffer is drained, Accept returns an error.
	if _, err := listener.Accept(); err == nil {
		t.Fatal("second Accept on closed listener should return an error")
	}
}

// wsUpgradeHandler performs a bare-minimum WebSocket handshake (101 response),
// writes sendPayload to the hijacked connection, then reads one chunk and sends
// it to receivedCh.
func wsUpgradeHandler(t *testing.T, sendPayload []byte, receivedCh chan<- []byte) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Errorf("wsUpgradeHandler: Hijack not supported")
			http.Error(w, "hijack required", http.StatusInternalServerError)
			return
		}
		conn, brw, err := hj.Hijack()
		if err != nil {
			t.Errorf("wsUpgradeHandler: Hijack error: %v", err)
			return
		}
		defer conn.Close()

		fmt.Fprintf(brw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		if err := brw.Flush(); err != nil {
			return
		}

		if len(sendPayload) > 0 {
			if _, err := conn.Write(sendPayload); err != nil {
				return
			}
		}

		if receivedCh != nil {
			buf := make([]byte, 1024)
			n, err := brw.Read(buf)
			if err != nil && !isClosedErr(err) {
				t.Errorf("wsUpgradeHandler: read error: %v", err)
			}
			if n > 0 {
				cp := make([]byte, n)
				copy(cp, buf[:n])
				receivedCh <- cp
			}
		}
	}
}

func isClosedErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return err == io.EOF ||
		strings.Contains(s, "closed") ||
		strings.Contains(s, "EOF")
}

func TestRunSessionHandlesWebSocketUpgrade(t *testing.T) {
	t.Parallel()

	const (
		handlerMsg = "from-handler"
		edgeMsg    = "from-edge"
	)

	receivedByHandler := make(chan []byte, 1)
	handler := wsUpgradeHandler(t, []byte(handlerMsg), receivedByHandler)

	addr, cleanup := startTunnelGRPCServer(t, func(stream tunnelpb.TunnelService_ConnectServer) error {
		// Receive Register.
		frame, err := stream.Recv()
		if err != nil {
			return err
		}
		if frame.GetRegister() == nil {
			t.Fatalf("expected Register, got %T", frame.GetMsg())
		}

		// Send WebSocket upgrade request to tunnel client.
		if err := stream.Send(&tunnelpb.ServerFrame{
			Msg: &tunnelpb.ServerFrame_RequestStart{RequestStart: &tunnelpb.RequestStart{
				RequestId: "ws-1",
				Method:    http.MethodGet,
				Scheme:    "https",
				Host:      "demo.example.com",
				Path:      "/ws",
				Headers: []*tunnelpb.Header{
					{Key: "Upgrade", Values: []string{"websocket"}},
					{Key: "Connection", Values: []string{"Upgrade"}},
					{Key: "Sec-WebSocket-Version", Values: []string{"13"}},
					{Key: "Sec-WebSocket-Key", Values: []string{"dGhlIHNhbXBsZSBub25jZQ=="}},
				},
			}},
		}); err != nil {
			return err
		}

		// Expect ResponseStart 101 from tunnel client.
		frame, err = stream.Recv()
		if err != nil {
			return err
		}
		rs := frame.GetResponseStart()
		if rs == nil || rs.GetStatusCode() != http.StatusSwitchingProtocols {
			t.Fatalf("expected 101 ResponseStart, got code=%v msg=%T", rs.GetStatusCode(), frame.GetMsg())
		}

		// Expect WsData carrying the handler's initial message.
		frame, err = stream.Recv()
		if err != nil {
			return err
		}
		wsData := frame.GetWsData()
		if wsData == nil || wsData.GetRequestId() != "ws-1" {
			t.Fatalf("expected WsData for ws-1, got %T", frame.GetMsg())
		}
		if !bytes.Equal(wsData.GetPayload(), []byte(handlerMsg)) {
			t.Fatalf("WsData payload = %q, want %q", wsData.GetPayload(), handlerMsg)
		}

		// Send WsData back toward the handler.
		if err := stream.Send(&tunnelpb.ServerFrame{
			Msg: &tunnelpb.ServerFrame_WsData{WsData: &tunnelpb.WebSocketData{
				RequestId: "ws-1",
				Payload:   []byte(edgeMsg),
			}},
		}); err != nil {
			return err
		}

		// Handler reads edgeMsg then closes; expect WsClose.
		frame, err = stream.Recv()
		if err != nil {
			return err
		}
		if frame.GetWsClose() == nil || frame.GetWsClose().GetRequestId() != "ws-1" {
			t.Fatalf("expected WsClose for ws-1, got %T", frame.GetMsg())
		}

		return nil
	})
	defer cleanup()

	client, err := New(Config{
		EdgeAddr: "passthrough:///" + addr,
		Handler:  handler,
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

	select {
	case received := <-receivedByHandler:
		if !bytes.Equal(received, []byte(edgeMsg)) {
			t.Fatalf("handler received %q, want %q", received, edgeMsg)
		}
	case <-time.After(time.Second):
		t.Fatal("handler did not receive the edge payload")
	}
}

func TestRunSessionHandlesWebSocketRejection(t *testing.T) {
	t.Parallel()

	// Handler that rejects the WebSocket upgrade.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not allowed", http.StatusForbidden)
	})

	addr, cleanup := startTunnelGRPCServer(t, func(stream tunnelpb.TunnelService_ConnectServer) error {
		frame, err := stream.Recv()
		if err != nil {
			return err
		}
		if frame.GetRegister() == nil {
			t.Fatalf("expected Register, got %T", frame.GetMsg())
		}

		if err := stream.Send(&tunnelpb.ServerFrame{
			Msg: &tunnelpb.ServerFrame_RequestStart{RequestStart: &tunnelpb.RequestStart{
				RequestId: "ws-rej",
				Method:    http.MethodGet,
				Scheme:    "https",
				Host:      "demo.example.com",
				Path:      "/ws",
				Headers: []*tunnelpb.Header{
					{Key: "Upgrade", Values: []string{"websocket"}},
					{Key: "Connection", Values: []string{"Upgrade"}},
				},
			}},
		}); err != nil {
			return err
		}

		// Expect a non-101 ResponseStart (403 from the local handler).
		frame, err = stream.Recv()
		if err != nil {
			return err
		}
		rs := frame.GetResponseStart()
		if rs == nil {
			t.Fatalf("expected ResponseStart, got %T", frame.GetMsg())
		}
		if rs.GetStatusCode() == http.StatusSwitchingProtocols {
			t.Fatalf("expected non-101 status, got 101")
		}

		return nil
	})
	defer cleanup()

	client, err := New(Config{
		EdgeAddr: "passthrough:///" + addr,
		Handler:  handler,
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

func TestHandleWebSocketReportsInvalidRequest(t *testing.T) {
	t.Parallel()

	var frames []*tunnelpb.ClientFrame
	client := &Client{handler: http.NewServeMux()}
	client.handleWebSocket(
		context.Background(),
		&tunnelpb.RequestStart{
			RequestId: "ws-bad",
			Method:    "BAD METHOD",
			Scheme:    "https",
			Host:      "demo.example.com",
			Path:      "/ws",
		},
		make(chan []byte),
		func(frame *tunnelpb.ClientFrame) error {
			frames = append(frames, frame)
			return nil
		},
		func() {},
	)
	if len(frames) != 1 {
		t.Fatalf("frames len = %d, want 1", len(frames))
	}
	if respErr := frames[0].GetResponseError(); respErr == nil || respErr.GetRequestId() != "ws-bad" {
		t.Fatalf("frame = %#v, want response error", frames[0].GetMsg())
	}
}

func TestHandleWebSocketReturnsWhenInboundCloses(t *testing.T) {
	t.Parallel()

	done := make(chan struct{})
	frames := make(chan *tunnelpb.ClientFrame, 4)
	client := &Client{handler: wsUpgradeHandler(t, nil, nil)}
	inbound := make(chan []byte)
	go func() {
		defer close(done)
		client.handleWebSocket(
			context.Background(),
			&tunnelpb.RequestStart{
				RequestId: "ws-close",
				Method:    http.MethodGet,
				Scheme:    "https",
				Host:      "demo.example.com",
				Path:      "/ws",
				Headers: []*tunnelpb.Header{
					{Key: "Upgrade", Values: []string{"websocket"}},
					{Key: "Connection", Values: []string{"Upgrade"}},
				},
			},
			inbound,
			func(frame *tunnelpb.ClientFrame) error {
				frames <- frame
				return nil
			},
			func() {},
		)
	}()

	select {
	case frame := <-frames:
		if rs := frame.GetResponseStart(); rs == nil || rs.GetStatusCode() != http.StatusSwitchingProtocols {
			t.Fatalf("frame = %#v, want 101 response start", frame.GetMsg())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive response start")
	}

	close(inbound)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleWebSocket did not return")
	}
}

func TestHandleWebSocketReturnsWhenContextCancels(t *testing.T) {
	t.Parallel()

	done := make(chan struct{})
	frames := make(chan *tunnelpb.ClientFrame, 4)
	client := &Client{handler: wsUpgradeHandler(t, nil, nil)}
	inbound := make(chan []byte)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		defer close(done)
		client.handleWebSocket(
			ctx,
			&tunnelpb.RequestStart{
				RequestId: "ws-cancel",
				Method:    http.MethodGet,
				Scheme:    "https",
				Host:      "demo.example.com",
				Path:      "/ws",
				Headers: []*tunnelpb.Header{
					{Key: "Upgrade", Values: []string{"websocket"}},
					{Key: "Connection", Values: []string{"Upgrade"}},
				},
			},
			inbound,
			func(frame *tunnelpb.ClientFrame) error {
				frames <- frame
				return nil
			},
			func() {},
		)
	}()

	select {
	case frame := <-frames:
		if rs := frame.GetResponseStart(); rs == nil || rs.GetStatusCode() != http.StatusSwitchingProtocols {
			t.Fatalf("frame = %#v, want 101 response start", frame.GetMsg())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive response start")
	}

	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleWebSocket did not return after cancel")
	}
}

func TestHandleWebSocketSendsErrorOnBadRequest(t *testing.T) {
	t.Parallel()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})

	addr, cleanup := startTunnelGRPCServer(t, func(stream tunnelpb.TunnelService_ConnectServer) error {
		frame, err := stream.Recv()
		if err != nil {
			return err
		}
		if frame.GetRegister() == nil {
			return nil
		}

		// Send a WebSocket RequestStart with an invalid method that will cause
		// http.NewRequest to fail or the pipe write to fail.
		if err := stream.Send(&tunnelpb.ServerFrame{
			Msg: &tunnelpb.ServerFrame_RequestStart{RequestStart: &tunnelpb.RequestStart{
				RequestId: "ws-bad",
				Method:    "BAD METHOD",
				Scheme:    "https",
				Host:      "demo.example.com",
				Path:      "/ws",
				Headers: []*tunnelpb.Header{
					{Key: "Upgrade", Values: []string{"websocket"}},
				},
			}},
		}); err != nil {
			return err
		}

		// Expect either a ResponseError or a ResponseStart with a non-101 status.
		frame, err = stream.Recv()
		if err != nil {
			return err
		}
		if frame.GetResponseError() == nil && frame.GetResponseStart() == nil {
			t.Fatalf("expected ResponseError or ResponseStart, got %T", frame.GetMsg())
		}

		return nil
	})
	defer cleanup()

	client, err := New(Config{
		EdgeAddr: "passthrough:///" + addr,
		Handler:  handler,
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

// TestRunSessionWsCloseReleasesInbound verifies that when the edge sends WsClose
// the inbound channel is closed and handleWebSocket exits cleanly.
func TestRunSessionWsCloseReleasesInbound(t *testing.T) {
	t.Parallel()

	// Handler that blocks until its connection is closed.
	handlerDone := make(chan struct{})
	handler := wsUpgradeHandler(t, nil, nil)
	_ = handlerDone

	addr, cleanup := startTunnelGRPCServer(t, func(stream tunnelpb.TunnelService_ConnectServer) error {
		frame, err := stream.Recv()
		if err != nil {
			return err
		}
		if frame.GetRegister() == nil {
			return nil
		}

		if err := stream.Send(&tunnelpb.ServerFrame{
			Msg: &tunnelpb.ServerFrame_RequestStart{RequestStart: &tunnelpb.RequestStart{
				RequestId: "ws-close",
				Method:    http.MethodGet,
				Scheme:    "https",
				Host:      "demo.example.com",
				Path:      "/ws",
				Headers: []*tunnelpb.Header{
					{Key: "Upgrade", Values: []string{"websocket"}},
					{Key: "Connection", Values: []string{"Upgrade"}},
				},
			}},
		}); err != nil {
			return err
		}

		// Wait for ResponseStart 101.
		frame, err = stream.Recv()
		if err != nil {
			return err
		}
		if rs := frame.GetResponseStart(); rs == nil || rs.GetStatusCode() != http.StatusSwitchingProtocols {
			t.Fatalf("expected 101, got %T code=%v", frame.GetMsg(), frame.GetResponseStart().GetStatusCode())
		}

		// Edge closes the WebSocket before the handler sends anything.
		if err := stream.Send(&tunnelpb.ServerFrame{
			Msg: &tunnelpb.ServerFrame_WsClose{WsClose: &tunnelpb.WebSocketClose{RequestId: "ws-close"}},
		}); err != nil {
			return err
		}

		// Expect WsClose back (handler connection was closed).
		frame, err = stream.Recv()
		if err != nil {
			return err
		}
		if frame.GetWsClose() == nil {
			t.Fatalf("expected WsClose, got %T", frame.GetMsg())
		}

		return nil
	})
	defer cleanup()

	client, err := New(Config{
		EdgeAddr: "passthrough:///" + addr,
		Handler:  handler,
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

// Compile-time check that bufio is used in this file (via wsUpgradeHandler).
var _ *bufio.Reader
