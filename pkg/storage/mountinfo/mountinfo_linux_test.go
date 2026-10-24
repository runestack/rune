//go:build linux

package mountinfo

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Reads the real table: "/" is always mounted.
func TestStack_Root(t *testing.T) {
	got, err := Stack("/")
	require.NoError(t, err)
	assert.NotEmpty(t, got)
}

func TestDeviceOf_NotBlock(t *testing.T) {
	_, err := DeviceOf("/proc/self/mountinfo")
	assert.ErrorContains(t, err, "not a block device")
}
