package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/caddyserver/certmagic"
	"github.com/define42/muxbridge/gen/tunnelpb"
	"github.com/define42/muxbridge/internal/hostnames"
	"github.com/define42/muxbridge/server"
	"golang.org/x/net/http2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
)

type edgeTLSAssets struct {
	tlsConfig   *tls.Config
	httpHandler http.Handler
	manage      func(context.Context) error
}

const (
	// maxGRPCMessageBytes caps the size of a single gRPC frame processed by
	// the edge's TunnelService. Individual request/response bodies are
	// streamed in 32 KiB chunks (see server.forwardRequest), so this
	// accommodates large headers and a safety margin without allowing an
	// abusive client to send arbitrarily large frames.
	maxGRPCMessageBytes = 4 * 1024 * 1024

	// gRPC keepalive tuning detects dead tunnels behind NATs while not being
	// so aggressive that well-behaved but quiet sessions get torn down.
	grpcServerKeepaliveTime    = 30 * time.Second
	grpcServerKeepaliveTimeout = 20 * time.Second
	grpcClientMinKeepaliveTime = 10 * time.Second
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, nil, getenv, nil, nil); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}

func newEdgeGRPCServer() *grpc.Server {
	return grpc.NewServer(
		grpc.MaxRecvMsgSize(maxGRPCMessageBytes),
		grpc.MaxSendMsgSize(maxGRPCMessageBytes),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    grpcServerKeepaliveTime,
			Timeout: grpcServerKeepaliveTimeout,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             grpcClientMinKeepaliveTime,
			PermitWithoutStream: true,
		}),
	)
}

func run(ctx context.Context, args []string, getenv func(string) string, httpListener, httpsListener net.Listener) error {
	cfg, err := loadConfig(args, getenv)
	if err != nil {
		return err
	}
	startedAt := time.Now()

	srv, err := server.New(server.Config{
		PublicDomain: cfg.PublicDomain,
		TokenUsers:   cfg.ClientCredentials,
		Logger:       log.Default(),
		Debug:        cfg.Debug,
	})
	if err != nil {
		return err
	}

	grpcServer := newEdgeGRPCServer()
	tunnelpb.RegisterTunnelServiceServer(grpcServer, srv)

	var pprofHandler http.Handler
	if cfg.Debug {
		pprofHandler = newPprofHandler()
	}

	httpsHandler := newHTTPSHandler(
		cfg.PublicDomain,
		cfg.EdgeDomain,
		srv.HasPublicHost,
		newStatusHandler(startedAt, time.Now),
		srv,
		grpcServer,
		pprofHandler,
	)
	tlsAssets, err := edgeTLSAssetsFor(cfg)
	if err != nil {
		return err
	}

	if httpListener == nil {
		httpListener, err = net.Listen("tcp", ":80")
		if err != nil {
			return err
		}
	}
	if httpsListener == nil {
		httpsListener, err = net.Listen("tcp", ":443")
		if err != nil {
			_ = httpListener.Close()
			return err
		}
	}

	httpServer := newRedirectHTTPServer(tlsAssets.httpHandler)
	httpsServer := newPublicHTTPSServer(httpsHandler, tlsAssets.tlsConfig)
	if err := http2.ConfigureServer(httpsServer, &http2.Server{}); err != nil {
		_ = httpListener.Close()
		_ = httpsListener.Close()
		return err
	}

	serveErr := make(chan error, 2)
	go serve("HTTP", httpServer.Serve, httpListener, serveErr)
	if err := tlsAssets.manage(ctx); err != nil {
		_ = httpsListener.Close()
		_ = shutdownServers(httpServer, nil)
		return err
	}
	go serve("HTTPS", httpsServer.Serve, tls.NewListener(httpsListener, tlsAssets.tlsConfig), serveErr)

	if cfg.Debug {
		log.Printf(
			"edge debug enabled: edge_domain=%s public_domain=%s public_hosts=%s",
			cfg.EdgeDomain,
			cfg.PublicDomain,
			strings.Join(srv.PublicHosts(), ", "),
		)
	}
	log.Printf("edge listening on https://%s and http://:80", cfg.EdgeDomain)
	select {
	case err := <-serveErr:
		_ = shutdownServers(httpServer, httpsServer)
		gracefulStopGRPC(grpcServer)
		return err
	case <-ctx.Done():
		_ = shutdownServers(httpServer, httpsServer)
		gracefulStopGRPC(grpcServer)
		return ctx.Err()
	}
}

func newRedirectHTTPServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
}

func newPublicHTTPSServer(handler http.Handler, tlsConfig *tls.Config) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		// Leave ReadTimeout and WriteTimeout unset here. The HTTPS listener
		// serves long-lived gRPC tunnel streams and proxied streaming
		// responses, and server-wide deadlines would tear those down even when
		// the session is healthy.
		IdleTimeout: 5 * time.Minute,
		TLSConfig:   tlsConfig,
	}
}

// gracefulStopGRPC drains in-flight streams with a timeout, then forces the
// server down. It is safe to call even if the server has already stopped.
func gracefulStopGRPC(s *grpc.Server) {
	if s == nil {
		return
	}
	done := make(chan struct{})
	go func() {
		s.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		s.Stop()
		<-done
	}
}

func edgeTLSConfig(ctx context.Context, cfg edgeConfig) (*tls.Config, http.Handler, error) {
	assets, err := edgeTLSAssetsFor(cfg)
	if err != nil {
		return nil, nil, err
	}
	if err := assets.manage(ctx); err != nil {
		return nil, nil, err
	}
	return assets.tlsConfig, assets.httpHandler, nil
}

func edgeTLSAssetsFor(cfg edgeConfig) (edgeTLSAssets, error) {
	if cfg.TLSCertFile != "" {
		tlsCert, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
		if err != nil {
			return edgeTLSAssets{}, fmt.Errorf("load static TLS certificate: %w", err)
		}

		log.Printf("using static TLS certificate from %s and %s", cfg.TLSCertFile, cfg.TLSKeyFile)
		return edgeTLSAssets{
			tlsConfig: &tls.Config{
				Certificates: []tls.Certificate{tlsCert},
				MinVersion:   tls.VersionTLS12,
				NextProtos:   []string{"h2", "http/1.1"},
			},
			httpHandler: http.HandlerFunc(redirectToHTTPS),
			manage:      func(context.Context) error { return nil },
		}, nil
	}

	certmagic.DefaultACME.Agreed = true
	certmagic.DefaultACME.DisableTLSALPNChallenge = true

	dataDir := cfg.DataDir
	if dataDir == "" {
		dataDir = resolveDataDir(getenv)
	}

	certConfig, stopCache := newManagedCertMagicConfig(dataDir)
	managedHosts := cfg.managedHosts()
	tlsConfig := certConfig.TLSConfig()
	tlsConfig.NextProtos = appendMissing(tlsConfig.NextProtos, "h2", "http/1.1")

	var stopOnce sync.Once
	stopManagedCache := func() {
		stopOnce.Do(stopCache)
	}

	return edgeTLSAssets{
		tlsConfig:   tlsConfig,
		httpHandler: wrapHTTPChallenge(certConfig, http.HandlerFunc(redirectToHTTPS)),
		manage: func(ctx context.Context) error {
			go func() {
				<-ctx.Done()
				stopManagedCache()
			}()

			log.Printf("managing TLS certificates for %s using %s", strings.Join(managedHosts, ", "), dataDir)
			if err := certConfig.ManageSync(ctx, managedHosts); err != nil {
				stopManagedCache()
				return err
			}
			return nil
		},
	}, nil
}

