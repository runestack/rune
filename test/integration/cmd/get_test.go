package cmd_test

import (
	"testing"

	"github.com/runestack/rune/test/integration/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGetCommand tests the get command functionality
func TestGetCommand(t *testing.T) {
	helper := helpers.NewTestHelper(t)
	defer helper.Cleanup()

	// Test only the simplest case for now - just list services
	t.Run("list all services", func(t *testing.T) {
		output, err := helper.RunCommand("get", "services")
		require.NoError(t, err)

		// Just check that we get some output
		assert.NotEmpty(t, output)
		assert.Contains(t, output, "NAME")
	})

	// Test getting a specific service
	t.Run("get specific service", func(t *testing.T) {
		output, err := helper.RunCommand("get", "service", "web")
		require.NoError(t, err)

		// Check that we get some output
		assert.NotEmpty(t, output)
		assert.Contains(t, output, "web")
	})

	// Test YAML output format
	t.Run("get service with yaml output", func(t *testing.T) {
		output, err := helper.RunCommand("get", "service", "web", "--output=yaml")
		require.NoError(t, err)

		// Check that we get YAML output
		assert.NotEmpty(t, output)
		assert.Contains(t, output, "name:")
		assert.Contains(t, output, "web")
	})

	// Test JSON output format
	t.Run("get service with json output", func(t *testing.T) {
		output, err := helper.RunCommand("get", "service", "web", "--output=json")
		require.NoError(t, err)

		// Check that we get JSON output
		assert.NotEmpty(t, output)
		assert.Contains(t, output, "\"name\"")
		assert.Contains(t, output, "\"web\"")
	})

	// Test no-headers option
	t.Run("list services with no headers", func(t *testing.T) {
		output, err := helper.RunCommand("get", "services", "--no-headers")
		require.NoError(t, err)

		// Check that we get output without headers
		assert.NotEmpty(t, output)
		assert.NotContains(t, output, "NAME")
		assert.NotContains(t, output, "TYPE")
		assert.NotContains(t, output, "STATUS")
	})
}
