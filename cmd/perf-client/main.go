package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/define42/muxbridge/internal/hostnames"
	"github.com/define42/muxbridge/tunnel"
)

const (
	defaultPerfToken       = "perf-token"
	defaultConnections     = 1000
	defaultDuration        = 30 * time.Second
	defaultRequestTimeout  = 10 * time.Second
	defaultReadyTimeout    = 30 * time.Second
	defaultScenario        = "mixed"
	defaultBytesBodySize   = 64 * 1024
	latencyBucketWidth     = time.Millisecond
	latencyBucketLimit     = 60 * time.Second
	defaultReadyPollPeriod = 500 * time.Millisecond
)

var (
	perfFastBody         = []byte("muxbridge perf ok\n")
	perfBytesBody        = bytes.Repeat([]byte("b"), defaultBytesBodySize)
	perfStreamChunk      = bytes.Repeat([]byte("s"), 1024)
	perfStreamChunkCount = 8
	perfStreamChunkDelay = 10 * time.Millisecond
)

type perfConfig struct {
	PublicHost     string
	PublicDomain   string
	EdgeAddr       string
	Token          string
	Scenario       string
	Connections    int
	Duration       time.Duration
	RequestTimeout time.Duration
	ReadyTimeout   time.Duration
	Debug          bool
}

type loadRunConfig struct {
	BaseURL        string
	PublicHost     string
	Scenario       scenario
	Connections    int
	Duration       time.Duration
	RequestTimeout time.Duration
}

type requestSpec struct {
	Path string
}

type scenario struct {
	Name     string
	Requests []requestSpec
}

type requestResult struct {
	StatusCode int
	Bytes      int64
	Latency    time.Duration
	Err        error
}

type latencyHistogram struct {
	bucketWidth  time.Duration
	overflowFrom time.Duration
	buckets      []uint64
	total        uint64
}

type loadSummary struct {
	PublicHost          string
	Scenario            string
	Connections         int
	PlannedDuration     time.Duration
	StartedAt           time.Time
	EndedAt             time.Time
	TotalRequests       uint64
	SuccessfulResponses uint64
	RequestErrors       uint64
	ResponseBytes       uint64
	StatusCounts        map[int]uint64
	MinLatency          time.Duration
	MaxLatency          time.Duration
	LatencySum          time.Duration
	LatencyHistogram    latencyHistogram
}

