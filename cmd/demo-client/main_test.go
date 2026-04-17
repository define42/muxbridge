package main

import (
	"bufio"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

func TestDefaultString(t *testing.T) {
	t.Parallel()

	if got := defaultString("", "fallback"); got != "fallback" {
		t.Fatalf("defaultString empty = %q, want %q", got, "fallback")
	}
	if got := defaultString("value", "fallback"); got != "value" {
		t.Fatalf("defaultString value = %q, want %q", got, "value")
	}
}

func TestLoadConfigDerivesEdgeAddr(t *testing.T) {
	t.Parallel()

	cfg, err := loadConfig([]string{"--public-domain", "Example.COM."}, func(string) string { return "" })
	if err != nil {
		t.Fatalf("loadConfig error: %v", err)
	}
	if cfg.PublicDomain != "example.com" {
		t.Fatalf("PublicDomain = %q, want %q", cfg.PublicDomain, "example.com")
	}
	if cfg.EdgeAddr != "edge.example.com:443" {
		t.Fatalf("EdgeAddr = %q, want %q", cfg.EdgeAddr, "edge.example.com:443")
	}
	if cfg.Token != "demo-token" {
		t.Fatalf("Token = %q, want %q", cfg.Token, "demo-token")
	}
}

func TestLoadConfigUsesEnvAndFlags(t *testing.T) {
	t.Parallel()

	cfg, err := loadConfig([]string{"--token", "flag-token"}, func(key string) string {
		switch key {
		case "MUXBRIDGE_PUBLIC_DOMAIN":
			return " env.example.com "
		case "MUXBRIDGE_EDGE_ADDR":
			return " edge.example.com:443 "
		case "MUXBRIDGE_CLIENT_TOKEN":
			return " env-token "
		case "MUXBRIDGE_DEBUG":
			return "true"
		default:
			return ""
		}
	})
	if err != nil {
		t.Fatalf("loadConfig error: %v", err)
	}
	if cfg.PublicDomain != "env.example.com" {
		t.Fatalf("PublicDomain = %q, want %q", cfg.PublicDomain, "env.example.com")
	}
	if cfg.EdgeAddr != "edge.example.com:443" {
		t.Fatalf("EdgeAddr = %q, want %q", cfg.EdgeAddr, "edge.example.com:443")
	}
	if cfg.Token != "flag-token" {
		t.Fatalf("Token = %q, want %q", cfg.Token, "flag-token")
	}
	if !cfg.Debug {
		t.Fatal("Debug = false, want true")
	}
}

func TestLoadConfigRequiresPublicDomainWhenEdgeAddrMissing(t *testing.T) {
	t.Parallel()

	_, err := loadConfig(nil, func(string) string { return "" })
	if err == nil || !strings.Contains(err.Error(), "public domain is required") {
		t.Fatalf("loadConfig error = %v, want missing public domain", err)
	}
}

func TestLoadConfigRejectsInvalidPublicDomain(t *testing.T) {
	t.Parallel()

	_, err := loadConfig([]string{"--public-domain", "localhost"}, func(string) string { return "" })
	if err == nil || !strings.Contains(err.Error(), "invalid public domain") {
		t.Fatalf("loadConfig error = %v, want invalid public domain", err)
	}
}

func TestNewDemoMuxHandlers(t *testing.T) {
	mux := newDemoMux()

	rootReq := httptest.NewRequest(http.MethodGet, "http://demo.example.com/", nil)
	rootReq.RemoteAddr = "203.0.113.10:4321"
	rootRes := httptest.NewRecorder()
	mux.ServeHTTP(rootRes, rootReq)
	if rootRes.Code != http.StatusOK {
		t.Fatalf("/ status = %d, want %d", rootRes.Code, http.StatusOK)
	}
	if rootRes.Body.String() != "hello through grpc tunnel\nremote ip: 203.0.113.10\n" {
		t.Fatalf("/ body = %q, want greeting plus remote ip", rootRes.Body.String())
	}

	wsPageReq := httptest.NewRequest(http.MethodGet, "http://demo.example.com/ws-demo", nil)
	wsPageRes := httptest.NewRecorder()
	mux.ServeHTTP(wsPageRes, wsPageReq)
	if wsPageRes.Code != http.StatusOK {
		t.Fatalf("/ws-demo status = %d, want %d", wsPageRes.Code, http.StatusOK)
	}
	if got := wsPageRes.Body.String(); !strings.Contains(got, "WebSocket Demo") || !strings.Contains(got, "/ws-demo/socket") {
		t.Fatalf("/ws-demo body = %q, want websocket demo page", got)
	}

	ssePageReq := httptest.NewRequest(http.MethodGet, "http://demo.example.com/sse-demo", nil)
	ssePageRes := httptest.NewRecorder()
	mux.ServeHTTP(ssePageRes, ssePageReq)
	if ssePageRes.Code != http.StatusOK {
		t.Fatalf("/sse-demo status = %d, want %d", ssePageRes.Code, http.StatusOK)
	}
	if got := ssePageRes.Body.String(); !strings.Contains(got, "SSE Demo") || !strings.Contains(got, "/sse-demo/events") {
		t.Fatalf("/sse-demo body = %q, want SSE demo page", got)
	}

	previousDelay := slowChunkDelay
	slowChunkDelay = 0
	t.Cleanup(func() { slowChunkDelay = previousDelay })

	slowReq := httptest.NewRequest(http.MethodGet, "http://demo.example.com/slow", nil)
	slowRes := httptest.NewRecorder()
	mux.ServeHTTP(slowRes, slowReq)
	if slowRes.Code != http.StatusOK {
		t.Fatalf("/slow status = %d, want %d", slowRes.Code, http.StatusOK)
	}
	if got := slowRes.Body.String(); got != "chunk 1\nchunk 2\nchunk 3\nchunk 4\nchunk 5\n" {
		t.Fatalf("/slow body = %q, want five chunks", got)
	}
}

func TestWebSocketDemoEcho(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(newDemoMux())
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws-demo/socket"
	ws, err := websocket.Dial(wsURL, "", server.URL)
	if err != nil {
		t.Fatalf("websocket.Dial error: %v", err)
	}
	defer func() {
		_ = ws.Close()
	}()

	var welcome string
	if err := websocket.Message.Receive(ws, &welcome); err != nil {
		t.Fatalf("Receive welcome error: %v", err)
	}
	if !strings.Contains(welcome, "connected from ") {
		t.Fatalf("welcome = %q, want connection banner", welcome)
	}

	if err := websocket.Message.Send(ws, "ping demo"); err != nil {
		t.Fatalf("Send error: %v", err)
	}

	var reply string
	if err := websocket.Message.Receive(ws, &reply); err != nil {
		t.Fatalf("Receive reply error: %v", err)
	}
	if !strings.Contains(reply, "echo from ") || !strings.Contains(reply, "ping demo") {
		t.Fatalf("reply = %q, want echoed message", reply)
	}
}

func TestSSEDemoStream(t *testing.T) {
	t.Parallel()

	previousInterval := sseTickInterval
	sseTickInterval = 5 * time.Millisecond
	t.Cleanup(func() { sseTickInterval = previousInterval })

	server := httptest.NewServer(newDemoMux())
	defer server.Close()

	req, err := http.NewRequest(http.MethodGet, server.URL+"/sse-demo/events", nil)
	if err != nil {
		t.Fatalf("NewRequest error: %v", err)
	}
	req.Header.Set("Accept", "text/event-stream")

	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("Do error: %v", err)
	}
	defer func() {
		_ = res.Body.Close()
	}()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", res.StatusCode, http.StatusOK)
	}
	if got := res.Header.Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}

	reader := bufio.NewReader(res.Body)
	sawConnected := false
	sawTick := false
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			t.Fatalf("ReadString error: %v", readErr)
		}
		if strings.Contains(line, "event: connected") {
			sawConnected = true
		}
		if strings.Contains(line, "event: tick") {
			sawTick = true
			break
		}
	}

	if !sawConnected {
		t.Fatal("did not observe connected SSE event")
	}
	if !sawTick {
		t.Fatal("did not observe tick SSE event")
	}
}

