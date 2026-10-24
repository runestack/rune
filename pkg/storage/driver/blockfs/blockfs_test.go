package blockfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fake(t *testing.T, statErr error, r probeResult) *int {
	return fakeZero(t, statErr, r, true, nil)
}

func fakeZero(t *testing.T, statErr error, r probeResult, zero bool, zeroErr error) *int {
	t.Helper()
	formats := 0
	oldProbe, oldStat, oldMkfs, oldZero := probeFunc, statFunc, mkfsFunc, zeroFunc
	t.Cleanup(func() { probeFunc, statFunc, mkfsFunc, zeroFunc = oldProbe, oldStat, oldMkfs, oldZero })
	zeroFunc = func(string) (bool, error) { return zero, zeroErr }
	statFunc = func(string) error { return statErr }
	probeFunc = func(context.Context, string) probeResult { return r }
	mkfsFunc = func(context.Context, string, string) error { formats++; return nil }
	return &formats
}

// Only a clean "nothing found" from a device that exists may format.
func TestEnsureFormatted(t *testing.T) {
	cases := []struct {
		name    string
		statErr error
		probe   probeResult
		format  bool
		wantErr string
	}{
		{"filesystem present", nil, probeResult{stdout: []byte("DEVNAME=/dev/sdb\nTYPE=ext4\n"), exitCode: 0}, false, ""},
		{"blank disk", nil, probeResult{exitCode: 2}, true, ""},
		{"partition table only", nil, probeResult{stdout: []byte("PTTYPE=gpt\n"), exitCode: 0}, false, "partition table"},
		{"device missing", errors.New("no such file"), probeResult{exitCode: 2}, false, "cannot inspect device"},
		{"blkid could not open it", nil, probeResult{exitCode: 2, stderr: []byte("error: /dev/sdb: Permission denied")}, false, "Permission denied"},
		{"ambivalent probe", nil, probeResult{exitCode: 8, err: errors.New("exit status 8")}, false, "exit 8"},
		{"blkid did not run", nil, probeResult{exitCode: -1, err: context.Canceled}, false, "context canceled"},
		{"unnamed signature", nil, probeResult{stdout: []byte("DEVNAME=/dev/sdb\n"), exitCode: 0}, false, "could not name"},
		{"LUKS container", nil, probeResult{stdout: []byte("TYPE=crypto_LUKS\n"), exitCode: 0}, false, "crypto_LUKS"},
		{"LVM member", nil, probeResult{stdout: []byte("TYPE=LVM2_member\n"), exitCode: 0}, false, "LVM2_member"},
		{"xfs when ext4 asked", nil, probeResult{stdout: []byte("TYPE=xfs\n"), exitCode: 0}, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			formats := fake(t, c.statErr, c.probe)
			err := EnsureFormatted(context.Background(), "gcepd", "/dev/sdb", "ext4")
			if c.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), c.wantErr)
				assert.Contains(t, err.Error(), "gcepd")
			} else {
				require.NoError(t, err)
			}
			if c.format {
				assert.Equal(t, 1, *formats)
			} else {
				assert.Zero(t, *formats, "must not format")
			}
		})
	}
}

func TestEnsureFormatted_EmptyFsType(t *testing.T) {
	formats := fake(t, nil, probeResult{exitCode: 2})
	require.Error(t, EnsureFormatted(context.Background(), "gcepd", "/dev/sdb", ""))
	assert.Zero(t, *formats)
}

// blkid exits 2 silently for read errors, bad ext4 checksums and data it
// does not know. Only a disk that reads as zeros may be formatted.
func TestEnsureFormatted_NotFoundButNotZero(t *testing.T) {
	for _, c := range []struct {
		name    string
		zero    bool
		zeroErr error
		wantErr string
	}{
		{"holds data", false, nil, "holds data"},
		{"unreadable", false, errors.New("input/output error"), "input/output error"},
	} {
		t.Run(c.name, func(t *testing.T) {
			formats := fakeZero(t, nil, probeResult{exitCode: 2}, c.zero, c.zeroErr)
			err := EnsureFormatted(context.Background(), "gcepd", "/dev/sdb", "ext4")
			require.Error(t, err)
			assert.Contains(t, err.Error(), c.wantErr)
			assert.Zero(t, *formats)
		})
	}
}

func TestEdgesZero(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, size int, at map[int]byte) string {
		p := filepath.Join(dir, name)
		b := make([]byte, size)
		for i, v := range at {
			b[i] = v
		}
		require.NoError(t, os.WriteFile(p, b, 0o600))
		return p
	}
	const mib = 1 << 20
	for _, c := range []struct {
		name    string
		path    string
		want    bool
		wantErr bool
	}{
		{"all zero", write("z", 4*mib, nil), true, false},
		{"data at start", write("s", 4*mib, map[int]byte{1080: 0x53}), false, false},
		{"data at end", write("e", 4*mib, map[int]byte{4*mib - 512: 1}), false, false},
		{"data in the middle is not looked at", write("m", 4*mib, map[int]byte{2 * mib: 1}), true, false},
		{"exactly two spans", write("two", 2*mib, nil), true, false},
		{"smaller than two spans", write("small", 2*mib-1, nil), false, true},
		{"reports zero size", write("empty", 0, nil), false, true},
	} {
		got, err := edgesZero(c.path)
		if c.wantErr {
			assert.Error(t, err, c.name)
			continue
		}
		require.NoError(t, err, c.name)
		assert.Equal(t, c.want, got, c.name)
	}
	_, err := edgesZero(filepath.Join(dir, "missing"))
	assert.Error(t, err)
}

type failingReader struct{ failAt int64 }

func (f failingReader) ReadAt(p []byte, off int64) (int, error) {
	if off == f.failAt {
		return 0, errors.New("input/output error")
	}
	clear(p)
	return len(p), nil
}

// A read error at either end is an error, never "zeros".
func TestZeroEdges_ReadError(t *testing.T) {
	const size = 8 << 20
	for _, at := range []int64{0, size - zeroSpan} {
		_, err := zeroEdges(failingReader{failAt: at}, size)
		assert.ErrorContains(t, err, "input/output error", "offset %d", at)
	}
}
