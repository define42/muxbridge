package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/define42/muxbridge/tunnel"
)

func main() {
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
		EdgeAddr: "127.0.0.1:8443",
		TunnelID: "localhost:8080",
		Token:    "dev-token",
		Handler:  mux,
		Insecure: true,
		Logger:   log.Default(),
	})
	if err != nil {
		log.Fatal(err)
	}

	log.Fatal(cli.Run(context.Background()))
}
