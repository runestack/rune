package cmd

import (
	"testing"
	"time"

	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
)

func TestInstanceReadyCell(t *testing.T) {
	changed := time.Now()
	assert.Equal(t, "yes", instanceReadyCell(&types.Instance{Status: types.InstanceStatusRunning}),
		"no verdict recorded counts as ready")
	assert.Equal(t, "yes", instanceReadyCell(&types.Instance{Status: types.InstanceStatusRunning, Ready: true, ReadyChangedAt: &changed}))
	assert.Equal(t, "no", instanceReadyCell(&types.Instance{Status: types.InstanceStatusRunning, Ready: false, ReadyChangedAt: &changed}))
	assert.Equal(t, "-", instanceReadyCell(&types.Instance{Status: types.InstanceStatusStarting}))
}
