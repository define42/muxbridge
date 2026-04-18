package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/define42/muxbridge/internal/hostnames"
	"github.com/define42/muxbridge/tunnel"
	"golang.org/x/net/websocket"
)

type demoConfig struct {
	PublicDomain string
	EdgeAddr     string
	Token        string
	Debug        bool
}

var slowChunkDelay = 500 * time.Millisecond
var sseTickInterval = time.Second

type demoTunnelClient interface {
	Run(context.Context) error
}

var newDemoTunnelClient = func(cfg tunnel.Config) (demoTunnelClient, error) {
	return tunnel.New(cfg)
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

	mux := newDemoMux()

	cli, err := newDemoTunnelClient(tunnel.Config{
		EdgeAddr: cfg.EdgeAddr,
		Token:    cfg.Token,
		Handler:  mux,
		Debug:    cfg.Debug,
		Logger:   log.Default(),
	})
	if err != nil {
		return err
	}
	if cfg.Debug {
		reconnectBackoff := 2 * time.Second
		dialTimeout := 10 * time.Second
		log.Printf(
			"client debug enabled: edge_addr=%s public_domain=%s reconnect_backoff=%s dial_timeout=%s",
			cfg.EdgeAddr,
			cfg.PublicDomain,
			reconnectBackoff,
			dialTimeout,
		)
	}

	return cli.Run(ctx)
}

func loadConfig(args []string, getenv func(string) string) (demoConfig, error) {
	fs := flag.NewFlagSet("demo-client", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	cfg := demoConfig{
		PublicDomain: getenv("MUXBRIDGE_PUBLIC_DOMAIN"),
		EdgeAddr:     getenv("MUXBRIDGE_EDGE_ADDR"),
		Token:        defaultString(getenv("MUXBRIDGE_CLIENT_TOKEN"), "demo-token"),
		Debug:        parseBoolString(getenv("MUXBRIDGE_DEBUG")),
	}

	fs.StringVar(&cfg.PublicDomain, "public-domain", cfg.PublicDomain, "Public base domain for the edge")
	fs.StringVar(&cfg.EdgeAddr, "edge-addr", cfg.EdgeAddr, "Edge gRPC address")
	fs.StringVar(&cfg.Token, "token", cfg.Token, "Client authentication token")
	fs.BoolVar(&cfg.Debug, "debug", cfg.Debug, "Enable debug logging")
	if err := fs.Parse(args); err != nil {
		return demoConfig{}, err
	}

	cfg.PublicDomain = strings.TrimSpace(cfg.PublicDomain)
	cfg.EdgeAddr = strings.TrimSpace(cfg.EdgeAddr)
	cfg.Token = strings.TrimSpace(cfg.Token)

	if cfg.EdgeAddr == "" {
		domain := hostnames.NormalizeDomain(cfg.PublicDomain)
		if domain == "" {
			return demoConfig{}, fmt.Errorf("public domain is required when edge addr is not provided")
		}
		if err := hostnames.ValidateDomain(domain); err != nil {
			return demoConfig{}, fmt.Errorf("invalid public domain: %w", err)
		}
		cfg.PublicDomain = domain
		cfg.EdgeAddr = hostnames.Subdomain("edge", domain) + ":443"
	}

	return cfg, nil
}

func newDemoMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = fmt.Fprintf(w, "hello through grpc tunnel\nremote ip: %s\n", remoteIPFromAddr(r.RemoteAddr))
	})
	mux.HandleFunc("/ws-demo", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, websocketDemoPage)
	})
	mux.HandleFunc("/sse-demo", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, sseDemoPage)
	})
	mux.HandleFunc("/sse-demo/events", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		remoteIP := remoteIPFromAddr(r.RemoteAddr)
		writeSSEEvent(w, "connected", "remote ip: "+remoteIP)
		flusher.Flush()

		ticker := time.NewTicker(sseTickInterval)
		defer ticker.Stop()

		tick := 0
		for {
			select {
			case <-r.Context().Done():
				return
			case ts := <-ticker.C:
				tick++
				writeSSEEvent(w, "tick", fmt.Sprintf("tick %d at %s from %s", tick, ts.UTC().Format(time.RFC3339), remoteIP))
				flusher.Flush()
			}
		}
	})
	mux.Handle("/ws-demo/socket", websocket.Handler(func(conn *websocket.Conn) {
		defer func() {
			_ = conn.Close()
		}()

		remoteIP := remoteIPFromAddr(conn.Request().RemoteAddr)
		if err := websocket.Message.Send(conn, "connected from "+remoteIP); err != nil {
			return
		}

		for {
			var message string
			if err := websocket.Message.Receive(conn, &message); err != nil {
				return
			}
			reply := fmt.Sprintf("echo from %s: %s", remoteIP, strings.TrimSpace(message))
			if err := websocket.Message.Send(conn, reply); err != nil {
				return
			}
		}
	}))
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		for i := 0; i < 5; i++ {
			_, _ = fmt.Fprintf(w, "chunk %d\n", i+1)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(slowChunkDelay)
		}
	})
	return mux
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

func remoteIPFromAddr(remoteAddr string) string {
	remoteAddr = strings.TrimSpace(remoteAddr)
	if remoteAddr == "" {
		return "unknown"
	}
	host, _, err := net.SplitHostPort(remoteAddr)
	if err == nil && host != "" {
		return host
	}
	return remoteAddr
}

