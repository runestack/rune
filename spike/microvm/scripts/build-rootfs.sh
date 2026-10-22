#!/usr/bin/env bash
# Build an ext4 root filesystem image for a spike template.
#
#   base:    Ubuntu 24.04 + systemd + Docker, git, Node, Python, sshd
#   desktop: base + Xvfb, Openbox, Google Chrome, x11vnc and noVNC.
#            Chrome starts at boot so the golden snapshot has it running.
#            Selkies is added for E12; this first cut only needs a live browser.
#
# The guest agent (sbx-agent) is installed as a systemd service on vsock 1024.
# The image has no network config: E1-E3 do not need it, and E11 adds it.
#
# Usage (as root, on Ubuntu):
#   AGENT=./bin/sbx-agent ./build-rootfs.sh base /opt/rune-spike/images/base.ext4 [8G]
set -euo pipefail

TEMPLATE="${1:?template: base or desktop}"
OUT="${2:?output image path}"
SIZE="${3:-8G}"
AGENT="${AGENT:?set AGENT to the static sbx-agent binary}"
SUITE=noble
MIRROR="${MIRROR:-http://archive.ubuntu.com/ubuntu}"

[[ $EUID -eq 0 ]] || { echo "run as root" >&2; exit 1; }
[[ "$TEMPLATE" == base || "$TEMPLATE" == desktop ]] || { echo "unknown template $TEMPLATE" >&2; exit 1; }

export DEBIAN_FRONTEND=noninteractive
apt-get install -yq --no-install-recommends debootstrap e2fsprogs >/dev/null

ROOT="$(mktemp -d /var/tmp/rootfs-XXXX)"
cleanup() {
  for m in proc sys dev/pts dev; do umount -lf "$ROOT/$m" 2>/dev/null || true; done
  rm -rf "$ROOT"
}
trap cleanup EXIT

debootstrap --variant=minbase --include=systemd-sysv,udev,dbus,ca-certificates "$SUITE" "$ROOT" "$MIRROR"
cat >"$ROOT/etc/apt/sources.list" <<SRC
deb $MIRROR $SUITE main universe
deb $MIRROR $SUITE-updates main universe
SRC

mount -t proc proc "$ROOT/proc"
mount -t sysfs sys "$ROOT/sys"
mount --bind /dev "$ROOT/dev"
mount --bind /dev/pts "$ROOT/dev/pts"
in_root() { chroot "$ROOT" /usr/bin/env DEBIAN_FRONTEND=noninteractive "$@"; }

in_root apt-get update -q
in_root apt-get install -yq --no-install-recommends \
  iproute2 iputils-ping curl git openssh-server sudo procps less \
  docker.io nodejs npm python3 python3-venv

if [[ "$TEMPLATE" == desktop ]]; then
  in_root apt-get install -yq --no-install-recommends \
    xvfb openbox x11vnc novnc websockify fonts-dejavu-core gnupg
  curl -fsSL https://dl.google.com/linux/direct/google-chrome-stable_current_amd64.deb -o "$ROOT/tmp/chrome.deb"
  in_root apt-get install -yq /tmp/chrome.deb
  rm -f "$ROOT/tmp/chrome.deb"
  in_root useradd -m -s /bin/bash sbx
  cat >"$ROOT/usr/local/bin/sbx-desktop" <<'SH'
#!/bin/sh
# Start the display stack, then hand the service over to Chrome.
Xvfb :0 -screen 0 1920x1080x24 -nolisten tcp &
for _ in $(seq 50); do [ -e /tmp/.X11-unix/X0 ] && break; sleep 0.1; done
openbox &
x11vnc -display :0 -forever -shared -nopw -quiet -rfbport 5900 &
exec /usr/bin/google-chrome --no-first-run --no-default-browser-check --disable-gpu \
  --user-data-dir=/home/sbx/.chrome --remote-debugging-port=9222 about:blank
SH
  chmod 0755 "$ROOT/usr/local/bin/sbx-desktop"
  cat >"$ROOT/etc/systemd/system/sbx-desktop.service" <<'UNIT'
[Unit]
Description=Spike desktop: Xvfb, Openbox, Chrome, x11vnc
After=multi-user.target

[Service]
User=sbx
Environment=DISPLAY=:0
ExecStart=/usr/local/bin/sbx-desktop
Restart=on-failure

[Install]
WantedBy=multi-user.target
UNIT
  in_root systemctl enable sbx-desktop.service
fi

install -m 0755 "$AGENT" "$ROOT/usr/local/bin/sbx-agent"
cat >"$ROOT/etc/systemd/system/sbx-agent.service" <<'UNIT'
[Unit]
Description=Rune sandbox guest agent (vsock)
DefaultDependencies=no
After=local-fs.target

[Service]
ExecStart=/usr/local/bin/sbx-agent
Restart=always
RestartSec=100ms

[Install]
WantedBy=multi-user.target
UNIT
in_root systemctl enable sbx-agent.service

# Boot speed and clone hygiene.
echo "/dev/vda / ext4 defaults,noatime 0 1" >"$ROOT/etc/fstab"
echo "sbx" >"$ROOT/etc/hostname"
: >"$ROOT/etc/machine-id"   # systemd fills it on first boot; the clone hook replaces it
in_root systemctl mask serial-getty@ttyS0.service systemd-networkd-wait-online.service \
  apt-daily.timer apt-daily-upgrade.timer motd-news.timer e2scrub_reap.service >/dev/null
in_root apt-get clean
rm -rf "$ROOT"/var/lib/apt/lists/* "$ROOT"/etc/ssh/ssh_host_*
# One set of sshd host keys, baked into the image, so every clone starts with
# the same keys; E3 checks that the clone hook replaces them.
in_root ssh-keygen -A >/dev/null 2>&1 || true

for m in dev/pts dev sys proc; do umount -l "$ROOT/$m"; done
mkdir -p "$(dirname "$OUT")"
rm -f "$OUT"
truncate -s "$SIZE" "$OUT"
mkfs.ext4 -q -F -L rootfs -d "$ROOT" "$OUT"
echo "built $TEMPLATE -> $OUT ($(du -h --apparent-size "$OUT" | cut -f1) apparent, $(du -h "$OUT" | cut -f1) used)"
