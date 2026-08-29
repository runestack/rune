package events

import (
	"context"
	"fmt"
	"testing"
	"time"

	badger "github.com/dgraph-io/badger/v4"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestRecorder(t *testing.T) *Recorder {
	t.Helper()
	db, err := badger.Open(badger.DefaultOptions("").WithInMemory(true).WithLogger(nil))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	r, err := NewRecorder(db, log.GetDefaultLogger(), Options{})
	require.NoError(t, err)
	return r
}

func mkEvent(reason, msg string) types.Event {
	return types.Event{
		Namespace: "shared", Kind: "Instance", Name: "flo-0",
		UID: "i-1", Level: types.EventLevelError, Reason: reason, Message: msg,
	}
}

func TestRecorder_EmitAssignsSeqAndID(t *testing.T) {
	r := newTestRecorder(t)
	ctx := context.Background()

	require.NoError(t, r.Emit(ctx, mkEvent("VolumeNotReady", "volume not ready")))
	got, err := r.ListByResource(ctx, "shared", "Instance", "flo-0", 10)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, int64(1), got[0].Seq)
	assert.Equal(t, "Instance/flo-0/1", got[0].ID)
	assert.Equal(t, 1, got[0].Count)
	assert.False(t, got[0].FirstSeen.IsZero())
	assert.False(t, got[0].LastSeen.IsZero())
}

// Identical consecutive events fold: Count bumps, Seq stays put, no
// new record in the by-resource scan.
func TestRecorder_Fold(t *testing.T) {
	r := newTestRecorder(t)
	ctx := context.Background()

	require.NoError(t, r.Emit(ctx, mkEvent("VolumeNotReady", "stalled")))
	require.NoError(t, r.Emit(ctx, mkEvent("VolumeNotReady", "stalled")))
	require.NoError(t, r.Emit(ctx, mkEvent("VolumeNotReady", "stalled")))

	got, err := r.ListByResource(ctx, "shared", "Instance", "flo-0", 10)
	require.NoError(t, err)
	require.Len(t, got, 1, "three identical events fold into one")
	assert.Equal(t, 3, got[0].Count)
	assert.Equal(t, int64(1), got[0].Seq)
}

// A change in Reason breaks the fold.
func TestRecorder_FoldBreaksOnReason(t *testing.T) {
	r := newTestRecorder(t)
	ctx := context.Background()

	require.NoError(t, r.Emit(ctx, mkEvent("VolumeNotReady", "x")))
	require.NoError(t, r.Emit(ctx, mkEvent("SecretNotFound", "x")))

	got, err := r.ListByResource(ctx, "shared", "Instance", "flo-0", 10)
	require.NoError(t, err)
	require.Len(t, got, 2)
	// Newest first.
	assert.Equal(t, "SecretNotFound", got[0].Reason)
	assert.Equal(t, "VolumeNotReady", got[1].Reason)
	assert.Equal(t, int64(2), got[0].Seq)
	assert.Equal(t, int64(1), got[1].Seq)
}

// UID flip prevents folding across incarnations of the same name.
func TestRecorder_FoldBreaksOnUID(t *testing.T) {
	r := newTestRecorder(t)
	ctx := context.Background()

	e1 := mkEvent("CreateFailed", "boom")
	e1.UID = "old-uid"
	e2 := mkEvent("CreateFailed", "boom")
	e2.UID = "new-uid"

	require.NoError(t, r.Emit(ctx, e1))
	require.NoError(t, r.Emit(ctx, e2))

	got, err := r.ListByResource(ctx, "shared", "Instance", "flo-0", 10)
	require.NoError(t, err)
	assert.Len(t, got, 2, "different UIDs do not fold")
}

func TestRecorder_ListSince(t *testing.T) {
	r := newTestRecorder(t)
	ctx := context.Background()

	require.NoError(t, r.Emit(ctx, mkEvent("A", "1")))
	require.NoError(t, r.Emit(ctx, mkEvent("B", "2")))
	require.NoError(t, r.Emit(ctx, mkEvent("C", "3")))

	all, err := r.ListSince(ctx, 0, 10)
	require.NoError(t, err)
	require.Len(t, all, 3)
	assert.Equal(t, int64(1), all[0].Seq)
	assert.Equal(t, int64(3), all[2].Seq)

	tail, err := r.ListSince(ctx, 1, 10)
	require.NoError(t, err)
	require.Len(t, tail, 2)
	assert.Equal(t, int64(2), tail[0].Seq)
}

