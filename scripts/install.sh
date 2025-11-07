#!/usr/bin/env bash
set -euo pipefail

# Rune binary-only server installer for Ubuntu or Amazon Linux
# - Installs Docker (if missing)
# - Creates rune user and directories
# - Installs rune/runed (from release or source)
# - Installs and enables systemd unit

RUNE_USER="rune"
RUNE_GROUP="rune"
DATA_DIR="/var/lib/rune"
GRPC_PORT=7863
HTTP_PORT=7861
RUNE_VERSION=""
FROM_SOURCE=false
BRANCH="master"

log() { echo "[install] $*"; }
die() { echo "[install] ERROR: $*" >&2; exit 1; }

usage() {
  cat <<USAGE
Usage: $0 [--version vX.Y.Z | --from-source] [--branch NAME] [--grpc-port N] [--http-port N]
Options:
  --version vX.Y.Z   Install from GitHub release tag (preferred)
  --from-source      Build from source if no release is specified
  --branch NAME      Git branch to clone when building from source (default: master)
  --grpc-port N      gRPC port (default: 7863)
  --http-port N      HTTP port (default: 7861)
  -h, --help         Show help

This installer sets up the full Rune server stack (API, Agent, CLI).
For CLI-only installation, use: curl -fsSL https://rune.sh/install-cli.sh | bash
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
    amzn|al2023) echo amazon ;;
    *) log "Unknown distro (${ID:-}); attempting Ubuntu-like path"; echo ubuntu ;;
  esac
}

install_docker() {
  local os; os=$(detect_os)
  if command -v docker >/dev/null 2>&1; then
    log "Docker already installed"
    return
  fi
  log "Installing Docker on $os"
  if [ "$os" = "amazon" ]; then
    if command -v dnf >/dev/null 2>&1; then
      dnf update -y || true
      dnf install -y docker
    else
      yum update -y || true
      yum install -y docker
    fi
    systemctl enable --now docker
  else
    apt-get update -y
    apt-get install -y ca-certificates curl gnupg lsb-release
    curl -fsSL https://download.docker.com/linux/ubuntu/gpg | gpg --dearmor -o /usr/share/keyrings/docker.gpg
    echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/docker.gpg] https://download.docker.com/linux/ubuntu $(. /etc/os-release && echo $UBUNTU_CODENAME) stable" > /etc/apt/sources.list.d/docker.list
    apt-get update -y
    apt-get install -y docker-ce docker-ce-cli containerd.io
    systemctl enable --now docker
  fi
}

ensure_user() {
  if ! id -u "$RUNE_USER" >/dev/null 2>&1; then
    useradd --system --home "$DATA_DIR" --shell /usr/sbin/nologin "$RUNE_USER"
  fi
  mkdir -p "$DATA_DIR"
  chown -R "$RUNE_USER":"$RUNE_GROUP" "$DATA_DIR" || chown -R "$RUNE_USER":"$RUNE_USER" "$DATA_DIR" || true
}

install_from_release() {
  local arch url tmp
  arch=$(arch_normalize)
  tmp=$(mktemp -d)
  url="https://github.com/runestack/rune/releases/download/${RUNE_VERSION}/rune_linux_${arch}.tar.gz"
  log "Downloading $url"
  curl -fsSL -o "$tmp/rune.tgz" "$url"
  tar -C /usr/local/bin -xzf "$tmp/rune.tgz" rune runed
  rm -rf "$tmp"
}

install_from_source() {
  if ! command -v go >/dev/null 2>&1; then
    install_go
  fi
  log "Building Rune from source"
  local src=/opt/rune
  rm -rf "$src" && git clone --branch "$BRANCH" --single-branch https://github.com/runestack/rune.git "$src"
  (cd "$src" && make build)
  install -m 0755 "$src/bin/rune" /usr/local/bin/rune
  install -m 0755 "$src/bin/runed" /usr/local/bin/runed
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
  
  # Verify installation
  if ! /usr/local/go/bin/go version >/dev/null 2>&1; then
    die "Go installation failed"
  fi
  
  log "Go ${version} installed successfully"
  rm -rf "$tmp"
}

install_systemd() {
  local unit=/etc/systemd/system/runed.service
  if [ -f "$unit" ]; then
    log "Systemd unit exists"
  else
    log "Installing systemd unit"
    cat >"$unit" <<UNIT
[Unit]
Description=Rune Server
After=network-online.target docker.service
Wants=network-online.target

[Service]
Type=simple
User=${RUNE_USER}
Group=${RUNE_GROUP}
ExecStart=/usr/local/bin/runed
Restart=on-failure
RestartSec=5
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
UNIT
  fi
  systemctl daemon-reload
  systemctl enable --now runed
}

main() {
  while [ $# -gt 0 ]; do
    case "$1" in
      --version) RUNE_VERSION="$2"; shift 2 ;;
      --from-source) FROM_SOURCE=true; shift ;;
      --branch) BRANCH="$2"; shift 2 ;;
      --grpc-port) GRPC_PORT="$2"; shift 2 ;;
      --http-port) HTTP_PORT="$2"; shift 2 ;;
      -h|--help) usage; exit 0 ;;
      *) die "Unknown argument: $1" ;;
    esac
  done

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

  install_systemd

  log "Done. Check status with: systemctl status runed --no-pager"
}

main "$@"


