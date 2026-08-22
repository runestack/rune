package instance

import (
	"strings"
	"unicode"

	"github.com/runestack/rune/pkg/types"
)

// serviceBeingDeleted reports whether a service is tombstoned for foreground
// deletion (RFC #129 Phase 4). Callers that create or resurrect instances must
// consult this so a tombstoned service never spawns new instances mid-teardown.
func serviceBeingDeleted(service *types.Service) bool {
	return service != nil && service.Metadata != nil && service.Metadata.DeletionTimestamp != nil
}

// areStringSlicesEqual checks if two string slices are equal
func areStringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}

	for i, v := range a {
		if v != b[i] {
			return false
		}
	}

	return true
}

// trimWhitespaces trims all whitespace from a string
func trimWhitespaces(value string) string {
	if value == "" {
		return ""
	}
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, value)
}
