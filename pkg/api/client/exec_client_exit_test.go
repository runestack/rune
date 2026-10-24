package client

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/runestack/rune/pkg/api/generated"
)

// The remote command's exit code is the session's result, so the CLI
// can exit with it.
func TestExitResult(t *testing.T) {
	assert.NoError(t, exitResult(&generated.ExitInfo{Code: 0}))

	var ee *ExitError
	err := exitResult(&generated.ExitInfo{Code: 3})
	if assert.True(t, errors.As(err, &ee)) {
		assert.Equal(t, 3, ee.ExitCode())
	}

	err = exitResult(&generated.ExitInfo{Code: 137, Signaled: true, Signal: "SIGKILL"})
	if assert.True(t, errors.As(err, &ee)) {
		assert.Equal(t, 137, ee.ExitCode())
		assert.Contains(t, ee.Error(), "SIGKILL")
	}

	// Signalled with no code still fails.
	err = exitResult(&generated.ExitInfo{Signaled: true})
	if assert.True(t, errors.As(err, &ee)) {
		assert.Equal(t, 1, ee.ExitCode())
	}
}
