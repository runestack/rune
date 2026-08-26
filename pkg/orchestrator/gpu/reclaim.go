package gpu

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
)

// ReclaimOrphans releases instance-held reservations whose instance is
// gone, and reports what it released.
//
// held is every instance ID that may still legitimately hold a row. The
// membership test the caller must apply is "a record exists and is not
// Deleted", NOT "the instance is running": a create that exhausted its
// retries sits at Failed or Stalled holding its card deliberately, since
// the retry path does not re-reserve and the held row is what lets it
// succeed. Reclaiming those would hand the card to somebody else behind
// the retry's back — the ledger would be tidy and the node overcommitted.
//
// before is the grace cutoff. The reservation is written just ahead of
// the instance record, so a sweep with no grace can land in that gap and
// reclaim a row whose instance is milliseconds from existing.
//
// Idle holds are never touched: only the controller that parked a model
// knows whether it is still wanted.
func (a *Admitter) ReclaimOrphans(ctx context.Context, nodeID string, held map[string]bool, before time.Time) ([]types.GPURes, error) {
	var dropped []types.GPURes
	err := a.mutate(ctx, nodeID, func(l *types.NodeDeviceLedger) bool {
		// Reset per attempt: the CAS re-runs this against a freshly read
		// ledger on a write conflict, and a carried-over list would report
		// releases that the winning attempt never made.
		dropped = nil
		return dropWhere(l, func(r types.GPURes) bool {
			if r.Holder != types.GPUResHolderInstance {
				return false
			}
			if held[r.InstanceID] || !r.CreatedAt.Before(before) {
				return false
			}
			dropped = append(dropped, r)
			return true
		})
	})
	if err != nil {
		return nil, err
	}
	return dropped, nil
}

// AdoptAssignment re-reserves the devices an instance is ALREADY running
// on, when its record names them and the ledger does not — a store
// restored from an older backup, or a row lost to surgery.
//
// It takes those exact UUIDs rather than choosing fresh ones. The
// container is running on that hardware; a reservation anywhere else is
// not a placement, it is a false statement about where the memory is.
//
// Refuses rather than overcommits when they no longer have room. The
// caller's move then is to flag the instance and leave it running — Rune
// does not kill a healthy serving instance to tidy up its own books.
//
// Two limits worth knowing. The request comes from the service spec as
// it reads NOW, so an instance still running an older spec is re-booked
// at the new number. And unlike the release path this WILL create the
// node's ledger if it is absent, because there is a live claim to record
// and refusing to write it would leave the card reading free.
// Returns the devices it actually claimed, which is empty when they were
// all covered already — the caller must not announce a repair it did not
// make.
func (a *Admitter) AdoptAssignment(ctx context.Context, node *types.Node, req Request, devices []string) ([]string, error) {
	if err := checkInventory(node, req.NodeID); err != nil {
		return nil, err
	}
	if len(devices) == 0 {
		return nil, fmt.Errorf("gpu: adopt needs at least one device")
	}
	if req.Holder == "" {
		req.Holder = types.GPUResHolderInstance
	}
	need, err := requestedBytes(req.GPU)
	if err != nil {
		return nil, err
	}

	present := make(map[string]types.GPUDevice, len(node.Devices))
	for _, d := range liveDevices(node.Devices) {
		present[d.UUID] = d
	}

	var (
		ledger  types.NodeDeviceLedger
		claimed []string
	)
	err = a.updateLedger(ctx, req.NodeID, &ledger, func() error {
		// Reset per attempt: the CAS re-runs this against a freshly read
		// ledger, and a carried-over list would report claims the winning
		// attempt never wrote.
		claimed = nil
		// IDEMPOTENT, and not as a nicety: the caller decides what is
		// missing from a snapshot read outside this transaction, so by the
		// time we commit some of it may be covered. Appending a second row
		// for a device this instance already holds would double-count its
		// VRAM and refuse the next service that should have fitted.
		wanted := unheldBy(&ledger, req.InstanceID, devices)
		if len(wanted) == 0 {
			return store.ErrSkipUpdate
		}
		for _, uuid := range wanted {
			dev, ok := present[uuid]
			if !ok {
				// The card the instance is running on is not in the
				// inventory any more. Re-reserving would claim capacity
				// against hardware the node no longer reports.
				return &AdmissionError{
					Reason: types.GPUReasonDeviceMissing,
					Message: fmt.Sprintf("%s/%s is still running on %s, but this node no longer reports that card — "+
						"it keeps serving; devices are probed once per runed start, so restart runed if the card is back",
						req.Namespace, req.ServiceName, uuid),
				}
			}
			if !deviceAccepts(&ledger, dev, req.GPU, need) {
				// Two different refusals wear the same reason. A card with
				// other workloads on it is a packing problem the operator
				// can act on; an empty one that still will not fit means
				// the request outgrew the card, and telling them to free
				// VRAM there would send them after memory nobody is using.
				if who := holdersOf(&ledger, uuid); who != "" {
					return &AdmissionError{
						Reason: types.GPUReasonOverCommitted,
						Message: fmt.Sprintf("%s/%s is still running on %s, but that card is now held by %s — "+
							"it keeps serving; free VRAM on that card, or `rune restart %s` to place it elsewhere",
							req.Namespace, req.ServiceName, uuid, who, req.ServiceName),
					}
				}
				return &AdmissionError{
					Reason: types.GPUReasonOverCommitted,
					Message: fmt.Sprintf("%s/%s is still running on %s, but its request no longer fits there: "+
						"%s asked for, %s usable of %s — it keeps serving, and nothing else is on the card to free",
						req.Namespace, req.ServiceName, uuid,
						humanBytes(need), humanBytes(usable(&ledger, dev, 1)), humanBytes(dev.VRAMBytes)),
				}
			}
		}
		appendReservations(&ledger, Placement{DeviceUUIDs: wanted}, req)
		claimed = wanted
		return nil
	})
	if err != nil {
		return nil, err
	}
	return claimed, nil
}

