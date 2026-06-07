package embedded

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/observe"
)

// queryAll drains every row the store returns for a bare selector match.
func queryAll(t *testing.T, s *Store, svc string) []observe.LogRow {
	t.Helper()
	ctx := context.Background()
	stream, err := s.Execute(ctx, &observe.Query{
		Selectors: []observe.Matcher{{Label: "service", Op: observe.MatchEqual, Value: svc}},
		Direction: observe.DirectionForward,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	defer stream.Close()
	var out []observe.LogRow
	for stream.Next(ctx) {
		out = append(out, stream.Row())
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream err: %v", err)
	}
	return out
}

// A write→Close→reopen-same-dir cycle must restore the logs from disk.
func TestStore_PersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	base := time.Now().Truncate(time.Second)

	s1 := New(Config{Dir: dir, Retention: -1})
	recs := []observe.LogRecord{
		{Timestamp: base, Service: "api", Line: "first"},
		{Timestamp: base.Add(time.Second), Service: "api", Line: "second"},
		{Timestamp: base.Add(2 * time.Second), Service: "web", Line: "third"},
	}
	if err := s1.Write(context.Background(), recs); err != nil {
		t.Fatal(err)
	}
	if err := s1.Close(); err != nil { // flushes + seals the segment
		t.Fatal(err)
	}

	// Reopen against the same directory: prior run's segment is replayed.
	s2 := New(Config{Dir: dir, Retention: -1})
	defer s2.Close()
	if got := s2.Len(); got != 3 {
		t.Fatalf("want 3 records restored, got %d", got)
	}
	api := queryAll(t, s2, "api")
	if len(api) != 2 {
		t.Fatalf("want 2 api records after restore, got %d", len(api))
	}
	if api[0].Line != "first" || api[1].Line != "second" {
		t.Fatalf("restored records out of order: %+v", api)
	}
}

// Replay must drop records older than the configured retention window.
func TestStore_ReplayHonoursRetention(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)

	s1 := New(Config{Dir: dir, Retention: time.Hour, now: func() time.Time { return now }})
	if err := s1.Write(context.Background(), []observe.LogRecord{
		{Timestamp: now.Add(-2 * time.Hour), Service: "api", Line: "expired"},
		{Timestamp: now.Add(-10 * time.Minute), Service: "api", Line: "fresh"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}

	s2 := New(Config{Dir: dir, Retention: time.Hour, now: func() time.Time { return now }})
	defer s2.Close()
	if got := s2.Len(); got != 1 {
		t.Fatalf("want 1 record within retention after replay, got %d", got)
	}
	if recs := queryAll(t, s2, "api"); len(recs) != 1 || recs[0].Line != "fresh" {
		t.Fatalf("want only the fresh record, got %+v", recs)
	}
}

// An absent/empty Dir leaves the store in pure in-memory mode (no WAL, no files).
func TestStore_NoDirIsInMemoryOnly(t *testing.T) {
	s := New(Config{Retention: -1})
	defer s.Close()
	if s.wal != nil {
		t.Fatal("expected nil WAL when Dir is empty")
	}
	if err := s.Write(context.Background(), []observe.LogRecord{{Service: "api", Line: "x"}}); err != nil {
		t.Fatal(err)
	}
	if got := s.Len(); got != 1 {
		t.Fatalf("want 1 in-memory record, got %d", got)
	}
}

// The size cap must drop the oldest sealed segments so the WAL never exceeds the
// configured disk budget. Driven against the WAL directly so we can force small
// segments (the store always uses the default segment size).
func TestWAL_DiskSizeCapDropsOldestSegments(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	const maxSeg, maxTotal = 512, 2048
	w, err := openWAL(dir, maxSeg, maxTotal, log.GetDefaultLogger())
	if err != nil {
		t.Fatalf("openWAL: %v", err)
	}
	defer w.close()

	line := strings.Repeat("x", 256) // each record ~> a few hundred bytes on disk
	for i := 0; i < 200; i++ {
		w.append([]observe.LogRecord{
			{Timestamp: now.Add(time.Duration(i) * time.Millisecond), Service: "api", Line: line},
		})
	}
	w.flush()
	w.prune(-1, now) // retention disabled => exercise the size cap alone

	if segs := w.segments(); len(segs) < 2 {
		t.Fatalf("expected the WAL to have rotated into multiple segments, got %d", len(segs))
	}

	var total int64
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if fi, err := os.Stat(filepath.Join(dir, e.Name())); err == nil {
			total += fi.Size()
		}
	}
	// Budget + one active (unsealed) segment of slack; never deletes the active one.
	if total > maxTotal+maxSeg {
		t.Fatalf("WAL disk usage %d exceeds cap+segment (%d)", total, maxTotal+maxSeg)
	}
}
