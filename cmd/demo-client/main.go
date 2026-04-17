package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/define42/muxbridge/internal/hostnames"
	"github.com/define42/muxbridge/tunnel"
)

type demoConfig struct {
	PublicDomain string
	EdgeAddr     string
	Token        string
	Debug        bool
}

var slowChunkDelay = 500 * time.Millisecond

func main() {
	if err := run(context.Background(), os.Args[1:], getenv); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, args []string, getenv func(string) string) error {
	cfg, err := loadConfig(args, getenv)
	if err != nil {
		return err
	}

	mux := newDemoMux()

	cli, err := tunnel.New(tunnel.Config{
		EdgeAddr: cfg.EdgeAddr,
		Token:    cfg.Token,
		Handler:  mux,
		Debug:    cfg.Debug,
		Logger:   log.Default(),
	})
	if err != nil {
		return err
	}
	if cfg.Debug {
		reconnectBackoff := 2 * time.Second
		dialTimeout := 10 * time.Second
		log.Printf(
			"client debug enabled: edge_addr=%s public_domain=%s reconnect_backoff=%s dial_timeout=%s",
			cfg.EdgeAddr,
			cfg.PublicDomain,
			reconnectBackoff,
			dialTimeout,
		)
	}

	return cli.Run(ctx)
}

func loadConfig(args []string, getenv func(string) string) (demoConfig, error) {
	fs := flag.NewFlagSet("demo-client", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	cfg := demoConfig{
		PublicDomain: getenv("MUXBRIDGE_PUBLIC_DOMAIN"),
		EdgeAddr:     getenv("MUXBRIDGE_EDGE_ADDR"),
		Token:        defaultString(getenv("MUXBRIDGE_CLIENT_TOKEN"), "demo-token"),
		Debug:        parseBoolString(getenv("MUXBRIDGE_DEBUG")),
	}

	fs.StringVar(&cfg.PublicDomain, "public-domain", cfg.PublicDomain, "Public base domain for the edge")
	fs.StringVar(&cfg.EdgeAddr, "edge-addr", cfg.EdgeAddr, "Edge gRPC address")
	fs.StringVar(&cfg.Token, "token", cfg.Token, "Client authentication token")
	fs.BoolVar(&cfg.Debug, "debug", cfg.Debug, "Enable debug logging")
	if err := fs.Parse(args); err != nil {
		return demoConfig{}, err
	}

	cfg.PublicDomain = strings.TrimSpace(cfg.PublicDomain)
	cfg.EdgeAddr = strings.TrimSpace(cfg.EdgeAddr)
	cfg.Token = strings.TrimSpace(cfg.Token)

	if cfg.EdgeAddr == "" {
		domain := hostnames.NormalizeDomain(cfg.PublicDomain)
		if domain == "" {
			return demoConfig{}, fmt.Errorf("public domain is required when edge addr is not provided")
		}
		if err := hostnames.ValidateDomain(domain); err != nil {
			return demoConfig{}, fmt.Errorf("invalid public domain: %w", err)
		}
		cfg.PublicDomain = domain
		cfg.EdgeAddr = hostnames.Subdomain("edge", domain) + ":443"
	}

	return cfg, nil
}

func newDemoMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = fmt.Fprintf(w, "hello through grpc tunnel\nremote ip: %s\n", remoteIPFromAddr(r.RemoteAddr))
	})
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		for i := 0; i < 5; i++ {
			_, _ = fmt.Fprintf(w, "chunk %d\n", i+1)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(slowChunkDelay)
		}
	})
	return mux
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func parseBoolString(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "t", "true", "y", "yes", "on":
		return true
	default:
		return false
	}
}

func remoteIPFromAddr(remoteAddr string) string {
	remoteAddr = strings.TrimSpace(remoteAddr)
	if remoteAddr == "" {
		return "unknown"
	}
	host, _, err := net.SplitHostPort(remoteAddr)
	if err == nil && host != "" {
		return host
	}
	return remoteAddr
}

func getenv(key string) string {
	return strings.TrimSpace(os.Getenv(key))
}
