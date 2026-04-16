package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"flag"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/caddyserver/certmagic"
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

func TestLoadConfigStaticTLSFromEnv(t *testing.T) {
	t.Parallel()

	cfg, err := loadConfig([]string{
		"--public-domain", "example.com",
		"--client-credential", "demo-token=demo",
	}, func(key string) string {
		switch key {
		case tlsCertFileEnv:
			return "/tmp/test-cert.pem"
		case tlsKeyFileEnv:
			return "/tmp/test-key.pem"
		default:
			return ""
		}
	})
	if err != nil {
		t.Fatalf("loadConfig error: %v", err)
	}

	if cfg.TLSCertFile != "/tmp/test-cert.pem" {
		t.Fatalf("TLSCertFile = %q, want %q", cfg.TLSCertFile, "/tmp/test-cert.pem")
	}
	if cfg.TLSKeyFile != "/tmp/test-key.pem" {
		t.Fatalf("TLSKeyFile = %q, want %q", cfg.TLSKeyFile, "/tmp/test-key.pem")
	}
}

func TestLoadConfigRejectsIncompleteStaticTLSConfig(t *testing.T) {
	t.Parallel()

	_, err := loadConfig([]string{
		"--public-domain", "example.com",
		"--tls-cert-file", "/tmp/test-cert.pem",
		"--client-credential", "demo-token=demo",
	}, func(string) string { return "" })
	if err == nil {
		t.Fatal("expected incomplete static TLS config error")
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

func TestLoadConfigRejectsReservedUsername(t *testing.T) {
	t.Parallel()

	_, err := loadConfig([]string{
		"--public-domain", "example.com",
		"--client-credential", "demo-token=edge",
	}, func(string) string { return "" })
	if err == nil {
		t.Fatal("expected reserved username error")
	}
}

func TestLoadConfigRejectsInvalidUsername(t *testing.T) {
	t.Parallel()

	_, err := loadConfig([]string{
		"--public-domain", "example.com",
		"--client-credential", "demo-token=demo.user",
	}, func(string) string { return "" })
	if err == nil {
		t.Fatal("expected invalid username error")
	}
}

func TestLoadConfigFlagOverridesEnvStaticTLSFiles(t *testing.T) {
	t.Parallel()

	cfg, err := loadConfig([]string{
		"--public-domain", "example.com",
		"--client-credential", "demo-token=demo",
		"--tls-cert-file", "/flag/cert.pem",
		"--tls-key-file", "/flag/key.pem",
	}, func(key string) string {
		switch key {
		case tlsCertFileEnv:
			return "/env/cert.pem"
		case tlsKeyFileEnv:
			return "/env/key.pem"
		default:
			return ""
		}
	})
	if err != nil {
		t.Fatalf("loadConfig error: %v", err)
	}

	if cfg.TLSCertFile != "/flag/cert.pem" {
		t.Fatalf("TLSCertFile = %q, want %q", cfg.TLSCertFile, "/flag/cert.pem")
	}
	if cfg.TLSKeyFile != "/flag/key.pem" {
		t.Fatalf("TLSKeyFile = %q, want %q", cfg.TLSKeyFile, "/flag/key.pem")
	}
}

func TestEdgeConfigManagedHostsAndSortedUsers(t *testing.T) {
	t.Parallel()

	cfg := edgeConfig{
		PublicDomain: "example.com",
		EdgeDomain:   "edge.example.com",
		ClientCredentials: map[string]string{
			"z-token": "zebra",
			"a-token": "alpha",
		},
	}

	if got := cfg.sortedUsers(); len(got) != 2 || got[0] != "alpha" || got[1] != "zebra" {
		t.Fatalf("sortedUsers = %#v, want [alpha zebra]", got)
	}
	if got := cfg.managedHosts(); len(got) != 3 || got[0] != "edge.example.com" || got[1] != "alpha.example.com" || got[2] != "zebra.example.com" {
		t.Fatalf("managedHosts = %#v, want edge/alpha/zebra", got)
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

func TestEdgeTLSConfigStaticCertificate(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	certFile := filepath.Join(tempDir, "cert.pem")
	keyFile := filepath.Join(tempDir, "key.pem")
	certPEM, keyPEM := mustGenerateTestCertificate(t, "localhost")
	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	tlsConfig, httpHandler, err := edgeTLSConfig(context.Background(), edgeConfig{
		PublicDomain: "example.com",
		EdgeDomain:   "edge.example.com",
		TLSCertFile:  certFile,
		TLSKeyFile:   keyFile,
	})
	if err != nil {
		t.Fatalf("edgeTLSConfig error: %v", err)
	}
	if tlsConfig == nil {
		t.Fatal("expected tlsConfig")
	}
	if len(tlsConfig.Certificates) != 1 {
		t.Fatalf("Certificates len = %d, want 1", len(tlsConfig.Certificates))
	}
	if httpHandler == nil {
		t.Fatal("expected http challenge handler")
	}
	if len(tlsConfig.NextProtos) != 2 || tlsConfig.NextProtos[0] != "h2" || tlsConfig.NextProtos[1] != "http/1.1" {
		t.Fatalf("NextProtos = %#v, want [h2 http/1.1]", tlsConfig.NextProtos)
	}
}

func TestEdgeTLSConfigStaticCertificateServesHTTPS(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	certFile := filepath.Join(tempDir, "cert.pem")
	keyFile := filepath.Join(tempDir, "key.pem")
	certPEM, keyPEM := mustGenerateTestCertificate(t, "edge.example.com")
	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	tlsConfig, _, err := edgeTLSConfig(context.Background(), edgeConfig{
		PublicDomain: "example.com",
		EdgeDomain:   "edge.example.com",
		TLSCertFile:  certFile,
		TLSKeyFile:   keyFile,
	})
	if err != nil {
		t.Fatalf("edgeTLSConfig error: %v", err)
	}

	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	server.TLS = tlsConfig.Clone()
	server.StartTLS()
	defer server.Close()

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certPEM) {
		t.Fatal("failed to add certificate to root pool")
	}

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:    roots,
				ServerName: "edge.example.com",
			},
		},
	}

	req, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Host = "edge.example.com"

	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("https request failed: %v", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("status code = %d, want %d", res.StatusCode, http.StatusNoContent)
	}
	if res.TLS == nil {
		t.Fatal("expected TLS connection state")
	}
}

