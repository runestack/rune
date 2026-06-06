package service

import (
	"context"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/observe"
	"github.com/runestack/rune/pkg/observe/embedded"
	"google.golang.org/grpc"
)

type mockExecuteStream struct {
	grpc.ServerStream
	ctx     context.Context
	results []*generated.QueryResult
}

func (m *mockExecuteStream) Send(r *generated.QueryResult) error {
	m.results = append(m.results, r)
	return nil
}

func (m *mockExecuteStream) Context() context.Context {
	if m.ctx != nil {
		return m.ctx
	}
	return context.Background()
}

func newSeededObserveService(t *testing.T) *ObserveService {
	t.Helper()
	st := embedded.New(embedded.Config{Retention: -1})
	t.Cleanup(func() { _ = st.Close() })
	base := time.Now().Add(-time.Minute)
	recs := []observe.LogRecord{
		{Timestamp: base, Service: "api", Namespace: "default", Instance: "api-1", Node: "n1", Stream: "stdout", Level: "info", Line: "ok"},
		{Timestamp: base.Add(time.Second), Service: "api", Namespace: "default", Instance: "api-1", Node: "n1", Stream: "stderr", Level: "error", Line: "boom"},
		{Timestamp: base.Add(2 * time.Second), Service: "web", Namespace: "default", Instance: "web-1", Node: "n1", Stream: "stdout", Level: "info", Line: "served"},
	}
	if err := st.Write(context.Background(), recs); err != nil {
		t.Fatal(err)
	}
	return NewObserveService(st, log.GetDefaultLogger())
}

func TestObserveService_ExecuteLogQuery(t *testing.T) {
	svc := newSeededObserveService(t)
	stream := &mockExecuteStream{}
	req := &generated.ObserveQuery{
		Logql: `{service="api"}`,
		Start: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		End:   time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}
	if err := svc.Execute(req, stream); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(stream.results) != 2 {
		t.Fatalf("want 2 api rows, got %d", len(stream.results))
	}
	for _, r := range stream.results {
		if r.GetRow() == nil {
			t.Fatalf("expected log row result, got %v", r)
		}
	}
}

func TestObserveService_ExecuteMetricQuery(t *testing.T) {
	svc := newSeededObserveService(t)
	stream := &mockExecuteStream{}
	req := &generated.ObserveQuery{
		Logql: `sum by (level) (count_over_time({service="api"}[1h]))`,
		Start: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		End:   time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}
	if err := svc.Execute(req, stream); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(stream.results) == 0 {
		t.Fatal("want metric samples, got none")
	}
	for _, r := range stream.results {
		if r.GetSample() == nil {
			t.Fatalf("expected metric sample, got %v", r)
		}
	}
}

func TestObserveService_Capabilities(t *testing.T) {
	svc := newSeededObserveService(t)
	caps, err := svc.GetCapabilities(context.Background(), &generated.CapabilitiesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !caps.GetEnabled() || caps.GetBackend() != "embedded" || caps.GetMaxTier() != "core" {
		t.Fatalf("unexpected capabilities: %+v", caps)
	}
}

func TestObserveService_DisabledReportsNotEnabled(t *testing.T) {
	svc := NewObserveService(nil, log.GetDefaultLogger())
	if svc.Enabled() {
		t.Fatal("expected disabled service")
	}
	caps, err := svc.GetCapabilities(context.Background(), &generated.CapabilitiesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if caps.GetEnabled() {
		t.Fatal("want enabled=false when no store wired")
	}
	// Ingest is a no-op when disabled.
	if err := svc.Ingest(context.Background(), []observe.LogRecord{{Line: "x"}}); err != nil {
		t.Fatalf("disabled ingest should no-op, got %v", err)
	}
}

func TestObserveService_PushAndIngest(t *testing.T) {
	st := embedded.New(embedded.Config{Retention: -1})
	t.Cleanup(func() { _ = st.Close() })
	svc := NewObserveService(st, log.GetDefaultLogger())

	resp, err := svc.PushLogs(context.Background(), &generated.PushLogsRequest{
		Records: []*generated.LogRecord{
			{Line: "via push", Service: "api", Timestamp: time.Now().UTC().Format(time.RFC3339)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetAccepted() != 1 {
		t.Fatalf("want 1 accepted, got %d", resp.GetAccepted())
	}
	if err := svc.Ingest(context.Background(), []observe.LogRecord{{Line: "via ingest", Service: "api", Timestamp: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	if got := st.Len(); got != 2 {
		t.Fatalf("want 2 records after push+ingest, got %d", got)
	}
}
