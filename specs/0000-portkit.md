# portkit plan

Oct 26, 2026 · @Alex Corrado

**Status: Implemented**, with the changes listed under Implementation notes.

portkit gives every local dev server an HTTPS URL at `<name>.localhost`. A socket-activated Caddy listens on loopback port 443 as the user, and a portkit plugin proxies each name to a fixed port or to a process it starts on demand. One custom Caddy build covers macOS (launchd) and Linux (systemd).

## Goals and non-goals

Goals:

- `https://<name>.localhost` works in browsers and CLI tools, with certificates from a locally trusted CA.
- Nothing runs until the first connection, and Caddy exits again after a long idle period.
- Caddy always runs as the user, never as root. It listens only on `127.0.0.1:443` and `[::1]:443`.
- Three ways to register a name: a fixed port, a command started on demand, or a command run now in the foreground.
- Local tools such as evalution can discover their running services without extra configuration.
- An install script short and plain enough to read before piping it into bash.

Non-goals:

- Access from other machines on the network, or public certificates.
- Windows.
- More than one portkit user per Linux machine, since only one process can own port 443.

## At a glance

&#91;embedded content: portkit architecture · request path and control path\]

Solid arrows carry requests and config changes. The dashed arrow is the supervisor starting and stopping mode 2 processes. The browser, the SDK and the CLI all reach Caddy through sockets that launchd or systemd holds, so any of them wakes it.

## macOS: launchd socket activation

A per-user LaunchAgent owns the sockets, and launchd starts Caddy on the first connection. launchd binds the sockets itself, so Caddy uses port 443 without root (the first thing to verify, see Risks).

The install script writes this to `~/Library/LaunchAgents/dev.portkit.caddy.plist`. It substitutes `$PORTKIT_HOME` (`~/Library/Application Support/portkit`), because launchd does not expand variables. The label is a placeholder until we choose a domain.

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>dev.portkit.caddy</string>
  <key>ProgramArguments</key>
  <array>
    <string>$PORTKIT_HOME/bin/caddy</string>
    <string>run</string>
    <string>--resume</string>
    <string>--config</string>
    <string>$PORTKIT_HOME/bootstrap.json</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PORTKIT_HOME</key><string>$PORTKIT_HOME</string>
    <key>XDG_DATA_HOME</key><string>$PORTKIT_HOME/data</string>
    <key>XDG_CONFIG_HOME</key><string>$PORTKIT_HOME/config</string>
  </dict>
  <key>Sockets</key>
  <dict>
    <key>https</key>
    <array>
      <dict>
        <key>SockNodeName</key><string>127.0.0.1</string>
        <key>SockServiceName</key><string>443</string>
      </dict>
      <dict>
        <key>SockNodeName</key><string>::1</string>
        <key>SockServiceName</key><string>443</string>
      </dict>
    </array>
    <key>admin</key>
    <dict>
      <key>SockPathName</key><string>$PORTKIT_HOME/admin.sock</string>
      <key>SockPathMode</key><integer>384</integer>
    </dict>
  </dict>
  <key>EnablePressuredExit</key><true/>
  <key>ProcessType</key><string>Interactive</string>
  <key>StandardOutPath</key><string>$PORTKIT_HOME/logs/caddy.log</string>
  <key>StandardErrorPath</key><string>$PORTKIT_HOME/logs/caddy.log</string>
</dict>
</plist>
```

Key choices in the plist:

- `XDG_DATA_HOME` and `XDG_CONFIG_HOME` keep this Caddy's certificates and `autosave.json` separate from any other Caddy on the machine.
- `SockPathMode` 384 is `0600`, so only the user can reach the admin API.
- `ProcessType` `Interactive` stops launchd from throttling the CPU and I/O of dev servers started under Caddy.
- There is no `RunAtLoad` or `KeepAlive`: the sockets are the only trigger.

What the plugin does on macOS:

1. **Load libSystem with purego.** `purego.Dlopen("/usr/lib/libSystem.B.dylib")`, then bind `launch_activate_socket`, `xpc_transaction_begin`, `xpc_transaction_end` and `free`. No cgo, so `CGO_ENABLED=0` builds work.
2. **Activate once.** On first use of a socket name, call `launch_activate_socket(name, &fds, &cnt)`, copy the fds, free the array and cache them for the life of the process.
3. **Survive config reloads.** Each time Caddy asks for a listener, wrap each cached fd with `net.FileListener(os.NewFile(fd))`. That duplicates the fd, so when Caddy closes an old listener on reload, launchd's socket stays open. The IPv4 and IPv6 fds are merged into one listener.
4. **Track transactions.** Begin one when a connection is accepted and end it when the connection closes. Hold one more for each running child process. With `EnablePressuredExit`, launchd kills Caddy under memory pressure only when no transaction is open.
5. **Exit when idle.** After `idle_exit` (default 2 h) with no connections and no children, Caddy exits with status 0. launchd starts it again on the next connection, and `--resume` reloads the saved config.

The install script loads the agent with `launchctl bootstrap gui/$(id -u) <plist>`, after `launchctl bootout` if an older copy is loaded. macOS shows a "Background Items Added" notification the first time.

## Linux: systemd socket activation

On Linux, the socket and service units are system units, and the service runs Caddy as the user with `User=`. A user unit (`systemctl --user`) can't bind port 443, because the user's systemd instance runs without privileges. Installing system units needs sudo once, but Caddy itself never runs as root.

Alternatives considered and rejected:

- Lowering `net.ipv4.ip_unprivileged_port_start` lets every user and program on the machine bind low ports.
- `setcap cap_net_bind_service` on the binary grants the privilege to Caddy itself and gives up socket activation.

The install script writes three units to `/etc/systemd/system/`, substituting `$USER` and `$PORTKIT_HOME` (`~/.local/share/portkit`).

`portkit-https.socket`:

```ini
[Unit]
Description=portkit HTTPS listener

