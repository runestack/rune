package service

import (
	"context"
	"strings"
	"testing"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/orchestrator"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// mockGetLogsStream is a minimal generated.LogService_GetLogsServer that
// collects everything the service sends.
type mockGetLogsStream struct {
	grpc.ServerStream
	ctx  context.Context
	sent []*generated.LogResponse
}

func (m *mockGetLogsStream) Send(r *generated.LogResponse) error {
	m.sent = append(m.sent, r)
	return nil
}

func (m *mockGetLogsStream) Context() context.Context {
	if m.ctx != nil {
		return m.ctx
	}
	return context.Background()
}

func newLogTestService(t *testing.T) (*LogService, *store.TestStore) {
	t.Helper()
	st := store.NewTestStore()
	fo := orchestrator.NewFakeOrchestrator()

	inst := &types.Instance{
		ID:          "instance123",
		Namespace:   "default",
		Name:        "test-instance",
		ServiceID:   "service123",
		ServiceName: "test-service",
		ContainerID: "container123",
		Status:      types.InstanceStatusRunning,
	}
	if err := st.Create(context.Background(), types.ResourceTypeInstance, "default", "instance123", inst); err != nil {
		t.Fatalf("seed instance: %v", err)
	}
	fo.AddInstance(inst)

	return NewLogService(st, log.GetDefaultLogger(), fo), st
}

// TestGetLogs_StreamsHistory proves the server-streaming GetLogs path (the
// browser-callable one, RUNE-200C) drains logs and half-closes when follow is
// false — using the same body as the bidi StreamLogs.
func TestGetLogs_StreamsHistory(t *testing.T) {
	svc, _ := newLogTestService(t)
	stream := &mockGetLogsStream{ctx: context.Background()}

	err := svc.GetLogs(&generated.LogRequest{
		ResourceTarget: "instance123",
		Namespace:      "default",
		Follow:         false,
	}, stream)
	if err != nil {
		t.Fatalf("GetLogs returned error: %v", err)
	}
	if len(stream.sent) == 0 {
		t.Fatal("GetLogs streamed no log responses")
	}
	var joined []string
	for _, r := range stream.sent {
		joined = append(joined, r.Content)
	}
	if !strings.Contains(strings.Join(joined, "\n"), "fake logs") {
		t.Errorf("expected streamed content to include fake instance logs, got %q", joined)
	}
}

// TestGetLogs_ValidatesEmptyTarget proves validation runs through GetLogs (the
// shared serveLogs body) — an empty resource target is InvalidArgument.
func TestGetLogs_ValidatesEmptyTarget(t *testing.T) {
	svc, _ := newLogTestService(t)
	err := svc.GetLogs(&generated.LogRequest{Namespace: "default"}, &mockGetLogsStream{ctx: context.Background()})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for empty target, got %v", err)
	}
}

// TestGetLogs_ValidatesSinceTimestamp proves the since/until validation is
// reached via GetLogs.
func TestGetLogs_ValidatesSinceTimestamp(t *testing.T) {
	svc, _ := newLogTestService(t)
	err := svc.GetLogs(&generated.LogRequest{
		ResourceTarget: "instance123",
		Namespace:      "default",
		Since:          "not-a-timestamp",
	}, &mockGetLogsStream{ctx: context.Background()})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for bad since, got %v", err)
	}
}
