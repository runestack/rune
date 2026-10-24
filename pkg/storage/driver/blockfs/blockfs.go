// Package blockfs decides whether a block device needs a filesystem
// before the cloud volume drivers mount it.
//
// Formatting is the one step in a mount that destroys data, so it must
// only happen on positive evidence that the disk is empty. mkfs.ext4 is
// no safety net: mke2fs looks for an existing filesystem only when
// stdin and stdout are terminals, so run from runed it formats over a
// populated disk without asking. Any failure to *read* the disk is
// therefore an error, never "unformatted".
package blockfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// probeResult is what blkid's low-level probe saw on the device.
type probeResult struct {
	stdout, stderr []byte
	exitCode       int // -1 when blkid did not run to completion
	err            error
}

// probeFunc runs the probe; replaced in tests.
var probeFunc = func(ctx context.Context, dev string) probeResult {
	// -p reads the device itself rather than the udev database, which
	// can be empty for a disk attached moments ago.
	cmd := exec.CommandContext(ctx, "blkid", "-p", "-o", "export", dev)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	r := probeResult{stdout: stdout.Bytes(), stderr: stderr.Bytes(), exitCode: -1, err: err}
	var ee *exec.ExitError
	switch {
	case err == nil:
		r.exitCode = 0
	case errors.As(err, &ee):
		r.exitCode = ee.ExitCode()
	}
	return r
}

// statFunc checks the device node; replaced in tests.
var statFunc = func(dev string) error {
	fi, err := os.Stat(dev)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeDevice == 0 {
		return fmt.Errorf("%s is not a device", dev)
	}
	return nil
}

// zeroFunc reports whether the device's first and last zeroSpan bytes
// are all zero; replaced in tests.
var zeroFunc = edgesZero

// zeroSpan covers every superblock and partition-table location a
// filesystem or label keeps at either end of a disk.
const zeroSpan = 1 << 20

func edgesZero(dev string) (bool, error) {
	f, err := os.Open(dev)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return false, err
	}
	return zeroEdges(f, size)
}

// zeroEdges reads both ends of a device of the given size. A device too
// small to hold both spans is refused, not passed: a freshly attached
// disk can briefly report a size of 0 before its real capacity appears.
func zeroEdges(r io.ReaderAt, size int64) (bool, error) {
	if size < 2*zeroSpan {
		return false, fmt.Errorf("device reports %d bytes, too small to confirm it is blank", size)
	}
	buf := make([]byte, zeroSpan)
	for _, off := range []int64{0, size - zeroSpan} {
		n, err := r.ReadAt(buf, off)
		if err != nil && !(errors.Is(err, io.EOF) && n == zeroSpan) {
			return false, err
		}
		for _, b := range buf[:n] {
			if b != 0 {
				return false, nil
			}
		}
	}
	return true, nil
}

// mkfsFunc formats the device; replaced in tests.
var mkfsFunc = func(ctx context.Context, dev, fsType string) error {
	mkfs := "mkfs." + fsType
	if _, err := exec.LookPath(mkfs); err != nil {
		return fmt.Errorf("%s not in PATH: %w", mkfs, err)
	}
	if out, err := exec.CommandContext(ctx, mkfs, dev).CombinedOutput(); err != nil {
		return fmt.Errorf("%s %s: %w (%s)", mkfs, dev, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// EnsureFormatted puts an fsType filesystem on dev if, and only if, the
// device provably carries nothing: no filesystem and no partition
// table. driver names the caller for error messages.
func EnsureFormatted(ctx context.Context, driver, dev, fsType string) error {
	if fsType == "" {
		return fmt.Errorf("%s: EnsureFormatted called with empty fsType", driver)
	}
	empty, err := isEmpty(ctx, dev)
	if err != nil {
		return fmt.Errorf("%s: not formatting %s: %w", driver, dev, err)
	}
	if !empty {
		return nil
	}
	if err := mkfsFunc(ctx, dev, fsType); err != nil {
		return fmt.Errorf("%s: %w", driver, err)
	}
	return nil
}

// isEmpty reports whether dev has no filesystem and no partition table.
// It errs on any answer short of a clean "nothing found".
func isEmpty(ctx context.Context, dev string) (bool, error) {
	// blkid reports a device it cannot open as "nothing found", so the
	// device must be shown to exist first.
	if err := statFunc(dev); err != nil {
		return false, fmt.Errorf("cannot inspect device: %w", err)
	}
	r := probeFunc(ctx, dev)
	switch r.exitCode {
	case 0:
		tags := parseExport(r.stdout)
		switch {
		case containerTypes[tags["TYPE"]]:
			// Not a filesystem but a layer that holds one; mounting it
			// would fail anyway, so say what it is.
			return false, fmt.Errorf("device holds %s, not a filesystem; refusing to format it "+
				"(wipe it by hand if it really is unused)", tags["TYPE"])
		case tags["TYPE"] != "":
			return false, nil
		case tags["PTTYPE"] != "":
			return false, fmt.Errorf("device has a %s partition table and no filesystem; refusing to format it "+
				"(wipe it by hand if it really is unused)", tags["PTTYPE"])
		default:
			return false, fmt.Errorf("blkid recognised something it could not name: %q", strings.TrimSpace(string(r.stdout)))
		}
	case 2:
		if msg := strings.TrimSpace(string(r.stderr)); msg != "" {
			return false, fmt.Errorf("blkid: %s", msg)
		}
		// blkid also exits 2, silently, when a read fails mid-probe, when
		// an ext4 superblock fails its checksum, and for data it does not
		// recognise. A new cloud disk reads as zeros; anything else is
		// left alone.
		zero, err := zeroFunc(dev)
		if err != nil {
			return false, fmt.Errorf("cannot read device to confirm it is blank: %w", err)
		}
		if !zero {
			return false, errors.New("blkid found no filesystem but the device holds data; refusing to format it " +
				"(a damaged superblock or an unrecognised format; check it, or wipe it by hand if it really is unused)")
		}
		return true, nil
	default:
		return false, fmt.Errorf("blkid -p %s failed (exit %d): %v %s", dev, r.exitCode, r.err, strings.TrimSpace(string(r.stderr)))
	}
}

// containerTypes are blkid TYPEs for a layer that holds data rather than
// a filesystem the drivers can mount.
var containerTypes = map[string]bool{
	"crypto_LUKS":       true,
	"LVM2_member":       true,
	"linux_raid_member": true,
	"zfs_member":        true,
	"swap":              true,
}

// parseExport reads blkid's KEY=value lines.
func parseExport(out []byte) map[string]string {
	tags := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			tags[k] = v
		}
	}
	return tags
}
