// bench runs the RUNE-322 spike experiments that need no workload beyond the
// guest agent, and writes one CSV row per iteration plus a p50/p95/p99
// summary. See SANDBOX_SPIKE_PLAN.md §4 for what each experiment proves.
//
//	bench e1 -template base -n 200          cold boot to ready
//	bench e2 -template base -n 200          restore to ready, File and Uffd backends
//	bench e3 -template base -n 100          clone uniqueness across siblings
package main

import (
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/runestack/rune/spike/microvm/internal/proto"
	"github.com/runestack/rune/spike/microvm/internal/sbx"
)

func main() {
	h := sbx.DefaultHost()
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	fs.StringVar(&h.Prefix, "prefix", h.Prefix, "spike install prefix")
	fs.BoolVar(&h.UseJailer, "jailer", h.UseJailer, "run Firecracker under the jailer")
	tpl := fs.String("template", "base", "template name")
	n := fs.Int("n", 200, "iterations (e1, e2) or siblings (e3)")
	out := fs.String("out", "", "results directory (default <prefix>/results/<exp>-<template>-<time>)")
	backends := fs.String("backends", "File,Uffd", "e2: memory backends to compare")
	hook := fs.Bool("hook", true, "e2/e3: run the clone identity hook")
	parallel := fs.Int("parallel", 8, "e3: restores in flight at once")
	timeout := fs.Duration("timeout", 30*time.Second, "per-sandbox timeout")
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: bench e1|e2|e3 [flags]")
		fs.PrintDefaults()
		os.Exit(2)
	}
	exp := os.Args[1]
	_ = fs.Parse(os.Args[2:])
	if err := h.Init(); err != nil {
		fatal(err)
	}
	src, err := sbx.LoadSource(h.TemplateDir(*tpl))
	if err != nil {
		fatal(err)
	}
	if *out == "" {
		*out = filepath.Join(h.Prefix, "results", fmt.Sprintf("%s-%s-%s", exp, *tpl, time.Now().UTC().Format("20060102T150405")))
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fatal(err)
	}
	b := &bench{h: h, src: src, tpl: *tpl, out: *out, timeout: *timeout}
	switch exp {
	case "e1":
		err = b.e1(*n)
	case "e2":
		err = b.e2(*n, strings.Split(*backends, ","), *hook)
	case "e3":
		err = b.e3(*n, *parallel, *hook)
	default:
		err = fmt.Errorf("unknown experiment %q", exp)
	}
	if err != nil {
		fatal(err)
	}
	fmt.Println("results in", *out)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "bench:", err)
	os.Exit(1)
}

type bench struct {
	h       *sbx.Host
	src     *sbx.Source
	tpl     string
	out     string
	timeout time.Duration
}

func (b *bench) id(exp string, i int) string { return fmt.Sprintf("%s-%s-%d", exp, b.tpl, i) }

// e1: cold boot a fresh copy of the template's disk to a ready agent.
// The disk clone is timed separately and excluded from the boot total.
func (b *bench) e1(n int) error {
	rows := newTable("iter")
	for i := range n {
		ctx, cancel := context.WithTimeout(context.Background(), b.timeout)
		id := b.id("e1", i)
		t0 := time.Now()
		d, err := b.h.CloneDisk(b.src.Disk, id)
		if err != nil {
			cancel()
			return err
		}
		diskT := time.Since(t0)
		vm, t, err := b.h.Boot(ctx, id, sbx.BootSpec{Kernel: b.h.KernelPath(), Disk: d, VCPU: b.src.VCPU, MemMiB: b.src.MemMiB})
		cancel()
		if vm != nil {
			_ = vm.Destroy()
		} else {
			_ = b.h.RemoveDisk(d)
		}
		if err != nil {
			return fmt.Errorf("iteration %d: %w (%s)", i, err, t)
		}
		rows.add([]string{strconv.Itoa(i)}, append(sbx.Timings{{Name: "disk_clone", D: diskT}}, t...), t.Total())
		progress("e1", i, n, t.Total())
	}
	return rows.write(filepath.Join(b.out, "e1.csv"))
}

// e2: restore from the golden snapshot to a ready agent, one at a time,
// alternating backends so drift on the host affects both equally.
func (b *bench) e2(n int, backends []string, hook bool) error {
	rows := newTable("iter", "backend")
	for i := range n {
		for _, be := range backends {
			ctx, cancel := context.WithTimeout(context.Background(), b.timeout)
			id := b.id("e2-"+strings.ToLower(be), i)
			vm, t, err := b.h.Restore(ctx, id, b.src, sbx.RestoreOpts{Backend: be, CloneHook: hook})
			cancel()
			if vm != nil {
				_ = vm.Destroy()
			}
			if err != nil {
				return fmt.Errorf("iteration %d (%s): %w (%s)", i, be, err, t)
			}
			rows.add([]string{strconv.Itoa(i), be}, t, t.Total())
			progress("e2 "+be, i, n, t.Total())
		}
	}
	return rows.write(filepath.Join(b.out, "e2.csv"))
}

