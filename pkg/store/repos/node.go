// Package repos — Node inventory repository (RUNE-301 §6.1).
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
// a machine has, written by that machine's own agent (RUNE-301 §6.1).
//
// # Why this repo never uses UpdateFunc
//
// The inventory record has exactly ONE writer: the agent that owns the
// node. There is no read-modify-write race to protect against, so Upsert
// does a whole-object Update rather than a store.UpdateFunc CAS.
//
// That is deliberate, not incidental. BadgerStore.UpdateFunc unmarshals
// the freshly-read row into the SAME target on every retry without
// zeroing it (pkg/store/badger_store.go, inside the retry loop), and
// encoding/json leaves a field absent from the JSON untouched. Any
// `omitempty` field would therefore keep a discarded attempt's value —
// RUNE-301 D-24 documents the reproduction for the P2 ledger, whose rows
// carry no `omitempty` for exactly this reason. types.Node predates that
// rule and has three `omitempty` fields of its own (Labels,
// StatusReason, StatusMessage), and the P1 fields the design specifies
// (Devices, DevicesProbedAt, DeviceProbeError) add three more.
//
// Whole-object Update sidesteps the hazard instead of relying on tag
// discipline: the caller supplies a fully-populated struct and the store
// marshals it as given, with no un-zeroed re-read anywhere on the path.
// A future second writer must add a CAS path AND revisit the tags
// together — hence this comment rather than a silent choice.
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
// function that stops being true, so the caller supplies one (127.0.0.1
// for the local node) rather than routing around it.
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
