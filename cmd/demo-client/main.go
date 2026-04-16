package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/define42/muxbridge/internal/hostnames"
	"github.com/define42/muxbridge/tunnel"
)

func main() {
	publicDomain := flag.String("public-domain", os.Getenv("MUXBRIDGE_PUBLIC_DOMAIN"), "Public base domain for the edge")
	edgeAddr := flag.String("edge-addr", os.Getenv("MUXBRIDGE_EDGE_ADDR"), "Edge gRPC address")
	token := flag.String("token", defaultString(os.Getenv("MUXBRIDGE_CLIENT_TOKEN"), "demo-token"), "Client authentication token")
	flag.Parse()

	if *edgeAddr == "" {
		domain := hostnames.NormalizeDomain(*publicDomain)
		if domain == "" {
			log.Fatal("public domain is required when edge addr is not provided")
		}
		if err := hostnames.ValidateDomain(domain); err != nil {
			log.Fatalf("invalid public domain: %v", err)
		}
		*edgeAddr = hostnames.Subdomain("edge", domain) + ":443"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("hello through grpc tunnel\n"))
	})
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		for i := 0; i < 5; i++ {
			_, _ = fmt.Fprintf(w, "chunk %d\n", i+1)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(500 * time.Millisecond)
		}
	})

	cli, err := tunnel.New(tunnel.Config{
		EdgeAddr: *edgeAddr,
		Token:    *token,
		Handler:  mux,
		Logger:   log.Default(),
	})
	if err != nil {
		log.Fatal(err)
	}

	log.Fatal(cli.Run(context.Background()))
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