[Socket]
ListenStream=127.0.0.1:443
ListenStream=[::1]:443
BindIPv6Only=ipv6-only
FileDescriptorName=https
Service=portkit.service

[Install]
WantedBy=sockets.target
```

`portkit-admin.socket`:

```ini
[Unit]
Description=portkit admin API

[Socket]
ListenStream=/run/portkit/admin.sock
SocketUser=$USER
SocketMode=0600
FileDescriptorName=admin
Service=portkit.service

[Install]
WantedBy=sockets.target
```

`portkit.service`:

```ini
[Unit]
Description=portkit (Caddy)
Requires=portkit-https.socket portkit-admin.socket
After=portkit-https.socket portkit-admin.socket

[Service]
User=$USER
Environment=PORTKIT_HOME=$PORTKIT_HOME
Environment=XDG_DATA_HOME=$PORTKIT_HOME/data
Environment=XDG_CONFIG_HOME=$PORTKIT_HOME/config
ExecStart=$PORTKIT_HOME/bin/caddy run --resume --config $PORTKIT_HOME/bootstrap.json
Sockets=portkit-https.socket portkit-admin.socket
Restart=on-failure
KillMode=control-group
```

The install script enables them with `systemctl enable --now portkit-https.socket portkit-admin.socket`.

What the plugin does on Linux:

- It reads `LISTEN_PID`, `LISTEN_FDS` and `LISTEN_FDNAMES`, and picks the fds named `https` and `admin`. It wraps and merges them the same way as on macOS.
- There are no XPC transactions. The idle exit works the same way: Caddy exits with status 0, the socket units keep holding the ports, and systemd starts Caddy again on the next connection. `Restart=on-failure` treats that exit as normal.
- `KillMode=control-group` stops child processes when Caddy stops.

Caddy has a built-in `fd/N` network for socket activation. We use our own `portkit` network instead, so the same Caddy config works on both platforms.

## Caddy configuration

The HTTP config never changes after install: one route serves portkit's own API, one proxies registered names, and a catch-all serves a help page for any other name. Services live in a `portkit` Caddy app (`apps.portkit.services`), so registering a name is a single `PUT /config/apps/portkit/services/<name>` through the admin API, with no route editing.

Persistence is on by default (`admin.config.persist`): every API change is saved to `autosave.json`. `caddy run --resume` loads that file when it exists and falls back to `bootstrap.json` on first start.

`bootstrap.json` (a sketch; the exact address syntax for a custom network is confirmed in the first spike):

```json
{
  "admin": {
    "listen": "portkit/admin",
    "config": { "persist": true }
  },
  "apps": {
    "portkit": {
      "idle_exit": "2h",
      "services": {}
    },
    "pki": {
      "certificate_authorities": {
        "local": { "name": "portkit local CA", "install_trust": false }
      }
    },
    "tls": {
      "automation": {
        "policies": [{ "issuers": [{ "module": "internal" }], "on_demand": true }],
        "on_demand": { "permission": { "module": "portkit" } }
      }
    },
    "http": {
      "servers": {
        "portkit": {
          "listen": ["portkit/https:443"],
          "protocols": ["h1", "h2"],
          "automatic_https": { "disable_redirects": true },
          "tls_connection_policies": [{}],
          "routes": [
            {
              "match": [{ "host": ["portkit.localhost"] }],
              "handle": [{ "handler": "portkit_api" }],
              "terminal": true
            },
            {
              "match": [{ "portkit": {} }],
              "handle": [{
                "handler": "reverse_proxy",
                "dynamic_upstreams": { "source": "portkit" },
                "load_balancing": { "try_duration": "60s", "try_interval": "250ms" }
              }],
              "terminal": true
            },
            {
              "handle": [{ "handler": "portkit_help" }]
            }
          ]
        }
      }
    }
  }
}
```

Caddy defaults we override:

| Setting | Why |
| --- | --- |
| `automatic_https.disable_redirects: true` | Otherwise Caddy binds port 80 for HTTP-to-HTTPS redirects, possibly on every interface. |
| `protocols: ["h1", "h2"]` | Otherwise Caddy binds UDP 443 for HTTP/3, outside the activated socket. |
| `install_trust: false` | Otherwise Caddy tries to run sudo under launchd or systemd to trust its CA. The install script runs `caddy trust` instead. |
| `on_demand` with the `portkit` permission module | Certificates are issued per name at the first TLS handshake, for any name under .localhost with up to four labels, registered or not. No route needs a host matcher, and we avoid a `*.localhost` wildcard certificate, which browsers may reject. |
| `tls_connection_policies: [{}]` | Turns on TLS even though no route names a host. |

The listen address keeps `:443` so Caddy treats the server as HTTPS. Every API change reloads the whole config. That is safe because the listeners survive reloads (see macOS) and the process supervisor keeps its state outside the app instance (see On-demand processes).

## Plugin components

The plugin is one Go module (`github.com/OWNER/portkit`, placeholder) built into Caddy with xcaddy. Only the socket activation code differs by platform, behind build tags.

| Component | Caddy registration | What it does |
| --- | --- | --- |
| Network | `caddy.RegisterNetwork("portkit", …)` | Gives Caddy the activated sockets from launchd (macOS) or systemd (Linux), duplicated on each config load. Counts open connections for idle exit and XPC transactions. |
| App | `apps.portkit` | Holds the service registry and settings such as `idle_exit`, and runs the idle-exit timer. |
| Supervisor | Package-level state, not a module | Starts, watches and stops child processes and detects their ports. Survives config reloads. |
| Upstream source | `http.reverse_proxy.upstreams.portkit` | Finds the service for the request's host, starts it if needed, waits until its port is known, and returns `localhost:<port>`. |
| Certificate permission | `tls.permission.portkit` | Allows on-demand certificates for any name under .localhost with up to four labels, registered or not, including `portkit.localhost`. |
| API handler | `http.handlers.portkit_api` | Serves the discovery endpoint at `https://portkit.localhost/services`. |
| CLI | `caddycmd.RegisterCommand` | The `caddy portkit …` subcommands that the `portkit` wrapper calls. |
| Matcher | http.matchers.portkit | Matches requests whose host is a registered service with a live process or port. |
| Help handler | http.handlers.portkit\_help | Serves the page for unregistered names, with the commands to register them. |

