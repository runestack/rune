#!/usr/bin/env bash
set -euo pipefail

# Rune Complete Server Installer
# - Installs Docker (Ubuntu/Debian/Amazon Linux)
# - Creates rune user and directories
# - Installs rune/runed (from release or source)
# - Installs and enables systemd unit
# - Complete server environment setup

RUNE_USER="rune"
RUNE_GROUP="rune"
DATA_DIR="/var/lib/rune"
GRPC_PORT=7863
HTTP_PORT=7861
RUNE_VERSION=""
FROM_SOURCE=false
BRANCH="master"
SKIP_DOCKER=false
EDGE=false
ACME_EMAIL=""
CLUSTER_CIDR="10.96.0.0/16"

log() { echo "[install-server] $*"; }
die() { echo "[install-server] ERROR: $*" >&2; exit 1; }

usage() {
  cat <<USAGE
Usage: $0 [--version vX.Y.Z | --from-source] [--branch NAME] [--grpc-port N] [--http-port N] [--skip-docker]
           [--edge] [--acme-email EMAIL] [--cluster-cidr CIDR]
Options:
  --version vX.Y.Z      Install from GitHub release tag (preferred)
  --from-source         Build from source if no release is specified
  --branch NAME         Git branch to clone when building from source (default: master)
  --grpc-port N         gRPC port (default: 7863)
  --http-port N         HTTP port (default: 7861)
  --skip-docker         Skip Docker installation (assume already installed)
  --edge                Configure as edge node: sets node.role=edge in runefile.toml
                        and grants cap_net_bind_service so runed can bind :80/:443
  --acme-email EMAIL    ACME / Let's Encrypt contact email (writes [acme].email)
  --cluster-cidr CIDR   Cluster CIDR for the networking layer (default: 10.96.0.0/16)
  -h, --help            Show help

This installer sets up the complete Rune server environment including Docker.
For CLI-only installation, use: curl -fsSL https://rune.sh/install-cli.sh | bash

Example (edge node with ACME):
  curl -fsSL https://rune.sh/install-server.sh | bash -s -- \
    --version v0.0.1-dev.18 --edge --acme-email you@example.com
USAGE
}

arch_normalize() {
  local m; m=$(uname -m)
  case "$m" in
    x86_64) echo amd64 ;;
    aarch64|arm64) echo arm64 ;;
    *) die "Unsupported architecture: $m" ;;
  esac
}

ensure_root() {
  if [ "${EUID:-$(id -u)}" -ne 0 ]; then
    die "Please run as root (use sudo)"
  fi
}

detect_os() {
  . /etc/os-release || true
  case "${ID:-}" in
    ubuntu) echo ubuntu ;;
    debian) echo debian ;;
    amzn|al2023) echo amazon ;;
    rhel|centos) echo rhel ;;
    *) log "Unknown distro (${ID:-}); attempting Ubuntu-like path"; echo ubuntu ;;
  esac
}

