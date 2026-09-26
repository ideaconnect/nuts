package nuts

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	_ "github.com/caddyserver/caddy/v2/modules/caddyhttp/standard"
	"github.com/nats-io/nats.go"
)

// loadCaddyWithNUTS runs a real Caddy server whose only route is a NUTS
// handler for the EVENTS stream. serverExtra is spliced into the server
// object (e.g. `"logs": {},`), handlerExtra into the handler object
// (e.g. `,"write_timeout": 5`). Returns the base URL.
func loadCaddyWithNUTS(t *testing.T, natsURL, serverExtra, handlerExtra string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	cfg := fmt.Sprintf(`{
		"admin": {"disabled": true},
		"logging": {"logs": {"default": {"level": "ERROR"}}},
		"apps": {"http": {"servers": {"nuts": {
			"listen": ["127.0.0.1:%d"],
			%s
			"routes": [{"handle": [{"handler": "nuts", "nats_url": %q, "stream_name": "EVENTS", "topic_prefix": "events."%s}]}]
		}}}}
	}`, port, serverExtra, natsURL, handlerExtra)
	if err := caddy.Load([]byte(cfg), true); err != nil {
		t.Fatalf("caddy.Load: %v", err)
	}
	t.Cleanup(func() { _ = caddy.Stop() })
	return fmt.Sprintf("http://127.0.0.1:%d", port)
}

// readSSEUntil reads lines from an SSE body until one has the given prefix
// or the deadline passes.
func readSSEUntil(t *testing.T, lines <-chan string, prefix string) string {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatalf("stream ended before a line starting with %q", prefix)
			}
			if strings.HasPrefix(line, prefix) {
				return line
			}
		case <-deadline:
			t.Fatalf("no line starting with %q within 3s", prefix)
		}
	}
}

// TestCaddy_StreamsWithAccessLogsAndHTTPMetrics is the regression test for
// #114: Caddy wraps the writer in a recorder without http.Flusher whenever
// access logs or HTTP metrics are enabled, which used to turn every stream
// into a 500 "Streaming not supported".
func TestCaddy_StreamsWithAccessLogsAndHTTPMetrics(t *testing.T) {
	ns := startJetStreamServer(t)
	defer ns.Shutdown()
	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()
	createTestStream(t, nc, "EVENTS", []string{"events.>"})
	js, _ := nc.JetStream()

	for _, tc := range []struct{ name, serverExtra string }{
		{name: "plain", serverExtra: ``},
		{name: "access logs", serverExtra: `"logs": {},`},
		{name: "http metrics", serverExtra: `"metrics": {},`},
		{name: "access logs and http metrics", serverExtra: `"logs": {}, "metrics": {},`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := loadCaddyWithNUTS(t, ns.ClientURL(), tc.serverExtra, "")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/alpha", nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
				t.Fatalf("Content-Type = %q, want text/event-stream", ct)
			}
			lines := make(chan string, 64)
			sc := bufio.NewScanner(resp.Body)
			go func() {
				defer close(lines)
				for sc.Scan() {
					lines <- sc.Text()
				}
			}()
			readSSEUntil(t, lines, "event: connected")
			if _, err := js.Publish("events.alpha", []byte(`{"n":1}`)); err != nil {
				t.Fatalf("publish: %v", err)
			}
			readSSEUntil(t, lines, "event: message")
			if got := readSSEUntil(t, lines, "data: "); !strings.Contains(got, `"payload":{"n":1}`) {
				t.Fatalf("data line = %q, want the published payload", got)
			}
		})
	}
}