A service record in `apps.portkit.services`:

```jsonc
{
  "mode": "on_demand",          // "port" | "on_demand" | "foreground" | "adopted"
  "port": 0,                    // fixed port ("port"), or the owner's port ("foreground", "adopted")
  "command": ["evalution", "serve", "--port", "{port}"],
  "cwd": "/Users/me/Projects/myapp",
  "env": { "PATH": "/opt/homebrew/bin:/usr/bin:/bin" },
  "labels": { "kind": "evalution" },
  "subdomains": {               // served at <sub>.<name>.localhost
    "api":  { "assign": true },              // from {api} in the command
    "grpc": { "match": "gRPC server listening" }  // from a log-line regex
  },
  "idle_timeout": "30m",        // or "never"
  "host_header": "",            // optional, e.g. "localhost:{port}" for dev servers that check Host
  "owner": { "pid": 0, "started": "" }  // "foreground" and "adopted": the process the record lives with
}
```

The key under `services` is the name, so this record serves `myapp.evalution.localhost`. A name is one or more DNS labels joined by dots, such as foo or myapp.evalution, each matching (`[a-z0-9-]`), and `portkit` is reserved. A subdomain name is a single label (see Dotted names).

## The portkit CLI

`portkit` on the PATH is a tiny shell wrapper. All logic lives in Go as `caddy portkit …` subcommands, so mode 2 and mode 3 share one implementation of process handling and port detection.

```sh
#!/bin/sh
# portkit: run the portkit commands built into portkit's copy of Caddy.
PORTKIT_HOME="${PORTKIT_HOME:-$HOME/Library/Application Support/portkit}"
export XDG_DATA_HOME="$PORTKIT_HOME/data" XDG_CONFIG_HOME="$PORTKIT_HOME/config"
exec "$PORTKIT_HOME/bin/caddy" portkit "$@"
```

(The install script writes the Linux default, `~/.local/share/portkit`, on Linux.)

| Command | Mode | What it does |
| --- | --- | --- |
| `portkit register foo 3000` | 1: fixed port | `foo.localhost` always proxies to `localhost:3000`. |
| `portkit register foo -- npm run dev -- --port {port}` | 2: on demand | Saves the command, working directory and `PATH`. The first visit to `foo.localhost` starts it on a free port. |
| `portkit run foo -- npm run dev` | 3: foreground | Starts the command now, in the terminal, and routes `foo.localhost` to it until it exits. Nothing is saved for later. |
| `portkit adopt foo 3000` | adopted | Routes `foo.localhost` to a server that is already running, for as long as the calling process lives. Meant to be run by the server itself (see Adopting a running server). |
| `portkit ls` |  | Lists each service: mode, URL, subdomains, working directory, and whether it is running and on which port. |
| `portkit unregister foo` |  | Removes a service and stops its process. `portkit rm` is an alias. |
| `portkit stop foo` |  | Stops a running on-demand process; the next visit starts it again. |
| `portkit logs foo [-f]` |  | Shows a service's output. |
| `portkit status` |  | Shows whether Caddy is running, the admin socket path and the CA certificate path. |
| `portkit trust` |  | Adds the portkit CA to the system trust store. The install script runs it. |

