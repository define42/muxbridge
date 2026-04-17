package tunnel

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/define42/muxbridge/gen/tunnelpb"
	"github.com/define42/muxbridge/internal/headers"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

var errSessionEnded = errors.New("tunnel session ended")

// ioBufSize is the chunk size used when reading request/response bodies and
// WebSocket frames. 32 KiB matches the default bufio reader size and
// balances throughput against per-frame overhead.
const ioBufSize = 32 * 1024

// wsShutdownTimeout bounds how long the embedded http.Server spun up for a
// WebSocket upgrade is given to drain during teardown. It is deliberately
// short so a misbehaving handler cannot stall session cleanup.
const wsShutdownTimeout = 5 * time.Second

type Config struct {
	EdgeAddr         string
	TunnelID         string
	Token            string
	Handler          http.Handler
	Insecure         bool
	ReconnectBackoff time.Duration
	// DialTimeout bounds how long a single connect attempt may block before
	// the reconnect loop gets a chance to try again. Defaults to 10 seconds
	// when not set.
	DialTimeout time.Duration
	Logger      *log.Logger
}

type Client struct {
	cfg     Config
	handler http.Handler
	logger  *log.Logger

	mu      sync.Mutex
	closed  bool
	conn    *grpc.ClientConn
	closeCh chan struct{}
}

type inflightRequest struct {
	ctx    context.Context
	cancel context.CancelFunc
	bodyR  *io.PipeReader
	bodyW  *io.PipeWriter
}

type oneConnListener struct {
	ch   chan net.Conn
	addr net.Addr
	once sync.Once
}

func newOneConnListener(conn net.Conn) *oneConnListener {
	l := &oneConnListener{ch: make(chan net.Conn, 1), addr: conn.LocalAddr()}
	l.ch <- conn
	return l
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	conn, ok := <-l.ch
	if !ok {
		return nil, net.ErrClosed
	}
	return conn, nil
}

func (l *oneConnListener) Close() error {
	l.once.Do(func() { close(l.ch) })
	return nil
}

func (l *oneConnListener) Addr() net.Addr { return l.addr }

func isWebSocketUpgrade(start *tunnelpb.RequestStart) bool {
	for _, h := range start.GetHeaders() {
		if strings.EqualFold(h.GetKey(), "Upgrade") {
			for _, v := range h.GetValues() {
				if strings.EqualFold(v, "websocket") {
					return true
				}
			}
		}
	}
	return false
}

func New(cfg Config) (*Client, error) {
	if cfg.EdgeAddr == "" {
		return nil, errors.New("edge address is required")
	}
	if cfg.Handler == nil {
		return nil, errors.New("handler is required")
	}
	if cfg.Token == "" {
		return nil, errors.New("token is required")
	}
	if cfg.ReconnectBackoff <= 0 {
		cfg.ReconnectBackoff = 2 * time.Second
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = 10 * time.Second
	}
	return &Client{
		cfg:     cfg,
		handler: cfg.Handler,
		logger:  cfg.Logger,
		closeCh: make(chan struct{}),
	}, nil
}

