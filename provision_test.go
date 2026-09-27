package nuts

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestHandler_Validate(t *testing.T) {
	tests := []struct {
		name        string
		handler     *Handler
		expectError bool
		errorMsg    string
	}{
		{
			name: "valid configuration",
			handler: &Handler{
				NatsURL:    "nats://localhost:4222",
				StreamName: "EVENTS",
			},
			expectError: false,
		},
		{
			name: "missing nats_url returns error",
			handler: &Handler{
				StreamName: "EVENTS",
			},
			expectError: true,
			errorMsg:    "nats_url is required",
		},
		{
			name: "missing stream_name",
			handler: &Handler{
				NatsURL: "nats://localhost:4222",
			},
			expectError: true,
			errorMsg:    "stream_name is required",
		},
		{
			name:        "missing both",
			handler:     &Handler{},
			expectError: true,
			errorMsg:    "nats_url is required",
		},
		{
			name: "conflicting authentication methods",
			handler: &Handler{
				NatsURL:         "nats://localhost:4222",
				StreamName:      "EVENTS",
				NatsCredentials: "/tmp/test.creds",
				NatsToken:       "token",
			},
			expectError: true,
			errorMsg:    "only one NATS authentication method can be configured",
		},
		{
			name: "partial user password auth",
			handler: &Handler{
				NatsURL:    "nats://localhost:4222",
				StreamName: "EVENTS",
				NatsUser:   "user-only",
			},
			expectError: true,
			errorMsg:    "nats_user and nats_password must be provided together",
		},
		{
			name: "valid user password auth",
			handler: &Handler{
				NatsURL:      "nats://localhost:4222",
				StreamName:   "EVENTS",
				NatsUser:     "user",
				NatsPassword: "password",
			},
			expectError: false,
		},
		{
			name: "wildcard origins with auth still validate",
			handler: &Handler{
				NatsURL:        "nats://localhost:4222",
				StreamName:     "EVENTS",
				NatsToken:      "token",
				AllowedOrigins: []string{"*"},
				logger:         zap.NewNop(),
			},
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.handler.Validate()
			if tt.expectError {
				if err == nil {
					t.Error("expected error, got nil")
				} else if !strings.Contains(err.Error(), tt.errorMsg) {
					t.Errorf("expected error containing %q, got %q", tt.errorMsg, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			}
		})
	}
}

func TestHandler_Provision(t *testing.T) {
	t.Run("success applies defaults and initializes jetstream", func(t *testing.T) {
		ns := startJetStreamServer(t)

		nc, err := nats.Connect(ns.ClientURL())
		if err != nil {
			t.Fatalf("failed to connect to NATS: %v", err)
		}
		defer nc.Close()

		createTestStream(t, nc, "TEST_EVENTS", []string{"events.>"})

		h := &Handler{
			NatsURL:    ns.ClientURL(),
			StreamName: "TEST_EVENTS",
		}

		ctx := caddy.Context{Context: context.Background()}
		if err := h.Provision(ctx); err != nil {
			t.Fatalf("Provision returned error: %v", err)
		}
		defer h.Cleanup()

		if h.HeartbeatInterval != 30 {
			t.Errorf("expected default heartbeat interval 30, got %d", h.HeartbeatInterval)
		}
		if h.ReconnectWait != 2 {
			t.Errorf("expected default reconnect wait 2, got %d", h.ReconnectWait)
		}
		if h.MaxReconnects == nil || *h.MaxReconnects != -1 {
			t.Errorf("expected default max reconnects -1, got %v", h.MaxReconnects)
		}
		if h.MaxEventSize != 1048576 {
			t.Errorf("expected default max event size 1048576, got %d", h.MaxEventSize)
		}
		if len(h.AllowedOrigins) != 1 || h.AllowedOrigins[0] != "*" {
			t.Errorf("expected default allowed origins [*], got %#v", h.AllowedOrigins)
		}

		h.mu.RLock()
		connNil := h.conn == nil
		jsNil := h.js == nil
		h.mu.RUnlock()
		if connNil {
			t.Fatal("expected Provision to initialize NATS connection")
		}
		if jsNil {
			t.Fatal("expected Provision to initialize JetStream context")
		}
		// #133: stream reads feed the watch, which confirms a rewind over
		// the watch interval.
		if h.watch == nil || h.watch.confirm != streamWatchInterval/2 || h.streamReads.observe == nil {
			t.Fatalf("expected Provision to watch the stream and confirm rewinds over %v", streamWatchInterval/2)
		}
	})

	t.Run("connect failure is wrapped", func(t *testing.T) {
		h := &Handler{
			NatsURL:    "nats://127.0.0.1:1",
			StreamName: "EVENTS",
		}

		ctx := caddy.Context{Context: context.Background()}
		err := h.Provision(ctx)
		if err == nil {
			t.Fatal("expected Provision to fail")
		}
		if !strings.Contains(err.Error(), "failed to connect to NATS") {
			t.Fatalf("expected wrapped connect error, got %v", err)
		}
		// The failure path runs Cleanup (#68): nothing Provision created may
		// be left behind for a hot reload to trip over.
		h.mu.RLock()
		shutdownNil, connNil, jsNil := h.shutdown == nil, h.conn == nil, h.js == nil
		h.mu.RUnlock()
		if !shutdownNil || !connNil || !jsNil {
			t.Fatalf("after a failed connect: shutdown nil=%v conn nil=%v js nil=%v, want all nil", shutdownNil, connNil, jsNil)
		}
	})

	t.Run("missing stream leaves nothing behind", func(t *testing.T) {
		ns := startJetStreamServer(t)
		h := &Handler{NatsURL: ns.ClientURL(), StreamName: "MISSING_STREAM"}
		err := h.Provision(caddy.Context{Context: context.Background()})
		if err == nil || !strings.Contains(err.Error(), "not found") {
			t.Fatalf("Provision = %v, want a stream-not-found error", err)
		}
		// Provision had connected before the stream lookup failed.
		h.mu.RLock()
		shutdownNil, connNil, jsNil := h.shutdown == nil, h.conn == nil, h.js == nil
		h.mu.RUnlock()
		if !shutdownNil || !connNil || !jsNil {
			t.Fatalf("after a failed stream lookup: shutdown nil=%v conn nil=%v js nil=%v, want all nil", shutdownNil, connNil, jsNil)
		}
	})
}

// TestHandler_connectNATS_RejectsInvalidCredentialsFile: with a reachable
// server, the only reason to fail is the credentials file itself. Token and
// user/password wiring are covered against servers that require them
// (TestHandler_ConnectNATS_TokenAuth_Integration and _UserPassAuth_).
func TestHandler_connectNATS_RejectsInvalidCredentialsFile(t *testing.T) {
	ns := startJetStreamServer(t)
	credsPath := t.TempDir() + "/user.creds"
	if err := os.WriteFile(credsPath, []byte("invalid creds"), 0600); err != nil {
		t.Fatalf("failed to create test creds file: %v", err)
	}
	h := &Handler{NatsURL: ns.ClientURL(), NatsCredentials: credsPath, logger: zap.NewNop()}
	if err := h.connectNATS(); err == nil {
		_ = h.Cleanup()
		t.Fatal("connectNATS accepted an invalid credentials file")
	}
}

func waitForLogMessage(t *testing.T, logs *observer.ObservedLogs, snippet string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, entry := range logs.All() {
			if strings.Contains(entry.Message, snippet) {
				return true
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// TestHandler_connectNATS_AsyncErrorsAreCountedAndLogged: an asynchronous
// error on NUTS' connection, here a subscription that overflows its pending
// limit, reaches nuts_nats_async_errors_total and the log with the
// subscription's subject, instead of nats.go's default printer.
func TestHandler_connectNATS_AsyncErrorsAreCountedAndLogged(t *testing.T) {
	ns := startJetStreamServer(t)
	core, obs := observer.New(zap.WarnLevel)
	h := &Handler{NatsURL: ns.ClientURL(), ReconnectWait: 1, MaxReconnects: intPtr(-1), logger: zap.New(core)}
	if err := h.connectNATS(); err != nil {
		t.Fatalf("connectNATS: %v", err)
	}
	t.Cleanup(func() { _ = h.Cleanup() })
	before := metricValue(t, metricsNATSAsyncErrors.WithLabelValues("slow_consumer"))

	release := make(chan struct{})
	defer close(release)
	sub, err := h.conn.Subscribe("flood", func(*nats.Msg) { <-release })
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := sub.SetPendingLimits(1, -1); err != nil {
		t.Fatalf("SetPendingLimits: %v", err)
	}
	pub, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("connect publisher: %v", err)
	}
	defer pub.Close()
	for i := 0; i < 10; i++ {
		if err := pub.Publish("flood", []byte("x")); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}
	if err := pub.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if !waitForLogMessage(t, obs, "NATS async error", 5*time.Second) {
		t.Fatal("the slow consumer was never reported")
	}
	if got := metricValue(t, metricsNATSAsyncErrors.WithLabelValues("slow_consumer")); got <= before {
		t.Errorf("nats_async_errors_total{kind=slow_consumer} = %v, want more than %v", got, before)
	}
	if !hasLogField(obs, "kind", "slow_consumer") || !hasLogField(obs, "subject", "flood") {
		t.Errorf("async error logged without its kind or subject: %+v", obs.All())
	}
}

// TestHandler_connectNATS_PingSettings: the connection pings the server every
// nats_ping_interval seconds (20 by default) and counts it stale after two
// unanswered pings. Two is nats.go's default; the documented detection time
// of two to three intervals relies on it.
func TestHandler_connectNATS_PingSettings(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  int
		want time.Duration
	}{
		{"default", 0, 20 * time.Second},
		{"configured", 7, 7 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := provisionOnStream(t, jetstream.StreamConfig{Name: "EVENTS", Subjects: []string{"events.>"}}, func(h *Handler) {
				h.NatsPingInterval = tc.set
			})
			h.mu.RLock()
			opts := h.conn.Opts
			h.mu.RUnlock()
			if opts.PingInterval != tc.want || opts.MaxPingsOut != 2 {
				t.Fatalf("PingInterval=%v MaxPingsOut=%d, want %v and 2", opts.PingInterval, opts.MaxPingsOut, tc.want)
			}
		})
	}
}

// TestHandler_connectNATS_NoticesAServerThatStopsAnswering covers #134: a
// server that stops answering without closing the connection (the proxy
// swallows all traffic, like a paused VM) is noticed after a few unanswered
// pings, and from then on a request is told to retry at once instead of
// waiting out its JetStream timeouts. With nats.go's two-minute default it
// went unnoticed for minutes.
func TestHandler_connectNATS_NoticesAServerThatStopsAnswering(t *testing.T) {
	t.Parallel() // asserts no process-wide metric
	ns := startJetStreamServer(t)
	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()
	createTestStream(t, nc, "EVENTS", []string{"events.>"})
	proxy := newBlackholeProxy(t, ns.Addr().String())
	core, obs := observer.New(zap.WarnLevel)
	h, srv := newContractServer(t, proxy.url(), func(h *Handler) {
		h.NatsPingInterval = 1
		h.logger = zap.New(core)
	})

	proxy.discard.Store(true)
	stalled := time.Now()
	for {
		h.mu.RLock()
		connected := h.conn.IsConnected()
		h.mu.RUnlock()
		if !connected {
			break
		}
		// Two unanswered pings, one second apart, then the third tick.
		if time.Since(stalled) > 5*time.Second {
			t.Fatal("a server that stopped answering was not noticed within 5 s")
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("noticed after %v", time.Since(stalled).Round(100*time.Millisecond))
	if !waitForLogMessage(t, obs, "disconnected from NATS", 2*time.Second) {
		t.Fatalf("no disconnect logged: %+v", obs.All())
	}
	stale := false
	for _, entry := range obs.FilterMessage("disconnected from NATS").All() {
		if err, ok := entry.ContextMap()["error"].(string); ok && strings.Contains(err, "stale connection") {
			stale = true
		}
	}
	if !stale {
		t.Fatalf("the disconnect was not put down to a stale connection: %+v", obs.All())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/events?topic=a", nil)
	req.Header.Set("Accept", "text/event-stream")
	asked := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if took := time.Since(asked); resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "retry:") || took > time.Second {
		t.Fatalf("answered %d after %v with %q, want the retry stream at once", resp.StatusCode, took, body)
	}
}

func TestHandler_connectNATS_ReconnectLifecycle(t *testing.T) {
	t.Parallel() // asserts no process-wide metric
	ns, restart := startRestartableJetStreamServer(t)

	observedCore, observedLogs := observer.New(zap.DebugLevel)
	h := &Handler{
		NatsURL:        ns.ClientURL(),
		ReconnectWait:  1,
		MaxReconnects:  intPtr(10),
		AllowedOrigins: []string{"*"},
		logger:         zap.New(observedCore),
	}

	if err := h.connectNATS(); err != nil {
		t.Fatalf("connectNATS returned error: %v", err)
	}
	defer h.Cleanup()

	ns.Shutdown()
	if !waitForLogMessage(t, observedLogs, "disconnected from NATS", 5*time.Second) {
		t.Fatal("expected disconnect log after server shutdown")
	}

	restart()
	if !waitForLogMessage(t, observedLogs, "reconnected to NATS", 5*time.Second) {
		t.Fatal("expected reconnect log after server restart")
	}
}

// TestHandler_Cleanup: whatever state Provision left the handler in, Cleanup
// closes the NATS connection, drops every handle a reload could trip over,
// marks the handler closing, and can run again without error or panic.
func TestHandler_Cleanup(t *testing.T) {
	ns := startJetStreamServer(t)
	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()
	createTestStream(t, nc, "EVENTS", []string{"events.>"})

	cases := []struct {
		name  string
		setup func(t *testing.T, h *Handler)
	}{
		{"after Provision", func(t *testing.T, h *Handler) {
			if err := h.Provision(caddy.Context{Context: context.Background()}); err != nil {
				t.Fatalf("Provision: %v", err)
			}
		}},
		{"after the connection closed", func(t *testing.T, h *Handler) {
			if err := h.connectNATS(); err != nil {
				t.Fatalf("connectNATS: %v", err)
			}
			h.conn.Close()
		}},
		{"never provisioned", func(*testing.T, *Handler) {}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handler{NatsURL: ns.ClientURL(), StreamName: "EVENTS", logger: zap.NewNop()}
			tc.setup(t, h)
			h.mu.RLock()
			conn := h.conn
			h.mu.RUnlock()

			for call := 1; call <= 3; call++ {
				start := time.Now()
				if err := h.Cleanup(); err != nil {
					t.Fatalf("Cleanup call %d: %v", call, err)
				}
				// With no stream running there is nothing to wait for; the
				// streams' deadline (cleanupStreamsTimeout) must not apply.
				if took := time.Since(start); took > time.Second {
					t.Fatalf("Cleanup call %d took %v with no stream running", call, took)
				}
			}
			h.mu.RLock()
			defer h.mu.RUnlock()
			if h.conn != nil || h.js != nil || h.shutdown != nil || !h.closing {
				t.Errorf("after Cleanup: conn=%v js=%v shutdown=%v closing=%v, want nil, nil, nil, true",
					h.conn, h.js, h.shutdown, h.closing)
			}
			if conn != nil && !conn.IsClosed() {
				t.Error("Cleanup left the NATS connection open")
			}
		})
	}
}

// ── NATS auth-mode integration ────────────────────────────────────────────
//
// These tests dial the embedded NATS server with auth ENFORCED. They
// validate that connectNATS wires the right nats.Option per auth mode
// (token, user/password) so a regression that swaps the option (e.g.
// passes the token as a username) would fail in CI rather than land
// silently.
//
// Credentials-file auth requires generating an NKEY-based JWT chain,
// which is outside the scope of these unit tests; the parser-level
// test elsewhere covers Caddyfile config validation for that mode.

func TestHandler_ConnectNATS_TokenAuth_Integration(t *testing.T) {
	const token = "test-token-with-entropy"
	ns := startJetStreamServer(t, func(o *server.Options) { o.Authorization = token })

	t.Run("missing token rejected", func(t *testing.T) {
		h := &Handler{
			NatsURL:    ns.ClientURL(),
			StreamName: "EVENTS",
			logger:     zap.NewNop(),
		}
		if err := h.connectNATS(); err == nil {
			h.Cleanup()
			t.Fatal("expected connectNATS without token to fail against token-authed server")
		}
	})

	t.Run("wrong token rejected", func(t *testing.T) {
		h := &Handler{
			NatsURL:    ns.ClientURL(),
			StreamName: "EVENTS",
			NatsToken:  "wrong-token",
			logger:     zap.NewNop(),
		}
		if err := h.connectNATS(); err == nil {
			h.Cleanup()
			t.Fatal("expected connectNATS with wrong token to fail")
		}
	})

	t.Run("correct token authenticates and JetStream works", func(t *testing.T) {
		h := &Handler{
			NatsURL:    ns.ClientURL(),
			StreamName: "EVENTS",
			NatsToken:  token,
			logger:     zap.NewNop(),
		}
		if err := h.connectNATS(); err != nil {
			t.Fatalf("connectNATS: %v", err)
		}
		defer h.Cleanup()
		js, err := jetstream.New(h.conn)
		if err != nil {
			t.Fatalf("JetStream: %v", err)
		}
		if _, err := js.CreateStream(context.Background(), jetstream.StreamConfig{
			Name:     "EVENTS",
			Subjects: []string{"events.>"},
			Storage:  jetstream.MemoryStorage,
		}); err != nil {
			t.Fatalf("AddStream: %v", err)
		}
	})
}

func TestHandler_ConnectNATS_UserPassAuth_Integration(t *testing.T) {
	const user, password = "nuts", "test-password"
	ns := startJetStreamServer(t, func(o *server.Options) {
		o.Users = []*server.User{{Username: user, Password: password}}
	})

	t.Run("missing credentials rejected", func(t *testing.T) {
		h := &Handler{
			NatsURL:    ns.ClientURL(),
			StreamName: "EVENTS",
			logger:     zap.NewNop(),
		}
		if err := h.connectNATS(); err == nil {
			h.Cleanup()
			t.Fatal("expected connectNATS without credentials to fail against user/pass-authed server")
		}
	})

	t.Run("wrong password rejected", func(t *testing.T) {
		h := &Handler{
			NatsURL:      ns.ClientURL(),
			StreamName:   "EVENTS",
			NatsUser:     user,
			NatsPassword: "wrong",
			logger:       zap.NewNop(),
		}
		if err := h.connectNATS(); err == nil {
			h.Cleanup()
			t.Fatal("expected connectNATS with wrong password to fail")
		}
	})

	t.Run("correct credentials authenticate and JetStream works", func(t *testing.T) {
		h := &Handler{
			NatsURL:      ns.ClientURL(),
			StreamName:   "EVENTS",
			NatsUser:     user,
			NatsPassword: password,
			logger:       zap.NewNop(),
		}
		if err := h.connectNATS(); err != nil {
			t.Fatalf("connectNATS: %v", err)
		}
		defer h.Cleanup()
		js, err := jetstream.New(h.conn)
		if err != nil {
			t.Fatalf("JetStream: %v", err)
		}
		if _, err := js.CreateStream(context.Background(), jetstream.StreamConfig{
			Name:     "EVENTS",
			Subjects: []string{"events.>"},
			Storage:  jetstream.MemoryStorage,
		}); err != nil {
			t.Fatalf("AddStream: %v", err)
		}
	})
}

// ── Warnings for cleartext auth + insecure TLS ────────────────────────────

// TestHandler_WarnAboutTransportSecurity covers which settings count as
// sending credentials unencrypted (#77): every plaintext scheme, servers
// without a scheme, credentials embedded in the URL, and the nats_tls_*
// directives that make nats.go use TLS on nats:// and ws:// too.
func TestHandler_WarnAboutTransportSecurity(t *testing.T) {
	cases := []struct {
		name         string
		h            *Handler
		wantCleartxt bool
		wantInsecure bool
	}{
		{name: "token over nats://", h: &Handler{NatsURL: "nats://nats.example.com:4222", NatsToken: "secret-token"}, wantCleartxt: true},
		{name: "creds file over nats://", h: &Handler{NatsURL: "nats://nats.example.com:4222", NatsCredentials: "/etc/nats/user.creds"}, wantCleartxt: true},
		{name: "user and password over ws://", h: &Handler{NatsURL: "ws://nats.example.com:8080", NatsUser: "u", NatsPassword: "p"}, wantCleartxt: true},
		{name: "token to a server without a scheme", h: &Handler{NatsURL: "nats.example.com:4222", NatsToken: "t"}, wantCleartxt: true},
		{name: "credentials embedded in the URL", h: &Handler{NatsURL: "nats://user:secret@nats.example.com:4222"}, wantCleartxt: true},
		{name: "one plaintext server in a list", h: &Handler{NatsURL: "tls://a:4222,nats://b:4222", NatsToken: "t"}, wantCleartxt: true},
		{name: "token over tls://", h: &Handler{NatsURL: "tls://nats.example.com:4222", NatsToken: "t"}},
		{name: "token over wss://", h: &Handler{NatsURL: "wss://nats.example.com:443", NatsToken: "t"}},
		{name: "token over nats:// with a CA bundle", h: &Handler{NatsURL: "nats://nats.example.com:4222", NatsToken: "t", NatsTLSCA: "/etc/ca.pem"}},
		{name: "token over ws:// with a client certificate", h: &Handler{NatsURL: "ws://nats.example.com:8080", NatsToken: "t", NatsTLSCert: "/c.pem", NatsTLSKey: "/k.pem"}},
		{name: "no credentials over nats://", h: &Handler{NatsURL: "nats://nats.example.com:4222"}},
		{name: "insecure skip verify", h: &Handler{NatsURL: "tls://nats.example.com:4222", NatsTLSInsecureSkipVerify: true}, wantInsecure: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			core, obs := observer.New(zap.WarnLevel)
			h := c.h
			h.logger = zap.New(core)
			h.warnAboutTransportSecurity()
			if got := hasLogContaining(obs, "sent unencrypted"); got != c.wantCleartxt {
				t.Fatalf("cleartext warning = %v, want %v: %v", got, c.wantCleartxt, obs.All())
			}
			if got := hasLogContaining(obs, "insecure_skip_verify"); got != c.wantInsecure {
				t.Fatalf("insecure warning = %v, want %v: %v", got, c.wantInsecure, obs.All())
			}
			if strings.Contains(fmt.Sprint(obs.All()), "secret") {
				t.Fatalf("a warning leaked a credential: %v", obs.All())
			}
		})
	}
}

// TestHandler_Provision_WarnsBeforeDialling covers #82: the transport
// warnings used to come from Validate, which Caddy runs after Provision has
// already sent the credentials and the first JetStream requests.
func TestHandler_Provision_WarnsBeforeDialling(t *testing.T) {
	core, obs := observer.New(zap.WarnLevel)
	h := &Handler{
		NatsURL:                   "tls://127.0.0.1:1",
		StreamName:                "EVENTS",
		NatsToken:                 "t",
		NatsTLSInsecureSkipVerify: true,
		logger:                    zap.New(core),
	}
	if err := h.Provision(caddy.Context{Context: context.Background()}); err == nil {
		t.Fatal("Provision against a closed port succeeded")
	}
	if !hasLogContaining(obs, "insecure_skip_verify") {
		t.Fatalf("no insecure_skip_verify warning although the dial failed: %v", obs.All())
	}
	if err := h.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if n := obs.FilterMessageSnippet("insecure_skip_verify").Len(); n != 1 {
		t.Fatalf("insecure_skip_verify warned %d times, want once (Validate must not repeat it)", n)
	}
}

// ── TLS field validation ──────────────────────────────────────────────────

func TestHandler_Validate_TLSCertKeyPairing(t *testing.T) {
	h := &Handler{
		NatsURL:     "tls://nats.example.com:4222",
		StreamName:  "EVENTS",
		NatsTLSCert: "/tmp/cert.pem",
		// NatsTLSKey intentionally empty
		logger: zap.NewNop(),
	}
	if err := h.Validate(); err == nil {
		t.Error("expected error when nats_tls_cert is set without nats_tls_key")
	}
}

// TestHandler_MaxReconnectsZero_HonoredFromJSON guards against regressions of
// the JSON/Caddyfile asymmetry: an explicit 0 in JSON must survive Provision's
// defaulting instead of being silently rewritten to -1.
func TestHandler_MaxReconnectsZero_HonoredFromJSON(t *testing.T) {
	raw := []byte(`{
        "nats_url": "nats://localhost:4222",
        "stream_name": "EVENTS",
        "max_reconnects": 0
    }`)
	var h Handler
	if err := json.Unmarshal(raw, &h); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if h.MaxReconnects == nil {
		t.Fatalf("MaxReconnects should be set after explicit JSON field")
	}
	if *h.MaxReconnects != 0 {
		t.Errorf("MaxReconnects should be 0 after explicit JSON field, got %d", *h.MaxReconnects)
	}

	// The explicit 0 must survive Provision and reach the connection.
	ns := startJetStreamServer(t)
	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()
	createTestStream(t, nc, "EVENTS", []string{"events.>"})
	h.NatsURL = ns.ClientURL()
	h.logger = zap.NewNop()
	if err := h.Provision(caddy.Context{Context: context.Background()}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	defer h.Cleanup()
	if got := h.conn.Opts.MaxReconnect; got != 0 {
		t.Fatalf("connection MaxReconnects = %d after Provision, want 0", got)
	}
}

// TestHandler_MaxReconnects_DefaultFromJSON proves that omitting the JSON
// field yields the "unlimited" default after Provision, matching Caddyfile
// behaviour.
func TestHandler_MaxReconnects_DefaultFromJSON(t *testing.T) {
	raw := []byte(`{
        "nats_url": "nats://localhost:4222",
        "stream_name": "EVENTS"
    }`)
	var h Handler
	if err := json.Unmarshal(raw, &h); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if h.MaxReconnects != nil {
		t.Errorf("MaxReconnects should be nil when JSON field omitted, got %d", *h.MaxReconnects)
	}
}

func TestHandler_Validate_RejectsInvalidOptionalConfig(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Handler)
		wantErr string
	}{
		{
			name:    "max reconnects below unlimited sentinel",
			mutate:  func(h *Handler) { h.MaxReconnects = intPtr(-2) },
			wantErr: "max_reconnects",
		},
		{
			name:    "negative max connections",
			mutate:  func(h *Handler) { h.MaxConnections = -1 },
			wantErr: "max_connections",
		},
		{
			name:    "negative client buffer size",
			mutate:  func(h *Handler) { h.ClientBufferSize = -1 },
			wantErr: "client_buffer_size",
		},
		{
			name:    "negative dispatch timeout",
			mutate:  func(h *Handler) { h.DispatchTimeout = -1 },
			wantErr: "dispatch_timeout",
		},
		{
			name:    "write timeout below the -1 disable sentinel",
			mutate:  func(h *Handler) { h.WriteTimeout = -2 },
			wantErr: "write_timeout",
		},
		{
			name:    "negative replay max messages",
			mutate:  func(h *Handler) { h.ReplayMaxMessages = -1 },
			wantErr: "replay_max_messages",
		},
		{
			name:    "negative replay window",
			mutate:  func(h *Handler) { h.ReplayWindow = -1 },
			wantErr: "replay_window",
		},
		{
			name:    "unsupported allowed method",
			mutate:  func(h *Handler) { h.AllowedMethods = []string{"GET", "POST", "OPTIONS"} },
			wantErr: "allowed_methods",
		},
		{
			name:    "subscriber cookie requires key",
			mutate:  func(h *Handler) { h.SubscriberJWTCookie = "nuts_session" },
			wantErr: "subscriber_jwt_key",
		},
		{
			name: "subscriber cookie validates name",
			mutate: func(h *Handler) {
				h.SubscriberJWTKey = "secret"
				h.SubscriberJWTCookie = "bad;name"
			},
			wantErr: "subscriber_jwt_cookie",
		},
		{
			name:    "topic_prefix with NATS asterisk wildcard",
			mutate:  func(h *Handler) { h.TopicPrefix = "*." },
			wantErr: "topic_prefix",
		},
		{
			name:    "topic_prefix with NATS greater-than wildcard",
			mutate:  func(h *Handler) { h.TopicPrefix = "events.>" },
			wantErr: "topic_prefix",
		},
		{
			name:    "topic_prefix with leading dot",
			mutate:  func(h *Handler) { h.TopicPrefix = ".events." },
			wantErr: "topic_prefix",
		},
		{
			name:    "topic_prefix with system-subject prefix",
			mutate:  func(h *Handler) { h.TopicPrefix = "$sys." },
			wantErr: "topic_prefix",
		},
		{
			name:    "topic_prefix with consecutive dots",
			mutate:  func(h *Handler) { h.TopicPrefix = "events..a." },
			wantErr: "topic_prefix",
		},
		{
			name:    "topic_prefix with disallowed byte",
			mutate:  func(h *Handler) { h.TopicPrefix = "events/" },
			wantErr: "topic_prefix",
		},
		{
			name:    "topic_prefix exceeds maxSubjectLen",
			mutate:  func(h *Handler) { h.TopicPrefix = strings.Repeat("a", maxSubjectLen+1) },
			wantErr: "topic_prefix",
		},
		{
			name:    "negative heartbeat interval",
			mutate:  func(h *Handler) { h.HeartbeatInterval = -30 },
			wantErr: "heartbeat_interval",
		},
		{
			name:    "negative reconnect wait",
			mutate:  func(h *Handler) { h.ReconnectWait = -2 },
			wantErr: "reconnect_wait",
		},
		{
			name:    "negative ping interval",
			mutate:  func(h *Handler) { h.NatsPingInterval = -1 },
			wantErr: "nats_ping_interval",
		},
		{
			name:    "unknown event id format",
			mutate:  func(h *Handler) { h.EventIDFormat = "sequence-time" },
			wantErr: "event_id_format",
		},
		{
			name:    "nats_idle_heartbeat at boundary equals InactiveThreshold/2",
			mutate:  func(h *Handler) { h.NatsIdleHeartbeat = 15 },
			wantErr: "nats_idle_heartbeat",
		},
		{
			name:    "nats_idle_heartbeat above InactiveThreshold/2",
			mutate:  func(h *Handler) { h.NatsIdleHeartbeat = 30 },
			wantErr: "nats_idle_heartbeat",
		},
		{
			name:    "nats_idle_heartbeat negative typo just below sentinel",
			mutate:  func(h *Handler) { h.NatsIdleHeartbeat = -2 },
			wantErr: "nats_idle_heartbeat",
		},
		{
			name:    "nats_idle_heartbeat large-magnitude negative typo",
			mutate:  func(h *Handler) { h.NatsIdleHeartbeat = -100 },
			wantErr: "nats_idle_heartbeat",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := Handler{
				NatsURL:    "nats://localhost:4222",
				StreamName: "EVENTS",
			}
			tt.mutate(&h)
			err := h.Validate()
			if err == nil {
				t.Fatalf("expected validation error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() error = %q, want to contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

// TestValidateTopicPrefix_AcceptsLegitimate documents the positive side
// of the prefix contract: empty is fine, the idiomatic trailing-dot
// forms shown in the README/CONFIGURATION docs must validate cleanly,
// and the cap accepts exactly-at-boundary strings (length=maxSubjectLen)
// so the boundary on the > check at provision.go is pinned in both
// directions (matching the >maxSubjectLen rejection test above).
func TestValidateTopicPrefix_AcceptsLegitimate(t *testing.T) {
	cases := []string{
		"", "events.", "tenants.a.", "events", "a_b-c.", "ABC.XYZ.",
		strings.Repeat("a", maxSubjectLen),
	}
	for _, p := range cases {
		if err := validateTopicPrefix(p); err != nil {
			t.Errorf("validateTopicPrefix(%q) rejected legitimate prefix: %v", p, err)
		}
	}
}

func TestHandler_Provision_RejectsInvalidOptionalJSONConfigBeforeDialing(t *testing.T) {
	tests := []struct {
		name     string
		fragment string
		wantErr  string
	}{
		{
			name:     "max reconnects below unlimited sentinel",
			fragment: `"max_reconnects": -2`,
			wantErr:  "max_reconnects",
		},
		{
			name:     "negative max connections",
			fragment: `"max_connections": -1`,
			wantErr:  "max_connections",
		},
		{
			name:     "negative client buffer size",
			fragment: `"client_buffer_size": -1`,
			wantErr:  "client_buffer_size",
		},
		{
			name:     "negative dispatch timeout",
			fragment: `"dispatch_timeout": -1`,
			wantErr:  "dispatch_timeout",
		},
		{
			name:     "write timeout below the -1 disable sentinel",
			fragment: `"write_timeout": -2`,
			wantErr:  "write_timeout",
		},
		{
			name:     "negative replay max messages",
			fragment: `"replay_max_messages": -1`,
			wantErr:  "replay_max_messages",
		},
		{
			name:     "negative replay window",
			fragment: `"replay_window": -1`,
			wantErr:  "replay_window",
		},
		{
			name:     "unsupported allowed method",
			fragment: `"allowed_methods": ["GET", "POST", "OPTIONS"]`,
			wantErr:  "allowed_methods",
		},
		{
			name:     "subscriber cookie requires key",
			fragment: `"subscriber_jwt_cookie": "nuts_session"`,
			wantErr:  "subscriber_jwt_key",
		},
		{
			name:     "subscriber cookie validates name",
			fragment: `"subscriber_jwt_key": "secret", "subscriber_jwt_cookie": "bad;name"`,
			wantErr:  "subscriber_jwt_cookie",
		},
		{
			name:     "negative heartbeat interval",
			fragment: `"heartbeat_interval": -30`,
			wantErr:  "heartbeat_interval",
		},
		{
			name:     "negative reconnect wait",
			fragment: `"reconnect_wait": -2`,
			wantErr:  "reconnect_wait",
		},
		{
			name:     "negative ping interval",
			fragment: `"nats_ping_interval": -1`,
			wantErr:  "nats_ping_interval",
		},
		{
			name:     "unknown event id format",
			fragment: `"event_id_format": "timestamp"`,
			wantErr:  "event_id_format",
		},
		{
			name:     "nats_idle_heartbeat at boundary equals InactiveThreshold/2",
			fragment: `"nats_idle_heartbeat": 15`,
			wantErr:  "nats_idle_heartbeat",
		},
		{
			name:     "nats_idle_heartbeat above InactiveThreshold/2",
			fragment: `"nats_idle_heartbeat": 30`,
			wantErr:  "nats_idle_heartbeat",
		},
		{
			name:     "nats_idle_heartbeat negative typo just below sentinel",
			fragment: `"nats_idle_heartbeat": -2`,
			wantErr:  "nats_idle_heartbeat",
		},
		{
			name:     "nats_idle_heartbeat large-magnitude negative typo",
			fragment: `"nats_idle_heartbeat": -100`,
			wantErr:  "nats_idle_heartbeat",
		},
		{
			name:     "topic cap below the -1 sentinel",
			fragment: `"max_topics_per_subscription": -2`,
			wantErr:  "max_topics_per_subscription",
		},
		{
			name:     "CA bundle with verification disabled",
			fragment: `"nats_tls_ca": "/etc/nats/ca.pem", "nats_tls_insecure_skip_verify": true`,
			wantErr:  "nats_tls_ca",
		},
		{
			name:     "mistyped nats_url scheme",
			fragment: `"nats_url": "tsl://127.0.0.1:1"`,
			wantErr:  "nats_url",
		},
		{
			name:     "allowed origin with a path",
			fragment: `"allowed_origins": ["https://app.example.com/"]`,
			wantErr:  "allowed_origins",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := []byte(`{"nats_url":"nats://127.0.0.1:1","stream_name":"EVENTS",` + tt.fragment + `}`)
			var h Handler
			if err := json.Unmarshal(raw, &h); err != nil {
				t.Fatalf("json.Unmarshal: %v", err)
			}

			if err := h.Validate(); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() error = %v, want to contain %q", err, tt.wantErr)
			}

			err := h.Provision(caddy.Context{Context: context.Background()})
			if err == nil {
				t.Fatalf("expected Provision error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Provision() error = %q, want to contain %q", err.Error(), tt.wantErr)
			}
			if strings.Contains(err.Error(), "failed to connect to NATS") {
				t.Fatalf("Provision dialed NATS before rejecting config: %v", err)
			}

			h.mu.RLock()
			connNil := h.conn == nil
			jsNil := h.js == nil
			shutdownNil := h.shutdown == nil
			h.mu.RUnlock()
			if !connNil || !jsNil || !shutdownNil {
				t.Fatalf("expected no runtime state after validation rejection, got connNil=%v jsNil=%v shutdownNil=%v", connNil, jsNil, shutdownNil)
			}
		})
	}
}

func TestHandler_Provision_PreservesSentinelConfigSemantics(t *testing.T) {
	ns := startJetStreamServer(t)

	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()
	createTestStream(t, nc, "EVENTS", []string{"events.>"})

	h := &Handler{
		NatsURL:          ns.ClientURL(),
		StreamName:       "EVENTS",
		MaxEventSize:     -1,
		ClientBufferSize: 0,
	}
	if err := h.Provision(caddy.Context{Context: context.Background()}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	defer h.Cleanup()

	if h.MaxEventSize != -1 {
		t.Fatalf("MaxEventSize = %d, want -1 unlimited sentinel", h.MaxEventSize)
	}
	if h.ClientBufferSize != defaultClientBufferSize {
		t.Fatalf("ClientBufferSize = %d, want default %d", h.ClientBufferSize, defaultClientBufferSize)
	}
}

// parseNatsIdleHeartbeatCaddyfile runs the Caddyfile parser on a minimal
// nuts block containing only the nats_idle_heartbeat directive and
// returns the resulting Handler. Helper used by the
// TestHandler_NatsIdleHeartbeat_ConfigContract sub-tests.
func parseNatsIdleHeartbeatCaddyfile(t *testing.T, value string) *Handler {
	t.Helper()
	input := "nuts {\n    nats_url nats://localhost:4222\n    stream_name EVENTS\n    nats_idle_heartbeat " + value + "\n}"
	d := caddyfile.NewTestDispenser(input)
	h := &Handler{}
	if err := h.UnmarshalCaddyfile(d); err != nil {
		t.Fatalf("UnmarshalCaddyfile(%q): %v", value, err)
	}
	return h
}

// provisionNatsIdleHeartbeat dials a real embedded JetStream server and
// runs Provision so the normalisation path is exercised end-to-end —
// the alternative (calling validateConfigValues + the normalisation
// block directly) would skip the public Provision contract that
// integration with Caddy depends on. The caller is responsible for
// Cleanup().
func provisionNatsIdleHeartbeat(t *testing.T, initial int) *Handler {
	t.Helper()
	ns := startJetStreamServer(t)
	t.Cleanup(ns.Shutdown)

	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)
	createTestStream(t, nc, "EVENTS", []string{"events.>"})

	h := &Handler{
		NatsURL:           ns.ClientURL(),
		StreamName:        "EVENTS",
		NatsIdleHeartbeat: initial,
	}
	if err := h.Provision(caddy.Context{Context: context.Background()}); err != nil {
		t.Fatalf("Provision(initial=%d): %v", initial, err)
	}
	return h
}

// TestHandler_NatsIdleHeartbeat_ConfigContract is the M9 Phase 2 contract
// test for the nats_idle_heartbeat knob. It pins every interesting input
// to its expected outcome across the four config surfaces a NUTS
// deployment can reach: Caddyfile parser, JSON unmarshal, Provision
// normalisation, Validate. The shape is intentionally exhaustive — the
// knob is the operator-facing handle for the M9 Batch A→B failure-
// detection contract and a silent regression on any boundary would
// silently undermine consumer-invalidation routing.
func TestHandler_NatsIdleHeartbeat_ConfigContract(t *testing.T) {
	t.Run("caddyfile parser accepts positive default", func(t *testing.T) {
		h := parseNatsIdleHeartbeatCaddyfile(t, "10")
		if h.NatsIdleHeartbeat != 10 {
			t.Fatalf("Caddyfile NatsIdleHeartbeat = %d, want 10", h.NatsIdleHeartbeat)
		}
	})
	t.Run("caddyfile parser accepts low positive", func(t *testing.T) {
		h := parseNatsIdleHeartbeatCaddyfile(t, "5")
		if h.NatsIdleHeartbeat != 5 {
			t.Fatalf("Caddyfile NatsIdleHeartbeat = %d, want 5", h.NatsIdleHeartbeat)
		}
	})
	t.Run("caddyfile parser accepts operator-disable sentinel", func(t *testing.T) {
		h := parseNatsIdleHeartbeatCaddyfile(t, "-1")
		if h.NatsIdleHeartbeat != -1 {
			t.Fatalf("Caddyfile NatsIdleHeartbeat = %d, want -1 (disable sentinel)", h.NatsIdleHeartbeat)
		}
	})
	t.Run("caddyfile parser preserves zero for Provision-time normalisation", func(t *testing.T) {
		// Operators who write `nats_idle_heartbeat 0` expect "use the
		// default" semantics, same as every other int knob in the file
		// (heartbeat_interval, reconnect_wait, etc.). The parser keeps
		// the zero; Provision rewrites it to the default. Validate must
		// also accept 0 because it runs before Provision normalisation
		// (provision.go:82-83).
		h := parseNatsIdleHeartbeatCaddyfile(t, "0")
		if h.NatsIdleHeartbeat != 0 {
			t.Fatalf("Caddyfile NatsIdleHeartbeat = %d, want 0 (Provision normalises)", h.NatsIdleHeartbeat)
		}
	})
	t.Run("JSON round-trip absent field decodes to zero", func(t *testing.T) {
		var h Handler
		if err := json.Unmarshal([]byte(`{"nats_url":"nats://localhost:4222","stream_name":"EVENTS"}`), &h); err != nil {
			t.Fatalf("json.Unmarshal: %v", err)
		}
		if h.NatsIdleHeartbeat != 0 {
			t.Fatalf("absent JSON field decoded to %d, want 0 (Provision normalises)", h.NatsIdleHeartbeat)
		}
	})
	t.Run("JSON round-trip explicit value preserved through Validate", func(t *testing.T) {
		var h Handler
		raw := []byte(`{"nats_url":"nats://localhost:4222","stream_name":"EVENTS","nats_idle_heartbeat":7}`)
		if err := json.Unmarshal(raw, &h); err != nil {
			t.Fatalf("json.Unmarshal: %v", err)
		}
		if h.NatsIdleHeartbeat != 7 {
			t.Fatalf("JSON nats_idle_heartbeat decoded to %d, want 7", h.NatsIdleHeartbeat)
		}
		if err := h.Validate(); err != nil {
			t.Fatalf("Validate rejected legitimate value: %v", err)
		}
	})
	t.Run("Provision normalises zero to default 10", func(t *testing.T) {
		h := provisionNatsIdleHeartbeat(t, 0)
		defer h.Cleanup()
		if h.NatsIdleHeartbeat != 10 {
			t.Fatalf("post-Provision NatsIdleHeartbeat = %d, want 10 default", h.NatsIdleHeartbeat)
		}
	})
	t.Run("Provision preserves explicit positive value", func(t *testing.T) {
		h := provisionNatsIdleHeartbeat(t, 5)
		defer h.Cleanup()
		if h.NatsIdleHeartbeat != 5 {
			t.Fatalf("post-Provision NatsIdleHeartbeat = %d, want 5", h.NatsIdleHeartbeat)
		}
	})
	t.Run("Provision preserves operator-disable sentinel", func(t *testing.T) {
		h := provisionNatsIdleHeartbeat(t, -1)
		defer h.Cleanup()
		if h.NatsIdleHeartbeat != -1 {
			t.Fatalf("post-Provision NatsIdleHeartbeat = %d, want -1 (disable sentinel preserved)", h.NatsIdleHeartbeat)
		}
	})
	t.Run("Validate accepts boundary value 14", func(t *testing.T) {
		// 15 is exactly InactiveThreshold/2 and rejected by the upper
		// bound; 14 is the largest accepted positive value. Pinning the
		// boundary on the accepted side guards against an inclusive-vs-
		// exclusive comparison regression in Validate.
		h := Handler{NatsURL: "nats://localhost:4222", StreamName: "EVENTS", NatsIdleHeartbeat: 14}
		if err := h.Validate(); err != nil {
			t.Fatalf("Validate rejected boundary-1 value: %v", err)
		}
	})
	t.Run("Validate accepts 1", func(t *testing.T) {
		h := Handler{NatsURL: "nats://localhost:4222", StreamName: "EVENTS", NatsIdleHeartbeat: 1}
		if err := h.Validate(); err != nil {
			t.Fatalf("Validate rejected low positive value: %v", err)
		}
	})
	t.Run("Validate accepts zero (Provision normalises)", func(t *testing.T) {
		h := Handler{NatsURL: "nats://localhost:4222", StreamName: "EVENTS", NatsIdleHeartbeat: 0}
		if err := h.Validate(); err != nil {
			t.Fatalf("Validate rejected zero (must pass — Provision normalises): %v", err)
		}
	})
	t.Run("Validate accepts operator-disable sentinel", func(t *testing.T) {
		h := Handler{NatsURL: "nats://localhost:4222", StreamName: "EVENTS", NatsIdleHeartbeat: -1}
		if err := h.Validate(); err != nil {
			t.Fatalf("Validate rejected disable sentinel: %v", err)
		}
	})
	t.Run("Validate rejects non-sentinel negatives as typo guard", func(t *testing.T) {
		// Sibling fields (heartbeat_interval, reconnect_wait) reject
		// negatives unconditionally; nats_idle_heartbeat accepts exactly
		// -1 (the explicit operator-disable sentinel) and rejects every
		// other negative as a likely typo. Pin both ends of the -1
		// boundary to catch a future regression that loosens the check
		// (e.g. "any negative disables") and re-enables silent typo
		// configurations.
		for _, v := range []int{-2, -10, -100} {
			h := Handler{NatsURL: "nats://localhost:4222", StreamName: "EVENTS", NatsIdleHeartbeat: v}
			err := h.Validate()
			if err == nil {
				t.Fatalf("Validate accepted nats_idle_heartbeat=%d, want rejection (only -1 is the disable sentinel)", v)
			}
			if !strings.Contains(err.Error(), "nats_idle_heartbeat") {
				t.Fatalf("Validate error for nats_idle_heartbeat=%d does not mention the field: %v", v, err)
			}
		}
	})
}

// ── Provision() fails BEFORE opening NATS when required fields absent ────

func TestHandler_Provision_RejectsBeforeDialing(t *testing.T) {
	// Point at a guaranteed-closed port so any actual dial attempt would fail
	// with a connection error — but we expect the missing-field error instead.
	for _, c := range []struct {
		name    string
		h       *Handler
		wantErr string
	}{
		{name: "missing nats_url", h: &Handler{StreamName: "EVENTS"}, wantErr: "nats_url is required"},
		{name: "missing stream_name", h: &Handler{NatsURL: "nats://127.0.0.1:1"}, wantErr: "stream_name is required"},
	} {
		t.Run(c.name, func(t *testing.T) {
			c.h.logger = zap.NewNop()
			err := c.h.Provision(caddy.Context{Context: context.Background()})
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("Provision error = %v, want %q before any dial", err, c.wantErr)
			}
			if c.h.conn != nil {
				t.Fatal("Provision opened a connection despite invalid config")
			}
		})
	}
}

// TestHandler_Cleanup_WakesInFlightHandlers verifies that Cleanup() closes
// the handler-scoped shutdown channel, causing any active SSE goroutine to
// return promptly instead of waiting until the next heartbeat tick (30s by
// default) notices the connection has been torn down.
func TestHandler_Cleanup_WakesInFlightHandlers(t *testing.T) {
	ns := startJetStreamServer(t)

	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()
	createTestStream(t, nc, "EVENTS", []string{"events.>"})

	h := &Handler{
		NatsURL:           ns.ClientURL(),
		StreamName:        "EVENTS",
		TopicPrefix:       "events.",
		HeartbeatInterval: 30, // deliberately long — shutdown must win, not heartbeat
		MaxEventSize:      -1,
		AllowedOrigins:    []string{"*"},
	}
	// The real Provision must create the shutdown channel; hand-building it
	// here would hide a Provision regression.
	if err := h.Provision(caddy.Context{Context: context.Background()}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	h.logger = zap.NewNop()

	req := httptest.NewRequest(http.MethodGet, "/events?topic=shutdown", nil)
	req = req.WithContext(context.Background())

	rr := newSafeRecorder()
	done := make(chan error, 1)
	go func() { done <- h.ServeHTTP(rr, req, nil) }()

	// Wait until the SSE "connected" event has been written so we know the
	// handler is inside the streaming loop and not still in setup.
	if !waitForSSEBody(rr, "event: connected", 2*time.Second) {
		t.Fatal("handler never reached the streaming loop")
	}

	// Cleanup must wake the handler in well under heartbeat_interval.
	cleanupStart := time.Now()
	if err := h.Cleanup(); err != nil {
		t.Fatalf("Cleanup returned error: %v", err)
	}

	select {
	case err := <-done:
		elapsed := time.Since(cleanupStart)
		if err != nil {
			t.Fatalf("ServeHTTP returned error after Cleanup: %v", err)
		}
		if elapsed > 500*time.Millisecond {
			t.Errorf("ServeHTTP took %v to return after Cleanup; expected < 500ms", elapsed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ServeHTTP did not return within 3s after Cleanup — shutdown signal not observed")
	}
}

// ── L1: Short subscriber_jwt_key warning ──────────────────────────────────

func TestHandler_Validate_WarnsShortJWTKey(t *testing.T) {
	core, obs := observer.New(zap.WarnLevel)
	h := &Handler{
		NatsURL:          "tls://nats.example.com:4222",
		StreamName:       "EVENTS",
		SubscriberJWTKey: strings.Repeat("a", 16),
		logger:           zap.New(core),
	}
	if err := h.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !hasLogContaining(obs, "subscriber_jwt_key is shorter than recommended") {
		t.Errorf("expected short-key warning, entries=%+v", obs.All())
	}
}

func TestHandler_Validate_NoWarningForLongJWTKey(t *testing.T) {
	core, obs := observer.New(zap.WarnLevel)
	h := &Handler{
		NatsURL:          "tls://nats.example.com:4222",
		StreamName:       "EVENTS",
		SubscriberJWTKey: strings.Repeat("a", 64),
		logger:           zap.New(core),
	}
	if err := h.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if hasLogContaining(obs, "subscriber_jwt_key is shorter") {
		t.Errorf("did not expect short-key warning, entries=%+v", obs.All())
	}
}

// TestHandler_Validate_WarnsAboutIneffectiveSettings covers the warnings for
// settings the pull consumer made meaningless: dispatch_timeout, and
// nats_idle_heartbeat -1 (heartbeats can no longer be turned off).
func TestHandler_Validate_WarnsAboutIneffectiveSettings(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*Handler)
		wantWarn string
	}{
		{name: "dispatch_timeout set", mutate: func(h *Handler) { h.DispatchTimeout = 5 }, wantWarn: "dispatch_timeout is deprecated"},
		{name: "nats_idle_heartbeat disabled", mutate: func(h *Handler) { h.NatsIdleHeartbeat = natsIdleHeartbeatDisabledSentinel }, wantWarn: "nats_idle_heartbeat -1 no longer disables"},
		{name: "defaults stay quiet", mutate: func(*Handler) {}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			core, obs := observer.New(zap.WarnLevel)
			h := &Handler{NatsURL: "tls://127.0.0.1:4222", StreamName: "EVENTS", NatsIdleHeartbeat: 10, AllowedOrigins: []string{"https://app.example"}, logger: zap.New(core)}
			c.mutate(h)
			if err := h.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			var messages []string
			for _, entry := range obs.All() {
				messages = append(messages, entry.Message)
			}
			joined := strings.Join(messages, "\n")
			if c.wantWarn == "" {
				if len(messages) != 0 {
					t.Fatalf("unexpected warnings: %v", messages)
				}
				return
			}
			if !strings.Contains(joined, c.wantWarn) {
				t.Fatalf("warnings %v do not mention %q", messages, c.wantWarn)
			}
		})
	}
}

func TestServerVersionBefore(t *testing.T) {
	cases := []struct {
		version string
		want    bool
	}{
		{"2.10.29", true},
		{"2.12.15", true},
		{"2.14.6", true},
		{"2.14.7", false},
		{"2.14.8", false},
		{"2.15.0", false},
		{"2.15.0-beta.1", false},
		{"2.14.6-RC.2", true},
		{"3.0.0", false},
		{"1.99.99", true},
		{"", false},
		{"2.14", false},
		{"2.x.7", false},
	}
	for _, c := range cases {
		if got := serverVersionBefore(c.version, multiFilterPurgeFixed); got != c.want {
			t.Errorf("serverVersionBefore(%q) = %v, want %v", c.version, got, c.want)
		}
	}
}

func TestHandler_WarnAboutServerVersion(t *testing.T) {
	for version, wantWarn := range map[string]bool{"2.12.15": true, "2.14.7": false, "garbage": false} {
		core, obs := observer.New(zap.WarnLevel)
		h := &Handler{logger: zap.New(core)}
		h.warnAboutServerVersion(version)
		if got := hasLogField(obs, "server_version", version); got != wantWarn {
			t.Errorf("server %q: warned=%v, want %v (%v)", version, got, wantWarn, obs.All())
		}
	}
}

func TestNatsServerSchemes(t *testing.T) {
	cases := []struct {
		raw     string
		want    []string
		wantErr string
	}{
		{raw: "nats://a:4222", want: []string{"nats"}},
		{raw: "tls://a:4222", want: []string{"tls"}},
		{raw: "ws://a:8080", want: []string{"ws"}},
		{raw: "wss://a:443", want: []string{"wss"}},
		{raw: "NATS://a:4222", want: []string{"nats"}},
		{raw: "a:4222", want: []string{"nats"}},
		{raw: "nats://user:pass@a:4222", want: []string{"nats"}},
		{raw: "tls://a:4222, nats://b:4222,c:4222", want: []string{"tls", "nats", "nats"}},
		{raw: "http://a:4222", wantErr: `scheme "http" is not supported`},
		{raw: "tsl://a:4222", wantErr: `scheme "tsl" is not supported`},
		{raw: "nats://", wantErr: "has no host"},
		{raw: "nats://a:4222,,nats://b:4222", wantErr: "empty server entry"},
		{raw: "nats://user:pa ss@a:4222", wantErr: "is not a valid URL"},
	}
	for _, c := range cases {
		got, err := natsServerSchemes(c.raw)
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("natsServerSchemes(%q) error = %v, want %q", c.raw, err, c.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), "pa ss") {
				t.Errorf("natsServerSchemes(%q) error leaks the password: %v", c.raw, err)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("natsServerSchemes(%q) = %v, %v; want %v", c.raw, got, err, c.want)
		}
	}
}

func TestValidateAllowedOrigins(t *testing.T) {
	valid := [][]string{
		nil,
		{"*"},
		{"https://app.example.com"},
		{"http://localhost:3000", "https://app.example.com", "*"},
		{"https://[::1]:8443"},
	}
	for _, origins := range valid {
		if err := validateAllowedOrigins(origins); err != nil {
			t.Errorf("validateAllowedOrigins(%q) = %v, want nil", origins, err)
		}
	}
	invalid := map[string]string{
		"": "empty entry",
		"https://a.example.com,https://b.example.com": "comma or whitespace",
		"https://a.example.com ":                      "comma or whitespace",
		"https://a.example.com\r\n":                   "comma or whitespace",
		"app.example.com":                             "not an origin",
		"https://app.example.com/":                    "not an origin",
		"https://app.example.com/path":                "not an origin",
		"https://app.example.com?x=1":                 "not an origin",
		"https://app.example.com?":                    "not an origin",
		"https://app.example.com#top":                 "not an origin",
		"https://user@app.example.com":                "not an origin",
		"null":                                        "not an origin",
		"mailto:someone@example.com":                  "not an origin",
		"https://App.Example.com":                     "must be lowercase",
	}
	for origin, wantErr := range invalid {
		err := validateAllowedOrigins([]string{"https://ok.example.com", origin})
		if err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Errorf("validateAllowedOrigins(%q) = %v, want an error containing %q", origin, err, wantErr)
		}
	}
}

