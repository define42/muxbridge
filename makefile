BIN_DIR := bin
PROTO := proto/tunnel.proto
PROTO_GEN := gen/tunnelpb/tunnel.pb.go gen/tunnelpb/tunnel_grpc.pb.go
BENCH_GUARDRAILS := HandleClientFrame|DispatchWSInbound

.PHONY: all proto clean test bench lint

all: $(BIN_DIR)/edge $(BIN_DIR)/demo-client

proto: $(PROTO_GEN)

$(PROTO_GEN): $(PROTO)
	protoc \
		--go_out=. \
		--go_opt=module=github.com/define42/muxbridge \
		--go-grpc_out=. \
		--go-grpc_opt=module=github.com/define42/muxbridge \
		$(PROTO)

$(BIN_DIR)/edge: $(PROTO_GEN)
	mkdir -p $(BIN_DIR)
	go build -o $@ ./cmd/edge

$(BIN_DIR)/demo-client: $(PROTO_GEN)
	mkdir -p $(BIN_DIR)
	go build -o $@ ./cmd/demo-client

clean:
	rm -rf $(BIN_DIR)

test:
	go test ./... -cover
	$(MAKE) bench

bench:
	go test ./server ./tunnel -run '^$$' -bench '$(BENCH_GUARDRAILS)' -benchmem
lint:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.1.6 run
