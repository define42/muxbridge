package main

import (
	"log"
	"net"
	"net/http"

	"github.com/define42/muxbridge/gen/tunnelpb"
	"github.com/define42/muxbridge/server"
	"google.golang.org/grpc"
)

func main() {
	srv := server.New()

	grpcServer := grpc.NewServer()
	tunnelpb.RegisterTunnelServiceServer(grpcServer, srv)

	lis, err := net.Listen("tcp", ":8443")
	if err != nil {
		log.Fatal(err)
	}
	go func() {
		log.Println("gRPC edge listening on :8443")
		log.Fatal(grpcServer.Serve(lis))
	}()

	log.Println("public HTTP ingress listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", srv))
}
