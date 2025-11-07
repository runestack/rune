#!/usr/bin/env bash
set -euo pipefail

# Rune CLI-only installer
# - Installs only the rune CLI binary (no server components)
# - Works on Linux, macOS, and Windows
# - Downloads from GitHub releases or builds from source

RUNE_VERSION=""
FROM_SOURCE=false
BRANCH="master"
INSTALL_DIR="/usr/local/bin"
FORCE=false

log() { echo "[install-cli] $*"; }
die() { echo "[install-cli] ERROR: $*" >&2; exit 1; }

usage() {
  cat <<USAGE
Usage: $0 [--version vX.Y.Z | --from-source] [--branch NAME] [--install-dir PATH] [--force]
Options:
  --version vX.Y.Z   Install from GitHub release tag (preferred)
  --from-source      Build from source if no release is specified
  --branch NAME      Git branch to clone when building from source (default: master)
  --install-dir PATH Install directory (default: /usr/local/bin)
  --force            Force overwrite if binary exists
  -h, --help         Show help
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

os_normalize() {
  local os; os=$(uname -s | tr '[:upper:]' '[:lower:]')
  case "$os" in
    linux) echo linux ;;
    darwin) echo darwin ;;
    *) die "Unsupported OS: $os" ;;
  esac
}

ensure_install_dir() {
  if [ ! -d "$INSTALL_DIR" ]; then
    if [ "$(id -u)" -eq 0 ]; then
      mkdir -p "$INSTALL_DIR"
    else
      die "Install directory $INSTALL_DIR does not exist and you don't have permission to create it"
    fi
  fi
  
  if [ ! -w "$INSTALL_DIR" ]; then
    die "No write permission to $INSTALL_DIR. Try running with sudo or use --install-dir to specify a writable location"
  fi
}

install_from_release() {
  local arch os url tmp
  arch=$(arch_normalize)
  os=$(os_normalize)
  tmp=$(mktemp -d)
  
  url="https://github.com/runestack/rune/releases/download/${RUNE_VERSION}/rune-cli_${os}_${arch}.tar.gz"
  log "Downloading CLI-only release from $url"
  
  if ! curl -fsSL -o "$tmp/rune-cli.tgz" "$url"; then
    die "Failed to download CLI-only release from ${RUNE_VERSION}. CLI-only releases are required for this installer."
  fi
  
  if [ -f "$INSTALL_DIR/rune" ] && [ "$FORCE" != "true" ]; then
    log "Binary already exists at $INSTALL_DIR/rune. Use --force to overwrite"
    return 1
  fi
  
  # Extract the rune binary from CLI-only release
  tar -C "$tmp" -xzf "$tmp/rune-cli.tgz" rune
  if [ "$(id -u)" -eq 0 ]; then
    install -m 0755 "$tmp/rune" "$INSTALL_DIR/rune"
  else
    cp "$tmp/rune" "$INSTALL_DIR/rune"
    chmod +x "$INSTALL_DIR/rune"
  fi
  
  rm -rf "$tmp"
  log "Installed rune CLI to $INSTALL_DIR/rune"
}

install_from_source() {
  if ! command -v git >/dev/null 2>&1 || ! command -v make >/dev/null 2>&1; then
    install_dev_tools
  fi

  if ! command -v go >/dev/null 2>&1; then
    install_go
  fi
  
  # Ensure module cache env in non-interactive shells
  if [ -z "${HOME:-}" ]; then
    export HOME=/root
  fi
  export GOPATH="${GOPATH:-$HOME/go}"
  export GOMODCACHE="${GOMODCACHE:-$GOPATH/pkg/mod}"
  go env -w GOPATH="$GOPATH" >/dev/null 2>&1 || true
  go env -w GOMODCACHE="$GOMODCACHE" >/dev/null 2>&1 || true
  
  log "Building Rune CLI from source"
  local src=/tmp/rune-cli-build
  rm -rf "$src"
  
  git clone --branch "$BRANCH" --single-branch https://github.com/runestack/rune.git "$src"
  (cd "$src" && make build)
  
  if [ -f "$INSTALL_DIR/rune" ] && [ "$FORCE" != "true" ]; then
    log "Binary already exists at $INSTALL_DIR/rune. Use --force to overwrite"
    return 1
  fi
  
  if [ "$(id -u)" -eq 0 ]; then
    install -m 0755 "$src/bin/rune" "$INSTALL_DIR/rune"
  else
    cp "$src/bin/rune" "$INSTALL_DIR/rune"
    chmod +x "$INSTALL_DIR/rune"
  fi
  
  rm -rf "$src"
  log "Built and installed rune CLI to $INSTALL_DIR/rune"
}

