package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/define42/muxbridge/gen/tunnelpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func TestConnectRejectsUnknownToken(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	stream := newFakeConnectStream(
		&tunnelpb.ClientFrame{
			Msg: &tunnelpb.ClientFrame_Register{Register: &tunnelpb.Register{Token: "bad-token"}},
		},
	)

	err := srv.Connect(stream)
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

	stream := newFakeConnectStream(
		&tunnelpb.ClientFrame{
			Msg: &tunnelpb.ClientFrame_Register{Register: &tunnelpb.Register{Token: "demo-token"}},
		},
	)

	done := make(chan error, 1)
	go func() {
		done <- srv.Connect(stream)
	}()

	waitFor(t, func() bool {
		current := srv.getSession("demo.example.com")
		return current != nil && current != oldSession
	})

	select {
	case <-oldSession.done:
	case <-time.After(2 * time.Second):
		t.Fatal("old session was not shut down")
	}

	stream.closeWith(io.EOF)
	if err := <-done; err != nil {
		t.Fatalf("Connect returned error: %v", err)
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

type fakeConnectStream struct {
	ctx      context.Context
	recv     chan *tunnelpb.ClientFrame
	recvErr  chan error
	sent     []*tunnelpb.ServerFrame
	trailer  metadata.MD
	header   metadata.MD
	sentHead metadata.MD
}

func newFakeConnectStream(frames ...*tunnelpb.ClientFrame) *fakeConnectStream {
	stream := &fakeConnectStream{
		ctx:     context.Background(),
		recv:    make(chan *tunnelpb.ClientFrame, len(frames)),
		recvErr: make(chan error, 1),
	}
	for _, frame := range frames {
		stream.recv <- frame
	}
	return stream
}

func (s *fakeConnectStream) closeWith(err error) {
	s.recvErr <- err
}

func (s *fakeConnectStream) SetHeader(md metadata.MD) error {
	s.header = md
	return nil
}

func (s *fakeConnectStream) SendHeader(md metadata.MD) error {
	s.sentHead = md
	return nil
}

func (s *fakeConnectStream) SetTrailer(md metadata.MD) {
	s.trailer = md
}

func (s *fakeConnectStream) Context() context.Context {
	return s.ctx
}

func (s *fakeConnectStream) SendMsg(any) error {
	return nil
}

func (s *fakeConnectStream) RecvMsg(any) error {
	return nil
}

func (s *fakeConnectStream) Send(frame *tunnelpb.ServerFrame) error {
	s.sent = append(s.sent, frame)
	return nil
}

func (s *fakeConnectStream) Recv() (*tunnelpb.ClientFrame, error) {
	select {
	case frame := <-s.recv:
		if frame != nil {
			return frame, nil
		}
	case err := <-s.recvErr:
		return nil, err
	}
	return nil, io.EOF
}
