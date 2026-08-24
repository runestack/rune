package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/stretchr/testify/assert"
)

// A node maintains no Status and no LastHeartbeat, so the render omits
// the line rather than printing a blank one — a blank status reads as a
// dead node on a perfectly healthy box.
func TestRenderDescribe_OmitsEmptyStatus(t *testing.T) {
	var buf bytes.Buffer
	renderDescribe(&buf, &generated.DescribeResult{
		Kind: "Node",
		Name: "node-8f6a12cd",
		Identity: []*generated.DescribeKV{
			{Key: "ID", Value: "node-8f6a12cd"},
			{Key: "Address", Value: "127.0.0.1"},
		},
		Sections: []*generated.DescribeSection{
			{Title: "GPUs", Lines: []string{"GPU-8f6a  NVIDIA L40S  48Gi  driver 550.54.15"}},
		},
	})
	out := buf.String()
	assert.NotContains(t, out, "Status:")
	assert.Contains(t, out, "node-8f6a12cd")
	assert.Contains(t, out, "GPUs:")
	assert.Contains(t, out, "NVIDIA L40S")
}

// Kinds that do maintain a status still render it.
func TestRenderDescribe_KeepsNonEmptyStatus(t *testing.T) {
	var buf bytes.Buffer
	renderDescribe(&buf, &generated.DescribeResult{
		Kind: "Instance", Name: "flo-0", Status: "Running",
	})
	assert.True(t, strings.Contains(buf.String(), "Status:"))
}

func TestDescribeKind_AcceptsNode(t *testing.T) {
	for _, in := range []string{"node", "nodes", "Node"} {
		got, ok := describeKind(in)
		assert.True(t, ok, in)
		assert.Equal(t, "node", got)
	}
}