// TestHandler_Provision_LogsDisabledTopicCap covers #81: switching the topic
// cap off is visible in the logs.
func TestHandler_Provision_LogsDisabledTopicCap(t *testing.T) {
	for _, c := range []struct {
		cap     int
		wantLog bool
	}{{-1, true}, {0, false}, {5, false}} {
		core, obs := observer.New(zap.InfoLevel)
		h := &Handler{NatsURL: "nats://127.0.0.1:1", StreamName: "EVENTS", MaxTopicsPerSubscription: c.cap, logger: zap.New(core)}
		_ = h.Provision(caddy.Context{Context: context.Background()}) // the dial fails; the log comes first
		if got := hasLogContaining(obs, "max_topics_per_subscription is -1"); got != c.wantLog {
			t.Errorf("cap %d: logged=%v, want %v", c.cap, got, c.wantLog)
		}
	}
}

// ── TLS: buildTLSConfig ───────────────────────────────────────────────────

// generateSelfSignedPEM returns a PEM-encoded cert + PKCS#8 key pair suitable
// for dropping into buildTLSConfig without starting a TLS server. The cert is
// a self-signed CA so the same blob can be used as both ca and client cert.
func generateSelfSignedPEM(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "nuts-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("x509.CreateCertificate: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyBytes, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey: %v", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes})
	return certPEM, keyPEM
}

func writeTempFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

func TestBuildTLSConfig_CAOnlyPopulatesRootCAs(t *testing.T) {
	certPEM, _ := generateSelfSignedPEM(t)
	dir := t.TempDir()
	caPath := writeTempFile(t, dir, "ca.pem", certPEM)

	h := &Handler{NatsTLSCA: caPath}
	cfg, err := h.buildTLSConfig()
	if err != nil {
		t.Fatalf("buildTLSConfig: %v", err)
	}
	if cfg.RootCAs == nil {
		t.Fatal("expected RootCAs to be populated when nats_tls_ca is set")
	}
	if cfg.MinVersion != 0x0303 { // TLS 1.2 sentinel
		t.Errorf("expected TLS 1.2 minimum, got 0x%x", cfg.MinVersion)
	}
	if len(cfg.Certificates) != 0 {
		t.Errorf("expected no client certs when only CA is configured, got %d", len(cfg.Certificates))
	}
	if cfg.InsecureSkipVerify {
		t.Error("expected InsecureSkipVerify to be false")
	}
}

func TestBuildTLSConfig_MTLSLoadsCertPair(t *testing.T) {
	certPEM, keyPEM := generateSelfSignedPEM(t)
	dir := t.TempDir()
	certPath := writeTempFile(t, dir, "client.crt", certPEM)
	keyPath := writeTempFile(t, dir, "client.key", keyPEM)

	h := &Handler{NatsTLSCert: certPath, NatsTLSKey: keyPath}
	cfg, err := h.buildTLSConfig()
	if err != nil {
		t.Fatalf("buildTLSConfig: %v", err)
	}
	if len(cfg.Certificates) != 1 {
		t.Fatalf("expected one client certificate, got %d", len(cfg.Certificates))
	}
	if cfg.RootCAs != nil {
		t.Error("expected RootCAs to be nil when no CA is configured")
	}
}

