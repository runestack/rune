package types

import (
	"reflect"
	"testing"
)

// Every Snapshot field is classified server- or client-owned; a new field in
// neither bucket fails here, so it can't silently become client-settable —
// the gap that let a client choose a snapshot Handle/Phase/Driver.
func TestSnapshotFieldOwnershipIsExhaustive(t *testing.T) {
	// Cleared by ZeroServerOwnedSnapshotFields.
	zeroed := []string{"ID", "Driver", "Handle", "SizeBytes", "Phase", "Reason", "Message"}
	serverOwned := map[string]bool{
		"CreatedAt": true, "UpdatedAt": true, // repo-stamped
		"NamespacedResource": true, // embedded key helper
	}
	for _, f := range zeroed {
		serverOwned[f] = true
	}
	clientOwned := map[string]bool{
		"Name": true, "Namespace": true, "Labels": true,
		"SourceVolume": true, "Scheduled": true,
	}
	rt := reflect.TypeOf(Snapshot{})
	for i := 0; i < rt.NumField(); i++ {
		name := rt.Field(i).Name
		if serverOwned[name] == clientOwned[name] {
			t.Errorf("Snapshot.%s is unclassified or double-classified", name)
		}
	}

	// Zero must clear every field in its list.
	s := &Snapshot{
		ID: "i", Driver: "d", Handle: "h", SizeBytes: 9,
		Phase: SnapshotPhaseReady, Reason: "r", Message: "m",
		Name: "n", Namespace: "ns", SourceVolume: "src",
	}
	ZeroServerOwnedSnapshotFields(s)
	rv := reflect.ValueOf(s).Elem()
	for _, f := range zeroed {
		if !rv.FieldByName(f).IsZero() {
			t.Errorf("Zero left %s set: %v", f, rv.FieldByName(f).Interface())
		}
	}
	if s.SourceVolume != "src" || s.Name != "n" {
		t.Errorf("Zero touched a client field: src=%q name=%q", s.SourceVolume, s.Name)
	}
}
