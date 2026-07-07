package controllers

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/google/uuid"
	"github.com/runestack/rune/pkg/types"
	"github.com/runestack/rune/pkg/utils"
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

// serviceHasStableIdentity reports whether a service needs stable per-replica
// identity. It mirrors the Kubernetes StatefulSet rule: a service that declares
// a per-replica volume claimTemplate is stateful — its replicas bind to
// per-ordinal volumes ("<mount>-<service>-<ordinal>") and so must keep stable
// "{service}-{ordinal}" names across restarts. Everything else is stateless and
// gets a unique "{service}-{shorthash}" name per instance lifetime, so a
// recreated replica never collides with its predecessor in logs (#84).
func serviceHasStableIdentity(service *types.Service) bool {
	for i := range service.Volumes {
		if service.Volumes[i].ClaimTemplate != nil {
			return true
		}
	}
	return false
}

// generateInstanceName builds the stable per-ordinal name for a stateful
// service: "{service}-{ordinal}". The ordinal is the replica slot index.
func generateInstanceName(service *types.Service, index int) string {
	return fmt.Sprintf("%s-%d", service.Name, index)
}

// generateHashInstanceName builds a unique-per-lifetime name for a stateless
// service: "{service}-{shorthash}". taken holds the names already in use within
// this service so the (rare) suffix collision never produces two live instances
// with the same name.
func generateHashInstanceName(service *types.Service, taken map[string]bool) string {
	for {
		name := fmt.Sprintf("%s-%s", service.Name, utils.ShortHash(uuid.NewString()))
		if !taken[name] {
			return name
		}
	}
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
