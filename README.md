# muxbridge
[![codecov](https://codecov.io/gh/define42/muxbridge/graph/badge.svg?token=V3CLO9YG7H)](https://codecov.io/gh/define42/muxbridge)

MuxBridge is a self-hosted HTTP tunnel inspired by [Cloudflare Tunnel](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/). It lets you securely expose HTTP services running behind NAT or a firewall to the public internet — no inbound firewall rules or port forwarding required.

## How It Works

```
Browser ──HTTPS──▶ Edge Server (public) ──gRPC tunnel──▶ Client (private network) ──▶ Local HTTP app
```

You run an **edge server** on a public host. Your **client** — sitting behind NAT, a corporate firewall, or any private network — connects outbound to the edge over a persistent gRPC stream. The edge then forwards incoming HTTP requests through that stream to the client, which answers them using its local HTTP handler. Responses travel back the same way.

No VPN. No open ports on the client machine. Just an outbound gRPC connection.

### Key features

- **Token-based authentication** — each client authenticates with a secret token; the edge maps tokens to usernames
- **Automatic subdomain routing** — a client registered as `demo` is published at `demo.<public-domain>`
- **WebSocket support** — upgraded connections are proxied as bidirectional byte streams
- **Automatic TLS** — edge uses [CertMagic](https://github.com/caddyserver/certmagic) for automatic ACME certificate provisioning, or you can supply your own cert/key
- **Multiplexed requests** — multiple in-flight HTTP requests share a single gRPC stream, correlated by request ID
- **Reconnect on disconnect** — clients automatically reconnect with configurable backoff

### Compared to Cloudflare Tunnel

| | Cloudflare Tunnel | MuxBridge |
|---|---|---|
| Control plane | Cloudflare's network | Your own edge server |
| Protocol | QUIC / HTTP/2 | gRPC over HTTP/2 |
| TLS | Cloudflare-managed | CertMagic (ACME) or static cert |
| Auth | Cloudflare Access | Token → username mapping |
| WebSocket | Yes | Yes |
| Self-hostable | No | Yes |
| Client deployment | Separate `cloudflared` daemon | Embeddable Go library — add `tunnel.NewClient(...)` directly to your app |

Cloudflare Tunnel requires you to install and run a separate `cloudflared` process on every network where you want to expose a service. MuxBridge ships a Go library so you can embed the tunnel client directly into your own application — no sidecar, no external process, no extra deployment step. Your app dials the edge and serves traffic through its own `http.Handler`.

## Build

Build both binaries with:

```bash
make -f makefile all
```

This produces:

- `bin/edge`
- `bin/demo-client`

You can also build directly with Go:

```bash
go build -o bin/edge ./cmd/edge
go build -o bin/demo-client ./cmd/demo-client
```

## Test

```bash
go test ./...
go test -race ./...
go test ./... -coverprofile=/tmp/muxbridge.cover.out
go tool cover -func=/tmp/muxbridge.cover.out
```

The current suite uses real listeners and gRPC streams and keeps total statement coverage above 90%.

## How Routing Works

- The edge control endpoint is always `edge.<public-domain>`.
- Each client authenticates with a token.
- The edge maps tokens to usernames with `token=username`.
- A connected client is published at `<username>.<public-domain>`.
- Example: `demo-token=demo` publishes the client at `demo.example.com`.
- Username `edge` is reserved and cannot be used.
- Usernames must be a single lowercase DNS label containing `a-z`, `0-9`, and interior `-`.
- Request forwarding preserves method, headers, body, query string, and the escaped request path via `raw_path`.
- The forwarded request scheme comes from the edge connection itself: HTTPS requests arrive as `https`, plain HTTP requests arrive as `http`, and client-supplied `X-Forwarded-Proto` is ignored.
- WebSocket upgrade requests are proxied as upgraded byte streams after the HTTP handshake.

`Register.tunnel_id` is still present in the wire format for compatibility, but routing no longer depends on it.

## Edge Server

The edge server listens on:

- `:80` for HTTP redirect and ACME HTTP-01 challenges
- `:443` for HTTPS and gRPC

Requests are routed like this:

- `<public-domain>` -> `MuxBridgh active with uptime <duration>`
- `edge.<public-domain>` with gRPC over HTTP/2 -> tunnel control plane
- `edge.<public-domain>` without gRPC -> `404`
- `<username>.<public-domain>` -> proxied through the authenticated client session
- anything else -> `404`

If the same user connects again, the newest connection replaces the old one.

## TLS

By default, the edge server uses CertMagic and manages certificates for:

- `<public-domain>`
- `edge.<public-domain>`
- every configured `<username>.<public-domain>`

Managed certificates use ACME HTTP-01 challenges on port `80`. DNS for each managed hostname must point at the edge server, and both ports `80` and `443` must be reachable. TLS-ALPN challenges are disabled.

### Static Certificate Support

If you provide both a certificate file and key file, the edge server uses them instead of CertMagic:

- `--tls-cert-file`
- `--tls-key-file`

or:

- `MUXBRIDGE_TLS_CERT_FILE`
- `MUXBRIDGE_TLS_KEY_FILE`

Both must be provided together.

## Edge Configuration

### Flags

```text
--public-domain
--client-credential token=username
--tls-cert-file
--tls-key-file
--debug
```

`--client-credential` may be repeated.

### Environment Variables

```text
MUXBRIDGE_PUBLIC_DOMAIN
MUXBRIDGE_CLIENT_CREDENTIALS
MUXBRIDGH_DATA
MUXBRIDGE_TLS_CERT_FILE
MUXBRIDGE_TLS_KEY_FILE
MUXBRIDGE_DEBUG
```

`MUXBRIDGE_CLIENT_CREDENTIALS` uses comma-separated `token=username` entries:

```text
demo-token=demo,admin-token=admin
```

Because entries are comma-separated, tokens supplied via `MUXBRIDGE_CLIENT_CREDENTIALS` must not contain `,`. Tokens that contain commas must be provided via repeated `--client-credential` flags instead.

`MUXBRIDGH_DATA` sets the single persistent data directory used by the edge. When the edge manages TLS with CertMagic, certificates, ACME account data, OCSP cache entries, and lock files are all stored here. `MUXBRIDGE_DATA` is also accepted as a compatibility alias.

Flag credential entries are appended after environment entries. Credential values are trimmed, usernames are normalized to lowercase before validation, and startup fails on malformed entries, duplicate tokens, duplicate usernames, invalid usernames, usernames with ports such as `demo:443`, or reserved username `edge`.

## Running The Edge

### CertMagic-managed TLS

```bash
bin/edge \
  --public-domain example.com \
  --client-credential demo-token=demo
```

### Static TLS Certificate

```bash
bin/edge \
  --public-domain example.com \
  --client-credential demo-token=demo \
  --tls-cert-file /etc/ssl/example/fullchain.pem \
  --tls-key-file /etc/ssl/example/privkey.pem
```

## Demo Client

The demo client serves a small HTTP app locally and connects it to the edge.

Routes served by the demo app:

- `/` -> plain text greeting plus the browser's remote IP
- `/slow` -> slow chunked plain-text response
- `/ws-demo` -> browser page that exercises a WebSocket through the tunnel
- `/ws-demo/socket` -> WebSocket endpoint used by `/ws-demo`
- `/sse-demo` -> browser page that exercises Server-Sent Events through the tunnel
- `/sse-demo/events` -> SSE endpoint used by `/sse-demo`

Defaults:

- TLS enabled
- token: `demo-token`
- edge address: `edge.<public-domain>:443` when `--edge-addr` is not provided
- flag and environment string values are trimmed before use

### Flags

```text
--public-domain
--edge-addr
--token
--debug
```

### Environment Variables

```text
MUXBRIDGE_PUBLIC_DOMAIN
MUXBRIDGE_EDGE_ADDR
MUXBRIDGE_CLIENT_TOKEN
MUXBRIDGE_DEBUG
```

### Run The Demo Client

```bash
bin/demo-client --public-domain example.com --token demo-token
```

With the matching edge configuration:

```text
demo-token=demo
```

the demo client becomes available at:

```text
https://demo.example.com/
```

Additional demo pages are available at:

```text
https://demo.example.com/ws-demo
https://demo.example.com/sse-demo
```

### Debug Logging

Both binaries support opt-in debug logging via `--debug` or `MUXBRIDGE_DEBUG=1`.

Example:

```bash
MUXBRIDGE_DEBUG=1 bin/edge \
  --public-domain example.com \
  --client-credential demo-token=demo

MUXBRIDGE_DEBUG=1 bin/demo-client \
  --public-domain example.com \
  --token demo-token
```

## Minimal End-To-End Example

1. Point DNS at the edge server for:
   - `edge.example.com`
   - `demo.example.com`
2. Start the edge:

```bash
bin/edge \
  --public-domain example.com \
  --client-credential demo-token=demo
```

3. Start the demo client:

```bash
bin/demo-client \
  --public-domain example.com \
  --token demo-token
```

4. Open:

```text
https://demo.example.com/
```

## Docker

A minimal Alpine-based image is provided. The edge binary is the entrypoint and exposes ports `80` and `443`. All persistent edge data, including CertMagic certificates, is stored under `MUXBRIDGH_DATA`, which defaults to `/var/lib/muxbridge` in the container image.

Example `docker-compose.yml`:

```yaml
services:
  muxbridge:
    image: ghcr.io/define42/muxbridge:latest
    restart: unless-stopped
    read_only: true
    ports:
      - "80:80"
      - "443:443"
    environment:
      MUXBRIDGE_PUBLIC_DOMAIN: example.com
      MUXBRIDGE_CLIENT_CREDENTIALS: demo-token=demo
      MUXBRIDGH_DATA: /var/lib/muxbridge
    volumes:
      - ./muxbridge-data:/var/lib/muxbridge
```

With `read_only: true`, the container root filesystem stays read-only and only the mounted data directory remains writable.

Then start the edge with:

```bash
docker compose up -d
```

## Using the Tunnel Package

To expose your own `http.Handler` instead of the demo app, import the `tunnel` package:

```go
import "github.com/define42/muxbridge/tunnel"

client, err := tunnel.New(tunnel.Config{
    EdgeAddr: "edge.example.com:443",
    Token:    "my-secret-token",
    Handler:  myHandler,
})
if err != nil {
    log.Fatal(err)
}

if err := client.Run(ctx); err != nil {
    log.Fatal(err)
}
```

The client automatically reconnects on disconnect with a configurable backoff (default 2 s).

### Hello World Example

A complete Go program that serves a Hello World page through the tunnel:

```go
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"

	"github.com/define42/muxbridge/tunnel"
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "Hello, World!")
	})

	client, err := tunnel.New(tunnel.Config{
		EdgeAddr: "edge.example.com:443",
		Token:    "my-secret-token",
		Handler:  mux,
	})
	if err != nil {
		log.Fatal(err)
	}

	if err := client.Run(context.Background()); err != nil {
		log.Fatal(err)
	}
}
```

With the matching edge credential `my-secret-token=alice`, visiting `https://alice.example.com/` returns `Hello, World!`. No separate process or sidecar required — the tunnel is part of your binary.
