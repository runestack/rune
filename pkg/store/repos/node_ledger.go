// Package repos — GPU device ledger repository.
package repos

import (
	"context"
	"fmt"

	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
)

// The ledger is cluster-scoped and keyed by node ID, like the node
// record it accompanies.
const nodeLedgerNamespaceKey = ""

// NodeLedgerRepo provides reads and existence-creation for a node's GPU
// device ledger.
//
// # Reservations are not written through this repo
//
// There is no Update here on purpose. Reservations are mutated by
// pkg/orchestrator/gpu under store.UpdateFunc, because the capacity check
// has to happen INSIDE the transaction that records the claim — several
// reconcile workers can decide about the same device at once, and any
// read-then-write lets each of them see room that another is about to
// take. A convenience Update on this repo would be an inviting way to
// write exactly that bug, so it does not exist.
type NodeLedgerRepo struct {
	base *BaseRepo[types.NodeDeviceLedger]
}

// NewNodeLedgerRepo constructs a NodeLedgerRepo.
func NewNodeLedgerRepo(core store.Store) *NodeLedgerRepo {
	return &NodeLedgerRepo{
		base: NewBaseRepo[types.NodeDeviceLedger](core, types.ResourceTypeNodeLedger),
	}
}

// Get retrieves a node's ledger.
func (r *NodeLedgerRepo) Get(ctx context.Context, nodeID string) (*types.NodeDeviceLedger, error) {
	if nodeID == "" {
		return nil, fmt.Errorf("node ledger: node id is required")
	}
	return r.base.Get(ctx, nodeLedgerNamespaceKey, nodeID)
}

// List returns every node's ledger.
func (r *NodeLedgerRepo) List(ctx context.Context) ([]*types.NodeDeviceLedger, error) {
	return r.base.List(ctx, nodeLedgerNamespaceKey)
}

// EnsureExists creates an empty ledger for the node if none exists, and
// leaves an existing one untouched.
//
// This is not tidiness. store.UpdateFunc returns not-found on an absent
// key, so without a row already present the FIRST reservation on every
// box fails with a raw store error rather than an admission decision.
// The agent calls this when it writes the node's inventory, which is the
// one moment we know the node exists and are already writing about it.
func (r *NodeLedgerRepo) EnsureExists(ctx context.Context, nodeID string) error {
	if nodeID == "" {
		return fmt.Errorf("node ledger: node id is required")
	}
	if _, err := r.base.Get(ctx, nodeLedgerNamespaceKey, nodeID); err == nil {
		return nil
	} else if !store.IsNotFoundError(err) {
		return err
	}
	return r.base.Create(ctx, nodeLedgerNamespaceKey, nodeID, &types.NodeDeviceLedger{
		NodeID:       nodeID,
		Reservations: []types.GPURes{},
	})
}

// Delete removes a node's ledger.
func (r *NodeLedgerRepo) Delete(ctx context.Context, nodeID string) error {
	if nodeID == "" {
		return fmt.Errorf("node ledger: node id is required")
	}
	return r.base.Delete(ctx, nodeLedgerNamespaceKey, nodeID)
}
