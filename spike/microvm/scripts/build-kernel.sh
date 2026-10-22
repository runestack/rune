#!/usr/bin/env bash
# Build the guest kernel (vmlinux) for the RUNE-322 spike from upstream
# linux-6.1.y with Firecracker's own CI guest config. That config already has
# VMGENID (reseeds the guest RNG on snapshot restore), virtio-balloon with
# free page reporting, vsock, virtio-rng, and what Docker needs (overlayfs,
# cgroups, namespaces, netfilter, bridge, veth).
#
# Usage (any x86_64 Linux box with build tools; the spike box works):
#   FC_VERSION=v1.x.y ./build-kernel.sh [/opt/rune-spike/kernels]
set -euo pipefail

: "${FC_VERSION:?set FC_VERSION to the Firecracker release the config is taken from}"
LINUX_BRANCH="${LINUX_BRANCH:-linux-6.1.y}"
OUT="${1:-/opt/rune-spike/kernels}"
WORK="${WORK:-/var/tmp/rune-spike-kernel}"

export DEBIAN_FRONTEND=noninteractive
apt-get install -yq --no-install-recommends build-essential bc bison flex libelf-dev libssl-dev git curl >/dev/null

mkdir -p "$WORK" "$OUT"
cd "$WORK"
if [[ ! -d linux ]]; then
  git clone --depth 1 --branch "$LINUX_BRANCH" \
    https://git.kernel.org/pub/scm/linux/kernel/git/stable/linux.git linux
fi
raw="https://raw.githubusercontent.com/firecracker-microvm/firecracker/${FC_VERSION}/resources/guest_configs"
curl -fsSL "$raw/microvm-kernel-ci-x86_64-6.1.config" -o fc-6.1.config
curl -fsSL "$raw/ci.config" -o fc-ci.config

cd linux
cp ../fc-6.1.config .config
# Merge Firecracker's CI fragment, then fill any new symbols with defaults.
scripts/kconfig/merge_config.sh -m .config ../fc-ci.config >/dev/null
make olddefconfig >/dev/null
for opt in VMGENID VIRTIO_BALLOON PAGE_REPORTING VIRTIO_VSOCKETS HW_RANDOM_VIRTIO OVERLAY_FS; do
  grep -q "^CONFIG_${opt}=y" .config || { echo "error: CONFIG_${opt} is not =y" >&2; exit 1; }
done
make -j"$(nproc)" vmlinux
install -m 0644 vmlinux "$OUT/vmlinux"
cp .config "$OUT/vmlinux.config"
echo "kernel: $(cat include/config/kernel.release) -> $OUT/vmlinux"
