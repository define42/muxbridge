all:
	protoc \
		--go_out=. \
		--go_opt=module=github.com/define42/muxbridge \
		--go-grpc_out=. \
		--go-grpc_opt=module=github.com/define42/muxbridge \
		proto/tunnel.proto