install_go() {
  local version="1.22.5"
  local arch os tmp
  
  log "Installing Go ${version}"
  
  arch=$(arch_normalize)
  os=$(os_normalize)
  tmp=$(mktemp -d)
  
  # Download Go
  local url="https://go.dev/dl/go${version}.${os}-${arch}.tar.gz"
  log "Downloading Go ${version} from $url"
  
  if ! curl -fsSL -o "$tmp/go.tgz" "$url"; then
    # Fallback to amd64 if specific arch fails
    url="https://go.dev/dl/go${version}.${os}-amd64.tar.gz"
    log "Falling back to amd64: $url"
    if ! curl -fsSL -o "$tmp/go.tgz" "$url"; then
      die "Failed to download Go ${version}"
    fi
  fi
  
  # Extract to /usr/local
  if [ "$(id -u)" -eq 0 ]; then
    rm -rf /usr/local/go && tar -C /usr/local -xzf "$tmp/go.tgz"
  else
    die "Go installation requires root privileges. Please install Go ${version}+ manually or run with sudo"
  fi
  
  # Add Go to PATH for current session
  export PATH="/usr/local/go/bin:$PATH"
  
  # Verify installation
  if ! /usr/local/go/bin/go version >/dev/null 2>&1; then
    die "Go installation failed"
  fi
  
  log "Go ${version} installed successfully"
  rm -rf "$tmp"
}

install_dev_tools() {
  log "Installing Development Tools (Git, Make, etc.)"
  
  if [ "$(id -u)" -eq 0 ]; then
    # Detect OS and install Development Tools
    if command -v apt-get >/dev/null 2>&1; then
      # Debian/Ubuntu
      apt-get update -y
      apt-get install -y build-essential git
    elif command -v yum >/dev/null 2>&1; then
      # RHEL/CentOS/Amazon Linux
      yum update -y || true
      yum groupinstall -y "Development Tools" || yum install -y git make
    elif command -v dnf >/dev/null 2>&1; then
      # Fedora/RHEL 8+
      dnf update -y || true
      dnf groupinstall -y "Development Tools" || dnf install -y git make
    elif command -v brew >/dev/null 2>&1; then
      # macOS with Homebrew
      brew install git make
    else
      die "Could not detect package manager. Please install Git and Make manually and try again"
    fi
  else
    die "Development tools installation requires root privileges. Please install them manually or run with sudo"
  fi
  
  # Verify key tools are available
  local missing_tools=""
  for tool in git make; do
    if ! command -v "$tool" >/dev/null 2>&1; then
      missing_tools="$missing_tools $tool"
    fi
  done
  
  if [ -n "$missing_tools" ]; then
    die "Installation incomplete. Missing tools:$missing_tools"
  fi
  
  log "Development tools installed successfully"
}

verify_installation() {
  if command -v rune >/dev/null 2>&1; then
    log "Installation verified successfully"
    rune version
  else
    die "Installation verification failed - 'rune' command not found in PATH"
  fi
}

main() {
  while [ $# -gt 0 ]; do
    case "$1" in
      --version) RUNE_VERSION="$2"; shift 2 ;;
      --from-source) FROM_SOURCE=true; shift ;;
      --branch) BRANCH="$2"; shift 2 ;;
      --install-dir) INSTALL_DIR="$2"; shift 2 ;;
      --force) FORCE=true; shift ;;
      -h|--help) usage; exit 0 ;;
      *) die "Unknown argument: $1" ;;
    esac
  done

  ensure_install_dir

  if [ -n "$RUNE_VERSION" ]; then
    install_from_release || { log "Release install failed; falling back to source"; install_from_source; }
  elif [ "$FROM_SOURCE" = true ]; then
    install_from_source
  else
    log "No version specified; installing from source"
    install_from_source
  fi

  verify_installation
  
  log "Rune CLI installation complete!"
  log "You can now use: rune --help"
}

main "$@"
