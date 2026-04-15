// Package main builds a custom Caddy binary that includes the NUTS module.
//
// Caddy is designed to be extended by importing modules as Go packages.
// This file pulls in the standard Caddy modules (file server, reverse
// proxy, TLS, etc.) plus the nuts module, then hands control to Caddy's
// CLI entrypoint. The resulting binary behaves exactly like stock Caddy
// but also understands the "nuts" Caddyfile directive.
//
// Build:
//
//	cd cmd/caddy && go build -o caddy .
package main

import (
	caddycmd "github.com/caddyserver/caddy/v2/cmd"

	// Blank imports ("_") are used because we don't call anything from
	// these packages directly — we only need their init() functions to
	// run, which register the modules with Caddy's global registry.

	_ "github.com/caddyserver/caddy/v2/modules/standard" // file_server, reverse_proxy, tls, etc.
	_ "github.com/ideaconnect/nuts"                      // the NUTS NATS-to-SSE handler
)

// main starts the Caddy process. caddycmd.Main() parses CLI flags,
// loads the config, provisions all registered modules (including ours),
// and begins serving.
func main() {
	caddycmd.Main()
}
