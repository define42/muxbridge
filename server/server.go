package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/define42/muxbridge/gen/tunnelpb"
	"github.com/define42/muxbridge/internal/headers"
	"github.com/define42/muxbridge/internal/hostnames"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var (
	errSessionClosed   = errors.New("tunnel session closed")
	errSessionReplaced = errors.New("tunnel session replaced by a newer connection")
)

// wsWriteTimeout bounds how long a hijacked WebSocket write from the edge to
// the browser may block. If exceeded the connection is torn down, so that a
// slow browser cannot stall delivery of frames for other requests sharing
// the same tunnel session.
const wsWriteTimeout = 30 * time.Second

type Config struct {
	PublicDomain string
	TokenUsers   map[string]string
	// HeartbeatInterval controls how often the edge sends Ping frames on an
	// otherwise idle tunnel session. The client answers with Pong, which keeps
	// the stream active through load balancers and other idle-connection
	// middleboxes. Defaults to 10 seconds when not set.
	HeartbeatInterval time.Duration
}

type Server struct {
	tunnelpb.UnimplementedTunnelServiceServer

	publicDomain string
	tokenUsers   map[string]string
	publicHosts  map[string]struct{}
	heartbeat    time.Duration

	mu            sync.RWMutex
	sessions      map[string]*session
	nextRequestID atomic.Uint64
}

type session struct {
	publicHost string
	username   string
	outbound   chan *tunnelpb.ServerFrame
	done       chan struct{}
	once       sync.Once

	mu       sync.Mutex
	inflight map[string]*responseState
	wsMap    map[string]*wsState
}

type wsState struct {
	fromClient chan []byte
	done       chan struct{}
	once       sync.Once
}

func (ws *wsState) close() {
	ws.once.Do(func() { close(ws.done) })
}

type responseState struct {
	start chan *tunnelpb.ResponseStart
	body  chan []byte
	done  chan struct{}
	err   chan error
	once  sync.Once
}

func New(cfg Config) (*Server, error) {
	publicDomain := hostnames.NormalizeDomain(cfg.PublicDomain)
	if err := hostnames.ValidateDomain(publicDomain); err != nil {
		return nil, fmt.Errorf("invalid public domain: %w", err)
	}
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = 10 * time.Second
	}

	tokenUsers := make(map[string]string, len(cfg.TokenUsers))
	publicHosts := make(map[string]struct{}, len(cfg.TokenUsers))
	userToToken := make(map[string]string, len(cfg.TokenUsers))

	for token, username := range cfg.TokenUsers {
		if token == "" {
			return nil, errors.New("token must not be empty")
		}

		username = hostnames.NormalizeLabel(username)
		if err := hostnames.ValidateLabel(username); err != nil {
			return nil, fmt.Errorf("invalid username %q: %w", username, err)
		}
		if username == "edge" {
			return nil, errors.New(`username "edge" is reserved`)
		}
		if previousToken, ok := userToToken[username]; ok {
			return nil, fmt.Errorf("duplicate username %q for tokens %q and %q", username, previousToken, token)
		}

		tokenUsers[token] = username
		userToToken[username] = token
		publicHosts[hostnames.Subdomain(username, publicDomain)] = struct{}{}
	}

	return &Server{
		publicDomain: publicDomain,
		tokenUsers:   tokenUsers,
		publicHosts:  publicHosts,
		heartbeat:    cfg.HeartbeatInterval,
		sessions:     make(map[string]*session),
	}, nil
}

func (s *Server) HasPublicHost(host string) bool {
	host = hostnames.NormalizeHost(host)

	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.publicHosts[host]
	return ok
}

func (s *Server) PublicHosts() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]string, 0, len(s.publicHosts))
	for host := range s.publicHosts {
		out = append(out, host)
	}
	sort.Strings(out)
	return out
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := hostnames.NormalizeHost(r.Host)
	if !s.HasPublicHost(host) {
		http.NotFound(w, r)
		return
	}

	sess := s.getSession(host)
	if sess == nil {
		http.Error(w, "no connected tunnel for host", http.StatusBadGateway)
		return
	}

	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		s.serveWebSocket(w, r, sess)
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
		// The client finished without sending ResponseStart. If an error was
		// reported, surface it; otherwise treat the empty response as a 502
		// so the browser doesn't see a bogus 200.
		if err := state.consumeErr(); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		http.Error(w, "tunnel closed response before sending headers", http.StatusBadGateway)
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
				if _, err := w.Write(chunk); err != nil {
					s.cancelRequest(sess, requestID)
					return
				}
				if flusher != nil {
					flusher.Flush()
				}
			}
		case <-state.done:
			if err := state.consumeErr(); err != nil {
				// Headers have already been written; the body is truncated.
				// Log so operators can diagnose and stop reading to signal
				// failure downstream.
				log.Printf("muxbridge: request %s failed after headers: %v", requestID, err)
			}
			return
		case <-r.Context().Done():
			s.cancelRequest(sess, requestID)
			return
		}
	}
}

