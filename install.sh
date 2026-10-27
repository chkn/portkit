#!/usr/bin/env bash
# install.sh: install portkit, which gives every local dev server an HTTPS
# URL at https://<name>.localhost.
#
# Please read this before running it. It asks no questions, so it behaves the
# same when piped into bash. Options are flags:
#
#   curl -fsSL https://github.com/chkn/portkit/releases/latest/download/install.sh | bash
#   ... | bash -s -- --dry-run      print every action without running it
#   ... | bash -s -- --uninstall    reverse every step
#   ./install.sh --binary ./caddy   install a local build instead of downloading
#
# Running it again upgrades in place and keeps your registered services.
#
# Files it writes:
#   macOS  ~/Library/Application Support/portkit/   PORTKIT_HOME: bin/caddy, bootstrap.json,
#                                                    and later the CA, saved config and logs
#          ~/Library/LaunchAgents/dev.portkit.caddy.plist
#   Linux  ~/.local/share/portkit/                   PORTKIT_HOME, as above
#          /etc/systemd/system/portkit.service       (with sudo)
#          /etc/systemd/system/portkit-https.socket  (with sudo)
#          /etc/systemd/system/portkit-admin.socket  (with sudo)
#   Both   ~/.local/bin/portkit                      a three-line wrapper
#
# Commands it runs with sudo:
#   Linux  tee (to write the three units), systemctl daemon-reload,
#          systemctl stop portkit.service, systemctl enable --now <sockets>,
#          systemctl restart <sockets>
#   Both   `portkit trust` adds the portkit CA to the system trust store. On
#          macOS that asks for your password; on Linux it uses sudo.
#
# Caddy itself never runs as root. launchd or systemd holds port 443 on
# 127.0.0.1 and ::1, and starts Caddy as you on the first connection.
#
# The whole script is one function, called on the last line, so a download
# cut off halfway runs nothing.

portkit_install() {
set -euo pipefail

VERSION="0.0.1"                 # portkit release
CADDY_VERSION="2.11.7"          # the Caddy it is built with
RELEASE_URL="https://github.com/chkn/portkit/releases/download/v$VERSION"
LABEL="dev.portkit.caddy"       # launchd label

DRY_RUN=0
UNINSTALL=0
LOCAL_BINARY=""

main() {
  parse_flags "$@"
  detect_platform
  if [ "$UNINSTALL" = 1 ]; then
    uninstall
    return
  fi
  say "Installing portkit $VERSION for $OS/$ARCH into $PORTKIT_HOME"
  make_tmp
  download_caddy
  install_files
  setup_socket_activation
  trust_ca
  check_it_works
  check_path
  say ""
  say "portkit is installed. Try:  portkit register foo 3000  and open https://foo.localhost"
}

# Read --dry-run, --uninstall and --binary.
parse_flags() {
  while [ $# -gt 0 ]; do
    case "$1" in
      --dry-run) DRY_RUN=1 ;;
      --uninstall) UNINSTALL=1 ;;
      --binary) LOCAL_BINARY="$2"; shift ;;
      -h|--help) sed -n '2,36p' "$0" 2>/dev/null || true; exit 0 ;;
      *) die "unknown option: $1" ;;
    esac
    shift
  done
}

# Find the OS and CPU, and where each file goes. Anything but macOS or Linux on
# arm64 or amd64 stops here.
detect_platform() {
  case "$(uname -s)" in
    Darwin) OS=darwin; PORTKIT_HOME="$HOME/Library/Application Support/portkit" ;;
    Linux) OS=linux; PORTKIT_HOME="$HOME/.local/share/portkit" ;;
    *) die "portkit supports macOS and Linux, not $(uname -s)" ;;
  esac
  case "$(uname -m)" in
    arm64|aarch64) ARCH=arm64 ;;
    x86_64|amd64) ARCH=amd64 ;;
    *) die "portkit supports arm64 and amd64, not $(uname -m)" ;;
  esac
  if [ "$OS" = linux ] && ! command -v systemctl >/dev/null; then
    die "portkit on Linux needs systemd"
  fi
  TARBALL="caddy-portkit_${VERSION}_${OS}_${ARCH}.tar.gz"
  BIN_DIR="$HOME/.local/bin"
  PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
  UNIT_DIR="/etc/systemd/system"
  CA_CERT="$PORTKIT_HOME/data/caddy/pki/authorities/local/root.crt"
}

