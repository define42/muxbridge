package main

import (
	"context"
	"crypto/tls"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/caddyserver/certmagic"
	"github.com/define42/muxbridge/gen/tunnelpb"
	"github.com/define42/muxbridge/internal/hostnames"
	"github.com/define42/muxbridge/server"
	"golang.org/x/net/http2"
	"google.golang.org/grpc"
)

func main() {
	cfg, err := loadConfig(nil, getenv)
	if err != nil {
		log.Fatal(err)
	}

	srv, err := server.New(server.Config{
		PublicDomain: cfg.PublicDomain,
		TokenUsers:   cfg.ClientCredentials,
	})
	if err != nil {
		log.Fatal(err)
	}

	grpcServer := grpc.NewServer()
	tunnelpb.RegisterTunnelServiceServer(grpcServer, srv)

	httpsHandler := newHTTPSHandler(cfg.EdgeDomain, srv.HasPublicHost, srv, grpcServer)

	certmagic.DefaultACME.Agreed = true
	certmagic.DefaultACME.DisableTLSALPNChallenge = true

	certConfig := certmagic.NewDefault()

	httpListener, err := net.Listen("tcp", ":80")
	if err != nil {
		log.Fatal(err)
	}
	httpsListener, err := net.Listen("tcp", ":443")
	if err != nil {
		log.Fatal(err)
	}

	httpServer := &http.Server{
		Handler:           wrapHTTPChallenge(certConfig, http.HandlerFunc(redirectToHTTPS)),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	go serveOrDie("HTTP challenge", httpServer.Serve, httpListener)

	managedHosts := cfg.managedHosts()
	log.Printf("managing TLS certificates for %s", strings.Join(managedHosts, ", "))
	if err := certConfig.ManageSync(context.Background(), managedHosts); err != nil {
		log.Fatal(err)
	}

	tlsConfig := certConfig.TLSConfig()
	tlsConfig.NextProtos = appendMissing(tlsConfig.NextProtos, "h2", "http/1.1")

	httpsServer := &http.Server{
		Handler:           httpsHandler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       5 * time.Minute,
		TLSConfig:         tlsConfig,
	}
	if err := http2.ConfigureServer(httpsServer, &http2.Server{}); err != nil {
		log.Fatal(err)
	}

	log.Printf("edge listening on https://%s and http://:80 for ACME challenges", cfg.EdgeDomain)
	log.Fatal(httpsServer.Serve(tls.NewListener(httpsListener, tlsConfig)))
}

func newHTTPSHandler(edgeDomain string, isPublicHost func(string) bool, publicHandler http.Handler, grpcHandler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := hostnames.NormalizeHost(r.Host)

		switch {
		case host == edgeDomain && isGRPCRequest(r):
			grpcHandler.ServeHTTP(w, r)
		case host == edgeDomain:
			http.NotFound(w, r)
		case isPublicHost(host):
			publicHandler.ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
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
	log.Printf("%s listening on %s", name, listener.Addr())
	if err := serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
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
