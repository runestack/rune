package gpu

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
)

// Admitter places GPU requests against a node's ledger.
//
// "Admission" here is about CAPACITY — can the hardware hold this? The
// orchestrator has a second gate also called admission (its `admission`
// field, an authz.Gate) which is about AUTHORIZATION — may this caller
// ask for it? That one runs on CreateService today; this one is not yet
// wired to anything. They will compose rather than overlap: a caller may
// be entitled to something the devices cannot hold, and the devices may
// have room for something the caller may not have.
type Admitter struct {
	store store.Store
}

// NewAdmitter constructs an Admitter over the given store.
func NewAdmitter(st store.Store) *Admitter { return &Admitter{store: st} }

// Request is one workload's claim on a node's devices.
type Request struct {
	NodeID      string
	Namespace   string
	ServiceName string
	InstanceID  string // may be empty; see types.GPUResHolder
	Holder      types.GPUResHolder
	GPU         types.GPURequest
}

// Reserve places req and records the claim. THE WRITE IS THE CHECK.
//
// Capacity is decided inside the store's read-modify-write transaction,
// so it is re-decided against current state on every retry. That is not
// belt and braces — it is the only correct shape here. The reconcile
// workqueue guarantees exclusivity per SERVICE key, not per device, so
// four workers can hold four different services and land on the same
// card. A read-then-write would have each of them read zero requested
// bytes, each conclude it fits, and each admit.
//
// The mutate is a pure function of (ledger, req) and writes nothing
// outside the target, which is what the store requires of a callback it
// may invoke more than once.
//
// NOT SAFE TO RE-RUN for the same instance: a second call appends a
// second row. BindInstance next door IS idempotent, and the asymmetry is
// a trap for a level-triggered caller that learns the habit there and
// carries it here. Callers must reserve once and remember they did.
func (a *Admitter) Reserve(ctx context.Context, node *types.Node, req Request) (Placement, error) {
	if err := checkInventory(node, req.NodeID); err != nil {
		return Placement{}, err
	}
	if req.Holder == "" {
		req.Holder = types.GPUResHolderInstance
	}

	var (
		ledger types.NodeDeviceLedger
		placed Placement
	)
	err := a.updateLedger(ctx, req.NodeID, &ledger, func() error {
		// Re-derived on every attempt, against whatever the ledger holds
		// now. Nothing from a previous attempt may influence this.
		placed = Placement{}

		p, err := ChooseDevices(node.Devices, &ledger, req.Namespace, req.GPU)
		if err != nil {
			return err // no write
		}
		appendReservations(&ledger, p, req)
		placed = p
		return nil
	})
	if err != nil {
		return Placement{}, err
	}
	return placed, nil
}

// checkInventory rejects the states in which no capacity answer exists
// yet, distinguishing "not known" from "none".
//
// The difference is not pedantry. A capacity refusal is terminal: it
// writes a Failed tombstone that is retained for an hour. "Not probed
// yet" is a window every restart passes through, because the agent starts
// after the control plane — so treating it as a refusal writes
// driver-blaming errors and hour-long tombstones on every upgrade, for a
// problem that does not exist.
func checkInventory(node *types.Node, nodeID string) error {
	if node == nil {
		return ErrInventoryUnknown()
	}
	if node.DevicesProbedAt == nil {
		return ErrInventoryUnknown()
	}
	// The ledger is keyed by nodeID and the devices come from node. If
	// they disagree, the claim is recorded against one machine's capacity
	// and checked against another's — silently.
	if nodeID != "" && node.ID != "" && nodeID != node.ID {
		return fmt.Errorf("gpu: node mismatch — inventory is for %q, ledger key is %q", node.ID, nodeID)
	}
	return nil
}

// appendReservations records one row per assigned device.
func appendReservations(l *types.NodeDeviceLedger, p Placement, req Request) {
	now := time.Now().UTC()
	for _, uuid := range p.DeviceUUIDs {
		l.Reservations = append(l.Reservations, types.GPURes{
			DeviceUUID:  uuid,
			Namespace:   req.Namespace,
			ServiceName: req.ServiceName,
			InstanceID:  req.InstanceID,
			VRAMBytes:   vramBytes(req.GPU),
			WholeDevice: !req.GPU.SharesDevice(),
			Holder:      req.Holder,
			CreatedAt:   now,
		})
	}
	l.NodeID = req.NodeID
	l.UpdatedAt = now
}