# Make a temporary directory, removed when the script exits.
make_tmp() {
  TMP="$(mktemp -d)"
  trap 'rm -rf "$TMP"' EXIT
}

# Download Caddy (built with the portkit plugin) and check it against the published checksum.
download_caddy() {
  if [ -n "$LOCAL_BINARY" ]; then
    say "Using the local build $LOCAL_BINARY"
    run cp "$LOCAL_BINARY" "$TMP/caddy"
    return
  fi
  say "Downloading $TARBALL"
  run curl -fsSL -o "$TMP/$TARBALL"   "$RELEASE_URL/$TARBALL"
  run curl -fsSL -o "$TMP/SHA256SUMS" "$RELEASE_URL/SHA256SUMS"
  run check_sha256 "$TMP" "$TARBALL"
  run tar -xzf "$TMP/$TARBALL" -C "$TMP" caddy
  if [ "$DRY_RUN" = 0 ] && ! "$TMP/caddy" version | grep -q "v$CADDY_VERSION"; then
    die "the downloaded binary isn't Caddy $CADDY_VERSION"
  fi
}

# Install the binary, bootstrap.json and the portkit wrapper.
install_files() {
  run mkdir -p "$PORTKIT_HOME/bin" "$PORTKIT_HOME/logs" "$BIN_DIR"
  # Rename into place, so a running Caddy keeps its old binary until it restarts.
  run install -m 0755 "$TMP/caddy" "$PORTKIT_HOME/bin/caddy.new"
  run mv -f "$PORTKIT_HOME/bin/caddy.new" "$PORTKIT_HOME/bin/caddy"

  # The Caddy config. It never changes after install; services are added
  # through the admin API and saved in config/caddy/autosave.json.
  write_file "$PORTKIT_HOME/bootstrap.json" <<'JSON'
{
  "admin": {
    "listen": "portkit/admin",
    "origins": ["127.0.0.1"],
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
      "grace_period": "5s",
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
              "handle": [
                { "handler": "portkit_start" },
                {
                  "handler": "reverse_proxy",
                  "dynamic_upstreams": { "source": "portkit" },
                  "load_balancing": { "try_duration": "60s", "try_interval": "250ms" }
                }
              ],
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
JSON

  write_file "$BIN_DIR/portkit" <<SH
#!/bin/sh
# portkit: run the portkit commands built into portkit's copy of Caddy.
PORTKIT_HOME="\${PORTKIT_HOME:-$PORTKIT_HOME}"
export PORTKIT_HOME
exec "\$PORTKIT_HOME/bin/caddy" portkit "\$@"
SH
  run chmod 0755 "$BIN_DIR/portkit"
}

# Hand port 443 and the admin socket to launchd or systemd, which start Caddy
# on the first connection. An upgrade stops Caddy first and carries the
# registered services over to the new bootstrap.json.
setup_socket_activation() {
  if [ "$OS" = darwin ]; then
    setup_launchd
  else
    setup_systemd
  fi
}

# macOS: a per-user LaunchAgent whose Sockets launchd binds itself.
setup_launchd() {
  run mkdir -p "$(dirname "$PLIST")"
  write_file "$PLIST" <<XML
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>$LABEL</string>
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
XML
  local domain="gui/$(id -u)"
  if launchctl print "$domain/$LABEL" >/dev/null 2>&1; then
    run launchctl bootout "$domain/$LABEL"
    wait_for_bootout "$domain/$LABEL"
  fi
  run env PORTKIT_HOME="$PORTKIT_HOME" "$PORTKIT_HOME/bin/caddy" portkit upgrade-config
  run launchctl bootstrap "$domain" "$PLIST"
  say "macOS may show a \"Background Items Added\" notification for caddy."
}

# Linux: system units, because a user unit can't bind port 443. The service
# runs Caddy as you with User=.
setup_systemd() {
  say "Writing systemd units to $UNIT_DIR (needs sudo)"
  write_file_sudo "$UNIT_DIR/portkit-https.socket" <<UNIT
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
UNIT
  write_file_sudo "$UNIT_DIR/portkit-admin.socket" <<UNIT
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
UNIT
  write_file_sudo "$UNIT_DIR/portkit.service" <<UNIT
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
UNIT
  run sudo systemctl daemon-reload
  run sudo systemctl stop portkit.service
  run env PORTKIT_HOME="$PORTKIT_HOME" "$PORTKIT_HOME/bin/caddy" portkit upgrade-config
  run sudo systemctl enable --now portkit-https.socket portkit-admin.socket
  run sudo systemctl restart portkit-https.socket portkit-admin.socket
}

# Trust the portkit CA. `portkit trust` asks the admin API for the CA, which
# starts Caddy and creates the CA, then adds it to the system trust store.
trust_ca() {
  run "$BIN_DIR/portkit" trust
}

# Check that https://portkit.localhost answers, through port 443 and the CA.
check_it_works() {
  say "Checking https://portkit.localhost/services"
  run curl -fsS --resolve portkit.localhost:443:127.0.0.1 --cacert "$CA_CERT" \
    -o /dev/null https://portkit.localhost/services
  if [ "$OS" = linux ] && [ "$DRY_RUN" = 0 ] && ! getent hosts portkit.localhost >/dev/null; then
    say "Note: this system doesn't resolve *.localhost for command-line tools. Browsers do."
    say "      Enable nss-myhostname or systemd-resolved to fix it for curl and others."
  fi
}

# Make sure the portkit wrapper is on the PATH.
check_path() {
  case ":$PATH:" in
    *":$BIN_DIR:"*) ;;
    *)
      say ""
      say "$BIN_DIR isn't on your PATH. Add this line to your shell profile:"
      say "  export PATH=\"\$HOME/.local/bin:\$PATH\""
      ;;
  esac
}