func TestBuildTLSConfig_InsecureSkipVerifyHonored(t *testing.T) {
	h := &Handler{NatsTLSInsecureSkipVerify: true}
	cfg, err := h.buildTLSConfig()
	if err != nil {
		t.Fatalf("buildTLSConfig: %v", err)
	}
	if !cfg.InsecureSkipVerify {
		t.Error("expected InsecureSkipVerify=true")
	}
}

func TestBuildTLSConfig_MalformedCAReturnsError(t *testing.T) {
	dir := t.TempDir()
	caPath := writeTempFile(t, dir, "bogus.pem", []byte("not a certificate"))

	h := &Handler{NatsTLSCA: caPath}
	if _, err := h.buildTLSConfig(); err == nil {
		t.Fatal("expected error when CA bundle contains no valid PEM blocks")
	}
}

func TestBuildTLSConfig_MissingCAFileReturnsError(t *testing.T) {
	h := &Handler{NatsTLSCA: filepath.Join(t.TempDir(), "does-not-exist.pem")}
	if _, err := h.buildTLSConfig(); err == nil {
		t.Fatal("expected error when CA file is missing")
	}
}

func TestBuildTLSConfig_InvalidCertKeyPairReturnsError(t *testing.T) {
	dir := t.TempDir()
	certPath := writeTempFile(t, dir, "cert.pem", []byte("not a cert"))
	keyPath := writeTempFile(t, dir, "key.pem", []byte("not a key"))

	h := &Handler{NatsTLSCert: certPath, NatsTLSKey: keyPath}
	if _, err := h.buildTLSConfig(); err == nil {
		t.Fatal("expected error when cert/key files are not valid PEM")
	}
}

