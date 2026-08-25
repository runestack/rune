// Package gpu is GPU admission: deciding which device a request can go
// on, and recording the claim so a second request cannot take the same
// bytes.
//
// The invariant, for every device:
//
//	Σ(requested VRAM of live reservations) ≤ VRAMBytes − reserved system VRAM
//
// and a whole-device holder is the only holder on its device.
//
// Admission is on REQUESTS, not on measured free memory. Serving engines
// pre-allocate — vLLM grabs 90% of the card at startup regardless of load
// — so "used" describes the engine's arena, not its need. Measuring would
// refuse everything after the first engine, or, tuned the other way,
// overcommit into arena headroom. Requests are also the only basis that
// is stable between admission time and model-load time, which are minutes
// apart.
package gpu

import (
	"fmt"
	"sort"
	"strings"

	"github.com/runestack/rune/pkg/types"
)

// Placement is the outcome of choosing devices for a request.
type Placement struct {
	// DeviceUUIDs are the devices to claim, in assignment order.
	DeviceUUIDs []string

	// CrossNamespace is set when the choice had to land on a device
	// already held by a different namespace. Callers surface it; a
	// shared device is a shared trust domain and the operator should
	// learn it from an event rather than from an incident.
	CrossNamespace bool

	// OtherNamespaces are the namespaces already on the chosen devices,
	// when CrossNamespace is set.
	OtherNamespaces []string
}

// usable is what a device has left for new claims, net of the system
// reserve and everything already requested on it.
func usable(ledger *types.NodeDeviceLedger, dev types.GPUDevice) int64 {
	free := dev.VRAMBytes - ledger.ReservedBytes(dev.UUID) - ledger.RequestedBytes(dev.UUID)
	if free < 0 {
		return 0
	}
	return free
}

// candidate is a device under consideration, carrying what the ranking
// needs so the sort stays a pure comparison.
type candidate struct {
	dev             types.GPUDevice
	free            int64
	tier            int // 0 own-namespace, 1 empty, 2 other-namespace
	otherNamespaces []string
}

// ChooseDevices picks the devices for req against the current ledger, or
// returns a reason it cannot.
//
// Policy is best-fit — pack the fullest device that still fits — so large
// future requests keep a chance at a whole card. With one rule ranked
// ABOVE best-fit: prefer a device that no other namespace holds.
//
// That tie-break is not a refinement, it reverses a bad default. Plain
// best-fit prefers the fullest card that fits, which is by construction
// the one with the most neighbours — so the default policy would actively
// maximise cross-tenant sharing, on hardware that gives no memory
// isolation between co-tenants. The cost is worse packing: a box with two
// half-full cards from two namespaces will refuse a request that plain
// best-fit would have placed. That is the trade, and the refusal message
// names the holders so an operator can see it is a tenancy boundary
// rather than an arithmetic one.
//
// There is no same-device affinity for a replacement instance. During a
// rolling update the old instance still holds its reservation, so its
// device is precisely the one that no longer fits; affinity would mean
// reserving twice the VRAM on one card.
func ChooseDevices(devices []types.GPUDevice, ledger *types.NodeDeviceLedger, namespace string, req types.GPURequest) (Placement, error) {
	if ledger == nil {
		ledger = &types.NodeDeviceLedger{}
	}
	live := liveDevices(devices)
	if len(live) == 0 {
		return Placement{}, errNoDevices(devices)
	}

	want := req.DeviceCount()
	if req.SharesDevice() {
		want = 1
	}
	if want > len(live) {
		return Placement{}, &AdmissionError{
			Reason: types.GPUReasonNoCapacity,
			Message: fmt.Sprintf("service asks for %d GPUs; this node has %d",
				want, len(live)),
		}
	}

	var need int64
	if req.SharesDevice() {
		parsed, err := types.ParseMemory(req.VRAM)
		if err != nil {
			return Placement{}, &AdmissionError{
				Reason:  types.GPUReasonNoCapacity,
				Message: fmt.Sprintf("unparseable vram request %q: %v", req.VRAM, err),
			}
		}
		need = parsed
	}

	cands := rankCandidates(live, ledger, namespace, req, need)
	if len(cands) < want {
		return Placement{}, insufficientCapacity(live, ledger, req, need)
	}

	// Whole-device multi-GPU must not silently span mismatched products:
	// tensor parallelism across different cards fails at model load with
	// an error naming neither.
	if want > 1 && !req.AllowHeterogeneous {
		matched := sameProduct(cands, want)
		if matched == nil {
			return Placement{}, &AdmissionError{
				Reason: types.GPUReasonNoCapacity,
				Message: fmt.Sprintf("service asks for %d matching GPUs; no %d free devices share a product "+
					"(set resources.gpu.allowHeterogeneous to span different cards)", want, want),
			}
		}
		cands = matched
	}

	chosen := cands[:want]
	p := Placement{DeviceUUIDs: make([]string, 0, want)}
	seen := map[string]bool{}
	for _, c := range chosen {
		p.DeviceUUIDs = append(p.DeviceUUIDs, c.dev.UUID)
		if c.tier == 2 {
			p.CrossNamespace = true
			for _, ns := range c.otherNamespaces {
				if !seen[ns] {
					seen[ns] = true
					p.OtherNamespaces = append(p.OtherNamespaces, ns)
				}
			}
		}
	}
	sort.Strings(p.OtherNamespaces)
	return p, nil
}

