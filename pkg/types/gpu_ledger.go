package types

import "time"

// NodeDeviceLedger records who has claimed a node's GPU capacity. It is
// a separate record from Node, with a different writer: Node says what
// hardware exists and is written by the node's own agent; this says what
// is claimed and is written by the orchestrator under a CAS.
//
// It is authoritative for admission. Deriving the same answer from live
// instance records loses two things: the race window is wider (the
// workqueue is exclusive per service, not per device), and an instance-
// free reservation — VRAM held for a model scaled to zero — cannot be
// expressed at all.
type NodeDeviceLedger struct {
	// NodeID is the node this ledger belongs to, and its store key.
	NodeID string `json:"nodeId" yaml:"nodeId"`

	// Reservations is every live claim on this node's devices.
	Reservations []GPURes `json:"reservations" yaml:"reservations"`

	// UpdatedAt is when the last claim changed.
	UpdatedAt time.Time `json:"updatedAt" yaml:"updatedAt"`
}

// GPUResHolder identifies what kind of thing holds a reservation. It
// exists because an empty InstanceID was doing two jobs that need
// different treatment by the reclaim sweep — see the constants.
type GPUResHolder string

const (
	// GPUResHolderInstance is a reservation belonging to an instance.
	// Its InstanceID may be briefly empty: the reservation is written
	// before the instance record exists, and a crash in that window
	// leaves a row that must eventually be reclaimed.
	GPUResHolderInstance GPUResHolder = "instance"

	// GPUResHolderIdle is VRAM deliberately held for a workload that has
	// no instance — a model scaled to zero that intends to come back.
	// It has no instance by design and must never be reclaimed for
	// lacking one.
	GPUResHolderIdle GPUResHolder = "idle"
)

// GPURes is one claim on one device.
//
// # No omitempty on any field, deliberately
//
// Three fields here have a MEANINGFUL zero value: InstanceID is legally
// empty for an instance-free hold, VRAMBytes: 0 together with
// WholeDevice: true is how an exclusive claim is spelled, and
// WholeDevice: false is the shared case. `omitempty` would drop exactly
// those from the stored JSON, leaving a reader unable to tell an
// exclusive hold from a field nobody wrote.
//
// That is a property of the encoding and holds regardless of the store.
// (An earlier reason — that the store's CAS retry re-read into an
// un-zeroed target, so an absent field kept a discarded attempt's value —
// was real but has since been fixed centrally.)
type GPURes struct {
	// DeviceUUID is the device claimed. UUIDs, never indices: indices
	// are renumbered by the driver across reboots.
	DeviceUUID string `json:"deviceUuid" yaml:"deviceUuid"`

	// Namespace and ServiceName identify the workload holding it.
	Namespace   string `json:"namespace" yaml:"namespace"`
	ServiceName string `json:"serviceName" yaml:"serviceName"`

	// InstanceID is the holding instance. EMPTY IS LEGAL — see Holder.
	InstanceID string `json:"instanceId" yaml:"instanceId"`

	// VRAMBytes is the requested share of the device. Zero together
	// with WholeDevice means the whole card, not "no memory".
	VRAMBytes int64 `json:"vramBytes" yaml:"vramBytes"`

	// WholeDevice claims the device exclusively: no other reservation
	// may share it, whatever the arithmetic says.
	WholeDevice bool `json:"wholeDevice" yaml:"wholeDevice"`

	// Holder discriminates the two reasons InstanceID can be empty.
	Holder GPUResHolder `json:"holder" yaml:"holder"`

	// CreatedAt is when the claim was written. The reclaim sweep uses it
	// as a grace window so it cannot race a reservation whose instance
	// record is legally still being written.
	CreatedAt time.Time `json:"createdAt" yaml:"createdAt"`
}

// Reserved system VRAM. Subtracted from a device's usable capacity by
// every admission decision.
//
// It covers what Rune cannot see: a stray `docker run --gpus all`, a
// notebook kernel, a compositor — and the per-process CUDA context, which
// is 300-600MB apiece and charged to nobody. Without it the first shared
// device overcommits by roughly 2GiB at five co-tenants, and the engine
// blamed is whichever one allocated last rather than whichever one was
// over.
//
// Charged to the device rather than added to each request on purpose: the
// alternative is arithmetically identical but makes the number an
// operator wrote in `vram:` differ from the number `describe node`
// reports back, and those being one number is worth more than the
// symmetry.
const (
	// DefaultReservedVRAMFloor is held back on any device with devices present.
	DefaultReservedVRAMFloor int64 = 512 << 20 // 512Mi

	// DefaultReservedVRAMPerInstance is held back per assigned instance,
	// covering that process's CUDA context.
	DefaultReservedVRAMPerInstance int64 = 400 << 20 // 400Mi
)

// IsWholeDevice reports whether this reservation claims its device
// exclusively.
func (r GPURes) IsWholeDevice() bool { return r.WholeDevice }

// FindReservations returns every reservation on the named device.
func (l *NodeDeviceLedger) FindReservations(deviceUUID string) []GPURes {
	if l == nil {
		return nil
	}
	var out []GPURes
	for _, r := range l.Reservations {
		if r.DeviceUUID == deviceUUID {
			out = append(out, r)
		}
	}
	return out
}

// RequestedBytes is the sum of VRAM claimed on a device by reservations,
// NOT counting the reserved system floor and NOT counting whole-device
// holders (which claim the card rather than a number of bytes).
func (l *NodeDeviceLedger) RequestedBytes(deviceUUID string) int64 {
	var total int64
	for _, r := range l.FindReservations(deviceUUID) {
		if !r.WholeDevice {
			total += r.VRAMBytes
		}
	}
	return total
}

// HasWholeDeviceHolder reports whether the device is claimed exclusively.
func (l *NodeDeviceLedger) HasWholeDeviceHolder(deviceUUID string) bool {
	for _, r := range l.FindReservations(deviceUUID) {
		if r.WholeDevice {
			return true
		}
	}
	return false
}

// ReservedBytes is the system VRAM withheld from a device: a flat floor
// plus a per-assigned-instance term for CUDA context overhead.
func (l *NodeDeviceLedger) ReservedBytes(deviceUUID string) int64 {
	return l.ReservedBytesWith(deviceUUID, 0)
}

// ReservedBytesWith is ReservedBytes for the state the device WILL be in
// once `incoming` more reservations land on it.
//
// Admission has to use this rather than ReservedBytes: the per-instance
// term covers the CUDA context of each process on the card, and a request
// checked against a reserve that does not yet include its own context is
// checked against a number that is too generous by exactly one context.
func (l *NodeDeviceLedger) ReservedBytesWith(deviceUUID string, incoming int) int64 {
	n := len(l.FindReservations(deviceUUID)) + incoming
	return DefaultReservedVRAMFloor + DefaultReservedVRAMPerInstance*int64(n)
}