func TestRemoteIPFromAddr(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"":                  "unknown",
		"203.0.113.10:443":  "203.0.113.10",
		"[2001:db8::1]:443": "2001:db8::1",
		"203.0.113.10":      "203.0.113.10",
	}

	for input, want := range tests {
		if got := remoteIPFromAddr(input); got != want {
			t.Fatalf("remoteIPFromAddr(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestRunReturnsConfigAndClientErrors(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := run(ctx, []string{"--edge-addr", "127.0.0.1:1"}, func(string) string { return "" }); !errors.Is(err, context.Canceled) {
		t.Fatalf("run canceled error = %v, want %v", err, context.Canceled)
	}

	err := run(context.Background(), []string{"--edge-addr", "127.0.0.1:1", "--token", ""}, func(string) string { return "" })
	if err == nil || !strings.Contains(err.Error(), "token is required") {
		t.Fatalf("run error = %v, want missing token", err)
	}
}

func TestGetenvTrimsWhitespace(t *testing.T) {
	key := "MUXBRIDGE_DEMO_TEST_ENV"
	t.Setenv(key, "  value  ")
	if got := getenv(key); got != "value" {
		t.Fatalf("getenv = %q, want %q", got, "value")
	}
}

func TestLoadConfigFlagOverridesDebugEnv(t *testing.T) {
	t.Parallel()

	cfg, err := loadConfig([]string{"--edge-addr", "edge.example.com:443", "--debug=false"}, func(key string) string {
		if key == "MUXBRIDGE_DEBUG" {
			return "true"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("loadConfig error: %v", err)
	}
	if cfg.Debug {
		t.Fatal("Debug = true, want false")
	}
}
