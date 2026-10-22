# RUNE-322 Phase 0: microVM spike

Throwaway code for the bare-metal spike in `SANDBOX_SPIKE_PLAN.md` (runestack/rfcs).
It exists to measure four claims of RUNE-322 on real hardware. **It is never
merged.** Phase 1 rewrites whatever survives behind the real `Runner`
interface; the harness and its raw results are what we keep.

This is a separate Go module so it never touches the main build.

## Status

| Part | State |
|---|---|
| `cmd/sbx-agent` guest agent (vsock, exec, files, clone hook, identity, meminfo) | Written. Unit tests pass on Linux; never run inside a microVM |
| `cmd/uffd-handler` (Firecracker `Uffd` memory backend) | Written. Test drives a **real userfaultfd** end to end (copy faults, balloon REMOVE → zero page) and passes; never attached to Firecracker |
| `internal/sbx` lifecycle: jailer, dm-thin or reflink disks, cold boot, restore, Full snapshot, fork, destroy | Written; compiles; **not run** (no KVM where it was written) |
| `cmd/bench` E1 (cold boot), E2 (restore, File vs Uffd), E3 (clone uniqueness) | Written; not run |
| `scripts/` host setup, guest kernel, base/desktop rootfs | Written; `bash -n` clean; not run |
| E4–E13 | Not written yet. Network (tap + nftables, E11), balloon (E8), Diff-snapshot fork (E6), Selkies (E12) come next |

Expect the first session on the build box to be debugging, not measuring.

## Layout

```
cmd/sbx-agent     guest agent, static binary, systemd service in the image
cmd/uffd-handler  one process per VM for the Uffd memory backend
cmd/sbxctl        drive one sandbox by hand
cmd/bench         E1, E2, E3 -> CSV + p50/p95/p99 summary
internal/proto    framed JSON protocol (draft of RUNE-322 §4.1)
internal/fc       Firecracker API client
internal/agent    host side of the agent protocol over Firecracker vsock
internal/sbx      sandbox lifecycle on one host
scripts/          host-setup.sh, build-kernel.sh, build-rootfs.sh
```

## How restore works here

Each sandbox gets a root directory: the jailer chroot, or Firecracker's working
directory with `-jailer=false`. Every path Firecracker sees is relative to it:
`vmlinux`, `rootfs`, `v.sock`, `vmstate`, `mem`. So the paths baked into a
snapshot resolve to each clone's own disk and vsock socket, with no overrides.

- **Memory, File backend**: `mem` is a hardlink to the template's memory file.
  Firecracker maps it `MAP_PRIVATE`, so clean pages are shared through the
  page cache across every clone on the host. This is where RAM sharing (E7)
  comes from.
- **Memory, Uffd backend**: `uffd-handler` serves faults with `UFFDIO_COPY`.
  Every clone gets private copies of the pages it touches, so there is **no
  sharing** with this backend. It is for lazy and remote loading (E5).
- **Disk**: `thin` makes a dm-thin snapshot of the template volume and
  `mknod`s it into the root; `copy` uses `cp --reflink=auto` (instant on XFS).
- **Clone hook**: right after restore the host sends `cloned` with 64 bytes
  of fresh entropy, the wall clock, a new hostname, and asks for a new
  machine-id and SSH host keys. The guest kernel (≥5.18, `CONFIG_VMGENID`)
  also reseeds its RNG on restore. E3 checks the result.

Snapshots only restore on the same CPU model, host kernel and Firecracker
version. Each template records a fingerprint and `sbxctl restore` warns on a
mismatch. A cell must be homogeneous hardware.

## Runbook (build box)

As root on a fresh Ubuntu 24.04 bare-metal box, with the OS on the first NVMe
and the second left empty:

```sh
git clone -b spike/rune-322-microvm https://github.com/runestack/rune && cd rune/spike/microvm
FC_VERSION=v1.x.y THIN_DEV=/dev/nvme1n1 scripts/host-setup.sh
CGO_ENABLED=0 go build -o /opt/rune-spike/bin/ ./cmd/...
go test ./...                       # the uffd test needs root

FC_VERSION=v1.x.y scripts/build-kernel.sh
AGENT=/opt/rune-spike/bin/sbx-agent scripts/build-rootfs.sh base /opt/rune-spike/images/base.ext4 8G
AGENT=/opt/rune-spike/bin/sbx-agent scripts/build-rootfs.sh desktop /opt/rune-spike/images/desktop.ext4 12G

export PATH=/opt/rune-spike/bin:$PATH
sbxctl template -name base -image /opt/rune-spike/images/base.ext4 -vcpu 2 -mem 2048
sbxctl template -name desktop -image /opt/rune-spike/images/desktop.ext4 -vcpu 4 -mem 8192 \
  -warm 'until curl -s localhost:9222/json/version; do sleep 0.2; done' -settle 10s

# Smoke test by hand
sbxctl restore -id t1 -from base
sbxctl exec -id t1 -- uname -a
sbxctl identity -id t1
sbxctl fork -id t1 -n 3
sbxctl ls; sbxctl rm t1 t1-f0 t1-f1 t1-f2

# Checkpoint first: E3 must pass before anything else is measured
bench e3 -template base -n 100
bench e3 -template base -n 100 -hook=false   # control: shows what the hook fixes
bench e1 -template base -n 200
bench e2 -template desktop -n 200
```

If the jailer gets in the way while debugging, add `-jailer=false` to `sbxctl`
and `bench` to run Firecracker directly as root. Numbers for `RESULTS.md` come
from jailed runs only.

Results land in `/opt/rune-spike/results/<exp>-<template>-<time>/`. Copy them
off the box before deleting it.
