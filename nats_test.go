package nuts

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"
)

// startJetStreamServer starts an embedded NATS server with JetStream enabled for testing
func startJetStreamServer(t *testing.T) *server.Server {
	t.Helper()
	opts := &server.Options{
		Host:      "127.0.0.1",
		Port:      -1, // Random available port
		JetStream: true,
		StoreDir:  t.TempDir(),
	}
	ns, err := server.NewServer(opts)
	if err != nil {
		t.Fatalf("failed to create NATS server: %v", err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		t.Fatal("NATS server not ready")
	}
	return ns
}

// createTestStream creates a JetStream stream for testing
func createTestStream(t *testing.T, nc *nats.Conn, streamName string, subjects []string) {
	t.Helper()
	js, err := nc.JetStream()
	if err != nil {
		t.Fatalf("failed to get JetStream context: %v", err)
	}
	_, err = js.AddStream(&nats.StreamConfig{
		Name:     streamName,
		Subjects: subjects,
		Storage:  nats.MemoryStorage,
	})
	if err != nil {
		t.Fatalf("failed to create stream: %v", err)
	}
}

func TestHandler_Validate(t *testing.T) {
	tests := []struct {
		name        string
		handler     Handler
		expectError bool
		errorMsg    string
	}{
		{
			name: "valid configuration",
			handler: Handler{
				NatsURL:    "nats://localhost:4222",
				StreamName: "EVENTS",
			},
			expectError: false,
		},
		{
			name: "missing nats_url uses default",
			handler: Handler{
				StreamName: "EVENTS",
			},
			expectError: true,
			errorMsg:    "nats_url is required",
		},
		{
			name: "missing stream_name",
			handler: Handler{
				NatsURL: "nats://localhost:4222",
			},
			expectError: true,
			errorMsg:    "stream_name is required",
		},
		{
			name:        "missing both",
			handler:     Handler{},
			expectError: true,
			errorMsg:    "nats_url is required",
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

func TestHandler_UnmarshalCaddyfile(t *testing.T) {
	tests := []struct {
		name        string
		caddyfile   string
		expected    Handler
		expectError bool
	}{
		{
			name: "full configuration",
			caddyfile: `nuts {
				nats_url nats://localhost:4222
				stream_name EVENTS
				topic_prefix events.
				heartbeat_interval 15
				reconnect_wait 5
				max_reconnects 10
				allowed_origins https://example.com https://other.com
			}`,
			expected: Handler{
				NatsURL:           "nats://localhost:4222",
				StreamName:        "EVENTS",
				TopicPrefix:       "events.",
				HeartbeatInterval: 15,
				ReconnectWait:     5,
				MaxReconnects:     10,
				AllowedOrigins:    []string{"https://example.com", "https://other.com"},
			},
			expectError: false,
		},
		{
			name: "minimal configuration",
			caddyfile: `nuts {
				nats_url nats://localhost:4222
				stream_name MYSTREAM
			}`,
			expected: Handler{
				NatsURL:    "nats://localhost:4222",
				StreamName: "MYSTREAM",
			},
			expectError: false,
		},
		{
			name: "with authentication options",
			caddyfile: `nuts {
				nats_url nats://localhost:4222
				stream_name EVENTS
				nats_token mytoken
			}`,
			expected: Handler{
				NatsURL:    "nats://localhost:4222",
				StreamName: "EVENTS",
				NatsToken:  "mytoken",
			},
			expectError: false,
		},
		{
			name: "with user/password auth",
			caddyfile: `nuts {
				nats_url nats://localhost:4222
				stream_name EVENTS
				nats_user myuser
				nats_password mypassword
			}`,
			expected: Handler{
				NatsURL:      "nats://localhost:4222",
				StreamName:   "EVENTS",
				NatsUser:     "myuser",
				NatsPassword: "mypassword",
			},
			expectError: false,
		},
		{
			name: "with credentials file",
			caddyfile: `nuts {
				nats_url nats://localhost:4222
				stream_name EVENTS
				nats_credentials /path/to/creds.creds
			}`,
			expected: Handler{
				NatsURL:         "nats://localhost:4222",
				StreamName:      "EVENTS",
				NatsCredentials: "/path/to/creds.creds",
			},
			expectError: false,
		},
		{
			name: "invalid heartbeat_interval",
			caddyfile: `nuts {
				nats_url nats://localhost:4222
				stream_name EVENTS
				heartbeat_interval invalid
			}`,
			expectError: true,
		},
		{
			name: "unrecognized option",
			caddyfile: `nuts {
				nats_url nats://localhost:4222
				stream_name EVENTS
				unknown_option value
			}`,
			expectError: true,
		},
		{
			name: "missing stream_name argument",
			caddyfile: `nuts {
				nats_url nats://localhost:4222
				stream_name
			}`,
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := caddyfile.NewTestDispenser(tt.caddyfile)
			h := Handler{}
			err := h.UnmarshalCaddyfile(d)

			if tt.expectError {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			// Check fields
			if h.NatsURL != tt.expected.NatsURL {
				t.Errorf("NatsURL: expected %q, got %q", tt.expected.NatsURL, h.NatsURL)
			}
			if h.StreamName != tt.expected.StreamName {
				t.Errorf("StreamName: expected %q, got %q", tt.expected.StreamName, h.StreamName)
			}
			if h.TopicPrefix != tt.expected.TopicPrefix {
				t.Errorf("TopicPrefix: expected %q, got %q", tt.expected.TopicPrefix, h.TopicPrefix)
			}
			if h.HeartbeatInterval != tt.expected.HeartbeatInterval {
				t.Errorf("HeartbeatInterval: expected %d, got %d", tt.expected.HeartbeatInterval, h.HeartbeatInterval)
			}
			if h.ReconnectWait != tt.expected.ReconnectWait {
				t.Errorf("ReconnectWait: expected %d, got %d", tt.expected.ReconnectWait, h.ReconnectWait)
			}
			if h.MaxReconnects != tt.expected.MaxReconnects {
				t.Errorf("MaxReconnects: expected %d, got %d", tt.expected.MaxReconnects, h.MaxReconnects)
			}
			if h.NatsToken != tt.expected.NatsToken {
				t.Errorf("NatsToken: expected %q, got %q", tt.expected.NatsToken, h.NatsToken)
			}
			if h.NatsUser != tt.expected.NatsUser {
				t.Errorf("NatsUser: expected %q, got %q", tt.expected.NatsUser, h.NatsUser)
			}
			if h.NatsPassword != tt.expected.NatsPassword {
				t.Errorf("NatsPassword: expected %q, got %q", tt.expected.NatsPassword, h.NatsPassword)
			}
			if h.NatsCredentials != tt.expected.NatsCredentials {
				t.Errorf("NatsCredentials: expected %q, got %q", tt.expected.NatsCredentials, h.NatsCredentials)
			}
			if len(tt.expected.AllowedOrigins) > 0 {
				if len(h.AllowedOrigins) != len(tt.expected.AllowedOrigins) {
					t.Errorf("AllowedOrigins length: expected %d, got %d", len(tt.expected.AllowedOrigins), len(h.AllowedOrigins))
				}
				for i, origin := range tt.expected.AllowedOrigins {
					if i < len(h.AllowedOrigins) && h.AllowedOrigins[i] != origin {
						t.Errorf("AllowedOrigins[%d]: expected %q, got %q", i, origin, h.AllowedOrigins[i])
					}
				}
			}
		})
	}
}

func TestHandler_ServeHTTP_Integration(t *testing.T) {
	// Start embedded NATS server with JetStream
	ns := startJetStreamServer(t)
	defer ns.Shutdown()

	// Connect to the server
	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("failed to connect to NATS: %v", err)
	}
	defer nc.Close()

	// Create test stream
	createTestStream(t, nc, "TEST_EVENTS", []string{"events.>"})

	// Create and provision handler
	h := &Handler{
		NatsURL:           ns.ClientURL(),
		StreamName:        "TEST_EVENTS",
		TopicPrefix:       "events.",
		HeartbeatInterval: 30,
		ReconnectWait:     2,
		MaxReconnects:     -1,
		AllowedOrigins:    []string{"*"},
		logger:            zap.NewNop(),
	}

	// Connect handler to NATS
	if err := h.connectNATS(); err != nil {
		t.Fatalf("failed to connect handler to NATS: %v", err)
	}
	defer h.Cleanup()

	// Initialize JetStream context
	js, err := h.conn.JetStream()
	if err != nil {
		t.Fatalf("failed to create JetStream context: %v", err)
	}
	h.js = js

	t.Run("SSE connection and message delivery", func(t *testing.T) {
		// Create request with topic
		req := httptest.NewRequest(http.MethodGet, "/events?topic=test", nil)
		ctx, cancel := context.WithTimeout(req.Context(), 3*time.Second)
		defer cancel()
		req = req.WithContext(ctx)

		// Create response recorder that supports flushing
		rr := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}

		// Start serving in goroutine
		done := make(chan error, 1)
		go func() {
			done <- h.ServeHTTP(rr, req, nil)
		}()

		// Wait for connection to establish
		time.Sleep(100 * time.Millisecond)

		// Publish a test message via JetStream
		jsCtx, _ := nc.JetStream()
		if _, err := jsCtx.Publish("events.test", []byte(`{"hello":"world"}`)); err != nil {
			t.Fatalf("failed to publish message: %v", err)
		}

		// Wait for message or timeout
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			cancel()
			<-done
		}

		// Check response headers
		if ct := rr.Header().Get("Content-Type"); ct != "text/event-stream" {
			t.Errorf("Content-Type: expected text/event-stream, got %q", ct)
		}

		body := rr.Body.String()

		// Should contain connected event
		if !strings.Contains(body, "event: connected") {
			t.Error("response should contain 'event: connected'")
		}

		// Should contain message event with ID
		if !strings.Contains(body, "event: message") {
			t.Error("response should contain 'event: message'")
		}

		// Should contain id field for replay support
		if !strings.Contains(body, "id: ") {
			t.Error("response should contain 'id: ' for replay support")
		}
	})

	t.Run("SSE with last-id parameter", func(t *testing.T) {
		// First, publish some messages to have history
		jsCtx, _ := nc.JetStream()
		for i := 0; i < 3; i++ {
			msg := map[string]interface{}{"count": i}
			data, _ := json.Marshal(msg)
			if _, err := jsCtx.Publish("events.history", data); err != nil {
				t.Fatalf("failed to publish message: %v", err)
			}
		}
		time.Sleep(100 * time.Millisecond)

		// Request with last-id=1 should get messages after sequence 1
		req := httptest.NewRequest(http.MethodGet, "/events?topic=history&last-id=1", nil)
		ctx, cancel := context.WithTimeout(req.Context(), 2*time.Second)
		defer cancel()
		req = req.WithContext(ctx)

		rr := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}

		done := make(chan error, 1)
		go func() {
			done <- h.ServeHTTP(rr, req, nil)
		}()

		select {
		case <-done:
		case <-time.After(1 * time.Second):
			cancel()
			<-done
		}

		body := rr.Body.String()

		// Should have connected event
		if !strings.Contains(body, "event: connected") {
			t.Error("response should contain 'event: connected'")
		}
	})

	t.Run("invalid last-id parameter", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/events?topic=test&last-id=invalid", nil)
		ctx, cancel := context.WithTimeout(req.Context(), 500*time.Millisecond)
		defer cancel()
		req = req.WithContext(ctx)
		rr := httptest.NewRecorder()

		h.ServeHTTP(rr, req, nil)

		if rr.Code != http.StatusBadRequest {
			t.Errorf("expected status %d for invalid last-id, got %d", http.StatusBadRequest, rr.Code)
		}

		if !strings.Contains(rr.Body.String(), "Invalid last-id") {
			t.Errorf("response should mention invalid last-id, got: %s", rr.Body.String())
		}
	})

	t.Run("no topics specified", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		ctx, cancel := context.WithTimeout(req.Context(), 500*time.Millisecond)
		defer cancel()
		req = req.WithContext(ctx)
		rr := httptest.NewRecorder()

		h.ServeHTTP(rr, req, nil)

		if rr.Code != http.StatusBadRequest {
			t.Errorf("expected status %d, got %d", http.StatusBadRequest, rr.Code)
		}
	})

	t.Run("path-based topic", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/mytopic", nil)
		ctx, cancel := context.WithTimeout(req.Context(), 500*time.Millisecond)
		defer cancel()
		req = req.WithContext(ctx)

		rr := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}

		done := make(chan error, 1)
		go func() {
			done <- h.ServeHTTP(rr, req, nil)
		}()

		select {
		case <-done:
		case <-time.After(1 * time.Second):
			cancel()
			<-done
		}

		body := rr.Body.String()
		if !strings.Contains(body, `"topics":["mytopic"]`) {
			t.Errorf("response should contain topic 'mytopic', got: %s", body)
		}
	})

	t.Run("CORS preflight", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodOptions, "/events?topic=test", nil)
		req.Header.Set("Origin", "https://example.com")
		rr := httptest.NewRecorder()

		h.ServeHTTP(rr, req, nil)

		if rr.Code != http.StatusNoContent {
			t.Errorf("expected status %d, got %d", http.StatusNoContent, rr.Code)
		}

		if origin := rr.Header().Get("Access-Control-Allow-Origin"); origin != "https://example.com" {
			t.Errorf("Access-Control-Allow-Origin: expected 'https://example.com', got %q", origin)
		}
	})

	t.Run("multiple topics", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/events?topic=topic1&topic=topic2", nil)
		ctx, cancel := context.WithTimeout(req.Context(), 500*time.Millisecond)
		defer cancel()
		req = req.WithContext(ctx)

		rr := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}

		done := make(chan error, 1)
		go func() {
			done <- h.ServeHTTP(rr, req, nil)
		}()

		select {
		case <-done:
		case <-time.After(1 * time.Second):
			cancel()
			<-done
		}

		body := rr.Body.String()
		if !strings.Contains(body, "topic1") || !strings.Contains(body, "topic2") {
			t.Errorf("response should contain both topics, got: %s", body)
		}
	})
}