Flags for `register`, `run` and `adopt`: `--label key=value` and `--sub NAME=SPEC` (both repeatable; see Subdomains). `register` and `run` also take `--idle-timeout 30m|never`, `--host-header localhost:{port}`, `--env KEY=VALUE`, `--cwd DIR` and `--listen-port N`.

The CLI talks to Caddy over the admin socket, which starts Caddy if it is asleep. Config changes use the standard `/config/apps/portkit/…` endpoints. Runtime state (running, port, process ID) and `stop` use an `admin.api.portkit` admin module.

How `portkit run` works:

1. Pick a free port by listening on `127.0.0.1:0`, reading the port and closing the listener.
2. Start the command in a pseudo-terminal (`creack/pty`), with `PORT` set and `{port}` replaced. The pty keeps colors and interactive features such as Vite's keyboard shortcuts working. Input, output, window resizes and signals pass through.
3. Detect the port the process actually listens on (see Port detection), then `PUT` a `foreground` service record with that port and `owner`.
4. When the command exits, or `portkit run` gets a signal, `DELETE` the record and exit with the command's status.
5. If `portkit run` is killed with SIGKILL, the record stays behind. The upstream source checks that `owner` is alive and drops the record if not, and the next `portkit run foo` overwrites it anyway.

## Adopting a running server

`portkit adopt` registers a server that something else started, and the record lasts as long as the process that ran the command. It is meant to be run by the server itself at startup, so tools like evalution can name and label themselves:

```sh
portkit adopt myapp.evalution 4100 --label kind=evalution
```

How it works:

1. **Record the owner.** By default the owner is the parent process, which is the server that ran `adopt`; `--pid N` names another, which must be a running process owned by the same user, or adopt fails. The record stores the process ID and the process's start time, so a reused ID doesn't count as the same process.
2. **Write the record and exit.** `adopt` creates or updates the record (rules below), prints the URL in use and exits. `--json` prints the final record instead. Nothing keeps running, so there's no child to clean up, no stray Ctrl-C to survive, and nothing holding Node's event loop open.
3. **Remove it when the owner exits.** While Caddy runs, it watches owners with kqueue on macOS and `pidfd` on Linux. It also checks owners when it starts and on every lookup. `portkit run` records use the same mechanism, with the `portkit run` process as the owner.

When a record already exists:

| Situation | Result |
| --- | --- |
| A service already routes to this port, in any mode | Labels and subdomains are merged into that record, which keeps its name and owner. `adopt` prints that service's URL. |
| The name is free | A new `adopted` record, owned by the caller. |
| The name belongs to an on-demand service that isn't running | The adopter takes the name while it lives, so a visit doesn't start a second copy. The on-demand record applies again when the adopter exits. |
| The name belongs to a running service on another port | `adopt` fails with a non-zero exit and a message naming the conflict. |

Labels merged into a registered service (mode 1 or 2) stay after its process stops, so discovery can still find a stopped on-demand service and wake it. Running `portkit register` for that name again resets its labels.

`adopt` takes `--label` and `--sub NAME=PORT`. It can't use placeholder or regex subdomains, because portkit never starts the process or sees its output.

Processes that portkit starts get `PORTKIT_HOME` in their environment, so they can run `$PORTKIT_HOME/bin/caddy portkit adopt` even when `portkit` isn't on their `PATH`.

## Unregistered names

Visiting a name that isn't registered shows a page with the commands to register it, filled in with that name. For this to work, every `<label>.localhost` name gets a certificate, registered or not. Without one, the browser would show a TLS error before any page could load.

What `https://foo.localhost` shows when `foo` isn't registered (as HTML in browsers, and as plain text when the request doesn't accept HTML, as with curl):

```text
foo.localhost isn't registered with portkit.
Run one of these from your project's directory:

Proxy to a server already running on a fixed port:
  portkit register foo 3000

Start a command whenever foo.localhost is visited:
  portkit register foo -- npm run dev -- --port {port}

Run a command now, in your terminal, until it exits:
  portkit run foo -- npm run dev

Did you mean fooo.localhost?
Registered: api.localhost, myapp.evalution.localhost, web.localhost
```

Details:

- **Status and caching.** The page returns 404 with `Cache-Control: no-store`, so it never sticks after the name is registered.
- **Refreshes itself.** The HTML page checks its own URL every 2 s and reloads once the response is no longer the help page (marked by an `X-Portkit: unregistered` header). Register the name in a terminal and the browser tab switches to the app on its own.
- **Close matches.** "Did you mean" lists registered names within two edits of the requested one. The registered names are listed as links. Working directories and commands are not shown.
- **Exited foreground runs.** A `foreground` record whose `portkit run` process has exited gets the same page, plus a line saying `foo` was last run with `portkit run`, which has exited. The stale record is removed.
- **Nested names.** `a.b.localhost` gets the page too. If b is a registered service, the page lists its subdomains. If services are registered under a.b, the page lists those, so an unregistered parent such as evalution.localhost works as an index of its instances.
- **Certificate cost.** Each visited name gets a certificate from the local CA, with the internal issuer's short default lifetime. Certificates for names nobody visits again expire and are removed by Caddy's storage cleanup. The permission module allows at most 10 new unregistered names a minute, so a runaway script can't fill the disk.