func newManagedCertMagicConfig(dataDir string) (*certmagic.Config, func()) {
	storage := &certmagic.FileStorage{Path: dataDir}

	var cache *certmagic.Cache
	cache = certmagic.NewCache(certmagic.CacheOptions{
		GetConfigForCert: func(cert certmagic.Certificate) (*certmagic.Config, error) {
			return certmagic.New(cache, certmagic.Config{
				Storage: storage,
			}), nil
		},
	})

	return certmagic.New(cache, certmagic.Config{
		Storage: storage,
	}), cache.Stop
}

func newStatusHandler(startedAt time.Time, now func() time.Time) http.Handler {
	if now == nil {
		now = time.Now
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uptime := now().Sub(startedAt)
		if uptime < 0 {
			uptime = 0
		}
		uptime = uptime.Truncate(time.Second)

		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = fmt.Fprintf(w, "MuxBridgh active with uptime %s", uptime)
	})
}

func newHTTPSHandler(publicDomain, edgeDomain string, isPublicHost func(string) bool, statusHandler, publicHandler http.Handler, grpcHandler http.Handler, pprofHandler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := hostnames.NormalizeHost(r.Host)

		switch {
		case host == edgeDomain && isGRPCRequest(r):
			grpcHandler.ServeHTTP(w, r)
		case host == edgeDomain && pprofHandler != nil && isPprofPath(r.URL.Path):
			pprofHandler.ServeHTTP(w, r)
		case host == edgeDomain:
			http.NotFound(w, r)
		case host == publicDomain:
			statusHandler.ServeHTTP(w, r)
		case isPublicHost(host):
			publicHandler.ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	})
}

func isPprofPath(path string) bool {
	return path == "/pprof" || path == "/pprof/" || strings.HasPrefix(path, "/pprof/")
}

// newPprofHandler exposes the standard net/http/pprof handlers under /pprof.
// The pprof.Index handler routes by stripping "/debug/pprof/", so incoming
// paths are rewritten into that namespace before delegation.
func newPprofHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rewritten := r.Clone(r.Context())
		suffix := strings.TrimPrefix(r.URL.Path, "/pprof")
		if suffix == "" {
			suffix = "/"
		}
		rewritten.URL.Path = "/debug/pprof" + suffix
		mux.ServeHTTP(w, rewritten)
	})
}

func wrapHTTPChallenge(cfg *certmagic.Config, next http.Handler) http.Handler {
	for _, issuer := range cfg.Issuers {
		acmeIssuer, ok := issuer.(*certmagic.ACMEIssuer)
		if ok {
			return acmeIssuer.HTTPChallengeHandler(next)
		}
	}
	return next
}

func redirectToHTTPS(w http.ResponseWriter, r *http.Request) {
	target := "https://" + hostnames.NormalizeHost(r.Host) + r.URL.RequestURI()
	http.Redirect(w, r, target, http.StatusMovedPermanently)
}

func serveOrDie(name string, serve func(net.Listener) error, listener net.Listener) {
	if err := serveOnce(name, serve, listener); err != nil {
		log.Fatal(err)
	}
}

func serve(name string, serve func(net.Listener) error, listener net.Listener, errCh chan<- error) {
	errCh <- serveOnce(name, serve, listener)
}

func serveOnce(name string, serve func(net.Listener) error, listener net.Listener) error {
	log.Printf("%s listening on %s", name, listener.Addr())
	if err := serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func shutdownServers(servers ...*http.Server) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var firstErr error
	for _, server := range servers {
		if server == nil {
			continue
		}
		if err := server.Shutdown(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func appendMissing(existing []string, values ...string) []string {
	present := make(map[string]struct{}, len(existing))
	out := append([]string(nil), existing...)
	for _, value := range existing {
		present[value] = struct{}{}
	}
	for _, value := range values {
		if _, ok := present[value]; ok {
			continue
		}
		out = append(out, value)
		present[value] = struct{}{}
	}
	return out
}

func isGRPCRequest(r *http.Request) bool {
	return r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc")
}

func getenv(key string) string {
	return strings.TrimSpace(os.Getenv(key))
}
