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
		return ErrInventoryUnknown
	}
	if node.DevicesProbedAt == nil {
		return ErrInventoryUnknown
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
	if cerr := a.store.Create(ctx, types.ResourceTypeNodeLedger, "", nodeID,
		&types.NodeDeviceLedger{NodeID: nodeID, Reservations: []types.GPURes{}}); cerr != nil {
		// Lost a race to create it — the retry below picks up the winner.
		if !store.IsAlreadyExistsError(cerr) {
			return cerr
		}
	}
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
// One per device is the whole contract, and getting it wrong is silent
// ledger corruption rather than an error. A scale-2 service sharing one
// card has two unbound rows for the same (namespace, service, device); a
// bind that stamped every match would put one instance's ID on both, and
// releasing that instance would then drop the OTHER instance's
// reservation too. The card would read as free while an engine was still
// holding its memory on it.
//
// It is an error, not a no-op, when a device has no unbound row to bind:
// the caller believes it holds a reservation there, and silence would let
// that belief stand.
func (a *Admitter) BindInstance(ctx context.Context, nodeID, namespace, service, instanceID string, devices []string) error {
	if instanceID == "" {
		return fmt.Errorf("gpu: bind requires an instance ID")
	}
	if len(devices) == 0 {
		return fmt.Errorf("gpu: bind requires at least one device")
	}

	var unbound []string
	err := a.mutate(ctx, nodeID, func(l *types.NodeDeviceLedger) bool {
		unbound = nil
		changed := false
		for _, want := range devices {
			bound := false
			for i := range l.Reservations {
				r := &l.Reservations[i]
				if r.DeviceUUID != want || r.InstanceID != "" ||
					r.Holder != types.GPUResHolderInstance ||
					r.Namespace != namespace || r.ServiceName != service {
					continue
				}
				r.InstanceID = instanceID
				changed, bound = true, true
				break // exactly one row per device
			}
			if !bound {
				unbound = append(unbound, want)
			}
		}
		return changed
	})
	if err != nil {
		return err
	}
	if len(unbound) > 0 {
		return fmt.Errorf("gpu: no unbound reservation for %s/%s on device(s) %s",
			namespace, service, strings.Join(unbound, ", "))
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
	if node == nil {
		return Placement{}, ErrInventoryUnknown
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
	var ledger types.NodeDeviceLedger
	return a.updateLedger(ctx, nodeID, &ledger, func() error {
		if !fn(&ledger) {
			return store.ErrSkipUpdate
		}
		ledger.NodeID = nodeID
		ledger.UpdatedAt = time.Now().UTC()
		return nil
	})
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
var ErrInventoryUnknown = &AdmissionError{
	Reason:  "GpuInventoryUnknown",
	Message: "waiting for this node's device inventory",
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
