package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
	rootRes := httptest.NewRecorder()
	mux.ServeHTTP(rootRes, rootReq)
	if rootRes.Code != http.StatusOK {
		t.Fatalf("/ status = %d, want %d", rootRes.Code, http.StatusOK)
	}
	if rootRes.Body.String() != "hello through grpc tunnel\n" {
		t.Fatalf("/ body = %q, want greeting", rootRes.Body.String())
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

func TestRunReturnsConfigAndClientErrors(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := run(ctx, []string{"--edge-addr", "127.0.0.1:1"}, func(string) string { return "" }); !errors.Is(err, context.Canceled) {
		t.Fatalf("run canceled error = %v, want %v", err, context.Canceled)
	}

	err := run(context.Background(), []string{"--edge-addr", "127.0.0.1:1", "--token", ""}, func(string) string { return "" })
	if err == nil || !strings.Contains(err.Error(), "Token is required") {
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
