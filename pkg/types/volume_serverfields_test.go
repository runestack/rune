package types

import (
	"reflect"
	"testing"
)

// Every Volume field is classified as either server-owned (cleared on create
// and carried from the stored object on update by Zero/CarryServerOwnedVolumeFields,
// or otherwise set only by the server) or client-owned (a caller may set it).
// A field added to Volume without being placed in one bucket fails this test,
// so the next field cannot silently become a client-settable control-plane
// field — the gap that let a client choose Handle and ID.
func TestVolumeFieldOwnershipIsExhaustive(t *testing.T) {
	serverOwned := map[string]bool{
		// Cleared by ZeroServerOwnedVolumeFields, carried by Carry.
		"ID": true, "Handle": true, "DriverParameters": true,
		"OwnerService": true, "BoundNode": true, "BoundClaim": true,
		"Status": true, "StatusReason": true, "Message": true, "InitializedFor": true,
		// Set only by the server, never persisted from a client write:
		"CreatedAt": true, "UpdatedAt": true, // repo-stamped
		"OwnedBy":            true, // release applier stamp
		"UsedBytes":          true, // read-only usage (json:"-")
		"CapacityBytes":      true, // read-only usage (json:"-")
		"NamespacedResource": true, // embedded key helper, not a data field
	}
	clientOwned := map[string]bool{
		"Name": true, "Namespace": true, "Labels": true, "StorageClassName": true,
		"Size": true, "AccessMode": true, "ReclaimPolicy": true,
		"Parameters": true, "SnapshotSchedule": true,
	}

	rt := reflect.TypeOf(Volume{})
	for i := 0; i < rt.NumField(); i++ {
		name := rt.Field(i).Name
		if serverOwned[name] == clientOwned[name] { // in both, or in neither
			t.Errorf("Volume.%s is unclassified or double-classified; add it to serverOwned "+
				"(and Zero/CarryServerOwnedVolumeFields if a client could set it) or clientOwned", name)
		}
	}
}
