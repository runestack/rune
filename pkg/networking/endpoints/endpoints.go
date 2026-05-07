// Package endpoints owns the OrderedLog op types that publish service
// endpoint sets through the seam established in RUNE-039.
//
// Endpoints are keyed by service ID. Each UpdateEndpoints op replaces
// the entire endpoint set for that service (last-writer-wins per
// service ID). DeleteEndpoints removes the entry entirely. The
// dataplane subsystem (RUNE-041) is the primary consumer: it watches
// the OrderedLog stream, filters mutations under the protected
// "endpoints/" prefix, and reconciles its in-memory cache + per-VIP
// proxy listeners.
//
// The producer is intentionally not the orchestrator (yet). The
// instance/health controllers will start calling Publisher.Update
// once the LocalInstances pipeline lands in RUNE-063. For now, this
// package provides:
//
//   - The op types + appliers (Register on a backend);
//   - The Publisher API for any owner of the endpoint set;
//   - A reverse Reader so consumers can hydrate their cache after a
//     snapshot recovery without re-replaying the entire log.
//
// Key prefix: "endpoints/<serviceID>" — protected by the seam-leakage
// lint in scripts/check_orderedlog_seam.sh.
package endpoints

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
	OpUpdateEndpoints = "endpoints.update"
	OpDeleteEndpoints = "endpoints.delete"

	// keyPrefix is the protected Badger key prefix owned by the
	// endpoints applier. Mirrors the seam-lint enforcement.
	keyPrefix = "endpoints/"

	// resourceType marks the Mutation.ResourceType field so consumers
	// can cheaply filter without unmarshalling.
	resourceType = "endpoints"
)

// ErrServiceIDRequired is returned when an op is constructed with an
// empty service ID. The applier rejects the same condition.
var ErrServiceIDRequired = errors.New("endpoints: service ID required")

// Publisher is the public producer API. The orchestrator instance/
// health controllers call this whenever the endpoint set for a
// service changes (instance Ready, instance Stopped, health flip).
//
// Implementations must be safe for concurrent use; the default
// implementation backed by orderedlog.OrderedLog is.
type Publisher interface {
	// Update replaces the endpoint set for serviceID. Empty endpoints
	// is allowed and means "this service has no ready instances right
	// now"; the data plane will fail-closed on connections to its VIP
	// in that case. To remove the entry entirely (e.g. service
	// deletion), use Delete instead.
	Update(ctx context.Context, serviceID string, endpoints []types.Endpoint) error

	// Delete removes the endpoint set for serviceID. Idempotent.
	Delete(ctx context.Context, serviceID string) error
}

// olPublisher implements Publisher on top of an OrderedLog backend.
type olPublisher struct {
	olog orderedlog.OrderedLog
}

// NewPublisher returns a Publisher that writes endpoint mutations
// through olog.Propose. The caller must have already called Register
// on the same olog (otherwise the proposes return ErrUnknownOpType).
func NewPublisher(olog orderedlog.OrderedLog) Publisher {
	return &olPublisher{olog: olog}
}

func (p *olPublisher) Update(ctx context.Context, serviceID string, eps []types.Endpoint) error {
	if serviceID == "" {
		return ErrServiceIDRequired
	}
	op := &updateOp{ServiceID: serviceID, Endpoints: eps}
	_, err := p.olog.Propose(ctx, op)
	return err
}

func (p *olPublisher) Delete(ctx context.Context, serviceID string) error {
	if serviceID == "" {
		return ErrServiceIDRequired
	}
	op := &deleteOp{ServiceID: serviceID}
	_, err := p.olog.Propose(ctx, op)
	return err
}

// Register installs the endpoints op types + appliers on the given
// OrderedLog. Idempotent in the sense that it tolerates
// orderedlog.ErrAlreadyRegistered (so multiple consumers in the same
// process — e.g. tests — don't fight). Must be called before any
// Publisher.Update / Delete.
func Register(olog orderedlog.OrderedLog) error {
	if err := olog.Register(OpUpdateEndpoints, applyUpdate, unmarshalUpdate); err != nil &&
		!errors.Is(err, orderedlog.ErrAlreadyRegistered) {
		return fmt.Errorf("endpoints: register update: %w", err)
	}
	if err := olog.Register(OpDeleteEndpoints, applyDelete, unmarshalDelete); err != nil &&
		!errors.Is(err, orderedlog.ErrAlreadyRegistered) {
		return fmt.Errorf("endpoints: register delete: %w", err)
	}
	return nil
}

