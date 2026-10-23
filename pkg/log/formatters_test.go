package log

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJSONFormatter_KeepsFieldsThatCollideWithStandardKeys(t *testing.T) {
	entry := &Entry{
		Level:     InfoLevel,
		Message:   "Readiness check failed",
		Timestamp: time.Now(),
		Fields:    Fields{"message": "connection refused", "level": "probe", "instance": "api-0"},
	}
	out, err := (&JSONFormatter{}).Format(entry)
	require.NoError(t, err)

	var got map[string]interface{}
	require.NoError(t, json.Unmarshal(out, &got))
	assert.Equal(t, "Readiness check failed", got["message"])
	assert.Equal(t, "INFO", got["level"])
	assert.Equal(t, "connection refused", got["fields.message"])
	assert.Equal(t, "probe", got["fields.level"])
	assert.Equal(t, "api-0", got["instance"])
}
