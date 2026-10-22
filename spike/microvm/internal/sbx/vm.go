package sbx

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/runestack/rune/spike/microvm/internal/agent"
	"github.com/runestack/rune/spike/microvm/internal/fc"
	"github.com/runestack/rune/spike/microvm/internal/proto"
)

// Source is a restorable snapshot: memory, VM state and the disk it was taken
// with. Templates and forks are both Sources.
type Source struct {
	Name    string `json:"name"`
	VMState string `json:"vmstate"`
	Mem     string `json:"mem"`
	Disk    Disk   `json:"disk"`
	VCPU    int    `json:"vcpu"`
	MemMiB  int    `json:"mem_mib"`
	// Host records where the snapshot was taken. Firecracker snapshots only
	// restore on identical CPU model, host kernel and Firecracker version.
	Host map[string]string `json:"host"`
}

func LoadSource(dir string) (*Source, error) {
	var s Source
	if err := readJSON(filepath.Join(dir, "source.json"), &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// VM is one running (or paused) sandbox.
type VM struct {
	ID      string `json:"id"`
	Root    string `json:"root"`
	PID     int    `json:"pid"`
	UffdPID int    `json:"uffd_pid,omitempty"`
	Disk    Disk   `json:"disk"`
	// MemBase is the full memory file this VM's dirty pages are relative to;
	// a Diff snapshot must be rebased onto a copy of it.
	MemBase string `json:"mem_base,omitempty"`
	VCPU    int    `json:"vcpu"`
	MemMiB  int    `json:"mem_mib"`

	h  *Host
	FC *fc.Client `json:"-"`
}

func (h *Host) statePath(id string) string { return filepath.Join(h.jailDir(id), "state.json") }

func (h *Host) LoadVM(id string) (*VM, error) {
	vm := &VM{}
	if err := readJSON(h.statePath(id), vm); err != nil {
		return nil, fmt.Errorf("sandbox %s: %w", id, err)
	}
	vm.h = h
	vm.FC = fc.New(filepath.Join(vm.Root, APISockName))
	return vm, nil
}

func (vm *VM) save() error { return writeJSON(vm.h.statePath(vm.ID), vm) }

// prepare creates an empty sandbox root.
func (h *Host) prepare(id string) (*VM, error) {
	dir := h.jailDir(id)
	if _, err := os.Stat(dir); err == nil {
		return nil, fmt.Errorf("sandbox %s already exists at %s", id, dir)
	}
	root := filepath.Join(dir, "root")
	if err := os.MkdirAll(filepath.Join(root, "run"), 0o755); err != nil {
		return nil, err
	}
	for _, p := range []string{root, filepath.Join(root, "run")} {
		if err := h.chown(p); err != nil {
			return nil, err
		}
	}
	return &VM{ID: id, Root: root, h: h, FC: fc.New(filepath.Join(root, APISockName))}, nil
}

// launch starts Firecracker for vm and waits for its API socket. The process
// gets its own session so it outlives sbxctl; it is reaped in the background
// while this process lives.
func (vm *VM) launch(ctx context.Context) error {
	h := vm.h
	var cmd *exec.Cmd
	if h.UseJailer {
		cmd = exec.Command(h.Jailer,
			"--id", vm.ID,
			"--exec-file", h.Firecracker,
			"--uid", strconv.Itoa(h.UID),
			"--gid", strconv.Itoa(h.GID),
			"--chroot-base-dir", h.JailBase,
			"--cgroup-version", "2",
			"--", "--api-sock", "/"+APISockName, "--id", vm.ID)
	} else {
		cmd = exec.Command(h.Firecracker, "--api-sock", filepath.Join(vm.Root, APISockName), "--id", vm.ID)
		cmd.Dir = vm.Root
	}
	logf, err := os.Create(filepath.Join(filepath.Dir(vm.Root), "firecracker.log"))
	if err != nil {
		return err
	}
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		logf.Close()
		return err
	}
	logf.Close()
	vm.PID = cmd.Process.Pid
	go cmd.Wait()
	if err := vm.save(); err != nil {
		return err
	}
	return vm.FC.WaitReady(ctx)
}

type BootSpec struct {
	Kernel   string
	Disk     Disk // installed as-is (not cloned); the caller owns it
	VCPU     int
	MemMiB   int
	BootArgs string
}

// DefaultBootArgs keep the serial console quiet: console output is slow
// enough to show up in boot latency.
const DefaultBootArgs = "console=ttyS0 quiet loglevel=1 reboot=k panic=1 pci=off root=/dev/vda rw init=/sbin/init"

// Boot cold-boots a VM and waits for the guest agent (E1).
func (h *Host) Boot(ctx context.Context, id string, spec BootSpec) (*VM, Timings, error) {
	var t Timings
	t0 := time.Now()
	vm, err := h.prepare(id)
	if err != nil {
		return nil, nil, err
	}
	vm.Disk, vm.VCPU, vm.MemMiB = spec.Disk, spec.VCPU, spec.MemMiB
	if err := linkOrCopy(spec.Kernel, filepath.Join(vm.Root, KernelName)); err != nil {
		return vm, nil, err
	}
	if err := h.Install(spec.Disk, vm.Root); err != nil {
		return vm, nil, err
	}
	t0 = t.Add("prepare", t0)
	if err := vm.launch(ctx); err != nil {
		return vm, t, err
	}
	t0 = t.Add("spawn", t0)
	args := spec.BootArgs
	if args == "" {
		args = DefaultBootArgs
	}
	steps := []func() error{
		func() error {
			return vm.FC.PutMachineConfig(ctx, fc.MachineConfig{VCPUCount: spec.VCPU, MemSizeMiB: spec.MemMiB, TrackDirtyPages: true})
		},
		func() error {
			return vm.FC.PutBootSource(ctx, fc.BootSource{KernelImagePath: KernelName, BootArgs: args})
		},
		func() error {
			return vm.FC.PutDrive(ctx, fc.Drive{DriveID: "rootfs", PathOnHost: DiskName, IsRootDevice: true})
		},
		func() error { return vm.FC.PutVsock(ctx, fc.Vsock{GuestCID: GuestCID, UDSPath: VsockName}) },
		func() error { return vm.FC.PutEntropy(ctx) },
	}
	for _, s := range steps {
		if err := s(); err != nil {
			return vm, t, err
		}
	}
	t0 = t.Add("configure", t0)
	if err := vm.FC.Start(ctx); err != nil {
		return vm, t, err
	}
	t0 = t.Add("start", t0)
	c, err := vm.Agent(ctx)
	if err != nil {
		return vm, t, err
	}
	defer c.Close()
	if _, err := c.Exec(ctx, "true"); err != nil {
		return vm, t, err
	}
	t.Add("boot_to_ready", t0)
	return vm, t, vm.save()
}

type RestoreOpts struct {
	Backend   string // "File" or "Uffd"
	CloneHook bool   // send the "cloned" identity reset before declaring ready
}

// Restore creates sandbox id from src (E2, E3, fork children).
func (h *Host) Restore(ctx context.Context, id string, src *Source, o RestoreOpts) (*VM, Timings, error) {
	var t Timings
	t0 := time.Now()
	vm, err := h.prepare(id)
	if err != nil {
		return nil, nil, err
	}
	vm.VCPU, vm.MemMiB, vm.MemBase = src.VCPU, src.MemMiB, src.Mem
	if vm.Disk, err = h.CloneDisk(src.Disk, id); err != nil {
		return vm, nil, err
	}
	if err := h.Install(vm.Disk, vm.Root); err != nil {
		return vm, nil, err
	}
	t0 = t.Add("disk", t0)
	if err := linkOrCopy(src.VMState, filepath.Join(vm.Root, VMStateName)); err != nil {
		return vm, t, err
	}
	backend := fc.MemBackend{BackendType: "File", BackendPath: MemName}
	switch o.Backend {
	case "", "File":
		if err := linkOrCopy(src.Mem, filepath.Join(vm.Root, MemName)); err != nil {
			return vm, t, err
		}
	case "Uffd":
		backend = fc.MemBackend{BackendType: "Uffd", BackendPath: UffdName}
		if err := vm.startUffd(ctx, src.Mem); err != nil {
			return vm, t, err
		}
	default:
		return vm, t, fmt.Errorf("unknown memory backend %q", o.Backend)
	}
	t0 = t.Add("link", t0)
	if err := vm.launch(ctx); err != nil {
		return vm, t, err
	}
	t0 = t.Add("spawn", t0)
	if err := vm.FC.LoadSnapshot(ctx, fc.SnapshotLoad{
		SnapshotPath:    VMStateName,
		MemBackend:      backend,
		TrackDirtyPages: true,
		ResumeVM:        true,
	}); err != nil {
		return vm, t, err
	}
	t0 = t.Add("load", t0)
	c, err := vm.Agent(ctx)
	if err != nil {
		return vm, t, err
	}
	defer c.Close()
	t0 = t.Add("agent", t0)
	if o.CloneHook {
		if err := vm.CloneHook(ctx, c); err != nil {
			return vm, t, err
		}
		t0 = t.Add("clone_hook", t0)
	}
	if _, err := c.Exec(ctx, "true"); err != nil {
		return vm, t, err
	}
	t.Add("first_exec", t0)
	return vm, t, vm.save()
}

func (vm *VM) startUffd(ctx context.Context, mem string) error {
	sock := filepath.Join(vm.Root, UffdName)
	args := []string{"-sock", sock, "-mem", mem, "-stats", filepath.Join(filepath.Dir(vm.Root), "uffd-stats.json")}
	if vm.h.UseJailer {
		args = append(args, "-owner", vm.h.owner())
	}
	cmd := exec.Command(vm.h.UffdHandler, args...)
	logf, err := os.Create(filepath.Join(filepath.Dir(vm.Root), "uffd.log"))
	if err != nil {
		return err
	}
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		logf.Close()
		return err
	}
	logf.Close()
	vm.UffdPID = cmd.Process.Pid
	go cmd.Wait()
	// The handler must be listening before Firecracker's /snapshot/load connects.
	for {
		if fi, err := os.Stat(sock); err == nil && fi.Mode()&os.ModeSocket != 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("uffd handler socket: %w", ctx.Err())
		case <-time.After(200 * time.Microsecond):
		}
	}
}

