package utils

// ShortIDMinPrefix is the shortest instance-ID prefix that may be resolved.
// `rune get instances` prints 8 hex chars; below 6 the collision risk and the
// chance of shadowing a real resource name are too high to auto-resolve.
const ShortIDMinPrefix = 6

// IsHexIDPrefix reports whether s is a plausible UUID prefix: at least
// ShortIDMinPrefix lowercase-hex characters. Callers use it to gate git/docker
// style short-id resolution so a partial *name* can never be mistaken for an
// abbreviated instance ID.
//
// This lives in pkg/utils because both resolvers need it — the server-side
// target resolver (pkg/api/service) and the CLI-side one (pkg/cli/cmd). They
// previously carried separate copies, which is how `rune logs <short-id>`
// came to work while `rune exec <short-id>` did not.
func IsHexIDPrefix(s string) bool {
	if len(s) < ShortIDMinPrefix {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
