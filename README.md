# Portkit

A [Caddy](https://caddyserver.com) plugin to manage your local dev servers and give
them each an HTTPS URL at `https://<name>.localhost`.

Portkit configures launchd (Mac) or systemd (Linux) to start Caddy whenever there's
activity on loopback port 443, and then proxies each name to either a fixed port,
or to a process it starts on demand.

```sh
portkit register web 3000            # a server you run manually on port 3000
portkit register app -- npm run dev  # run automatically when you visit app.localhost
portkit run api -- npm run dev       # run in this terminal session 
portkit adopt myapp 4100             # a server registering itself
portkit ls
```

## Install

Portkit runs Caddy as your local user. It only needs root privileges during
installation to create the systemd units on Linux, and to trust its CA for
serving HTTPS on localhost:

```sh
curl -fsSL https://github.com/chkn/portkit/releases/latest/download/install.sh | bash
```

[install.sh](install.sh) lists every file it writes and every command it runs with sudo.
Running it again upgrades in place, keeping your registered services. Command line options:
- `--dry-run` prints the actions without running them.
- `--uninstall` reverses everything.
- `--binary ./caddy` installs a local build.

## Usage

After running the install script, the following commands will be on your PATH:

| Command | What it does |
| --- | --- |
| `portkit register NAME PORT` | Always proxy `NAME.localhost` to `localhost:PORT`. |
| `portkit register NAME -- CMD…` | Start `CMD` on a free port whenever you visit `NAME.localhost`, stop it after 30 min idle. |
| `portkit run NAME -- CMD…` | Run `CMD` now, in the foreground on this terminal, routing `NAME.localhost` to it until it exits. |
| `portkit adopt NAME PORT` | Route `NAME.localhost` to a running server for as long as the calling process lives. |
| `portkit ls` | List services with their state and upstream address. |
| `portkit unregister NAME` | Remove a service and stop its process. |
| `portkit rm NAME` | Alias for `unregister` |
| `portkit stop NAME` | Stop an on-demand process, or clear its crash state. |
| `portkit logs NAME [-f] [-n N]` | Show an on-demand service's output. |
| `portkit status` | Caddy's process, the admin socket and the CA certificate. |
| `portkit trust` | Add the portkit CA to the system trust store (the installer runs it). |

Flags for `register`, `run` and `adopt`:
- `--label key=value` - Adds an arbitrary label to the service record that can be queried.
- `--sub NAME=PORT` (or `NAME=/regex/` for `register` and `run`) - Serve a subdomain `NAME.<name>.localhost`.

Flags for `register` and `run`:
- `--idle-timeout 30m|never`
- `--host-header localhost:{port}`
- `--env K=V`,
- `--cwd DIR`
- `--listen-port N`

`run` also takes `--replace` to borrow a name that is registered in another mode; the old record comes back when the command exits.

### Port assignment

When Portkit runs a server via the `register` or `run` command, it uses the following methods to determine which port to proxy to:

1. **`--listen-port` argument** - If you pass a port to the `register` or `run` command, it will use that port.
2. **`PORT` environment variable** - Portkit sets this to a free port when it starts the process. If it, or any process in its child process group, listens on this port, Portkit will proxy to this port.
3. **Interpolated command line arguments** - In any of the `CMD` arguments above, you can pass `{port}`, which will be replaced with a free port the server should listen on. Any interpolation other than `{port}` will be replaced with a free port for that subdomain (e.g. `{foo}` will be replaced with a free port and `foo.<name>.localhost` will proxy to it.
4. **Stdout and stderr** - Portkit scans the process's output for a port number. It only picks one that some process in the process group is listening on.
5. **Random port** - Portkit gets the list of open ports for the process and its child process group and picks one at random, ignoring some common sidechannel ports (e.g. Node debugger).

### Service Discovery

In addition to the `portkit ls` command, you can also discover registered services through a GET request to `https://portkit.localhost/services`. Query parameters filter the returned list:

- `label` - Returns services with the given label. Can be specified more than once.
- `cwd` - Returns services with the given working directory or one of its parents.

## Development

```sh
go test -race ./...
go build -o caddy ./cmd/caddy      # or: xcaddy build v2.11.7 --with github.com/chkn/portkit=.
```

To run Caddy by hand without launchd or systemd, set `PORTKIT_SOCKETS` to have
the plugin bind the sockets itself, and point the CLI at the admin socket:

```sh
export PORTKIT_HOME=$PWD/dev XDG_DATA_HOME=$PWD/dev/data XDG_CONFIG_HOME=$PWD/dev/config
export PORTKIT_SOCKETS="https=127.0.0.1:8443,https=[::1]:8443,admin=unix:$PWD/dev/admin.sock"
export PORTKIT_ADMIN_SOCKET=$PWD/dev/admin.sock
./caddy run --resume --config bootstrap.json
./caddy portkit register web 3000
curl -k --connect-to web.localhost:8443:127.0.0.1:8443 https://web.localhost:8443/
```

Socket paths are limited to 104 bytes on macOS, so keep `dev/` short.

### Releases

Pushing a `v*` tag runs [release.yml](.github/workflows/release.yml), which
builds all four targets with `CGO_ENABLED=0` on one Linux runner, then publishes the
tarballs, `install.sh` and `SHA256SUMS` with build attestations. Bump `VERSION`
in `install.sh` to match the tag first.

## License

Portkit is licensed under the [Apache License 2.0](LICENSE), the same license as Caddy.
