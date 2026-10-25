package types

import (
	"reflect"
	"testing"
	"time"
)

// helperManaged are the fields Zero clears and Carry copies. Keep in sync
// with ZeroServerOwnedVolumeFields / CarryServerOwnedVolumeFields.
var helperManaged = []string{
	"ID", "Handle", "DriverParameters", "OwnerService", "BoundNode",
	"BoundClaim", "Status", "StatusReason", "Message", "InitializedFor", "OwnedBy",
}

// fullVolume is a Volume with every field set to a non-zero value, so Zero
// and Carry are observable on all of them.
func fullVolume() *Volume {
	return &Volume{
		ID: "vid", Name: "n", Namespace: "ns", Labels: map[string]string{"k": "v"},
		OwnedBy:          &OwnedBy{Release: "r"},
		StorageClassName: "sc", Size: "1Gi", AccessMode: AccessModeRWO,
		ReclaimPolicy: ReclaimPolicyRetain, Parameters: map[string]string{"p": "q"},
		DriverParameters: map[string]string{"d": "e"},
		SnapshotSchedule: &SnapshotSchedule{Cron: "@daily", Retention: 3},
		Handle:           "/h", UsedBytes: 1, CapacityBytes: 2,
		OwnerService: "ns/s", BoundNode: "node", BoundClaim: "s/c/0",
		Status: VolumeStatusAvailable, StatusReason: "why", Message: "msg",
		InitializedFor: map[string]time.Time{"x": time.Unix(1, 0)},
		CreatedAt:      time.Unix(2, 0), UpdatedAt: time.Unix(3, 0),
	}
}

// Every Volume field is classified server- or client-owned; a new field in
// neither bucket fails here, so it can't silently become a client-settable
// control-plane field (the gap that let a client choose Handle and ID).
func TestVolumeFieldOwnershipIsExhaustive(t *testing.T) {
	serverOwned := map[string]bool{
		"ID": true, "Handle": true, "DriverParameters": true, "OwnerService": true,
		"BoundNode": true, "BoundClaim": true, "Status": true, "StatusReason": true,
		"Message": true, "InitializedFor": true, "OwnedBy": true,
		"CreatedAt": true, "UpdatedAt": true, // repo-stamped
		"UsedBytes": true, "CapacityBytes": true, // read-only usage (json:"-")
		"NamespacedResource": true, // embedded key helper
	}
	clientOwned := map[string]bool{
		"Name": true, "Namespace": true, "Labels": true, "StorageClassName": true,
		"Size": true, "AccessMode": true, "ReclaimPolicy": true,
		"Parameters": true, "SnapshotSchedule": true,
	}
	rt := reflect.TypeOf(Volume{})
	for i := 0; i < rt.NumField(); i++ {
		name := rt.Field(i).Name
		if serverOwned[name] == clientOwned[name] {
			t.Errorf("Volume.%s is unclassified or double-classified; add it to serverOwned "+
				"(and the helpers if a client could set it) or clientOwned", name)
		}
	}
}

// Zero must clear, and Carry must copy, every helper-managed field — so a
// field added to a helper's list without a matching line (or vice versa) is
// caught.
func TestZeroAndCarryCoverEveryHelperField(t *testing.T) {
	t.Run("Zero clears all", func(t *testing.T) {
		v := fullVolume()
		ZeroServerOwnedVolumeFields(v)
		rv := reflect.ValueOf(v).Elem()
		for _, f := range helperManaged {
			if !rv.FieldByName(f).IsZero() {
				t.Errorf("Zero left %s set: %v", f, rv.FieldByName(f).Interface())
			}
		}
		// A client-owned field must survive Zero.
		if v.Size != "1Gi" || v.Name != "n" {
			t.Errorf("Zero touched a client field: size=%q name=%q", v.Size, v.Name)
		}
	})
	t.Run("Carry copies all", func(t *testing.T) {
		stored := fullVolume()
		incoming := &Volume{Name: "n", Namespace: "ns"} // all helper fields zero
		CarryServerOwnedVolumeFields(stored, incoming)
		sv, iv := reflect.ValueOf(stored).Elem(), reflect.ValueOf(incoming).Elem()
		for _, f := range helperManaged {
			if !reflect.DeepEqual(sv.FieldByName(f).Interface(), iv.FieldByName(f).Interface()) {
				t.Errorf("Carry did not copy %s: stored=%v incoming=%v",
					f, sv.FieldByName(f).Interface(), iv.FieldByName(f).Interface())
			}
		}
	})
}
