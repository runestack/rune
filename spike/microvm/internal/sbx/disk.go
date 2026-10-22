package sbx

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Disk names a sandbox or template root disk.
//   - thin: a dm-thin volume LV in the spike VG; clones are thin snapshots.
//   - copy: a file; clones are cp --reflink=auto copies (instant on XFS/btrfs,
//     a full copy on ext4).
type Disk struct {
	Mode string `json:"mode"`
	LV   string `json:"lv,omitempty"`
	File string `json:"file,omitempty"`
}

func (h *Host) lvPath(lv string) string { return filepath.Join("/dev", h.VG, lv) }

// ImportImage turns a raw ext4 image into a template's base disk.
func (h *Host) ImportImage(image, name string) (Disk, error) {
	switch h.DiskMode {
	case "thin":
		fi, err := os.Stat(image)
		if err != nil {
			return Disk{}, err
		}
		lv := "tpl-" + name
		size := fmt.Sprintf("%dB", fi.Size())
		if err := run("lvcreate", "-y", "-V", size, "-T", h.VG+"/"+h.Pool, "-n", lv); err != nil {
			return Disk{}, err
		}
		// conv=sparse skips zero blocks so the thin volume stays thin.
		if err := run("dd", "if="+image, "of="+h.lvPath(lv), "bs=4M", "conv=sparse,fsync", "status=none"); err != nil {
			return Disk{}, err
		}
		return Disk{Mode: "thin", LV: lv}, nil
	default:
		dir := h.TemplateDir(name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return Disk{}, err
		}
		dst := filepath.Join(dir, "rootfs.ext4")
		if err := run("cp", "--reflink=auto", "--sparse=always", image, dst); err != nil {
			return Disk{}, err
		}
		return Disk{Mode: "copy", File: dst}, nil
	}
}

// CloneDisk makes a writable copy-on-write child of src named for id.
func (h *Host) CloneDisk(src Disk, id string) (Disk, error) {
	switch src.Mode {
	case "thin":
		lv := "sbx-" + id
		// -kn: no activation skip, so the snapshot is active immediately.
		if err := run("lvcreate", "-y", "-s", "-kn", "-n", lv, h.VG+"/"+src.LV); err != nil {
			return Disk{}, err
		}
		return Disk{Mode: "thin", LV: lv}, nil
	case "copy":
		dst := filepath.Join(h.Prefix, "disks", id+".ext4")
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return Disk{}, err
		}
		if err := run("cp", "--reflink=auto", "--sparse=always", src.File, dst); err != nil {
			return Disk{}, err
		}
		return Disk{Mode: "copy", File: dst}, nil
	}
	return Disk{}, fmt.Errorf("unknown disk mode %q", src.Mode)
}

// Install makes d appear as DiskName inside root: a device node for a thin
// volume (the jailer chroot has no /dev/<vg>), a hardlink for a file.
func (h *Host) Install(d Disk, root string) error {
	dst := filepath.Join(root, DiskName)
	switch d.Mode {
	case "thin":
		var st unix.Stat_t
		if err := unix.Stat(h.lvPath(d.LV), &st); err != nil {
			return fmt.Errorf("stat %s: %w", h.lvPath(d.LV), err)
		}
		if err := unix.Mknod(dst, unix.S_IFBLK|0o600, int(st.Rdev)); err != nil {
			return fmt.Errorf("mknod %s: %w", dst, err)
		}
	case "copy":
		if err := linkOrCopy(d.File, dst); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown disk mode %q", d.Mode)
	}
	return h.chown(dst)
}

func (h *Host) RemoveDisk(d Disk) error {
	switch d.Mode {
	case "thin":
		if d.LV == "" {
			return nil
		}
		return run("lvremove", "-y", h.VG+"/"+d.LV)
	case "copy":
		if d.File == "" {
			return nil
		}
		return os.Remove(d.File)
	}
	return nil
}
