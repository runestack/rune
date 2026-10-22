#!/usr/bin/env bash
# RUNE-322 Phase 0: one-time host setup for a microVM spike box
# (Scaleway Elastic Metal for the spike; any bare-metal Ubuntu box with KVM works).
#
# Before running, when installing the OS from the provider's console:
#   - Image: Ubuntu 24.04 LTS.
#   - Custom partitioning: NO software RAID. Put the OS on the first NVMe only
#     and leave the second NVMe completely empty. It becomes the dm-thin pool.
#
# Usage (as root):
#   FC_VERSION=<firecracker release, e.g. v1.x.y> THIN_DEV=/dev/nvme1n1 ./host-setup.sh
#
# Safe to re-run. The thin pool step refuses to touch a disk that has
# partitions, a filesystem, or is mounted, unless the pool already exists.
set -euo pipefail

: "${FC_VERSION:?set FC_VERSION to a Firecracker release tag, e.g. v1.x.y}"
: "${THIN_DEV:?set THIN_DEV to the empty NVMe for the thin pool, e.g. /dev/nvme1n1}"
ARCH="$(uname -m)"
PREFIX=/opt/rune-spike
VG=spike
POOL=thinpool

log() { printf '\n==> %s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "run as root"
[[ "$ARCH" == "x86_64" ]] || die "expected x86_64, got $ARCH"

log "Checking KVM and cgroup v2"
[[ -c /dev/kvm ]] || die "/dev/kvm missing: virtualization disabled in BIOS or not bare metal"
[[ "$(stat -fc %T /sys/fs/cgroup)" == "cgroup2fs" ]] \
  || die "cgroup v2 not mounted at /sys/fs/cgroup (Firecracker restore is slow on v1)"
grep -qw svm /proc/cpuinfo || die "AMD-V (svm) flag missing"

log "Installing packages"
export DEBIAN_FRONTEND=noninteractive
apt-get update -q
apt-get install -yq --no-install-recommends \
  ca-certificates curl jq git make gcc \
  lvm2 thin-provisioning-tools e2fsprogs \
  nftables iproute2 bridge-utils acl \
  sysstat hyperfine fio numactl
# perf for the kernel actually running; provider kernels may not have a package.
apt-get install -yq linux-tools-common linux-tools-"$(uname -r)" \
  || echo "warning: no linux-tools for $(uname -r); perf unavailable"
# Go for the guest agent, UFFD handler and harness. Ubuntu's golang-go is too
# old for this module (go 1.25), so install the upstream release, checksummed.
GO_VERSION="${GO_VERSION:-1.25.8}"
if ! /usr/local/go/bin/go version 2>/dev/null | grep -q "go${GO_VERSION} "; then
  gotgz="go${GO_VERSION}.linux-amd64.tar.gz"
  curl -fsSL "https://dl.google.com/go/${gotgz}" -o "/tmp/${gotgz}"
  echo "$(curl -fsSL "https://dl.google.com/go/${gotgz}.sha256")  /tmp/${gotgz}" | sha256sum -c -
  rm -rf /usr/local/go && tar -C /usr/local -xzf "/tmp/${gotgz}" && rm -f "/tmp/${gotgz}"
fi
ln -sf /usr/local/go/bin/go /usr/local/bin/go

log "Installing Firecracker $FC_VERSION (verified against the release checksum)"
mkdir -p "$PREFIX/bin" "$PREFIX/dl"
cd "$PREFIX/dl"
base="https://github.com/firecracker-microvm/firecracker/releases/download/${FC_VERSION}"
tgz="firecracker-${FC_VERSION}-${ARCH}.tgz"
[[ -f "$tgz" ]] || curl -fsSLO "$base/$tgz"
[[ -f "$tgz.sha256.txt" ]] || curl -fsSLO "$base/$tgz.sha256.txt"
sha256sum -c "$tgz.sha256.txt"
tar -xzf "$tgz"
rel="release-${FC_VERSION}-${ARCH}"
install -m 0755 "$rel/firecracker-${FC_VERSION}-${ARCH}" "$PREFIX/bin/firecracker"
install -m 0755 "$rel/jailer-${FC_VERSION}-${ARCH}" "$PREFIX/bin/jailer"
# snapshot-editor rebases a Diff memory snapshot onto its base (fork, E6).
install -m 0755 "$rel/snapshot-editor-${FC_VERSION}-${ARCH}" "$PREFIX/bin/snapshot-editor"
"$PREFIX/bin/firecracker" --version

log "KVM access and jailer user"
id -u fcjail >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin fcjail
setfacl -m u:fcjail:rw /dev/kvm
mkdir -p /srv/jailer

log "Kernel settings for many VMs"
cat >/etc/sysctl.d/90-rune-spike.conf <<'EOF'
net.ipv4.ip_forward = 1
net.ipv6.conf.all.forwarding = 1
vm.max_map_count = 1048576
fs.inotify.max_user_instances = 8192
kernel.pid_max = 4194304
# userfaultfd for unprivileged handlers (the spike runs them as root anyway).
vm.unprivileged_userfaultfd = 1
EOF
sysctl --system >/dev/null
# KSM off: the spike measures sharing from MAP_PRIVATE golden images alone (E7).
echo 0 >/sys/kernel/mm/ksm/run || true

log "dm-thin pool on $THIN_DEV"
if vgs "$VG" >/dev/null 2>&1 && lvs "$VG/$POOL" >/dev/null 2>&1; then
  echo "thin pool $VG/$POOL already exists; leaving it alone"
else
  [[ -b "$THIN_DEV" ]] || die "$THIN_DEV is not a block device"
  if lsblk -no MOUNTPOINT "$THIN_DEV" | grep -q .; then die "$THIN_DEV (or a partition) is mounted"; fi
  if [[ "$(lsblk -no NAME "$THIN_DEV" | wc -l)" -gt 1 ]]; then die "$THIN_DEV has partitions; refusing"; fi
  if blkid "$THIN_DEV" >/dev/null 2>&1; then die "$THIN_DEV has a filesystem or RAID signature; refusing"; fi
  pvcreate "$THIN_DEV"
  vgcreate "$VG" "$THIN_DEV"
  # Leave 5% of the VG free so the pool's metadata can be grown later.
  lvcreate --type thin-pool -l 95%VG --poolmetadatasize 16G -n "$POOL" "$VG"
fi
lvs "$VG"

log "Spike directories"
# Templates, forks and the jailer roots must share one filesystem so clones
# hardlink (and so share page cache for) one memory file.
mkdir -p "$PREFIX"/{kernels,images,templates,forks,disks,results}
[[ "$(stat -c %d "$PREFIX/templates")" == "$(stat -c %d /srv/jailer)" ]] \
  || echo "warning: $PREFIX and /srv/jailer are on different filesystems; restores will copy memory files"

log "Recording the host fingerprint for RESULTS.md"
{
  echo "date: $(date -Is)"
  echo "kernel: $(uname -r)"
  echo "cpu: $(lscpu | awk -F: '/Model name/{gsub(/^ +/,"",$2);print $2}')"
  echo "threads: $(nproc)"
  echo "mem_gib: $(awk '/MemTotal/{printf "%.0f", $2/1048576}' /proc/meminfo)"
  echo "firecracker: $("$PREFIX/bin/firecracker" --version | head -1)"
  echo "thin_dev: $THIN_DEV ($(lsblk -dno MODEL,SIZE "$THIN_DEV"))"
} | tee "$PREFIX/results/host.txt"

log "Done. Next: build the spike binaries, the guest kernel and the templates (see spike/microvm/README.md)."
