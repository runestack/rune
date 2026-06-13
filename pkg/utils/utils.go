package utils

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"strconv"
	"strings"
	"time"
)

// shortHashLen is the number of base36 characters in a ShortHash suffix.
const shortHashLen = 5

// ShortHash returns a short, lowercase base36 token derived from input —
// 5 characters drawn from [0-9a-z], DNS-1123-label safe. It is used to build
// unique-per-lifetime instance names ("{service}-{shorthash}") for stateless
// services, so a recreated instance never reuses a predecessor's name and its
// logs stay distinguishable. Not cryptographic: collision resistance is only
// "good enough" for the small per-service replica counts it names, so callers
// needing uniqueness within a set should still guard against the rare clash.
// Deterministic for a given input.
func ShortHash(input string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(input))
	s := strconv.FormatUint(h.Sum64(), 36)
	if len(s) >= shortHashLen {
		// Trailing chars carry the well-mixed low bits of the hash.
		return s[len(s)-shortHashLen:]
	}
	return strings.Repeat("0", shortHashLen-len(s)) + s
}

// Helper to safely convert non-negative ints to int32 with clamping
func ToInt32NonNegative(n int) int32 {
	if n < 0 {
		return 0
	}
	if n > int(math.MaxInt32) {
		return math.MaxInt32
	}
	return int32(n)
}

func ToInt32NonNegative64(n int64) int32 {
	if n < 0 {
		return 0
	}
	if n > int64(math.MaxInt32) {
		return math.MaxInt32
	}
	return int32(n)
}

func ToUint32NonNegative(n int) uint32 {
	if n < 0 {
		return 0
	}
	if n > int(math.MaxUint32) {
		return math.MaxUint32
	}
	return uint32(n)
}

func ToUint64NonNegative(n int) uint64 {
	if n < 0 {
		return 0
	}
	return uint64(n)
}

func ToUintNonNegative(n int) uint {
	if n < 0 {
		return 0
	}
	return uint(n)
}

func MapToPretty(v interface{}) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return ""
	}
	return string(b)
}

func PrettyPrint(v ...interface{}) {
	prints := CapturePrettyPrint(v...)
	fmt.Println(prints...)
}

func CapturePrettyPrint(v ...interface{}) []any {
	prints := []any{}
	for _, value := range v {
		// if v is a string then print it directly
		if str, ok := value.(string); ok {
			prints = append(prints, str)
			continue
		}

		if str, ok := value.(*string); ok {
			prints = append(prints, *str)
			continue
		}

		if intValue, ok := value.(int); ok {
			prints = append(prints, strconv.Itoa(intValue))
			continue
		}

		if intValue, ok := value.(*int); ok {
			prints = append(prints, strconv.Itoa(*intValue))
			continue
		}

		if intValue, ok := value.(int64); ok {
			prints = append(prints, strconv.FormatInt(intValue, 10))
			continue
		}

		if intValue, ok := value.(*int64); ok {
			prints = append(prints, strconv.FormatInt(*intValue, 10))
			continue
		}

		if floatValue, ok := value.(float64); ok {
			prints = append(prints, strconv.FormatFloat(floatValue, 'f', -1, 64))
			continue
		}

		if floatValue, ok := value.(*float64); ok {
			prints = append(prints, strconv.FormatFloat(*floatValue, 'f', -1, 64))
			continue
		}

		// default to pretty print
		prints = append(prints, MapToPretty(value))
	}

	return prints
}

func ProtoStringToTimestamp(timestamp string) *time.Time {
	if timestamp == "" {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339, timestamp)
	if err != nil {
		return nil
	}
	return &parsed
}

// ParseTimestamp parses a timestamp string into a time.Time.
func ParseTimestamp(timestampStr string) (*time.Time, error) {
	// Parse created_at timestamp
	if timestampStr != "" {
		timestamp, err := time.Parse(time.RFC3339, timestampStr)
		if err != nil {
			return nil, fmt.Errorf("failed to parse timestamp: %w", err)
		}
		return &timestamp, nil
	}
	return nil, nil
}

// PickFirstNonEmpty picks the first non-empty value from a list of strings.
// If all values are empty, returns the empty string.
func PickFirstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// ValidateDNS1123Name validates the a name according to DNS-1123 rules
func ValidateDNS1123Name(name string) error {
	// Check length (1-63 characters)
	if len(name) < 1 || len(name) > 63 {
		return fmt.Errorf("name must be between 1 and 63 characters")
	}

	// Check if starts or ends with hyphen
	if name[0] == '-' || name[len(name)-1] == '-' {
		return fmt.Errorf("name cannot start or end with a hyphen")
	}

	// Check characters (only lowercase letters, digits, and hyphens)
	for _, c := range name {
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
			return fmt.Errorf("name can only contain lowercase letters, digits, and hyphens")
		}
	}

	// Check for consecutive hyphens
	for i := 1; i < len(name); i++ {
		if name[i] == '-' && name[i-1] == '-' {
			return fmt.Errorf("name cannot contain consecutive hyphens")
		}
	}

	return nil
}

func SliceContains[T comparable](s []T, e T) bool {
	for _, v := range s {
		if v == e {
			return true
		}
	}
	return false
}
