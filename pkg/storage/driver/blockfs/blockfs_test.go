package blockfs

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fake(t *testing.T, statErr error, r probeResult) *int {
	t.Helper()
	formats := 0
	oldProbe, oldStat, oldMkfs := probeFunc, statFunc, mkfsFunc
	t.Cleanup(func() { probeFunc, statFunc, mkfsFunc = oldProbe, oldStat, oldMkfs })
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
