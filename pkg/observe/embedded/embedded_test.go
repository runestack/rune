package embedded

import (
	"context"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/observe"
)

func TestStore_WriteEvictsOldestOverMax(t *testing.T) {
	s := New(Config{MaxRecords: 3, Retention: -1})
	defer s.Close()
	base := time.Now()
	for i := 0; i < 5; i++ {
		rec := observe.LogRecord{Timestamp: base.Add(time.Duration(i) * time.Second), Service: "api", Line: "n"}
		if err := s.Write(context.Background(), []observe.LogRecord{rec}); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.Len(); got != 3 {
		t.Fatalf("want 3 records after eviction, got %d", got)
	}
}

func TestStore_RetentionSweep(t *testing.T) {
	now := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	s := New(Config{Retention: time.Hour, now: func() time.Time { return now }})
	defer s.Close()
	recs := []observe.LogRecord{
		{Timestamp: now.Add(-2 * time.Hour), Service: "api", Line: "old"},
		{Timestamp: now.Add(-30 * time.Minute), Service: "api", Line: "fresh"},
	}
	if err := s.Write(context.Background(), recs); err != nil {
		t.Fatal(err)
	}
	s.sweep()
	if got := s.Len(); got != 1 {
		t.Fatalf("want 1 record after retention sweep, got %d", got)
	}
}

func TestStore_Labels(t *testing.T) {
	s := New(Config{Retention: -1})
	defer s.Close()
	base := time.Now()
	recs := []observe.LogRecord{
		{Timestamp: base, Service: "api", Line: "a"},
		{Timestamp: base, Service: "api", Line: "b"},
		{Timestamp: base, Service: "web", Line: "c"},
	}
	if err := s.Write(context.Background(), recs); err != nil {
		t.Fatal(err)
	}
	vals, err := s.Labels(context.Background(), observe.Selector{Name: "service"})
	if err != nil {
		t.Fatal(err)
	}
	if len(vals) != 2 {
		t.Fatalf("want 2 service values, got %d", len(vals))
	}
	// api has 2 occurrences, ranked first.
	if vals[0].Value != "api" || vals[0].Count != 2 {
		t.Fatalf("want api with count 2 first, got %+v", vals[0])
	}
}

func TestStore_HealthAndCapabilities(t *testing.T) {
	s := New(Config{})
	defer s.Close()
	if err := s.Health(context.Background()); err != nil {
		t.Fatalf("health: %v", err)
	}
	c := s.Capabilities()
	if c.Backend != "embedded" || c.MaxTier != observe.TierCore {
		t.Fatalf("unexpected capabilities: %+v", c)
	}
}