func main() {
	if err := run(context.Background(), os.Args[1:], getenv); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, args []string, getenv func(string) string) error {
	cfg, err := loadConfig(args, getenv)
	if err != nil {
		return err
	}

	scn, err := loadScenario(cfg.Scenario)
	if err != nil {
		return err
	}

	cli, err := tunnel.New(tunnel.Config{
		EdgeAddr: cfg.EdgeAddr,
		Token:    cfg.Token,
		Handler:  newPerfMux(),
		Debug:    cfg.Debug,
		Logger:   log.Default(),
	})
	if err != nil {
		return err
	}
	if cfg.Debug {
		log.Printf(
			"perf client debug enabled: edge_addr=%s public_host=%s scenario=%s connections=%d duration=%s request_timeout=%s ready_timeout=%s",
			cfg.EdgeAddr,
			cfg.PublicHost,
			scn.Name,
			cfg.Connections,
			cfg.Duration,
			cfg.RequestTimeout,
			cfg.ReadyTimeout,
		)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	tunnelErrCh := make(chan error, 1)
	go func() {
		tunnelErrCh <- cli.Run(runCtx)
	}()

	readyCtx, readyCancel := context.WithTimeout(runCtx, cfg.ReadyTimeout)
	defer readyCancel()

	readyClient := newPublicClient(cfg.RequestTimeout, nil)
	if err := waitForReady(readyCtx, cfg.publicBaseURL(), readyClient, defaultReadyPollPeriod, tunnelErrCh); err != nil {
		cancel()
		_ = cli.Close()
		waitForClientExit(tunnelErrCh, 2*time.Second)
		return fmt.Errorf("public host %s did not become ready: %w", cfg.PublicHost, err)
	}

	summary, err := runLoad(runCtx, loadRunConfig{
		BaseURL:        cfg.publicBaseURL(),
		PublicHost:     cfg.PublicHost,
		Scenario:       scn,
		Connections:    cfg.Connections,
		Duration:       cfg.Duration,
		RequestTimeout: cfg.RequestTimeout,
	}, func(int) *http.Client {
		return newPublicClient(cfg.RequestTimeout, nil)
	})

	cancel()
	_ = cli.Close()
	if tunnelErr := waitForClientExit(tunnelErrCh, 2*time.Second); tunnelErr != nil && cfg.Debug {
		log.Printf("perf client debug: tunnel exited after load: %v", tunnelErr)
	}

	if err != nil {
		return err
	}

	fmt.Print(summary.String())
	return nil
}

func loadConfig(args []string, getenv func(string) string) (perfConfig, error) {
	fs := flag.NewFlagSet("perf-client", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	cfg := perfConfig{
		PublicHost:     getenv("MUXBRIDGE_PUBLIC_HOST"),
		PublicDomain:   getenv("MUXBRIDGE_PUBLIC_DOMAIN"),
		EdgeAddr:       getenv("MUXBRIDGE_EDGE_ADDR"),
		Token:          defaultString(getenv("MUXBRIDGE_CLIENT_TOKEN"), defaultPerfToken),
		Scenario:       defaultScenario,
		Connections:    defaultConnections,
		Duration:       defaultDuration,
		RequestTimeout: defaultRequestTimeout,
		ReadyTimeout:   defaultReadyTimeout,
		Debug:          parseBoolString(getenv("MUXBRIDGE_DEBUG")),
	}

	fs.StringVar(&cfg.PublicHost, "public-host", cfg.PublicHost, "Public hostname to probe and load-test")
	fs.StringVar(&cfg.PublicDomain, "public-domain", cfg.PublicDomain, "Public base domain for the edge")
	fs.StringVar(&cfg.EdgeAddr, "edge-addr", cfg.EdgeAddr, "Edge gRPC address")
	fs.StringVar(&cfg.Token, "token", cfg.Token, "Client authentication token")
	fs.IntVar(&cfg.Connections, "connections", cfg.Connections, "Concurrent public connections to keep active")
	fs.DurationVar(&cfg.Duration, "duration", cfg.Duration, "How long to sustain the load test")
	fs.StringVar(&cfg.Scenario, "scenario", cfg.Scenario, "Load scenario: fast, stream, mixed")
	fs.DurationVar(&cfg.RequestTimeout, "request-timeout", cfg.RequestTimeout, "Per-request timeout for public load traffic")
	fs.DurationVar(&cfg.ReadyTimeout, "ready-timeout", cfg.ReadyTimeout, "How long to wait for the public host to become reachable")
	fs.BoolVar(&cfg.Debug, "debug", cfg.Debug, "Enable debug logging")
	if err := fs.Parse(args); err != nil {
		return perfConfig{}, err
	}

	cfg.PublicHost = hostnames.NormalizeHost(cfg.PublicHost)
	cfg.PublicDomain = hostnames.NormalizeDomain(cfg.PublicDomain)
	cfg.EdgeAddr = strings.TrimSpace(cfg.EdgeAddr)
	cfg.Token = strings.TrimSpace(cfg.Token)
	cfg.Scenario = strings.ToLower(strings.TrimSpace(cfg.Scenario))

	if cfg.PublicHost == "" {
		return perfConfig{}, fmt.Errorf("public host is required")
	}
	if err := hostnames.ValidateDomain(cfg.PublicHost); err != nil {
		return perfConfig{}, fmt.Errorf("invalid public host: %w", err)
	}

	if cfg.EdgeAddr == "" {
		if cfg.PublicDomain == "" {
			return perfConfig{}, fmt.Errorf("public domain is required when edge addr is not provided")
		}
		if err := hostnames.ValidateDomain(cfg.PublicDomain); err != nil {
			return perfConfig{}, fmt.Errorf("invalid public domain: %w", err)
		}
		cfg.EdgeAddr = hostnames.Subdomain("edge", cfg.PublicDomain) + ":443"
	}

	if cfg.Connections <= 0 {
		return perfConfig{}, fmt.Errorf("connections must be greater than zero")
	}
	if cfg.Duration <= 0 {
		return perfConfig{}, fmt.Errorf("duration must be greater than zero")
	}
	if cfg.RequestTimeout <= 0 {
		return perfConfig{}, fmt.Errorf("request timeout must be greater than zero")
	}
	if cfg.ReadyTimeout <= 0 {
		return perfConfig{}, fmt.Errorf("ready timeout must be greater than zero")
	}
	if _, err := loadScenario(cfg.Scenario); err != nil {
		return perfConfig{}, err
	}

	return cfg, nil
}

func (c perfConfig) publicBaseURL() string {
	return "https://" + c.PublicHost
}

func loadScenario(name string) (scenario, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "fast":
		return scenario{
			Name: "fast",
			Requests: []requestSpec{
				{Path: "/fast"},
			},
		}, nil
	case "stream":
		return scenario{
			Name: "stream",
			Requests: []requestSpec{
				{Path: "/stream"},
			},
		}, nil
	case "", "mixed":
		requests := make([]requestSpec, 0, 20)
		for i := 0; i < 16; i++ {
			requests = append(requests, requestSpec{Path: "/fast"})
		}
		for i := 0; i < 3; i++ {
			requests = append(requests, requestSpec{Path: "/bytes"})
		}
		requests = append(requests, requestSpec{Path: "/stream"})
		return scenario{Name: "mixed", Requests: requests}, nil
	default:
		return scenario{}, fmt.Errorf("unknown scenario %q", name)
	}
}

func (s scenario) requestFor(workerID, iteration int) requestSpec {
	if len(s.Requests) == 0 {
		return requestSpec{Path: "/fast"}
	}
	return s.Requests[(workerID+iteration)%len(s.Requests)]
}

func newPerfMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("/fast", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Length", strconv.Itoa(len(perfFastBody)))
		_, _ = w.Write(perfFastBody)
	})
	mux.HandleFunc("/bytes", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(perfBytesBody)))
		_, _ = w.Write(perfBytesBody)
	})
	mux.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/octet-stream")
		for i := 0; i < perfStreamChunkCount; i++ {
			if _, err := w.Write(perfStreamChunk); err != nil {
				return
			}
			flusher.Flush()
			if perfStreamChunkDelay > 0 {
				select {
				case <-r.Context().Done():
					return
				case <-time.After(perfStreamChunkDelay):
				}
			}
		}
	})
	return mux
}