func (c *Client) Run(ctx context.Context) error {
	c.mu.Lock()
	if c.closeCh == nil {
		c.closeCh = make(chan struct{})
	}
	closeCh := c.closeCh
	c.mu.Unlock()

	for {
		err := c.runSession(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if c.isClosed() {
			return nil
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			c.logf("tunnel session ended: %v", err)
		}

		timer := time.NewTimer(c.cfg.ReconnectBackoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-closeCh:
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

func (c *Client) logf(format string, args ...any) {
	if c.logger != nil {
		c.logger.Printf(format, args...)
	}
}

func (c *Client) Close() error {
	c.mu.Lock()
	alreadyClosed := c.closed
	c.closed = true
	conn := c.conn
	c.conn = nil
	closeCh := c.closeCh
	c.mu.Unlock()

	if !alreadyClosed && closeCh != nil {
		close(closeCh)
	}

	if conn != nil {
		return conn.Close()
	}
	return nil
}

func (c *Client) runSession(ctx context.Context) error {
	dialOpts := []grpc.DialOption{grpc.WithBlock()}
	if c.cfg.Insecure {
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	} else {
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{})))
	}

	// Bound the dial so that an unreachable edge does not trap the reconnect
	// loop indefinitely — the parent context may still be long-lived.
	dialCtx, dialCancel := context.WithTimeout(ctx, c.cfg.DialTimeout)
	defer dialCancel()
	conn, err := grpc.DialContext(dialCtx, c.cfg.EdgeAddr, dialOpts...)
	if err != nil {
		return err
	}
	defer func() {
		_ = conn.Close()
	}()

	c.setConn(conn)
	defer c.clearConn(conn)

	stream, err := tunnelpb.NewTunnelServiceClient(conn).Connect(ctx)
	if err != nil {
		return err
	}

	var sendMu sync.Mutex
	send := func(frame *tunnelpb.ClientFrame) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		return stream.Send(frame)
	}

	if err := send(&tunnelpb.ClientFrame{
		Msg: &tunnelpb.ClientFrame_Register{Register: &tunnelpb.Register{
			TunnelId: c.cfg.TunnelID,
			Token:    c.cfg.Token,
		}},
	}); err != nil {
		return err
	}

	var inflightMu sync.Mutex
	inflight := make(map[string]*inflightRequest)
	wsInbound := make(map[string]chan []byte)

	getRequest := func(id string) *inflightRequest {
		inflightMu.Lock()
		defer inflightMu.Unlock()
		return inflight[id]
	}
	putRequest := func(id string, req *inflightRequest) {
		inflightMu.Lock()
		defer inflightMu.Unlock()
		inflight[id] = req
	}
	deleteRequest := func(id string) *inflightRequest {
		inflightMu.Lock()
		defer inflightMu.Unlock()
		req := inflight[id]
		delete(inflight, id)
		return req
	}
	getWSInbound := func(id string) chan []byte {
		inflightMu.Lock()
		defer inflightMu.Unlock()
		return wsInbound[id]
	}
	putWSInbound := func(id string, ch chan []byte) {
		inflightMu.Lock()
		defer inflightMu.Unlock()
		wsInbound[id] = ch
	}
	deleteWSInbound := func(id string) {
		inflightMu.Lock()
		defer inflightMu.Unlock()
		delete(wsInbound, id)
	}
	defer func() {
		inflightMu.Lock()
		for id, req := range inflight {
			req.cancel()
			_ = req.bodyW.CloseWithError(errSessionEnded)
			_ = req.bodyR.CloseWithError(errSessionEnded)
			delete(inflight, id)
		}
		for _, ch := range wsInbound {
			close(ch)
		}
		inflightMu.Unlock()
	}()

	for {
		frame, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}

		switch msg := frame.GetMsg().(type) {
		case *tunnelpb.ServerFrame_RequestStart:
			start := msg.RequestStart

			if isWebSocketUpgrade(start) {
				inbound := make(chan []byte, 16)
				putWSInbound(start.GetRequestId(), inbound)
				go c.handleWebSocket(ctx, start, inbound, send, func() {
					deleteWSInbound(start.GetRequestId())
				})
				continue
			}

			bodyR, bodyW := io.Pipe()
			reqCtx, cancel := context.WithCancel(ctx)

			req, reqErr := http.NewRequestWithContext(reqCtx, start.GetMethod(), requestURL(start), bodyR)
			if reqErr != nil {
				cancel()
				_ = bodyR.CloseWithError(reqErr)
				_ = bodyW.CloseWithError(reqErr)
				_ = send(&tunnelpb.ClientFrame{
					Msg: &tunnelpb.ClientFrame_ResponseError{ResponseError: &tunnelpb.ResponseError{
						RequestId: start.GetRequestId(),
						Message:   reqErr.Error(),
					}},
				})
				continue
			}

			req.Header = headers.FromProto(start.GetHeaders())
			req.Host = start.GetHost()
			req.RemoteAddr = start.GetRemoteAddr()

			inflightReq := &inflightRequest{
				ctx:    reqCtx,
				cancel: cancel,
				bodyR:  bodyR,
				bodyW:  bodyW,
			}
			putRequest(start.GetRequestId(), inflightReq)

			go c.serveRequest(req, start.GetRequestId(), inflightReq, send, func() {
				deleteRequest(start.GetRequestId())
			})

		case *tunnelpb.ServerFrame_RequestBody:
			req := getRequest(msg.RequestBody.GetRequestId())
			if req == nil {
				continue
			}
			if _, writeErr := req.bodyW.Write(msg.RequestBody.GetChunk()); writeErr != nil {
				c.logf("body write failed for request %s: %v", msg.RequestBody.GetRequestId(), writeErr)
			}

		case *tunnelpb.ServerFrame_RequestEnd:
			req := getRequest(msg.RequestEnd.GetRequestId())
			if req == nil {
				continue
			}
			_ = req.bodyW.Close()

		case *tunnelpb.ServerFrame_CancelRequest:
			req := getRequest(msg.CancelRequest.GetRequestId())
			if req == nil {
				continue
			}
			req.cancel()
			_ = req.bodyW.CloseWithError(context.Canceled)

		case *tunnelpb.ServerFrame_WsData:
			if ch := getWSInbound(msg.WsData.GetRequestId()); ch != nil {
				payload := append([]byte(nil), msg.WsData.GetPayload()...)
				select {
				case ch <- payload:
				case <-ctx.Done():
					return ctx.Err()
				}
			}

		case *tunnelpb.ServerFrame_WsClose:
			if ch := getWSInbound(msg.WsClose.GetRequestId()); ch != nil {
				deleteWSInbound(msg.WsClose.GetRequestId())
				close(ch)
			}

		case *tunnelpb.ServerFrame_Ping:
			if err := send(&tunnelpb.ClientFrame{
				Msg: &tunnelpb.ClientFrame_Pong{Pong: &tunnelpb.Pong{UnixNano: msg.Ping.GetUnixNano()}},
			}); err != nil {
				return err
			}
		}
	}
}

