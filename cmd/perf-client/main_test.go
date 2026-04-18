package main

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLoadConfigDerivesEdgeAddrAndDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := loadConfig([]string{"--public-host", "Perf.Example.COM.", "--public-domain", "Example.COM."}, func(string) string {
		return ""
	})
	if err != nil {
		t.Fatalf("loadConfig error: %v", err)
	}

	if cfg.PublicHost != "perf.example.com" {
		t.Fatalf("PublicHost = %q, want %q", cfg.PublicHost, "perf.example.com")
	}
	if cfg.PublicDomain != "example.com" {
		t.Fatalf("PublicDomain = %q, want %q", cfg.PublicDomain, "example.com")
	}
	if cfg.EdgeAddr != "edge.example.com:443" {
		t.Fatalf("EdgeAddr = %q, want %q", cfg.EdgeAddr, "edge.example.com:443")
	}
	if cfg.Token != defaultPerfToken {
		t.Fatalf("Token = %q, want %q", cfg.Token, defaultPerfToken)
	}
	if cfg.Connections != defaultConnections {
		t.Fatalf("Connections = %d, want %d", cfg.Connections, defaultConnections)
	}
	if cfg.Duration != defaultDuration {
		t.Fatalf("Duration = %s, want %s", cfg.Duration, defaultDuration)
	}
	if cfg.Scenario != defaultScenario {
		t.Fatalf("Scenario = %q, want %q", cfg.Scenario, defaultScenario)
	}
}

