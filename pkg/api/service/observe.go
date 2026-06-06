package service

import (
	"context"
	"errors"
	"time"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/observe"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ObserveService implements the gRPC ObserveService: the native observability
// (RuneSight) query + ingest seam (plan §4.4). It parses the LogQL subset into
// the observe.Query AST, runs it against the configured LogStore, and reports
// Capabilities so the dashboard can feature-flag Advanced-tier widgets.
//
// It also backs the control-plane ingest path: the agent forwarder calls Ingest
// in-process on single-node, or PushLogs over gRPC on multi-node. Both funnel
// into LogStore.Write.
//
// When observability is disabled (no store wired), the service is constructed
// with a nil store; Execute/PushLogs return FailedPrecondition and
// GetCapabilities reports enabled=false so clients can fall back to the live
// ephemeral log stream.
type ObserveService struct {
	generated.UnimplementedObserveServiceServer

	store  observe.LogStore
	logger log.Logger
}

// NewObserveService constructs the service. A nil store means observability is
// disabled; the service still registers so clients get a clean
// "observability disabled" signal rather than an unimplemented method.
func NewObserveService(store observe.LogStore, logger log.Logger) *ObserveService {
	return &ObserveService{
		store:  store,
		logger: logger.WithComponent("observe-service"),
	}
}

// Enabled reports whether a backend store is wired.
func (s *ObserveService) Enabled() bool { return s.store != nil }

// Ingest is the in-process ingest path (single-node). The agent forwarder calls
// it directly with already-enriched records, bypassing the gRPC envelope. It is
// a no-op when observability is disabled.
func (s *ObserveService) Ingest(ctx context.Context, records []observe.LogRecord) error {
	if s.store == nil {
		return nil
	}
	if len(records) == 0 {
		return nil
	}
	return s.store.Write(ctx, records)
}

// PushLogs is the multi-node ingest RPC. Remote agents call it; it validates
// and writes the batch via LogStore.Write.
func (s *ObserveService) PushLogs(ctx context.Context, req *generated.PushLogsRequest) (*generated.PushLogsResponse, error) {
	if s.store == nil {
		return nil, status.Error(codes.FailedPrecondition, "observability is disabled")
	}
	records := make([]observe.LogRecord, 0, len(req.GetRecords()))
	for _, r := range req.GetRecords() {
		records = append(records, protoToLogRecord(r))
	}
	if err := s.store.Write(ctx, records); err != nil {
		s.logger.Error("ingest write failed", log.Err(err), log.Int("records", len(records)))
		return nil, status.Errorf(codes.Internal, "ingest write: %v", err)
	}
	return &generated.PushLogsResponse{
		Accepted: uint32(len(records)), //nolint:gosec // batch size bounded; proto field is uint32
		Status:   &generated.Status{Code: int32(codes.OK)},
	}, nil
}

// GetCapabilities reports the backend handshake. When disabled, enabled=false.
func (s *ObserveService) GetCapabilities(ctx context.Context, _ *generated.CapabilitiesRequest) (*generated.ObserveCapabilities, error) {
	if s.store == nil {
		return &generated.ObserveCapabilities{Enabled: false}, nil
	}
	c := s.store.Capabilities()
	return &generated.ObserveCapabilities{
		Backend:                c.Backend,
		MaxTier:                c.MaxTier.String(),
		RawSql:                 c.RawSQL,
		Percentiles:            c.Percentiles,
		HighCardinalityFilters: c.HighCardinalityFilters,
		Enabled:                true,
	}, nil
}

// Execute parses the LogQL subset, runs it, and server-streams results.
func (s *ObserveService) Execute(req *generated.ObserveQuery, stream generated.ObserveService_ExecuteServer) error {
	if s.store == nil {
		return status.Error(codes.FailedPrecondition, "observability is disabled")
	}
	ctx := stream.Context()

	start, err := parseObserveTime(req.GetStart())
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "invalid start: %v", err)
	}
	end, err := parseObserveTime(req.GetEnd())
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "invalid end: %v", err)
	}

	q, err := observe.ParseLogQL(req.GetLogql(), start, end, int(req.GetLimit()), req.GetForward())
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "parse logql: %v", err)
	}

	rs, err := s.store.Execute(ctx, q)
	if err != nil {
		if errors.Is(err, observe.ErrCapabilityUnsupported) {
			return status.Errorf(codes.Unimplemented, "query not supported by backend: %v", err)
		}
		return status.Errorf(codes.Internal, "execute: %v", err)
	}
	defer rs.Close()

	for rs.Next(ctx) {
		var out *generated.QueryResult
		if rs.IsMetric() {
			out = &generated.QueryResult{Result: &generated.QueryResult_Sample{Sample: metricToProto(rs.Sample())}}
		} else {
			out = &generated.QueryResult{Result: &generated.QueryResult_Row{Row: rowToProto(rs.Row())}}
		}
		if err := stream.Send(out); err != nil {
			return err
		}
	}
	if err := rs.Err(); err != nil {
		return status.Errorf(codes.Internal, "stream: %v", err)
	}
	return nil
}

// --- proto <-> domain conversions ---

func protoToLogRecord(r *generated.LogRecord) observe.LogRecord {
	ts, _ := parseObserveTime(r.GetTimestamp())
	return observe.LogRecord{
		Timestamp: ts,
		Line:      r.GetLine(),
		Stream:    r.GetStream(),
		Level:     r.GetLevel(),
		Namespace: r.GetNamespace(),
		Service:   r.GetService(),
		Instance:  r.GetInstance(),
		Node:      r.GetNode(),
		Labels:    r.GetLabels(),
	}
}

func rowToProto(row observe.LogRow) *generated.LogRow {
	return &generated.LogRow{
		Timestamp: formatObserveTime(row.Timestamp),
		Line:      row.Line,
		Stream:    row.Stream,
		Level:     row.Level,
		Labels:    row.Labels,
	}
}

func metricToProto(s observe.MetricSample) *generated.MetricSample {
	return &generated.MetricSample{
		Timestamp:   formatObserveTime(s.Timestamp),
		Value:       s.Value,
		GroupLabels: s.GroupLabels,
	}
}

// parseObserveTime parses an RFC3339 timestamp; empty string => zero time.
func parseObserveTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, s)
}

func formatObserveTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}
