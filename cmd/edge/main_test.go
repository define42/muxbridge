package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLoadConfigDerivesEdgeDomainAndParsesCredentials(t *testing.T) {
	t.Parallel()

	cfg, err := loadConfig(nil, func(key string) string {
		switch key {
		case publicDomainEnv:
			return "Example.COM."
		case clientCredentialsEnv:
			return "demo-token=demo"
		default:
			return ""
		}
	})
	if err != nil {
		t.Fatalf("loadConfig error: %v", err)
	}

	if cfg.PublicDomain != "example.com" {
		t.Fatalf("PublicDomain = %q, want %q", cfg.PublicDomain, "example.com")
	}
	if cfg.EdgeDomain != "edge.example.com" {
		t.Fatalf("EdgeDomain = %q, want %q", cfg.EdgeDomain, "edge.example.com")
	}
	if got := cfg.ClientCredentials["demo-token"]; got != "demo" {
		t.Fatalf("credential for demo-token = %q, want %q", got, "demo")
	}
}

func TestLoadConfigFlagOverridesEnvPublicDomain(t *testing.T) {
	t.Parallel()

	cfg, err := loadConfig([]string{
		"--public-domain", "flag.example.com",
		"--client-credential", "demo-token=demo",
	}, func(key string) string {
		if key == publicDomainEnv {
			return "env.example.com"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("loadConfig error: %v", err)
	}

	if cfg.PublicDomain != "flag.example.com" {
		t.Fatalf("PublicDomain = %q, want %q", cfg.PublicDomain, "flag.example.com")
	}
}

func TestLoadConfigRejectsDuplicateUsername(t *testing.T) {
	t.Parallel()

	_, err := loadConfig([]string{
		"--public-domain", "example.com",
		"--client-credential", "token-a=demo",
		"--client-credential", "token-b=demo",
	}, func(string) string { return "" })
	if err == nil {
		t.Fatal("expected duplicate username error")
	}
}

func TestNewHTTPSHandlerDispatch(t *testing.T) {
	t.Parallel()

	publicCalls := 0
	grpcCalls := 0

	handler := newHTTPSHandler(
		"edge.example.com",
		func(host string) bool { return host == "demo.example.com" },
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			publicCalls++
			w.WriteHeader(http.StatusAccepted)
		}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			grpcCalls++
			w.WriteHeader(http.StatusNoContent)
		}),
	)

	grpcReq := httptest.NewRequest(http.MethodPost, "https://edge.example.com/tunnel.v1.TunnelService/Connect", nil)
	grpcReq.Host = "EDGE.EXAMPLE.COM:443"
	grpcReq.ProtoMajor = 2
	grpcReq.Header.Set("Content-Type", "application/grpc+proto")
	grpcRes := httptest.NewRecorder()
	handler.ServeHTTP(grpcRes, grpcReq)
	if grpcRes.Code != http.StatusNoContent {
		t.Fatalf("gRPC response = %d, want %d", grpcRes.Code, http.StatusNoContent)
	}

	publicReq := httptest.NewRequest(http.MethodGet, "https://demo.example.com/", nil)
	publicReq.Host = "DEMO.EXAMPLE.COM:443"
	publicRes := httptest.NewRecorder()
	handler.ServeHTTP(publicRes, publicReq)
	if publicRes.Code != http.StatusAccepted {
		t.Fatalf("public response = %d, want %d", publicRes.Code, http.StatusAccepted)
	}

	edgeReq := httptest.NewRequest(http.MethodGet, "https://edge.example.com/", nil)
	edgeReq.Host = "edge.example.com"
	edgeRes := httptest.NewRecorder()
	handler.ServeHTTP(edgeRes, edgeReq)
	if edgeRes.Code != http.StatusNotFound {
		t.Fatalf("edge non-gRPC response = %d, want %d", edgeRes.Code, http.StatusNotFound)
	}

	unknownReq := httptest.NewRequest(http.MethodGet, "https://unknown.example.com/", nil)
	unknownReq.Host = "unknown.example.com"
	unknownRes := httptest.NewRecorder()
	handler.ServeHTTP(unknownRes, unknownReq)
	if unknownRes.Code != http.StatusNotFound {
		t.Fatalf("unknown response = %d, want %d", unknownRes.Code, http.StatusNotFound)
	}

	if grpcCalls != 1 {
		t.Fatalf("grpcCalls = %d, want %d", grpcCalls, 1)
	}
	if publicCalls != 1 {
		t.Fatalf("publicCalls = %d, want %d", publicCalls, 1)
	}
}
