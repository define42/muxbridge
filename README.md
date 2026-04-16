# muxbridge

MuxBridge exposes an HTTP server running behind a client over a gRPC tunnel.

The edge server accepts authenticated client connections on `edge.<public-domain>` and publishes each connected client at `<username>.<public-domain>`.

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

## How Routing Works

- The edge control endpoint is always `edge.<public-domain>`.
- Each client authenticates with a token.
- The edge maps tokens to usernames with `token=username`.
- A connected client is published at `<username>.<public-domain>`.
- Example: `demo-token=demo` publishes the client at `demo.example.com`.
- Username `edge` is reserved and cannot be used.
- Usernames must be a single lowercase DNS label containing `a-z`, `0-9`, and interior `-`.

`Register.tunnel_id` is still present in the wire format for compatibility, but routing no longer depends on it.

## Edge Server

The edge server listens on:

- `:80` for HTTP redirect and ACME HTTP-01 challenges
- `:443` for HTTPS and gRPC

Requests are routed like this:

- `edge.<public-domain>` with gRPC over HTTP/2 -> tunnel control plane
- `edge.<public-domain>` without gRPC -> `404`
- `<username>.<public-domain>` -> proxied through the authenticated client session
- anything else -> `404`

If the same user connects again, the newest connection replaces the old one.

## TLS

By default, the edge server uses CertMagic and manages certificates for:

- `edge.<public-domain>`
- every configured `<username>.<public-domain>`

This requires DNS for each managed hostname to point at the edge server and ports `80` and `443` to be reachable.

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
```

`--client-credential` may be repeated.

### Environment Variables

```text
MUXBRIDGE_PUBLIC_DOMAIN
MUXBRIDGE_CLIENT_CREDENTIALS
MUXBRIDGE_TLS_CERT_FILE
MUXBRIDGE_TLS_KEY_FILE
```

`MUXBRIDGE_CLIENT_CREDENTIALS` uses comma-separated `token=username` entries:

```text
demo-token=demo,admin-token=admin
```

Flag credential entries are appended after environment entries. Startup fails on malformed entries, duplicate tokens, duplicate usernames, invalid usernames, or reserved username `edge`.

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

Defaults:

- TLS enabled
- token: `demo-token`
- edge address: `edge.<public-domain>:443` when `--edge-addr` is not provided

### Flags

```text
--public-domain
--edge-addr
--token
```

### Environment Variables

```text
MUXBRIDGE_PUBLIC_DOMAIN
MUXBRIDGE_EDGE_ADDR
MUXBRIDGE_CLIENT_TOKEN
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