// TestHandler_ConnectNATS_TLSConfigErrorPropagates exercises the error
// return on the buildTLSConfig() failure branch inside connectNATS, which
// otherwise sits behind an unreachable code path because the existing
// TLS tests call buildTLSConfig directly.
func TestHandler_ConnectNATS_TLSConfigErrorPropagates(t *testing.T) {
	h := &Handler{
		NatsTLSCA: filepath.Join(t.TempDir(), "does-not-exist.pem"),
		logger:    zap.NewNop(),
	}
	if err := h.connectNATS(); err == nil {
		t.Fatal("expected connectNATS to return the TLS-build error when CA file is missing")
	}
}

// TestHandler_ConnectNATS_TLS_LiveHandshake proves that the *tls.Config
// produced by buildTLSConfig is actually used to dial NATS — that
// nats.Secure(tlsCfg) is wired correctly in connectNATS. The existing
// TestBuildTLSConfig_* tests only inspect the returned struct; a
// regression that dropped the nats.Secure() option (e.g. replacing it
// with nats.RootCAs(...) which discards client certs and TLS-Required
// negotiation) would pass every prior TLS test but break this one.
func TestHandler_ConnectNATS_TLS_LiveHandshake(t *testing.T) {
	certPEM, keyPEM := generateSelfSignedPEM(t)
	// The certificate is self-signed for CN=nuts-test without SANs, so a
	// client that verifies the hostname rejects it unless it opts into
	// InsecureSkipVerify: the negative case below proves the operator's
	// tls.Config is the one nats.go dials with.
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("tls.X509KeyPair: %v", err)
	}
	ns := startJetStreamServer(t, func(o *server.Options) {
		o.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	})

	t.Run("InsecureSkipVerify connects against self-signed CN-only cert", func(t *testing.T) {
		h := &Handler{
			NatsURL:                   ns.ClientURL(),
			StreamName:                "EVENTS",
			NatsTLSInsecureSkipVerify: true,
			MaxReconnects:             intPtr(0),
			logger:                    zap.NewNop(),
		}
		if err := h.connectNATS(); err != nil {
			t.Fatalf("connectNATS against TLS NATS with InsecureSkipVerify: %v", err)
		}
		defer h.Cleanup()
		if !h.conn.TLSRequired() {
			t.Error("TLSRequired = false; the connection negotiated plaintext, not TLS — nats.Secure(tlsCfg) may not be wired")
		}
	})

	t.Run("strict verify against same CA fails on 127.0.0.1 hostname mismatch", func(t *testing.T) {
		// Same self-signed cert configured as a trust root; no
		// InsecureSkipVerify. The cert has CN="nuts-test" and no
		// IPAddresses SAN, so Go's verifier must reject the 127.0.0.1
		// dial. A pass here would mean either the CA bundle is being
		// ignored OR InsecureSkipVerify was silently flipped on.
		dir := t.TempDir()
		caPath := writeTempFile(t, dir, "ca.pem", certPEM)
		h := &Handler{
			NatsURL:       ns.ClientURL(),
			StreamName:    "EVENTS",
			NatsTLSCA:     caPath,
			MaxReconnects: intPtr(0),
			logger:        zap.NewNop(),
		}
		err := h.connectNATS()
		if err == nil {
			t.Fatal("expected handshake failure for hostname-mismatched self-signed CA")
		}
		// nats.go wraps the verifier error; the underlying x509 error for
		// our CN-only cert dialled by IP is:
		//   "x509: cannot validate certificate for 127.0.0.1 because it
		//    doesn't contain any IP SANs"
		// We pin to the IP-SAN phrase: it is emitted only by the x509
		// verifier during the handshake, so a pass here cannot be faked
		// by a setup-time error from buildTLSConfig (which uses phrases
		// like "no certificates found in" or "read nats_tls_ca").
		msg := err.Error()
		if !strings.Contains(msg, "doesn't contain any IP SANs") {
			t.Fatalf("error %q doesn't look like an x509 hostname-mismatch failure; nats.Secure(tlsCfg) may not be wiring buildTLSConfig's output", msg)
		}
	})
}

