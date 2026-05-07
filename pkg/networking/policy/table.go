// Package policy: LocalInstances table.
//
// The agent maintains a containerIP -> (service, namespace) lookup
// table populated from the local_instances/ watch stream produced
// by the orchestrator (RUNE-063). The table is the source of truth
// for "is this peer same-node, and if so what's its identity?"
// during ingress policy evaluation.
//
// Concurrency: Apply / Remove are infrequent (one per orchestration
// step on this node); Lookup is hot (per inbound connection).
// We keep a single map under an RWMutex — the map is small (one
// entry per local container).

package policy

import (
	"net"
	"sync"

	"github.com/runestack/rune/pkg/types"
)

// LocalInstancesTable is a concurrent IP -> InstanceIdentity index.
//
// Records are partitioned by node ID to make Apply / Remove cheap:
// when a node's local-instances record changes, the table replaces
// only that node's entries. The query path flattens the partitions
// behind the lock.
type LocalInstancesTable struct {
	mu sync.RWMutex
	// nodes maps nodeID -> (containerIP -> identity).
	nodes map[string]map[string]types.InstanceIdentity
	// flat is a derived single-level index for fast lookup. It is
	// rebuilt on every mutation (rare); the trade-off favours the
	// hot read path.
	flat map[string]types.InstanceIdentity
	// localNodeID, when non-empty, lets SameNode flag lookups.
	localNodeID string
	// reverseNode is the inverse of flat: identity-IP -> owning
	// node, used by SameNode without extra allocation.
	reverseNode map[string]string
}

// NewLocalInstancesTable returns an empty table. localNodeID is the
// agent's node identity; SameNode(ip) returns true only for IPs
// owned by that node.
func NewLocalInstancesTable(localNodeID string) *LocalInstancesTable {
	return &LocalInstancesTable{
		nodes:       map[string]map[string]types.InstanceIdentity{},
		flat:        map[string]types.InstanceIdentity{},
		reverseNode: map[string]string{},
		localNodeID: localNodeID,
	}
}

// Apply replaces the entries for li.NodeID with li.Instances. An
// empty li.Instances clears the node's entries. Returns the new
// total entry count for diagnostics.
func (t *LocalInstancesTable) Apply(li types.LocalInstances) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	if li.NodeID == "" {
		return len(t.flat)
	}
	t.nodes[li.NodeID] = cloneIdents(li.Instances)
	t.rebuildLocked()
	return len(t.flat)
}

// Remove drops all entries for nodeID. Idempotent.
func (t *LocalInstancesTable) Remove(nodeID string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.nodes[nodeID]; !ok {
		return len(t.flat)
	}
	delete(t.nodes, nodeID)
	t.rebuildLocked()
	return len(t.flat)
}

// Lookup returns the identity for ip (and whether it was found).
// ip is matched as a string — callers should pass ip.String().
func (t *LocalInstancesTable) Lookup(ip net.IP) (types.InstanceIdentity, bool) {
	if ip == nil {
		return types.InstanceIdentity{}, false
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	id, ok := t.flat[ip.String()]
	return id, ok
}

// SameNode reports whether ip belongs to a container hosted on this
// agent's node.
func (t *LocalInstancesTable) SameNode(ip net.IP) bool {
	if ip == nil || t.localNodeID == "" {
		return false
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	owner, ok := t.reverseNode[ip.String()]
	return ok && owner == t.localNodeID
}

// PeerInfoFor builds a PeerInfo for an inbound source IP, attaching
// identity + same-node flag from the table.
func (t *LocalInstancesTable) PeerInfoFor(ip net.IP) PeerInfo {
	if ip == nil {
		return PeerInfo{}
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	pi := PeerInfo{IP: ip}
	if id, ok := t.flat[ip.String()]; ok {
		idCopy := id
		pi.Identity = &idCopy
	}
	if owner, ok := t.reverseNode[ip.String()]; ok && owner == t.localNodeID {
		pi.SameNode = true
	}
	return pi
}

// Size returns the total number of entries across all nodes. For
// metrics.
func (t *LocalInstancesTable) Size() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.flat)
}

func (t *LocalInstancesTable) rebuildLocked() {
	flat := make(map[string]types.InstanceIdentity, 16)
	rev := make(map[string]string, 16)
	for nodeID, m := range t.nodes {
		for ip, id := range m {
			flat[ip] = id
			rev[ip] = nodeID
		}
	}
	t.flat = flat
	t.reverseNode = rev
}

func cloneIdents(in map[string]types.InstanceIdentity) map[string]types.InstanceIdentity {
	out := make(map[string]types.InstanceIdentity, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