Routing: the `portkit` matcher sends registered names to `reverse_proxy`, and the last route sends everything else to the help handler (see `bootstrap.json` under Caddy configuration).

## On-demand processes

The supervisor starts a mode 2 service on its first request and stops it after `idle_timeout` (default 30 min) without traffic. A request to a stopped service goes like this:

1. The upstream source looks up the service by the request's host.
2. The supervisor picks a free port and starts the command in its own process group, in the saved working directory and environment.
3. The request waits, for up to 60 s, until the port is detected. Other requests that arrive meanwhile wait on the same start.
4. The upstream source returns the address the process listens on. `reverse_proxy` retries the dial for its `try_duration` if the server is still starting.

Lifecycle rules:

- **Idle stop.** Each request and each open connection (including WebSocket hot-reload connections) resets the idle timer. When it expires, the supervisor sends SIGTERM to the process group, then SIGKILL after 10 s. `--idle-timeout never` keeps a service running until Caddy exits.
- **Crashes.** If the process exits, the next request starts it again. If it exits within 5 s of starting three times in a row, requests get a 502 page with the last 50 lines of its log instead of another restart.
- **Config reloads.** Supervisor state is package-level and keyed by name, so it outlives the app instance that Caddy rebuilds on every reload. After a reload, removed services are stopped, services whose command, directory or environment changed are restarted on their next request, and the rest keep running.
- **Caddy exit.** On macOS, launchd kills the job's process group when Caddy exits. On Linux, `KillMode=control-group` does the same. Caddy only exits for idleness when no child is running.
- **Transactions (macOS).** Each running child holds an XPC transaction, so memory-pressure exits never kill a working dev server.

Environment:

- launchd and systemd give Caddy a minimal `PATH`, so commands like `npm` would not be found. `portkit register` saves the caller's `PATH` and working directory with the service.
- The rest of the caller's environment is not saved, so secrets stay out of `autosave.json`. `--env KEY=VALUE` adds specific variables.
- The command is run directly, not through a shell. Shell syntax needs an explicit `sh -c '…'`.
- portkit adds `PORT`, `PORTKIT_SERVICE`, `PORTKIT_URL` and `PORTKIT_HOME`. It also sets `NODE_EXTRA_CA_CERTS` to the portkit CA certificate when it isn't already set, so Node apps started by portkit trust `*.localhost` certificates.

Logs: stdout and stderr go to `$PORTKIT_HOME/logs/<name>.log`, rotated at 10 MB with one old file kept. Caddy's own log records each start, stop and detected port.

On macOS, processes started by Caddy count as Caddy for privacy permissions. A project in `~/Documents`, `~/Desktop` or `~/Downloads` will trigger permission prompts that name `caddy`, or fail to read its files. `portkit register` warns when the working directory is in one of these folders.

## Port detection

portkit trusts what the OS says the process is listening on, and uses log lines to choose between candidates. Each process gets an assigned port through `PORT` and `{port}`, but many tools ignore `PORT` or move to another port when theirs is taken.

Detection runs every 250 ms from start until it settles, in this order:

1. **Assigned port.** If the process group listens on the assigned port, use it and ignore the logs.
2. **Listening sockets.** Collect the TCP ports in LISTEN state owned by the process group. On macOS that is `lsof -a -iTCP -sTCP:LISTEN -g <pgid> -P -n -F n`. On Linux, match the socket inodes under `/proc/<pid>/fd` against `/proc/net/tcp` and `/proc/net/tcp6`. A process group covers chains like `npm → sh → node`.
3. **Log match.** A port named in a log line that is also a listening socket wins. The most recent such line wins.
4. **Single socket.** With no log match and exactly one listening port, use it.
5. **Logged port outside the group.** If a log line names a port that accepts connections but the group doesn't own it, use it. This covers tools like `docker compose`, whose ports belong to another process.
6. **Guess.** With several sockets and no log match, drop known side ports (9229 for the Node debugger) and use the lowest remaining port, with a warning.

Log lines are matched after stripping ANSI escapes. Vite, for example, prints the port in bold, which splits it from the URL. The patterns, in priority order:

```text
ANSI escapes   \x1b\[[0-9;?]*[ -/]*[@-~]   and   \x1b\][^\x07]*(\x07|\x1b\\)
URL            \bhttps?://(localhost|127\.0\.0\.1|0\.0\.0\.0|\[::1?\]|[a-z0-9.-]+):(\d{2,5})\b
"listening"    (?i)\blisten(ing)?\b.*?(port\s*|:)(\d{2,5})\b
bare port      (?i)\bport\s*[:=]?\s*(\d{2,5})\b
```

Lines containing `in use` or `EADDRINUSE` are skipped, so Vite's "Port 5173 is in use, trying another one…" isn't read as the answer.

How some common servers are expected to resolve:

| Server | Uses `PORT`? | Resolved by |
| --- | --- | --- |
| Next.js (`next dev`) | Yes | Step 1 |
| Vite | No; use `--port {port}` | Step 1 with the placeholder; otherwise steps 2–3 from "Local: http://localhost:5173/" |
| Django (`runserver`) | No | Steps 2–3 from "Starting development server at http://127.0.0.1:8000/" |
| A server plus `node --inspect` | Varies | Step 3, or step 6 drops 9229 |

