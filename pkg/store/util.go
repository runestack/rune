package store

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/runestack/rune/pkg/types"
)

// MakeKey creates a standardized key for a resource.
func MakeKey(resourceType types.ResourceType, namespace, name string) []byte {
	return []byte(fmt.Sprintf("%s/%s/%s", resourceType, namespace, name))
}

// MakeVersionKey creates a standardized key for a resource version.
func MakeVersionKey(resourceType types.ResourceType, namespace, name, version string) []byte {
	return []byte(fmt.Sprintf("%s-versions/%s/%s/%s", resourceType, namespace, name, version))
}

// MakePrefix creates a prefix for listing resources by type and namespace.
func MakePrefix(resourceType types.ResourceType, namespace string) []byte {
	if namespace == "all" || namespace == "*" {
		return []byte(fmt.Sprintf("%s/", resourceType))
	}

	return []byte(fmt.Sprintf("%s/%s", resourceType, namespace))
}

// MakeVersionPrefix creates a prefix for listing resource versions.
func MakeVersionPrefix(resourceType types.ResourceType, namespace, name string) []byte {
	if namespace == "all" || namespace == "*" {
		return []byte(fmt.Sprintf("%s-versions/%s/", resourceType, name))
	}

	return []byte(fmt.Sprintf("%s-versions/%s/%s/", resourceType, namespace, name))
}

// ParseKey parses a key into its components.
func ParseKey(key []byte) (resourceType, namespace, name string, ok bool) {
	parts := strings.Split(string(key), "/")
	if len(parts) != 3 {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

// ParseVersionKey parses a version key into its components.
func ParseVersionKey(key []byte) (resourceType, namespace, name, version string, ok bool) {
	s := string(key)
	if !strings.Contains(s, "-versions/") {
		return "", "", "", "", false
	}

	// Remove the -versions part
	resourceType = strings.Split(s, "-versions/")[0]

	// Get the rest of the path
	rest := s[len(resourceType)+len("-versions/"):]
	parts := strings.Split(rest, "/")
	if len(parts) != 3 {
		return "", "", "", "", false
	}

	return resourceType, parts[0], parts[1], parts[2], true
}

// ResourceTypes returns a slice of the standard resource types used in the system.
func ResourceTypes() []string {
	return []string{
		"services",
		"instances",
		"configs",
		"secrets",
		"functions",
		"jobs",
		"jobruns",
		"networks",
		"volumes",
	}
}

// resetTarget zeroes *target so a following json.Unmarshal decodes into a clean
// value. encoding/json never zeroes its destination: absent keys keep whatever
// was in the slot, non-nil maps are merged into, non-nil pointers are decoded
// through, and a slice's existing backing array is reused element-by-element.
// Every field an `omitempty` writer left out therefore survives the decode — so
// a re-read that is supposed to replace the target instead merges the previous
// contents into it. In UpdateFunc that previous content is the mutation of an
// attempt the store already rejected, which is how discarded state gets
// committed. Zeroing first makes each decode a replace, as callers assume.
//
// A non-pointer or nil target is left alone; json.Unmarshal reports it.
func resetTarget(target interface{}) {
	v := reflect.ValueOf(target)
	if v.Kind() != reflect.Ptr || v.IsNil() {
		return
	}
	elem := v.Elem()
	if !elem.CanSet() {
		return
	}
	elem.Set(reflect.Zero(elem.Type()))
}

// UnmarshalResource converts a resource interface to a target type using JSON marshaling/unmarshaling.
// This is useful for converting between different types that share the same JSON structure.
// The function handles the conversion by first marshaling the source to JSON and then
// unmarshaling into the target type.
func UnmarshalResource(source interface{}, target interface{}) error {
	// Marshal the source to JSON
	jsonData, err := json.Marshal(source)
	if err != nil {
		return fmt.Errorf("failed to marshal resource: %w", err)
	}

	// Unmarshal into the target type
	if err := json.Unmarshal(jsonData, target); err != nil {
		return fmt.Errorf("failed to unmarshal resource: %w", err)
	}

	return nil
}

// IsAlreadyExistsError checks if an error is an already exists error.
func IsAlreadyExistsError(err error) bool {
	return strings.Contains(err.Error(), "already exists")
}

// IsNotFoundError checks if an error is a not found error.
func IsNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "not found")
}