# Reverse every step: stop Caddy, remove the service definition, untrust the
# CA, and delete PORTKIT_HOME (with every registered service) and the wrapper.
uninstall() {
  say "Uninstalling portkit from $PORTKIT_HOME"
  if [ "$OS" = darwin ]; then
    local domain="gui/$(id -u)"
    if launchctl print "$domain/$LABEL" >/dev/null 2>&1; then
      run launchctl bootout "$domain/$LABEL"
      wait_for_bootout "$domain/$LABEL"
    fi
    run rm -f "$PLIST"
  else
    run sudo systemctl disable --now portkit-https.socket portkit-admin.socket || true
    run sudo systemctl stop portkit.service || true
    run sudo rm -f "$UNIT_DIR/portkit.service" "$UNIT_DIR/portkit-https.socket" "$UNIT_DIR/portkit-admin.socket"
    run sudo systemctl daemon-reload
  fi
  if [ -f "$CA_CERT" ]; then
    run "$PORTKIT_HOME/bin/caddy" untrust --cert "$CA_CERT"
  fi
  run rm -f "$BIN_DIR/portkit"
  run rm -rf "$PORTKIT_HOME"
  say "portkit is uninstalled."
}

# wait_for_bootout SERVICE: launchctl bootout returns before the job is gone.
wait_for_bootout() {
  local i
  for i in 1 2 3 4 5 6 7 8 9 10; do
    if [ "$DRY_RUN" = 1 ] || ! launchctl print "$1" >/dev/null 2>&1; then
      return
    fi
    sleep 0.5
  done
}

# check_sha256 DIR FILE: check DIR/FILE against DIR/SHA256SUMS.
check_sha256() {
  local line
  line="$(grep " $2\$" "$1/SHA256SUMS")" || die "$2 isn't listed in SHA256SUMS"
  if command -v sha256sum >/dev/null; then
    (cd "$1" && echo "$line" | sha256sum -c --quiet -)
  else
    (cd "$1" && echo "$line" | shasum -a 256 -c --quiet -)
  fi
}

# say MESSAGE: print a line.
say() {
  printf '%s\n' "$*"
}

# run COMMAND...: print a command, then run it unless this is a dry run.
run() {
  printf '+ %s\n' "$*"
  if [ "$DRY_RUN" = 0 ]; then
    "$@"
  fi
}

# write_file PATH: write stdin to PATH, unless this is a dry run.
write_file() {
  printf '+ write %s\n' "$1"
  if [ "$DRY_RUN" = 0 ]; then
    cat > "$1"
  else
    cat > /dev/null
  fi
}

# write_file_sudo PATH: write stdin to a root-owned PATH.
write_file_sudo() {
  printf '+ sudo write %s\n' "$1"
  if [ "$DRY_RUN" = 0 ]; then
    sudo tee "$1" > /dev/null
  else
    cat > /dev/null
  fi
}

die() {
  printf 'install.sh: %s\n' "$*" >&2
  exit 1
}

main "$@"
}

portkit_install "$@"