// Agent waits for the guest agent and returns a connection to it.
func (vm *VM) Agent(ctx context.Context) (*agent.Client, error) {
	return agent.WaitDial(ctx, filepath.Join(vm.Root, VsockName), proto.AgentPort)
}

// CloneHook gives a restored sibling its own identity and fresh entropy.
func (vm *VM) CloneHook(ctx context.Context, c *agent.Client) error {
	ent := make([]byte, 64)
	if _, err := rand.Read(ent); err != nil {
		return err
	}
	_, err := c.Call(ctx, &proto.Request{Op: proto.OpCloned, Clone: &proto.CloneArgs{
		Hostname:   "sbx-" + vm.ID,
		Entropy:    ent,
		UnixNanos:  time.Now().UnixNano(),
		RegenSSH:   true,
		NewMachine: true,
	}})
	return err
}

func (vm *VM) Pause(ctx context.Context) error  { return vm.FC.Pause(ctx) }
func (vm *VM) Resume(ctx context.Context) error { return vm.FC.Resume(ctx) }

// Snapshot pauses vm and writes a Full snapshot into dir, leaving the VM
// paused. The disk is snapshotted at the same moment, so the result is a
// complete Source. Diff snapshots (only dirty pages, then rebased onto a copy
// of MemBase with snapshot-editor) are the next step for E6; Full is the
// simple, always-correct baseline.
func (vm *VM) Snapshot(ctx context.Context, dir, name string) (*Source, Timings, error) {
	var t Timings
	t0 := time.Now()
	if err := vm.Pause(ctx); err != nil {
		return nil, nil, err
	}
	t0 = t.Add("pause", t0)
	if err := vm.FC.CreateSnapshot(ctx, fc.SnapshotCreate{
		SnapshotType: "Full", SnapshotPath: "snap-" + VMStateName, MemFilePath: "snap-" + MemName,
	}); err != nil {
		return nil, t, err
	}
	t0 = t.Add("snapshot", t0)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, t, err
	}
	src := &Source{
		Name: name, VMState: filepath.Join(dir, VMStateName), Mem: filepath.Join(dir, MemName),
		VCPU: vm.VCPU, MemMiB: vm.MemMiB, Host: Fingerprint(vm.h),
	}
	for _, p := range [][2]string{{"snap-" + VMStateName, src.VMState}, {"snap-" + MemName, src.Mem}} {
		if err := os.Rename(filepath.Join(vm.Root, p[0]), p[1]); err != nil {
			return nil, t, err
		}
		// Read-only and owned by the jailer user, so every clone's
		// Firecracker can open the shared inode and none can change it.
		if err := os.Chmod(p[1], 0o440); err != nil {
			return nil, t, err
		}
		if err := vm.h.chown(p[1]); err != nil {
			return nil, t, err
		}
	}
	var err error
	if src.Disk, err = vm.h.CloneDisk(vm.Disk, "snap-"+name); err != nil {
		return nil, t, err
	}
	t.Add("persist", t0)
	return src, t, writeJSON(filepath.Join(dir, "source.json"), src)
}

