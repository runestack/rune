package mountinfo

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const table = `22 1 253:1 / / rw,relatime shared:1 - ext4 /dev/vda1 rw
40 22 8:16 / /var/lib/rune/mounts/data rw,relatime shared:20 - ext4 /dev/sdb rw
51 40 8:32 / /var/lib/rune/mounts/data rw,relatime shared:25 - ext4 /dev/sdc rw
60 22 8:48 / /var/lib/rune/mounts/prod/data rw,relatime shared:30 - ext4 /dev/sdd rw
61 22 8:64 / /mnt/with\040space rw - ext4 /dev/sde rw
`

func TestParseStack(t *testing.T) {
	cases := []struct {
		path string
		want []Dev
	}{
		{"/var/lib/rune/mounts/data", []Dev{{8, 16}, {8, 32}}},
		{"/var/lib/rune/mounts/data/", []Dev{{8, 16}, {8, 32}}},
		{"/var/lib/rune/mounts/prod/data", []Dev{{8, 48}}},
		{"/var/lib/rune/mounts/prod", nil},
		{"/mnt/with space", []Dev{{8, 64}}},
	}
	for _, c := range cases {
		got, err := parseStack(strings.NewReader(table), c.path)
		require.NoError(t, err, c.path)
		assert.Equal(t, c.want, got, c.path)
	}
}

// The stack order comes from the parent links, not from line order.
func TestParseStack_OrderFromParents(t *testing.T) {
	reordered := `51 40 8:32 / /m rw - ext4 /dev/sdc rw
40 22 8:16 / /m rw - ext4 /dev/sdb rw
`
	got, err := parseStack(strings.NewReader(reordered), "/m")
	require.NoError(t, err)
	assert.Equal(t, []Dev{{8, 16}, {8, 32}}, got)
}

func TestParseStack_Malformed(t *testing.T) {
	_, err := parseStack(strings.NewReader("garbage\n"), "/m")
	assert.Error(t, err)
}

func TestParseMountPoints(t *testing.T) {
	got, err := parseMountPoints(strings.NewReader(table+"70 22 8:16 / /var/lib/rune/mounts/prod/data2 rw - ext4 /dev/sdb rw\n"), Dev{8, 16})
	require.NoError(t, err)
	assert.Equal(t, []string{"/var/lib/rune/mounts/data", "/var/lib/rune/mounts/prod/data2"}, got)

	got, err = parseMountPoints(strings.NewReader(table), Dev{9, 9})
	require.NoError(t, err)
	assert.Empty(t, got)
}
