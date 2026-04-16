package tunnel

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/define42/muxbridge/gen/tunnelpb"
	"github.com/define42/muxbridge/internal/headers"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

type Config struct {
	EdgeAddr         string
	TunnelID         string
	Token            string
	Handler          http.Handler
	Insecure         bool
	ReconnectBackoff time.Duration
	Logger           *log.Logger
}

type Client struct {
	cfg     Config
	handler http.Handler
	logger  *log.Logger

	mu     sync.Mutex
	closed bool
	conn   *grpc.ClientConn
}

type inflightRequest struct {
	ctx    context.Context
	cancel context.CancelFunc
	bodyR  *io.PipeReader
	bodyW  *io.PipeWriter
}

func New(cfg Config) (*Client, error) {
	if cfg.EdgeAddr == "" {
		return nil, errors.New("EdgeAddr is required")
	}
	if cfg.TunnelID == "" {
		return nil, errors.New("TunnelID is required")
	}
	if cfg.Handler == nil {
		return nil, errors.New("Handler is required")
	}
	if cfg.ReconnectBackoff <= 0 {
		cfg.ReconnectBackoff = 2 * time.Second
	}
	return &Client{
		cfg:     cfg,
		handler: cfg.Handler,
		logger:  cfg.Logger,
	}, nil
}

func (c *Client) Run(ctx context.Context) error {
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
	c.closed = true
	conn := c.conn
	c.conn = nil
	c.mu.Unlock()

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

	conn, err := grpc.DialContext(ctx, c.cfg.EdgeAddr, dialOpts...)
	if err != nil {
		return err
	}
	defer conn.Close()

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

		case *tunnelpb.ServerFrame_Ping:
			if err := send(&tunnelpb.ClientFrame{
				Msg: &tunnelpb.ClientFrame_Pong{Pong: &tunnelpb.Pong{UnixNano: msg.Ping.GetUnixNano()}},
			}); err != nil {
				return err
			}
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
	defer inflight.bodyR.Close()

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
