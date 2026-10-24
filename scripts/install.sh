#!/bin/sh
set -eu

# Rune server installer — the script behind
#   curl -fsSL https://install.runestack.io | sudo sh
# - Installs Docker (if missing)
# - Creates the rune user and directories
# - Installs rune/runed (latest release by default; --version to pin)
# - Installs and enables the systemd unit
# Building from source is opt-in (--from-source). Requires root.

REPO="runestack/rune"
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

# resolve_latest returns the newest release tag (currently includes prereleases
# — every release today is v0.0.1-dev.*, so releases/latest 404s).
# TODO(stable): switch to the releases/latest endpoint once a stable tag exists.
resolve_latest() {
  tag=$(curl -fsSL "https://api.github.com/repos/$REPO/releases?per_page=1" 2>/dev/null \
    | grep -m1 '"tag_name"' \
    | sed -E 's/.*"tag_name"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/')
  [ -n "$tag" ] || die "could not resolve the latest release (GitHub API unreachable or rate-limited). Pass --version vX.Y.Z."
  echo "$tag"
}

# sha256 prints the hex digest of a file, or "" if no tool is available.
sha256() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1" | awk '{print $1}'
  else echo ""; fi
}

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

This installer sets up the full Rune server stack (API, Agent, CLI) and requires root.
  curl -fsSL https://install.runestack.io | sudo sh
For the CLI only (no root, no server), use: curl -fsSL https://get.runestack.io | sh
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
  if [ "$(id -u)" -ne 0 ]; then
    die "the server installer must run as root — pipe into sudo, e.g.:
  curl -fsSL https://install.runestack.io | sudo sh
(to install just the CLI without root, use https://get.runestack.io instead)"
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
  local arch asset url tmp have want
  arch=$(arch_normalize)
  asset="rune_linux_${arch}.tar.gz"
  tmp=$(mktemp -d)
  url="https://github.com/${REPO}/releases/download/${RUNE_VERSION}/${asset}"
  log "Downloading $asset (${RUNE_VERSION})"
  curl -fsSL -o "$tmp/$asset" "$url" || { rm -rf "$tmp"; die "download failed: $url"; }

  # Verify against the release checksums.txt (best-effort; a real mismatch aborts).
  if curl -fsSL -o "$tmp/checksums.txt" "https://github.com/${REPO}/releases/download/${RUNE_VERSION}/checksums.txt" 2>/dev/null; then
    have=$(sha256 "$tmp/$asset")
    want=$(grep "$asset" "$tmp/checksums.txt" | awk '{print $1}' | head -1)
    if [ -n "$have" ] && [ -n "$want" ]; then
      [ "$have" = "$want" ] || { rm -rf "$tmp"; die "checksum mismatch for $asset (expected $want, got $have)"; }
      log "checksum verified"
    fi
  fi

  tar -C /usr/local/bin -xzf "$tmp/$asset" rune runed
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
  install_upgrade_units
  systemctl daemon-reload
  systemctl enable --now runed
}

# RUNE-321: in-band upgrade units + version floor, so upgrades after this
# one can be `rune upgrade` instead of SSH. Rendered by the installed
# binary — no second template to drift. Skipped on builds that predate it.
install_upgrade_units() {
  local bin=/usr/local/bin/runed staging="$DATA_DIR/upgrade" floor_version
  if ! "$bin" print-systemd --upgrade-units --staging "$staging" </dev/null >/dev/null 2>&1; then
    return
  fi
  # A build with no release version cannot seed a floor, and arming the
  # units without one leaves the host downgrade-open until its first
  # successful in-band upgrade seeds one.
  floor_version="${RUNE_VERSION:-$("$bin" --version 2>/dev/null | grep -oE 'v[0-9][^ )]*' | head -1 || true)}"
  if [ -z "$floor_version" ] && [ ! -f /etc/rune/version-floor ]; then
    log "This build reports no release version; skipping in-band upgrade units"
    return
  fi
  "$bin" print-systemd --upgrade-units --staging "$staging" --binary "$bin" --config "" > /etc/systemd/system/runed-upgrade.service
  "$bin" print-systemd --upgrade-path-unit --staging "$staging" > /etc/systemd/system/runed-upgrade.path
  systemctl daemon-reload
  systemctl enable --now runed-upgrade.path 2>/dev/null || true
  if [ ! -f /etc/rune/version-floor ]; then
    mkdir -p /etc/rune
    printf '%s\n' "$floor_version" > /etc/rune/version-floor
  fi
  log "Installed in-band upgrade units (rune upgrade)"
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

  if [ "$FROM_SOURCE" = true ]; then
    install_from_source
  else
    [ -n "$RUNE_VERSION" ] || RUNE_VERSION=$(resolve_latest)
    log "Installing rune server $RUNE_VERSION"
    install_from_release
  fi

  install_systemd

  log "Done. Check status with: systemctl status runed --no-pager"
}

main "$@"


