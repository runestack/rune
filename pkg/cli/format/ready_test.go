package format

import (
	"testing"
	"time"

	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
)

func TestInstanceReady(t *testing.T) {
	changed := time.Now()
	assert.Equal(t, "yes", InstanceReady(&types.Instance{Status: types.InstanceStatusRunning}),
		"no verdict recorded counts as ready")
	assert.Equal(t, "yes", InstanceReady(&types.Instance{Status: types.InstanceStatusRunning, Ready: true, ReadyChangedAt: &changed}))
	assert.Equal(t, "no", InstanceReady(&types.Instance{Status: types.InstanceStatusRunning, Ready: false, ReadyChangedAt: &changed}))
	assert.Equal(t, "-", InstanceReady(&types.Instance{Status: types.InstanceStatusStarting}))
}