func TestLoadConfigUsesEnvAndFlags(t *testing.T) {
	t.Parallel()

	cfg, err := loadConfig([]string{
		"--public-host", "flag.example.com",
		"--token", "flag-token",
		"--connections", "12",
		"--duration", "45s",
		"--scenario", "fast",
		"--request-timeout", "4s",
		"--ready-timeout", "6s",
		"--debug=false",
	}, func(key string) string {
		switch key {
		case "MUXBRIDGE_PUBLIC_HOST":
			return " env.example.com "
		case "MUXBRIDGE_PUBLIC_DOMAIN":
			return " perf.example.com "
		case "MUXBRIDGE_EDGE_ADDR":
			return " edge.perf.example.com:443 "
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

	if cfg.PublicHost != "flag.example.com" {
		t.Fatalf("PublicHost = %q, want %q", cfg.PublicHost, "flag.example.com")
	}
	if cfg.PublicDomain != "perf.example.com" {
		t.Fatalf("PublicDomain = %q, want %q", cfg.PublicDomain, "perf.example.com")
	}
	if cfg.EdgeAddr != "edge.perf.example.com:443" {
		t.Fatalf("EdgeAddr = %q, want %q", cfg.EdgeAddr, "edge.perf.example.com:443")
	}
	if cfg.Token != "flag-token" {
		t.Fatalf("Token = %q, want %q", cfg.Token, "flag-token")
	}
	if cfg.Connections != 12 {
		t.Fatalf("Connections = %d, want %d", cfg.Connections, 12)
	}
	if cfg.Duration != 45*time.Second {
		t.Fatalf("Duration = %s, want %s", cfg.Duration, 45*time.Second)
	}
	if cfg.Scenario != "fast" {
		t.Fatalf("Scenario = %q, want %q", cfg.Scenario, "fast")
	}
	if cfg.RequestTimeout != 4*time.Second {
		t.Fatalf("RequestTimeout = %s, want %s", cfg.RequestTimeout, 4*time.Second)
	}
	if cfg.ReadyTimeout != 6*time.Second {
		t.Fatalf("ReadyTimeout = %s, want %s", cfg.ReadyTimeout, 6*time.Second)
	}
	if cfg.Debug {
		t.Fatal("Debug = true, want false")
	}
}

func TestLoadConfigRequiresPublicHost(t *testing.T) {
	t.Parallel()

	_, err := loadConfig([]string{"--public-domain", "example.com"}, func(string) string { return "" })
	if err == nil || !strings.Contains(err.Error(), "public host is required") {
		t.Fatalf("loadConfig error = %v, want missing public host", err)
	}
}

func TestLoadConfigRequiresPublicDomainWhenEdgeAddrMissing(t *testing.T) {
	t.Parallel()

	_, err := loadConfig([]string{"--public-host", "perf.example.com"}, func(string) string { return "" })
	if err == nil || !strings.Contains(err.Error(), "public domain is required") {
		t.Fatalf("loadConfig error = %v, want missing public domain", err)
	}
}

func TestLoadConfigRejectsInvalidValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "invalid public host",
			args: []string{"--public-host", "localhost", "--public-domain", "example.com"},
			want: "invalid public host",
		},
		{
			name: "invalid scenario",
			args: []string{"--public-host", "perf.example.com", "--public-domain", "example.com", "--scenario", "burst"},
			want: "unknown scenario",
		},
		{
			name: "connections",
			args: []string{"--public-host", "perf.example.com", "--public-domain", "example.com", "--connections", "0"},
			want: "connections must be greater than zero",
		},
		{
			name: "duration",
			args: []string{"--public-host", "perf.example.com", "--public-domain", "example.com", "--duration", "0s"},
			want: "duration must be greater than zero",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := loadConfig(tc.args, func(string) string { return "" })
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("loadConfig error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestLoadScenario(t *testing.T) {
	t.Parallel()

	fast, err := loadScenario("fast")
	if err != nil {
		t.Fatalf("loadScenario fast error: %v", err)
	}
	if got := fast.requestFor(0, 3).Path; got != "/fast" {
		t.Fatalf("fast request path = %q, want %q", got, "/fast")
	}

	stream, err := loadScenario("stream")
	if err != nil {
		t.Fatalf("loadScenario stream error: %v", err)
	}
	if got := stream.requestFor(2, 9).Path; got != "/stream" {
		t.Fatalf("stream request path = %q, want %q", got, "/stream")
	}

	mixed, err := loadScenario("mixed")
	if err != nil {
		t.Fatalf("loadScenario mixed error: %v", err)
	}
	if len(mixed.Requests) != 20 {
		t.Fatalf("mixed request length = %d, want %d", len(mixed.Requests), 20)
	}

	counts := map[string]int{}
	for _, req := range mixed.Requests {
		counts[req.Path]++
	}
	if counts["/fast"] != 16 || counts["/bytes"] != 3 || counts["/stream"] != 1 {
		t.Fatalf("mixed counts = %#v, want fast=16 bytes=3 stream=1", counts)
	}
}

func TestNewPerfMuxHandlers(t *testing.T) {
	oldChunk := perfStreamChunk
	oldDelay := perfStreamChunkDelay
	oldCount := perfStreamChunkCount
	perfStreamChunk = []byte("xyz")
	perfStreamChunkDelay = 0
	perfStreamChunkCount = 3
	t.Cleanup(func() {
		perfStreamChunk = oldChunk
		perfStreamChunkDelay = oldDelay
		perfStreamChunkCount = oldCount
	})

	mux := newPerfMux()

	tests := []struct {
		path         string
		status       int
		contentType  string
		wantBody     []byte
		wantBodySize int
	}{
		{
			path:        "/healthz",
			status:      http.StatusOK,
			contentType: "text/plain",
			wantBody:    []byte("ok\n"),
		},
		{
			path:        "/fast",
			status:      http.StatusOK,
			contentType: "text/plain",
			wantBody:    perfFastBody,
		},
		{
			path:         "/bytes",
			status:       http.StatusOK,
			contentType:  "application/octet-stream",
			wantBodySize: len(perfBytesBody),
		},
		{
			path:         "/stream",
			status:       http.StatusOK,
			contentType:  "application/octet-stream",
			wantBodySize: len(perfStreamChunk) * perfStreamChunkCount,
		},
	}

	for _, tc := range tests {
		req := httptest.NewRequest(http.MethodGet, "http://perf.example.com"+tc.path, nil)
		res := httptest.NewRecorder()
		mux.ServeHTTP(res, req)

		if res.Code != tc.status {
			t.Fatalf("%s status = %d, want %d", tc.path, res.Code, tc.status)
		}
		if got := res.Header().Get("Content-Type"); !strings.Contains(got, tc.contentType) {
			t.Fatalf("%s content type = %q, want to contain %q", tc.path, got, tc.contentType)
		}
		if tc.wantBody != nil && res.Body.String() != string(tc.wantBody) {
			t.Fatalf("%s body = %q, want %q", tc.path, res.Body.Bytes(), tc.wantBody)
		}
		if tc.wantBodySize > 0 && res.Body.Len() != tc.wantBodySize {
			t.Fatalf("%s body length = %d, want %d", tc.path, res.Body.Len(), tc.wantBodySize)
		}
	}
}

func TestWaitForReadySuccess(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		if attempts.Add(1) < 3 {
			http.Error(w, "warming up", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "ok\n")
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := waitForReady(ctx, server.URL, newTestPublicClient(t, server, 200*time.Millisecond), 5*time.Millisecond, make(chan error)); err != nil {
		t.Fatalf("waitForReady error: %v", err)
	}
	if got := attempts.Load(); got < 3 {
		t.Fatalf("attempts = %d, want at least 3", got)
	}
}

func TestWaitForReadyReturnsTunnelError(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	tunnelErrCh := make(chan error, 1)
	tunnelErrCh <- errors.New("tunnel boom")

	client := &http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("dial failed")
		}),
	}

	err := waitForReady(ctx, "https://perf.example.com", client, 5*time.Millisecond, tunnelErrCh)
	if err == nil || !strings.Contains(err.Error(), "tunnel boom") {
		t.Fatalf("waitForReady error = %v, want tunnel error", err)
	}
}

func TestRunLoadCollectsMetrics(t *testing.T) {
	t.Parallel()

	server := httptest.NewTLSServer(newPerfMux())
	defer server.Close()

	scn, err := loadScenario("mixed")
	if err != nil {
		t.Fatalf("loadScenario error: %v", err)
	}

	summary, err := runLoad(context.Background(), loadRunConfig{
		BaseURL:        server.URL,
		PublicHost:     "perf.example.com",
		Scenario:       scn,
		Connections:    4,
		Duration:       80 * time.Millisecond,
		RequestTimeout: 500 * time.Millisecond,
	}, func(int) *http.Client {
		return newTestPublicClient(t, server, 500*time.Millisecond)
	})
	if err != nil {
		t.Fatalf("runLoad error: %v", err)
	}

	if summary.TotalRequests == 0 {
		t.Fatal("TotalRequests = 0, want > 0")
	}
	if summary.SuccessfulResponses == 0 {
		t.Fatal("SuccessfulResponses = 0, want > 0")
	}
	if summary.RequestErrors != 0 {
		t.Fatalf("RequestErrors = %d, want 0", summary.RequestErrors)
	}
	if summary.StatusCounts[http.StatusOK] != summary.TotalRequests {
		t.Fatalf("StatusCounts[200] = %d, want %d", summary.StatusCounts[http.StatusOK], summary.TotalRequests)
	}
	if summary.ResponseBytes == 0 {
		t.Fatal("ResponseBytes = 0, want > 0")
	}
	if !strings.Contains(summary.String(), "performance test summary") {
		t.Fatalf("summary string = %q, want summary header", summary.String())
	}
}

func TestRunLoadCountsRequestErrors(t *testing.T) {
	t.Parallel()

	scn, err := loadScenario("fast")
	if err != nil {
		t.Fatalf("loadScenario error: %v", err)
	}

	summary, err := runLoad(context.Background(), loadRunConfig{
		BaseURL:        "https://perf.example.com",
		PublicHost:     "perf.example.com",
		Scenario:       scn,
		Connections:    2,
		Duration:       30 * time.Millisecond,
		RequestTimeout: 100 * time.Millisecond,
	}, func(int) *http.Client {
		return &http.Client{
			Timeout: 100 * time.Millisecond,
			Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, errors.New("synthetic failure")
			}),
		}
	})
	if err != nil {
		t.Fatalf("runLoad error: %v", err)
	}

	if summary.RequestErrors == 0 {
		t.Fatal("RequestErrors = 0, want > 0")
	}
	if summary.SuccessfulResponses != 0 {
		t.Fatalf("SuccessfulResponses = %d, want 0", summary.SuccessfulResponses)
	}
}

