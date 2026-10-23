// Package mountinfo answers "which block devices are mounted at this
// path" from /proc/self/mountinfo, without a subprocess.
//
// A path can carry several mounts stacked on top of each other; only the
// top one is visible to a new bind mount. Stack reports all of them, so a
// caller can tell "my device is here" from "my device is here but
// something else has been mounted over it".
package mountinfo

import (
	"bufio"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
)

// Dev identifies a block device by major:minor, which is what mountinfo
// records and what stat(2) on the device node returns.
type Dev struct {
	Major, Minor uint32
}

func (d Dev) String() string { return fmt.Sprintf("%d:%d", d.Major, d.Minor) }

type entry struct {
	id, parent int
	dev        Dev
	mountPoint string
}

// parseStack reads mountinfo-format text and returns the devices mounted
// exactly at path, bottom of the stack first.
func parseStack(r io.Reader, path string) ([]Dev, error) {
	path = filepath.Clean(path)
	var at []entry
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		e, err := parseLine(sc.Text())
		if err != nil {
			return nil, err
		}
		if e.mountPoint == path {
			at = append(at, e)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("mountinfo: read: %w", err)
	}
	return order(at), nil
}

// parseMountPoints reads mountinfo-format text and returns every path
// dev is mounted at.
func parseMountPoints(r io.Reader, dev Dev) ([]string, error) {
	var out []string
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		e, err := parseLine(sc.Text())
		if err != nil {
			return nil, err
		}
		if e.dev == dev {
			out = append(out, e.mountPoint)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("mountinfo: read: %w", err)
	}
	return out, nil
}

// order sorts the mounts at one path bottom-to-top. A mount stacked on
// another has the lower one as its parent, so the bottom is the entry
// whose parent is not itself mounted at this path.
func order(at []entry) []Dev {
	if len(at) == 0 {
		return nil
	}
	byParent := make(map[int]entry, len(at))
	ids := make(map[int]bool, len(at))
	for _, e := range at {
		byParent[e.parent] = e
		ids[e.id] = true
	}
	var cur entry
	found := false
	for _, e := range at {
		if !ids[e.parent] {
			cur, found = e, true
			break
		}
	}
	if !found {
		// A cycle is impossible in a real mount table; fall back to file
		// order, which the kernel also emits parent-before-child.
		out := make([]Dev, len(at))
		for i, e := range at {
			out[i] = e.dev
		}
		return out
	}
	out := []Dev{cur.dev}
	for len(out) < len(at) {
		next, ok := byParent[cur.id]
		if !ok {
			break
		}
		out = append(out, next.dev)
		cur = next
	}
	return out
}

// parseLine parses one mountinfo line:
//
//	36 35 98:0 /mnt1 /mnt2 rw,noatime master:1 - ext3 /dev/root rw
//	id parent maj:min root mountpoint ...
func parseLine(line string) (entry, error) {
	f := strings.Fields(line)
	if len(f) < 5 {
		return entry{}, fmt.Errorf("mountinfo: short line %q", line)
	}
	id, err := strconv.Atoi(f[0])
	if err != nil {
		return entry{}, fmt.Errorf("mountinfo: mount id in %q: %w", line, err)
	}
	parent, err := strconv.Atoi(f[1])
	if err != nil {
		return entry{}, fmt.Errorf("mountinfo: parent id in %q: %w", line, err)
	}
	maj, min, ok := strings.Cut(f[2], ":")
	if !ok {
		return entry{}, fmt.Errorf("mountinfo: device in %q", line)
	}
	major, err := strconv.ParseUint(maj, 10, 32)
	if err != nil {
		return entry{}, fmt.Errorf("mountinfo: major in %q: %w", line, err)
	}
	minor, err := strconv.ParseUint(min, 10, 32)
	if err != nil {
		return entry{}, fmt.Errorf("mountinfo: minor in %q: %w", line, err)
	}
	return entry{
		id:         id,
		parent:     parent,
		dev:        Dev{Major: uint32(major), Minor: uint32(minor)},
		mountPoint: unescape(f[4]),
	}, nil
}

// unescape undoes the kernel's octal escaping of space, tab, newline and
// backslash in mount points.
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
