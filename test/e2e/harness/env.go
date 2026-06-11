//go:build e2e
// +build e2e

package harness

import "strings"

// scrubRuneEnv drops every RUNE_* variable from env so the developer's
// shell (RUNE_TOKEN, RUNE_LOG_LEVEL, RUNE_SERVER_GRPC_ADDRESS, …)
// cannot leak into spawned servers or CLI invocations. The harness
// re-adds the variables it owns explicitly.
func scrubRuneEnv(env []string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if strings.HasPrefix(kv, "RUNE_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}
