#!/usr/bin/env bash
set -euo pipefail

# Rune Server Upgrade
# - Replaces /usr/local/bin/{rune,runed} with a specific release version.
# - Re-applies cap_net_bind_service to runed if the existing install had it.
# - Optionally refreshes the systemd unit from `runed print-unit`
#   (--refresh-unit), so a host whose on-disk unit pre-dates current can
#   pick up new directives like AmbientCapabilities=CAP_NET_BIND_SERVICE.
# - Backs up the old binaries (and old unit, when refreshed) so rollback
#   on verification failure is automatic.
# - Restarts the runed systemd unit and verifies it comes up healthy.
#
# What this script DOES NOT do:
# - Without --refresh-unit, it does not touch the systemd unit. The
#   warning printed when AmbientCapabilities= is missing tells you when
#   to opt in.
# - It does NOT touch the runefile, data dir, KEK, or any Docker state.
# - It does NOT install Docker or Go.
#
# Use install-server.sh for greenfield installs.

RUNE_VERSION=""
BIN_DIR="/usr/local/bin"
SERVICE_NAME="runed"
SKIP_RESTART=false
SKIP_CAPS=false
KEEP_BACKUP=true
REFRESH_UNIT=false
UNIT_PATH="/etc/systemd/system/runed.service"

# Populated at runtime so the cleanup trap can find them.
TMP_DIR=""
BACKUP_RUNE=""
BACKUP_RUNED=""
BACKUP_UNIT=""
HAD_CAPS=false
UNIT_REFRESHED=false

log() { echo "[upgrade-server] $*"; }
die() { echo "[upgrade-server] ERROR: $*" >&2; exit 1; }

