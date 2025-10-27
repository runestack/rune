package controllers

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
)

// buildWorkItemKey builds a key for a work item
func buildWorkItemKey(event store.WatchEvent) string {
	return fmt.Sprintf("%s/%s/%s/%s", event.ResourceType, event.Namespace, event.Name, event.Type)
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

// generateInstanceName generates a unique instance name for a service
func generateInstanceName(service *types.Service, index int) string {
	return fmt.Sprintf("%s-%d", service.Name, index)
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
