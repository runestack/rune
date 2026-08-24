package agent

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dgraph-io/badger/v4"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/store/orderedlog"
)

// --- test helpers -----------------------------------------------------

func newOlog(t *testing.T) orderedlog.OrderedLog {
	t.Helper()
	dir := t.TempDir()
	bopts := badger.DefaultOptions(filepath.Join(dir, "olog")).WithLogger(nil)
	db, err := badger.Open(bopts)
	if err != nil {
		t.Fatalf("badger open: %v", err)
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

func newAgent(t *testing.T) *Agent {
	t.Helper()
	a, err := New(Config{
		Identity:     Identity{NodeID: "n-test", Hostname: "h"},
		OrderedLog:   newOlog(t),
		ReadyTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	return a
}

// fakeSubsystem is a controllable Subsystem used by lifecycle tests.
type fakeSubsystem struct {
	name      string
	startErr  error
	startedN  atomic.Int32
	stoppedN  atomic.Int32
	readyCh   chan struct{}
	readyAuto bool // close ready inside Start
}

func (f *fakeSubsystem) Name() string { return f.name }
func (f *fakeSubsystem) Start(ctx context.Context) error {
	f.startedN.Add(1)
	if f.startErr != nil {
		return f.startErr
	}
	if f.readyCh == nil {
		f.readyCh = make(chan struct{})
	}
	if f.readyAuto {
		close(f.readyCh)
	}
	return nil
}
func (f *fakeSubsystem) Ready() <-chan struct{} {
	if f.readyCh == nil {
		f.readyCh = make(chan struct{})
	}
	return f.readyCh
}
func (f *fakeSubsystem) Stop(ctx context.Context) error {
	f.stoppedN.Add(1)
	return nil
}

// --- tests ------------------------------------------------------------

func TestNew_RejectsEmptyNodeID(t *testing.T) {
	_, err := New(Config{OrderedLog: newOlog(t)})
	if err == nil {
		t.Fatal("expected error for empty NodeID")
	}
}

func TestNew_RejectsNilOrderedLog(t *testing.T) {
	_, err := New(Config{Identity: Identity{NodeID: "n"}})
	if err == nil {
		t.Fatal("expected error for nil OrderedLog")
	}
}

func TestAgent_LifecycleNoSubsystems(t *testing.T) {
	a := newAgent(t)
	ctx := context.Background()
	if err := a.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	select {
	case <-a.Ready():
	case <-time.After(time.Second):
		t.Fatal("agent never reported ready")
	}
	if err := a.ReadyErr(); err != nil {
		t.Fatalf("unexpected ready err: %v", err)
	}
	if err := a.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	// Second Stop is a no-op.
	if err := a.Stop(ctx); err != nil {
		t.Fatalf("double stop: %v", err)
	}
}

func TestAgent_StartIsIdempotent(t *testing.T) {
	a := newAgent(t)
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := a.Start(context.Background()); err == nil {
		t.Fatal("expected double-start error")
	}
}

func TestAgent_RegisterAfterStartFails(t *testing.T) {
	a := newAgent(t)
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := a.Register(&fakeSubsystem{name: "late"}); err == nil {
		t.Fatal("expected error registering after start")
	}
}

func TestAgent_RegisterDuplicateName(t *testing.T) {
	a := newAgent(t)
	if err := a.Register(&fakeSubsystem{name: "dup"}); err != nil {
		t.Fatal(err)
	}
	if err := a.Register(&fakeSubsystem{name: "dup"}); err == nil {
		t.Fatal("expected duplicate name error")
	}
}

func TestAgent_SubsystemsStartedInOrder_StoppedReverse(t *testing.T) {
	a := newAgent(t)
	s1 := &fakeSubsystem{name: "s1", readyAuto: true}
	s2 := &fakeSubsystem{name: "s2", readyAuto: true}
	s3 := &fakeSubsystem{name: "s3", readyAuto: true}
	for _, s := range []*fakeSubsystem{s1, s2, s3} {
		if err := a.Register(s); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	select {
	case <-a.Ready():
	case <-time.After(time.Second):
		t.Fatal("agent not ready in time")
	}
	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
	for _, s := range []*fakeSubsystem{s1, s2, s3} {
		if got := s.startedN.Load(); got != 1 {
			t.Errorf("%s started=%d, want 1", s.name, got)
		}
		if got := s.stoppedN.Load(); got != 1 {
			t.Errorf("%s stopped=%d, want 1", s.name, got)
		}
	}
}

func TestAgent_StartFailureRollsBackPriorSubsystems(t *testing.T) {
	a := newAgent(t)
	good1 := &fakeSubsystem{name: "good1", readyAuto: true}
	bad := &fakeSubsystem{name: "bad", startErr: errors.New("nope")}
	good2 := &fakeSubsystem{name: "good2", readyAuto: true}
	for _, s := range []Subsystem{good1, bad, good2} {
		if err := a.Register(s); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Start(context.Background()); err == nil {
		t.Fatal("expected start error")
	}
	if good1.stoppedN.Load() != 1 {
		t.Errorf("good1 should be stopped after rollback, got %d", good1.stoppedN.Load())
	}
	if good2.startedN.Load() != 0 {
		t.Errorf("good2 should not have started, got %d", good2.startedN.Load())
	}
}

func TestAgent_ReadyTimeoutSetsReadyErr(t *testing.T) {
	a, err := New(Config{
		Identity:     Identity{NodeID: "n"},
		OrderedLog:   newOlog(t),
		ReadyTimeout: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	never := &fakeSubsystem{name: "slow"} // readyAuto=false
	if err := a.Register(never); err != nil {
		t.Fatal(err)
	}
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-a.Ready():
	case <-time.After(time.Second):
		t.Fatal("ready ch never closed")
	}
	if a.ReadyErr() == nil {
		t.Fatal("expected ready timeout error")
	}
	_ = a.Stop(context.Background())
}

func TestLoadOrCreateIdentity_Persists(t *testing.T) {
	dir := t.TempDir()
	id1, minted, err := LoadOrCreateIdentity(dir, "")
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if id1.NodeID == "" {
		t.Fatal("empty node id")
	}
	id2, reminted, err := LoadOrCreateIdentity(dir, "")
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if id1.NodeID != id2.NodeID {
		t.Fatalf("node id changed across reload: %q -> %q", id1.NodeID, id2.NodeID)
	}
	if !minted || reminted {
		t.Fatalf("minted = %v then %v; want true then false", minted, reminted)
	}
}

func TestLoadOrCreateIdentity_RejectsMalformed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "node-identity.json")
	if err := writeFile(path, []byte("{not json")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadOrCreateIdentity(dir, ""); err == nil {
		t.Fatal("expected parse error")
	}
}
