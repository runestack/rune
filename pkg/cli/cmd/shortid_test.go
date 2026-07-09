package cmd

import "testing"

func TestShortInstanceID(t *testing.T) {
	cases := map[string]string{
		"0f238d0c-cd8e-4d37-922f-b554965a4ecd": "0f238d0c", // UUID → first segment
		"abcdef1234567890abcdef1234567890":     "abcdef12", // no dashes → first 8
		"short":                                "short",    // shorter than 8 → as-is
		"":                                     "",
		"12345678":                             "12345678", // exactly 8, no dash
	}
	for in, want := range cases {
		if got := shortInstanceID(in); got != want {
			t.Errorf("shortInstanceID(%q) = %q, want %q", in, got, want)
		}
	}
}