func waitForReady(
	ctx context.Context,
	baseURL string,
	client *http.Client,
	pollInterval time.Duration,
	tunnelErrCh <-chan error,
) error {
	if pollInterval <= 0 {
		pollInterval = defaultReadyPollPeriod
	}

	healthURL := strings.TrimRight(baseURL, "/") + "/healthz"
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
		if err != nil {
			return err
		}

		resp, err := client.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			err = fmt.Errorf("health check returned %s", resp.Status)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case tunnelErr := <-tunnelErrCh:
			if tunnelErr == nil {
				return errors.New("tunnel client exited before public host became ready")
			}
			return fmt.Errorf("tunnel client exited before public host became ready: %w", tunnelErr)
		case <-time.After(pollInterval):
		}
	}
}

func runLoad(ctx context.Context, cfg loadRunConfig, clientFactory func(int) *http.Client) (loadSummary, error) {
	if cfg.Connections <= 0 {
		return loadSummary{}, fmt.Errorf("connections must be greater than zero")
	}
	if cfg.Duration <= 0 {
		return loadSummary{}, fmt.Errorf("duration must be greater than zero")
	}
	if clientFactory == nil {
		return loadSummary{}, fmt.Errorf("client factory is required")
	}

	clients := make([]*http.Client, cfg.Connections)
	for workerID := 0; workerID < cfg.Connections; workerID++ {
		client := clientFactory(workerID)
		if client == nil {
			for _, created := range clients[:workerID] {
				closeIdleConnections(created)
			}
			return loadSummary{}, fmt.Errorf("client factory returned nil for worker %d", workerID)
		}
		clients[workerID] = client
	}

	startedAt := time.Now()
	stopCh := make(chan struct{})
	timer := time.NewTimer(cfg.Duration)
	defer timer.Stop()

	go func() {
		select {
		case <-ctx.Done():
		case <-timer.C:
			close(stopCh)
		}
	}()

	results := make(chan requestResult, cfg.Connections*2)
	var wg sync.WaitGroup

	for workerID, client := range clients {
		wg.Add(1)
		go func(workerID int, client *http.Client) {
			defer wg.Done()
			defer closeIdleConnections(client)

			for iteration := 0; ; iteration++ {
				select {
				case <-ctx.Done():
					return
				case <-stopCh:
					return
				default:
				}

				spec := cfg.Scenario.requestFor(workerID, iteration)
				result := doRequest(ctx, client, strings.TrimRight(cfg.BaseURL, "/")+spec.Path)

				select {
				case results <- result:
				case <-ctx.Done():
					return
				}
			}
		}(workerID, client)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	summary := newLoadSummary(cfg, startedAt)
	for result := range results {
		summary.Record(result)
	}
	summary.EndedAt = time.Now()
	return summary, nil
}

func doRequest(ctx context.Context, client *http.Client, targetURL string) requestResult {
	startedAt := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return requestResult{Latency: time.Since(startedAt), Err: err}
	}

	resp, err := client.Do(req)
	if err != nil {
		return requestResult{Latency: time.Since(startedAt), Err: err}
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	written, readErr := io.Copy(io.Discard, resp.Body)
	return requestResult{
		StatusCode: resp.StatusCode,
		Bytes:      written,
		Latency:    time.Since(startedAt),
		Err:        readErr,
	}
}

func newPublicClient(requestTimeout time.Duration, transport *http.Transport) *http.Client {
	if transport == nil {
		transport = newHTTP11Transport(requestTimeout)
	} else {
		transport = transport.Clone()
		transport.ForceAttemptHTTP2 = false
		transport.MaxIdleConns = 1
		transport.MaxIdleConnsPerHost = 1
		transport.MaxConnsPerHost = 1
		transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
		if transport.ResponseHeaderTimeout <= 0 {
			transport.ResponseHeaderTimeout = requestTimeout
		}
		if transport.TLSHandshakeTimeout <= 0 {
			transport.TLSHandshakeTimeout = requestTimeout
		}
		if transport.IdleConnTimeout <= 0 {
			transport.IdleConnTimeout = 30 * time.Second
		}
	}

	return &http.Client{
		Transport: transport,
		Timeout:   requestTimeout,
	}
}

func newHTTP11Transport(requestTimeout time.Duration) *http.Transport {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: requestTimeout, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          1,
		MaxIdleConnsPerHost:   1,
		MaxConnsPerHost:       1,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   requestTimeout,
		ResponseHeaderTimeout: requestTimeout,
		ExpectContinueTimeout: time.Second,
		TLSNextProto:          map[string]func(string, *tls.Conn) http.RoundTripper{},
	}
}

