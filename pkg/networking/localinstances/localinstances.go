// Package localinstances owns the OrderedLog op type that publishes
// the per-node container-IP -> (service, namespace) identity table
// the agent uses to attribute incoming connections to a managed
// service for policy enforcement.
//
// Records are keyed by node ID. Each Update op replaces the entire
// table for that node (last-writer-wins), which is the same shape as
// endpoints.Publisher: the orchestrator owns the source-of-truth
// per-node and republishes whenever an instance starts or stops on
// that node.
//
// Key prefix: "local_instances/<nodeID>" — protected by the
// seam-leakage lint in scripts/check_orderedlog_seam.sh.
package localinstances

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/runestack/rune/pkg/store/orderedlog"
	"github.com/runestack/rune/pkg/types"
)

// Reserved op type strings.
const (
	OpUpdateLocalInstances = "local_instances.update"
	OpDeleteLocalInstances = "local_instances.delete"

	keyPrefix    = "local_instances/"
	resourceType = "local_instances"
)

// ErrNodeIDRequired is returned when an op is constructed with an
// empty node ID.
var ErrNodeIDRequired = errors.New("localinstances: node ID required")

// Publisher is the producer API. The orchestrator instance controller
// calls Update whenever the local-instance table for a node changes
// (instance Started or Stopped on that node). Implementations must be
// safe for concurrent use; the default OrderedLog-backed
// implementation is.
type Publisher interface {
	// Update replaces the table for nodeID. An empty map is allowed
	// and means "this node hosts no instances right now".
	Update(ctx context.Context, nodeID string, instances map[string]types.InstanceIdentity) error

	// Delete removes the table for nodeID. Idempotent.
	Delete(ctx context.Context, nodeID string) error
}

type olPublisher struct {
	olog orderedlog.OrderedLog
}

// NewPublisher returns a Publisher that writes through olog.Propose.
// Register must have been called on the same olog beforehand.
func NewPublisher(olog orderedlog.OrderedLog) Publisher {
	return &olPublisher{olog: olog}
}

func (p *olPublisher) Update(ctx context.Context, nodeID string, instances map[string]types.InstanceIdentity) error {
	if nodeID == "" {
		return ErrNodeIDRequired
	}
	cp := make(map[string]types.InstanceIdentity, len(instances))
	for k, v := range instances {
		cp[k] = v
	}
	op := &updateOp{NodeID: nodeID, Instances: cp}
	_, err := p.olog.Propose(ctx, op)
	return err
}

func (p *olPublisher) Delete(ctx context.Context, nodeID string) error {
	if nodeID == "" {
		return ErrNodeIDRequired
	}
	op := &deleteOp{NodeID: nodeID}
	_, err := p.olog.Propose(ctx, op)
	return err
}

// Register installs the local_instances op types + appliers on olog.
// Tolerates orderedlog.ErrAlreadyRegistered so multiple consumers in
// the same process can call it.
func Register(olog orderedlog.OrderedLog) error {
	if err := olog.Register(OpUpdateLocalInstances, applyUpdate, unmarshalUpdate); err != nil &&
		!errors.Is(err, orderedlog.ErrAlreadyRegistered) {
		return fmt.Errorf("localinstances: register update: %w", err)
	}
	if err := olog.Register(OpDeleteLocalInstances, applyDelete, unmarshalDelete); err != nil &&
		!errors.Is(err, orderedlog.ErrAlreadyRegistered) {
		return fmt.Errorf("localinstances: register delete: %w", err)
	}
	return nil
}

// --- Op types ----------------------------------------------------------------

type updateOp struct {
	NodeID    string                          `json:"nodeId"`
	Instances map[string]types.InstanceIdentity `json:"instances"`
}

func (o *updateOp) OpType() string           { return OpUpdateLocalInstances }
func (o *updateOp) Marshal() ([]byte, error) { return json.Marshal(o) }

func unmarshalUpdate(raw []byte) (orderedlog.Op, error) {
	o := &updateOp{}
	if err := json.Unmarshal(raw, o); err != nil {
		return nil, fmt.Errorf("localinstances: unmarshal update: %w", err)
	}
	return o, nil
}

type deleteOp struct {
	NodeID string `json:"nodeId"`
}

func (o *deleteOp) OpType() string           { return OpDeleteLocalInstances }
func (o *deleteOp) Marshal() ([]byte, error) { return json.Marshal(o) }

func unmarshalDelete(raw []byte) (orderedlog.Op, error) {
	o := &deleteOp{}
	if err := json.Unmarshal(raw, o); err != nil {
		return nil, fmt.Errorf("localinstances: unmarshal delete: %w", err)
	}
	return o, nil
}

// --- Appliers ----------------------------------------------------------------

func applyUpdate(tx orderedlog.Txn, op orderedlog.Op) ([]orderedlog.Mutation, error) {
	uo, ok := op.(*updateOp)
	if !ok {
		return nil, fmt.Errorf("localinstances: apply update: unexpected op type %T", op)
	}
	if uo.NodeID == "" {
		return nil, ErrNodeIDRequired
	}
	li := types.LocalInstances{NodeID: uo.NodeID, Instances: uo.Instances}
	if li.Instances == nil {
		li.Instances = map[string]types.InstanceIdentity{}
	}
	payload, err := json.Marshal(&li)
	if err != nil {
		return nil, fmt.Errorf("localinstances: marshal payload: %w", err)
	}
	key := []byte(keyPrefix + uo.NodeID)
	if err := tx.Set(key, payload); err != nil {
		return nil, fmt.Errorf("localinstances: persist: %w", err)
	}
	return []orderedlog.Mutation{{
		Kind:         orderedlog.MutationPut,
		ResourceType: resourceType,
		Name:         uo.NodeID,
		Payload:      payload,
	}}, nil
}

func applyDelete(tx orderedlog.Txn, op orderedlog.Op) ([]orderedlog.Mutation, error) {
	do, ok := op.(*deleteOp)
	if !ok {
		return nil, fmt.Errorf("localinstances: apply delete: unexpected op type %T", op)
	}
	if do.NodeID == "" {
		return nil, ErrNodeIDRequired
	}
	key := []byte(keyPrefix + do.NodeID)
	if err := tx.Delete(key); err != nil {
		return nil, fmt.Errorf("localinstances: delete: %w", err)
	}
	return []orderedlog.Mutation{{
		Kind:         orderedlog.MutationDelete,
		ResourceType: resourceType,
		Name:         do.NodeID,
	}}, nil
}

// IsLocalInstancesMutation reports whether m was produced by this
// package. Used by consumers to filter the watch stream cheaply.
func IsLocalInstancesMutation(m orderedlog.Mutation) bool {
	return m.ResourceType == resourceType
}

// DecodePayload unmarshals an Update mutation payload into a
// LocalInstances. Returns an error for delete mutations or non-
// local_instances mutations.
func DecodePayload(m orderedlog.Mutation) (types.LocalInstances, error) {
	if m.ResourceType != resourceType {
		return types.LocalInstances{}, fmt.Errorf("localinstances: not a local_instances mutation: %q", m.ResourceType)
	}
	if m.Kind != orderedlog.MutationPut {
		return types.LocalInstances{}, fmt.Errorf("localinstances: not a put mutation: %v", m.Kind)
	}
	var li types.LocalInstances
	if err := json.Unmarshal(m.Payload, &li); err != nil {
		return types.LocalInstances{}, fmt.Errorf("localinstances: decode payload: %w", err)
	}
	return li, nil
}
