package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/define42/muxbridge/gen/tunnelpb"
	"github.com/define42/muxbridge/internal/headers"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var errSessionClosed = errors.New("tunnel session closed")

type Server struct {
	tunnelpb.UnimplementedTunnelServiceServer

	mu            sync.RWMutex
	sessions      map[string]*session
	nextRequestID atomic.Uint64
}

type session struct {
	tunnelID string
	outbound chan *tunnelpb.ServerFrame
	done     chan struct{}
	once     sync.Once

	mu       sync.Mutex
	inflight map[string]*responseState
}

type responseState struct {
	start chan *tunnelpb.ResponseStart
	body  chan []byte
	done  chan struct{}
	err   chan error
	once  sync.Once
}

func New() *Server {
	return &Server{
		sessions: make(map[string]*session),
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sess := s.getSession(r.Host)
	if sess == nil {
		http.Error(w, "no connected tunnel for host", http.StatusBadGateway)
		return
	}

	requestID := strconv.FormatUint(s.nextRequestID.Add(1), 10)
	state := newResponseState()
	sess.put(requestID, state)
	defer sess.del(requestID)

	if err := s.forwardRequest(r, sess, requestID); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, r.Context().Err()) {
			return
		}
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	select {
	case start := <-state.start:
		headers.Copy(w.Header(), headers.FromProto(start.Headers))
		w.WriteHeader(int(start.StatusCode))
	case err := <-state.err:
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	case <-state.done:
		w.WriteHeader(http.StatusOK)
		return
	case <-r.Context().Done():
		s.cancelRequest(sess, requestID)
		return
	}

	flusher, _ := w.(http.Flusher)
	for {
		select {
		case chunk := <-state.body:
			if len(chunk) > 0 {
				_, _ = w.Write(chunk)
				if flusher != nil {
					flusher.Flush()
				}
			}
		case <-state.done:
			return
		case err := <-state.err:
			_ = err
			return
		case <-r.Context().Done():
			s.cancelRequest(sess, requestID)
			return
		}
	}
}

func (s *Server) Connect(stream tunnelpb.TunnelService_ConnectServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}

	reg := first.GetRegister()
	if reg == nil || reg.TunnelId == "" {
		return status.Error(codes.InvalidArgument, "first frame must be a register frame with tunnel_id")
	}

	sess := &session{
		tunnelID: reg.TunnelId,
		outbound: make(chan *tunnelpb.ServerFrame, 64),
		done:     make(chan struct{}),
		inflight: make(map[string]*responseState),
	}
	s.putSession(sess)
	defer s.removeSession(sess.tunnelID, sess)

	sendErr := make(chan error, 1)
	go func() {
		for {
			select {
			case <-stream.Context().Done():
				sendErr <- stream.Context().Err()
				return
			case <-sess.done:
				sendErr <- nil
				return
			case frame := <-sess.outbound:
				if err := stream.Send(frame); err != nil {
					sendErr <- err
					return
				}
			}
		}
	}()

	var recvErr error
	for {
		frame, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				recvErr = nil
			} else {
				recvErr = err
			}
			break
		}
		s.handleClientFrame(sess, frame)
	}

	shutdownErr := errSessionClosed
	if recvErr != nil {
		shutdownErr = recvErr
	}
	sess.shutdown(shutdownErr)

	if err := <-sendErr; err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return recvErr
}

func (s *Server) forwardRequest(r *http.Request, sess *session, requestID string) error {
	if err := s.sendFrame(sess, &tunnelpb.ServerFrame{
		Msg: &tunnelpb.ServerFrame_RequestStart{RequestStart: &tunnelpb.RequestStart{
			RequestId:  requestID,
			Method:     r.Method,
			Scheme:     schemeOf(r),
			Host:       r.Host,
			Path:       r.URL.Path,
			RawQuery:   r.URL.RawQuery,
			Headers:    headers.ToProto(r.Header),
			RemoteAddr: r.RemoteAddr,
		}},
	}); err != nil {
		return err
	}

	if r.Body != nil {
		defer r.Body.Close()

		buf := make([]byte, 32*1024)
		for {
			n, err := r.Body.Read(buf)
			if n > 0 {
				chunk := append([]byte(nil), buf[:n]...)
				if sendErr := s.sendFrame(sess, &tunnelpb.ServerFrame{
					Msg: &tunnelpb.ServerFrame_RequestBody{RequestBody: &tunnelpb.RequestBody{
						RequestId: requestID,
						Chunk:     chunk,
					}},
				}); sendErr != nil {
					return sendErr
				}
			}

			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return err
			}
		}
	}

	return s.sendFrame(sess, &tunnelpb.ServerFrame{
		Msg: &tunnelpb.ServerFrame_RequestEnd{RequestEnd: &tunnelpb.RequestEnd{RequestId: requestID}},
	})
}

