// Package sbx is the spike's sandbox lifecycle on one host: lay out a
// per-sandbox chroot, start Firecracker (under the jailer by default), boot
// cold or restore from a snapshot, fork, and tear down. Phase 1 replaces all
// of this with a real Runner; this exists to get honest numbers.
//
// Every path Firecracker sees is relative to the sandbox root ("vmlinux",
// "rootfs", "v.sock", "vmstate", "mem"). Under the jailer the root is the
// chroot; without it, the root is Firecracker's working directory. Because
// the paths baked into a snapshot are relative, every clone of one snapshot
// finds its own disk and vsock socket in its own root, with no overrides.
package sbx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Names inside a sandbox root.
const (
	KernelName  = "vmlinux"
	DiskName    = "rootfs"
	VsockName   = "v.sock"
	APISockName = "run/firecracker.socket"
	VMStateName = "vmstate"
	MemName     = "mem"
	UffdName    = "uffd.sock"
	GuestCID    = 3
)

type Host struct {
	Prefix      string // /opt/rune-spike
	JailBase    string // jailer --chroot-base-dir
	Firecracker string
	Jailer      string
	UffdHandler string
	UseJailer   bool
	User        string // unprivileged user Firecracker runs as under the jailer
	UID, GID    int
	DiskMode    string // "thin" (dm-thin snapshots) or "copy" (cp --reflink=auto)
	VG, Pool    string
}

func DefaultHost() *Host {
	p := "/opt/rune-spike"
	return &Host{
		Prefix:      p,
		JailBase:    "/srv/jailer",
		Firecracker: filepath.Join(p, "bin/firecracker"),
		Jailer:      filepath.Join(p, "bin/jailer"),
		UffdHandler: filepath.Join(p, "bin/uffd-handler"),
		UseJailer:   true,
		User:        "fcjail",
		DiskMode:    "thin",
		VG:          "spike",
		Pool:        "thinpool",
	}
}

// Init resolves the jailer user. Call after flags are parsed.
func (h *Host) Init() error {
	if h.DiskMode != "thin" && h.DiskMode != "copy" {
		return fmt.Errorf("disk mode %q: want thin or copy", h.DiskMode)
	}
	if !h.UseJailer {
		return nil
	}
	u, err := user.Lookup(h.User)
	if err != nil {
		return fmt.Errorf("jailer user: %w (run host-setup.sh)", err)
	}
	h.UID, _ = strconv.Atoi(u.Uid)
	h.GID, _ = strconv.Atoi(u.Gid)
	return nil
}

func (h *Host) TemplateDir(name string) string { return filepath.Join(h.Prefix, "templates", name) }
func (h *Host) KernelPath() string             { return filepath.Join(h.Prefix, "kernels", "vmlinux") }

// jailDir is the jailer's per-VM directory; the chroot is its "root".
func (h *Host) jailDir(id string) string {
	return filepath.Join(h.JailBase, filepath.Base(h.Firecracker), id)
}

func (h *Host) owner() string { return fmt.Sprintf("%d:%d", h.UID, h.GID) }

func (h *Host) chown(path string) error {
	if !h.UseJailer {
		return nil
	}
	return os.Lchown(path, h.UID, h.GID)
}

// Phase is one timed step; Timings keeps them in order for CSV output.
type Phase struct {
	Name string
	D    time.Duration
}

type Timings []Phase

func (t *Timings) Add(name string, since time.Time) time.Time {
	now := time.Now()
	*t = append(*t, Phase{name, now.Sub(since)})
	return now
}

func (t Timings) Total() time.Duration {
	var d time.Duration
	for _, p := range t {
		d += p.D
	}
	return d
}

func (t Timings) String() string {
	var b strings.Builder
	for _, p := range t {
		fmt.Fprintf(&b, "%s=%.2fms ", p.Name, ms(p.D))
	}
	fmt.Fprintf(&b, "total=%.2fms", ms(t.Total()))
	return b.String()
}

func ms(d time.Duration) float64 { return float64(d.Nanoseconds()) / 1e6 }

// run executes a host tool and folds its output into the error.
func run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// linkOrCopy hardlinks src to dst so clones share one inode (and so one set
// of page-cache pages); it copies only when they are on different filesystems.
func linkOrCopy(src, dst string) error {
	err := os.Link(src, dst)
	if err == nil || !errors.Is(err, syscall.EXDEV) {
		return err
	}
	fmt.Fprintf(os.Stderr, "warning: %s and %s are on different filesystems; copying (no page sharing)\n", src, dst)
	return copyFile(src, dst)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