install_docker() {
  if [ "$SKIP_DOCKER" = true ]; then
    log "Skipping Docker installation (--skip-docker flag used)"
    return
  fi

  if command -v docker >/dev/null 2>&1; then
    log "Docker already installed"
    return
  fi

  local os; os=$(detect_os)
  log "Installing Docker on $os"
  
  if [ "$os" = "amazon" ] || [ "$os" = "rhel" ]; then
    if command -v dnf >/dev/null 2>&1; then
      dnf update -y || true
      dnf install -y docker git make gcc openssl || dnf groupinstall -y "Development Tools"
    else
      yum update -y || true
      yum install -y docker git make gcc openssl || yum groupinstall -y "Development Tools"
    fi
    systemctl enable --now docker
    
    # Add common cloud users to docker group
    for user in ec2-user ubuntu admin; do
      if id -u "$user" >/dev/null 2>&1; then
        usermod -aG docker "$user" || true
      fi
    done
  else
    # Ubuntu/Debian
    apt-get update -y
    apt-get install -y ca-certificates curl gnupg lsb-release git build-essential make openssl
    
    # Import Docker's official GPG key
    if [ ! -f /usr/share/keyrings/docker.gpg ]; then
      curl -fsSL https://download.docker.com/linux/$os/gpg | gpg --dearmor -o /usr/share/keyrings/docker.gpg
    fi
    
    # Determine codename per distro
    if [ "$os" = "ubuntu" ]; then
      CODENAME=$(. /etc/os-release && echo "${UBUNTU_CODENAME:-jammy}")
      DIST="ubuntu"
    else
      CODENAME=$(. /etc/os-release && echo "$VERSION_CODENAME")
      DIST="debian"
    fi
    
    echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/docker.gpg] https://download.docker.com/linux/$DIST $CODENAME stable" > /etc/apt/sources.list.d/docker.list
    apt-get update -y
    apt-get install -y docker-ce docker-ce-cli containerd.io
    systemctl enable --now docker
    
    # Add common cloud users to docker group
    for user in ubuntu admin debian; do
      if id -u "$user" >/dev/null 2>&1; then
        usermod -aG docker "$user" || true
      fi
    done
  fi
  
  log "Docker installation completed"
}

ensure_user() {
  if ! id -u "$RUNE_USER" >/dev/null 2>&1; then
    useradd --system --home "$DATA_DIR" --shell /usr/sbin/nologin "$RUNE_USER"
  fi
  
  mkdir -p "$DATA_DIR"
  chown -R "$RUNE_USER":"$RUNE_GROUP" "$DATA_DIR" || chown -R "$RUNE_USER":"$RUNE_USER" "$DATA_DIR" || true
  
  # Add rune user to docker group if Docker is installed
  if command -v docker >/dev/null 2>&1 && getent group docker >/dev/null 2>&1; then
    usermod -aG docker "$RUNE_USER" || true
  fi
}

install_from_release() {
  local arch url tmp
  arch=$(arch_normalize)
  tmp=$(mktemp -d)
  url="https://github.com/runestack/rune/releases/download/${RUNE_VERSION}/rune_linux_${arch}.tar.gz"
  log "Downloading $url"
  
  if ! curl -fsSL -o "$tmp/rune.tgz" "$url"; then
    die "Failed to download release from $url"
  fi
  
  tar -C /usr/local/bin -xzf "$tmp/rune.tgz" rune runed
  rm -rf "$tmp"
  log "Installed Rune binaries from release ${RUNE_VERSION}"
}

install_from_source() {
  if ! command -v go >/dev/null 2>&1; then
    install_go
  fi
  
  # Ensure module cache env in non-interactive root shells
  if [ -z "${HOME:-}" ]; then
    export HOME=/root
  fi
  export GOPATH="${GOPATH:-$HOME/go}"
  export GOMODCACHE="${GOMODCACHE:-$GOPATH/pkg/mod}"
  go env -w GOPATH="$GOPATH" >/dev/null 2>&1 || true
  go env -w GOMODCACHE="$GOMODCACHE" >/dev/null 2>&1 || true
  
  log "Building Rune from source (branch: $BRANCH)"
  local src=/opt/rune
  rm -rf "$src" && git clone --branch "$BRANCH" --single-branch https://github.com/runestack/rune.git "$src"
  (cd "$src" && make build)
  install -m 0755 "$src/bin/rune" /usr/local/bin/rune
  install -m 0755 "$src/bin/runed" /usr/local/bin/runed
  rm -rf "$src"
  log "Built and installed Rune binaries from source"
}

