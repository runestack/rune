package service

import (
	"context"
	"time"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/observe"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// GetObserveStats reports store-level facts for the Sources page. Stores that
// don't own their storage (loki/clickhouse) report supported=false — the
// dashboard shows "managed by the backend" for the disk cards and still
// renders the query-derived volume/coverage panels.
func (s *ObserveService) GetObserveStats(ctx context.Context, _ *generated.ObserveStatsRequest) (*generated.ObserveStats, error) {
	if s.store == nil {
		return nil, status.Error(codes.FailedPrecondition, "observability is disabled")
	}
	out := &generated.ObserveStats{Backend: s.store.Capabilities().Backend}
	sp, ok := s.store.(observe.StatsProvider)
	if !ok {
		return out, nil // supported=false: backend manages its own storage
	}
	st := sp.Stats()
	out.Supported = true
	out.Records = st.Records
	out.DiskUsedBytes = st.DiskUsedBytes
	out.DiskCapBytes = st.DiskCapBytes
	out.RetentionDays = st.Retention.Hours() / 24
	if !st.OldestRecord.IsZero() {
		out.OldestRecord = st.OldestRecord.UTC().Format(time.RFC3339)
	}
	return out, nil
}
