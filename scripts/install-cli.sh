#!/bin/sh
set -eu

# Rune CLI installer — the script behind `curl -fsSL https://get.runestack.io | sh`.
# - Installs the rune CLI binary (no server components).
# - Works on Linux and macOS, amd64/arm64.
# - Defaults to the latest release; downloads a prebuilt, checksum-verified
#   binary. Building from source is opt-in (--from-source).

REPO="runestack/rune"
RUNE_VERSION=""
FROM_SOURCE=false
BRANCH="master"
INSTALL_DIR="/usr/local/bin"
FORCE=true

log() { echo "[install-cli] $*"; }
die() { echo "[install-cli] ERROR: $*" >&2; exit 1; }

# resolve_latest returns the newest release tag.
# NOTE: this currently includes prereleases (every release today is a
# v0.0.1-dev.* prerelease, so GitHub's releases/latest endpoint 404s).
# TODO(stable): once a non-prerelease release exists, switch to:
#   curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" | tag_name
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
  # Friendly default for `curl | sh`: if the default dir isn't writable and we
  # aren't root, fall back to a user-owned dir instead of demanding sudo.
  if [ "$INSTALL_DIR" = "/usr/local/bin" ] && [ "$(id -u)" -ne 0 ] && [ ! -w "$INSTALL_DIR" ]; then
    INSTALL_DIR="$HOME/.local/bin"
    log "Not root; installing to $INSTALL_DIR"
  fi
  mkdir -p "$INSTALL_DIR" 2>/dev/null || die "cannot create install dir $INSTALL_DIR (try sudo, or --install-dir PATH)"
  [ -w "$INSTALL_DIR" ] || die "no write permission to $INSTALL_DIR (try sudo, or --install-dir PATH)"
}

# path_hint warns if INSTALL_DIR isn't on PATH so the user knows to add it.
path_hint() {
  case ":$PATH:" in
    *":$INSTALL_DIR:"*) ;;
    *) log "Note: $INSTALL_DIR is not on your PATH. Add it, e.g.: export PATH=\"$INSTALL_DIR:\$PATH\"" ;;
  esac
}

install_from_release() {
  local arch os asset url tmp have want
  arch=$(arch_normalize)
  os=$(os_normalize)
  asset="rune-cli_${os}_${arch}.tar.gz"
  tmp=$(mktemp -d)

  url="https://github.com/${REPO}/releases/download/${RUNE_VERSION}/${asset}"
  log "Downloading $asset (${RUNE_VERSION})"
  curl -fsSL -o "$tmp/$asset" "$url" || { rm -rf "$tmp"; die "download failed: $url"; }

  # Verify against the release checksums.txt. Best-effort: skipped only when no
  # sha256 tool is present or the asset's line is absent; a real mismatch aborts.
  if curl -fsSL -o "$tmp/checksums.txt" "https://github.com/${REPO}/releases/download/${RUNE_VERSION}/checksums.txt" 2>/dev/null; then
    have=$(sha256 "$tmp/$asset")
    want=$(grep "$asset" "$tmp/checksums.txt" | awk '{print $1}' | head -1)
    if [ -n "$have" ] && [ -n "$want" ]; then
      [ "$have" = "$want" ] || { rm -rf "$tmp"; die "checksum mismatch for $asset (expected $want, got $have)"; }
      log "checksum verified"
    fi
  fi

  tar -C "$tmp" -xzf "$tmp/$asset" rune
  install -m 0755 "$tmp/rune" "$INSTALL_DIR/rune" 2>/dev/null || { cp "$tmp/rune" "$INSTALL_DIR/rune"; chmod 0755 "$INSTALL_DIR/rune"; }
  rm -rf "$tmp"
  log "Installed rune CLI ${RUNE_VERSION} to $INSTALL_DIR/rune"
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
  # Check the binary we just installed directly — it may not be on PATH yet
  # (e.g. a ~/.local/bin fallback install), which path_hint already flagged.
  if [ -x "$INSTALL_DIR/rune" ]; then
    log "Installation verified:"
    "$INSTALL_DIR/rune" --version || true
  else
    die "Installation verification failed - $INSTALL_DIR/rune not found"
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

  if [ "$FROM_SOURCE" = true ]; then
    install_from_source
  else
    [ -n "$RUNE_VERSION" ] || RUNE_VERSION=$(resolve_latest)
    log "Installing rune CLI $RUNE_VERSION"
    install_from_release
  fi

  path_hint
  verify_installation

  log "Rune CLI installation complete! Run: rune --help"
}

main "$@"