// TestHandler_Provision_IdleHeartbeatReachesPullOptions pins the
// nats_idle_heartbeat contract end to end: the value Provision settles on is
// the heartbeat every pull request asks JetStream for.
func TestHandler_Provision_IdleHeartbeatReachesPullOptions(t *testing.T) {
	ns := startJetStreamServer(t)
	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()
	createTestStream(t, nc, "EVENTS", []string{"events.>"})

	for _, c := range []struct {
		name       string
		configured int
		want       time.Duration
	}{
		{name: "omitted uses the 10s default", configured: 0, want: 10 * time.Second},
		{name: "explicit value is kept", configured: 5, want: 5 * time.Second},
		{name: "disable sentinel leaves the library default", configured: natsIdleHeartbeatDisabledSentinel, want: 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := &Handler{NatsURL: ns.ClientURL(), StreamName: "EVENTS", NatsIdleHeartbeat: c.configured}
			if err := h.Provision(caddy.Context{Context: context.Background()}); err != nil {
				t.Fatalf("Provision: %v", err)
			}
			defer h.Cleanup()
			var got time.Duration
			for _, opt := range h.pullOptions() {
				if hb, ok := opt.(jetstream.PullHeartbeat); ok {
					got = time.Duration(hb)
				}
			}
			if got != c.want {
				t.Fatalf("PullHeartbeat = %v, want %v", got, c.want)
			}
		})
	}
}

func TestHandler_Provision_WriteTimeoutDefaults(t *testing.T) {
	ns := startJetStreamServer(t)
	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()
	createTestStream(t, nc, "EVENTS", []string{"events.>"})

	for _, c := range []struct {
		name       string
		configured int
		want       int
	}{
		{name: "omitted uses the 30s default", configured: 0, want: defaultWriteTimeoutSeconds},
		{name: "explicit value is kept", configured: 5, want: 5},
		{name: "-1 keeps deadlines disabled", configured: writeTimeoutDisabledSentinel, want: writeTimeoutDisabledSentinel},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := &Handler{NatsURL: ns.ClientURL(), StreamName: "EVENTS", WriteTimeout: c.configured}
			if err := h.Provision(caddy.Context{Context: context.Background()}); err != nil {
				t.Fatalf("Provision: %v", err)
			}
			defer h.Cleanup()
			if h.WriteTimeout != c.want {
				t.Fatalf("WriteTimeout = %d, want %d", h.WriteTimeout, c.want)
			}
		})
	}
}