func TestRecorder_Cursors(t *testing.T) {
	r := newTestRecorder(t)
	ctx := context.Background()

	got, err := r.LoadCursor(ctx, "runesight")
	require.NoError(t, err)
	assert.Equal(t, int64(0), got, "unknown consumer reads as 0")

	require.NoError(t, r.SaveCursor(ctx, "runesight", 42))
	got, err = r.LoadCursor(ctx, "runesight")
	require.NoError(t, err)
	assert.Equal(t, int64(42), got)
}

// nextSeq must resume strictly above the largest persisted Seq across
// recorder restarts on the same DB.
func TestRecorder_SeqResumesAcrossRestart(t *testing.T) {
	db, err := badger.Open(badger.DefaultOptions("").WithInMemory(true).WithLogger(nil))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	r1, err := NewRecorder(db, log.GetDefaultLogger(), Options{})
	require.NoError(t, err)
	require.NoError(t, r1.Emit(context.Background(), mkEvent("A", "1")))
	require.NoError(t, r1.Emit(context.Background(), mkEvent("B", "2")))

	r2, err := NewRecorder(db, log.GetDefaultLogger(), Options{})
	require.NoError(t, err)
	require.NoError(t, r2.Emit(context.Background(), mkEvent("C", "3")))

	all, err := r2.ListSince(context.Background(), 0, 10)
	require.NoError(t, err)
	require.Len(t, all, 3)
	assert.Equal(t, int64(3), all[2].Seq, "new emit lands above the persisted max")
}

// Per-key ring is bounded — older events are dropped from the cache
// but the store still holds them until TTL.
func TestRecorder_RingBounded(t *testing.T) {
	db, err := badger.Open(badger.DefaultOptions("").WithInMemory(true).WithLogger(nil))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	r, err := NewRecorder(db, log.GetDefaultLogger(), Options{RingPerKey: 3})
	require.NoError(t, err)

	ctx := context.Background()
	for i := 0; i < 5; i++ {
		require.NoError(t, r.Emit(ctx, mkEvent("R", time.Now().String()+string(rune('a'+i)))))
	}

	got, err := r.ListByResource(ctx, "shared", "Instance", "flo-0", 100)
	require.NoError(t, err)
	assert.Len(t, got, 3, "ring caps cache at RingPerKey")
}

func TestRecorder_EmitRejectsBadInput(t *testing.T) {
	r := newTestRecorder(t)
	err := r.Emit(context.Background(), types.Event{Name: "x"})
	require.Error(t, err)
	err = r.Emit(context.Background(), types.Event{Kind: "x"})
	require.Error(t, err)
}

// ListSince is the ascending outbox view, so asking it for "the newest" by
// passing cursor 0 hands back the OLDEST events. ListLatest exists because
// the CLI's event views want recency; a caller that confuses the two shows
// a stale window on exactly the busy box where it matters.
func TestListLatest_ReturnsNewestNotOldest(t *testing.T) {
	r := newTestRecorder(t)
	ctx := context.Background()
	for i := 0; i < 30; i++ {
		if err := r.Emit(ctx, mkEvent("Reason", fmt.Sprintf("event-%02d", i))); err != nil {
			t.Fatal(err)
		}
	}

	latest, err := r.ListLatest(ctx, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(latest) != 5 {
		t.Fatalf("want 5 events, got %d", len(latest))
	}
	if latest[0].Message != "event-29" {
		t.Fatalf("newest first: got %q, want event-29", latest[0].Message)
	}
	if latest[4].Message != "event-25" {
		t.Fatalf("descending: got %q, want event-25", latest[4].Message)
	}

	// The distinction this function exists for: the ascending view with a
	// zero cursor starts at the other end of the log.
	oldest, err := r.ListSince(ctx, 0, 5)
	if err != nil {
		t.Fatal(err)
	}
	if oldest[0].Message != "event-00" {
		t.Fatalf("ListSince(0) must start at the oldest, got %q", oldest[0].Message)
	}
	if latest[0].Seq <= oldest[0].Seq {
		t.Fatalf("ListLatest must return higher sequences than ListSince(0): %d vs %d", latest[0].Seq, oldest[0].Seq)
	}
}