func writeSSEEvent(w io.Writer, eventName, data string) {
	if eventName != "" {
		_, _ = fmt.Fprintf(w, "event: %s\n", eventName)
	}
	for _, line := range strings.Split(data, "\n") {
		_, _ = fmt.Fprintf(w, "data: %s\n", line)
	}
	_, _ = io.WriteString(w, "\n")
}

func getenv(key string) string {
	return strings.TrimSpace(os.Getenv(key))
}

const websocketDemoPage = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>MuxBridge WebSocket Demo</title>
  <style>
    :root {
      color-scheme: light dark;
      font-family: system-ui, sans-serif;
    }
    body {
      margin: 0;
      padding: 24px;
      background: #111827;
      color: #f3f4f6;
    }
    main {
      max-width: 720px;
      margin: 0 auto;
    }
    h1 {
      margin: 0 0 8px;
      font-size: 2rem;
    }
    p {
      margin: 0 0 16px;
      color: #d1d5db;
    }
    form {
      display: flex;
      gap: 8px;
      margin: 16px 0;
    }
    input, button {
      font: inherit;
      border-radius: 6px;
      border: 1px solid #4b5563;
      padding: 10px 12px;
    }
    input {
      flex: 1;
      background: #030712;
      color: inherit;
    }
    button {
      background: #2563eb;
      color: white;
      cursor: pointer;
    }
    pre {
      min-height: 220px;
      padding: 12px;
      border-radius: 6px;
      background: #030712;
      border: 1px solid #374151;
      overflow: auto;
      white-space: pre-wrap;
    }
    .status {
      color: #93c5fd;
      font-size: 0.95rem;
    }
  </style>
</head>
<body>
  <main>
    <h1>WebSocket Demo</h1>
    <p>Open a live socket to the demo client, send a message, and watch the echo come back through the tunnel.</p>
    <div class="status" id="status">connecting...</div>
    <form id="chat-form">
      <input id="message" name="message" autocomplete="off" value="hello websocket" aria-label="Message">
      <button type="submit">Send</button>
    </form>
    <pre id="log"></pre>
  </main>
  <script>
    const statusEl = document.getElementById('status');
    const logEl = document.getElementById('log');
    const formEl = document.getElementById('chat-form');
    const inputEl = document.getElementById('message');
    const socketUrl = (location.protocol === 'https:' ? 'wss://' : 'ws://') + location.host + '/ws-demo/socket';
    const socket = new WebSocket(socketUrl);

    function appendLog(line) {
      logEl.textContent += line + '\n';
      logEl.scrollTop = logEl.scrollHeight;
    }

    socket.addEventListener('open', () => {
      statusEl.textContent = 'connected: ' + socketUrl;
      appendLog('socket open');
    });

    socket.addEventListener('message', (event) => {
      appendLog('server: ' + event.data);
    });

    socket.addEventListener('close', () => {
      statusEl.textContent = 'socket closed';
      appendLog('socket closed');
    });

    socket.addEventListener('error', () => {
      statusEl.textContent = 'socket error';
      appendLog('socket error');
    });

    formEl.addEventListener('submit', (event) => {
      event.preventDefault();
      const value = inputEl.value.trim();
      if (!value) {
        return;
      }
      appendLog('client: ' + value);
      socket.send(value);
      inputEl.select();
    });
  </script>
</body>
</html>
`

const sseDemoPage = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>MuxBridge SSE Demo</title>
  <style>
    :root {
      color-scheme: light dark;
      font-family: system-ui, sans-serif;
    }
    body {
      margin: 0;
      padding: 24px;
      background: #111827;
      color: #f3f4f6;
    }
    main {
      max-width: 720px;
      margin: 0 auto;
    }
    h1 {
      margin: 0 0 8px;
      font-size: 2rem;
    }
    p {
      margin: 0 0 16px;
      color: #d1d5db;
    }
    .status {
      color: #93c5fd;
      font-size: 0.95rem;
      margin-bottom: 12px;
    }
    pre {
      min-height: 220px;
      padding: 12px;
      border-radius: 6px;
      background: #030712;
      border: 1px solid #374151;
      overflow: auto;
      white-space: pre-wrap;
    }
  </style>
</head>
<body>
  <main>
    <h1>SSE Demo</h1>
    <p>Watch a server-sent event stream come through the tunnel without polling.</p>
    <div class="status" id="status">connecting...</div>
    <pre id="log"></pre>
  </main>
  <script>
    const statusEl = document.getElementById('status');
    const logEl = document.getElementById('log');
    const streamUrl = '/sse-demo/events';
    const source = new EventSource(streamUrl);

    function appendLog(line) {
      logEl.textContent += line + '\n';
      logEl.scrollTop = logEl.scrollHeight;
    }

    source.addEventListener('open', () => {
      statusEl.textContent = 'connected: ' + streamUrl;
      appendLog('stream open');
    });

    source.addEventListener('connected', (event) => {
      appendLog('server: ' + event.data);
    });

    source.addEventListener('tick', (event) => {
      appendLog('tick: ' + event.data);
    });

    source.addEventListener('error', () => {
      statusEl.textContent = 'stream reconnecting...';
      appendLog('stream error');
    });
  </script>
</body>
</html>
`