func (c *Client) handleWebSocket(
	ctx context.Context,
	start *tunnelpb.RequestStart,
	inbound <-chan []byte,
	send func(*tunnelpb.ClientFrame) error,
	done func(),
) {
	defer done()

	requestID := start.GetRequestId()

	clientConn, serverConn := net.Pipe()
	defer func() {
		_ = clientConn.Close()
	}()

	listener := newOneConnListener(serverConn)
	srv := &http.Server{Handler: c.handler}
	go srv.Serve(listener) //nolint:errcheck
	defer func() {
		_ = listener.Close()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), wsShutdownTimeout)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	req, err := http.NewRequest(start.GetMethod(), requestURL(start), http.NoBody)
	if err != nil {
		_ = send(&tunnelpb.ClientFrame{
			Msg: &tunnelpb.ClientFrame_ResponseError{ResponseError: &tunnelpb.ResponseError{
				RequestId: requestID, Message: err.Error(),
			}},
		})
		return
	}
	req.Header = headers.FromProto(start.GetHeaders())
	req.Host = start.GetHost()

	if err := req.Write(clientConn); err != nil {
		_ = send(&tunnelpb.ClientFrame{
			Msg: &tunnelpb.ClientFrame_ResponseError{ResponseError: &tunnelpb.ResponseError{
				RequestId: requestID, Message: err.Error(),
			}},
		})
		return
	}

	br := bufio.NewReader(clientConn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		_ = send(&tunnelpb.ClientFrame{
			Msg: &tunnelpb.ClientFrame_ResponseError{ResponseError: &tunnelpb.ResponseError{
				RequestId: requestID, Message: err.Error(),
			}},
		})
		return
	}

	if err := send(&tunnelpb.ClientFrame{
		Msg: &tunnelpb.ClientFrame_ResponseStart{ResponseStart: &tunnelpb.ResponseStart{
			RequestId:  requestID,
			StatusCode: int32(resp.StatusCode),
			Headers:    headers.ToProto(resp.Header),
		}},
	}); err != nil {
		return
	}

	if resp.StatusCode != http.StatusSwitchingProtocols {
		// The handler refused the upgrade. Forward its response body so the
		// browser sees the reason (e.g., a rendered 403/404 page) instead of
		// an empty body, then close out the request cleanly.
		if resp.Body != nil {
			buf := make([]byte, ioBufSize)
			for {
				n, readErr := resp.Body.Read(buf)
				if n > 0 {
					payload := make([]byte, n)
					copy(payload, buf[:n])
					if sendErr := send(&tunnelpb.ClientFrame{
						Msg: &tunnelpb.ClientFrame_ResponseBody{ResponseBody: &tunnelpb.ResponseBody{
							RequestId: requestID, Chunk: payload,
						}},
					}); sendErr != nil {
						_ = resp.Body.Close()
						return
					}
				}
				if readErr != nil {
					break
				}
			}
			_ = resp.Body.Close()
		}
		_ = send(&tunnelpb.ClientFrame{
			Msg: &tunnelpb.ClientFrame_ResponseEnd{ResponseEnd: &tunnelpb.ResponseEnd{
				RequestId: requestID,
			}},
		})
		return
	}

	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buf := make([]byte, ioBufSize)
		for {
			n, err := br.Read(buf)
			if n > 0 {
				payload := make([]byte, n)
				copy(payload, buf[:n])
				if sendErr := send(&tunnelpb.ClientFrame{
					Msg: &tunnelpb.ClientFrame_WsData{WsData: &tunnelpb.WebSocketData{
						RequestId: requestID, Payload: payload,
					}},
				}); sendErr != nil {
					return
				}
			}
			if err != nil {
				_ = send(&tunnelpb.ClientFrame{
					Msg: &tunnelpb.ClientFrame_WsClose{WsClose: &tunnelpb.WebSocketClose{RequestId: requestID}},
				})
				return
			}
		}
	}()

	for {
		select {
		case payload, ok := <-inbound:
			if !ok {
				_ = clientConn.Close()
				<-readDone
				return
			}
			if _, err := clientConn.Write(payload); err != nil {
				return
			}
		case <-readDone:
			return
		case <-ctx.Done():
			return
		}
	}
}

func (c *Client) serveRequest(
	req *http.Request,
	requestID string,
	inflight *inflightRequest,
	send func(*tunnelpb.ClientFrame) error,
	done func(),
) {
	defer done()
	defer inflight.cancel()
	defer func() {
		_ = inflight.bodyR.Close()
	}()

	w := newTunnelResponseWriter(requestID, send)
	defer func() {
		if recovered := recover(); recovered != nil {
			_ = w.Fail(fmt.Errorf("panic while serving request: %v", recovered))
			return
		}
		_ = w.Finish()
	}()

	c.handler.ServeHTTP(w, req)
}

func requestURL(start *tunnelpb.RequestStart) string {
	path := start.GetPath()
	if path == "" {
		path = "/"
	}
	u := url.URL{
		Scheme:   start.GetScheme(),
		Host:     start.GetHost(),
		Path:     path,
		RawPath:  start.GetRawPath(),
		RawQuery: start.GetRawQuery(),
	}
	return u.String()
}

func (c *Client) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

func (c *Client) setConn(conn *grpc.ClientConn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		_ = conn.Close()
		return
	}
	c.conn = conn
}

func (c *Client) clearConn(conn *grpc.ClientConn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == conn {
		c.conn = nil
	}
}