// updateLedger runs mutate under the ledger CAS, creating the row first
// if it is absent.
//
// The create matters: UpdateFunc returns not-found on an absent key, so
// without it the FIRST reservation on a box fails with a raw store error
// carrying no admission reason. The agent creates this row when it writes
// inventory, but that write is best-effort and a node whose ledger write
// failed would otherwise refuse every GPU workload opaquely until the
// next agent restart.
func (a *Admitter) updateLedger(ctx context.Context, nodeID string, ledger *types.NodeDeviceLedger, mutate func() error) error {
	err := a.store.UpdateFunc(ctx, types.ResourceTypeNodeLedger, "", nodeID, ledger, mutate)
	if err == nil || !store.IsNotFoundError(err) {
		return err
	}
	// A lost create race does NOT necessarily surface as already-exists:
	// Create enrols its existence check in the transaction's read set, so
	// concurrent creators lose on COMMIT with a badger conflict instead.
	// Either way the winner's row is what we want, so the create's error
	// is not interesting — only the retry's is.
	_ = a.store.Create(ctx, types.ResourceTypeNodeLedger, "", nodeID,
		&types.NodeDeviceLedger{NodeID: nodeID, Reservations: []types.GPURes{}})
	return a.store.UpdateFunc(ctx, types.ResourceTypeNodeLedger, "", nodeID, ledger, mutate)
}

// Release drops every reservation held by an instance.
//
// Released on TERMINAL STATUS, never on record deletion: a Failed
// instance record is deliberately kept as its own tombstone for up to an
// hour, so releasing on deletion would let a crash-looping engine hold
// its VRAM for that hour — and block its own replacement. The most
// common failure mode there is would become a self-inflicted outage.
//
// It must also be prompt rather than swept. A dipping rolling update
// retires a replica and creates its replacement in the same reconcile
// pass, so the bytes have to be free by the time the replacement is
// admitted; a GC sweep would make every dipping step wait a full tick.
func (a *Admitter) Release(ctx context.Context, nodeID, instanceID string) error {
	if instanceID == "" {
		return fmt.Errorf("gpu: release requires an instance ID")
	}
	return a.mutate(ctx, nodeID, func(l *types.NodeDeviceLedger) bool {
		return dropWhere(l, func(r types.GPURes) bool {
			return r.InstanceID == instanceID && r.Holder == types.GPUResHolderInstance
		})
	})
}

// ReleaseService drops every reservation held by a service, whatever the
// holder. Used when the service itself is going away, where an idle hold
// has no owner left to reclaim it.
func (a *Admitter) ReleaseService(ctx context.Context, nodeID, namespace, service string) error {
	return a.mutate(ctx, nodeID, func(l *types.NodeDeviceLedger) bool {
		return dropWhere(l, func(r types.GPURes) bool {
			return r.Namespace == namespace && r.ServiceName == service
		})
	})
}

// BindInstance fills in the instance ID on reservations written before
// the instance record existed — exactly ONE row per named device.
//
// One per device is the contract, and getting it wrong is silent ledger
// corruption rather than an error. A scale-2 service sharing one card has
// two unbound rows for the same (namespace, service, device); a bind that
// stamped every match would put one instance's ID on both, and releasing
// that instance would drop the other's reservation too — the card reading
// as free while an engine still holds memory on it.
//
// IDEMPOTENT, and that is not a nicety. A row already bound to THIS
// instance satisfies its device. Without that, a second call for the same
// instance — a retried reconcile, a store error after the commit landed,
// any level-triggered step that re-derives its work — skips its own row,
// finds a SIBLING's unbound row, and stamps itself onto that instead.
// Re-running would be worse than failing.
//
// ALL-OR-NOTHING. If any named device has no row to bind, nothing is
// written and the call errors. A partial commit would leave the caller
// with a half-bound instance and no safe way to retry, since retrying is
// what steals the sibling.
func (a *Admitter) BindInstance(ctx context.Context, nodeID, namespace, service, instanceID string, devices []string) error {
	if instanceID == "" {
		return fmt.Errorf("gpu: bind requires an instance ID")
	}
	if len(devices) == 0 {
		return fmt.Errorf("gpu: bind requires at least one device")
	}
	// A device may appear once: two rows on one card belong to two
	// instances, bound by two calls. A repeat here is a caller bug that
	// would otherwise bind a sibling's row.
	seen := map[string]bool{}
	for _, d := range devices {
		if seen[d] {
			return fmt.Errorf("gpu: device %s named twice in one bind", d)
		}
		seen[d] = true
	}

	// Seeded with every device, not nil. `mutate` maps a missing ledger to
	// success — right for a release, where "no ledger" and "no matching
	// row" are the same answer — but that means the closure below never
	// runs, and a nil `unmatched` would report a bind that bound nothing
	// as a success. Bind is a positive claim; absence has to read as
	// "nothing matched".
	unmatched := append([]string(nil), devices...)
	err := a.mutate(ctx, nodeID, func(l *types.NodeDeviceLedger) bool {
		unmatched = nil
		// Resolve every device before writing anything.
		targets := make([]int, 0, len(devices))
		for _, want := range devices {
			idx := -1
			for i := range l.Reservations {
				r := &l.Reservations[i]
				if r.DeviceUUID != want || r.Holder != types.GPUResHolderInstance ||
					r.Namespace != namespace || r.ServiceName != service {
					continue
				}
				if r.InstanceID == instanceID {
					// Already ours: this device is satisfied, and we must
					// not go looking for another row to claim.
					idx = -2
					break
				}
				if r.InstanceID == "" && idx == -1 {
					idx = i
				}
			}
			switch idx {
			case -1:
				unmatched = append(unmatched, want)
			case -2: // already bound to this instance
			default:
				targets = append(targets, idx)
			}
		}
		if len(unmatched) > 0 || len(targets) == 0 {
			return false
		}
		for _, i := range targets {
			l.Reservations[i].InstanceID = instanceID
		}
		return true
	})
	if err != nil {
		return err
	}
	if len(unmatched) > 0 {
		return fmt.Errorf("gpu: no unbound reservation for %s/%s on device(s) %s",
			namespace, service, strings.Join(unmatched, ", "))
	}
	return nil
}