install_go() {
  local version="1.22.5"
  local arch tmp
  
  log "Installing Go ${version}"
  
  arch=$(arch_normalize)
  tmp=$(mktemp -d)
  
  # Download Go
  local url="https://go.dev/dl/go${version}.linux-${arch}.tar.gz"
  log "Downloading Go ${version} from $url"
  
  if ! curl -fsSL -o "$tmp/go.tgz" "$url"; then
    # Fallback to amd64 if specific arch fails
    url="https://go.dev/dl/go${version}.linux-amd64.tar.gz"
    log "Falling back to amd64: $url"
    if ! curl -fsSL -o "$tmp/go.tgz" "$url"; then
      die "Failed to download Go ${version}"
    fi
  fi
  
  # Extract to /usr/local
  rm -rf /usr/local/go && tar -C /usr/local -xzf "$tmp/go.tgz"
  
  # Add Go to PATH for current session
  export PATH="/usr/local/go/bin:$PATH"
  
  # Add Go to PATH permanently for server installations
  echo 'export PATH=/usr/local/go/bin:$PATH' >> /etc/profile
  echo 'export PATH=/usr/local/go/bin:$PATH' >> /etc/bash.bashrc
  
  # Verify installation
  if ! /usr/local/go/bin/go version >/dev/null 2>&1; then
    die "Go installation failed"
  fi
  
  log "Go ${version} installed successfully"
  rm -rf "$tmp"
}

setup_cli_access() {
  log "Setting up CLI access for primary users..."
  
  # Detect a primary non-system user (prefer common cloud users, then first UID >= 1000)
  TARGET_USER=""
  for candidate in admin ubuntu ec2-user debian; do
    if id -u "$candidate" >/dev/null 2>&1; then
      TARGET_USER="$candidate"
      break
    fi
  done
  
  if [ -z "$TARGET_USER" ]; then
    TARGET_USER=$(awk -F: '$3>=1000 && $1!="nobody" {print $1; exit}' /etc/passwd || true)
  fi
  
  if [ -n "$TARGET_USER" ]; then
    TARGET_HOME=$(getent passwd "$TARGET_USER" | cut -d: -f6)
    CLI_CONFIG_PATH="${DATA_DIR}/.rune/config.yaml"
    
    if [ -f "$CLI_CONFIG_PATH" ]; then
      mkdir -p "$TARGET_HOME/.rune"
      cp "$CLI_CONFIG_PATH" "$TARGET_HOME/.rune/config.yaml"
      chown -R "$TARGET_USER":"$TARGET_USER" "$TARGET_HOME/.rune"
      chmod 700 "$TARGET_HOME/.rune"
      chmod 600 "$TARGET_HOME/.rune/config.yaml"
      log "✅ Copied CLI config to $TARGET_HOME/.rune/config.yaml for user $TARGET_USER"
      log "   User can now run: rune status"
    else
      log "⚠️  CLI config not found at $CLI_CONFIG_PATH; user will need to run 'rune login' manually"
    fi
  else
    log "⚠️  No primary user detected; CLI config will need to be set up manually"
  fi
}

generate_runefile() {
  # Skip when no runefile-affecting flag was supplied; let runed create
  # its own default runefile.yaml on first start.
  if [ "$EDGE" = false ] && [ -z "$ACME_EMAIL" ] && [ "$CLUSTER_CIDR" = "10.96.0.0/16" ]; then
    return
  fi

  # Operator config lives in /etc/rune/ (system-wide). The data
  # directory is for state (KEK, store, secrets) and should not also
  # contain config. runed's auto-discovery checks both locations,
  # but writing to /etc/rune/ is the canonical placement and matches
  # what the systemd unit's --config flag points at.
  local etc_dir="/etc/rune"
  local runefile="$etc_dir/runefile.toml"
  for dir in "$etc_dir" "$DATA_DIR"; do
    for ext in toml yaml yml; do
      if [ -f "$dir/runefile.$ext" ]; then
        log "Existing runefile found at $dir/runefile.$ext; leaving untouched"
        return
      fi
    done
  done

  local role="worker"
  [ "$EDGE" = true ] && role="edge"

  mkdir -p "$etc_dir" "$DATA_DIR"
  cat > "$runefile" <<EOF
data_dir = "$DATA_DIR"

[server]
grpc_address = ":$GRPC_PORT"
http_address = ":$HTTP_PORT"

[log]
level = "info"
format = "text"

[networking]
cluster_cidr = "$CLUSTER_CIDR"

[telemetry]
metrics_addr = "127.0.0.1:9100"

[node]
role = "$role"

[acme]
email = "${ACME_EMAIL}"
EOF
  chown "$RUNE_USER":"$RUNE_GROUP" "$runefile" 2>/dev/null || true
  chmod 0640 "$runefile"
  log "Wrote $runefile (role=$role, cluster_cidr=$CLUSTER_CIDR${ACME_EMAIL:+, acme_email=$ACME_EMAIL})"
}

