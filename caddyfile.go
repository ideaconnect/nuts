// caddyfile.go — Caddyfile directive parser.
package nuts

import (
	"strconv"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

// UnmarshalCaddyfile implements caddyfile.Unmarshaler.
func (h *Handler) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	parseInt := func(field string) (int, error) {
		if !d.NextArg() {
			return 0, d.ArgErr()
		}
		v, err := strconv.Atoi(d.Val())
		if err != nil {
			return 0, d.Errf("invalid %s: %v", field, err)
		}
		return v, nil
	}

	for d.Next() {
		for d.NextBlock(0) {
			switch d.Val() {

			// --- Required ---

			case "nats_url":
				if !d.NextArg() {
					return d.ArgErr()
				}
				h.NatsURL = d.Val()

			case "stream_name":
				if !d.NextArg() {
					return d.ArgErr()
				}
				h.StreamName = d.Val()

			// --- Authentication (pick at most one) ---

			case "nats_credentials":
				if !d.NextArg() {
					return d.ArgErr()
				}
				h.NatsCredentials = d.Val()

			case "nats_token":
				if !d.NextArg() {
					return d.ArgErr()
				}
				h.NatsToken = d.Val()

			case "nats_user":
				if !d.NextArg() {
					return d.ArgErr()
				}
				h.NatsUser = d.Val()

			case "nats_password":
				if !d.NextArg() {
					return d.ArgErr()
				}
				h.NatsPassword = d.Val()

			// --- NATS TLS ---

			case "nats_tls_ca":
				if !d.NextArg() {
					return d.ArgErr()
				}
				h.NatsTLSCA = d.Val()

			case "nats_tls_cert":
				if !d.NextArg() {
					return d.ArgErr()
				}
				h.NatsTLSCert = d.Val()

			case "nats_tls_key":
				if !d.NextArg() {
					return d.ArgErr()
				}
				h.NatsTLSKey = d.Val()

			case "nats_tls_insecure_skip_verify":
				if d.NextArg() {
					b, err := strconv.ParseBool(d.Val())
					if err != nil {
						return d.Errf("invalid nats_tls_insecure_skip_verify: %v", err)
					}
					h.NatsTLSInsecureSkipVerify = b
				} else {
					h.NatsTLSInsecureSkipVerify = true
				}

			// --- Optional tuning ---

			case "topic_prefix":
				if !d.NextArg() {
					return d.ArgErr()
				}
				h.TopicPrefix = d.Val()

			case "allowed_origins":
				h.AllowedOrigins = d.RemainingArgs()
				if len(h.AllowedOrigins) == 0 {
					return d.ArgErr()
				}

			case "allowed_headers":
				h.AllowedHeaders = d.RemainingArgs()
				if len(h.AllowedHeaders) == 0 {
					return d.ArgErr()
				}

			case "allowed_methods":
				h.AllowedMethods = d.RemainingArgs()
				if len(h.AllowedMethods) == 0 {
					return d.ArgErr()
				}

			case "heartbeat_interval":
				v, err := parseInt("heartbeat_interval")
				if err != nil {
					return err
				}
				h.HeartbeatInterval = v

			case "reconnect_wait":
				v, err := parseInt("reconnect_wait")
				if err != nil {
					return err
				}
				h.ReconnectWait = v

			case "max_reconnects":
				v, err := parseInt("max_reconnects")
				if err != nil {
					return err
				}
				h.MaxReconnects = v
				h.maxReconnectsSet = true

			case "max_event_size":
				v, err := parseInt("max_event_size")
				if err != nil {
					return err
				}
				h.MaxEventSize = v

			case "max_connections":
				v, err := parseInt("max_connections")
				if err != nil {
					return err
				}
				if v < 0 {
					return d.Errf("max_connections must be >= 0")
				}
				h.MaxConnections = v

			case "client_buffer_size":
				v, err := parseInt("client_buffer_size")
				if err != nil {
					return err
				}
				if v <= 0 {
					return d.Errf("client_buffer_size must be > 0")
				}
				h.ClientBufferSize = v

			case "health_path":
				if !d.NextArg() {
					return d.ArgErr()
				}
				h.HealthPath = d.Val()

			case "hub_url":
				if !d.NextArg() {
					return d.ArgErr()
				}
				h.HubURL = d.Val()

			default:
				return d.Errf("unrecognized option: %s", d.Val())
			}
		}
	}
	return nil
}

// parseCaddyfile is the entry point that Caddy calls when it encounters the
// "nuts" directive in a Caddyfile.
func parseCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var handler Handler
	err := handler.UnmarshalCaddyfile(h.Dispenser)
	return &handler, err
}
