// Package portkit is a Caddy plugin that gives every local dev server an
// HTTPS URL at <name>.localhost.
//
// Caddy runs socket-activated as the user: launchd (macOS) or systemd
// (Linux) holds the loopback port 443 and the admin socket, and the
// "portkit" network hands those sockets to Caddy. Services live in the
// "portkit" app (apps.portkit.services); each one proxies to a fixed port,
// to a process the supervisor starts on demand, or to a process that
// `portkit run` or `portkit adopt` registered.
package portkit

import (
	"os"
	"path/filepath"
	"runtime"

	"github.com/caddyserver/caddy/v2"
)

func init() {
	caddy.RegisterModule(App{})
	caddy.RegisterModule(Matcher{})
	caddy.RegisterModule(StartHandler{})
	caddy.RegisterModule(Upstreams{})
	caddy.RegisterModule(Permission{})
	caddy.RegisterModule(APIHandler{})
	caddy.RegisterModule(HelpHandler{})
	caddy.RegisterModule(AdminAPI{})
	caddy.RegisterNetwork("portkit", getListener)
	registerCommands()
}

// Home is PORTKIT_HOME: the directory holding the binary, config, CA and logs.
func Home() string {
	if h := os.Getenv("PORTKIT_HOME"); h != "" {
		return h
	}
	home, _ := os.UserHomeDir()
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "portkit")
	}
	return filepath.Join(home, ".local", "share", "portkit")
}

// AdminSocket is the path of the admin API socket that launchd or systemd holds.
func AdminSocket() string {
	if s := os.Getenv("PORTKIT_ADMIN_SOCKET"); s != "" {
		return s
	}
	if runtime.GOOS == "linux" {
		return "/run/portkit/admin.sock"
	}
	return filepath.Join(Home(), "admin.sock")
}

// CACertPath is the root certificate of the portkit local CA. Caddy runs
// with XDG_DATA_HOME=$PORTKIT_HOME/data, so its storage is under there.
func CACertPath() string {
	return filepath.Join(Home(), "data", "caddy", "pki", "authorities", "local", "root.crt")
}

// LogDir holds one log file per on-demand service.
func LogDir() string {
	return filepath.Join(Home(), "logs")
}