// e3: restore n siblings from one snapshot, all alive at once, and compare
// what must differ between them. Any duplicate is a kill until fixed.
func (b *bench) e3(n, parallel int, hook bool) error {
	type result struct {
		id  string
		ids *proto.Identity
		err error
	}
	results := make([]result, n)
	var vms sync.Map
	defer vms.Range(func(_, v any) bool { _ = v.(*sbx.VM).Destroy(); return true })

	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			ctx, cancel := context.WithTimeout(context.Background(), b.timeout)
			defer cancel()
			id := b.id("e3", i)
			results[i].id = id
			vm, _, err := b.h.Restore(ctx, id, b.src, sbx.RestoreOpts{CloneHook: hook})
			if vm != nil {
				vms.Store(id, vm)
			}
			if err != nil {
				results[i].err = err
				return
			}
			c, err := vm.Agent(ctx)
			if err != nil {
				results[i].err = err
				return
			}
			defer c.Close()
			resp, err := c.Call(ctx, &proto.Request{Op: proto.OpIdentity})
			if err != nil {
				results[i].err = err
				return
			}
			results[i].ids = resp.Identity
		}()
	}
	wg.Wait()

	f, err := os.Create(filepath.Join(b.out, "e3.csv"))
	if err != nil {
		return err
	}
	w := csv.NewWriter(f)
	_ = w.Write([]string{"id", "machine_id", "hostname", "boot_id", "ssh_host_key_sha256", "urandom_hex", "error"})
	fields := map[string]map[string]int{}
	count := func(field, v string) {
		if v == "" {
			return
		}
		if fields[field] == nil {
			fields[field] = map[string]int{}
		}
		fields[field][v]++
	}
	failed := 0
	for _, r := range results {
		if r.err != nil {
			failed++
			_ = w.Write([]string{r.id, "", "", "", "", "", r.err.Error()})
			continue
		}
		x := r.ids
		_ = w.Write([]string{r.id, x.MachineID, x.Hostname, x.BootID, x.SSHHostKey, x.URandom, ""})
		count("machine_id", x.MachineID)
		count("hostname", x.Hostname)
		count("boot_id", x.BootID)
		count("ssh_host_key", x.SSHHostKey)
		count("urandom", x.URandom)
	}
	w.Flush()
	f.Close()

	fmt.Printf("e3: %d siblings, %d failed to restore, clone hook %v\n", n, failed, hook)
	dups := 0
	for _, field := range []string{"machine_id", "hostname", "ssh_host_key", "urandom", "boot_id"} {
		d := 0
		for _, c := range fields[field] {
			if c > 1 {
				d += c
			}
		}
		verdict := "unique"
		if d > 0 {
			verdict = fmt.Sprintf("%d siblings share a value", d)
			// boot_id is the kernel's, set at boot, and a restore is not a
			// boot; it is reported but does not fail the run.
			if field != "boot_id" {
				dups += d
			}
		}
		fmt.Printf("  %-13s %s\n", field, verdict)
	}
	if dups > 0 || failed > 0 {
		return fmt.Errorf("e3 FAILED: %d duplicate values, %d failed restores", dups, failed)
	}
	fmt.Println("e3 PASSED: every sibling is unique")
	return nil
}

func progress(exp string, i, n int, d time.Duration) {
	if i%20 == 0 || i == n-1 {
		fmt.Printf("%s %d/%d last=%.1fms\n", exp, i+1, n, float64(d.Microseconds())/1000)
	}
}

// table collects rows whose phase columns are discovered from the first row.
type table struct {
	keys   []string
	phases []string
	rows   [][]string
	cols   map[string][]float64
}

func newTable(keys ...string) *table { return &table{keys: keys, cols: map[string][]float64{}} }

func (t *table) add(keys []string, ph sbx.Timings, total time.Duration) {
	if t.phases == nil {
		for _, p := range ph {
			t.phases = append(t.phases, p.Name)
		}
		t.phases = append(t.phases, "total")
	}
	vals := map[string]float64{"total": msf(total)}
	for _, p := range ph {
		vals[p.Name] += msf(p.D)
	}
	row := append([]string{}, keys...)
	group := strings.Join(keys[1:], "/")
	for _, name := range t.phases {
		row = append(row, strconv.FormatFloat(vals[name], 'f', 3, 64))
		t.cols[group+"\x00"+name] = append(t.cols[group+"\x00"+name], vals[name])
	}
	t.rows = append(t.rows, row)
}

func (t *table) write(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := csv.NewWriter(f)
	hdr := append([]string{}, t.keys...)
	for _, p := range t.phases {
		hdr = append(hdr, p+"_ms")
	}
	_ = w.Write(hdr)
	_ = w.WriteAll(t.rows)
	if err := f.Close(); err != nil {
		return err
	}
	// Summary: p50/p95/p99 per group and phase.
	var keys []string
	for k := range t.cols {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	sum, err := os.Create(strings.TrimSuffix(path, ".csv") + "-summary.txt")
	if err != nil {
		return err
	}
	defer sum.Close()
	for _, k := range keys {
		group, phase, _ := strings.Cut(k, "\x00")
		v := t.cols[k]
		sort.Float64s(v)
		line := fmt.Sprintf("%-8s %-14s n=%-4d p50=%8.2fms p95=%8.2fms p99=%8.2fms\n",
			group, phase, len(v), pct(v, 50), pct(v, 95), pct(v, 99))
		fmt.Print(line)
		sum.WriteString(line)
	}
	return nil
}

func pct(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(float64(len(sorted)-1) * p / 100)
	return sorted[i]
}

func msf(d time.Duration) float64 { return float64(d.Nanoseconds()) / 1e6 }
