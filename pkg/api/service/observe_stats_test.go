package service

import (
	"context"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/observe"
	"github.com/runestack/rune/pkg/observe/embedded"
)

func TestObserveService_GetObserveStats_Embedded(t *testing.T) {
	dir := t.TempDir()
	st := embedded.New(embedded.Config{Retention: 7 * 24 * time.Hour, Dir: dir, MaxDiskBytes: 1 << 20})
	t.Cleanup(func() { _ = st.Close() })

	base := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	if err := st.Write(context.Background(), []observe.LogRecord{
		{Timestamp: base, Service: "api", Line: "a"},
		{Timestamp: base.Add(time.Second), Service: "api", Line: "b"},
	}); err != nil {
		t.Fatal(err)
	}

	svc := NewObserveService(st, nil, log.GetDefaultLogger())
	stats, err := svc.GetObserveStats(context.Background(), &generated.ObserveStatsRequest{})
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if !stats.GetSupported() || stats.GetBackend() != "embedded" {
		t.Fatalf("embedded must report supported stats: %+v", stats)
	}
	if stats.GetRecords() != 2 {
		t.Errorf("records = %d, want 2", stats.GetRecords())
	}
	if stats.GetRetentionDays() != 7 {
		t.Errorf("retention_days = %v, want 7", stats.GetRetentionDays())
	}
	if stats.GetDiskCapBytes() != 1<<20 {
		t.Errorf("disk_cap = %d, want %d", stats.GetDiskCapBytes(), 1<<20)
	}
	if stats.GetOldestRecord() == "" {
		t.Error("oldest_record missing")
	}
}

// A store without StatsProvider reports supported=false (backend-managed).
type statlessStore struct{ observe.LogStore }

func TestObserveService_GetObserveStats_Unsupported(t *testing.T) {
	inner := embedded.New(embedded.Config{Retention: -1})
	t.Cleanup(func() { _ = inner.Close() })
	svc := NewObserveService(statlessStore{inner}, nil, log.GetDefaultLogger())
	stats, err := svc.GetObserveStats(context.Background(), &generated.ObserveStatsRequest{})
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.GetSupported() {
		t.Fatal("wrapped store must report supported=false")
	}
	if stats.GetBackend() != "embedded" {
		t.Errorf("backend name still reported: %+v", stats)
	}
}