Once a port is chosen, portkit writes one line saying what it picked and why. In mode 3 it goes to the terminal (stderr); in mode 2 it goes to the service log:

```text
portkit: foo.localhost → [::1]:5174 (from log: "Local: http://localhost:5174/")
portkit: foo.localhost → 127.0.0.1:3000 (assigned port)
portkit: foo.localhost → 127.0.0.1:5174 (guessed lowest of 5174, 24678; use {port} or --listen-port to fix)
```

The upstream dials the exact address found (`[::1]` or `127.0.0.1`), which avoids the IPv4/IPv6 mismatch with Node servers that bind only `::1`. `--listen-port N` skips detection for a process whose port is known. If dials start failing after a restart, detection runs again.

## Subdomains

A service can serve extra ports at `<sub>.<name>.localhost`, for example `api.foo.localhost` beside `foo.localhost`. Each one is declared by a placeholder in the command or with `--sub NAME=SPEC`.

| Form | Example | Works with | Where the port comes from |
| --- | --- | --- | --- |
| Placeholder | `portkit register foo -- app --port {port} --api-port {api}` | `register`, `run` | portkit picks a free port for each placeholder, as it does for `{port}`. |
| Fixed port | `--sub api=3001` | `register`, `run`, `adopt` | The given port. |
| Log regex | `--sub 'api=/GraphQL server ready/'` | `register`, `run` | The first output line matching the regex. The port is the regex's capture group if it has one, otherwise the port found on that line by the usual patterns. |

Rules:

- **Placeholders.** Any `{name}` token in the command, where the name is a valid DNS label, declares a subdomain; `{port}` is always the main port. `{{` and `}}` write literal braces.
- **Environment.** Each subdomain's port is also set as `PORTKIT_PORT_<NAME>`, for example `PORTKIT_PORT_API`, beside `PORT`.
- **Detection.** Subdomain ports are confirmed as listening sockets, like the main port. A regex match on a port outside the process group is accepted only if it takes connections (step 5 of Port detection). Subdomain ports are resolved first, and the main-port steps skip them.
- **adopt.** Only fixed ports work, because portkit never sees an adopter's output.
- **On-demand start.** Visiting `api.foo.localhost` starts `foo` like a visit to `foo.localhost` does, and waits for the `api` port.
- **Idle.** Traffic to any subdomain counts as activity for the service.
- **Logs.** portkit writes one line per subdomain, for example `portkit: api.foo.localhost → 127.0.0.1:4001 (from log: "GraphQL server ready at :4001")`.
- **Discovery.** `/services` includes each subdomain's URL.

## Dotted names

Service names may contain dots, so `portkit register myapp.evalution 4100` serves `myapp.evalution.localhost` with no `evalution` service. This groups instances of one tool under a shared parent name.

- **Lookup.** For a request to `<host>.localhost`, an exact service name wins. Otherwise portkit drops the first label and looks for a service with that subdomain: `api.myapp.evalution.localhost` is the `api` subdomain of `myapp.evalution`.
- **One owner per host.** A host belongs to exactly one service or subdomain. Registering `myapp.evalution` fails if a service named `evalution` has a `myapp` subdomain, and declaring that subdomain fails if `myapp.evalution` exists. `adopt` reports the same conflict as an error.
- **Parents need not exist.** `evalution.localhost` doesn't have to be registered. When it isn't, its help page lists the services under it, which works as an index of evalution instances.
- **Depth.** Names under `.localhost` can have up to four labels, subdomain included. The certificate permission module refuses deeper names.
- **Matching by directory.** The `cwd` filter in discovery is unchanged: the SDK still picks its instance by working directory and label, not by name (see next section).

## Service discovery

The evalution SDK finds its server with one request to portkit, matched by label and working directory. It never contacts the services themselves: probing each service's own endpoint would start every on-demand service.

**Registering.** The evalution server labels itself. At startup, if portkit is installed, it runs `portkit adopt` with a name taken from its directory, and ignores any failure (see Adopting a running server):

```sh
portkit adopt myapp.evalution 4100 --label kind=evalution
```

Users never have to remember `--label`. Whether evalution was started by hand, with `portkit run`, or on demand through `portkit register`, its record ends up labelled.

**Endpoint.** `GET https://portkit.localhost/services` lists every service. Two optional filters:

- `label=kind=evalution` (repeatable): only services with all the given labels.
- `cwd=/path/to/dir`: only services whose working directory is that directory or one of its parents, deepest first. That handles monorepos and apps started from a subdirectory.

```json
{
  "services": [
    {
      "name": "myapp.evalution",
      "url": "https://myapp.evalution.localhost",
      "mode": "on_demand",
      "cwd": "/Users/me/Projects/myapp",
      "labels": { "kind": "evalution" },
      "running": false
    }
  ]
}
```

Commands and environments are left out of the response.

**What the SDK does:**

