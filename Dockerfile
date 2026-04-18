# syntax=docker/dockerfile:1

FROM golang:1.26.2-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY gen ./gen
COPY internal ./internal
COPY server ./server
COPY tunnel ./tunnel

RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/edge ./cmd/edge

FROM alpine:3.22 AS certs

RUN apk add --no-cache ca-certificates

FROM scratch

ENV MUXBRIDGH_DATA=/var/lib/muxbridge
ENV SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt

COPY --from=certs /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/edge /usr/local/bin/edge

EXPOSE 80 443

ENTRYPOINT ["/usr/local/bin/edge"]
