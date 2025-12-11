// Package main provides a custom Caddy build with the nuts module.
// Use this to build a Caddy binary with NATS-to-SSE support.
package main

import (
	caddycmd "github.com/caddyserver/caddy/v2/cmd"

	// plug in Caddy modules here
	_ "github.com/caddyserver/caddy/v2/modules/standard"
	_ "github.com/idct/nuts"
)

func main() {
	caddycmd.Main()
}
