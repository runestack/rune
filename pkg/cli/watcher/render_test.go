package watcher

import (
	"testing"
	"time"

	"github.com/runestack/rune/pkg/types"
)

func TestDefaultServiceResourceToRows_SameNamespaceHeader(t *testing.T) {
	now := time.Now().Add(-2 * time.Hour)
	services := []types.Resource{
		&types.Service{Name: "api", Namespace: "dev", Scale: 1, Status: types.ServiceStatusRunning, Image: "img", Metadata: &types.ServiceMetadata{CreatedAt: now}},
		&types.Service{Name: "web", Namespace: "dev", Scale: 2, Status: types.ServiceStatusPending, Image: "img", Metadata: &types.ServiceMetadata{CreatedAt: now}},
	}

	rows := DefaultServiceResourceToRows(services)
	if len(rows) < 2 {
		t.Fatalf("expected at least header + 1 row, got %d", len(rows))
	}
	// Same namespace -> header without NAMESPACE column
	if got, want := len(rows[0]), 6; got != want {
		t.Fatalf("unexpected header column count: got %d want %d", got, want)
	}
}

func TestDefaultServiceResourceToRows_AllNamespacesHeader(t *testing.T) {
	now := time.Now().Add(-90 * time.Minute)
	services := []types.Resource{
		&types.Service{Name: "api", Namespace: "dev", Scale: 1, Status: types.ServiceStatusRunning, Image: "img", Metadata: &types.ServiceMetadata{CreatedAt: now}},
		&types.Service{Name: "api", Namespace: "prod", Scale: 1, Status: types.ServiceStatusRunning, Image: "img", Metadata: &types.ServiceMetadata{CreatedAt: now}},
	}
	rows := DefaultServiceResourceToRows(services)
	if len(rows) < 2 {
		t.Fatalf("expected at least header + 1 row, got %d", len(rows))
	}
	// Mixed namespaces -> header with NAMESPACE column
	if got, want := len(rows[0]), 7; got != want {
		t.Fatalf("unexpected header column count: got %d want %d", got, want)
	}
}

func TestDefaultInstanceResourceToRows_HeaderVariants(t *testing.T) {
	now := time.Now().Add(-5 * time.Minute)
	instSame := []types.Resource{
		&types.Instance{ID: "1", Name: "a", Namespace: "ns", ServiceID: "svc", CreatedAt: now},
		&types.Instance{ID: "2", Name: "b", Namespace: "ns", ServiceID: "svc", CreatedAt: now},
	}
	rows := DefaultInstanceResourceToRows(instSame)
	if len(rows) < 2 {
		t.Fatalf("expected at least header + 1 row for same ns, got %d", len(rows))
	}
	if got, want := len(rows[0]), 6; got != want {
		t.Fatalf("unexpected header col count for same ns: got %d want %d", got, want)
	}

	instMixed := []types.Resource{
		&types.Instance{ID: "1", Name: "a", Namespace: "ns1", ServiceID: "svc", CreatedAt: now},
		&types.Instance{ID: "2", Name: "b", Namespace: "ns2", ServiceID: "svc", CreatedAt: now},
	}
	rows2 := DefaultInstanceResourceToRows(instMixed)
	if got, want := len(rows2[0]), 7; got != want {
		t.Fatalf("unexpected header col count for mixed ns: got %d want %d", got, want)
	}
}
