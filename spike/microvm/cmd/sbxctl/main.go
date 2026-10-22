// sbxctl drives single sandboxes on a spike box by hand: build a template,
// cold boot, restore, exec, pause/resume, fork and remove. The benchmark
// harness (cmd/bench) uses the same internal/sbx code in loops.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/runestack/rune/spike/microvm/internal/proto"
	"github.com/runestack/rune/spike/microvm/internal/sbx"
)

const usage = `usage: sbxctl [global flags] <command> [flags]

commands:
  template  -name N -image rootfs.ext4 [-vcpu 2 -mem 2048 -settle 5s -warm CMD]
            cold boot the image, wait for the agent, snapshot it as template N
  boot      -id X -from N                  cold boot a copy of template N's disk
  restore   -id X -from N|DIR [-backend File|Uffd] [-hook=true]
  exec      -id X -- argv...
  identity  -id X                          print what E3 compares
  pause     -id X
  resume    -id X
  fork      -id X -n 1                     snapshot X and restore N children
  rm        X [Y...]
  ls

global flags:
`

func main() {
	h := sbx.DefaultHost()
	global := flag.NewFlagSet("sbxctl", flag.ExitOnError)
	global.StringVar(&h.Prefix, "prefix", h.Prefix, "spike install prefix")
	global.BoolVar(&h.UseJailer, "jailer", h.UseJailer, "run Firecracker under the jailer")
	global.StringVar(&h.DiskMode, "disk", h.DiskMode, "disk mode for new templates: thin or copy")
	timeout := global.Duration("timeout", 60*time.Second, "overall timeout")
	global.Usage = func() { fmt.Fprint(os.Stderr, usage); global.PrintDefaults() }
	_ = global.Parse(os.Args[1:])
	if global.NArg() == 0 {
		global.Usage()
		os.Exit(2)
	}
	if err := h.Init(); err != nil {
		fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	cmd, args := global.Arg(0), global.Args()[1:]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	id := fs.String("id", "", "sandbox id")
	var err error
	switch cmd {
	case "template":
		name := fs.String("name", "", "template name")
		image := fs.String("image", "", "ext4 root filesystem image (scripts/build-rootfs.sh)")
		kernel := fs.String("kernel", h.KernelPath(), "guest kernel (vmlinux)")
		vcpu := fs.Int("vcpu", 2, "vCPUs")
		mem := fs.Int("mem", 2048, "memory MiB")
		bootArgs := fs.String("boot-args", sbx.DefaultBootArgs, "kernel command line")
		settle := fs.Duration("settle", 5*time.Second, "idle time after the warm command, before the snapshot")
		warm := fs.String("warm", "", "shell command to run in the guest before snapshotting (e.g. start Chrome)")
		_ = fs.Parse(args)
		err = buildTemplate(ctx, h, *name, *image, sbx.BootSpec{Kernel: *kernel, VCPU: *vcpu, MemMiB: *mem, BootArgs: *bootArgs}, *warm, *settle)
	case "boot":
		from := fs.String("from", "", "template name")
		kernel := fs.String("kernel", h.KernelPath(), "guest kernel")
		_ = fs.Parse(args)
		var src *sbx.Source
		if src, err = findSource(h, *from); err != nil {
			break
		}
		var d sbx.Disk
		if d, err = h.CloneDisk(src.Disk, *id); err != nil {
			break
		}
		var t sbx.Timings
		_, t, err = h.Boot(ctx, *id, sbx.BootSpec{Kernel: *kernel, Disk: d, VCPU: src.VCPU, MemMiB: src.MemMiB})
		fmt.Println(t)
	case "restore":
		from := fs.String("from", "", "template name or source directory")
		backend := fs.String("backend", "File", "memory backend: File or Uffd")
		hook := fs.Bool("hook", true, "run the clone identity hook")
		_ = fs.Parse(args)
		var src *sbx.Source
		if src, err = findSource(h, *from); err != nil {
			break
		}
		warnFingerprint(h, src)
		var t sbx.Timings
		_, t, err = h.Restore(ctx, *id, src, sbx.RestoreOpts{Backend: *backend, CloneHook: *hook})
		fmt.Println(t)
	case "exec":
		_ = fs.Parse(args)
		err = execCmd(ctx, h, *id, fs.Args())
	case "identity":
		_ = fs.Parse(args)
		err = identity(ctx, h, *id)
	case "pause", "resume":
		_ = fs.Parse(args)
		var vm *sbx.VM
		if vm, err = h.LoadVM(*id); err != nil {
			break
		}
		start := time.Now()
		if cmd == "pause" {
			err = vm.Pause(ctx)
		} else {
			err = vm.Resume(ctx)
		}
		fmt.Printf("%s: %.2fms\n", cmd, float64(time.Since(start).Microseconds())/1000)
	case "fork":
		n := fs.Int("n", 1, "children")
		backend := fs.String("backend", "File", "memory backend for children")
		_ = fs.Parse(args)
		err = fork(ctx, h, *id, *n, *backend)
	case "rm":
		_ = fs.Parse(args)
		for _, x := range fs.Args() {
			vm, e := h.LoadVM(x)
			if e == nil {
				e = vm.Destroy()
			}
			if e != nil {
				fmt.Fprintf(os.Stderr, "rm %s: %v\n", x, e)
				err = fmt.Errorf("some sandboxes were not removed")
			}
		}
	case "ls":
		dirs, _ := filepath.Glob(filepath.Join(h.JailBase, filepath.Base(h.Firecracker), "*", "state.json"))
		sort.Strings(dirs)
		for _, d := range dirs {
			fmt.Println(filepath.Base(filepath.Dir(d)))
		}
	default:
		global.Usage()
		os.Exit(2)
	}
	if err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "sbxctl:", err)
	os.Exit(1)
}