// rankCandidates returns the devices req could go on, best first.
func rankCandidates(devices []types.GPUDevice, ledger *types.NodeDeviceLedger, namespace string, req types.GPURequest, need int64) []candidate {
	var out []candidate
	for _, dev := range devices {
		held := ledger.FindReservations(dev.UUID)

		// A whole-device holder excludes everyone, and a whole-device
		// request excludes everyone already there.
		if ledger.HasWholeDeviceHolder(dev.UUID) {
			continue
		}
		if !req.SharesDevice() && len(held) > 0 {
			continue
		}
		if req.SharesDevice() && usable(ledger, dev) < need {
			continue
		}

		c := candidate{dev: dev, free: usable(ledger, dev)}
		var others []string
		for _, r := range held {
			if r.Namespace != namespace {
				others = append(others, r.Namespace)
			}
		}
		switch {
		case len(held) == 0:
			c.tier = 1
		case len(others) == 0:
			c.tier = 0
		default:
			c.tier = 2
			c.otherNamespaces = dedupeSorted(others)
		}
		out = append(out, c)
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].tier != out[j].tier {
			return out[i].tier < out[j].tier
		}
		// Best-fit: least free space that still fits, so roomier cards
		// stay available for larger requests.
		if out[i].free != out[j].free {
			return out[i].free < out[j].free
		}
		return out[i].dev.UUID < out[j].dev.UUID
	})
	return out
}

// sameProduct returns the first want candidates that share a product,
// preserving rank order, or nil.
func sameProduct(cands []candidate, want int) []candidate {
	byProduct := map[string][]candidate{}
	for _, c := range cands {
		byProduct[c.dev.Product] = append(byProduct[c.dev.Product], c)
	}
	// Deterministic: consider products in the order their best-ranked
	// candidate appears.
	seen := map[string]bool{}
	for _, c := range cands {
		if seen[c.dev.Product] {
			continue
		}
		seen[c.dev.Product] = true
		if group := byProduct[c.dev.Product]; len(group) >= want {
			return group[:want]
		}
	}
	return nil
}

func liveDevices(devices []types.GPUDevice) []types.GPUDevice {
	out := make([]types.GPUDevice, 0, len(devices))
	for _, d := range devices {
		if !d.Missing {
			out = append(out, d)
		}
	}
	return out
}

func dedupeSorted(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// errNoDevices distinguishes "this node has no GPUs" from "its GPUs are
// all gone", because blaming the driver for absent cards sends the
// operator hunting in the wrong place.
func errNoDevices(devices []types.GPUDevice) error {
	if len(devices) > 0 {
		var uuids []string
		for _, d := range devices {
			uuids = append(uuids, d.UUID)
		}
		return &AdmissionError{
			Reason: types.GPUReasonDeviceMissing,
			Message: fmt.Sprintf("this node's %d GPUs are all marked missing (%s) — see `rune describe node`",
				len(devices), strings.Join(uuids, ", ")),
		}
	}
	return &AdmissionError{
		Reason:  types.GPUReasonNoCapacity,
		Message: "this node reports no GPUs",
	}
}

// insufficientCapacity builds the refusal, naming who holds what — an
// operator needs to know whether they are short of memory or short of a
// device nobody else is on.
func insufficientCapacity(devices []types.GPUDevice, ledger *types.NodeDeviceLedger, req types.GPURequest, need int64) error {
	var b strings.Builder
	if req.SharesDevice() {
		fmt.Fprintf(&b, "no GPU with %s free VRAM", req.VRAM)
	} else {
		fmt.Fprintf(&b, "no free GPU for a whole-device request")
	}
	for _, dev := range devices {
		held := ledger.FindReservations(dev.UUID)
		if len(held) == 0 {
			continue
		}
		var who []string
		for _, r := range held {
			if r.WholeDevice {
				who = append(who, fmt.Sprintf("%s/%s (whole device)", r.Namespace, r.ServiceName))
				continue
			}
			who = append(who, fmt.Sprintf("%s/%s %s", r.Namespace, r.ServiceName, humanBytes(r.VRAMBytes)))
		}
		fmt.Fprintf(&b, "\n  %s: %s free — held by %s",
			dev.UUID, humanBytes(usable(ledger, dev)), strings.Join(who, ", "))
	}
	return &AdmissionError{Reason: types.GPUReasonNoCapacity, Message: b.String()}
}

func humanBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.0fGi", float64(b)/float64(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.0fMi", float64(b)/float64(1<<20))
	default:
		return fmt.Sprintf("%dB", b)
	}
}
