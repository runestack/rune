package endpoints

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/dgraph-io/badger/v4"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/store/orderedlog"
	"github.com/runestack/rune/pkg/types"
)

func newOlog(t *testing.T) *orderedlog.BadgerBackend {
	t.Helper()
	dir := t.TempDir()
	db, err := badger.Open(badger.DefaultOptions(filepath.Join(dir, "olog")).WithLogger(nil))
	if err != nil {
		t.Fatalf("badger: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	be := orderedlog.NewBadgerBackend(db, orderedlog.BackendOptions{
		Logger: log.GetDefaultLogger().WithComponent("test.olog"),
	})
	if err := be.Open(); err != nil {
		t.Fatalf("olog open: %v", err)
	}
	t.Cleanup(func() { _ = be.Close() })
	return be
}

func TestRegisterIdempotent(t *testing.T) {
	ol := newOlog(t)
	if err := Register(ol); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if err := Register(ol); err != nil {
		t.Fatalf("second register should be idempotent: %v", err)
	}
}

func TestUpdateAndDeleteRoundTrip(t *testing.T) {
	ol := newOlog(t)
	if err := Register(ol); err != nil {
		t.Fatalf("register: %v", err)
	}
	pub := NewPublisher(ol)
	ctx := context.Background()

	eps := []types.Endpoint{
		{InstanceID: "i-1", IP: "10.0.0.1", Port: 8080, Protocol: "tcp", Healthy: true},
		{InstanceID: "i-2", IP: "10.0.0.2", Port: 8080, Protocol: "tcp", Healthy: true},
	}
	if err := pub.Update(ctx, "svc-1", eps); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// Snapshot should now contain the endpoints/svc-1 key.
	snap, _, err := ol.Snapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	defer snap.Close()
	found := false
	if err := snap.Range([]byte("endpoints/"), func(k, v []byte) error {
		if string(k) == "endpoints/svc-1" {
			found = true
			var se types.ServiceEndpoints
			if err := decodeJSON(v, &se); err != nil {
				return err
			}
			if se.ServiceID != "svc-1" || len(se.Endpoints) != 2 {
				t.Errorf("decoded payload mismatch: %+v", se)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("Range: %v", err)
	}
	if !found {
		t.Fatal("endpoints/svc-1 not present after Update")
	}

	if err := pub.Delete(ctx, "svc-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	snap2, _, err := ol.Snapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot2: %v", err)
	}
	defer snap2.Close()
	if err := snap2.Range([]byte("endpoints/"), func(k, _ []byte) error {
		if string(k) == "endpoints/svc-1" {
			t.Errorf("key still present after Delete")
		}
		return nil
	}); err != nil {
		t.Fatalf("Range2: %v", err)
	}
}

func TestUpdateRejectsEmptyServiceID(t *testing.T) {
	ol := newOlog(t)
	if err := Register(ol); err != nil {
		t.Fatalf("register: %v", err)
	}
	pub := NewPublisher(ol)
	if err := pub.Update(context.Background(), "", nil); err == nil {
		t.Fatal("expected error for empty serviceID")
	}
}

func TestDecodePayloadAndFilter(t *testing.T) {
	ol := newOlog(t)
	if err := Register(ol); err != nil {
		t.Fatalf("register: %v", err)
	}
	pub := NewPublisher(ol)
	ctx := context.Background()
	if err := pub.Update(ctx, "svc-x", []types.Endpoint{{IP: "10.0.0.5", Port: 1}}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	ch, err := ol.Watch(ctx, 0)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	ev := <-ch
	if len(ev.Mutations) == 0 {
		t.Fatal("no mutations in event")
	}
	m := ev.Mutations[0]
	if !IsEndpointsMutation(m) {
		t.Fatal("IsEndpointsMutation false for endpoints update")
	}
	se, err := DecodePayload(m)
	if err != nil {
		t.Fatalf("DecodePayload: %v", err)
	}
	if se.ServiceID != "svc-x" || len(se.Endpoints) != 1 || se.Endpoints[0].IP != "10.0.0.5" {
		t.Errorf("decoded mismatch: %+v", se)
	}
}

// decodeJSON is a tiny test helper to avoid pulling encoding/json in
// tests that just want to round-trip the persisted payload format.
func decodeJSON(raw []byte, out *types.ServiceEndpoints) error {
	return json.Unmarshal(raw, out)
}