func TestRunLoadUsesConcurrentWorkers(t *testing.T) {
	t.Parallel()

	var active atomic.Int32
	var maxActive atomic.Int32
	var releaseOnce sync.Once
	releaseCh := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		current := active.Add(1)
		defer active.Add(-1)

		for {
			seen := maxActive.Load()
			if current <= seen || maxActive.CompareAndSwap(seen, current) {
				break
			}
		}
		if current >= 4 {
			releaseOnce.Do(func() { close(releaseCh) })
		}

		select {
		case <-releaseCh:
		case <-time.After(100 * time.Millisecond):
		}

		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()

	scn, err := loadScenario("fast")
	if err != nil {
		t.Fatalf("loadScenario error: %v", err)
	}

	_, err = runLoad(context.Background(), loadRunConfig{
		BaseURL:        server.URL,
		PublicHost:     "perf.example.com",
		Scenario:       scn,
		Connections:    4,
		Duration:       50 * time.Millisecond,
		RequestTimeout: 300 * time.Millisecond,
	}, func(int) *http.Client {
		return newTestPublicClient(t, server, 300*time.Millisecond)
	})
	if err != nil {
		t.Fatalf("runLoad error: %v", err)
	}
	if got := maxActive.Load(); got < 4 {
		t.Fatalf("max concurrent requests = %d, want at least 4", got)
	}
}