func closeIdleConnections(client *http.Client) {
	type idleCloser interface {
		CloseIdleConnections()
	}

	if closer, ok := client.Transport.(idleCloser); ok {
		closer.CloseIdleConnections()
	}
}

func newLoadSummary(cfg loadRunConfig, startedAt time.Time) loadSummary {
	return loadSummary{
		PublicHost:       cfg.PublicHost,
		Scenario:         cfg.Scenario.Name,
		Connections:      cfg.Connections,
		PlannedDuration:  cfg.Duration,
		StartedAt:        startedAt,
		StatusCounts:     make(map[int]uint64),
		LatencyHistogram: newLatencyHistogram(latencyBucketWidth, latencyBucketLimit),
	}
}

func (s *loadSummary) Record(result requestResult) {
	s.TotalRequests++
	s.LatencySum += result.Latency
	s.LatencyHistogram.Observe(result.Latency)
	if s.MinLatency == 0 || result.Latency < s.MinLatency {
		s.MinLatency = result.Latency
	}
	if result.Latency > s.MaxLatency {
		s.MaxLatency = result.Latency
	}

	if result.StatusCode > 0 {
		s.StatusCounts[result.StatusCode]++
	}
	if result.Bytes > 0 {
		s.ResponseBytes += uint64(result.Bytes)
	}
	if result.Err != nil {
		s.RequestErrors++
		return
	}
	s.SuccessfulResponses++
}

