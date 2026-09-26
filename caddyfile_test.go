package nuts

import (
	"reflect"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
)

func TestHandler_UnmarshalCaddyfile(t *testing.T) {
	tests := []struct {
		name        string
		caddyfile   string
		expected    *Handler
		expectError bool
	}{
		{
			name: "full configuration",
			caddyfile: `nuts {
				nats_url nats://localhost:4222
				stream_name EVENTS
				topic_prefix events.
				nats_tls_ca /path/to/ca.pem
				nats_tls_cert /path/to/client.crt
				nats_tls_key /path/to/client.key
				nats_tls_insecure_skip_verify true
				heartbeat_interval 15
				reconnect_wait 5
				max_reconnects 10
				max_event_size 524288
				max_connections 100
				max_topics_per_subscription 8
				client_buffer_size 128
				dispatch_timeout 2
				write_timeout 3
				replay_max_messages 25
				replay_window 300
				health_path /health
				live_path /live
				ready_path /ready
				hub_url https://example.com/events
				subscriber_jwt_key secret-key
				subscriber_jwt_cookie nuts_session
				allowed_origins https://example.com https://other.com
				allowed_headers Cache-Control Last-Event-ID Authorization
				allowed_methods GET OPTIONS
			}`,
			expected: &Handler{
				NatsURL:                   "nats://localhost:4222",
				StreamName:                "EVENTS",
				TopicPrefix:               "events.",
				NatsTLSCA:                 "/path/to/ca.pem",
				NatsTLSCert:               "/path/to/client.crt",
				NatsTLSKey:                "/path/to/client.key",
				NatsTLSInsecureSkipVerify: true,
				HeartbeatInterval:         15,
				ReconnectWait:             5,
				MaxReconnects:             intPtr(10),
				MaxEventSize:              524288,
				MaxConnections:            100,
				MaxTopicsPerSubscription:  8,
				ClientBufferSize:          128,
				DispatchTimeout:           2,
				WriteTimeout:              3,
				ReplayMaxMessages:         25,
				ReplayWindow:              300,
				HealthPath:                "/health",
				LivePath:                  "/live",
				ReadyPath:                 "/ready",
				HubURL:                    "https://example.com/events",
				SubscriberJWTKey:          "secret-key",
				SubscriberJWTCookie:       "nuts_session",
				AllowedOrigins:            []string{"https://example.com", "https://other.com"},
				AllowedHeaders:            []string{"Cache-Control", "Last-Event-ID", "Authorization"},
				AllowedMethods:            []string{"GET", "OPTIONS"},
			},
			expectError: false,
		},
		{
			name: "bare insecure tls skip verify",
			caddyfile: `nuts {
				nats_url nats://localhost:4222
				stream_name EVENTS
				nats_tls_insecure_skip_verify
			}`,
			expected: &Handler{
				NatsURL:                   "nats://localhost:4222",
				StreamName:                "EVENTS",
				NatsTLSInsecureSkipVerify: true,
			},
			expectError: false,
		},
		{
			name: "minimal configuration",
			caddyfile: `nuts {
				nats_url nats://localhost:4222
				stream_name MYSTREAM
			}`,
			expected: &Handler{
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
			expected: &Handler{
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
			expected: &Handler{
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
			expected: &Handler{
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
			expected:    nil,
			expectError: true,
		},
		{
			name: "invalid reconnect_wait",
			caddyfile: `nuts {
				nats_url nats://localhost:4222
				stream_name EVENTS
				reconnect_wait invalid
			}`,
			expected:    nil,
			expectError: true,
		},
		{
			name: "invalid max_reconnects",
			caddyfile: `nuts {
				nats_url nats://localhost:4222
				stream_name EVENTS
				max_reconnects invalid
			}`,
			expected:    nil,
			expectError: true,
		},
		{
			name: "invalid max_event_size",
			caddyfile: `nuts {
				nats_url nats://localhost:4222
				stream_name EVENTS
				max_event_size invalid
			}`,
			expected:    nil,
			expectError: true,
		},
		{
			name: "allowed_origins requires at least one value",
			caddyfile: `nuts {
				nats_url nats://localhost:4222
				stream_name EVENTS
				allowed_origins
			}`,
			expected:    nil,
			expectError: true,
		},
		{
			name: "allowed_headers requires at least one value",
			caddyfile: `nuts {
				nats_url nats://localhost:4222
				stream_name EVENTS
				allowed_headers
			}`,
			expected:    nil,
			expectError: true,
		},
		{
			name: "allowed_methods requires at least one value",
			caddyfile: `nuts {
				nats_url nats://localhost:4222
				stream_name EVENTS
				allowed_methods
			}`,
			expected:    nil,
			expectError: true,
		},
		{
			name: "invalid nats_tls_insecure_skip_verify",
			caddyfile: `nuts {
				nats_url nats://localhost:4222
				stream_name EVENTS
				nats_tls_insecure_skip_verify maybe
			}`,
			expected:    nil,
			expectError: true,
		},
		{
			name: "missing nats_tls_ca argument",
			caddyfile: `nuts {
				nats_url nats://localhost:4222
				stream_name EVENTS
				nats_tls_ca
			}`,
			expected:    nil,
			expectError: true,
		},
		{
			name: "missing nats_tls_cert argument",
			caddyfile: `nuts {
				nats_url nats://localhost:4222
				stream_name EVENTS
				nats_tls_cert
			}`,
			expected:    nil,
			expectError: true,
		},
		{
			name: "missing nats_tls_key argument",
			caddyfile: `nuts {
				nats_url nats://localhost:4222
				stream_name EVENTS
				nats_tls_key
			}`,
			expected:    nil,
			expectError: true,
		},
		{
			name: "invalid max_topics_per_subscription",
			caddyfile: `nuts {
				nats_url nats://localhost:4222
				stream_name EVENTS
				max_topics_per_subscription invalid
			}`,
			expected:    nil,
			expectError: true,
		},
		{
			name: "unrecognized option",
			caddyfile: `nuts {
				nats_url nats://localhost:4222
				stream_name EVENTS
				unknown_option value
			}`,
			expected:    nil,
			expectError: true,
		},
		{
			name: "missing stream_name argument",
			caddyfile: `nuts {
				nats_url nats://localhost:4222
				stream_name
			}`,
			expected:    nil,
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
			switch {
			case tt.expected.MaxReconnects == nil && h.MaxReconnects != nil:
				t.Errorf("MaxReconnects: expected nil, got %d", *h.MaxReconnects)
			case tt.expected.MaxReconnects != nil && h.MaxReconnects == nil:
				t.Errorf("MaxReconnects: expected %d, got nil", *tt.expected.MaxReconnects)
			case tt.expected.MaxReconnects != nil && h.MaxReconnects != nil && *tt.expected.MaxReconnects != *h.MaxReconnects:
				t.Errorf("MaxReconnects: expected %d, got %d", *tt.expected.MaxReconnects, *h.MaxReconnects)
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
			if h.NatsTLSCA != tt.expected.NatsTLSCA {
				t.Errorf("NatsTLSCA: expected %q, got %q", tt.expected.NatsTLSCA, h.NatsTLSCA)
			}
			if h.NatsTLSCert != tt.expected.NatsTLSCert {
				t.Errorf("NatsTLSCert: expected %q, got %q", tt.expected.NatsTLSCert, h.NatsTLSCert)
			}
			if h.NatsTLSKey != tt.expected.NatsTLSKey {
				t.Errorf("NatsTLSKey: expected %q, got %q", tt.expected.NatsTLSKey, h.NatsTLSKey)
			}
			if h.NatsTLSInsecureSkipVerify != tt.expected.NatsTLSInsecureSkipVerify {
				t.Errorf("NatsTLSInsecureSkipVerify: expected %v, got %v", tt.expected.NatsTLSInsecureSkipVerify, h.NatsTLSInsecureSkipVerify)
			}
			if h.MaxEventSize != tt.expected.MaxEventSize {
				t.Errorf("MaxEventSize: expected %d, got %d", tt.expected.MaxEventSize, h.MaxEventSize)
			}
			if h.MaxConnections != tt.expected.MaxConnections {
				t.Errorf("MaxConnections: expected %d, got %d", tt.expected.MaxConnections, h.MaxConnections)
			}
			if h.MaxTopicsPerSubscription != tt.expected.MaxTopicsPerSubscription {
				t.Errorf("MaxTopicsPerSubscription: expected %d, got %d", tt.expected.MaxTopicsPerSubscription, h.MaxTopicsPerSubscription)
			}
			if h.ClientBufferSize != tt.expected.ClientBufferSize {
				t.Errorf("ClientBufferSize: expected %d, got %d", tt.expected.ClientBufferSize, h.ClientBufferSize)
			}
			if h.DispatchTimeout != tt.expected.DispatchTimeout {
				t.Errorf("DispatchTimeout: expected %d, got %d", tt.expected.DispatchTimeout, h.DispatchTimeout)
			}
			if h.WriteTimeout != tt.expected.WriteTimeout {
				t.Errorf("WriteTimeout: expected %d, got %d", tt.expected.WriteTimeout, h.WriteTimeout)
			}
			if h.ReplayMaxMessages != tt.expected.ReplayMaxMessages {
				t.Errorf("ReplayMaxMessages: expected %d, got %d", tt.expected.ReplayMaxMessages, h.ReplayMaxMessages)
			}
			if h.ReplayWindow != tt.expected.ReplayWindow {
				t.Errorf("ReplayWindow: expected %d, got %d", tt.expected.ReplayWindow, h.ReplayWindow)
			}
			if h.HealthPath != tt.expected.HealthPath {
				t.Errorf("HealthPath: expected %q, got %q", tt.expected.HealthPath, h.HealthPath)
			}
			if h.HubURL != tt.expected.HubURL {
				t.Errorf("HubURL: expected %q, got %q", tt.expected.HubURL, h.HubURL)
			}
			if h.SubscriberJWTKey != tt.expected.SubscriberJWTKey {
				t.Errorf("SubscriberJWTKey: expected %q, got %q", tt.expected.SubscriberJWTKey, h.SubscriberJWTKey)
			}
			if h.SubscriberJWTCookie != tt.expected.SubscriberJWTCookie {
				t.Errorf("SubscriberJWTCookie: expected %q, got %q", tt.expected.SubscriberJWTCookie, h.SubscriberJWTCookie)
			}
			if h.LivePath != tt.expected.LivePath {
				t.Errorf("LivePath: expected %q, got %q", tt.expected.LivePath, h.LivePath)
			}
			if h.ReadyPath != tt.expected.ReadyPath {
				t.Errorf("ReadyPath: expected %q, got %q", tt.expected.ReadyPath, h.ReadyPath)
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
			if len(tt.expected.AllowedHeaders) > 0 && !reflect.DeepEqual(h.AllowedHeaders, tt.expected.AllowedHeaders) {
				t.Errorf("AllowedHeaders: expected %#v, got %#v", tt.expected.AllowedHeaders, h.AllowedHeaders)
			}
			if len(tt.expected.AllowedMethods) > 0 && !reflect.DeepEqual(h.AllowedMethods, tt.expected.AllowedMethods) {
				t.Errorf("AllowedMethods: expected %#v, got %#v", tt.expected.AllowedMethods, h.AllowedMethods)
			}
		})
	}
}

func TestParseCaddyfile(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		helper := httpcaddyfile.Helper{
			Dispenser: caddyfile.NewTestDispenser(`nuts {
				nats_url nats://localhost:4222
				stream_name EVENTS
				topic_prefix events.
			}`),
		}

		middleware, err := parseCaddyfile(helper)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		handler, ok := middleware.(*Handler)
		if !ok {
			t.Fatalf("expected *Handler, got %T", middleware)
		}
		if handler.NatsURL != "nats://localhost:4222" {
			t.Errorf("expected NatsURL to be parsed, got %q", handler.NatsURL)
		}
		if handler.StreamName != "EVENTS" {
			t.Errorf("expected StreamName to be parsed, got %q", handler.StreamName)
		}
		if handler.TopicPrefix != "events." {
			t.Errorf("expected TopicPrefix to be parsed, got %q", handler.TopicPrefix)
		}
	})

	t.Run("error", func(t *testing.T) {
		helper := httpcaddyfile.Helper{
			Dispenser: caddyfile.NewTestDispenser(`nuts {
				stream_name EVENTS
				nats_url
			}`),
		}

		if _, err := parseCaddyfile(helper); err == nil {
			t.Fatal("expected parseCaddyfile to return an error")
		}
	})
}

func TestHandler_UnmarshalCaddyfile_MissingArgs(t *testing.T) {
	tests := []struct {
		name      string
		directive string
	}{
		{name: "missing nats_credentials arg", directive: "nats_credentials"},
		{name: "missing nats_token arg", directive: "nats_token"},
		{name: "missing nats_user arg", directive: "nats_user"},
		{name: "missing nats_password arg", directive: "nats_password"},
		{name: "missing subscriber_jwt_key arg", directive: "subscriber_jwt_key"},
		{name: "missing subscriber_jwt_cookie arg", directive: "subscriber_jwt_cookie"},
		{name: "missing topic_prefix arg", directive: "topic_prefix"},
		{name: "missing heartbeat_interval arg", directive: "heartbeat_interval"},
		{name: "missing reconnect_wait arg", directive: "reconnect_wait"},
		{name: "missing max_reconnects arg", directive: "max_reconnects"},
		{name: "missing max_event_size arg", directive: "max_event_size"},
		{name: "missing dispatch_timeout arg", directive: "dispatch_timeout"},
		{name: "missing write_timeout arg", directive: "write_timeout"},
		{name: "missing replay_max_messages arg", directive: "replay_max_messages"},
		{name: "missing replay_window arg", directive: "replay_window"},
		{name: "missing health_path arg", directive: "health_path"},
		{name: "missing live_path arg", directive: "live_path"},
		{name: "missing ready_path arg", directive: "ready_path"},
		{name: "missing hub_url arg", directive: "hub_url"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := caddyfile.NewTestDispenser("nuts {\n" +
				"nats_url nats://localhost:4222\n" +
				"stream_name EVENTS\n" +
				tt.directive + "\n" +
				"}")

			var h Handler
			if err := h.UnmarshalCaddyfile(d); err == nil {
				t.Fatal("expected missing argument error")
			}
		})
	}
}

func TestHandler_UnmarshalCaddyfile_HubURL(t *testing.T) {
	t.Run("hub_url parsed correctly", func(t *testing.T) {
		caddyfileInput := `nuts {
			nats_url nats://localhost:4222
			stream_name EVENTS
			hub_url https://example.com/hub
		}`
		d := caddyfile.NewTestDispenser(caddyfileInput)
		h := Handler{}
		if err := h.UnmarshalCaddyfile(d); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if h.HubURL != "https://example.com/hub" {
			t.Errorf("HubURL: expected %q, got %q", "https://example.com/hub", h.HubURL)
		}
	})

	t.Run("hub_url missing argument", func(t *testing.T) {
		caddyfileInput := `nuts {
			nats_url nats://localhost:4222
			stream_name EVENTS
			hub_url
		}`
		d := caddyfile.NewTestDispenser(caddyfileInput)
		h := Handler{}
		if err := h.UnmarshalCaddyfile(d); err == nil {
			t.Error("expected error for missing hub_url argument")
		}
	})
}

// ── MaxReconnects=0 honored when user wrote it ────────────────────────────

func TestHandler_MaxReconnectsZero_HonoredFromCaddyfile(t *testing.T) {
	input := `nuts {
        nats_url nats://localhost:4222
        stream_name EVENTS
        max_reconnects 0
    }`
	d := caddyfile.NewTestDispenser(input)
	h := Handler{}
	if err := h.UnmarshalCaddyfile(d); err != nil {
		t.Fatalf("UnmarshalCaddyfile: %v", err)
	}
	if h.MaxReconnects == nil {
		t.Fatalf("MaxReconnects should be set after explicit directive")
	}
	if *h.MaxReconnects != 0 {
		t.Errorf("MaxReconnects should be 0 after explicit directive, got %d", *h.MaxReconnects)
	}
}

func TestHandler_MaxReconnectsDefault_WhenOmitted(t *testing.T) {
	input := `nuts {
        nats_url nats://localhost:4222
        stream_name EVENTS
    }`
	d := caddyfile.NewTestDispenser(input)
	h := Handler{}
	if err := h.UnmarshalCaddyfile(d); err != nil {
		t.Fatalf("UnmarshalCaddyfile: %v", err)
	}
	if h.MaxReconnects != nil {
		t.Errorf("MaxReconnects should be nil when directive omitted, got %d", *h.MaxReconnects)
	}
}

// ── Integer directives reject junk suffix ────────────────────────────────

func TestHandler_UnmarshalCaddyfile_RejectsNonNumericInt(t *testing.T) {
	cases := []string{
		"heartbeat_interval 123abc",
		"reconnect_wait 9x",
		"max_reconnects 1.5",
		"max_event_size 1kb",
		"max_connections twelve",
		"client_buffer_size -",
		"dispatch_timeout 1s",
		"write_timeout 2s",
		"nats_idle_heartbeat 5x",
	}
	for _, line := range cases {
		line := line
		t.Run(line, func(t *testing.T) {
			input := "nuts {\n    nats_url nats://localhost:4222\n    stream_name EVENTS\n    " + line + "\n}"
			d := caddyfile.NewTestDispenser(input)
			h := Handler{}
			if err := h.UnmarshalCaddyfile(d); err == nil {
				t.Errorf("expected parse error for %q", line)
			}
		})
	}
}

func TestHandler_UnmarshalCaddyfile_RejectsInvalidOptionalConfig(t *testing.T) {
	tests := []struct {
		name        string
		line        string
		wantErr     string
		validateErr bool
	}{
		{
			name:        "max reconnects below unlimited sentinel",
			line:        "max_reconnects -2",
			wantErr:     "max_reconnects",
			validateErr: true,
		},
		{
			name:    "negative max connections",
			line:    "max_connections -1",
			wantErr: "max_connections",
		},
		{
			name:    "negative client buffer size",
			line:    "client_buffer_size -1",
			wantErr: "client_buffer_size",
		},
		{
			name:    "negative dispatch timeout",
			line:    "dispatch_timeout -1",
			wantErr: "dispatch_timeout",
		},
		{
			name:    "write timeout below the -1 disable sentinel",
			line:    "write_timeout -2",
			wantErr: "write_timeout",
		},
		{
			name:    "negative replay max messages",
			line:    "replay_max_messages -1",
			wantErr: "replay_max_messages",
		},
		{
			name:    "negative replay window",
			line:    "replay_window -1",
			wantErr: "replay_window",
		},
		{
			name:        "unsupported allowed method",
			line:        "allowed_methods GET POST OPTIONS",
			wantErr:     "allowed_methods",
			validateErr: true,
		},
		{
			name:        "subscriber cookie requires key",
			line:        "subscriber_jwt_cookie nuts_session",
			wantErr:     "subscriber_jwt_key",
			validateErr: true,
		},
		{
			name:        "subscriber cookie validates name",
			line:        "subscriber_jwt_key secret\n    subscriber_jwt_cookie bad;name",
			wantErr:     "subscriber_jwt_cookie",
			validateErr: true,
		},
		{
			name:    "negative heartbeat interval at parse time",
			line:    "heartbeat_interval -30",
			wantErr: "heartbeat_interval must be >= 0",
		},
		{
			name:    "negative reconnect wait at parse time",
			line:    "reconnect_wait -1",
			wantErr: "reconnect_wait must be >= 0",
		},
		{
			name:    "topic cap below the -1 sentinel",
			line:    "max_topics_per_subscription -2",
			wantErr: "max_topics_per_subscription",
		},
		{
			name:        "CA bundle with verification disabled",
			line:        "nats_tls_ca /etc/nats/ca.pem\n    nats_tls_insecure_skip_verify",
			wantErr:     "nats_tls_ca cannot be combined with nats_tls_insecure_skip_verify",
			validateErr: true,
		},
		{
			name:        "unsupported nats_url scheme",
			line:        "nats_url http://localhost:4222",
			wantErr:     `nats_url scheme "http" is not supported`,
			validateErr: true,
		},
		{
			name:        "comma-joined allowed origins",
			line:        "allowed_origins https://a.example.com,https://b.example.com",
			wantErr:     "allowed_origins",
			validateErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := "nuts {\n    nats_url nats://localhost:4222\n    stream_name EVENTS\n    " + tt.line + "\n}"
			d := caddyfile.NewTestDispenser(input)
			h := Handler{}
			err := h.UnmarshalCaddyfile(d)
			if tt.validateErr {
				if err != nil {
					t.Fatalf("UnmarshalCaddyfile returned error before validation: %v", err)
				}
				err = h.Validate()
			}
			if err == nil {
				t.Fatalf("expected error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %q, want to contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestHandler_UnmarshalCaddyfile_PreservesSentinelConfigSemantics(t *testing.T) {
	input := `nuts {
        nats_url nats://localhost:4222
        stream_name EVENTS
        max_event_size -1
        client_buffer_size 0
    }`
	d := caddyfile.NewTestDispenser(input)
	h := Handler{}
	if err := h.UnmarshalCaddyfile(d); err != nil {
		t.Fatalf("UnmarshalCaddyfile: %v", err)
	}
	if err := h.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if h.MaxEventSize != -1 {
		t.Fatalf("MaxEventSize = %d, want -1 unlimited sentinel", h.MaxEventSize)
	}
	if h.ClientBufferSize != 0 {
		t.Fatalf("ClientBufferSize = %d, want 0 default sentinel", h.ClientBufferSize)
	}
}

func TestHandler_UnmarshalCaddyfile_SharedSubscriptions(t *testing.T) {
	for _, c := range []struct {
		line    string
		want    bool
		wantErr bool
	}{
		{line: "shared_subscriptions", want: true},
		{line: "shared_subscriptions true", want: true},
		{line: "shared_subscriptions false", want: false},
		{line: "shared_subscriptions maybe", wantErr: true},
	} {
		d := caddyfile.NewTestDispenser("nuts {\n    nats_url nats://localhost:4222\n    stream_name EVENTS\n    " + c.line + "\n}")
		h := Handler{}
		err := h.UnmarshalCaddyfile(d)
		if c.wantErr {
			if err == nil || !strings.Contains(err.Error(), "shared_subscriptions") {
				t.Errorf("%q: error = %v, want an invalid shared_subscriptions error", c.line, err)
			}
			continue
		}
		if err != nil || h.SharedSubscriptions != c.want {
			t.Errorf("%q: SharedSubscriptions = %v (err %v), want %v", c.line, h.SharedSubscriptions, err, c.want)
		}
	}
}