func TestHandler_StreamNotFound(t *testing.T) {
	// Start embedded NATS server with JetStream
	ns := startJetStreamServer(t)
	defer ns.Shutdown()

	// Create handler with non-existent stream
	h := &Handler{
		NatsURL:           ns.ClientURL(),
		StreamName:        "NONEXISTENT_STREAM",
		TopicPrefix:       "events.",
		HeartbeatInterval: 30,
		ReconnectWait:     2,
		MaxReconnects:     -1,
		AllowedOrigins:    []string{"*"},
		logger:            zap.NewNop(),
	}

	// Connect to NATS
	if err := h.connectNATS(); err != nil {
		t.Fatalf("failed to connect to NATS: %v", err)
	}
	defer h.Cleanup()

	// Initialize JetStream context
	js, err := h.conn.JetStream()
	if err != nil {
		t.Fatalf("failed to create JetStream context: %v", err)
	}
	h.js = js

	// Verify stream doesn't exist
	_, err = h.js.StreamInfo(h.StreamName)
	if err == nil {
		t.Error("expected error for non-existent stream")
	}
}

func TestToJSON(t *testing.T) {
	tests := []struct {
		name     string
		input    interface{}
		expected string
	}{
		{
			name:     "simple map",
			input:    map[string]string{"key": "value"},
			expected: `{"key":"value"}`,
		},
		{
			name:     "slice of strings",
			input:    []string{"a", "b", "c"},
			expected: `["a","b","c"]`,
		},
		{
			name:     "nested structure",
			input:    map[string]interface{}{"nested": map[string]int{"count": 42}},
			expected: `{"nested":{"count":42}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := toJSON(tt.input)
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}

func TestTryParseJSON(t *testing.T) {
	tests := []struct {
		name     string
		input    []byte
		isJSON   bool
		expected interface{}
	}{
		{
			name:   "valid JSON object",
			input:  []byte(`{"key":"value"}`),
			isJSON: true,
		},
		{
			name:   "valid JSON array",
			input:  []byte(`[1,2,3]`),
			isJSON: true,
		},
		{
			name:     "invalid JSON returns string",
			input:    []byte(`not json`),
			isJSON:   false,
			expected: "not json",
		},
		{
			name:     "empty string",
			input:    []byte(``),
			isJSON:   false,
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tryParseJSON(tt.input)
			if tt.isJSON {
				// For valid JSON, just verify it doesn't return a string type
				if _, ok := result.(string); ok && len(tt.input) > 0 {
					t.Error("expected parsed JSON, got string")
				}
			} else {
				if str, ok := result.(string); !ok || str != tt.expected {
					t.Errorf("expected %q, got %v", tt.expected, result)
				}
			}
		})
	}
}

func TestHandler_CaddyModule(t *testing.T) {
	h := Handler{}
	info := h.CaddyModule()

	if info.ID != "http.handlers.nuts" {
		t.Errorf("expected module ID %q, got %q", "http.handlers.nuts", info.ID)
	}

	module := info.New()
	if _, ok := module.(*Handler); !ok {
		t.Error("New() did not return *Handler")
	}
}

func TestHandler_Cleanup(t *testing.T) {
	ns := startJetStreamServer(t)
	defer ns.Shutdown()

	h := &Handler{
		NatsURL:        ns.ClientURL(),
		StreamName:     "TEST",
		ReconnectWait:  1,
		MaxReconnects:  3,
		AllowedOrigins: []string{"*"},
		logger:         zap.NewNop(),
	}

	if err := h.connectNATS(); err != nil {
		t.Fatalf("failed to connect: %v", err)
	}

	// Verify connection is active
	if h.conn == nil || !h.conn.IsConnected() {
		t.Error("expected connection to be active")
	}

	// Cleanup
	if err := h.Cleanup(); err != nil {
		t.Errorf("Cleanup returned error: %v", err)
	}

	// Connection should be nil after cleanup
	if h.conn != nil {
		t.Error("expected connection to be nil after cleanup")
	}
}

// flushRecorder wraps httptest.ResponseRecorder to implement http.Flusher
type flushRecorder struct {
	*httptest.ResponseRecorder
}

func (f *flushRecorder) Flush() {
	// No-op for testing, actual flushing happens in real HTTP response
}