// unheldBy is the subset of devices that instanceID does not already
// hold a row for.
func unheldBy(l *types.NodeDeviceLedger, instanceID string, devices []string) []string {
	covered := map[string]bool{}
	for _, r := range l.Reservations {
		if r.InstanceID == instanceID {
			covered[r.DeviceUUID] = true
		}
	}
	out := make([]string, 0, len(devices))
	for _, uuid := range devices {
		if !covered[uuid] {
			// Marking it covered as we go dedupes the input. The check
			// loop runs against the pre-append ledger, so a device named
			// twice would pass it twice and be counted twice — and a
			// record from a restored backup or operator surgery is
			// exactly the input this path exists for.
			covered[uuid] = true
			out = append(out, uuid)
		}
	}
	return out
}

// holdersOf names who else is on a device, for a refusal message.
//
// The message has to carry this because nothing renders a ledger: an
// operator told their card is full has no command that shows what is on
// it. That makes this string the whole diagnosis surface, so it counts
// repeats rather than deduping them — two instances of one service at
// 20Gi are 40Gi, and rendering them as one 20Gi row leaves an operator
// unable to make the arithmetic add up.
//
// Empty means nothing else is on the card, which is a different refusal
// and gets a different sentence from the caller.
func holdersOf(l *types.NodeDeviceLedger, uuid string) string {
	type holder struct {
		label string
		n     int
	}
	var order []string
	seen := map[string]*holder{}
	for _, r := range l.Reservations {
		if r.DeviceUUID != uuid {
			continue
		}
		label := fmt.Sprintf("%s/%s (whole card)", r.Namespace, r.ServiceName)
		if !r.WholeDevice {
			label = fmt.Sprintf("%s/%s %s", r.Namespace, r.ServiceName, humanBytes(r.VRAMBytes))
		}
		if h, ok := seen[label]; ok {
			h.n++
			continue
		}
		seen[label] = &holder{label: label, n: 1}
		order = append(order, label)
	}
	sort.Strings(order)
	who := make([]string, 0, len(order))
	for _, label := range order {
		if h := seen[label]; h.n > 1 {
			who = append(who, fmt.Sprintf("%s ×%d", label, h.n))
			continue
		}
		who = append(who, label)
	}
	return strings.Join(who, ", ")
}