func findSource(h *sbx.Host, from string) (*sbx.Source, error) {
	if from == "" {
		return nil, fmt.Errorf("-from is required")
	}
	dir := from
	if !strings.Contains(from, "/") {
		dir = h.TemplateDir(from)
	}
	return sbx.LoadSource(dir)
}

func warnFingerprint(h *sbx.Host, src *sbx.Source) {
	here := sbx.Fingerprint(h)
	for k, v := range src.Host {
		if here[k] != v {
			fmt.Fprintf(os.Stderr, "warning: snapshot %s was taken with %s %q, this host has %q; restore may fail\n", src.Name, k, v, here[k])
		}
	}
}

func buildTemplate(ctx context.Context, h *sbx.Host, name, image string, spec sbx.BootSpec, warm string, settle time.Duration) error {
	if name == "" || image == "" {
		return fmt.Errorf("-name and -image are required")
	}
	dir := h.TemplateDir(name)
	if _, err := os.Stat(filepath.Join(dir, "source.json")); err == nil {
		return fmt.Errorf("template %s already exists in %s", name, dir)
	}
	d, err := h.ImportImage(image, name)
	if err != nil {
		return err
	}
	spec.Disk = d
	vm, t, err := h.Boot(ctx, "tplbuild-"+name, spec)
	fmt.Println("boot:", t)
	if vm != nil {
		defer vm.Destroy()
	}
	if err != nil {
		return err
	}
	c, err := vm.Agent(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	if warm != "" {
		res, err := c.Exec(ctx, "/bin/sh", "-c", warm)
		if err != nil {
			return err
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("warm command exited %d: %s", res.ExitCode, res.Stderr)
		}
	}
	time.Sleep(settle)
	// Flush the guest page cache so the disk snapshot matches what memory believes.
	if _, err := c.Exec(ctx, "sync"); err != nil {
		return err
	}
	src, st, err := vm.Snapshot(ctx, dir, name)
	fmt.Println("snapshot:", st)
	if err != nil {
		return err
	}
	fmt.Printf("template %s: vmstate=%s mem=%s disk=%+v\n", name, src.VMState, src.Mem, src.Disk)
	return nil
}

func execCmd(ctx context.Context, h *sbx.Host, id string, argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("exec: no command")
	}
	vm, err := h.LoadVM(id)
	if err != nil {
		return err
	}
	c, err := vm.Agent(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	res, err := c.Exec(ctx, argv...)
	if err != nil {
		return err
	}
	os.Stdout.Write(res.Stdout)
	os.Stderr.Write(res.Stderr)
	if res.ExitCode != 0 {
		os.Exit(res.ExitCode)
	}
	return nil
}

func identity(ctx context.Context, h *sbx.Host, id string) error {
	vm, err := h.LoadVM(id)
	if err != nil {
		return err
	}
	c, err := vm.Agent(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	resp, err := c.Call(ctx, &proto.Request{Op: proto.OpIdentity})
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(resp.Identity, "", "  ")
	fmt.Println(string(b))
	return nil
}

// fork snapshots a running sandbox (pausing it only for the snapshot), then
// restores n children from that snapshot in parallel.
func fork(ctx context.Context, h *sbx.Host, id string, n int, backend string) error {
	parent, err := h.LoadVM(id)
	if err != nil {
		return err
	}
	stamp := time.Now().UTC().Format("20060102T150405")
	name := id + "-" + stamp
	start := time.Now()
	src, t, err := parent.Snapshot(ctx, filepath.Join(h.Prefix, "forks", name), name)
	if err != nil {
		return err
	}
	if err := parent.Resume(ctx); err != nil {
		return err
	}
	fmt.Printf("parent paused %.2fms (%s)\n", float64(time.Since(start).Microseconds())/1000, t)
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cid := fmt.Sprintf("%s-f%d", id, i)
			_, ct, err := h.Restore(ctx, cid, src, sbx.RestoreOpts{Backend: backend, CloneHook: true})
			errs[i] = err
			fmt.Printf("child %s: %s\n", cid, ct)
		}()
	}
	wg.Wait()
	fmt.Printf("fork x%d: %.2fms total\n", n, float64(time.Since(start).Microseconds())/1000)
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}