func TestNewPublicClientDisablesHTTP2AndCapsConnections(t *testing.T) {
	t.Parallel()

	client := newPublicClient(3*time.Second, nil)
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport type = %T, want *http.Transport", client.Transport)
	}

	if client.Timeout != 3*time.Second {
		t.Fatalf("Timeout = %s, want %s", client.Timeout, 3*time.Second)
	}
	if transport.ForceAttemptHTTP2 {
		t.Fatal("ForceAttemptHTTP2 = true, want false")
	}
	if transport.MaxConnsPerHost != 1 {
		t.Fatalf("MaxConnsPerHost = %d, want 1", transport.MaxConnsPerHost)
	}
	if transport.MaxIdleConnsPerHost != 1 {
		t.Fatalf("MaxIdleConnsPerHost = %d, want 1", transport.MaxIdleConnsPerHost)
	}
	if len(transport.TLSNextProto) != 0 {
		t.Fatalf("TLSNextProto length = %d, want 0", len(transport.TLSNextProto))
	}
}

func TestGetenvTrimsWhitespace(t *testing.T) {
	key := "MUXBRIDGE_PERF_TEST_ENV"
	t.Setenv(key, "  value  ")
	if got := getenv(key); got != "value" {
		t.Fatalf("getenv = %q, want %q", got, "value")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func newTestPublicClient(t *testing.T, server *httptest.Server, requestTimeout time.Duration) *http.Client {
	t.Helper()

	baseClient := server.Client()
	transport, ok := baseClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("server transport type = %T, want *http.Transport", baseClient.Transport)
	}

	cloned := transport.Clone()
	if cloned.TLSClientConfig == nil {
		cloned.TLSClientConfig = &tls.Config{}
	}
	return newPublicClient(requestTimeout, cloned)
}
