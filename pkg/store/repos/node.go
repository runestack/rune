// Package repos — Node inventory repository.
package repos

import (
	"context"
	"fmt"
	"time"

	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
)

// Node is cluster-scoped: rows live under an empty namespace key, the
// same pattern StorageClass uses.
const nodeNamespaceKey = ""

// NodeRepo provides CRUD over the node inventory record — what hardware
// a machine has, written by that machine's own agent.
//
// Nothing reaps rows. A node writes its own record under its own ID and
// keeps that ID for life, so on a single-node install there is exactly
// one row forever. Regenerating node-identity.json (deleting the data
// directory but keeping the store, say) strands the old row, which then
// reports a machine that no longer exists. Whatever adds nodes to a
// cluster owes a removal path.
//
// # Why this repo never uses UpdateFunc
//
// The inventory record has exactly ONE writer: the agent that owns the
// node. There is no read-modify-write race to protect against, so Upsert
// does a whole-object Update rather than a store.UpdateFunc CAS.
//
// That remains true if a second writer ever appears, but it is no longer
// the only reason: UpdateFunc used to re-read into an un-zeroed target on
// retry, so any `omitempty` field absent from the fresh JSON kept the
// discarded attempt's value. types.Node has six such fields. It now
// zeroes the target first, and node_test.go pins that.
//
// A future second writer therefore needs a CAS path but not a tag audit.
type NodeRepo struct {
	base *BaseRepo[types.Node]
}

// NewNodeRepo constructs a NodeRepo.
func NewNodeRepo(core store.Store) *NodeRepo {
	return &NodeRepo{
		base: NewBaseRepo[types.Node](core, types.ResourceTypeNode),
	}
}

// Get retrieves a node record by ID.
func (r *NodeRepo) Get(ctx context.Context, id string) (*types.Node, error) {
	if id == "" {
		return nil, fmt.Errorf("node: id is required")
	}
	return r.base.Get(ctx, nodeNamespaceKey, id)
}

// List returns every node record.
func (r *NodeRepo) List(ctx context.Context) ([]*types.Node, error) {
	return r.base.List(ctx, nodeNamespaceKey)
}

// Upsert writes the node record, creating it on first boot and replacing
// it wholesale afterwards. CreatedAt is carried over from the stored row
// so a restart does not reset it.
//
// The write goes through Node.Validate(), which requires a non-empty
// Address — a validation function the write path skips is a validation
// function that stops being true, so the caller supplies one rather than
// routing around it.
func (r *NodeRepo) Upsert(ctx context.Context, node *types.Node) error {
	if node == nil || node.ID == "" {
		return fmt.Errorf("node: id is required")
	}
	if node.Name == "" {
		node.Name = node.ID
	}
	if err := node.Validate(); err != nil {
		return err
	}

	existing, err := r.base.Get(ctx, nodeNamespaceKey, node.ID)
	if err != nil {
		if !store.IsNotFoundError(err) {
			return err
		}
		if node.CreatedAt.IsZero() {
			node.CreatedAt = time.Now()
		}
		return r.base.Create(ctx, nodeNamespaceKey, node.ID, node)
	}

	node.CreatedAt = existing.CreatedAt
	if node.CreatedAt.IsZero() {
		node.CreatedAt = time.Now()
	}
	return r.base.Update(ctx, nodeNamespaceKey, node.ID, node)
}

// Delete removes a node record.
func (r *NodeRepo) Delete(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("node: id is required")
	}
	return r.base.Delete(ctx, nodeNamespaceKey, id)
}