install_systemd() {
  local unit=/etc/systemd/system/runed.service
  if [ -f "$unit" ]; then
    log "Systemd unit exists: $unit"
  else
    log "Installing systemd unit: $unit"
    cat >"$unit" <<UNIT
[Unit]
Description=Rune Server
After=network-online.target docker.service
Wants=network-online.target

[Service]
Type=simple
User=${RUNE_USER}
Group=${RUNE_GROUP}
# Grants access to /var/run/docker.sock without putting docker as
# the primary group of the rune user. Required for the Docker runner.
SupplementaryGroups=docker
# Ensure non-interactive stdin under systemd
StandardInput=null
# --config makes the resolved runefile path explicit and survives
# anyone running 'systemctl status runed' / 'cat /etc/systemd/system/runed.service'
# trying to figure out where config came from. If the file is absent
# runed falls through to its built-in defaults; the auto-discovery
# search order is unchanged.
ExecStart=/usr/local/bin/runed --config /etc/rune/runefile.toml
Restart=on-failure
RestartSec=5
LimitNOFILE=65536
# Capabilities the agent needs while running as the rune user:
#   - CAP_NET_BIND_SERVICE — bind :80 / :443 for the ingress.
#   - CAP_SYS_ADMIN        — mount(2) for cloud block-device drivers
#     (do-volume + future) which attach a device and mount it under
#     /var/lib/rune/mounts/. Without it, /bin/mount fails with
#     "must be superuser to use mount" and every cloud volume gets
#     stuck post-Attach.
#   - CAP_NET_ADMIN        — netlink AddrAdd to host each service's
#     cluster VIP as a /32 on loopback (dataplane). Without it VIP
#     listeners cannot bind and service-to-service traffic has no path.
AmbientCapabilities=CAP_NET_BIND_SERVICE CAP_SYS_ADMIN CAP_CHOWN CAP_FOWNER CAP_NET_ADMIN
CapabilityBoundingSet=CAP_NET_BIND_SERVICE CAP_SYS_ADMIN CAP_CHOWN CAP_FOWNER CAP_NET_ADMIN

[Install]
WantedBy=multi-user.target
UNIT
  fi

  # SupplementaryGroups=docker is baked into the canonical unit
  # above; no drop-in override is needed.


  install_upgrade_units
  systemctl daemon-reload
  systemctl enable --now runed
  log "Systemd service installed and enabled"
}

# RUNE-321: in-band upgrade units + version floor, so upgrades after this
# one can be `rune upgrade` instead of SSH. Rendered by the installed
# binary — no second template to drift. Skipped on builds that predate it.
install_upgrade_units() {
  local bin=/usr/local/bin/runed staging="$DATA_DIR/upgrade"
  if ! "$bin" print-systemd --upgrade-units --staging "$staging" </dev/null >/dev/null 2>&1; then
    return
  fi
  "$bin" print-systemd --upgrade-units --staging "$staging" --binary "$bin" > /etc/systemd/system/runed-upgrade.service
  "$bin" print-systemd --upgrade-path-unit --staging "$staging" > /etc/systemd/system/runed-upgrade.path
  systemctl daemon-reload
  systemctl enable --now runed-upgrade.path 2>/dev/null || true
  if [ ! -f /etc/rune/version-floor ] && [ -n "${RUNE_VERSION:-}" ]; then
    mkdir -p /etc/rune
    printf '%s\n' "$RUNE_VERSION" > /etc/rune/version-floor
    log "Seeded version floor at /etc/rune/version-floor ($RUNE_VERSION)"
  fi
  log "Installed in-band upgrade units (rune upgrade)"
}

