// Command caddy is Caddy with the portkit plugin, for local builds. Releases
// are built with xcaddy, which generates the same program.
package main

import (
	caddycmd "github.com/caddyserver/caddy/v2/cmd"

	_ "github.com/caddyserver/caddy/v2/modules/standard"

	_ "github.com/chkn/portkit"
)

func main() {
	caddycmd.Main()
}
