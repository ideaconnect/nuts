// caddyfile.go — Caddyfile directive parser.
//
// This file teaches Caddy how to read a "nuts" block in a Caddyfile and
// populate a Handler struct from its directives. A typical block looks
// like this:
//
//	nuts {
//	    nats_url            nats://localhost:4222
//	    stream_name         EVENTS
//	    topic_prefix        events.
//	    allowed_origins     https://example.com https://staging.example.com
//	    heartbeat_interval  30
//	    reconnect_wait      2
//	    max_reconnects      -1
//	    max_event_size      1048576
//
//	    # Authentication (pick one):
//	    # nats_credentials  /path/to/file.creds
//	    # nats_token        s3cret
//	    # nats_user         admin
//	    # nats_password     hunter2
//	}
package nuts

import (
	"fmt"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

// UnmarshalCaddyfile implements caddyfile.Unmarshaler. Caddy calls this
// method to populate a Handler from the tokens parsed out of the Caddyfile.
//
// The outer d.Next() loop advances past the directive name ("nuts").
// The inner d.NextBlock(0) loop iterates over each line inside the braces.
// Each line’s first token is the directive key; the remaining tokens are
// its arguments.
func (h *Handler) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	for d.Next() {
		for d.NextBlock(0) {
			// Match the directive key and read its argument(s).
			// d.Val() returns the current token (the key).
			// d.NextArg()/d.RemainingArgs() advance to read argument tokens.
			// d.ArgErr() returns a standard "missing argument" error.
			// d.Errf() returns a formatted parse error.
			switch d.Val() {

			// --- Required ---

			case "nats_url": // e.g. nats://localhost:4222
				if !d.NextArg() {
					return d.ArgErr()
				}
				h.NatsURL = d.Val()

			// --- Authentication (pick at most one) ---

			case "nats_credentials": // path to a .creds file
				if !d.NextArg() {
					return d.ArgErr()
				}
				h.NatsCredentials = d.Val()

			case "nats_token": // shared-secret token
				if !d.NextArg() {
					return d.ArgErr()
				}
				h.NatsToken = d.Val()

			case "nats_user": // basic auth username (pair with nats_password)
				if !d.NextArg() {
					return d.ArgErr()
				}
				h.NatsUser = d.Val()

			case "nats_password": // basic auth password (pair with nats_user)
				if !d.NextArg() {
					return d.ArgErr()
				}
				h.NatsPassword = d.Val()

			// --- Optional tuning ---

			case "topic_prefix": // string prepended to every NATS subject
				if !d.NextArg() {
					return d.ArgErr()
				}
				h.TopicPrefix = d.Val()

			case "allowed_origins": // one or more CORS origins; use * for any
				h.AllowedOrigins = d.RemainingArgs()
				if len(h.AllowedOrigins) == 0 {
					return d.ArgErr()
				}

			case "heartbeat_interval": // keep-alive interval in seconds (int)
				if !d.NextArg() {
					return d.ArgErr()
				}
				var interval int
				// Sscanf is used instead of strconv.Atoi so we get a
				// consistent error style for all integer directives.
				if _, err := fmt.Sscanf(d.Val(), "%d", &interval); err != nil {
					return d.Errf("invalid heartbeat_interval: %v", err)
				}
				h.HeartbeatInterval = interval

			case "reconnect_wait": // NATS reconnect delay in seconds (int)
				if !d.NextArg() {
					return d.ArgErr()
				}
				var wait int
				if _, err := fmt.Sscanf(d.Val(), "%d", &wait); err != nil {
					return d.Errf("invalid reconnect_wait: %v", err)
				}
				h.ReconnectWait = wait

			case "max_reconnects": // max NATS reconnect attempts; -1 = unlimited (int)
				if !d.NextArg() {
					return d.ArgErr()
				}
				var max int
				if _, err := fmt.Sscanf(d.Val(), "%d", &max); err != nil {
					return d.Errf("invalid max_reconnects: %v", err)
				}
				h.MaxReconnects = max

			case "stream_name": // JetStream stream name (e.g. "EVENTS")
				if !d.NextArg() {
					return d.ArgErr()
				}
				h.StreamName = d.Val()

			case "max_event_size": // max SSE event frame size in bytes (int)
				if !d.NextArg() {
					return d.ArgErr()
				}
				var size int
				if _, err := fmt.Sscanf(d.Val(), "%d", &size); err != nil {
					return d.Errf("invalid max_event_size: %v", err)
				}
				h.MaxEventSize = size
			case "hub_url": // URL for Link header hub discovery
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
// "nuts" directive in a Caddyfile. It creates a fresh Handler, populates it
// via UnmarshalCaddyfile, and returns it as a MiddlewareHandler for Caddy
// to install in the HTTP handler chain.
func parseCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var handler Handler
	err := handler.UnmarshalCaddyfile(h.Dispenser)
	return &handler, err
}
