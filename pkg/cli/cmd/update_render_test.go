package cmd

// RUNE-042 Phase 6: the operator-facing rendering of an in-flight update.
// Both design reviews found that Service.Update was plumbed end-to-end and
// then consumed by nothing — a held update was diagnosable only by reading
// `rune get svc -o yaml`.

import (
	"testing"

	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
)

func TestUpdateCell(t *testing.T) {
	assert.Equal(t, "—", updateCell(nil), "no update in flight reads as nothing, not 0/0")
	assert.Equal(t, "2/4 replaced", updateCell(&types.UpdateStatus{
		Desired: 4, Updated: 3, UpdatedReady: 2, Available: 4, Outdated: 2,
	}), "the table cell reports replacement progress against the desired scale")
}

// The UPDATE column must appear only when something is actually updating: a
// permanently-empty column is a permanent question for a feature most
// services are not using at any given moment.
func TestServiceTable_UpdateColumnOnlyWhenUpdating(t *testing.T) {
	steady := []serviceReport{
		{Name: "api", Status: "Running", DesiredScale: 2, ReadyInstances: 2},
		{Name: "web", Status: "Running", DesiredScale: 1, ReadyInstances: 1},
	}
	assert.False(t, anyUpdating(steady), "a steady cluster must render the pre-RUNE-042 table")

	updating := append([]serviceReport{}, steady...)
	updating[1].Update = &types.UpdateStatus{Desired: 1, UpdatedReady: 0}
	assert.True(t, anyUpdating(updating), "one updating service is enough to show the column")
}
