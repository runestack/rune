package localinstances

import (
	"context"
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
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = be.Close() })
	return be
}

func TestRegisterIdempotent(t *testing.T) {
	ol := newOlog(t)
	if err := Register(ol); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := Register(ol); err != nil {
		t.Fatalf("second should be idempotent: %v", err)
	}
}

func TestUpdateAndDeleteRoundTrip(t *testing.T) {
	ol := newOlog(t)
	if err := Register(ol); err != nil {
		t.Fatalf("register: %v", err)
	}
	pub := NewPublisher(ol)
	ctx := context.Background()

	table := map[string]types.InstanceIdentity{
		"172.17.0.5": {InstanceID: "i-1", Service: "web", Namespace: "prod"},
		"172.17.0.6": {InstanceID: "i-2", Service: "web", Namespace: "prod"},
	}
	if err := pub.Update(ctx, "node-a", table); err != nil {
		t.Fatalf("Update: %v", err)
	}

	ch, err := ol.Watch(ctx, 0)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	ev := <-ch
	if len(ev.Mutations) == 0 {
		t.Fatal("no mutations")
	}
	m := ev.Mutations[0]
	if !IsLocalInstancesMutation(m) {
		t.Fatalf("filter rejected: %+v", m)
	}
	li, err := DecodePayload(m)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if li.NodeID != "node-a" || len(li.Instances) != 2 {
		t.Fatalf("payload mismatch: %+v", li)
	}
	if li.Instances["172.17.0.5"].Service != "web" {
		t.Fatalf("identity mismatch: %+v", li.Instances)
	}

	if err := pub.Delete(ctx, "node-a"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

func TestUpdateRejectsEmptyNodeID(t *testing.T) {
	ol := newOlog(t)
	if err := Register(ol); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := NewPublisher(ol).Update(context.Background(), "", nil); err == nil {
		t.Fatal("expected error for empty nodeID")
	}
}