verify_installation() {
  log "Verifying installation..."
  
  # Check binaries
  if command -v rune >/dev/null 2>&1 && command -v runed >/dev/null 2>&1; then
    log "✅ Rune binaries installed successfully"
    rune version
    runed --version
  else
    die "❌ Rune binaries not found in PATH"
  fi
  
  # Check service
  if systemctl is-active --quiet runed; then
    log "✅ Rune service is running"
  else
    log "⚠️  Rune service is not running"
    systemctl status runed --no-pager || true
  fi
  
  # Check Docker
  if command -v docker >/dev/null 2>&1; then
    log "✅ Docker is installed"
    docker --version
  else
    log "⚠️  Docker is not installed"
  fi
  
  # Check ports
  if netstat -tlnp 2>/dev/null | grep -q ":${GRPC_PORT}"; then
    log "✅ gRPC port ${GRPC_PORT} is listening"
  else
    log "⚠️  gRPC port ${GRPC_PORT} is not listening"
  fi
  
  if netstat -tlnp 2>/dev/null | grep -q ":${HTTP_PORT}"; then
    log "✅ HTTP port ${HTTP_PORT} is listening"
  else
    log "⚠️  HTTP port ${HTTP_PORT} is not listening"
  fi
}

main() {
  while [ $# -gt 0 ]; do
    case "$1" in
      --version) RUNE_VERSION="$2"; shift 2 ;;
      --from-source) FROM_SOURCE=true; shift ;;
      --branch) BRANCH="$2"; shift 2 ;;
      --grpc-port) GRPC_PORT="$2"; shift 2 ;;
      --http-port) HTTP_PORT="$2"; shift 2 ;;
      --skip-docker) SKIP_DOCKER=true; shift ;;
      --edge) EDGE=true; shift ;;
      --acme-email) ACME_EMAIL="$2"; shift 2 ;;
      --cluster-cidr) CLUSTER_CIDR="$2"; shift 2 ;;
      -h|--help) usage; exit 0 ;;
      *) die "Unknown argument: $1" ;;
    esac
  done

  log "Starting Rune Complete Server Installation..."
  ensure_root
  install_docker
  ensure_user

  if [ -n "$RUNE_VERSION" ]; then
    install_from_release || { log "Release install failed; falling back to source"; install_from_source; }
  elif [ "$FROM_SOURCE" = true ]; then
    install_from_source
  else
    log "No version specified; installing from source"
    install_from_source
  fi

  # Edge mode: grant low-port binding capability so runed can listen on :80/:443
  # without running as root.
  if [ "$EDGE" = true ] && [ -x /usr/local/bin/runed ]; then
    if command -v setcap >/dev/null 2>&1; then
      setcap 'cap_net_bind_service=+ep' /usr/local/bin/runed
      log "Granted cap_net_bind_service to /usr/local/bin/runed (edge mode)"
    else
      log "⚠️  setcap not found; ingress binds on :80/:443 may fail. Install libcap2-bin (Debian/Ubuntu) or libcap (RHEL)."
    fi
  fi

  # Generate runefile.toml when edge / acme / non-default cidr is requested.
  generate_runefile

  # Server will create runefile.yaml and KEK in the data dir on first start
  install_systemd
  
  # Wait a moment for service to start
  sleep 3
  verify_installation
  
  # Set up CLI access for primary users
  setup_cli_access

  log "🎉 Rune Complete Server Installation completed successfully!"
  log "Check service status with: systemctl status runed --no-pager"
  log "View logs with: journalctl -u runed -f"
  log "Test endpoints:"
  log "  - gRPC: localhost:${GRPC_PORT}"
  log "  - HTTP: localhost:${HTTP_PORT}"
}

main "$@"