// Destroy kills the VM and its UFFD handler and removes its disk and root.
func (vm *VM) Destroy() error {
	var errs []error
	for _, pid := range []int{vm.PID, vm.UffdPID} {
		if pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
	// Wait for Firecracker to exit so the thin volume is no longer open.
	for i := 0; vm.PID > 0 && i < 200; i++ {
		if err := syscall.Kill(vm.PID, 0); errors.Is(err, syscall.ESRCH) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	errs = append(errs, vm.h.RemoveDisk(vm.Disk))
	errs = append(errs, os.RemoveAll(filepath.Dir(vm.Root)))
	if vm.h.UseJailer {
		_ = os.Remove(filepath.Join("/sys/fs/cgroup", filepath.Base(vm.h.Firecracker), vm.ID))
	}
	return errors.Join(errs...)
}

// Fingerprint is what must match for a snapshot to restore on a host.
func Fingerprint(h *Host) map[string]string {
	fp := map[string]string{}
	if b, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		fp["kernel"] = strings.TrimSpace(string(b))
	}
	if out, err := exec.Command(h.Firecracker, "--version").Output(); err == nil {
		fp["firecracker"] = strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	}
	if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if k, v, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(k) == "model name" {
				fp["cpu"] = strings.TrimSpace(v)
				break
			}
		}
	}
	return fp
}