// ReserveReplacing releases one instance's reservations and takes new
// ones in a SINGLE ledger write.
//
// A dipping rolling update retires a replica and creates its replacement
// in the same reconcile pass, so doing this as release-then-reserve opens
// a window where the freed bytes are visible to a concurrent admission
// that should not get them — and doubles the writes on a record that is
// already the one hot key every GPU transition on the node contends for.
func (a *Admitter) ReserveReplacing(ctx context.Context, node *types.Node, releaseInstanceID string, req Request) (Placement, error) {
	// The same guard Reserve uses, and for a sharper reason here: this is
	// the rolling-update path, so the restart window it protects — the
	// agent starting after the control plane — is exactly when a rollout
	// in flight arrives.
	if err := checkInventory(node, req.NodeID); err != nil {
		return Placement{}, err
	}
	if req.Holder == "" {
		req.Holder = types.GPUResHolderInstance
	}

	var (
		ledger types.NodeDeviceLedger
		placed Placement
	)
	err := a.updateLedger(ctx, req.NodeID, &ledger, func() error {
		placed = Placement{}
		if releaseInstanceID != "" {
			dropWhere(&ledger, func(r types.GPURes) bool {
				return r.InstanceID == releaseInstanceID && r.Holder == types.GPUResHolderInstance
			})
		}
		p, err := ChooseDevices(node.Devices, &ledger, req.Namespace, req.GPU)
		if err != nil {
			return err
		}
		appendReservations(&ledger, p, req)
		placed = p
		return nil
	})
	if err != nil {
		return Placement{}, err
	}
	return placed, nil
}

// mutate applies fn under the ledger CAS, skipping the write when fn
// reports nothing changed. Skipping matters: this record is one hot key
// that every GPU transition on the node contends for, and every write
// also appends an unpruned version-history row.
func (a *Admitter) mutate(ctx context.Context, nodeID string, fn func(*types.NodeDeviceLedger) bool) error {
	// NOT updateLedger: releasing against a node with no ledger must not
	// conjure one. There is nothing to release, and creating a row for
	// any node ID a caller names grows the orphan set from both ends.
	var ledger types.NodeDeviceLedger
	err := a.store.UpdateFunc(ctx, types.ResourceTypeNodeLedger, "", nodeID, &ledger, func() error {
		if !fn(&ledger) {
			return store.ErrSkipUpdate
		}
		ledger.NodeID = nodeID
		ledger.UpdatedAt = time.Now().UTC()
		return nil
	})
	if err != nil && store.IsNotFoundError(err) {
		return nil // no ledger, nothing held, nothing to release
	}
	return err
}

// CanFit reports whether req could be placed right now. ADVISORY ONLY —
// it never writes.
//
// A rolling update needs to know whether a replacement will fit BEFORE it
// retires the instance it is replacing, and a wake needs to know before
// it queues anything. Both need a question, not a claim.
//
// This is a TOCTOU pattern and that is acceptable here for exactly one
// reason: Reserve re-checks inside its transaction, so a stale yes costs
// a clean "doesn't fit" error rather than an overcommit. A caller that
// treats a CanFit pass as a guarantee — or worse, skips Reserve because
// of it — is a bug.
func CanFit(node *types.Node, ledger *types.NodeDeviceLedger, namespace string, req types.GPURequest) (Placement, error) {
	if err := checkInventory(node, ""); err != nil {
		return Placement{}, err
	}
	return ChooseDevices(node.Devices, ledger, namespace, req)
}

// ErrInventoryUnknown means the node has not reported its devices yet.
// It is RETRYABLE and must never be surfaced as a capacity refusal: it is
// a statement about what is known, not about what exists.
//
// A function rather than a package-level value: an exported *AdmissionError
// is mutable, and one caller writing through it would change the error
// every other caller sees.
func ErrInventoryUnknown() error {
	return &AdmissionError{
		Reason:  types.GPUReasonInventoryUnknown,
		Message: "waiting for this node's device inventory",
	}
}

func vramBytes(req types.GPURequest) int64 {
	if !req.SharesDevice() {
		return 0
	}
	v, err := types.ParseMemory(req.VRAM)
	if err != nil {
		return 0
	}
	return v
}

func dropWhere(l *types.NodeDeviceLedger, match func(types.GPURes) bool) bool {
	kept := l.Reservations[:0]
	dropped := false
	for _, r := range l.Reservations {
		if match(r) {
			dropped = true
			continue
		}
		kept = append(kept, r)
	}
	l.Reservations = kept
	return dropped
}
