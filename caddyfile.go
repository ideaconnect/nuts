package nuts

import (
	"fmt"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

// UnmarshalCaddyfile implements caddyfile.Unmarshaler.
func (h *Handler) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	for d.Next() {
		for d.NextBlock(0) {
			// Each directive maps directly onto a handler field or tunable.
			switch d.Val() {
			case "nats_url":
				if !d.NextArg() {
					return d.ArgErr()
				}
				h.NatsURL = d.Val()

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

			case "heartbeat_interval":
				if !d.NextArg() {
					return d.ArgErr()
				}
				var interval int
				if _, err := fmt.Sscanf(d.Val(), "%d", &interval); err != nil {
					return d.Errf("invalid heartbeat_interval: %v", err)
				}
				h.HeartbeatInterval = interval

			case "reconnect_wait":
				if !d.NextArg() {
					return d.ArgErr()
				}
				var wait int
				if _, err := fmt.Sscanf(d.Val(), "%d", &wait); err != nil {
					return d.Errf("invalid reconnect_wait: %v", err)
				}
				h.ReconnectWait = wait

			case "max_reconnects":
				if !d.NextArg() {
					return d.ArgErr()
				}
				var max int
				if _, err := fmt.Sscanf(d.Val(), "%d", &max); err != nil {
					return d.Errf("invalid max_reconnects: %v", err)
				}
				h.MaxReconnects = max

			case "stream_name":
				if !d.NextArg() {
					return d.ArgErr()
				}
				h.StreamName = d.Val()

			case "max_event_size":
				if !d.NextArg() {
					return d.ArgErr()
				}
				var size int
				if _, err := fmt.Sscanf(d.Val(), "%d", &size); err != nil {
					return d.Errf("invalid max_event_size: %v", err)
				}
				h.MaxEventSize = size

			default:
				return d.Errf("unrecognized option: %s", d.Val())
			}
		}
	}
	return nil
}

// parseCaddyfile parses the Caddyfile directive.
func parseCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var handler Handler
	err := handler.UnmarshalCaddyfile(h.Dispenser)
	return &handler, err
}