func (s *Server) handleClientFrame(sess *session, frame *tunnelpb.ClientFrame) {
	switch msg := frame.GetMsg().(type) {
	case *tunnelpb.ClientFrame_ResponseStart:
		if rs := sess.get(msg.ResponseStart.GetRequestId()); rs != nil {
			select {
			case rs.start <- msg.ResponseStart:
			case <-rs.done:
			}
		}
	case *tunnelpb.ClientFrame_ResponseBody:
		if rs := sess.get(msg.ResponseBody.GetRequestId()); rs != nil {
			chunk := append([]byte(nil), msg.ResponseBody.GetChunk()...)
			select {
			case rs.body <- chunk:
			case <-rs.done:
			}
		}
	case *tunnelpb.ClientFrame_ResponseEnd:
		if rs := sess.get(msg.ResponseEnd.GetRequestId()); rs != nil {
			rs.finish()
		}
	case *tunnelpb.ClientFrame_ResponseError:
		if rs := sess.get(msg.ResponseError.GetRequestId()); rs != nil {
			rs.fail(errors.New(msg.ResponseError.GetMessage()))
		}
	case *tunnelpb.ClientFrame_Pong:
		// No-op for now; the ping/pong frames keep the stream shape future-friendly.
	case *tunnelpb.ClientFrame_Register:
		// Duplicate register frames are ignored after session setup.
	}
}

func (s *Server) sendFrame(sess *session, frame *tunnelpb.ServerFrame) error {
	select {
	case <-sess.done:
		return errSessionClosed
	case sess.outbound <- frame:
		return nil
	}
}

func (s *Server) cancelRequest(sess *session, requestID string) {
	_ = s.sendFrame(sess, &tunnelpb.ServerFrame{
		Msg: &tunnelpb.ServerFrame_CancelRequest{CancelRequest: &tunnelpb.CancelRequest{RequestId: requestID}},
	})
}

func schemeOf(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		return proto
	}
	return "http"
}

func newResponseState() *responseState {
	return &responseState{
		start: make(chan *tunnelpb.ResponseStart, 1),
		body:  make(chan []byte, 16),
		done:  make(chan struct{}),
		err:   make(chan error, 1),
	}
}

func (s *Server) putSession(sess *session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[sess.tunnelID] = sess
}

func (s *Server) getSession(tunnelID string) *session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sessions[tunnelID]
}

func (s *Server) removeSession(tunnelID string, current *session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions[tunnelID] == current {
		delete(s.sessions, tunnelID)
	}
}

func (s *session) put(id string, rs *responseState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inflight[id] = rs
}

func (s *session) get(id string) *responseState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inflight[id]
}

func (s *session) del(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.inflight, id)
}

func (s *session) failAll(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, rs := range s.inflight {
		rs.fail(err)
		delete(s.inflight, id)
	}
}

func (s *session) shutdown(err error) {
	s.once.Do(func() {
		close(s.done)
		s.failAll(err)
	})
}

func (r *responseState) finish() {
	r.once.Do(func() {
		close(r.done)
	})
}

func (r *responseState) fail(err error) {
	r.once.Do(func() {
		if err != nil {
			select {
			case r.err <- err:
			default:
			}
		}
		close(r.done)
	})
}

var _ http.Handler = (*Server)(nil)
var _ interface{ Connect(tunnelpb.TunnelService_ConnectServer) error } = (*Server)(nil)