func (s *Server) serveWebSocket(w http.ResponseWriter, r *http.Request, sess *session) {
	requestID := strconv.FormatUint(s.nextRequestID.Add(1), 10)

	state := newResponseState()
	sess.put(requestID, state)
	defer sess.del(requestID)

	ws := &wsState{
		fromClient: make(chan []byte, 16),
		done:       make(chan struct{}),
	}
	sess.putWS(requestID, ws)
	defer sess.delWS(requestID)

	if err := s.forwardRequest(r, sess, requestID); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, r.Context().Err()) {
			return
		}
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	var responseStart *tunnelpb.ResponseStart
	select {
	case responseStart = <-state.start:
	case err := <-state.err:
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	case <-state.done:
		http.Error(w, "tunnel closed connection", http.StatusBadGateway)
		return
	case <-sess.done:
		http.Error(w, "session closed", http.StatusBadGateway)
		return
	case <-r.Context().Done():
		s.cancelRequest(sess, requestID)
		return
	}

	if responseStart.StatusCode != http.StatusSwitchingProtocols {
		headers.Copy(w.Header(), headers.FromProto(responseStart.Headers))
		w.WriteHeader(int(responseStart.StatusCode))
		return
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "WebSocket requires HTTP/1.1", http.StatusHTTPVersionNotSupported)
		return
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		return
	}
	defer func() {
		_ = conn.Close()
	}()
	defer ws.close()

	if _, err := io.WriteString(brw, "HTTP/1.1 101 Switching Protocols\r\n"); err != nil {
		return
	}
	for k, vs := range headers.FromProto(responseStart.Headers) {
		for _, v := range vs {
			if _, err := fmt.Fprintf(brw, "%s: %s\r\n", k, v); err != nil {
				return
			}
		}
	}
	if _, err := io.WriteString(brw, "\r\n"); err != nil {
		return
	}
	if err := brw.Flush(); err != nil {
		return
	}

	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		// Drain any bytes already buffered from the browser before the hijack.
		if brw.Reader.Buffered() > 0 {
			buffered := make([]byte, brw.Reader.Buffered())
			_, _ = io.ReadFull(brw.Reader, buffered)
			_ = s.sendFrame(sess, &tunnelpb.ServerFrame{
				Msg: &tunnelpb.ServerFrame_WsData{WsData: &tunnelpb.WebSocketData{
					RequestId: requestID, Payload: buffered,
				}},
			})
		}
		buf := make([]byte, 32*1024)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				payload := make([]byte, n)
				copy(payload, buf[:n])
				if sendErr := s.sendFrame(sess, &tunnelpb.ServerFrame{
					Msg: &tunnelpb.ServerFrame_WsData{WsData: &tunnelpb.WebSocketData{
						RequestId: requestID, Payload: payload,
					}},
				}); sendErr != nil {
					return
				}
			}
			if err != nil {
				_ = s.sendFrame(sess, &tunnelpb.ServerFrame{
					Msg: &tunnelpb.ServerFrame_WsClose{WsClose: &tunnelpb.WebSocketClose{RequestId: requestID}},
				})
				return
			}
		}
	}()

	for {
		select {
		case data := <-ws.fromClient:
			// Bound the time a slow browser can block this write so a stalled
			// WebSocket does not hold up the hijacked connection indefinitely.
			// If the deadline cannot be set (e.g. the connection is already
			// closed) or the write fails, we tear down; that closes ws.done
			// and unblocks the session-level receiver, avoiding head-of-line
			// blocking across other multiplexed requests on the same tunnel.
			if err := conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout)); err != nil {
				return
			}
			if _, err := conn.Write(data); err != nil {
				return
			}
			if err := conn.SetWriteDeadline(time.Time{}); err != nil {
				return
			}
		case <-ws.done:
			return
		case <-readDone:
			return
		case <-sess.done:
			return
		case <-r.Context().Done():
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
	if reg == nil {
		return status.Error(codes.InvalidArgument, "first frame must be a register frame")
	}
	if reg.Token == "" {
		return status.Error(codes.Unauthenticated, "missing client token")
	}

	username, ok := s.tokenUsers[reg.Token]
	if !ok {
		return status.Error(codes.Unauthenticated, "invalid client token")
	}

	publicHost := hostnames.Subdomain(username, s.publicDomain)
	sess := &session{
		publicHost: publicHost,
		username:   username,
		outbound:   make(chan *tunnelpb.ServerFrame, 64),
		done:       make(chan struct{}),
		inflight:   make(map[string]*responseState),
		wsMap:      make(map[string]*wsState),
	}
	s.putSession(sess)
	defer s.removeSession(sess.publicHost, sess)

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
	go func() {
		ticker := time.NewTicker(s.heartbeat)
		defer ticker.Stop()
		for {
			select {
			case <-stream.Context().Done():
				return
			case <-sess.done:
				return
			case <-ticker.C:
				// Heartbeats are best-effort. If the outbound queue is already
				// busy with real traffic, skip this tick; active traffic keeps
				// the session fresh anyway.
				select {
				case <-stream.Context().Done():
					return
				case <-sess.done:
					return
				case sess.outbound <- &tunnelpb.ServerFrame{
					Msg: &tunnelpb.ServerFrame_Ping{Ping: &tunnelpb.Ping{UnixNano: time.Now().UnixNano()}},
				}:
				default:
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
	ctx := r.Context()
	if err := s.sendFrameCtx(ctx, sess, &tunnelpb.ServerFrame{
		Msg: &tunnelpb.ServerFrame_RequestStart{RequestStart: &tunnelpb.RequestStart{
			RequestId:  requestID,
			Method:     r.Method,
			Scheme:     schemeOf(r),
			Host:       r.Host,
			Path:       r.URL.Path,
			RawPath:    r.URL.RawPath,
			RawQuery:   r.URL.RawQuery,
			Headers:    headers.ToProto(r.Header),
			RemoteAddr: r.RemoteAddr,
		}},
	}); err != nil {
		return err
	}

	if r.Body != nil {
		defer func() {
			_ = r.Body.Close()
		}()

		buf := make([]byte, 32*1024)
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			n, err := r.Body.Read(buf)
			if n > 0 {
				chunk := append([]byte(nil), buf[:n]...)
				if sendErr := s.sendFrameCtx(ctx, sess, &tunnelpb.ServerFrame{
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

	return s.sendFrameCtx(ctx, sess, &tunnelpb.ServerFrame{
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
	case *tunnelpb.ClientFrame_WsData:
		if ws := sess.getWS(msg.WsData.GetRequestId()); ws != nil {
			payload := append([]byte(nil), msg.WsData.GetPayload()...)
			select {
			case ws.fromClient <- payload:
			case <-ws.done:
			}
		}
	case *tunnelpb.ClientFrame_WsClose:
		if ws := sess.getWS(msg.WsClose.GetRequestId()); ws != nil {
			ws.close()
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

// sendFrameCtx enqueues a frame like sendFrame but also aborts when the
// provided context is canceled. This prevents handlers from stalling on a
// full outbound queue after their HTTP request has been aborted.
func (s *Server) sendFrameCtx(ctx context.Context, sess *session, frame *tunnelpb.ServerFrame) error {
	select {
	case <-sess.done:
		return errSessionClosed
	case <-ctx.Done():
		return ctx.Err()
	case sess.outbound <- frame:
		return nil
	}
}

func (s *Server) cancelRequest(sess *session, requestID string) {
	frame := &tunnelpb.ServerFrame{
		Msg: &tunnelpb.ServerFrame_CancelRequest{CancelRequest: &tunnelpb.CancelRequest{RequestId: requestID}},
	}
	// Best-effort: if the session is already closed or its outbound queue is
	// full, drop the cancel frame. Blocking here would prevent the caller
	// (typically an aborting HTTP handler) from making forward progress.
	select {
	case <-sess.done:
	case sess.outbound <- frame:
	default:
	}
}

func schemeOf(r *http.Request) string {
	if r.TLS != nil {
		return "https"
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
	var replaced *session

	s.mu.Lock()
	replaced = s.sessions[sess.publicHost]
	s.sessions[sess.publicHost] = sess
	s.mu.Unlock()

	if replaced != nil && replaced != sess {
		replaced.shutdown(errSessionReplaced)
	}
}

func (s *Server) getSession(publicHost string) *session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sessions[publicHost]
}

func (s *Server) removeSession(publicHost string, current *session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions[publicHost] == current {
		delete(s.sessions, publicHost)
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

func (s *session) putWS(id string, ws *wsState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.wsMap[id] = ws
}

func (s *session) getWS(id string) *wsState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.wsMap[id]
}

func (s *session) delWS(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.wsMap, id)
}

func (s *session) failAll(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, rs := range s.inflight {
		rs.fail(err)
		delete(s.inflight, id)
	}
	for id, ws := range s.wsMap {
		ws.close()
		delete(s.wsMap, id)
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

// consumeErr returns any error delivered via fail() without blocking.
// It must only be called after done has been observed closed.
func (r *responseState) consumeErr() error {
	select {
	case err := <-r.err:
		return err
	default:
		return nil
	}
}

var _ http.Handler = (*Server)(nil)
var _ interface {
	Connect(tunnelpb.TunnelService_ConnectServer) error
} = (*Server)(nil)