func (s loadSummary) String() string {
	var b strings.Builder

	elapsed := s.elapsed()
	fmt.Fprintf(&b, "performance test summary\n")
	fmt.Fprintf(&b, "host: %s\n", s.PublicHost)
	fmt.Fprintf(&b, "scenario: %s\n", s.Scenario)
	fmt.Fprintf(&b, "connections: %d\n", s.Connections)
	fmt.Fprintf(&b, "duration: planned=%s observed=%s\n", humanDuration(s.PlannedDuration), humanDuration(elapsed))
	fmt.Fprintf(&b, "requests: total=%d success=%d errors=%d\n", s.TotalRequests, s.SuccessfulResponses, s.RequestErrors)
	fmt.Fprintf(&b, "throughput: req/s=%.2f bytes/s=%.2f\n", requestsPerSecond(s.TotalRequests, elapsed), bytesPerSecond(s.ResponseBytes, elapsed))

	if s.TotalRequests > 0 {
		fmt.Fprintf(
			&b,
			"latency: min=%s avg=%s p50=%s p95=%s p99=%s max=%s\n",
			humanDuration(s.MinLatency),
			humanDuration(s.avgLatency()),
			humanDuration(s.LatencyHistogram.Percentile(50)),
			humanDuration(s.LatencyHistogram.Percentile(95)),
			humanDuration(s.LatencyHistogram.Percentile(99)),
			humanDuration(s.MaxLatency),
		)
	}

	if len(s.StatusCounts) > 0 {
		fmt.Fprintf(&b, "statuses: %s\n", formatStatusCounts(s.StatusCounts))
	}

	return b.String()
}

func (s loadSummary) elapsed() time.Duration {
	if s.EndedAt.After(s.StartedAt) {
		return s.EndedAt.Sub(s.StartedAt)
	}
	return s.PlannedDuration
}

func (s loadSummary) avgLatency() time.Duration {
	if s.TotalRequests == 0 {
		return 0
	}
	return time.Duration(int64(s.LatencySum) / int64(s.TotalRequests))
}

func formatStatusCounts(statusCounts map[int]uint64) string {
	statuses := make([]int, 0, len(statusCounts))
	for status := range statusCounts {
		statuses = append(statuses, status)
	}
	sort.Ints(statuses)

	parts := make([]string, 0, len(statuses))
	for _, status := range statuses {
		parts = append(parts, fmt.Sprintf("%d=%d", status, statusCounts[status]))
	}
	return strings.Join(parts, " ")
}

func requestsPerSecond(total uint64, elapsed time.Duration) float64 {
	if elapsed <= 0 {
		return 0
	}
	return float64(total) / elapsed.Seconds()
}

func bytesPerSecond(total uint64, elapsed time.Duration) float64 {
	if elapsed <= 0 {
		return 0
	}
	return float64(total) / elapsed.Seconds()
}

func newLatencyHistogram(bucketWidth, overflowFrom time.Duration) latencyHistogram {
	bucketCount := int(overflowFrom / bucketWidth)
	if overflowFrom%bucketWidth != 0 {
		bucketCount++
	}
	return latencyHistogram{
		bucketWidth:  bucketWidth,
		overflowFrom: overflowFrom,
		buckets:      make([]uint64, bucketCount+1),
	}
}

func (h *latencyHistogram) Observe(latency time.Duration) {
	if latency < 0 {
		latency = 0
	}

	index := len(h.buckets) - 1
	if latency < h.overflowFrom {
		index = int(latency / h.bucketWidth)
	}
	h.buckets[index]++
	h.total++
}

func (h latencyHistogram) Percentile(percent float64) time.Duration {
	if h.total == 0 {
		return 0
	}
	if percent <= 0 {
		return 0
	}
	if percent >= 100 {
		percent = 100
	}

	target := uint64(math.Ceil(percent / 100 * float64(h.total)))
	if target == 0 {
		target = 1
	}

	var seen uint64
	for index, count := range h.buckets {
		seen += count
		if seen < target {
			continue
		}
		if index == len(h.buckets)-1 {
			return h.overflowFrom
		}
		return time.Duration(index) * h.bucketWidth
	}
	return h.overflowFrom
}

func humanDuration(value time.Duration) string {
	if value <= 0 {
		return "0s"
	}
	return value.Round(time.Microsecond).String()
}

func waitForClientExit(errCh <-chan error, timeout time.Duration) error {
	if errCh == nil {
		return nil
	}

	select {
	case err := <-errCh:
		if err == nil || errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	case <-time.After(timeout):
		return nil
	}
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func parseBoolString(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "t", "true", "y", "yes", "on":
		return true
	default:
		return false
	}
}

func getenv(key string) string {
	return strings.TrimSpace(os.Getenv(key))
}