usage() {
  cat <<USAGE
Usage: $0 --version vX.Y.Z [--bin-dir DIR] [--skip-restart] [--skip-caps]
           [--no-keep-backup] [--refresh-unit [--unit-path PATH]]

Required:
  --version vX.Y.Z      GitHub release tag to upgrade to (e.g. v0.0.1-dev.43)

Options:
  --bin-dir DIR         Where rune/runed are installed (default: /usr/local/bin)
  --skip-restart        Replace the binaries but don't restart $SERVICE_NAME
                        (useful if you're scripting a maintenance window)
  --skip-caps           Don't re-apply cap_net_bind_service even if the
                        current runed has it. Use if you've moved low-port
                        binding to systemd AmbientCapabilities exclusively.
  --no-keep-backup      Remove the backup binaries after a successful
                        upgrade. Default keeps them at \$BIN_DIR/.{rune,runed}.bak
                        so you can rollback by hand.
  --refresh-unit        Refresh /etc/systemd/system/runed.service from
                        \`runed print-systemd\` of the new binary, before
                        the restart. The previous unit is backed up to
                        \$UNIT_PATH.bak and restored on verification
                        failure. Use this when an older install is
                        missing directives shipped in newer versions
                        (e.g. AmbientCapabilities=CAP_NET_BIND_SERVICE).
                        The script warns when it detects that drift.
  --unit-path PATH      Override the unit file path (default: $UNIT_PATH).
                        Only relevant with --refresh-unit.
  -h, --help            Show help

Behaviour:
  1. Downloads rune_linux_<arch>.tar.gz from the GitHub release.
  2. Notes whether the current runed has cap_net_bind_service set
     (so we know to re-apply it after replacement; setcap doesn't
     survive 'cp' / 'mv' on the binary).
  3. Stops $SERVICE_NAME, swaps the binaries, re-applies caps if needed.
  4. With --refresh-unit: writes \`runed print-unit\` from the NEW
     binary to $UNIT_PATH (backing up the previous unit) and runs
     \`systemctl daemon-reload\` before restart.
  5. Starts $SERVICE_NAME and waits up to 15s for it to report active.
  6. On verification failure: restores the backup binaries and (if
     refreshed) the previous unit, then restarts.

Example:
  curl -fsSL https://rune.sh/upgrade-server.sh | bash -s -- --version v0.0.1-dev.43
USAGE
}

arch_normalize() {
  local m
  m=$(uname -m)
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

# Cleanup runs on every exit. On failure (non-zero exit) it also restores
# the backups so the operator isn't left with a half-upgraded host.
cleanup() {
  local rc=$?
  if [ -n "$TMP_DIR" ] && [ -d "$TMP_DIR" ]; then
    rm -rf "$TMP_DIR"
  fi

  if [ $rc -ne 0 ] && [ -n "$BACKUP_RUNED" ] && [ -f "$BACKUP_RUNED" ]; then
    log "Upgrade failed; restoring previous binaries from backup"
    [ -f "$BACKUP_RUNE" ] && cp -p "$BACKUP_RUNE" "$BIN_DIR/rune" || true
    cp -p "$BACKUP_RUNED" "$BIN_DIR/runed" || true
    if [ "$HAD_CAPS" = true ] && command -v setcap >/dev/null 2>&1; then
      setcap 'cap_net_bind_service=+ep' "$BIN_DIR/runed" || true
    fi
    # Restore the previous unit if --refresh-unit had already swapped
    # it. Skip if we never got that far. daemon-reload is cheap to call
    # again even when the unit didn't actually change.
    if [ "$UNIT_REFRESHED" = true ] && [ -n "$BACKUP_UNIT" ] && [ -f "$BACKUP_UNIT" ]; then
      cp -p "$BACKUP_UNIT" "$UNIT_PATH" || true
      systemctl daemon-reload || true
      log "Restored previous systemd unit from $BACKUP_UNIT"
    fi
    if [ "$SKIP_RESTART" != true ]; then
      systemctl restart "$SERVICE_NAME" || true
    fi
    log "Rolled back to previous state; service restart attempted."
  fi
  exit "$rc"
}
trap cleanup EXIT

download_release() {
  local arch url
  arch=$(arch_normalize)
  TMP_DIR=$(mktemp -d)
  url="https://github.com/runestack/rune/releases/download/${RUNE_VERSION}/rune_linux_${arch}.tar.gz"
  log "Downloading $url"

  if ! curl -fsSL -o "$TMP_DIR/rune.tgz" "$url"; then
    die "Failed to download release from $url"
  fi

  tar -C "$TMP_DIR" -xzf "$TMP_DIR/rune.tgz" rune runed
  chmod 0755 "$TMP_DIR/rune" "$TMP_DIR/runed"
  log "Fetched ${RUNE_VERSION} for linux/${arch}"
}

detect_caps() {
  if [ "$SKIP_CAPS" = true ]; then
    HAD_CAPS=false
    return
  fi
  if ! command -v getcap >/dev/null 2>&1; then
    log "getcap not found; assuming no file capabilities on current runed"
    HAD_CAPS=false
    return
  fi
  if getcap "$BIN_DIR/runed" 2>/dev/null | grep -q 'cap_net_bind_service'; then
    HAD_CAPS=true
    log "Detected cap_net_bind_service on current runed; will re-apply after replacement"
  else
    HAD_CAPS=false
    log "No file capabilities on current runed; nothing to re-apply"
  fi
}

backup_binaries() {
  BACKUP_RUNE="$BIN_DIR/.rune.bak"
  BACKUP_RUNED="$BIN_DIR/.runed.bak"
  if [ -f "$BIN_DIR/rune" ]; then
    cp -p "$BIN_DIR/rune" "$BACKUP_RUNE"
  fi
  if [ -f "$BIN_DIR/runed" ]; then
    cp -p "$BIN_DIR/runed" "$BACKUP_RUNED"
  else
    die "$BIN_DIR/runed not found — is rune installed on this host?"
  fi
  log "Backed up current binaries to $BACKUP_RUNE / $BACKUP_RUNED"
}

stop_service() {
  if [ "$SKIP_RESTART" = true ]; then
    return
  fi
  if systemctl is-active --quiet "$SERVICE_NAME"; then
    log "Stopping $SERVICE_NAME"
    systemctl stop "$SERVICE_NAME"
  else
    log "$SERVICE_NAME is not active; skipping stop"
  fi
}

swap_binaries() {
  # `install` is atomic per-file: it writes to a temp inode and renames.
  # Doing it twice (rune + runed) isn't transactionally atomic across both,
  # but neither binary is referenced by the other at startup, so a transient
  # mismatch during the ~ms window between the two `install` calls is fine.
  install -m 0755 "$TMP_DIR/rune"  "$BIN_DIR/rune"
  install -m 0755 "$TMP_DIR/runed" "$BIN_DIR/runed"
  log "Installed new binaries to $BIN_DIR"
}

reapply_caps() {
  if [ "$HAD_CAPS" != true ]; then
    return
  fi
  if ! command -v setcap >/dev/null 2>&1; then
    log "⚠️  setcap not found; cannot re-apply cap_net_bind_service. Install libcap2-bin (Debian/Ubuntu) or libcap (RHEL)."
    return
  fi
  setcap 'cap_net_bind_service=+ep' "$BIN_DIR/runed"
  log "Re-applied cap_net_bind_service to $BIN_DIR/runed"
}

warn_if_unit_missing_ambient() {
  if [ "$REFRESH_UNIT" = true ]; then
    # Caller is opting into the unit refresh already; no need to nag.
    return
  fi
  local unit
  for unit in /etc/systemd/system/runed.service /lib/systemd/system/runed.service; do
    if [ -f "$unit" ]; then
      if ! grep -q '^AmbientCapabilities=.*CAP_NET_BIND_SERVICE' "$unit"; then
        log "⚠️  $unit is missing 'AmbientCapabilities=CAP_NET_BIND_SERVICE'."
        log "    File caps cover this install today. To pick up newer unit"
        log "    directives, re-run with --refresh-unit (writes a fresh unit"
        log "    via 'runed print-systemd' from the upgraded binary)."
      fi
      return
    fi
  done
}

refresh_unit() {
  if [ "$REFRESH_UNIT" != true ]; then
    return
  fi
  if [ ! -x "$BIN_DIR/runed" ]; then
    die "--refresh-unit: $BIN_DIR/runed is not executable; cannot generate unit"
  fi
  if ! "$BIN_DIR/runed" print-systemd --help >/dev/null 2>&1; then
    # Older runed without the subcommand. Skip rather than break the
    # upgrade — operators upgrading TO a print-systemd-capable build
    # for the first time will hit this once; the next upgrade refreshes
    # cleanly.
    log "⚠️  --refresh-unit: this runed build does not support 'print-systemd'."
    log "    The binary swap has already completed; the on-disk unit is unchanged."
    log "    Next upgrade with --refresh-unit will pick up the new unit shape."
    return
  fi

  local new_unit
  new_unit="$TMP_DIR/runed.service.new"
  # Render with the NEW binary so the unit matches what the new runed
  # expects (ExecStart points at it, any new directives ship cleanly).
  if ! "$BIN_DIR/runed" print-systemd > "$new_unit"; then
    die "--refresh-unit: 'runed print-systemd' failed"
  fi

  if [ -f "$UNIT_PATH" ]; then
    BACKUP_UNIT="${UNIT_PATH}.bak"
    cp -p "$UNIT_PATH" "$BACKUP_UNIT"
    log "Backed up current unit to $BACKUP_UNIT"
  fi

  install -m 0644 "$new_unit" "$UNIT_PATH"
  UNIT_REFRESHED=true
  log "Installed refreshed unit at $UNIT_PATH"

  systemctl daemon-reload
}

start_service() {
  if [ "$SKIP_RESTART" = true ]; then
    log "Skipping restart (--skip-restart). Binaries are in place; restart $SERVICE_NAME manually when ready."
    return
  fi
  log "Starting $SERVICE_NAME"
  systemctl start "$SERVICE_NAME"
}

verify() {
  if [ "$SKIP_RESTART" = true ]; then
    return
  fi

  local deadline=$(( $(date +%s) + 15 ))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    if systemctl is-active --quiet "$SERVICE_NAME"; then
      log "✅ $SERVICE_NAME is active"
      break
    fi
    sleep 1
  done

  if ! systemctl is-active --quiet "$SERVICE_NAME"; then
    log "❌ $SERVICE_NAME did not become active within 15s"
    log "Recent logs:"
    journalctl -u "$SERVICE_NAME" --no-pager -n 30 || true
    die "Service did not come up after upgrade"
  fi

  # Spot-check the new binary actually got swapped in.
  local installed
  installed=$("$BIN_DIR/runed" --version 2>/dev/null | head -n1 || true)
  if [ -n "$installed" ]; then
    log "Installed version reports: $installed"
  fi
}

cleanup_backups() {
  if [ "$KEEP_BACKUP" = true ]; then
    log "Keeping rollback backups at $BACKUP_RUNE / $BACKUP_RUNED"
    if [ "$UNIT_REFRESHED" = true ] && [ -n "$BACKUP_UNIT" ]; then
      log "  (previous unit at $BACKUP_UNIT)"
    fi
    log "Remove them when you're satisfied:  rm $BACKUP_RUNE $BACKUP_RUNED${BACKUP_UNIT:+ $BACKUP_UNIT}"
    return
  fi
  rm -f "$BACKUP_RUNE" "$BACKUP_RUNED"
  if [ "$UNIT_REFRESHED" = true ] && [ -n "$BACKUP_UNIT" ]; then
    rm -f "$BACKUP_UNIT"
  fi
  log "Removed backup binaries (--no-keep-backup)"
}

main() {
  while [ $# -gt 0 ]; do
    case "$1" in
      --version) RUNE_VERSION="$2"; shift 2 ;;
      --bin-dir) BIN_DIR="$2"; shift 2 ;;
      --skip-restart) SKIP_RESTART=true; shift ;;
      --skip-caps) SKIP_CAPS=true; shift ;;
      --no-keep-backup) KEEP_BACKUP=false; shift ;;
      --refresh-unit) REFRESH_UNIT=true; shift ;;
      --unit-path) UNIT_PATH="$2"; shift 2 ;;
      -h|--help) usage; exit 0 ;;
      *) die "Unknown argument: $1" ;;
    esac
  done

  [ -n "$RUNE_VERSION" ] || die "--version is required (e.g. --version v0.0.1-dev.43)"

  ensure_root

  log "Upgrading Rune server to $RUNE_VERSION"
  download_release
  detect_caps
  backup_binaries
  stop_service
  swap_binaries
  reapply_caps
  refresh_unit
  warn_if_unit_missing_ambient
  start_service
  verify
  cleanup_backups

  log "✅ Upgrade to $RUNE_VERSION complete"
}

main "$@"