1. Look for the portkit CA certificate at `$PORTKIT_HOME/data/caddy/pki/authorities/local/root.crt`, using the platform default when `PORTKIT_HOME` is unset. No certificate means portkit isn't installed: stop, with no network call.
2. Request `/services?label=kind=evalution&cwd=<its working directory>`. It connects to `127.0.0.1:443` with `portkit.localhost` as the TLS server name, so it doesn't depend on the system resolving `*.localhost`. It trusts the portkit CA for this connection only, and allows about 1 s, since Caddy may be starting.
3. Use the first result's `url`. With no result, fall back to the SDK's usual default.

Sending traces to that URL starts the evalution server if it isn't running. The same CA certificate is used for those requests.

**Exposure.** The endpoint is read-only and sends no CORS headers, so websites open in the browser can't read it. Any process on the machine can reach loopback port 443, so other local users could see service names and directory paths. That's acceptable for a development tool.

Alternatives considered:

| Approach | Why not |
| --- | --- |
| Query each service's own config endpoint | Starts every on-demand service. |
| Derive the name from the directory, e.g. a hash | Breaks when the directory is renamed or two apps share a name. |
| A JSON registry file in `PORTKIT_HOME` | Avoids TLS, but is a second copy of state to keep in sync with `autosave.json`. Worth adding later if an SDK can't do TLS easily. |

## Install script

`install.sh` is written to be read before it is run. It asks no questions while running, so it behaves the same when piped into bash. Options are flags: `bash -s -- --dry-run`.

Readability rules:

- A header comment lists every file it writes and every command it runs with sudo.
- `main` sits at the top and calls one function per step, in order. Each function opens with a one-line comment saying what it does and why.
- The whole script is a function body called on the last line, so a cut-off download never runs half a script.
- Every action is printed before it runs. `--dry-run` prints them without running anything.
- The Caddy version and release URL are pinned at the top. Downloads are checked against `SHA256SUMS`.
- Running it again upgrades in place. `--uninstall` reverses every step.

The steps:

1. **Detect the platform** from `uname -s` and `uname -m`: macOS or Linux, arm64 or amd64. Anything else stops with a message.
2. **Download Caddy with the portkit plugin** (`caddy-portkit_<version>_<os>_<arch>.tar.gz`) and `SHA256SUMS` from the GitHub release, and check the checksum.
3. **Install files:** the binary, `bootstrap.json` and the `portkit` wrapper.
4. **Set up socket activation.** macOS: write the plist, `launchctl bootout` any old copy, then `launchctl bootstrap gui/$(id -u)`. Linux: print the three units, write them with sudo, then `systemctl daemon-reload` and `systemctl enable --now` both sockets.
5. **Trust the CA** with `portkit trust`, which runs `caddy trust` against the admin socket. That starts Caddy, which creates the CA, then adds it to the system trust store. macOS asks for the user's password once; Linux uses sudo. If `certutil` is installed, Firefox's store is updated too; if not, the script says how.
6. **Check it works** with `curl -fsS https://portkit.localhost/services`.
7. **Check the PATH.** If `~/.local/bin` isn't on it, print the line to add to the shell profile.

An example step, to show the intended style:

```bash
# Download Caddy (built with the portkit plugin) and check it against the published checksum.
download_caddy() {
  say "Downloading $TARBALL"
  run curl -fsSL -o "$TMP/$TARBALL"   "$RELEASE_URL/$TARBALL"
  run curl -fsSL -o "$TMP/SHA256SUMS" "$RELEASE_URL/SHA256SUMS"
  run check_sha256 "$TMP" "$TARBALL"
}
```

What it writes:

| Item | macOS | Linux |
| --- | --- | --- |
| `PORTKIT_HOME` (binary, config, CA, logs) | `~/Library/Application Support/portkit` | `~/.local/share/portkit` |
| `portkit` wrapper | `~/.local/bin/portkit` | `~/.local/bin/portkit` |
| Service definition | `~/Library/LaunchAgents/dev.portkit.caddy.plist` | `/etc/systemd/system/portkit.service`, `portkit-https.socket`, `portkit-admin.socket` (sudo) |
| Admin socket | `$PORTKIT_HOME/admin.sock` | `/run/portkit/admin.sock` |
| CA trust | System keychain (password prompt) | System CA store (sudo) |

## Build and release

One Linux CI job builds all four binaries with `CGO_ENABLED=0`, because purego needs no cgo on macOS:

```sh
xcaddy build v2.X.Y --with github.com/OWNER/portkit@vA.B.C   # per GOOS/GOARCH
```

- **Targets:** darwin/arm64, darwin/amd64, linux/arm64 and linux/amd64. The Caddy version is pinned and upgraded deliberately.
- **Signing:** the Go linker ad-hoc signs darwin/arm64 binaries, which is all Apple Silicon needs to run them. Files downloaded with `curl` aren't quarantined, so Gatekeeper doesn't check for notarization. Notarization only matters if we later ship through a browser download or a Homebrew cask.
- **Release assets:** one tarball per target plus `SHA256SUMS`, with GitHub artifact attestations so a careful user can run `gh attestation verify`.
- **Optional:** registering the plugin on caddyserver.com would also let people get a build from Caddy's download page.

Tests:

- Unit tests for port detection, run against captured startup output from real servers (Vite, Next.js, Django, Rails, a Node server with `--inspect`), ANSI codes included.
- Integration tests on Linux in a VM or container with systemd: install, first request, reload, idle exit, uninstall.
- On macOS, the same flow run by hand at first; later on a CI runner if it supports per-user launchd agents.

## Risks to test first

The first assumption below is the one the whole design rests on, so it is tested before anything else is built.

| Assumption | How to test | Fallback |
| --- | --- | --- |
| A LaunchAgent's `Sockets` can bind `127.0.0.1:443` and `[::1]:443` for a non-root user. | A minimal plist plus a small Go program that accepts a connection and replies. | A LaunchDaemon in `/Library/LaunchDaemons` with `UserName` set: root launchd binds the port and Caddy still runs as the user. Install needs sudo. |
| `launch_activate_socket` works through purego. | Call it from the same spike, twice, and record what the second call returns. | Build the macOS binaries with cgo on a macOS CI runner. |
| Caddy accepts `portkit/https:443` and `portkit/admin` as listen addresses, treats the server as HTTPS, and serves the admin API on it. | Run `bootstrap.json` with the plugin. | Set TLS explicitly. For admin, use Caddy's `unix/` network and have the CLI start Caddy with `launchctl kickstart` first. |
| On-demand TLS with the internal issuer and our permission module issues certificates quickly. | Request a new name and time the first handshake. | Add a host-matched route per service, so certificates are issued when a service is registered. |
| Safari and the macOS resolver resolve `*.localhost` to loopback. | Open `https://foo.localhost` in Safari; run `dscacheutil -q host -a name foo.localhost`. | A resolver file at `/etc/resolver/localhost` pointing to a tiny DNS responder in the plugin (sudo once at install). |
| Child processes survive config reloads and die with Caddy. | Register a second service while the first is running, then `launchctl kill SIGKILL` Caddy. | Fix in the supervisor; no design change. |
| XPC transactions protect a non-XPC process from pressured exit. | Hold a connection open and run `memory_pressure -l critical`. | Drop `EnablePressuredExit` and rely on the idle exit alone. |

On Linux, browsers resolve `*.localhost` themselves, but command-line tools need `nss-myhostname` or `systemd-resolved`. The docs will say so.

## Milestones

Each milestone is usable on its own, and each depends on the one before.

1. **macOS socket activation spike.** The purego bindings, the `portkit` network, the plist, binding 443 as the user, XPC transactions, surviving reloads and idle exit. Answers the first three risks.
2. **Linux socket activation.** The `LISTEN_FDS` network and the three systemd units, tested in a VM.
3. **Fixed-port services (mode 1).** `bootstrap.json`, the `portkit` app, the upstream source, the certificate permission module, and `portkit register <name> <port>`, `ls`, `unregister` and `trust`.
4. **On-demand services (mode 2).** The supervisor, port detection, logs, idle stop, crash handling, subdomains and `portkit stop` and `logs`.
5. **Foreground services (mode 3).** `portkit run` with a pseudo-terminal, and portkit adopt with owner tracking.
6. **Discovery.** The `/services` endpoint, the `PORTKIT_*` and `NODE_EXTRA_CA_CERTS` variables, and the evalution SDK integration.
7. **Distribution.** `install.sh` and `--uninstall`, the release pipeline, checksums and attestations.

## Implementation notes

The plan above is implemented, with these changes:

- **`portkit_start` handler before `reverse_proxy`.** If `GetUpstreams` returns an error, `reverse_proxy` retries for its whole `try_duration` (60 s) and then returns a bare 503. So this handler starts the process and waits for its port, and serves the 502 page with the log tail itself. The upstream source still looks up the service and returns the address it detected.
- **Fixed-port services that aren't listening** get a 502 page that reloads on its own, instead of hanging for `try_duration`.
- **The `portkit` wrapper exports only `PORTKIT_HOME`**, not `XDG_*`, so processes started with `portkit run` don't inherit portkit's data directories.
- **Record fields added:** `listen_port`, and `replaces`, which holds the record an adopter (or `run --replace`) took the name from, so it can be restored.
- **`grace_period: 5s`**, so an open WebSocket can't hold up a SIGTERM shutdown forever.
- **A crashed on-demand service stays stopped** until `portkit stop` or until its record changes.
- **Upgrades:** `caddy portkit upgrade-config` (hidden) carries the services from `autosave.json` over to a new `bootstrap.json` while Caddy is stopped, so upgrades can change the rest of the config.

## Open questions

- [ ] Which domain and GitHub owner do we use? They set the launchd label (`dev.portkit.caddy` for now) and the Go module path.
- [ ] Is the name `portkit` free on GitHub, Homebrew and npm?
- [ ] Does evalution keep traces only in memory? If so, its services should default to `--idle-timeout never`, or evalution should save to disk on SIGTERM.
- [ ] Should evalution ship its own registration command, such as `evalution dev`?
- [ ] When `portkit run foo` finds `foo` already registered in another mode, should it refuse (the current plan, unless `--replace` is passed) or take over and restore the old record on exit?
- [ ] Are the default timeouts right: 30 min for an idle child, 2 h for Caddy itself?