// --- Op types ----------------------------------------------------------------

type updateOp struct {
	ServiceID string           `json:"serviceId"`
	Endpoints []types.Endpoint `json:"endpoints"`
}

func (o *updateOp) OpType() string         { return OpUpdateEndpoints }
func (o *updateOp) Marshal() ([]byte, error) { return json.Marshal(o) }

func unmarshalUpdate(raw []byte) (orderedlog.Op, error) {
	o := &updateOp{}
	if err := json.Unmarshal(raw, o); err != nil {
		return nil, fmt.Errorf("endpoints: unmarshal update: %w", err)
	}
	return o, nil
}

type deleteOp struct {
	ServiceID string `json:"serviceId"`
}

func (o *deleteOp) OpType() string         { return OpDeleteEndpoints }
func (o *deleteOp) Marshal() ([]byte, error) { return json.Marshal(o) }

func unmarshalDelete(raw []byte) (orderedlog.Op, error) {
	o := &deleteOp{}
	if err := json.Unmarshal(raw, o); err != nil {
		return nil, fmt.Errorf("endpoints: unmarshal delete: %w", err)
	}
	return o, nil
}

// --- Appliers ----------------------------------------------------------------

func applyUpdate(tx orderedlog.Txn, op orderedlog.Op) ([]orderedlog.Mutation, error) {
	uo, ok := op.(*updateOp)
	if !ok {
		return nil, fmt.Errorf("endpoints: apply update: unexpected op type %T", op)
	}
	if uo.ServiceID == "" {
		return nil, ErrServiceIDRequired
	}
	se := types.ServiceEndpoints{
		ServiceID: uo.ServiceID,
		Endpoints: append([]types.Endpoint(nil), uo.Endpoints...),
	}
	payload, err := json.Marshal(&se)
	if err != nil {
		return nil, fmt.Errorf("endpoints: marshal payload: %w", err)
	}
	key := []byte(keyPrefix + uo.ServiceID)
	if err := tx.Set(key, payload); err != nil {
		return nil, fmt.Errorf("endpoints: persist: %w", err)
	}
	return []orderedlog.Mutation{{
		Kind:         orderedlog.MutationPut,
		ResourceType: resourceType,
		Name:         uo.ServiceID,
		Payload:      payload,
	}}, nil
}

func applyDelete(tx orderedlog.Txn, op orderedlog.Op) ([]orderedlog.Mutation, error) {
	do, ok := op.(*deleteOp)
	if !ok {
		return nil, fmt.Errorf("endpoints: apply delete: unexpected op type %T", op)
	}
	if do.ServiceID == "" {
		return nil, ErrServiceIDRequired
	}
	key := []byte(keyPrefix + do.ServiceID)
	if err := tx.Delete(key); err != nil {
		return nil, fmt.Errorf("endpoints: delete: %w", err)
	}
	return []orderedlog.Mutation{{
		Kind:         orderedlog.MutationDelete,
		ResourceType: resourceType,
		Name:         do.ServiceID,
	}}, nil
}

// IsEndpointsMutation reports whether m was produced by this package.
// Consumers (e.g. the dataplane watch loop) use this to filter the
// stream cheaply.
func IsEndpointsMutation(m orderedlog.Mutation) bool {
	return m.ResourceType == resourceType
}

// DecodePayload unmarshals an UpdateEndpoints mutation payload into a
// ServiceEndpoints. Returns an error for empty or delete mutations.
func DecodePayload(m orderedlog.Mutation) (types.ServiceEndpoints, error) {
	if m.ResourceType != resourceType {
		return types.ServiceEndpoints{}, fmt.Errorf("endpoints: not an endpoints mutation: %q", m.ResourceType)
	}
	if m.Kind != orderedlog.MutationPut {
		return types.ServiceEndpoints{}, fmt.Errorf("endpoints: not a put mutation: %v", m.Kind)
	}
	var se types.ServiceEndpoints
	if err := json.Unmarshal(m.Payload, &se); err != nil {
		return types.ServiceEndpoints{}, fmt.Errorf("endpoints: decode payload: %w", err)
	}
	return se, nil
}