func TestEdgeTLSConfigRejectsInvalidStaticCertificate(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	certFile := filepath.Join(tempDir, "cert.pem")
	keyFile := filepath.Join(tempDir, "key.pem")
	if err := os.WriteFile(certFile, []byte("not a cert"), 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyFile, []byte("not a key"), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	_, _, err := edgeTLSConfig(context.Background(), edgeConfig{
		PublicDomain: "example.com",
		EdgeDomain:   "edge.example.com",
		TLSCertFile:  certFile,
		TLSKeyFile:   keyFile,
	})
	if err == nil {
		t.Fatal("expected invalid static TLS certificate error")
	}
}

func TestWrapHTTPChallengeFallsBackToNext(t *testing.T) {
	t.Parallel()

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})

	handler := wrapHTTPChallenge(&certmagic.Config{}, next)
	req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if !called {
		t.Fatal("expected next handler to be called")
	}
	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusNoContent)
	}
}

func TestRedirectToHTTPS(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "http://example.com/path?x=1", nil)
	req.Host = "EXAMPLE.COM:80"
	res := httptest.NewRecorder()

	redirectToHTTPS(res, req)

	if res.Code != http.StatusMovedPermanently {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusMovedPermanently)
	}
	if got := res.Header().Get("Location"); got != "https://example.com/path?x=1" {
		t.Fatalf("Location = %q, want %q", got, "https://example.com/path?x=1")
	}
}

func TestServeOrDieAllowsServerClosed(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen error: %v", err)
	}
	defer listener.Close()

	serveOrDie("test", func(net.Listener) error {
		return http.ErrServerClosed
	}, listener)
}

func TestAppendMissingAndGetenv(t *testing.T) {
	if got := appendMissing([]string{"h2"}, "h2", "http/1.1"); len(got) != 2 || got[0] != "h2" || got[1] != "http/1.1" {
		t.Fatalf("appendMissing = %#v, want [h2 http/1.1]", got)
	}

	key := "MUXBRIDGE_EDGE_TEST_ENV"
	t.Setenv(key, "  value  ")
	if got := getenv(key); got != "value" {
		t.Fatalf("getenv = %q, want %q", got, "value")
	}
}

func TestMainRejectsInvalidConfigInHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_EDGE_HELPER_PROCESS") == "1" {
		flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)
		_ = os.Setenv(publicDomainEnv, "localhost")
		_ = os.Setenv(clientCredentialsEnv, "demo-token=demo")
		main()
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestMainRejectsInvalidConfigInHelperProcess")
	cmd.Env = append(os.Environ(), "GO_WANT_EDGE_HELPER_PROCESS=1")

	err := cmd.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("Run error = %v, want ExitError", err)
	}
}

func mustGenerateTestCertificate(t *testing.T, dnsNames ...string) ([]byte, []byte) {
	t.Helper()

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate private key: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName: "localhost",
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dnsNames,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(privateKey)})
	return certPEM, keyPEM
}
