BIN_DIR := bin
PROTO := proto/tunnel.proto
PROTO_GEN := gen/tunnelpb/tunnel.pb.go gen/tunnelpb/tunnel_grpc.pb.go

.PHONY: all proto clean

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
