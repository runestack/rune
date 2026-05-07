package dataplane

import (
	"math/rand"

	"github.com/runestack/rune/pkg/types"
)

// LocalityPreference values mirror Service.Discovery.LocalityPreference.
const (
	LocalityNone        = ""
	LocalityPreferLocal = "prefer-local"
	LocalityLocalOnly   = "local-only"

	// metaNodeID is the conventional Endpoint.Metadata key the
	// orchestrator (RUNE-063) will populate with the node ID hosting
	// the underlying instance. The dataplane treats a missing key as
	// "remote" — locality selection is best-effort, never fails open.
	metaNodeID = "node_id"
)

// selectEndpoint picks one endpoint from healthy according to pref +
// localNodeID. Returns the chosen endpoint and ok=false if the policy
// produced an empty candidate set (caller fails closed).
//
// Selection inside the candidate set is uniform random; weighted
// selection is reserved for a future iteration where Endpoint.Metadata
// carries an explicit "weight". For now all healthy endpoints are
// equally weighted.
func selectEndpoint(healthy []types.Endpoint, pref string, localNodeID string, rng *rand.Rand) (types.Endpoint, bool) {
	if len(healthy) == 0 {
		return types.Endpoint{}, false
	}
	switch pref {
	case LocalityLocalOnly:
		local := filterLocal(healthy, localNodeID)
		if len(local) == 0 {
			return types.Endpoint{}, false
		}
		return pick(local, rng), true
	case LocalityPreferLocal:
		local := filterLocal(healthy, localNodeID)
		if len(local) > 0 {
			return pick(local, rng), true
		}
		return pick(healthy, rng), true
	default: // LocalityNone or unknown
		return pick(healthy, rng), true
	}
}

func filterLocal(eps []types.Endpoint, nodeID string) []types.Endpoint {
	if nodeID == "" {
		return nil
	}
	out := make([]types.Endpoint, 0, len(eps))
	for _, e := range eps {
		if e.Metadata != nil && e.Metadata[metaNodeID] == nodeID {
			out = append(out, e)
		}
	}
	return out
}

func pick(eps []types.Endpoint, rng *rand.Rand) types.Endpoint {
	if rng == nil {
		// Defensive default; callers always pass a seeded rng.
		return eps[0]
	}
	return eps[rng.Intn(len(eps))]
}
